package bot

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/viceor/tele-admin/internal/config"
)

func newTestBot(t *testing.T, cfg *config.Config) (*Bot, *mockTelegram) {
	t.Helper()
	mock := newMockTelegram(t)
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	cfg.APIServerURL = srv.URL
	b, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b, mock
}

func startWebhookConsumer(t *testing.T, b *Bot) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Client().StartWebhook(ctx)
}

func postUpdateWithToken(t *testing.T, h http.HandlerFunc, updateJSON, token string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(updateJSON))
	if token != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
}

// anonymousMessageUpdate builds a text message without a From field, as
// Telegram sends for anonymous posts made on behalf of a chat.
func anonymousMessageUpdate(chatID int64, text string) string {
	message := map[string]any{
		"message_id": 30,
		"chat":       map[string]any{"id": chatID, "title": "group"},
		"text":       text,
	}
	if cmd, ok := commandToken(text); ok {
		message["entities"] = []map[string]any{
			{"type": "bot_command", "offset": 0, "length": len(cmd)},
		}
	}
	return toJSON(map[string]any{"update_id": 3, "message": message})
}

func TestNewWebhookRequiresSecretToken(t *testing.T) {
	cfg := testConfig("")
	cfg.Webhook.SecretToken = ""
	if _, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("expected error when webhook.secret_token is empty")
	}
}

func TestNewWebhookRejectsWildcardListen(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8443", "[::]:8443", ":8443", ""} {
		cfg := testConfig("")
		cfg.Webhook.Listen = listen
		if _, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
			t.Errorf("expected error for wildcard webhook.listen %q", listen)
		}
	}
}

func TestNewEnforceAdminFalseRejectedWithoutDevMode(t *testing.T) {
	t.Setenv(config.DevModeEnvVar, "")
	cfg := testConfig("")
	cfg.Admin.EnforceAdmin = false
	if _, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("expected error for enforce_admin=false outside development mode")
	}
}

func TestNewEnforceAdminFalseAllowedInDevMode(t *testing.T) {
	t.Setenv(config.DevModeEnvVar, "1")
	cfg := testConfig("")
	cfg.Admin.EnforceAdmin = false
	if _, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestWebhookHandlerRejectsMissingSecretToken(t *testing.T) {
	b, mock := newTestBot(t, testConfig(""))
	startWebhookConsumer(t, b)

	postUpdateWithToken(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/start"), "")

	assertNoSent(t, mock, "/start", 400*time.Millisecond)
}

func TestWebhookHandlerRejectsWrongSecretToken(t *testing.T) {
	b, mock := newTestBot(t, testConfig(""))
	startWebhookConsumer(t, b)

	postUpdateWithToken(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/start"), "wrong-secret")

	assertNoSent(t, mock, "/start", 400*time.Millisecond)
}

func TestWebhookHandlerAcceptsValidSecretToken(t *testing.T) {
	b, mock := newTestBot(t, testConfig(""))
	startWebhookConsumer(t, b)

	postUpdateWithToken(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/start"), "test-secret")

	waitForSent(t, mock, "/start")
}

func TestAnonymousSenderRejectedForAdminCommands(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		reply string
	}{
		{"welcome", "/welcome", "Welcome"},
		{"lang", "/lang zh", "Language set"},
		{"tz", "/tz Asia/Shanghai", "Timezone set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, mock := newTestBot(t, testConfig(""))
			startWebhookConsumer(t, b)

			postUpdate(t, b.Client().WebhookHandler(), anonymousMessageUpdate(500, tc.text))

			waitForSent(t, mock, "administrator")
			assertNoSent(t, mock, tc.reply, 300*time.Millisecond)
		})
	}
}

func TestAnonymousSenderGovernanceDoesNotPanic(t *testing.T) {
	b, mock := newTestBot(t, testConfig(""))
	startWebhookConsumer(t, b)

	postUpdate(t, b.Client().WebhookHandler(), anonymousMessageUpdate(500, "hello"))

	assertNoSent(t, mock, "slow down", 300*time.Millisecond)
}

func TestOwnerBypassesChatAdminCheck(t *testing.T) {
	cfg := testConfig("")
	cfg.Admin.OwnerIDs = []int64{7}
	b, mock := newTestBot(t, cfg)
	mock.setMemberStatus("member")
	startWebhookConsumer(t, b)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/lang zh"))

	waitForSent(t, mock, "语言")
}

func TestNonOwnerMemberStillDenied(t *testing.T) {
	cfg := testConfig("")
	cfg.Admin.OwnerIDs = []int64{42}
	b, mock := newTestBot(t, cfg)
	mock.setMemberStatus("member")
	startWebhookConsumer(t, b)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, 7, "/lang zh"))

	waitForSent(t, mock, "administrator")
}
