package bot

import (
	"testing"
	"time"

	"github.com/viceor/tele-admin/internal/config"
)

// memberCmdUpdate builds a /restrict or /unrestrict message sent by actor in
// chat 500. When argID is non-empty it is passed as the first argument; when
// replyTo is non-zero the message replies to a message from that user.
func memberCmdUpdate(actor int64, cmd string, argID string, replyTo int64) string {
	text := cmd
	if argID != "" {
		text += " " + argID
	}
	msg := map[string]any{
		"message_id": 10,
		"from":       map[string]any{"id": actor, "first_name": "admin"},
		"chat":       map[string]any{"id": 500, "title": "group"},
		"text":       text,
		"entities": []map[string]any{
			{"type": "bot_command", "offset": 0, "length": len(cmd)},
		},
	}
	if replyTo != 0 {
		msg["reply_to_message"] = map[string]any{
			"message_id": 9,
			"from":       map[string]any{"id": replyTo, "first_name": "target"},
			"chat":       map[string]any{"id": 500, "title": "group"},
			"text":       "hi",
		}
	}
	return toJSON(map[string]any{"update_id": 3, "message": msg})
}

const (
	actorAdmin  int64 = 100
	actorMember int64 = 200
	targetUser  int64 = 300
	ownerUser   int64 = 400
)

func newMemberTestBot(t *testing.T, enforce bool, owners []int64) (*Bot, *mockTelegram) {
	t.Helper()
	mock := newMockTelegram(t)
	cfg := testConfig("")
	cfg.Admin.EnforceAdmin = enforce
	cfg.Admin.OwnerIDs = owners
	b := startTestBot(t, cfg, mock)
	return b, mock
}

// --- success paths ---------------------------------------------------------

func TestRestrictByReplyAsAdminSucceeds(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorAdmin, "administrator")
	mock.setMemberStatusFor(targetUser, "member")

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/restrict", "", targetUser))

	waitFor(t, 2*time.Second, func() bool { return len(mock.restricts()) == 1 })
	c := mock.restricts()[0]
	if c.chat != 500 || c.user != targetUser || c.allowAll {
		t.Fatalf("restrict call = %+v, want chat 500 user %d with all actions denied", c, targetUser)
	}
	waitForSent(t, mock, "has been restricted")
}

func TestRestrictByUserIDArgumentAsAdminSucceeds(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorAdmin, "creator")

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/restrict", "300", 0))

	waitFor(t, 2*time.Second, func() bool { return len(mock.restricts()) == 1 })
	if got := mock.restricts()[0].user; got != targetUser {
		t.Fatalf("restricted user = %d, want %d", got, targetUser)
	}
}

func TestUnrestrictGrantsAllPermissions(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorAdmin, "administrator")
	// MYC-55 SEC-1: /unrestrict applies only to restricted targets.
	mock.setRestrictedFor(targetUser, true, false)

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/unrestrict", "300", 0))

	waitFor(t, 2*time.Second, func() bool { return len(mock.restricts()) == 1 })
	c := mock.restricts()[0]
	if !c.allowAll || c.user != targetUser {
		t.Fatalf("unrestrict call = %+v, want all actions granted to %d", c, targetUser)
	}
	waitForSent(t, mock, "no longer restricted")
}

func TestOwnerMayRestrictWithoutChatAdminStatus(t *testing.T) {
	b, mock := newMemberTestBot(t, true, []int64{ownerUser})
	mock.setMemberStatusFor(ownerUser, "member")

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(ownerUser, "/restrict", "300", 0))

	waitFor(t, 2*time.Second, func() bool { return len(mock.restricts()) == 1 })
}

// --- denial paths: the caller is not authorized ----------------------------

func TestRestrictDeniedForNonAdmin(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorMember, "member")

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorMember, "/restrict", "300", 0))

	waitForSent(t, mock, "Only group administrators")
	assertNoRestrictCalls(t, mock)
}

func TestUnrestrictDeniedForNonAdmin(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorMember, "member")

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorMember, "/unrestrict", "300", 0))

	waitForSent(t, mock, "Only group administrators")
	assertNoRestrictCalls(t, mock)
}

