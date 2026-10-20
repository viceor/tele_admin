package store

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/viceor/tele-admin/internal/i18n"
)

func TestSettingsDefaults(t *testing.T) {
	s := New(20, 0)
	cs := s.Settings(100)
	if cs.Language != i18n.English {
		t.Errorf("default language = %v, want English", cs.Language)
	}
	if cs.Timezone != "UTC" {
		t.Errorf("default timezone = %q, want UTC", cs.Timezone)
	}
}

// TestNewWithDefaultsAppliesConfiguredDefaults guards the requirement that a
// brand-new chat picks up i18n.default_language and timezone.default.
func TestNewWithDefaultsAppliesConfiguredDefaults(t *testing.T) {
	s := NewWithDefaults(20, 0, i18n.Chinese, "Asia/Shanghai")
	cs := s.Settings(100)
	if cs.Language != i18n.Chinese {
		t.Errorf("default language = %v, want Chinese", cs.Language)
	}
	if cs.Timezone != "Asia/Shanghai" {
		t.Errorf("default timezone = %q, want Asia/Shanghai", cs.Timezone)
	}
	if got := s.Language(100); got != i18n.Chinese {
		t.Errorf("Language accessor = %v, want Chinese", got)
	}
	if got := s.Timezone(100); got != "Asia/Shanghai" {
		t.Errorf("Timezone accessor = %q, want Asia/Shanghai", got)
	}
}

// TestNewWithDefaultsFallsBackWhenEmpty keeps the store safe when callers pass
// zero values.
func TestNewWithDefaultsFallsBackWhenEmpty(t *testing.T) {
	s := NewWithDefaults(20, 0, "", "")
	cs := s.Settings(100)
	if cs.Language != i18n.English || cs.Timezone != "UTC" {
		t.Errorf("empty defaults = (%v, %q), want (English, UTC)", cs.Language, cs.Timezone)
	}
}

func TestSetLanguageAndTimezone(t *testing.T) {
	s := New(20, 0)
	s.SetLanguage(100, i18n.Chinese)
	s.SetTimezone(100, "Asia/Shanghai")
	cs := s.Settings(100)
	if cs.Language != i18n.Chinese {
		t.Errorf("language = %v, want Chinese", cs.Language)
	}
	if cs.Timezone != "Asia/Shanghai" {
		t.Errorf("timezone = %q", cs.Timezone)
	}
}

func TestLanguageAndTimezoneAccessors(t *testing.T) {
	s := New(20, 0)
	if got := s.Language(100); got != i18n.English {
		t.Errorf("default language = %v, want English", got)
	}
	if got := s.Timezone(100); got != "UTC" {
		t.Errorf("default timezone = %q, want UTC", got)
	}
	s.SetLanguage(100, i18n.Chinese)
	s.SetTimezone(100, "Asia/Shanghai")
	if got := s.Language(100); got != i18n.Chinese {
		t.Errorf("language = %v, want Chinese", got)
	}
	if got := s.Timezone(100); got != "Asia/Shanghai" {
		t.Errorf("timezone = %q", got)
	}
}

// TestSettingsReturnsIsolatedSnapshot guards the MYC-36 fix: Settings must not
// hand out the internal mutable state. Mutating the snapshot (including its
// Welcomed map) must never change what the Store returns later.
func TestSettingsReturnsIsolatedSnapshot(t *testing.T) {
	s := New(20, 0)
	s.SetLanguage(100, i18n.Chinese)
	s.MarkWelcomed(100, 7)

	snap := s.Settings(100)
	snap.Language = i18n.English
	snap.Timezone = "Mars/Olympus"
	snap.Welcomed[99] = true
	delete(snap.Welcomed, 7)

	again := s.Settings(100)
	if again.Language != i18n.Chinese {
		t.Errorf("mutating a snapshot changed stored language: %v", again.Language)
	}
	if again.Timezone != "UTC" {
		t.Errorf("mutating a snapshot changed stored timezone: %q", again.Timezone)
	}
	if !again.Welcomed[7] {
		t.Error("mutating a snapshot removed a stored welcomed entry")
	}
	if again.Welcomed[99] {
		t.Error("mutating a snapshot added a welcomed entry to the store")
	}
}

// TestSettingsSnapshotsAreIndependent verifies two snapshots do not share the
// underlying Welcomed map.
func TestSettingsSnapshotsAreIndependent(t *testing.T) {
	s := New(20, 0)
	s.MarkWelcomed(100, 7)

	first := s.Settings(100)
	second := s.Settings(100)
	first.Welcomed[8] = true
	if second.Welcomed[8] {
		t.Error("snapshots share their Welcomed map")
	}
}

func TestRecordMessageAllowsUnderLimit(t *testing.T) {
	s := New(3, 0)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if ok, _ := s.RecordMessage(1, now.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatalf("message %d should be allowed", i)
		}
	}
}

