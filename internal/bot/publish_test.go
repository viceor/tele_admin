package bot

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Publishing (slice S2) tests. Source chat is 500; registered targets are
// 600 and 700 (plus -100888 for a channel-style negative ID). The mock bot
// user ID is 1 and is an administrator of test chats by default.

const (
	publishAdmin  int64 = 100
	publishMember int64 = 200
	publishOwner  int64 = 400
	// publishPrivateUser chats with the bot privately; in Telegram a private
	// chat's ID equals the user's ID.
	publishPrivateUser int64 = 210
)

func newPublishTestBot(t *testing.T, owners []int64, tune func(*mockTelegram)) (*Bot, *mockTelegram) {
	t.Helper()
	mock := newMockTelegram(t)
	mock.setMemberStatusFor(publishAdmin, "administrator")
	cfg := testConfig("")
	cfg.Admin.OwnerIDs = owners
	b := startTestBot(t, cfg, mock)
	// Register targets 600 and 700 in the store directly; /publish_add tests
	// exercise the command path separately.
	if err := b.store.AddPublishTarget(600, publishAdmin); err != nil {
		t.Fatalf("AddPublishTarget 600: %v", err)
	}
	if err := b.store.AddPublishTarget(700, publishAdmin); err != nil {
		t.Fatalf("AddPublishTarget 700: %v", err)
	}
	if tune != nil {
		tune(mock)
	}
	return b, mock
}

func waitForMedia(t *testing.T, mock *mockTelegram, n int) []mediaCall {
	t.Helper()
	waitFor(t, 2*time.Second, func() bool { return len(mock.media()) >= n })
	return mock.media()
}

// waitForDelivered waits until the exact text has been sent n times to mocks.
func waitForDelivered(t *testing.T, mock *mockTelegram, exact string, n int) {
	t.Helper()
	waitFor(t, 2*time.Second, func() bool {
		c := 0
		for _, s := range mock.sent() {
			if s["text"] == exact {
				c++
			}
		}
		return c >= n
	})
}

func deliveredCount(mock *mockTelegram, exact string) int {
	c := 0
	for _, s := range mock.sent() {
		if s["text"] == exact {
			c++
		}
	}
	return c
}

// assertDeliveredExactly fails when the exact text was not delivered exactly
// n times.
func assertDeliveredExactly(t *testing.T, mock *mockTelegram, exact string, n int) {
	t.Helper()
	if c := deliveredCount(mock, exact); c != n {
		t.Fatalf("deliveries of %q = %d, want %d", exact, c, n)
	}
}

func assertNoMediaSends(t *testing.T, mock *mockTelegram) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	if n := len(mock.media()); n != 0 {
		t.Fatalf("expected no media sends, got %d", n)
	}
}

func assertNoPublishText(t *testing.T, mock *mockTelegram) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	for _, m := range mock.sent() {
		if strings.Contains(m["text"].(string), "\u2713") || strings.Contains(m["text"].(string), "\u2717") {
			t.Fatalf("unexpected publish summary: %v", m["text"])
		}
	}
}

// --- success paths -----------------------------------------------------------

func TestPublishTextToRegisteredTargets(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600,700 hello group"))

	waitForDelivered(t, mock, "hello group", 2)
	assertDeliveredExactly(t, mock, "hello group", 2)
	// The summary is composed only after every target finishes and the record
	// is persisted, so it can land after both deliveries are observed; poll
	// for it instead of reading a single snapshot (MYC-96 flake).
	var summary string
	waitFor(t, 2*time.Second, func() bool {
		for _, s := range mock.sent() {
			if strings.HasPrefix(s["text"].(string), "Publish succeeded") {
				summary = s["text"].(string)
				return true
			}
		}
		return false
	})
	if summary == "" || !strings.Contains(summary, "2/2") || !strings.Contains(summary, "\u2713 600") || !strings.Contains(summary, "\u2713 700") {
		t.Fatalf("summary = %q", summary)
	}
}

