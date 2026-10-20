package bot

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viceor/tele-admin/internal/config"
)

func testConfigWithDB(serverURL, dbPath string) *config.Config {
	cfg := testConfig(serverURL)
	cfg.DB.Path = dbPath
	return cfg
}

// TestNewInMemoryModeLogsAnnotation covers the acceptance criterion that a
// service started without db.path stays in-memory and says so at startup.
func TestNewInMemoryModeLogsAnnotation(t *testing.T) {
	mock := newMockTelegram(t)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	b, err := New(testConfig(srv.URL), logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.store.FileStore() != nil {
		t.Fatal("no FileStore should be attached when db.path is empty")
	}
	out := buf.String()
	if !strings.Contains(out, "in-memory") {
		t.Errorf("startup log should annotate in-memory mode, got: %s", out)
	}
	if err := b.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestNewFileModeLogsPersistence verifies that a configured db.path opens the
// file backend and reports it at startup.
func TestNewFileModeLogsPersistence(t *testing.T) {
	mock := newMockTelegram(t)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "data", "teleadmin.db")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	b, err := New(testConfigWithDB(srv.URL, dbPath), logger)
	if err != nil {
		t.Fatalf("New with db.path: %v", err)
	}
	defer b.Close()
	if b.store.FileStore() == nil {
		t.Fatal("FileStore should be attached when db.path is configured")
	}
	out := buf.String()
	if !strings.Contains(out, "persistence enabled") {
		t.Errorf("startup log should report enabled persistence, got: %s", out)
	}
	if !strings.Contains(out, dbPath) {
		t.Errorf("startup log should name the database path, got: %s", out)
	}
}

// TestNewFailsWhenDatabaseIsLocked verifies fail-closed startup: when another
// process holds the database lock, New refuses to start instead of running on
// possibly-stale in-memory state.
func TestNewFailsWhenDatabaseIsLocked(t *testing.T) {
	mock := newMockTelegram(t)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	b1, err := New(testConfigWithDB(srv.URL, dbPath), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	defer b1.Close()

	_, err = New(testConfigWithDB(srv.URL, dbPath), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("second New on a locked database should fail")
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Errorf("second New error = %v, want a lock message", err)
	}
}

// TestPersistenceSurvivesRestart is the end-to-end acceptance test: language,
// timezone, and welcome state written by one process are still in force when
// a fresh process opens the same database file.
func TestPersistenceSurvivesRestart(t *testing.T) {
	mock := newMockTelegram(t)
	mock.setMemberStatus("administrator")
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")

	b1, err := New(testConfigWithDB(srv.URL, dbPath), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	go b1.client.StartWebhook(ctx1)

	postUpdate(t, b1.client.WebhookHandler(), textMessageUpdate(500, 7, "/lang zh"))
	waitFor(t, 2*time.Second, func() bool {
		for _, m := range mock.sent() {
			if strings.Contains(m["text"].(string), "语言已设置") {
				return true
			}
		}
		return false
	})
	postUpdate(t, b1.client.WebhookHandler(), textMessageUpdate(500, 7, "/tz Asia/Shanghai"))
	waitFor(t, 2*time.Second, func() bool {
		for _, m := range mock.sent() {
			if strings.Contains(m["text"].(string), "时区已设置为 Asia/Shanghai") {
				return true
			}
		}
		return false
	})
	postUpdate(t, b1.client.WebhookHandler(), joinUpdate(500, 42, "bob"))
	// The chat language is Chinese at this point, so the welcome text is the
	// catalog's Chinese string ("欢迎 @name！…"), not the English "Welcome".
	waitFor(t, 2*time.Second, func() bool {
		for _, m := range mock.sent() {
			if strings.Contains(m["text"].(string), "欢迎") {
				return true
			}
		}
		return false
	})

	cancel1()
	if err := b1.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// Fresh process against the same database file, with its own mock server.
	mock2 := newMockTelegram(t)
	mock2.setMemberStatus("administrator")
	srv2 := httptest.NewServer(mock2.handler())
	defer srv2.Close()

	b2, err := New(testConfigWithDB(srv2.URL, dbPath), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer b2.Close()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go b2.client.StartWebhook(ctx2)

	// The timezone confirmation comes back in Chinese: the language choice
	// survived the restart.
	postUpdate(t, b2.client.WebhookHandler(), textMessageUpdate(500, 7, "/tz Asia/Shanghai"))
	waitFor(t, 2*time.Second, func() bool {
		for _, m := range mock2.sent() {
			if strings.Contains(m["text"].(string), "时区已设置为 Asia/Shanghai") {
				return true
			}
		}
		return false
	})

	// The welcome for member 42 is not re-sent: the welcomed set survived too.
	// Any re-sent welcome would be in Chinese because the language choice
	// persisted, so the assertion matches the Chinese catalog text.
	postUpdate(t, b2.client.WebhookHandler(), joinUpdate(500, 42, "bob"))
	for _, m := range mock2.sent() {
		if strings.Contains(m["text"].(string), "欢迎") {
			t.Error("welcome was re-sent after restart although it was already persisted")
		}
	}
}
