package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/viceor/tele-admin/internal/i18n"
)

// blockDatabase replaces the database file with a directory, so the final
// rename at the heart of every FileStore commit fails while reads (served
// from the store's in-memory snapshot) keep working. This works even when the
// test runs as root, where read-only directories would not fail writes.
func blockDatabase(t *testing.T, dbPath string) {
	t.Helper()
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("remove database file before blocking: %v", err)
	}
	if err := os.Mkdir(dbPath, 0o700); err != nil {
		t.Fatalf("create blocking directory: %v", err)
	}
}

// unblockDatabase removes the blocker directory so commits work again.
func unblockDatabase(t *testing.T, dbPath string) {
	t.Helper()
	if err := os.RemoveAll(dbPath); err != nil {
		t.Fatalf("remove blocking directory: %v", err)
	}
}

func newFailureTestStore(t *testing.T) (*Store, *FileStore, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	s := NewWithDefaults(20, 0, i18n.English, "UTC")
	if err := s.AttachFileStore(fs, nil); err != nil {
		t.Fatalf("AttachFileStore: %v", err)
	}
	return s, fs, dbPath
}

// TestSetLanguageSetTimezonePersistFailure asserts MYC-51 #3: the mutation
// methods surface persistence failures, roll the memory change back, and the
// chat record on disk keeps its old values.
func TestSetLanguageSetTimezonePersistFailure(t *testing.T) {
	s, fs, dbPath := newFailureTestStore(t)
	// Seed persisted values so the failure test shows the old record stays.
	if err := s.SetTimezone(500, "Europe/Berlin"); err != nil {
		t.Fatalf("seed SetTimezone: %v", err)
	}
	blockDatabase(t, dbPath)
	defer unblockDatabase(t, dbPath)

	if err := s.SetLanguage(500, i18n.Chinese); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("SetLanguage error = %v, want ErrPersistFailed wrap", err)
	}
	if got := s.Language(500); got != i18n.English {
		t.Errorf("Language after failed persist = %v, want old value en", got)
	}
	if err := s.SetTimezone(500, "Asia/Shanghai"); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("SetTimezone error = %v, want ErrPersistFailed wrap", err)
	}
	if got := s.Timezone(500); got != "Europe/Berlin" {
		t.Errorf("Timezone after failed persist = %v, want old value Europe/Berlin", got)
	}

	var rec persistedChatRecord
	ok, err := fs.Get("chats", "500", &rec)
	if err != nil || !ok {
		t.Fatalf("Get chats/500 = (%v, %v)", ok, err)
	}
	if rec.Language != "en" || rec.Timezone != "Europe/Berlin" {
		t.Errorf("persisted record = %+v, want unchanged en/Europe-Berlin", rec)
	}
}

// TestMarkWelcomedAndCompleteWelcomePersistFailure asserts the welcomed set is
// not applied in memory when the chat record cannot be persisted.
func TestMarkWelcomedAndCompleteWelcomePersistFailure(t *testing.T) {
	s, _, dbPath := newFailureTestStore(t)
	blockDatabase(t, dbPath)
	defer unblockDatabase(t, dbPath)

	if err := s.MarkWelcomed(500, 7); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("MarkWelcomed error = %v, want ErrPersistFailed wrap", err)
	}
	if s.WasWelcomed(500, 7) {
		t.Error("welcomed flag must roll back when persistence fails")
	}

	// CompleteWelcome rolls the flag back too and re-opens the in-flight
	// reservation, so a later join event can retry the whole welcome.
	if !s.BeginWelcome(500, 8) {
		t.Fatal("BeginWelcome should win for user 8")
	}
	if err := s.CompleteWelcome(500, 8); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("CompleteWelcome error = %v, want ErrPersistFailed wrap", err)
	}
	if s.WasWelcomed(500, 8) {
		t.Error("welcomed flag must roll back when persistence fails")
	}
	// The reservation is blocking again: BeginWelcome no longer wins.
	if s.BeginWelcome(500, 8) {
		t.Error("BeginWelcome after rolled-back CompleteWelcome must conflict with the re-opened reservation")
	}
}

// TestAddPublishTargetPersistFailure asserts a failed registry write leaves
// the chat unregistered on both sides.
func TestAddPublishTargetPersistFailure(t *testing.T) {
	s, fs, dbPath := newFailureTestStore(t)
	blockDatabase(t, dbPath)
	defer unblockDatabase(t, dbPath)

	if err := s.AddPublishTarget(600, 100); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("AddPublishTarget error = %v, want ErrPersistFailed wrap", err)
	}
	if s.IsPublishTarget(600) {
		t.Error("target must not be registered in memory when the write failed")
	}
	if n := len(fs.Keys("publish_targets")); n != 0 {
		t.Errorf("disk publish_targets table has %d records, want 0", n)
	}
}

// TestRemovePublishTargetPersistFailure asserts a failed delete leaves the
// chat registered on both sides: memory must not lead the disk.
func TestRemovePublishTargetPersistFailure(t *testing.T) {
	s, fs, dbPath := newFailureTestStore(t)
	if err := s.AddPublishTarget(600, 100); err != nil {
		t.Fatalf("AddPublishTarget: %v", err)
	}
	blockDatabase(t, dbPath)
	defer unblockDatabase(t, dbPath)

	if err := s.RemovePublishTarget(600); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("RemovePublishTarget error = %v, want ErrPersistFailed wrap", err)
	}
	if !s.IsPublishTarget(600) {
		t.Error("target must stay registered in memory when the delete failed")
	}
	var rec PublishTarget
	ok, err := fs.Get("publish_targets", targetKey(600), &rec)
	if err != nil || !ok {
		t.Fatalf("Get publish_targets = (%v, %v), want the record still on disk", ok, err)
	}
}