func TestPublishAllUsesRegisteredTargets(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setMemberStatusFor(publishAdmin, "administrator")
	})
	if err := b.store.AddPublishTarget(-100888, publishAdmin); err != nil {
		t.Fatalf("AddPublishTarget: %v", err)
	}

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text ALL announcement"))

	waitForDelivered(t, mock, "announcement", 3)
	chats := map[int64]bool{}
	for _, s := range mock.sent() {
		if s["text"] == "announcement" {
			chats[atoi(s["chat_id"])] = true
		}
	}
	if !chats[600] || !chats[700] || !chats[-100888] {
		t.Fatalf("delivered chats = %v, want 600, 700, -100888", chats)
	}
}

func atoi(s any) int64 {
	n, _ := strconv.ParseInt(s.(string), 10, 64)
	return n
}

func TestPublishPhotoWithFileIDAndCaption(t *testing.T) {
	b, mock := newPublishTestBot(t, []int64{publishOwner}, nil)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishOwner, "/publish photo 600 AgACphoto123 See this"))

	calls := waitForMedia(t, mock, 1)
	if calls[0].kind != "photo" || calls[0].chat != 600 || calls[0].ref != "AgACphoto123" || calls[0].caption != "See this" {
		t.Fatalf("photo call = %+v", calls[0])
	}
}

func TestPublishVideoAndDocumentByURL(t *testing.T) {
	for _, tc := range []struct {
		kind, url string
	}{
		{"video", "https://example.com/clip.mp4"},
		{"document", "https://example.com/report.pdf"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			b, mock := newPublishTestBot(t, nil, nil)

			cmd := "/publish " + tc.kind + " 600,700 " + tc.url
			postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, cmd))

			calls := waitForMedia(t, mock, 2)
			for _, c := range calls {
				if c.kind != tc.kind || c.ref != tc.url || c.caption != "" {
					t.Fatalf("%s call = %+v", tc.kind, c)
				}
			}
		})
	}
}

func TestPublishMediaWithTextContentStillCounted(t *testing.T) {
	// A media ref that borrows text syntax must still reach the right target.
	b, mock := newPublishTestBot(t, nil, nil)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish document 600 BQADdoc999 quarterly report"))

	calls := waitForMedia(t, mock, 1)
	if calls[0].kind != "document" || calls[0].ref != "BQADdoc999" || calls[0].caption != "quarterly report" {
		t.Fatalf("document call = %+v", calls[0])
	}
}

// --- partial failure is never reported as overall success --------------------

func TestPublishPartialFailureNotReportedAsSuccess(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setSendErrForChat(700, errString("Bad Request: not enough rights"))
	})

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600,700 hello all"))

	waitForDelivered(t, mock, "hello all", 1)
	assertDeliveredExactly(t, mock, "hello all", 1)
	// The summary is composed only after every target finishes and the record
	// is persisted, so it can land after the first delivery is observed; poll
	// for it instead of reading a single snapshot (MYC-94/MYC-96 flake).
	var summary string
	waitFor(t, 2*time.Second, func() bool {
		for _, s := range mock.sent() {
			if strings.Contains(s["text"].(string), "\u2717") {
				summary = s["text"].(string)
				return true
			}
		}
		return false
	})
	if summary == "" {
		t.Fatal("no failure summary sent")
	}
	if !strings.Contains(summary, "failures (1/2)") || !strings.Contains(summary, "\u2713 600") || !strings.Contains(summary, "\u2717 700: send failed") {
		t.Fatalf("summary = %q", summary)
	}
	if strings.Contains(summary, "succeeded") {
		t.Fatalf("partial failure must not be reported as success: %q", summary)
	}
	// The stored record must carry the per-target result.
	recs := b.store.PublishRecords(0, 1)
	if len(recs) != 1 || len(recs[0].Results) != 2 {
		t.Fatalf("records = %+v", recs)
	}
	if !recs[0].Results[0].Success || recs[0].Results[1].Success || recs[0].Results[1].Reason != "send_failed" {
		t.Fatalf("results = %+v", recs[0].Results)
	}
}

