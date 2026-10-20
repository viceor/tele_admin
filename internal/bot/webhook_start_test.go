package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/viceor/tele-admin/internal/config"
)

func newBotForStart(t *testing.T, cfg *config.Config, mock *mockTelegram) *Bot {
	t.Helper()
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	cfg.APIServerURL = srv.URL
	b, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestStartRegistersWebhookWithPublicURLAndSecret(t *testing.T) {
	mock := newMockTelegram(t)
	cfg := testConfig("")
	cfg.Webhook.Listen = "127.0.0.1:0"
	cfg.Webhook.PublicURL = "https://bot.example.com/hook"
	cfg.Webhook.SecretToken = "s3cret"
	b := newBotForStart(t, cfg, mock)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()

	waitFor(t, 2*time.Second, func() bool {
		calls, _, _ := mock.webhookRegistration()
		return calls >= 1
	})
	calls, url, secret := mock.webhookRegistration()
	if calls != 1 {
		t.Errorf("setWebhook calls = %d, want 1", calls)
	}
	if url != cfg.Webhook.PublicURL {
		t.Errorf("setWebhook url = %q, want %q", url, cfg.Webhook.PublicURL)
	}
	if secret != "s3cret" {
		t.Errorf("setWebhook secret = %q, want s3cret", secret)
	}

	cancel()
	<-done
}

func TestStartFailsWhenSetWebhookFails(t *testing.T) {
	mock := newMockTelegram(t)
	mock.setSetWebhookErr(errors.New("bad webhook"))
	cfg := testConfig("")
	cfg.Webhook.Listen = "127.0.0.1:0"
	b := newBotForStart(t, cfg, mock)

	err := b.Start(context.Background())
	if err == nil {
		t.Fatal("Start must fail when setWebhook fails")
	}
	if !strings.Contains(err.Error(), "setWebhook") {
		t.Fatalf("error = %v, want setWebhook context", err)
	}
}

func TestStartFailsWhenListenAddressInUse(t *testing.T) {
	mock := newMockTelegram(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer ln.Close()

	cfg := testConfig("")
	cfg.Webhook.Listen = ln.Addr().String()
	b := newBotForStart(t, cfg, mock)

	if err := b.Start(context.Background()); err == nil {
		t.Fatal("Start must fail when the listen address is already in use")
	} else if !strings.Contains(err.Error(), "webhook listen") {
		t.Fatalf("error = %v, want webhook listen context", err)
	}
	if calls, _, _ := mock.webhookRegistration(); calls != 0 {
		t.Errorf("setWebhook calls = %d, want 0 so the remote registration is unchanged", calls)
	}
}

func TestStartClosesListenerWhenSetWebhookFails(t *testing.T) {
	mock := newMockTelegram(t)
	mock.setSetWebhookErr(errors.New("bad webhook"))

	// Reserve a free loopback port, then release it so the bot can bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}

	cfg := testConfig("")
	cfg.Webhook.Listen = addr
	b := newBotForStart(t, cfg, mock)

	if err := b.Start(context.Background()); err == nil {
		t.Fatal("Start must fail when setWebhook fails")
	}
	if calls, _, _ := mock.webhookRegistration(); calls != 1 {
		t.Fatalf("setWebhook calls = %d, want 1", calls)
	}

	// The listener must be closed after setWebhook fails; rebinding the same
	// address proves no listener is left running.
	rebound, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listener was not closed after setWebhook failure: %v", err)
	}
	_ = rebound.Close()
}

func TestNewRejectsNonHTTPSPublicURL(t *testing.T) {
	cfg := testConfig("")
	cfg.Webhook.PublicURL = "http://example.com/webhook"
	if _, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("New must reject a non-HTTPS webhook.public_url")
	}
}

func TestNewRejectsPublicListen(t *testing.T) {
	for _, listen := range []string{"8.8.8.8:8443", "example.com:8443"} {
		cfg := testConfig("")
		cfg.Webhook.Listen = listen
		if _, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
			t.Errorf("New must reject non-loopback/private webhook.listen %q", listen)
		}
	}
}