// TestAddPublishPersistFailure asserts a failed record write leaves no trace:
// no in-memory record, no on-disk record, and the ID is not consumed.
func TestAddPublishPersistFailure(t *testing.T) {
	s, fs, dbPath := newFailureTestStore(t)
	blockDatabase(t, dbPath)
	defer unblockDatabase(t, dbPath)

	rec, err := s.AddPublish(500, 100, "text", []int64{600}, []PublishResult{{ChatID: 600, Success: true}})
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("AddPublish error = %v, want ErrPersistFailed wrap", err)
	}
	if rec.ID != 1 {
		t.Errorf("returned record ID = %d, want the would-be ID 1 for the not-recorded notice", rec.ID)
	}
	if got := s.PublishRecords(0, 0); len(got) != 0 {
		t.Errorf("PublishRecords after failure = %+v, want none", got)
	}
	if n := len(fs.Keys("publishes")); n != 0 {
		t.Errorf("disk publishes table has %d records, want 0", n)
	}

	// The failed attempt did not burn the ID: the next publish is ID 1.
	unblockDatabase(t, dbPath)
	next, err := s.AddPublish(500, 100, "text", []int64{600}, []PublishResult{{ChatID: 600, Success: true}})
	if err != nil {
		t.Fatalf("AddPublish after unblocking: %v", err)
	}
	if next.ID != 1 {
		t.Errorf("post-failure publish ID = %d, want 1 (ID not consumed by the failure)", next.ID)
	}
}

// TestCreateCardPersistFailure asserts no card exists anywhere after a failed
// create.
func TestCreateCardPersistFailure(t *testing.T) {
	s, fs, dbPath := newFailureTestStore(t)
	blockDatabase(t, dbPath)
	defer unblockDatabase(t, dbPath)

	if err := s.CreateCard(500, 7, CardFields{Name: "Alice"}); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("CreateCard error = %v, want ErrPersistFailed wrap", err)
	}
	if _, ok := s.GetCard(500, 7); ok {
		t.Error("card must not exist in memory when the write failed")
	}
	if n := len(fs.Keys("cards")); n != 0 {
		t.Errorf("disk cards table has %d records, want 0", n)
	}
}

// TestUpdateCardPersistFailure asserts a failed update keeps the previous
// card values in memory and on disk.
func TestUpdateCardPersistFailure(t *testing.T) {
	s, fs, dbPath := newFailureTestStore(t)
	if err := s.CreateCard(500, 7, CardFields{Name: "Alice"}); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	blockDatabase(t, dbPath)
	defer unblockDatabase(t, dbPath)

	if err := s.UpdateCard(500, 7, CardFields{Name: "Alice", Bio: "changed"}); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("UpdateCard error = %v, want ErrPersistFailed wrap", err)
	}
	card, ok := s.GetCard(500, 7)
	if !ok || card.Fields.Bio != "" {
		t.Errorf("card after failed update = (%+v, %v), want the previous values", card, ok)
	}
	var rec Card
	okey, gerr := fs.Get("cards", cardKey(500, 7), &rec)
	if gerr != nil || !okey {
		t.Fatalf("Get cards = (%v, %v)", okey, gerr)
	}
	if rec.Fields.Bio != "" {
		t.Errorf("persisted card bio = %q, want the previous empty value", rec.Fields.Bio)
	}
}

// TestSaveTemplatePersistFailure asserts a failed template write leaves the
// stored template untouched: a new name stays undefined and an overwrite
// keeps the previous revision and content.
func TestSaveTemplatePersistFailure(t *testing.T) {
	s, fs, dbPath := newFailureTestStore(t)
	content := ProfileContent{Config: ProfileConfig{Language: "en", Timezone: "UTC"}}

	// A new template that cannot be persisted must not exist at all.
	blockDatabase(t, dbPath)
	if _, err := s.SaveTemplate(500, "cfg", content); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("SaveTemplate error = %v, want ErrPersistFailed wrap", err)
	}
	if _, _, found := s.Template(500, "cfg"); found {
		t.Errorf("Template(%q) exists after failed save, want not found", "cfg")
	}
	unblockDatabase(t, dbPath)

	// A subsequent save succeeds and lands version 1.
	v, err := s.SaveTemplate(500, "cfg", content)
	if err != nil {
		t.Fatalf("SaveTemplate after unblocking: %v", err)
	}
	if v != 1 {
		t.Fatalf("first successful SaveTemplate version = %d, want 1", v)
	}

	// An overwrite whose write fails keeps the old revision and content.
	changed := content
	changed.Config.Welcome = WelcomeProfile{Enabled: true, Message: "hi"}
	blockDatabase(t, dbPath)
	if _, err := s.SaveTemplate(500, "cfg", changed); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("overwrite SaveTemplate error = %v, want ErrPersistFailed wrap", err)
	}
	stored, version, found := s.Template(500, "cfg")
	if !found || version != 1 || stored.Config.Welcome.Message != "" {
		t.Errorf("template after failed overwrite = (%+v, %d, %v), want old content with version 1", stored, version, found)
	}
	if n := len(fs.Keys("templates")); n != 1 {
		t.Errorf("disk templates table has %d records, want 1", n)
	}
}