func TestPublishFailsTargetWhenBotLacksSendRight(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		// The bot (user 1) is a restricted member without send rights in 600.
		m.setRestrictedFor(1, true, false)
	})

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600 hi"))

	var summary string
	waitFor(t, 2*time.Second, func() bool {
		for _, s := range mock.sent() {
			if strings.Contains(s["text"].(string), "\u2717") {
				summary = s["text"].(string)
				return true
			}
		}
		return false
	})
	if !strings.Contains(summary, "\u2717 600: bot cannot send") || strings.Contains(summary, "\u2713") {
		t.Fatalf("summary = %q", summary)
	}
	if got := mock.media(); len(got) != 0 {
		t.Fatalf("media sends = %+v, want none", got)
	}
}

// A plain-member bot may send when the chat's default permissions allow it;
// the getChat permission probe is the deciding check.
func TestPublishAllowsBotWhenChatPermissionsAllowMemberBot(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setMemberStatusFor(1, "member")
		m.setChatPerm(true)
	})

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600 hi there"))

	waitForDelivered(t, mock, "hi there", 1)
}

func TestPublishFailsTargetWhenBotIsPlainMemberAndChatMutesAll(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setMemberStatusFor(1, "member")
		m.setChatPerm(false)
	})

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600 hi"))

	waitFor(t, 2*time.Second, func() bool { return len(mock.sent()) > 0 })
	for _, s := range mock.sent() {
		if strings.Contains(s["text"].(string), "\u2717 600: bot cannot send") {
			return
		}
	}
	t.Fatalf("no per-target failure summary; sent = %v", mock.sent())
}

// A failed permission lookup must fail closed for that target: no send.
func TestPublishFailsTargetWhenBotStatusLookupFails(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setMemberErrFor(1, errString("getChatMember unavailable"))
	})

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600 hi"))

	var summary string
	waitFor(t, 2*time.Second, func() bool {
		for _, s := range mock.sent() {
			if strings.Contains(s["text"].(string), "\u2717") {
				summary = s["text"].(string)
				return true
			}
		}
		return false
	})
	if !strings.Contains(summary, "permission check failed") {
		t.Fatalf("summary = %q", summary)
	}
}

// --- bounded concurrency and per-target deadline ------------------------------

// A slow target must not block the other targets: sends run concurrently, so
// the target listed after the slow one is delivered without waiting out the
// slowness. 700 is listed first on purpose — serial sending would delay 600
// by the full slowness of 700.
func TestPublishSlowTargetDoesNotBlockOtherTargets(t *testing.T) {
	const slowDelay = 600 * time.Millisecond
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setSendDelayFor(700, slowDelay)
	})

	start := time.Now()
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 700,600 slow target check"))

	waitForDelivered(t, mock, "slow target check", 1)
	if elapsed := time.Since(start); elapsed > slowDelay/2 {
		t.Fatalf("first delivery took %v; the slow target blocked the fast one", elapsed)
	}
	waitForDelivered(t, mock, "slow target check", 2)
	// The summary is sent after the record is persisted, so seeing it
	// guarantees the record is readable.
	waitForSent(t, mock, "Publish succeeded")
	// The record keeps input order (700 first) no matter the completion order.
	recs := b.store.PublishRecords(0, 1)
	if len(recs) != 1 || len(recs[0].Results) != 2 {
		t.Fatalf("records = %+v", recs)
	}
	if recs[0].Results[0].ChatID != 700 || !recs[0].Results[0].Success {
		t.Fatalf("result[0] = %+v, want success for 700 (input order)", recs[0].Results[0])
	}
	if recs[0].Results[1].ChatID != 600 || !recs[0].Results[1].Success {
		t.Fatalf("result[1] = %+v, want success for 600", recs[0].Results[1])
	}
}

