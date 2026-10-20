package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// syncBuffer is a goroutine-safe log sink: webhook updates are processed
// asynchronously, so the test reads the log while handlers may still write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

const secretPayload = "SECRET-UPDATE-PAYLOAD-7f3a"

// serverDescription stands in for the dynamic Description that the Telegram
// server puts into TooManyRequestsError and MigrateError messages.
const serverDescription = "SERVER-DESCRIPTION-9f3b"

// A decode failure as the library builds it: the raw update is embedded in the
// error text, and the JSON error is wrapped.
func libraryDecodeError(t *testing.T) error {
	t.Helper()
	var upd models.Update
	jsonErr := json.Unmarshal([]byte(`{"message":`+secretPayload+`}`), &upd)
	if jsonErr == nil {
		t.Fatal("expected a JSON decode error")
	}
	return fmt.Errorf("error decode update, %s, %w", `{"text":"`+secretPayload+`"}`, jsonErr)
}

// libraryTooManyRequestsError builds a TooManyRequestsError exactly as the
// library does in raw_request.go: the server Description lands in Message.
func libraryTooManyRequestsError() *tgbot.TooManyRequestsError {
	return &tgbot.TooManyRequestsError{
		Message:    fmt.Sprintf("%s, %s", tgbot.ErrorTooManyRequests, serverDescription),
		RetryAfter: 17,
	}
}

// libraryMigrateError builds a MigrateError exactly as the library does in
// raw_request.go: the server Description lands in Message.
func libraryMigrateError() *tgbot.MigrateError {
	return &tgbot.MigrateError{
		Message:         fmt.Sprintf("%s: %s", tgbot.ErrorBadRequest, serverDescription),
		MigrateToChatID: -1002003004005,
	}
}

func TestLibraryErrorsHandlerNeverLogsRawPayload(t *testing.T) {
	var out syncBuffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	handler := newLibraryErrorsHandler(log)

	handler(libraryDecodeError(t))
	handler(fmt.Errorf("some updates lost, ctx done"))
	handler(nil)

	got := out.String()
	if strings.Contains(got, secretPayload) {
		t.Fatalf("library error log leaked the raw update payload:\n%s", got)
	}
	if strings.Contains(got, "cause=") {
		t.Fatalf("library error log must not carry a cause field:\n%s", got)
	}
	if !strings.Contains(got, "op=\"error decode update\"") {
		t.Errorf("expected a stable operation label, got:\n%s", got)
	}
	if !strings.Contains(got, "op=\"some updates lost\"") {
		t.Errorf("expected the fixed-text error to be reduced to its operation label, got:\n%s", got)
	}
}

// Only whitelisted fields may be logged: op, category and numeric fields.
func TestLibraryErrorAttrsLogsWhitelistedFieldsOnly(t *testing.T) {
	op, attrs := libraryErrorAttrs(libraryDecodeError(t))
	if op != "error decode update" {
		t.Errorf("op = %q, want %q", op, "error decode update")
	}
	for _, attr := range append([]any{"op", op}, attrs...) {
		if s, ok := attr.(string); ok && strings.Contains(s, secretPayload) {
			t.Errorf("attr %q leaked the payload", s)
		}
	}
	if len(attrs) != 2 || attrs[0] != "category" {
		t.Errorf("attrs = %v, want a single category field", attrs)
	}
	if attrs[1] != "other" {
		t.Errorf("category = %v, want other", attrs[1])
	}
}

// MYC-92: the 429 error carries a server-provided Description in Message.
// Neither the wrapped polling path nor the unwrapped error may log it; only
// retry_after is whitelisted.
func TestLibraryErrorsHandlerRedactsTooManyRequestsDescription(t *testing.T) {
	var out syncBuffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	handler := newLibraryErrorsHandler(log)

	handler(fmt.Errorf("error get updates, %w", libraryTooManyRequestsError()))
	handler(libraryTooManyRequestsError())

	got := out.String()
	if strings.Contains(got, serverDescription) {
		t.Fatalf("429 log leaked the server Description:\n%s", got)
	}
	if !strings.Contains(got, "category=too_many_requests") {
		t.Errorf("expected the too_many_requests category, got:\n%s", got)
	}
	if !strings.Contains(got, "retry_after=17") {
		t.Errorf("expected the whitelisted retry_after field, got:\n%s", got)
	}
	if !strings.Contains(got, `op="error get updates"`) || !strings.Contains(got, `op="too many requests"`) {
		t.Errorf("expected stable operation labels, got:\n%s", got)
	}
}

