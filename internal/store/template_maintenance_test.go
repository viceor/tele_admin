package store

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacyOwnerlessRecord is the pre-ownership template shape (no owner field).
func legacyOwnerlessRecord() map[string]any {
	now := time.Now().UTC()
	return map[string]any{
		"version":    1,
		"content":    validProfile(),
		"created_at": now,
		"updated_at": now,
	}
}

// writeDB seeds a database file via the FileStore and closes it.
func writeDB(t *testing.T, dbPath string, seed func(fs *FileStore)) {
	t.Helper()
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	seed(fs)
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func attachOnce(t *testing.T, dbPath string) (*Store, *FileStore, error) {
	t.Helper()
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	s := New(20, 0)
	if err := s.AttachFileStore(fs, nil); err != nil {
		_ = fs.Close()
		return nil, nil, err
	}
	return s, fs, nil
}

// A legacy bare-name record with an owner is upgraded to the namespaced key on
// attach. The bare key is removed, the content and revision survive, and a
// second attach is a no-op.
func TestLegacyBareNameTemplateMigratesToNamespace(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	now := time.Now().UTC()
	writeDB(t, dbPath, func(fs *FileStore) {
		rec := persistedTemplateRecord{Owner: 500, Version: 3, Content: validProfile(), CreatedAt: now, UpdatedAt: now}
		if err := fs.Put("templates", "alpha", rec); err != nil {
			t.Fatalf("seed legacy: %v", err)
		}
	})

	s, fs, err := attachOnce(t, dbPath)
	if err != nil {
		t.Fatalf("attach with legacy record: %v", err)
	}
	content, version, ok := s.Template(500, "alpha")
	if !ok || version != 3 || content.Config.Timezone != "UTC" {
		t.Fatalf("migrated Template(500, alpha) = (%+v, %d, %v), want version 3 with content", content, version, ok)
	}
	if keys := fs.Keys("templates"); len(keys) != 1 || keys[0] != "500/alpha" {
		t.Fatalf("templates keys after migration = %v, want [500/alpha]", keys)
	}
	var rec persistedTemplateRecord
	if ok, err := fs.Get("templates", "500/alpha", &rec); err != nil || !ok || rec.Name != "alpha" || rec.Owner != 500 {
		t.Fatalf("migrated record = (%+v, %v, %v), want name alpha owner 500", rec, ok, err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, fs2, err := attachOnce(t, dbPath)
	if err != nil {
		t.Fatalf("second attach: %v", err)
	}
	defer fs2.Close()
	if _, v, ok := s2.Template(500, "alpha"); !ok || v != 3 {
		t.Fatalf("second attach Template = (%d, %v), want (3, true)", v, ok)
	}
}

// A legacy record whose namespaced twin already exists refuses the attach and
// leaves the database byte-for-byte in its previous keys: no record is picked
// over the other.
func TestLegacyMigrationCollisionRefusesAttach(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	now := time.Now().UTC()
	writeDB(t, dbPath, func(fs *FileStore) {
		if err := fs.Put("templates", "alpha", persistedTemplateRecord{Owner: 500, Version: 1, Content: validProfile(), CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed legacy: %v", err)
		}
		if err := fs.Put("templates", "500/alpha", persistedTemplateRecord{Name: "alpha", Owner: 500, Version: 2, Content: validProfile(), CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed namespaced: %v", err)
		}
	})

	if _, fs, err := attachOnce(t, dbPath); err == nil {
		fs.Close()
		t.Fatal("attach accepted a legacy/namespaced collision; want refusal")
	}
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer fs.Close()
	if keys := fs.Keys("templates"); len(keys) != 2 {
		t.Fatalf("keys after refused attach = %v, want both records kept", keys)
	}
}

// A legacy record whose bare name is invalid is refused, not renamed.
func TestLegacyMigrationRefusesInvalidName(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	writeDB(t, dbPath, func(fs *FileStore) {
		if err := fs.Put("templates", "bad name!", persistedTemplateRecord{Owner: 500, Version: 1, Content: validProfile()}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	})
	if _, fs, err := attachOnce(t, dbPath); err == nil {
		fs.Close()
		t.Fatal("attach accepted an invalid legacy template name; want refusal")
	}
}

// A namespaced record must agree with its own owner and name. A mismatch means
// the file was edited or corrupted, so the database fails to load.
func TestNamespacedRecordMismatchFailsLoad(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	writeDB(t, dbPath, func(fs *FileStore) {
		if err := fs.Put("templates", "500/alpha", persistedTemplateRecord{Name: "alpha", Owner: 600, Version: 1, Content: validProfile()}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	})
	if fs, err := OpenFileStore(dbPath); err == nil {
		fs.Close()
		t.Fatal("OpenFileStore accepted a template whose key contradicts its owner; want refusal")
	}
}

// Diagnose classifies records without changing the database.
func TestDiagnoseTemplatesClassifiesRecordsReadOnly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	now := time.Now().UTC()
	writeDB(t, dbPath, func(fs *FileStore) {
		_ = fs.Put("templates", "500/alpha", persistedTemplateRecord{Name: "alpha", Owner: 500, Version: 1, Content: validProfile()})
		_ = fs.Put("templates", "beta", persistedTemplateRecord{Owner: 500, Version: 1, Content: validProfile(), CreatedAt: now, UpdatedAt: now})
		_ = fs.Put("templates", "legacy", legacyOwnerlessRecord())
	})

	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	rep := DiagnoseTemplates(fs)
	if !equalStrings(rep.Namespaced, []string{"500/alpha"}) || !equalStrings(rep.Legacy, []string{"beta"}) || !equalStrings(rep.Ownerless, []string{"legacy"}) {
		t.Fatalf("report = %+v, want namespaced [500/alpha], legacy [beta], ownerless [legacy]", rep)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var out bytes.Buffer
	if err := RunTemplateMaintenance(dbPath, false, &out); err != nil {
		t.Fatalf("RunTemplateMaintenance(diagnose): %v", err)
	}
	if !strings.Contains(out.String(), "ownerless template record(s) block startup") {
		t.Errorf("diagnose output lacks the startup warning:\n%s", out.String())
	}
	fs2, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer fs2.Close()
	if keys := fs2.Keys("templates"); len(keys) != 3 {
		t.Fatalf("diagnose changed the database: keys = %v", keys)
	}
}

// Quarantine moves ownerless records aside with their content intact, after
// which startup succeeds. The records stay recoverable and are not deleted.
func TestQuarantineOwnerlessMovesRecordsAndStartupSucceeds(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	now := time.Now().UTC()
	writeDB(t, dbPath, func(fs *FileStore) {
		_ = fs.Put("templates", "500/alpha", persistedTemplateRecord{Name: "alpha", Owner: 500, Version: 1, Content: validProfile()})
		_ = fs.Put("templates", "beta", persistedTemplateRecord{Owner: 500, Version: 4, Content: validProfile(), CreatedAt: now, UpdatedAt: now})
		_ = fs.Put("templates", "legacy", legacyOwnerlessRecord())
	})

	if _, fs, err := attachOnce(t, dbPath); err == nil {
		fs.Close()
		t.Fatal("attach succeeded with an ownerless record before quarantine; want refusal")
	}

	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	moved, err := QuarantineOwnerlessTemplates(fs)
	if err != nil {
		t.Fatalf("QuarantineOwnerlessTemplates: %v", err)
	}
	if !equalStrings(moved, []string{"legacy"}) {
		t.Fatalf("moved = %v, want [legacy]", moved)
	}
	var kept map[string]any
	if ok, err := fs.Get(templateQuarantineTable, "legacy", &kept); err != nil || !ok {
		t.Fatalf("quarantined record missing: found=%v err=%v", ok, err)
	}
	if v, _ := kept["version"].(float64); v != 1 {
		t.Errorf("quarantined content version = %v, want 1 (content must be kept)", kept["version"])
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, fs2, err := attachOnce(t, dbPath)
	if err != nil {
		t.Fatalf("attach after quarantine: %v", err)
	}
	defer fs2.Close()
	if _, v, ok := s.Template(500, "beta"); !ok || v != 4 {
		t.Errorf("legacy owned template after upgrade = (%d, %v), want (4, true)", v, ok)
	}
	if _, _, ok := s.Template(500, "legacy"); ok {
		t.Error("quarantined template is served by the store")
	}
	if keys := fs2.Keys("templates"); !equalStrings(keys, []string{"500/alpha", "500/beta"}) {
		t.Errorf("templates keys after startup = %v, want [500/alpha 500/beta]", keys)
	}

	// A second quarantine run finds nothing to move.
	again, err := QuarantineOwnerlessTemplates(fs2)
	if err != nil || len(again) != 0 {
		t.Errorf("second quarantine = (%v, %v), want no-op", again, err)
	}
}

// Quarantine never overwrites an earlier quarantined record of the same key,
// and it moves nothing when it refuses.
func TestQuarantineRefusesToOverwriteEarlierRecord(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	writeDB(t, dbPath, func(fs *FileStore) {
		_ = fs.Put(templateQuarantineTable, "legacy", map[string]any{"earlier": true})
		_ = fs.Put("templates", "legacy", legacyOwnerlessRecord())
	})
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	defer fs.Close()
	if moved, err := QuarantineOwnerlessTemplates(fs); err == nil || len(moved) != 0 {
		t.Fatalf("quarantine over an earlier record = (%v, %v), want refusal and nothing moved", moved, err)
	}
	if keys := fs.Keys("templates"); !equalStrings(keys, []string{"legacy"}) {
		t.Errorf("ownerless record moved despite refusal: templates = %v", keys)
	}
	var earlier map[string]any
	if ok, _ := fs.Get(templateQuarantineTable, "legacy", &earlier); !ok || earlier["earlier"] != true {
		t.Errorf("earlier quarantined record was overwritten: %v", earlier)
	}
}

// The namespaced record shape, including the stored name, round-trips through JSON.
func TestNamespacedRecordJSONRoundTrip(t *testing.T) {
	rec := persistedTemplateRecord{Name: "alpha", Owner: 500, Version: 1, Content: validProfile()}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back persistedTemplateRecord
	if err := json.Unmarshal(raw, &back); err != nil || back.Name != "alpha" || back.Owner != 500 {
		t.Fatalf("round trip = (%+v, %v)", back, err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Diagnose reports the Malformed category for a record that cannot decode
// into a template record, and lists the quarantine table without classifying
// those keys as loadable. Nothing changes and nothing is moved.
func TestDiagnoseClassifiesMalformedAndQuarantined(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	writeDB(t, dbPath, func(fs *FileStore) {
		_ = fs.Put("templates", "500/alpha", persistedTemplateRecord{Name: "alpha", Owner: 500, Version: 1, Content: validProfile()})
		// A record whose owner field is un-decodable stays a valid JSON
		// object, so the database loads, but the store cannot read it
		// (fail-closed at attach; here it is only classified).
		_ = fs.Put("templates", "broken", map[string]any{"owner": "not-a-number", "content": validProfile()})
		_ = fs.Put(templateQuarantineTable, "older", map[string]any{"kept": true})
	})

	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	defer fs.Close()

	rep := DiagnoseTemplates(fs)
	if !equalStrings(rep.Namespaced, []string{"500/alpha"}) {
		t.Fatalf("namespaced = %v, want [500/alpha]", rep.Namespaced)
	}
	if !equalStrings(rep.Malformed, []string{"broken"}) {
		t.Fatalf("malformed = %v, want [broken]", rep.Malformed)
	}
	if !equalStrings(rep.Quarantined, []string{"older"}) {
		t.Fatalf("quarantined = %v, want [older]", rep.Quarantined)
	}
	if len(rep.Ownerless) != 0 || len(rep.Legacy) != 0 {
		t.Errorf("ownerless/legacy misclassified: %+v", rep)
	}

	// Quarantine moves nothing: only ownerless records are candidates.
	if moved, err := QuarantineOwnerlessTemplates(fs); err != nil || len(moved) != 0 {
		t.Fatalf("quarantine = (%v, %v), want a no-op over malformed records", moved, err)
	}
	if keys := fs.Keys("templates"); len(keys) != 2 {
		t.Errorf("quarantine changed the templates table: %v", keys)
	}
}

// Quarantined records keep their exact stored JSON. QuarantineOwnerlessTemplates
// must preserve every field byte-for-byte, not merely keep a decodable copy
// (MYC-91 P3).
func TestQuarantinePreservesRawRecordJSON(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	now := time.Now().UTC()
	writeDB(t, dbPath, func(fs *FileStore) {
		rec := persistedTemplateRecord{Owner: 0, Version: 7, Content: validProfile(), CreatedAt: now, UpdatedAt: now}
		if err := fs.Put("templates", "helpless", rec); err != nil {
			t.Fatalf("seed ownerless: %v", err)
		}
	})

	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	defer fs.Close()

	var rawBefore json.RawMessage
	if ok, err := fs.Get("templates", "helpless", &rawBefore); err != nil || !ok {
		t.Fatalf("read before quarantine: found=%v err=%v", ok, err)
	}
	if moved, err := QuarantineOwnerlessTemplates(fs); err != nil || !equalStrings(moved, []string{"helpless"}) {
		t.Fatalf("quarantine = (%v, %v), want [helpless]", moved, err)
	}
	var rawAfter json.RawMessage
	if ok, err := fs.Get(templateQuarantineTable, "helpless", &rawAfter); err != nil || !ok {
		t.Fatalf("read quarantined record: found=%v err=%v", ok, err)
	}
	if string(rawAfter) != string(rawBefore) {
		t.Errorf("quarantined JSON differs from the original:\n before: %s\n after:  %s", rawBefore, rawAfter)
	}
	if gone, _ := fs.Get("templates", "helpless", new(json.RawMessage)); gone {
		t.Error("ownerless record stayed in the templates table after quarantine")
	}
}

// RunTemplateMaintenance must fail while the service holds the single-writer
// lock: both CLI commands open the database themselves and never work around
// the lock (README "服务运行时执行会失败").
func TestRunTemplateMaintenanceFailsWhileServiceHoldsLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	writeDB(t, dbPath, func(fs *FileStore) {})

	service, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore (simulated service): %v", err)
	}
	defer service.Close()

	var out bytes.Buffer
	if err := RunTemplateMaintenance(dbPath, false, &out); err == nil {
		t.Fatal("diagnose succeeded while the service held the database lock; want failure")
	}
	if err := RunTemplateMaintenance(dbPath, true, &out); err == nil {
		t.Fatal("quarantine succeeded while the service held the database lock; want failure")
	}
}