// A target whose send exceeds the per-target deadline is recorded as a
// timeout failure while the other target still succeeds; the whole publish
// finishes shortly after the deadline, not after the slow send returns.
func TestPublishTimesOutSlowTargetAndRecordsFailure(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setSendDelayFor(700, 3*time.Second)
	})
	b.cfg.Publish.PerTargetTimeout = 150 * time.Millisecond

	start := time.Now()
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600,700 timeout probe"))

	var summary string
	waitFor(t, 2*time.Second, func() bool {
		for _, s := range mock.sent() {
			if strings.Contains(s["text"].(string), "\u2717") {
				summary = s["text"].(string)
				return true
			}
		}
		return false
	})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("publish took %v; the per-target deadline did not bound the slow target", elapsed)
	}
	if !strings.Contains(summary, "\u2713 600") || !strings.Contains(summary, "\u2717 700: send timed out") {
		t.Fatalf("summary = %q", summary)
	}
	recs := b.store.PublishRecords(0, 1)
	if len(recs) != 1 || len(recs[0].Results) != 2 {
		t.Fatalf("records = %+v", recs)
	}
	if !recs[0].Results[0].Success {
		t.Fatalf("fast target must succeed: %+v", recs[0].Results[0])
	}
	if recs[0].Results[1].Success || recs[0].Results[1].Reason != publishReasonTimeout {
		t.Fatalf("slow target must be recorded as a timeout: %+v", recs[0].Results[1])
	}
}

// A permission probe that exceeds the per-target deadline is recorded as a
// timeout failure, not as a lookup failure.
func TestPublishTimesOutDuringPermissionProbe(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setMemberDelayFor(1, 3*time.Second)
	})
	b.cfg.Publish.PerTargetTimeout = 150 * time.Millisecond

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600 probe timeout"))

	var summary string
	waitFor(t, 2*time.Second, func() bool {
		for _, s := range mock.sent() {
			if strings.Contains(s["text"].(string), "\u2717") {
				summary = s["text"].(string)
				return true
			}
		}
		return false
	})
	if !strings.Contains(summary, "\u2717 600: send timed out") {
		t.Fatalf("summary = %q", summary)
	}
	recs := b.store.PublishRecords(0, 1)
	if len(recs) != 1 || recs[0].Results[0].Success || recs[0].Results[0].Reason != publishReasonTimeout {
		t.Fatalf("result = %+v", recs[0].Results[0])
	}
}

// --- channel administrator post right -----------------------------------------

// registerChannelTarget registers a channel-style chat as a publish target
// and makes the mock report it as a broadcast channel.
func registerChannelTarget(t *testing.T, b *Bot, mock *mockTelegram, chatID int64) {
	t.Helper()
	mock.setChatTypeFor(chatID, "channel")
	if err := b.store.AddPublishTarget(chatID, publishAdmin); err != nil {
		t.Fatalf("AddPublishTarget %d: %v", chatID, err)
	}
}

// Regression: a channel administrator without can_post_messages must be
// rejected for that target; administrators were previously allowed outright.
func TestPublishRejectsChannelAdminWithoutPostRight(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)
	registerChannelTarget(t, b, mock, -100555)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text -100555 channel drop"))

	var summary string
	waitFor(t, 2*time.Second, func() bool {
		for _, s := range mock.sent() {
			if strings.Contains(s["text"].(string), "\u2717") {
				summary = s["text"].(string)
				return true
			}
		}
		return false
	})
	if !strings.Contains(summary, "\u2717 -100555: bot cannot send") || strings.Contains(summary, "\u2713") {
		t.Fatalf("summary = %q", summary)
	}
	if n := deliveriesToChat(mock, -100555, "channel drop"); n != 0 {
		t.Fatalf("channel without post right received %d messages", n)
	}
	recs := b.store.PublishRecords(0, 1)
	if len(recs) != 1 || recs[0].Results[0].Success || recs[0].Results[0].Reason != "no_send_right" {
		t.Fatalf("result = %+v", recs[0].Results[0])
	}
}

// A channel administrator holding can_post_messages may receive publishes.
func TestPublishAllowsChannelAdminWithPostRight(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setAdminCanPostFor(1, true)
	})
	registerChannelTarget(t, b, mock, -100555)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text -100555 channel news"))

	waitForDelivered(t, mock, "channel news", 1)
	if n := deliveriesToChat(mock, -100555, "channel news"); n != 1 {
		t.Fatalf("channel deliveries = %d, want 1", n)
	}
	// The summary is sent after the record is persisted.
	waitForSent(t, mock, "Publish succeeded")
	recs := b.store.PublishRecords(0, 1)
	if len(recs) != 1 || !recs[0].Results[0].Success {
		t.Fatalf("result = %+v", recs[0].Results[0])
	}
}