// MYC-92: the migrate error carries a server-provided Description in Message.
// Only migrate_to_chat_id is whitelisted.
func TestLibraryErrorsHandlerRedactsMigrateDescription(t *testing.T) {
	var out syncBuffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	handler := newLibraryErrorsHandler(log)

	handler(fmt.Errorf("error do request for method sendMessage, %w", libraryMigrateError()))
	handler(libraryMigrateError())

	got := out.String()
	if strings.Contains(got, serverDescription) {
		t.Fatalf("migrate log leaked the server Description:\n%s", got)
	}
	if !strings.Contains(got, "category=migrate") {
		t.Errorf("expected the migrate category, got:\n%s", got)
	}
	if !strings.Contains(got, "migrate_to_chat_id=-1002003004005") {
		t.Errorf("expected the whitelisted migrate_to_chat_id field, got:\n%s", got)
	}
	if !strings.Contains(got, `op="error do request for method sendMessage"`) || !strings.Contains(got, `op="telegram library error"`) {
		t.Errorf("expected stable operation labels, got:\n%s", got)
	}
}

// MYC-92: errors without a wrapped cause are no longer logged verbatim. Their
// text can embed a server-provided Description, so only the fixed prefix
// before the first ", " is kept, and text without a separator is reduced to a
// generic label.
func TestLibraryErrorsHandlerRedactsUnwrappedDynamicError(t *testing.T) {
	var out syncBuffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	handler := newLibraryErrorsHandler(log)

	handler(fmt.Errorf("error response from telegram for method getUpdates, 429 %s", serverDescription))
	handler(fmt.Errorf("%w, %s", tgbot.ErrorForbidden, serverDescription))
	handler(errors.New("invalid webhook secret token received from update"))

	got := out.String()
	if strings.Contains(got, serverDescription) {
		t.Fatalf("unwrapped error log leaked the server Description:\n%s", got)
	}
	if strings.Contains(got, "invalid webhook secret token") {
		t.Fatalf("separator-less error text must be reduced to a generic label:\n%s", got)
	}
	if !strings.Contains(got, `op="error response from telegram for method getUpdates"`) {
		t.Errorf("expected the fixed operation label, got:\n%s", got)
	}
	if !strings.Contains(got, "category=forbidden") {
		t.Errorf("expected the forbidden category for the wrapped sentinel, got:\n%s", got)
	}
}

func TestLibraryDebugHandlerLogsFormatOnly(t *testing.T) {
	var out syncBuffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	newLibraryDebugHandler(log)("request url: %s, payload: %s", "https://api/bot***/sendMessage", secretPayload)

	got := out.String()
	if strings.Contains(got, secretPayload) {
		t.Fatalf("library debug log leaked an argument:\n%s", got)
	}
	if !strings.Contains(got, "request url: %s, payload: %s") {
		t.Errorf("expected the constant format string, got:\n%s", got)
	}
}

func TestLibraryDefaultHandlerLogsUpdateIDOnly(t *testing.T) {
	var out syncBuffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	upd := &models.Update{ID: 42, Message: &models.Message{Text: secretPayload}}
	newLibraryDefaultHandler(log)(context.Background(), nil, upd)

	got := out.String()
	if strings.Contains(got, secretPayload) {
		t.Fatalf("default update handler leaked message text:\n%s", got)
	}
	if !strings.Contains(got, "update_id=42") {
		t.Errorf("expected update_id in log, got:\n%s", got)
	}
}

// log_level=debug must not turn on library dumps: the bot never sets
// tgbot.WithDebug, so no request or webhook body reaches the debug log. The
// test drives a real webhook update through a debug-level logger and checks
// the captured output.
func TestDebugLogLevelDoesNotDumpUpdates(t *testing.T) {
	mock := newMockTelegram(t)
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)

	var out syncBuffer
	cfg := testConfig(srv.URL)
	cfg.LogLevel = "debug"
	logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: cfg.SlogLevel()}))
	b, err := New(cfg, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Client().StartWebhook(ctx)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, actorMember, "/start "+secretPayload))

	waitFor(t, 2*time.Second, func() bool { return len(mock.sent()) > 0 })

	got := out.String()
	if strings.Contains(got, secretPayload) {
		t.Fatalf("debug log level leaked update content:\n%s", got)
	}
	if strings.Contains(got, "telegram library debug event") {
		t.Fatalf("library debug mode is enabled; it must stay off:\n%s", got)
	}
}

// The bot must never enable library debug mode. This guard keeps the
// tgbot.WithDebug option from being added back silently.
func TestLibraryDebugModeIsNotEnabled(t *testing.T) {
	src, err := os.ReadFile("telegram.go")
	if err != nil {
		t.Fatalf("read telegram.go: %v", err)
	}
	if strings.Contains(string(src), "tgbot.WithDebug()") {
		t.Fatal("telegram.go enables tgbot.WithDebug; library request and webhook bodies would be logged")
	}
}
