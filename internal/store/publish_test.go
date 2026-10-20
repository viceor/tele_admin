package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/viceor/tele-admin/internal/i18n"
)

// --- target registry ---------------------------------------------------------

func TestPublishTargetRegistryCRUD(t *testing.T) {
	s := New(20, 0)

	if s.IsPublishTarget(500) {
		t.Fatal("fresh store must have no registered targets")
	}
	if got := s.PublishTargets(); len(got) != 0 {
		t.Fatalf("PublishTargets = %v, want empty", got)
	}
	if err := s.RemovePublishTarget(500); !errors.Is(err, ErrTargetNotRegistered) {
		t.Fatalf("removing unregistered target err = %v, want ErrTargetNotRegistered", err)
	}

	if err := s.AddPublishTarget(500, 100); err != nil {
		t.Fatalf("AddPublishTarget: %v", err)
	}
	if err := s.AddPublishTarget(500, 200); !errors.Is(err, ErrTargetRegistered) {
		t.Fatalf("re-adding target err = %v, want ErrTargetRegistered", err)
	}
	// The failed duplicate must not overwrite the original attribution.
	targets := s.PublishTargets()
	if len(targets) != 1 || targets[0] != 500 {
		t.Fatalf("PublishTargets = %v, want [500]", targets)
	}

	// Negative chat IDs (supergroups/channels) are valid targets.
	if err := s.AddPublishTarget(-1001234, 100); err != nil {
		t.Fatalf("AddPublishTarget(-1001234): %v", err)
	}
	if got := s.PublishTargets(); len(got) != 2 || got[0] != -1001234 || got[1] != 500 {
		t.Fatalf("PublishTargets = %v, want [-1001234 500] sorted", got)
	}
	if !s.IsPublishTarget(-1001234) || !s.IsPublishTarget(500) {
		t.Fatal("registered targets must be reported")
	}
	if s.PublishTargetCount() != 2 {
		t.Fatalf("PublishTargetCount = %d, want 2", s.PublishTargetCount())
	}

	if err := s.RemovePublishTarget(500); err != nil {
		t.Fatalf("RemovePublishTarget: %v", err)
	}
	if s.IsPublishTarget(500) {
		t.Fatal("removed target must be unregistered")
	}
	if err := s.RemovePublishTarget(500); !errors.Is(err, ErrTargetNotRegistered) {
		t.Fatalf("double remove err = %v, want ErrTargetNotRegistered", err)
	}
}

func TestPublishTargetPersistAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	s1 := NewWithDefaults(20, 0, i18n.English, "UTC")
	if err := s1.AttachFileStore(fs, nil); err != nil {
		t.Fatalf("AttachFileStore: %v", err)
	}
	if err := s1.AddPublishTarget(500, 100); err != nil {
		t.Fatalf("AddPublishTarget: %v", err)
	}
	if err := s1.AddPublishTarget(-100777, 100); err != nil {
		t.Fatalf("AddPublishTarget: %v", err)
	}
	if err := s1.RemovePublishTarget(-100777); err != nil {
		t.Fatalf("RemovePublishTarget: %v", err)
	}
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
	if !s2.IsPublishTarget(500) {
		t.Fatal("registered target 500 must survive a restart")
	}
	if s2.IsPublishTarget(-100777) {
		t.Fatal("removed target -100777 must stay unregistered after restart")
	}
}

// --- publish records ---------------------------------------------------------