func TestRecordMessageBlocksOverLimit(t *testing.T) {
	s := New(2, 0)
	now := time.Now()
	if ok, _ := s.RecordMessage(1, now); !ok {
		t.Fatal("first message should be allowed")
	}
	if ok, _ := s.RecordMessage(1, now.Add(time.Second)); !ok {
		t.Fatal("second message should be allowed")
	}
	if ok, reason := s.RecordMessage(1, now.Add(2*time.Second)); ok {
		t.Errorf("third message should be blocked (reason %q)", reason)
	}
}

func TestRecordMessageBlocksTooFast(t *testing.T) {
	s := New(20, 1000)
	now := time.Now()
	if ok, _ := s.RecordMessage(1, now); !ok {
		t.Fatal("first message should be allowed")
	}
	if ok, reason := s.RecordMessage(1, now.Add(500*time.Millisecond)); ok {
		t.Errorf("rapid message should be blocked (reason %q)", reason)
	}
}

func TestRecordMessageWindowSlides(t *testing.T) {
	s := New(1, 0)
	now := time.Now()
	if ok, _ := s.RecordMessage(1, now); !ok {
		t.Fatal("first should be allowed")
	}
	if ok, _ := s.RecordMessage(1, now.Add(time.Second)); ok {
		t.Fatal("second within window should be blocked")
	}
	if ok, _ := s.RecordMessage(1, now.Add(2*time.Minute)); !ok {
		t.Error("message after window should be allowed")
	}
}

func TestWelcomed(t *testing.T) {
	s := New(20, 0)
	if s.WasWelcomed(100, 7) {
		t.Fatal("should not be welcomed yet")
	}
	s.MarkWelcomed(100, 7)
	if !s.WasWelcomed(100, 7) {
		t.Fatal("should be welcomed after mark")
	}
}

// TestWasWelcomedUnknownChat guards against a nil-pointer panic when a chat has
// no settings yet (for example, the first member join in a new chat).
func TestWasWelcomedUnknownChat(t *testing.T) {
	s := New(20, 0)
	if s.WasWelcomed(12345, 99) {
		t.Fatal("unknown chat/user should not be welcomed")
	}
}

func TestRecordMessageMinIntervalThenRecovers(t *testing.T) {
	s := New(20, 1000)
	now := time.Now()
	if ok, _ := s.RecordMessage(1, now); !ok {
		t.Fatal("first message should be allowed")
	}
	if ok, _ := s.RecordMessage(1, now.Add(2*time.Second)); !ok {
		t.Fatal("message after the interval should be allowed")
	}
}

// TestPruneRatesRemovesInactiveWindows verifies that rate windows whose newest
// timestamp is past the retention period are dropped, while active windows are
// kept.
func TestPruneRatesRemovesInactiveWindows(t *testing.T) {
	s := New(20, 0)
	now := time.Now()
	if ok, _ := s.RecordMessage(1, now); !ok {
		t.Fatal("active user message should be allowed")
	}
	if ok, _ := s.RecordMessage(2, now.Add(-2*time.Minute)); !ok {
		t.Fatal("stale user message should be allowed")
	}
	if got := len(s.rates); got != 2 {
		t.Fatalf("tracked windows = %d, want 2", got)
	}

	if removed := s.pruneRatesLocked(now); removed != 1 {
		t.Errorf("pruned windows = %d, want 1", removed)
	}
	if _, ok := s.rates[2]; ok {
		t.Error("inactive window for user 2 should be pruned")
	}
	if _, ok := s.rates[1]; !ok {
		t.Error("active window for user 1 should be kept")
	}
}

// TestRecordMessagePrunesAtThreshold verifies the automatic sweep keeps the
// rate map bounded once it reaches the configured limit.
func TestRecordMessagePrunesAtThreshold(t *testing.T) {
	s := New(20, 0)
	s.maxRateWindow = 2
	now := time.Now()
	if ok, _ := s.RecordMessage(1, now.Add(-2*time.Minute)); !ok {
		t.Fatal("stale message 1 should be allowed")
	}
	if ok, _ := s.RecordMessage(2, now.Add(-2*time.Minute)); !ok {
		t.Fatal("stale message 2 should be allowed")
	}
	// Crossing the threshold triggers a sweep before the new window is added.
	if ok, _ := s.RecordMessage(3, now); !ok {
		t.Fatal("message 3 should be allowed")
	}
	if got := len(s.rates); got != 1 {
		t.Fatalf("tracked windows = %d, want 1 after pruning", got)
	}
	if _, ok := s.rates[3]; !ok {
		t.Error("active window for user 3 should be tracked")
	}
}

// TestPruningKeepsRateSemantics confirms pruning an inert window does not
// change a subsequent decision for the same user.
func TestPruningKeepsRateSemantics(t *testing.T) {
	s := New(1, 0)
	now := time.Now()
	if ok, _ := s.RecordMessage(1, now); !ok {
		t.Fatal("first message should be allowed")
	}
	if ok, _ := s.RecordMessage(1, now.Add(30*time.Second)); ok {
		t.Fatal("second message within the window should be blocked")
	}
	s.pruneRatesLocked(now.Add(2 * time.Minute))
	if ok, _ := s.RecordMessage(1, now.Add(2*time.Minute)); !ok {
		t.Error("message after the retention window should be allowed")
	}
}

