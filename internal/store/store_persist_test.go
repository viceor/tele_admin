package store

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/viceor/tele-admin/internal/i18n"
)

// TestStorePersistChatSettingsAcrossRestart is the acceptance test for
// persistence: a store is closed, a fresh store opens the same database file,
// and the chat settings are still readable.
func TestStorePersistChatSettingsAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")

	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	s1 := NewWithDefaults(20, 0, i18n.English, "UTC")
	if err := s1.AttachFileStore(fs, nil); err != nil {
		t.Fatalf("AttachFileStore: %v", err)
	}

	s1.SetLanguage(500, i18n.Chinese)
	s1.SetTimezone(500, "Asia/Shanghai")
	s1.MarkWelcomed(500, 7)
	if !s1.BeginWelcome(500, 8) {
		t.Fatal("BeginWelcome should win for user 8")
	}
	s1.CompleteWelcome(500, 8)
	s1.MarkWelcomed(500, 9)

	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen OpenFileStore: %v", err)
	}
	defer reopened.Close()

	s2 := NewWithDefaults(20, 0, i18n.English, "UTC")
	if err := s2.AttachFileStore(reopened, nil); err != nil {
		t.Fatalf("AttachFileStore after restart: %v", err)
	}

	if got := s2.Language(500); got != i18n.Chinese {
		t.Errorf("Language after restart = %v, want Chinese", got)
	}
	if got := s2.Timezone(500); got != "Asia/Shanghai" {
		t.Errorf("Timezone after restart = %q, want Asia/Shanghai", got)
	}
	for _, id := range []int64{7, 8, 9} {
		if !s2.WasWelcomed(500, id) {
			t.Errorf("WasWelcomed(500, %d) after restart = false, want true", id)
		}
	}
	// A chat that was never configured keeps the built-in defaults.
	if got := s2.Language(999); got != i18n.English {
		t.Errorf("Language for unknown chat = %v, want English default", got)
	}
}

func TestStoreDoubleAttachFails(t *testing.T) {
	s := New(20, 0)
	fs1, err := OpenFileStore(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatalf("OpenFileStore a: %v", err)
	}
	defer fs1.Close()
	if err := s.AttachFileStore(fs1, nil); err != nil {
		t.Fatalf("first AttachFileStore: %v", err)
	}
	fs2, err := OpenFileStore(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatalf("OpenFileStore b: %v", err)
	}
	defer fs2.Close()
	if err := s.AttachFileStore(fs2, nil); err == nil {
		t.Error("second AttachFileStore should fail")
	}
}

// TestStorePersistWriteFailureKeepsInMemoryState verifies the R1b error
// propagation contract: a failed write-through is reported to the caller, does
// not leave the in-memory state ahead of the disk, and the next successful
// mutation persists the full record.
func TestStorePersistWriteFailureKeepsInMemoryState(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	defer fs.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	s := NewWithDefaults(20, 0, i18n.English, "UTC")
	if err := s.AttachFileStore(fs, logger); err != nil {
		t.Fatalf("AttachFileStore: %v", err)
	}

	// Block the commit path for the next write: a directory at the database
	// path makes the final rename fail (works even as root).
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("remove database file: %v", err)
	}
	if err := os.Mkdir(dbPath, 0o700); err != nil {
		t.Fatalf("create blocking directory: %v", err)
	}
	err = s.SetLanguage(500, i18n.Chinese)
	if err == nil {
		t.Fatal("SetLanguage must fail when persistence fails")
	}
	if !errors.Is(err, ErrPersistFailed) {
		t.Errorf("SetLanguage error %v, want ErrPersistFailed wrap", err)
	}
	if got := s.Language(500); got != i18n.English {
		t.Errorf("in-memory Language after failed persist = %v, want the old value en", got)
	}
	if !bytes.Contains(buf.Bytes(), []byte("persist chat settings failed")) {
		t.Errorf("expected a loud persistence failure log, got: %s", buf.String())
	}

	// Remove the blocker; the next mutation persists the full record,
	// including the timezone. The language whose earlier write failed was
	// rolled back, so it is not on disk either.
	if err := os.RemoveAll(dbPath); err != nil {
		t.Fatalf("remove blocker: %v", err)
	}
	if err := s.SetTimezone(500, "Asia/Shanghai"); err != nil {
		t.Fatalf("SetTimezone after unblocking: %v", err)
	}

	var rec persistedChatRecord
	ok, err := fs.Get("chats", "500", &rec)
	if err != nil || !ok {
		t.Fatalf("Get chats/500 = (%v, %v), want (true, nil)", ok, err)
	}
	if rec.Language != "en" || rec.Timezone != "Asia/Shanghai" {
		t.Errorf("persisted record = %+v, want language en (rolled back) and timezone Asia/Shanghai", rec)
	}
}

// TestStoreWithoutPersistenceIsUnchanged verifies that a Store with no
// attached FileStore behaves exactly as the in-memory implementation: no
// file is written, and all existing behavior is intact.
func TestStoreWithoutPersistenceIsUnchanged(t *testing.T) {
	s := NewWithDefaults(20, 0, i18n.English, "UTC")
	if s.FileStore() != nil {
		t.Fatal("FileStore should be nil before attach")
	}
	s.SetLanguage(100, i18n.Chinese)
	s.SetTimezone(100, "Asia/Shanghai")
	s.MarkWelcomed(100, 7)
	if got := s.Language(100); got != i18n.Chinese {
		t.Errorf("Language = %v, want Chinese", got)
	}
	if !s.WasWelcomed(100, 7) {
		t.Error("Welcomed should be recorded in memory")
	}
}

// TestStorePersistDeterministicWelcomedOrder checks the persisted welcomed
// list is sorted, so the on-disk form does not depend on map iteration order.
func TestStorePersistDeterministicWelcomedOrder(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	defer fs.Close()
	s := NewWithDefaults(20, 0, i18n.English, "UTC")
	if err := s.AttachFileStore(fs, nil); err != nil {
		t.Fatalf("AttachFileStore: %v", err)
	}
	s.MarkWelcomed(100, 42)
	s.MarkWelcomed(100, 5)
	s.MarkWelcomed(100, 1000)

	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database: %v", err)
	}
	want := []byte(`{"schema_version":1,"data":{"chats":{"100":{"chat_id":100,"language":"en","timezone":"UTC","welcomed":[5,42,1000]}}}}`)
	if !bytes.Equal(raw, want) {
		t.Errorf("database content = %s, want %s", raw, want)
	}
}
