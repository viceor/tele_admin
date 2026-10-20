package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// cardRecord is a stand-in for the typed entity records later slices will
// persist (targets, cards, rules, templates, publish records).
type cardRecord struct {
	ChatID int64  `json:"chat_id"`
	Title  string `json:"title"`
}

func openTestStore(t *testing.T) *FileStore {
	t.Helper()
	fs, err := OpenFileStore(filepath.Join(t.TempDir(), "teleadmin.db"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs
}

func TestFileStoreCRUD(t *testing.T) {
	fs := openTestStore(t)

	var got cardRecord
	ok, err := fs.Get("cards", "1", &got)
	if err != nil {
		t.Fatalf("Get on empty store: %v", err)
	}
	if ok {
		t.Fatal("Get on empty store should report not-found")
	}
	if ok, err = fs.Get("missing-table", "1", &got); ok || err != nil {
		t.Errorf("Get on missing table = (%v, %v), want (false, nil)", ok, err)
	}

	if err := fs.Put("cards", "1", cardRecord{ChatID: 5, Title: "hello"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := fs.Put("cards", "2", cardRecord{ChatID: 6, Title: "world"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ok, err = fs.Get("cards", "1", &got)
	if err != nil || !ok {
		t.Fatalf("Get after Put = (%v, %v), want (true, nil)", ok, err)
	}
	if got.Title != "hello" || got.ChatID != 5 {
		t.Errorf("Get = %+v, want {5 hello}", got)
	}

	keys := fs.Keys("cards")
	if len(keys) != 2 || keys[0] != "1" || keys[1] != "2" {
		t.Errorf("Keys = %v, want [1 2]", keys)
	}

	if err := fs.Put("cards", "1", cardRecord{ChatID: 5, Title: "updated"}); err != nil {
		t.Fatalf("overwrite Put: %v", err)
	}
	ok, _ = fs.Get("cards", "1", &got)
	if got.Title != "updated" {
		t.Errorf("Get after overwrite = %+v, want title updated", got)
	}

	if err := fs.Delete("cards", "1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ = fs.Get("cards", "1", &got); ok {
		t.Error("Get after Delete should report not-found")
	}
	if keys = fs.Keys("cards"); len(keys) != 1 || keys[0] != "2" {
		t.Errorf("Keys after Delete = %v, want [2]", keys)
	}

	if err := fs.Delete("cards", "absent"); err != nil {
		t.Errorf("Delete of missing record: %v", err)
	}
	if err := fs.Delete("cards", "2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if keys = fs.Keys("cards"); len(keys) != 0 {
		t.Errorf("Keys after deleting all = %v, want empty", keys)
	}
}

func TestFileStoreRestartPreservesData(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")

	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("first OpenFileStore: %v", err)
	}
	if err := fs.Put("cards", "1", cardRecord{ChatID: 5, Title: "persist-me"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := fs.Put("chats", "500", persistedChatRecord{ChatID: 500, Language: "zh", Timezone: "Asia/Shanghai", Welcomed: []int64{7}}); err != nil {
		t.Fatalf("Put chat: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen OpenFileStore: %v", err)
	}
	defer reopened.Close()

	var card cardRecord
	ok, err := reopened.Get("cards", "1", &card)
	if err != nil || !ok {
		t.Fatalf("Get card after restart = (%v, %v), want (true, nil)", ok, err)
	}
	if card.Title != "persist-me" || card.ChatID != 5 {
		t.Errorf("card after restart = %+v, want {5 persist-me}", card)
	}

	var chat persistedChatRecord
	ok, err = reopened.Get("chats", "500", &chat)
	if err != nil || !ok {
		t.Fatalf("Get chat after restart = (%v, %v), want (true, nil)", ok, err)
	}
	if chat.Language != "zh" || chat.Timezone != "Asia/Shanghai" || len(chat.Welcomed) != 1 || chat.Welcomed[0] != 7 {
		t.Errorf("chat after restart = %+v, want {500 zh Asia/Shanghai [7]}", chat)
	}
}

func TestFileStoreCreatesParentDirectory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nested", "dir", "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore with missing parents: %v", err)
	}
	if err := fs.Put("cards", "1", cardRecord{Title: "x"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	_ = fs.Close()
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("database file not created: %v", err)
	}
}

func TestFileStoreTransactionCommitAndRollback(t *testing.T) {
	fs := openTestStore(t)
	if err := fs.Put("cards", "a", cardRecord{Title: "original"}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	tx, err := fs.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Put("cards", "a", cardRecord{Title: "changed"}); err != nil {
		t.Fatalf("tx Put: %v", err)
	}
	if err := tx.Put("cards", "b", cardRecord{Title: "new"}); err != nil {
		t.Fatalf("tx Put new: %v", err)
	}

	var got cardRecord
	ok, _ := tx.Get("cards", "a", &got)
	if !ok || got.Title != "changed" {
		t.Errorf("tx Get a = (%v, %+v), want changed", ok, got)
	}
	ok, _ = tx.Get("cards", "b", &got)
	if !ok || got.Title != "new" {
		t.Errorf("tx Get b = (%v, %+v), want new", ok, got)
	}

	ok, _ = fs.Get("cards", "a", &got)
	if !ok || got.Title != "original" {
		t.Errorf("store Get a before commit = (%v, %+v), want original", ok, got)
	}
	if ok, _ = fs.Get("cards", "b", &got); ok {
		t.Error("store Get b before commit should be not-found")
	}

	tx.Rollback()
	ok, _ = fs.Get("cards", "a", &got)
	if !ok || got.Title != "original" {
		t.Errorf("store Get a after rollback = (%v, %+v), want original", ok, got)
	}
	if ok, _ = fs.Get("cards", "b", &got); ok {
		t.Error("store Get b after rollback should be not-found")
	}

	tx2, err := fs.Begin()
	if err != nil {
		t.Fatalf("Begin 2: %v", err)
	}
	if err := tx2.Put("cards", "b", cardRecord{Title: "new"}); err != nil {
		t.Fatalf("tx2 Put: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	ok, _ = fs.Get("cards", "b", &got)
	if !ok || got.Title != "new" {
		t.Errorf("store Get b after commit = (%v, %+v), want new", ok, got)
	}

	tx3, err := fs.Begin()
	if err != nil {
		t.Fatalf("Begin 3: %v", err)
	}
	if err := tx3.Delete("cards", "b"); err != nil {
		t.Fatalf("tx3 Delete: %v", err)
	}
	if ok, _ = tx3.Get("cards", "b", &got); ok {
		t.Error("tx Get b after staged Delete should be not-found")
	}
	if err := tx3.Commit(); err != nil {
		t.Fatalf("Commit 3: %v", err)
	}
	if ok, _ = fs.Get("cards", "b", &got); ok {
		t.Error("store Get b after committed Delete should be not-found")
	}
}

func TestFileStoreFailedCommitLeavesNoPartialWrites(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	if err := fs.Put("cards", "keep", cardRecord{Title: "keep-me"}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	// The commit path renames its temporary file over the database path. A
	// directory at the database path makes that rename fail for real (this
	// also works when tests run as root, where permission bits do not block
	// writes).
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("remove database file: %v", err)
	}
	if err := os.Mkdir(dbPath, 0o700); err != nil {
		t.Fatalf("create blocking directory: %v", err)
	}

	tx, err := fs.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Put("cards", "a", cardRecord{Title: "staged-a"}); err != nil {
		t.Fatalf("tx Put a: %v", err)
	}
	if err := tx.Put("cards", "b", cardRecord{Title: "staged-b"}); err != nil {
		t.Fatalf("tx Put b: %v", err)
	}

	if err := tx.Commit(); err == nil {
		t.Fatal("Commit should fail when the database file cannot be replaced")
	}

	// The failure path cleaned up its temporary file: no commit leftovers in
	// the database directory.
	entries, err := os.ReadDir(filepath.Dir(dbPath))
	if err != nil {
		t.Fatalf("read database directory: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("failed commit left a temporary file behind: %s", e.Name())
		}
	}

	var got cardRecord
	if ok, _ := fs.Get("cards", "a", &got); ok {
		t.Error("staged write a leaked into the store after a failed commit")
	}
	if ok, _ := fs.Get("cards", "b", &got); ok {
		t.Error("staged write b leaked into the store after a failed commit")
	}
	ok, _ := fs.Get("cards", "keep", &got)
	if !ok || got.Title != "keep-me" {
		t.Errorf("pre-existing record keep = (%v, %+v), want keep-me", ok, got)
	}

	// The store stays usable: after removing the blocker, a retry commits,
	// and the database file holds the full snapshot, not a partial write.
	if err := os.RemoveAll(dbPath); err != nil {
		t.Fatalf("remove blocker: %v", err)
	}
	tx2, err := fs.Begin()
	if err != nil {
		t.Fatalf("Begin after failure: %v", err)
	}
	if err := tx2.Put("cards", "a", cardRecord{Title: "staged-a"}); err != nil {
		t.Fatalf("retry tx Put: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("retry Commit: %v", err)
	}
	if ok, _ = fs.Get("cards", "a", &got); !ok || got.Title != "staged-a" {
		t.Errorf("Get a after retry = (%v, %+v), want staged-a", ok, got)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen after retry: %v", err)
	}
	defer reopened.Close()
	if ok, err := reopened.Get("cards", "keep", &got); err != nil || !ok || got.Title != "keep-me" {
		t.Errorf("Get keep after reopen = (%v, %+v, %v), want keep-me", ok, got, err)
	}
	if ok, err := reopened.Get("cards", "a", &got); err != nil || !ok || got.Title != "staged-a" {
		t.Errorf("Get a after reopen = (%v, %+v, %v), want staged-a", ok, got, err)
	}
}

func TestFileStoreConcurrentWrites(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}

	const workers = 8
	const writesPerWorker = 50
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < writesPerWorker; i++ {
				key := fmt.Sprintf("w%d-%d", id, i)
				if err := fs.Put("cards", key, cardRecord{ChatID: int64(id), Title: key}); err != nil {
					t.Errorf("Put %s: %v", key, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if got := len(fs.Keys("cards")); got != workers*writesPerWorker {
		t.Fatalf("keys after concurrent writes = %d, want %d", got, workers*writesPerWorker)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got := len(reopened.Keys("cards")); got != workers*writesPerWorker {
		t.Errorf("keys after reopen = %d, want %d", got, workers*writesPerWorker)
	}
}

func TestFileStoreSingleWriterLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")

	first, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("first OpenFileStore: %v", err)
	}

	if _, err := OpenFileStore(dbPath); err == nil {
		t.Fatal("second OpenFileStore on the same path should fail while the first is open")
	} else if !bytes.Contains([]byte(err.Error()), []byte("locked")) {
		t.Errorf("second OpenFileStore error = %v, want a lock message", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	second, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore after the lock was released: %v", err)
	}
	_ = second.Close()
}

func TestFileStoreRejectsCorruptFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	if err := os.WriteFile(dbPath, []byte("{not valid json"), 0o640); err != nil {
		t.Fatalf("write corrupt database: %v", err)
	}
	_, err := OpenFileStore(dbPath)
	if err == nil {
		t.Fatal("OpenFileStore should reject a corrupt database file")
	}
	raw, readErr := os.ReadFile(dbPath)
	if readErr != nil {
		t.Fatalf("read database: %v", readErr)
	}
	if string(raw) != "{not valid json" {
		t.Error("corrupt database file was modified")
	}
}

func TestFileStoreRejectsInvalidArguments(t *testing.T) {
	if _, err := OpenFileStore(""); err == nil {
		t.Error("OpenFileStore with empty path should fail")
	}
	fs := openTestStore(t)
	if err := fs.Put("", "k", cardRecord{Title: "x"}); err == nil {
		t.Error("Put with empty table should fail")
	}
	if err := fs.Put("cards", "", cardRecord{Title: "x"}); err == nil {
		t.Error("Put with empty key should fail")
	}
	if _, err := fs.Get("cards", "", new(cardRecord)); err == nil {
		t.Error("Get with empty key should fail")
	}
	if err := fs.Delete("cards", ""); err == nil {
		t.Error("Delete with empty key should fail")
	}
}

func TestFileStoreWriteFailsDuringTransaction(t *testing.T) {
	fs := openTestStore(t)
	tx, err := fs.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := fs.Put("cards", "x", cardRecord{Title: "y"}); err == nil {
		t.Error("store-level Put should fail while a transaction is active")
	}
	if _, err := fs.Begin(); err == nil {
		t.Error("a second Begin should fail while a transaction is active")
	}
	tx.Rollback()
	if err := fs.Put("cards", "x", cardRecord{Title: "y"}); err != nil {
		t.Errorf("Put after rollback: %v", err)
	}
}

// ---------------------------------------------------------------------------
// MYC-72 storage hardening regression tests (MYC-51 #1/#2/#4/#5/#6).
// ---------------------------------------------------------------------------

// onDiskEnvelope mirrors the versioned top-level on-disk format the store must
// write. It is declared locally so the test pins the on-disk contract instead
// of an internal type.
type onDiskEnvelope struct {
	SchemaVersion int                                   `json:"schema_version"`
	Data          map[string]map[string]json.RawMessage `json:"data"`
}

// TestFileStoreLockSurvivesCommitRename is the regression test for MYC-51 #1:
// the advisory lock used to ride on the database file's inode, which every
// commit replaces via rename. After the first instance's first write the lock
// guarded an orphaned inode and a second instance could open the same
// database. The lock must live on a dedicated file that is never replaced.
func TestFileStoreLockSurvivesCommitRename(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")

	first, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("first OpenFileStore: %v", err)
	}
	// This commit renames a temporary file over the database path, replacing
	// the inode the old lock implementation held.
	if err := first.Put("cards", "1", cardRecord{Title: "commit-1"}); err != nil {
		t.Fatalf("first Put: %v", err)
	}

	second, err := OpenFileStore(dbPath)
	if err == nil {
		_ = second.Close()
		t.Fatal("second OpenFileStore after the first store committed must fail while the first is open")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("locked")) {
		t.Errorf("second OpenFileStore error = %v, want a lock message", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	third, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore after the lock was released: %v", err)
	}
	_ = third.Close()
}

// TestFileStoreTransactionDeleteCommitsAsRemoval is the regression test for
// MYC-51 #2: a committed transaction delete used to persist the record as a
// JSON null instead of removing it. A committed delete must read back as a
// clean not-found, leave no null in the file, and stay not-found after a
// restart.
func TestFileStoreTransactionDeleteCommitsAsRemoval(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	if err := fs.Put("cards", "1", cardRecord{Title: "delete-me"}); err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	if err := fs.Put("cards", "2", cardRecord{Title: "keep-me"}); err != nil {
		t.Fatalf("Put 2: %v", err)
	}

	tx, err := fs.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Delete("cards", "1"); err != nil {
		t.Fatalf("tx Delete: %v", err)
	}
	var got cardRecord
	// A staged delete must read back as a clean not-found inside the
	// transaction, not as a decode error on a nil record.
	if ok, err := tx.Get("cards", "1", &got); ok || err != nil {
		t.Errorf("tx Get after staged Delete = (%v, %v), want (false, nil)", ok, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if ok, err := fs.Get("cards", "1", &got); ok || err != nil {
		t.Errorf("Get after committed Delete = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, _ := fs.Get("cards", "2", &got); !ok {
		t.Error("sibling record must survive a committed delete")
	}

	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database: %v", err)
	}
	if bytes.Contains(raw, []byte("null")) {
		t.Errorf("committed delete left a JSON null in the database: %s", raw)
	}

	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if ok, err := reopened.Get("cards", "1", &got); ok || err != nil {
		t.Errorf("Get after restart = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := reopened.Get("cards", "2", &got); err != nil || !ok {
		t.Errorf("Get sibling after restart = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestFileStoreIgnoresHijackedLegacyTempName is the regression test for
// MYC-51 #4: the commit used a fixed temporary name (<db>.tmp), so anyone who
// could write the directory could pre-plant a symlink there and make the
// commit write through it. Temporary files must now use random names created
// O_EXCL in the same directory, and the planted symlink must be ignored.
func TestFileStoreIgnoresHijackedLegacyTempName(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	defer fs.Close()

	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("do not touch"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	if err := os.Symlink(victim, dbPath+".tmp"); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	if err := fs.Put("cards", "1", cardRecord{Title: "safe"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	raw, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(raw) != "do not touch" {
		t.Errorf("victim file content = %q, want %q (the commit wrote through the symlinked temporary path)", raw, "do not touch")
	}
	fi, err := os.Lstat(dbPath + ".tmp")
	if err != nil {
		t.Fatalf("lstat planted symlink: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("planted symlink was replaced or consumed by the commit path")
	}
}

// TestFileStoreTightensWideFilePermissions is the regression test for
// MYC-51 #5 (file side): an existing database or lock file with group/other
// access must be tightened to owner-only, and a commit must leave the
// database a private regular file.
func TestFileStoreTightensWideFilePermissions(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "teleadmin.db")
	if err := os.WriteFile(dbPath, nil, 0o666); err != nil {
		t.Fatalf("seed wide database file: %v", err)
	}
	if err := os.Chmod(dbPath, 0o666); err != nil {
		t.Fatalf("chmod wide database file: %v", err)
	}
	if err := os.WriteFile(dbPath+".lock", nil, 0o666); err != nil {
		t.Fatalf("seed wide lock file: %v", err)
	}
	if err := os.Chmod(dbPath+".lock", 0o666); err != nil {
		t.Fatalf("chmod wide lock file: %v", err)
	}
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	defer fs.Close()

	if fi, err := os.Stat(dbPath); err != nil {
		t.Fatalf("stat database: %v", err)
	} else if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("existing database file mode = %o, want tightened 600", got)
	}
	if fi, err := os.Stat(dbPath + ".lock"); err != nil {
		t.Fatalf("stat lock file: %v", err)
	} else if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("lock file mode = %o, want tightened 600", got)
	}

	if err := fs.Put("cards", "1", cardRecord{Title: "x"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat database after commit: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("database file mode after commit = %o, want 600", got)
	}
	if !fi.Mode().IsRegular() {
		t.Error("database file is not a regular file after commit")
	}
}

// TestFileStoreRejectsWritableDatabaseDirectory is the regression test for
// MYC-51 #5 (directory side): the store must refuse to place its database in
// a directory that group or other can write (they could swap the database or
// lock files). It must not create any file there.
func TestFileStoreRejectsWritableDatabaseDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wide")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("create wide directory: %v", err)
	}
	// MkdirAll applies the umask, so force the wide mode the rejection is
	// about (the test owns the directory it just created).
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod wide directory: %v", err)
	}
	dbPath := filepath.Join(dir, "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err == nil {
		_ = fs.Close()
		t.Fatal("OpenFileStore must reject a database directory that is writable by group or other")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("directory")) {
		t.Errorf("error = %v, want a directory permission message", err)
	}
	if _, err := os.Stat(dbPath); err == nil {
		t.Error("database file was created despite the rejected directory")
	}
	if _, err := os.Stat(dbPath + ".lock"); err == nil {
		t.Error("lock file was created despite the rejected directory")
	}
}

// TestFileStoreWritesVersionedEnvelope is the regression test for MYC-51 #6:
// the on-disk format must carry a schema version inside a versioned envelope,
// and the file written by this build must load again.
func TestFileStoreWritesVersionedEnvelope(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	if err := fs.Put("cards", "1", cardRecord{ChatID: 5, Title: "x"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database: %v", err)
	}
	var env onDiskEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("database file is not a versioned envelope: %v (content %s)", err, raw)
	}
	if env.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", env.SchemaVersion)
	}
	var rec cardRecord
	if err := json.Unmarshal(env.Data["cards"]["1"], &rec); err != nil {
		t.Fatalf("record under data envelope: %v", err)
	}
	if rec.Title != "x" {
		t.Errorf("record = %+v, want title x", rec)
	}

	reopened, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	var got cardRecord
	if ok, err := reopened.Get("cards", "1", &got); err != nil || !ok || got.Title != "x" {
		t.Errorf("Get after reopen = (%v, %+v, %v), want (true, x, nil)", ok, got, err)
	}
}

// TestFileStoreLoadsLegacyDatabaseAndUpgrades is the regression test for
// MYC-51 #6 (migration entry): a database written before the envelope existed
// (a bare {table: {key: record}} object, implicitly version 0) must still
// load, and the next commit must upgrade it to the versioned envelope.
func TestFileStoreLoadsLegacyDatabaseAndUpgrades(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	legacy := `{"chats":{"500":{"chat_id":500,"language":"zh","timezone":"Asia/Shanghai","welcomed":[7]}}}`
	if err := os.WriteFile(dbPath, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy database: %v", err)
	}

	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore on legacy format: %v", err)
	}
	var chat persistedChatRecord
	if ok, err := fs.Get("chats", "500", &chat); err != nil || !ok {
		t.Fatalf("Get from legacy database = (%v, %v), want (true, nil)", ok, err)
	}
	if chat.ChatID != 500 || chat.Language != "zh" || chat.Timezone != "Asia/Shanghai" || len(chat.Welcomed) != 1 {
		t.Errorf("legacy record = %+v", chat)
	}

	// The next commit upgrades the on-disk format to the versioned envelope.
	if err := fs.Put("chats", "501", persistedChatRecord{ChatID: 501, Language: "en", Timezone: "UTC"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database: %v", err)
	}
	var env onDiskEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("database after commit is not an envelope: %v (content %s)", err, raw)
	}
	if env.SchemaVersion != 1 {
		t.Errorf("database after commit has schema_version = %d, want 1 (legacy format was not upgraded)", env.SchemaVersion)
	}

	reopened, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if ok, err := reopened.Get("chats", "500", &chat); err != nil || !ok || chat.ChatID != 500 {
		t.Errorf("Get migrated record = (%v, %+v, %v), want (true, 500, nil)", ok, chat, err)
	}
	var rec persistedChatRecord
	if ok, err := reopened.Get("chats", "501", &rec); err != nil || !ok || rec.ChatID != 501 {
		t.Errorf("Get new record = (%v, %+v, %v), want (true, 501, nil)", ok, rec, err)
	}
}

// TestFileStoreRejectsUnknownSchemaVersion is the regression test for
// MYC-51 #6 (fail-closed): a database whose schema version this build does not
// know must be refused without being modified.
func TestFileStoreRejectsUnknownSchemaVersion(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	content := []byte(`{"schema_version":999,"data":{"chats":{"500":{"chat_id":500}}}}`)
	if err := os.WriteFile(dbPath, content, 0o600); err != nil {
		t.Fatalf("write database: %v", err)
	}
	fs, err := OpenFileStore(dbPath)
	if err == nil {
		_ = fs.Close()
		t.Fatal("OpenFileStore must fail closed on an unknown schema version")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("schema version")) {
		t.Errorf("error = %v, want a schema version message", err)
	}
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database: %v", err)
	}
	if !bytes.Equal(raw, content) {
		t.Error("database file was modified while rejecting it")
	}
}

// TestFileStoreRejectsPrimaryKeyMismatch is the regression test for MYC-51 #6
// (record validation): a record whose primary-key field contradicts the map
// key it is stored under must fail to load instead of serving mismatched
// state.
func TestFileStoreRejectsPrimaryKeyMismatch(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"chats", `{"chats":{"500":{"chat_id":600,"language":"en","timezone":"UTC","welcomed":[]}}}`},
		{"cards", `{"cards":{"5_7":{"chat_id":5,"user_id":9,"fields":{"name":"x"},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}}}`},
		{"publish_targets", `{"publish_targets":{"500":{"chat_id":600,"added_by":1,"added_at":"2026-01-01T00:00:00Z"}}}`},
		{"publishes", `{"publishes":{"publish_3":{"id":4,"source_chat_id":1,"sender_id":1,"kind":"text","targets":[1],"results":[]}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
			if err := os.WriteFile(dbPath, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write database: %v", err)
			}
			fs, err := OpenFileStore(dbPath)
			if err == nil {
				_ = fs.Close()
				t.Fatalf("OpenFileStore must reject a %s record whose primary key contradicts its map key", tc.name)
			}
		})
	}
}

// TestFileStoreRejectsMissingOrNullDataPayload is the regression test for
// MYC-81 blocker 1: a schema-1 envelope that lost its data payload (the data
// key is absent, data is JSON null, or a table value inside data is null)
// must be refused instead of loading as an empty database, and the file must
// stay byte-for-byte unchanged so the original data stays recoverable; the
// next successful commit would otherwise overwrite it. A null table in the
// legacy format must likewise be refused, not normalized to an empty table.
func TestFileStoreRejectsMissingOrNullDataPayload(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"missing data key", `{"schema_version":1}`},
		{"null data", `{"schema_version":1,"data":null}`},
		{"null table in data", `{"schema_version":1,"data":{"cards":null}}`},
		{"null legacy table", `{"chats":null,"cards":{"1":{"chat_id":5}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
			if err := os.WriteFile(dbPath, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write database: %v", err)
			}
			if fs, err := OpenFileStore(dbPath); err == nil {
				_ = fs.Close()
				t.Fatal("OpenFileStore must refuse a database whose data payload or a table is missing or null; loading it as empty would let the next commit overwrite the original data")
			}
			raw, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatalf("read database: %v", err)
			}
			if string(raw) != tc.content {
				t.Errorf("database file was modified while refusing it: %s", raw)
			}
		})
	}
}

// TestFileStoreRejectsMalformedRecords is the regression test for MYC-51 #6
// (record structure): records on disk must be JSON objects. A JSON null
// record (the trace the old transaction-delete bug left behind) must be
// refused, not silently served as a zero record.
func TestFileStoreRejectsMalformedRecords(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"null record", `{"cards":{"1":null}}`},
		{"array record", `{"cards":{"1":[{"chat_id":5}]}}`},
		{"array table", `{"cards":[{"chat_id":5}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
			if err := os.WriteFile(dbPath, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write database: %v", err)
			}
			fs, err := OpenFileStore(dbPath)
			if err == nil {
				_ = fs.Close()
				t.Fatalf("OpenFileStore must reject a malformed database (%s)", tc.name)
			}
		})
	}
}