// A failed caller-status lookup must fail closed: no mutation at all.
func TestRestrictDeniedWhenCallerStatusLookupFails(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberErr(errString("getChatMember unavailable"))

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/restrict", "300", 0))

	time.Sleep(200 * time.Millisecond)
	assertNoRestrictCalls(t, mock)
	if countSent(mock, "restricted") != 0 {
		t.Fatal("must not report a restriction when the admin check failed")
	}
}

// An anonymous (sender-less) command must never act.
func TestRestrictDeniedForAnonymousSender(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	upd := toJSON(map[string]any{
		"update_id": 4,
		"message": map[string]any{
			"message_id": 10,
			"chat":       map[string]any{"id": 500, "title": "group"},
			"text":       "/restrict 300",
			"entities":   []map[string]any{{"type": "bot_command", "offset": 0, "length": 9}},
		},
	})

	postUpdate(t, b.Client().WebhookHandler(), upd)

	waitForSent(t, mock, "Only group administrators")
	assertNoRestrictCalls(t, mock)
}

// With enforcement disabled the admin check is permissive, so the anonymous
// guard in handleMemberAction is the only barrier. Neither command may panic
// on a nil From, reach the audit log, or call restrictChatMember (MYC-83).
func TestMemberCommandsAnonymousWithEnforcementOffDoNotPanic(t *testing.T) {
	for _, cmd := range []string{"/restrict 300", "/unrestrict 300"} {
		t.Run(cmd, func(t *testing.T) {
			t.Setenv(config.DevModeEnvVar, "1")
			b, mock := newMemberTestBot(t, false, nil)
			upd := toJSON(map[string]any{
				"update_id": 5,
				"message": map[string]any{
					"message_id": 10,
					"chat":       map[string]any{"id": 500, "title": "group"},
					"text":       cmd,
					"entities":   []map[string]any{{"type": "bot_command", "offset": 0, "length": len(cmd) - len(" 300")}},
				},
			})

			postUpdate(t, b.Client().WebhookHandler(), upd)

			waitForSent(t, mock, "Only group administrators")
			assertNoRestrictCalls(t, mock)
		})
	}
}

// --- denial paths: the target is protected ----------------------------------

func TestRestrictRefusesChatAdministratorTarget(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorAdmin, "administrator")
	mock.setMemberStatusFor(targetUser, "administrator")

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/restrict", "300", 0))

	waitForSent(t, mock, "cannot be applied")
	assertNoRestrictCalls(t, mock)
}

func TestRestrictRefusesChatCreatorTarget(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorAdmin, "administrator")
	mock.setMemberStatusFor(targetUser, "creator")

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/restrict", "300", 0))

	waitForSent(t, mock, "cannot be applied")
	assertNoRestrictCalls(t, mock)
}

func TestRestrictRefusesConfiguredOwnerTarget(t *testing.T) {
	b, mock := newMemberTestBot(t, true, []int64{ownerUser})
	mock.setMemberStatusFor(actorAdmin, "administrator")
	mock.setMemberStatusFor(ownerUser, "member")

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/restrict", "400", 0))

	waitForSent(t, mock, "cannot be applied")
	assertNoRestrictCalls(t, mock)
}

// If the target status cannot be read, the target is treated as protected and
// nothing is restricted. The caller lookup succeeds, so only the target fails.
func TestRestrictRefusesWhenTargetStatusUnknown(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorAdmin, "administrator")
	mock.setMemberErrFor(targetUser, errString("getChatMember unavailable for target"))

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/restrict", "300", 0))

	waitForSent(t, mock, "cannot be applied")
	assertNoRestrictCalls(t, mock)
}

// --- error paths: Telegram rejects or fails the mutation --------------------

func TestRestrictReportsFailureOnAPIError(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorAdmin, "administrator")
	mock.setRestrictErr(errString("Bad Request: not enough rights"))

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/restrict", "300", 0))

	waitForSent(t, mock, "Could not confirm the result")
	if countSent(mock, "has been restricted") != 0 {
		t.Fatal("must not report success after a Telegram error")
	}
}

