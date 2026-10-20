package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/viceor/tele-admin/internal/config"
	"github.com/viceor/tele-admin/internal/i18n"
)

// startTestBot wires a Bot against a fresh mock Telegram server and starts the
// webhook consumer. It returns the bot; the caller posts updates through
// b.Client().WebhookHandler().
func startTestBot(t *testing.T, cfg *config.Config, mock *mockTelegram) *Bot {
	t.Helper()
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	cfg.APIServerURL = srv.URL

	b, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Client().StartWebhook(ctx)
	return b
}

func waitForSent(t *testing.T, mock *mockTelegram, substr string) {
	t.Helper()
	waitFor(t, 2*time.Second, func() bool {
		for _, m := range mock.sent() {
			if strings.Contains(m["text"].(string), substr) {
				return true
			}
		}
		return false
	})
}

func countSent(mock *mockTelegram, substr string) int {
	var n int
	for _, m := range mock.sent() {
		if strings.Contains(m["text"].(string), substr) {
			n++
		}
	}
	return n
}

func assertNoSent(t *testing.T, mock *mockTelegram, substr string, d time.Duration) {
	t.Helper()
	time.Sleep(d)
	if n := countSent(mock, substr); n != 0 {
		t.Fatalf("expected no message containing %q, got %d", substr, n)
	}
}

func TestWebhookHelpCommand(t *testing.T) {
	mock := newMockTelegram(t)
	b := startTestBot(t, testConfig(""), mock)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/help"))

	waitForSent(t, mock, "Available commands")
}

// TestWebhookUsesConfiguredDefaultLanguage verifies that i18n.default_language
// and timezone.default reach the Store so a brand-new chat starts with them.
func TestWebhookUsesConfiguredDefaultLanguage(t *testing.T) {
	mock := newMockTelegram(t)
	cfg := testConfig("")
	cfg.I18N.DefaultLanguage = "zh"
	cfg.Timezone.Default = "Asia/Shanghai"
	b := startTestBot(t, cfg, mock)

	snap := b.store.Settings(999)
	if snap.Language != i18n.Chinese {
		t.Errorf("new chat language = %v, want Chinese", snap.Language)
	}
	if snap.Timezone != "Asia/Shanghai" {
		t.Errorf("new chat timezone = %q, want Asia/Shanghai", snap.Timezone)
	}

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/help"))
	waitForSent(t, mock, "可用命令")
}

// TestWebhookWelcomeRetriesAfterSendFailure verifies a failed Telegram send is
// not recorded as welcomed, so a later event retries and succeeds.
func TestWebhookWelcomeRetriesAfterSendFailure(t *testing.T) {
	mock := newMockTelegram(t)
	b := startTestBot(t, testConfig(""), mock)

	mock.setSendErr(errors.New("flood control"))
	postUpdate(t, b.Client().WebhookHandler(), joinUpdate(500, 42, "bob"))
	assertNoSent(t, mock, "Welcome", 300*time.Millisecond)
	if b.store.WasWelcomed(500, 42) {
		t.Fatal("a failed send must not mark the member as welcomed")
	}

	mock.setSendErr(nil)
	postUpdate(t, b.Client().WebhookHandler(), joinUpdate(500, 42, "bob"))
	waitForSent(t, mock, "Welcome")
	waitFor(t, 2*time.Second, func() bool { return b.store.WasWelcomed(500, 42) })
}

func TestWebhookCommandDispatchesToHandlerNotGovernance(t *testing.T) {
	mock := newMockTelegram(t)
	cfg := testConfig("")
	cfg.Keywords.Blocked = []string{"start"}
	b := startTestBot(t, cfg, mock)

	// "/start" contains the blocked keyword "start". Command routing must win,
	// so the keyword rule must not delete it.
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/start"))

	waitForSent(t, mock, "/start")
	time.Sleep(100 * time.Millisecond)
	if n := len(mock.deleted()); n != 0 {
		t.Fatalf("command message was deleted by governance (%d deletions)", n)
	}
}

func TestWebhookWelcomeCommandAdmin(t *testing.T) {
	mock := newMockTelegram(t)
	mock.setMemberStatus("administrator")
	b := startTestBot(t, testConfig(""), mock)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/welcome"))

	waitForSent(t, mock, "Welcome")
}

func TestWebhookWelcomeCommandDeniedForNonAdmin(t *testing.T) {
	mock := newMockTelegram(t)
	mock.setMemberStatus("member")
	b := startTestBot(t, testConfig(""), mock)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/welcome"))

	waitForSent(t, mock, "administrator")
}

func TestWebhookLangMissingArgument(t *testing.T) {
	mock := newMockTelegram(t)
	mock.setMemberStatus("administrator")
	b := startTestBot(t, testConfig(""), mock)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/lang"))

	waitForSent(t, mock, "usage: /lang")
}