// In groups and supergroups the Bot API does not report can_post_messages
// for administrators; a group administrator must stay allowed.
func TestPublishAllowsGroupAdminWithoutCanPostMessages(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600 group admin news"))

	waitForDelivered(t, mock, "group admin news", 1)
}

// --- authorization -----------------------------------------------------------

func TestPublishDeniedForNonAdmin(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setMemberStatusFor(publishMember, "member")
	})

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishMember, "/publish text 600,700 hi"))

	waitForSent(t, mock, "Only group administrators")
	assertNoMediaSends(t, mock)
	assertNoPublishText(t, mock)
	if len(b.store.PublishRecords(0, 0)) != 0 {
		t.Fatal("no publish record may be created for a denied command")
	}
}

func TestPublishDeniedForAnonymousSender(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)

	upd := toJSON(map[string]any{
		"update_id": 5,
		"message": map[string]any{
			"message_id": 10,
			"chat":       map[string]any{"id": 500, "title": "group"},
			"text":       "/publish text 600,700 hi",
			"entities":   []map[string]any{{"type": "bot_command", "offset": 0, "length": 8}},
		},
	})
	postUpdate(t, b.Client().WebhookHandler(), upd)

	waitForSent(t, mock, "Only group administrators")
	assertNoMediaSends(t, mock)
}

func TestPublishAllowedForConfiguredOwnerWithoutChatAdminStatus(t *testing.T) {
	b, mock := newPublishTestBot(t, []int64{publishOwner}, func(m *mockTelegram) {
		m.setMemberStatusFor(publishOwner, "member")
	})

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishOwner, "/publish text 600 owner news"))

	waitForDelivered(t, mock, "owner news", 1)
}

// In the real Bot API a private chat's human participant resolves to creator
// status, so any user passes requireAdmin from a private chat. A non-owner
// must be refused /publish there with no delivery and no publish record.
func TestPublishDeniedInPrivateChatForNonOwner(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setMemberStatusForChat(publishPrivateUser, publishPrivateUser, "creator")
	})

	upd := toJSON(map[string]any{
		"update_id": 8,
		"message": map[string]any{
			"message_id": 12,
			"from":       map[string]any{"id": publishPrivateUser, "first_name": "stranger"},
			"chat":       map[string]any{"id": publishPrivateUser, "type": "private", "first_name": "stranger"},
			"text":       "/publish text all private hello",
			"entities":   []map[string]any{{"type": "bot_command", "offset": 0, "length": 8}},
		},
	})
	postUpdate(t, b.Client().WebhookHandler(), upd)

	waitForSent(t, mock, "Only group administrators")
	assertDeliveredExactly(t, mock, "private hello", 0)
	assertNoMediaSends(t, mock)
	assertNoPublishText(t, mock)
	if len(b.store.PublishRecords(0, 0)) != 0 {
		t.Fatal("no publish record may be created for a denied command")
	}
}

// A configured bot owner keeps the ability to publish from a private chat,
// matching the owner bypass in requireAdmin.
func TestPublishAllowedInPrivateChatForConfiguredOwner(t *testing.T) {
	b, mock := newPublishTestBot(t, []int64{publishOwner}, func(m *mockTelegram) {
		m.setMemberStatusForChat(publishOwner, publishOwner, "creator")
	})

	upd := toJSON(map[string]any{
		"update_id": 9,
		"message": map[string]any{
			"message_id": 13,
			"from":       map[string]any{"id": publishOwner, "first_name": "owner"},
			"chat":       map[string]any{"id": publishOwner, "type": "private", "first_name": "owner"},
			"text":       "/publish text all owner private hello",
			"entities":   []map[string]any{{"type": "bot_command", "offset": 0, "length": 8}},
		},
	})
	postUpdate(t, b.Client().WebhookHandler(), upd)

	waitForDelivered(t, mock, "owner private hello", 2)
	chats := map[int64]bool{}
	for _, s := range mock.sent() {
		if s["text"] == "owner private hello" {
			chats[atoi(s["chat_id"])] = true
		}
	}
	if !chats[600] || !chats[700] {
		t.Fatalf("delivered chats = %v, want 600 and 700", chats)
	}
	waitForSent(t, mock, "Publish succeeded")
}