func TestAddPublishAssignsSequentialIDs(t *testing.T) {
	s := New(20, 0)
	first, err := s.AddPublish(500, 100, "text", []int64{600}, []PublishResult{{ChatID: 600, Success: true}})
	if err != nil {
		t.Fatalf("AddPublish: %v", err)
	}
	second, err2 := s.AddPublish(500, 100, "photo", []int64{600, 700}, []PublishResult{
		{ChatID: 600, Success: true},
		{ChatID: 700, Success: false, Reason: "send_failed"},
	})
	if err2 != nil {
		t.Fatalf("AddPublish photo: %v", err2)
	}
	if first.ID != 1 || second.ID != 2 {
		t.Fatalf("IDs = %d, %d; want 1, 2", first.ID, second.ID)
	}

	recs := s.PublishRecords(0, 0)
	if len(recs) != 2 || recs[0].ID != 2 || recs[1].ID != 1 {
		t.Fatalf("PublishRecords = %+v, want newest first", recs)
	}
	if recs[0].Kind != "photo" || recs[0].SenderID != 100 || recs[0].SourceChatID != 500 {
		t.Fatalf("record fields = %+v", recs[0])
	}
	if len(recs[0].Targets) != 2 || recs[0].Targets[1] != 700 {
		t.Fatalf("targets = %v", recs[0].Targets)
	}
	if recs[0].Results[1].Success || recs[0].Results[1].Reason != "send_failed" {
		t.Fatalf("results = %+v", recs[0].Results)
	}

	limited := s.PublishRecords(0, 1)
	if len(limited) != 1 || limited[0].ID != 2 {
		t.Fatalf("PublishRecords(limit 1) = %+v", limited)
	}
	bounded := s.PublishRecords(1, 0)
	if len(bounded) != 1 || bounded[0].ID != 1 {
		t.Fatalf("PublishRecords(maxID 1) = %+v", bounded)
	}
}

// The caller mutating its own slices after AddPublish must not change the
// stored record.
func TestAddPublishCopiesCallerSlices(t *testing.T) {
	s := New(20, 0)
	targets := []int64{600}
	results := []PublishResult{{ChatID: 600, Success: true}}
	rec, err := s.AddPublish(500, 100, "text", targets, results)
	if err != nil {
		t.Fatalf("AddPublish: %v", err)
	}
	targets[0] = 999
	results[0].Success = false
	stored := s.PublishRecords(0, 0)[0]
	if stored.Targets[0] != 600 || !stored.Results[0].Success {
		t.Fatalf("stored record = %+v, want untouched copy of %+v", stored, rec)
	}
}

func TestPublishRecordsPersistAcrossRestartAndContinueIDs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	s1 := NewWithDefaults(20, 0, i18n.English, "UTC")
	if err := s1.AttachFileStore(fs, nil); err != nil {
		t.Fatalf("AttachFileStore: %v", err)
	}
	if _, err := s1.AddPublish(500, 100, "text", []int64{600}, []PublishResult{{ChatID: 600, Success: true}}); err != nil {
		t.Fatalf("AddPublish: %v", err)
	}
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

	recs := s2.PublishRecords(0, 0)
	if len(recs) != 1 || recs[0].ID != 1 || recs[0].Kind != "text" || recs[0].Targets[0] != 600 || !recs[0].Results[0].Success {
		t.Fatalf("records after restart = %+v", recs)
	}
	// IDs must continue after the restart, not restart from 1.
	again, err2 := s2.AddPublish(500, 100, "video", []int64{600}, []PublishResult{{ChatID: 600, Success: true}})
	if err2 != nil {
		t.Fatalf("post-restart AddPublish: %v", err2)
	}
	if again.ID != 2 {
		t.Fatalf("post-restart publish ID = %d, want 2", again.ID)
	}
}

// --- concurrency -------------------------------------------------------------

func TestPublishStateConcurrentAccess(t *testing.T) {
	s := New(20, 0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			chat := int64(600 + i%2)
			_ = s.AddPublishTarget(chat, 100)
			s.IsPublishTarget(chat)
			_ = s.PublishTargets()
			_, _ = s.AddPublish(500, 100, "text", []int64{chat}, []PublishResult{{ChatID: chat, Success: true}})
			_ = s.PublishRecords(0, 0)
		}(i)
	}
	wg.Wait()
	if s.PublishTargetCount() != 2 {
		t.Fatalf("PublishTargetCount = %d, want 2", s.PublishTargetCount())
	}
	if got := len(s.PublishRecords(0, 0)); got != 8 {
		t.Fatalf("publish records = %d, want 8", got)
	}
}

// The persisted record timestamps are stored, not zeroed by round-tripping.
func TestPublishRecordKeepsTimestamp(t *testing.T) {
	s := New(20, 0)
	before := time.Now().Add(-time.Minute)
	rec, err := s.AddPublish(500, 100, "text", []int64{600}, nil)
	if err != nil {
		t.Fatalf("AddPublish: %v", err)
	}
	if rec.CreatedAt.Before(before) {
		t.Fatalf("CreatedAt %v predates the test window", rec.CreatedAt)
	}
}