func TestUnrestrictReportsFailureOnOkFalse(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorAdmin, "administrator")
	mock.setRestrictedFor(targetUser, true, false)
	mock.setRestrictOK(false)

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/unrestrict", "300", 0))

	waitForSent(t, mock, "Could not confirm the result")
	if countSent(mock, "no longer restricted") != 0 {
		t.Fatal("must not report success when Telegram returns ok=false")
	}
}

// --- usage and parsing -------------------------------------------------------

func TestRestrictWithoutTargetShowsUsage(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorAdmin, "administrator")

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/restrict", "", 0))

	waitForSent(t, mock, "Usage:")
	assertNoRestrictCalls(t, mock)
}

func TestRestrictRejectsInvalidUserIDs(t *testing.T) {
	for _, arg := range []string{"@alice", "abc", "0", "-5"} {
		t.Run(arg, func(t *testing.T) {
			b, mock := newMemberTestBot(t, true, nil)
			mock.setMemberStatusFor(actorAdmin, "administrator")

			postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/restrict", arg, 0))

			waitForSent(t, mock, "Usage:")
			assertNoRestrictCalls(t, mock)
		})
	}
}

// Member commands in a chat use the chat language, so a zh chat gets zh text.
func TestRestrictRepliesInChineseWhenChatIsChinese(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorAdmin, "administrator")
	// MYC-55 SEC-1: /unrestrict applies only to restricted targets.
	mock.setRestrictedFor(targetUser, true, false)
	b.store.SetLanguage(500, "zh")

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/unrestrict", "300", 0))

	waitForSent(t, mock, "已解除限制")
}

// --- target status gating (MYC-55 SEC-1) --------------------------------------

// /restrict applies to restricted targets as well as plain members.
func TestRestrictAllowsRestrictedTarget(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorAdmin, "administrator")
	mock.setRestrictedFor(targetUser, true, false)

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/restrict", "300", 0))

	waitFor(t, 2*time.Second, func() bool { return len(mock.restricts()) == 1 })
	if c := mock.restricts()[0]; c.allowAll {
		t.Fatalf("restrict call = %+v, want all actions denied", c)
	}
	waitForSent(t, mock, "has been restricted")
}

// /unrestrict must refuse a plain member target: the action applies only to
// restricted members.
func TestUnrestrictRefusesPlainMemberTarget(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorAdmin, "administrator")
	mock.setMemberStatusFor(targetUser, "member")

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/unrestrict", "300", 0))

	waitForSent(t, mock, "does not apply to banned or non-member users")
	assertNoRestrictCalls(t, mock)
}

// Banned (kicked) and left users are non-members: neither command may act on
// them, and unban stays out of scope for this slice.
func TestMemberCommandsRefuseKickedAndLeftTargets(t *testing.T) {
	for _, status := range []string{"kicked", "left"} {
		for _, cmd := range []string{"/restrict", "/unrestrict"} {
			t.Run(cmd+"-"+status, func(t *testing.T) {
				b, mock := newMemberTestBot(t, true, nil)
				mock.setMemberStatusFor(actorAdmin, "administrator")
				mock.setMemberStatusFor(targetUser, status)

				postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, cmd, "300", 0))

				waitForSent(t, mock, "does not apply to banned or non-member users")
				assertNoRestrictCalls(t, mock)
				if countSent(mock, "has been restricted") != 0 || countSent(mock, "no longer restricted") != 0 {
					t.Fatal("must not report a change for a banned or non-member target")
				}
			})
		}
	}
}

// The zh catalog states the not-applicable rejection in Chinese.
func TestUnrestrictRefusesKickedTargetInChinese(t *testing.T) {
	b, mock := newMemberTestBot(t, true, nil)
	mock.setMemberStatusFor(actorAdmin, "administrator")
	mock.setMemberStatusFor(targetUser, "kicked")
	b.store.SetLanguage(500, "zh")

	postUpdate(t, b.Client().WebhookHandler(), memberCmdUpdate(actorAdmin, "/unrestrict", "300", 0))

	waitForSent(t, mock, "该操作不适用于被封禁/非成员用户")
	assertNoRestrictCalls(t, mock)
}

// --- helpers ----------------------------------------------------------------

func assertNoRestrictCalls(t *testing.T, mock *mockTelegram) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	if n := len(mock.restricts()); n != 0 {
		t.Fatalf("expected no restrictChatMember calls, got %d", n)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