// TestPruneRetentionCoversLongMinInterval guards the R2 regression: when the
// minimum interval exceeds one minute, the retention window must grow so a
// sweep does not drop the timestamp the min_interval rule still needs.
func TestPruneRetentionCoversLongMinInterval(t *testing.T) {
	s := New(20, 90000) // 90s minimum interval
	now := time.Now()
	if ok, _ := s.RecordMessage(1, now.Add(-70*time.Second)); !ok {
		t.Fatal("first message should be allowed")
	}
	if got := s.retention(); got < 2*90*time.Second {
		t.Fatalf("retention = %v, want at least 180s for a 90s interval", got)
	}
	s.pruneRatesLocked(now)
	if _, ok := s.rates[1]; !ok {
		t.Fatal("window with a 70s-old timestamp must survive for a 90s min interval")
	}
	if ok, reason := s.RecordMessage(1, now); ok {
		t.Errorf("message within the 90s interval must be blocked, got allowed (reason %q)", reason)
	}
}

// TestOtherUserSweepKeepsLongIntervalEnforcement guards the R2 regression from
// the sweep path: another user crossing the window threshold triggers a prune
// that must not clear an active long-interval window.
func TestOtherUserSweepKeepsLongIntervalEnforcement(t *testing.T) {
	s := New(20, 90000)
	s.maxRateWindow = 1
	now := time.Now()
	if ok, _ := s.RecordMessage(1, now.Add(-70*time.Second)); !ok {
		t.Fatal("user 1 first message should be allowed")
	}
	// User 2 reaches the threshold, forcing a sweep before its window is added.
	if ok, _ := s.RecordMessage(2, now); !ok {
		t.Fatal("user 2 message should be allowed")
	}
	if _, ok := s.rates[1]; !ok {
		t.Fatal("user 1 window must survive another user's sweep within the retention window")
	}
	if ok, _ := s.RecordMessage(1, now); ok {
		t.Error("user 1 must still be limited after another user triggered the sweep")
	}
}

// TestPruneRetentionStaysAtLeastOneMinute confirms the default retention never
// drops below one minute even with a tiny interval.
func TestPruneRetentionStaysAtLeastOneMinute(t *testing.T) {
	s := New(20, 1000)
	if got := s.retention(); got != time.Minute {
		t.Errorf("retention = %v, want 1m", got)
	}
}

// TestBeginWelcomeExclusive confirms concurrent duplicate join events produce a
// single send reservation.
func TestBeginWelcomeExclusive(t *testing.T) {
	s := New(20, 0)
	var winners int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.BeginWelcome(100, 7) {
				atomic.AddInt32(&winners, 1)
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("BeginWelcome winners = %d, want exactly 1", winners)
	}
}

// TestWelcomeCompleteThenSuppressed verifies a delivered welcome is remembered
// and later events are skipped.
func TestWelcomeCompleteThenSuppressed(t *testing.T) {
	s := New(20, 0)
	if !s.BeginWelcome(100, 7) {
		t.Fatal("first BeginWelcome should win")
	}
	s.CompleteWelcome(100, 7)
	if !s.WasWelcomed(100, 7) {
		t.Fatal("CompleteWelcome must mark the user welcomed")
	}
	if s.BeginWelcome(100, 7) {
		t.Error("BeginWelcome must not win after a completed welcome")
	}
}

// TestWelcomeAbortAllowsRetry verifies a failed send does not permanently
// suppress the welcome: the reservation is released and a later event wins.
func TestWelcomeAbortAllowsRetry(t *testing.T) {
	s := New(20, 0)
	if !s.BeginWelcome(100, 7) {
		t.Fatal("first BeginWelcome should win")
	}
	s.AbortWelcome(100, 7)
	if s.WasWelcomed(100, 7) {
		t.Error("AbortWelcome must not mark the user welcomed")
	}
	if !s.BeginWelcome(100, 7) {
		t.Error("BeginWelcome must win again after an aborted send")
	}
}

func TestStoreConcurrentAccess(t *testing.T) {
	s := New(50, 0)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(id int64) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				snap := s.Settings(id)
				_ = snap.Language
				_ = snap.Timezone
				_ = snap.Welcomed[id]
				s.SetLanguage(id, i18n.Chinese)
				s.SetTimezone(id, "UTC")
				s.RecordMessage(id, time.Now())
				s.MarkWelcomed(id, id)
				s.WasWelcomed(id, id)
			}
		}(int64(i))
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// TestSettingsSnapshotUnderConcurrentWrites exercises snapshot reads racing
// with locked writes, the exact pattern MYC-36 flagged.
func TestSettingsSnapshotUnderConcurrentWrites(t *testing.T) {
	s := New(50, 0)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func(id int64) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.SetLanguage(id, i18n.Chinese)
				s.SetTimezone(id, "Asia/Shanghai")
				s.MarkWelcomed(id, id+1)
			}
		}(int64(i))
		go func(id int64) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				snap := s.Settings(id)
				_ = snap.Language
				_ = snap.Timezone
				_ = snap.Welcomed[id+1]
			}
		}(int64(i))
	}
	wg.Wait()
}