// --- limits and validation ---------------------------------------------------

func TestPublishRejectsUnregisteredTargetBeforeSending(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600,999 hi"))

	waitForSent(t, mock, "not a registered publish target")
	assertNoMediaSends(t, mock)
	assertNoPublishText(t, mock)
}

func TestPublishRejectsTooManyTargets(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)
	cfgMax := 2
	// Register three targets, but the configured cap is 2. Mutate the config
	// directly: limits are service-level, not store-level.
	b.cfg.Publish.MaxTargets = cfgMax
	if err := b.store.AddPublishTarget(800, publishAdmin); err != nil {
		t.Fatalf("AddPublishTarget: %v", err)
	}

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600,700,800 hi"))

	waitForSent(t, mock, "Too many targets (3)")
	assertNoMediaSends(t, mock)
	assertNoPublishText(t, mock)
}

func TestPublishRejectsAllWhenRegistryExceedsLimit(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)
	b.cfg.Publish.MaxTargets = 1
	if err := b.store.AddPublishTarget(800, publishAdmin); err != nil {
		t.Fatalf("AddPublishTarget: %v", err)
	}

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text all hi"))

	waitForSent(t, mock, "Too many targets (3)")
	assertNoMediaSends(t, mock)
}

func TestPublishRejectsOversizedText(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)
	b.cfg.Publish.MaxContentLength = 10
	long := strings.Repeat("x", 11)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600 "+long))

	waitForSent(t, mock, "text is too long")
	assertNoMediaSends(t, mock)
}

func TestPublishAcceptsTextAtLimit(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)
	b.cfg.Publish.MaxContentLength = 10

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600 "+strings.Repeat("y", 10)))

	waitForDelivered(t, mock, strings.Repeat("y", 10), 1)
}

func TestPublishRejectsOversizedCaption(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)
	b.cfg.Publish.MaxContentLength = 2000
	caption := strings.Repeat("c", 1100)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish photo 600 BQADphotoid "+caption))

	waitForSent(t, mock, "caption is too long")
	assertNoMediaSends(t, mock)
}

func TestPublishRejectsLocalPathsAndBadRefs(t *testing.T) {
	for _, ref := range []string{"/etc/passwd", "C:\\Users\\x.png", "./report.pdf", "docs/readme.md", "https:///no-host"} {
		t.Run(ref, func(t *testing.T) {
			b, mock := newPublishTestBot(t, nil, nil)

			postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish photo 600 "+ref))

			waitForSent(t, mock, "file_id or an http(s) URL")
			assertNoMediaSends(t, mock)
		})
	}
}

func TestPublishRejectsNoTargets(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)
	// Remove every registered target; then "all" must be rejected.
	for _, id := range b.store.PublishTargets() {
		if err := b.store.RemovePublishTarget(id); err != nil {
			t.Fatalf("RemovePublishTarget: %v", err)
		}
	}

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text all hi"))

	waitForSent(t, mock, "No chat is registered")
	assertNoMediaSends(t, mock)
}

func TestPublishUsageErrors(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"/publish", "Publish commands"},
		{"/publish text", "Publish commands"},
		{"/publish text 600", "Publish commands"},
		{"/publish photo 600", "file_id or an http(s) URL"},
		{"/publish song 600 abc", "Publish commands"},
		{"/publish text 600,abc hi", "Publish commands"},
		{"/publish text 600,, hi", "Publish commands"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			b, mock := newPublishTestBot(t, nil, nil)

			postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, tc.text))

			waitForSent(t, mock, tc.want)
			assertNoMediaSends(t, mock)
		})
	}
}

// --- registry commands -------------------------------------------------------

func TestPublishAddRegistersCurrentChat(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(800, publishAdmin, "/publish_add"))

	waitForSent(t, mock, "registered as a publish target")
	if !b.store.IsPublishTarget(800) {
		t.Fatal("chat 800 must be registered")
	}
}

func TestPublishAddAlreadyRegistered(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(600, publishAdmin, "/publish_add"))

	waitForSent(t, mock, "already a publish target")
}