func TestWebhookLangUnsupported(t *testing.T) {
	mock := newMockTelegram(t)
	mock.setMemberStatus("administrator")
	cfg := testConfig("")
	cfg.I18N.Supported = []string{"en"}
	b := startTestBot(t, cfg, mock)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/lang zh"))

	waitForSent(t, mock, "unsupported language")
}

func TestWebhookTimezoneSet(t *testing.T) {
	mock := newMockTelegram(t)
	mock.setMemberStatus("administrator")
	b := startTestBot(t, testConfig(""), mock)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/tz Asia/Shanghai"))

	waitForSent(t, mock, "Timezone set to Asia/Shanghai")
}

func TestWebhookTimezoneInvalid(t *testing.T) {
	mock := newMockTelegram(t)
	mock.setMemberStatus("administrator")
	b := startTestBot(t, testConfig(""), mock)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/tz Not/AZone"))

	waitForSent(t, mock, "invalid IANA timezone")
}

func TestWebhookTimezoneMissingArgument(t *testing.T) {
	mock := newMockTelegram(t)
	mock.setMemberStatus("administrator")
	b := startTestBot(t, testConfig(""), mock)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/tz"))

	waitForSent(t, mock, "usage: /tz")
}

func TestWebhookAdminCheckErrorDeniesCommand(t *testing.T) {
	mock := newMockTelegram(t)
	mock.setMemberErr(errors.New("network down"))
	b := startTestBot(t, testConfig(""), mock)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/lang zh"))

	assertNoSent(t, mock, "Language set", 400*time.Millisecond)
}

func TestWebhookWelcomeDisabled(t *testing.T) {
	mock := newMockTelegram(t)
	cfg := testConfig("")
	cfg.Welcome.Enabled = false
	b := startTestBot(t, cfg, mock)

	postUpdate(t, b.Client().WebhookHandler(), joinUpdate(500, 42, "bob"))

	assertNoSent(t, mock, "Welcome", 400*time.Millisecond)
}

func TestWebhookWelcomeCustomMessage(t *testing.T) {
	mock := newMockTelegram(t)
	cfg := testConfig("")
	cfg.Welcome.Message = "hi {name}, welcome"
	b := startTestBot(t, cfg, mock)

	postUpdate(t, b.Client().WebhookHandler(), joinUpdate(500, 42, "bob"))

	waitForSent(t, mock, "hi bob, welcome")
}

func TestWebhookWelcomeSkipsBots(t *testing.T) {
	mock := newMockTelegram(t)
	b := startTestBot(t, testConfig(""), mock)

	postUpdate(t, b.Client().WebhookHandler(), botJoinUpdate(500, 42, "helper"))

	assertNoSent(t, mock, "Welcome", 400*time.Millisecond)
}

func TestWebhookWelcomeOnlyOncePerMember(t *testing.T) {
	mock := newMockTelegram(t)
	b := startTestBot(t, testConfig(""), mock)

	postUpdate(t, b.Client().WebhookHandler(), joinUpdate(500, 42, "bob"))
	waitForSent(t, mock, "Welcome")

	postUpdate(t, b.Client().WebhookHandler(), joinUpdate(500, 42, "bob"))
	time.Sleep(300 * time.Millisecond)

	if n := countSent(mock, "Welcome"); n != 1 {
		t.Fatalf("expected exactly 1 welcome, got %d", n)
	}
}

func TestWebhookBlockedKeywordSendsNotice(t *testing.T) {
	mock := newMockTelegram(t)
	b := startTestBot(t, testConfig(""), mock)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "buy cheap spam now"))

	waitForSent(t, mock, "blocked term")
	waitFor(t, 2*time.Second, func() bool { return len(mock.deleted()) > 0 })
}

func TestWebhookCleanMessageNotDeleted(t *testing.T) {
	mock := newMockTelegram(t)
	b := startTestBot(t, testConfig(""), mock)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "just chatting normally"))

	time.Sleep(200 * time.Millisecond)
	if n := len(mock.deleted()); n != 0 {
		t.Fatalf("clean message should not be deleted, got %d deletions", n)
	}
	if n := countSent(mock, "slow down"); n != 0 {
		t.Fatalf("clean single message should not warn, got %d warnings", n)
	}
}

func botJoinUpdate(chatID int64, newID int64, name string) string {
	return toJSON(map[string]any{
		"update_id": 2,
		"message": map[string]any{
			"message_id": 11,
			"from":       map[string]any{"id": 999, "first_name": "inviter"},
			"chat":       map[string]any{"id": chatID},
			"new_chat_members": []map[string]any{
				{"id": newID, "first_name": name, "is_bot": true},
			},
		},
	})
}