func TestPublishAddDeniedForNonAdmin(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setMemberStatusFor(publishMember, "member")
	})

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(800, publishMember, "/publish_add"))

	waitForSent(t, mock, "Only group administrators")
	if b.store.IsPublishTarget(800) {
		t.Fatal("chat 800 must not be registered by a non-admin")
	}
}

func TestPublishAddAnonymousSenderIsRejected(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)

	upd := toJSON(map[string]any{
		"update_id": 6,
		"message": map[string]any{
			"message_id": 10,
			"chat":       map[string]any{"id": 800, "title": "group"},
			"text":       "/publish_add",
			"entities":   []map[string]any{{"type": "bot_command", "offset": 0, "length": 12}},
		},
	})
	postUpdate(t, b.Client().WebhookHandler(), upd)

	waitForSent(t, mock, "Only group administrators")
	if b.store.IsPublishTarget(800) {
		t.Fatal("chat 800 must not be registered anonymously")
	}
}

func TestPublishRemoveUnregistersChat(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(600, publishAdmin, "/publish_remove"))

	waitForSent(t, mock, "no longer a publish target")
	if b.store.IsPublishTarget(600) {
		t.Fatal("chat 600 must be unregistered")
	}
}

func TestPublishRemoveWhenNotRegistered(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(900, publishAdmin, "/publish_remove"))

	waitForSent(t, mock, "not a publish target")
}

// A private chat must not become a publish target even when its "admin"
// check passes in the mock.
func TestPublishAddRejectsPrivateChat(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)

	upd := toJSON(map[string]any{
		"update_id": 7,
		"message": map[string]any{
			"message_id": 10,
			"from":       map[string]any{"id": publishAdmin, "first_name": "admin"},
			"chat":       map[string]any{"id": 800, "type": "private", "first_name": "admin"},
			"text":       "/publish_add",
			"entities":   []map[string]any{{"type": "bot_command", "offset": 0, "length": 12}},
		},
	})
	postUpdate(t, b.Client().WebhookHandler(), upd)

	waitForSent(t, mock, "Private chats cannot be publish targets")
	if b.store.IsPublishTarget(800) {
		t.Fatal("private chat 800 must not be registered")
	}
}

// --- localization and record ------------------------------------------------

func TestPublishRepliesInChineseWhenChatIsChinese(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)
	b.store.SetLanguage(500, "zh")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600 你好群"))

	waitForDelivered(t, mock, "你好群", 1)
	waitForSent(t, mock, "发布完成")
}

func TestPublishRecordContainsTargetsAndResults(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600,700 news"))

	waitForDelivered(t, mock, "news", 2)
	// publishToTargets sends to the targets before AddPublish persists the
	// record, so the record may not exist yet when the last delivery is
	// observed. Poll until the record is stored instead of reading it
	// immediately (MYC-93).
	waitFor(t, 2*time.Second, func() bool {
		return len(b.store.PublishRecords(0, 0)) > 0
	})
	recs := b.store.PublishRecords(0, 0)
	if len(recs) != 1 {
		t.Fatalf("records = %+v", recs)
	}
	r := recs[0]
	if r.SourceChatID != 500 || r.SenderID != publishAdmin || r.Kind != "text" {
		t.Fatalf("record = %+v", r)
	}
	if len(r.Targets) != 2 || r.Targets[0] != 600 || r.Targets[1] != 700 {
		t.Fatalf("targets = %v", r.Targets)
	}
	if !r.Results[0].Success || !r.Results[1].Success {
		t.Fatalf("results = %+v", r.Results)
	}
	if r.CreatedAt.IsZero() {
		t.Fatal("record timestamp missing")
	}
}

// make sure the store's PublishResult type flows through the summary untouched
func TestPublishSummaryMapsReasonCodes(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)
	mock.setMediaErr("video", errString("Bad Request: media invalid"))

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish video 600 BQACvid1"))

	var summary string
	waitFor(t, 2*time.Second, func() bool {
		for _, s := range mock.sent() {
			if strings.Contains(s["text"].(string), "\u2717") {
				summary = s["text"].(string)
				return true
			}
		}
		return false
	})
	if !strings.Contains(summary, "\u2717 600: send failed") {
		t.Fatalf("summary = %q", summary)
	}
}
