package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// FileStore is a generic, file-backed record store. It provides the
// persistence foundation for tele-admin: typed CRUD over named tables plus
// explicit transactions, so later slices can persist chat settings, target
// registries, cards, rules, templates, and publish records without a new
// dependency (JSON documents on disk, Go standard library only).
//
// On-disk format: the database file is a versioned envelope
// {"schema_version":N,"data":{table:{key:record}}} (see dbEnvelope). Databases
// written before versioning existed (a bare {table:{key:record}} object) load
// through the migration entry point and are upgraded on the next commit;
// unknown schema versions fail closed. Records are validated on load: every
// record must be a JSON object and, where the table has a primary-key field,
// it must agree with the key the record is stored under.
//
// Durability and atomicity: the full dataset is kept in memory and rewritten
// as one JSON snapshot on every commit. A commit writes a fresh, randomly
// named temporary file in the database's directory (created O_EXCL, forced to
// mode 0600, verified to be a regular file), fsyncs it, then atomically
// renames it over the database file. A failed commit therefore leaves the
// database file untouched, removes its temporary file, and can never be
// hijacked through a pre-planted path: there is never a partial write.
//
// Concurrency: a process-wide mutex serializes commits, and an exclusive
// advisory file lock (flock on the dedicated <db path>.lock file, held for the
// lifetime of the open store) serializes across processes. The lock file is
// never renamed or replaced, so the lock survives the renames every commit
// performs on the database file itself. The lock model is single-writer: a
// second process that opens the same file fails fast with an error instead of
// blocking or silently interleaving writes. Reads are served from the
// in-memory snapshot and never touch the disk.
//
// One transaction may be active at a time; Begin returns an error otherwise.
// While a transaction is active, store-level Put/Delete fail: write either
// inside a transaction or outside one, never both.
type FileStore struct {
	mu    sync.Mutex
	path  string
	lockF *os.File
	data  map[string]map[string]json.RawMessage
	tx    *FileTx
}

// FileTx is an active transaction on a FileStore. Reads see a snapshot that
// includes the staged writes; Commit applies them atomically, Rollback
// discards them.
type FileTx struct {
	store  *FileStore
	shadow map[string]map[string]json.RawMessage
}

// dbSchemaVersion is the schema version this build writes and understands.
// Bump it whenever the on-disk layout changes, and extend migrateDatabase to
// upgrade older versions to it.
const dbSchemaVersion = 1

// dbEnvelope is the versioned top-level on-disk format. Data holds the
// tables; schema_version lets future formats migrate instead of guessing what
// an unlabeled file means.
type dbEnvelope struct {
	SchemaVersion int                                   `json:"schema_version"`
	Data          map[string]map[string]json.RawMessage `json:"data"`
}

// OpenFileStore opens (creating if necessary) the database file at path and
// loads its contents. It fails when the path cannot be created or opened,
// when the database directory is writable by group or other, when another
// process holds the database lock, or when the file exists but holds corrupt
// data, records with an unknown schema version, or records that contradict
// their primary keys; a database that cannot be understood is never silently
// overwritten. An existing database file or lock file that grants group or
// other any access is tightened to owner-only.
func OpenFileStore(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("empty database path")
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
		if fi, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("stat database directory: %w", err)
		} else if !fi.Mode().IsDir() {
			return nil, fmt.Errorf("database directory %q is not a directory", dir)
		} else if fi.Mode().Perm()&0o022 != 0 {
			// A directory others can write lets them swap the database or
			// lock files underneath the store. Refuse instead of tightening:
			// the directory may predate the store and be shared on purpose.
			return nil, fmt.Errorf("database directory %q is writable by group or other (mode %o); move the database to a protected directory or fix its permissions", dir, fi.Mode().Perm())
		}
	}
	lockF, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open database lock file: %w", err)
	}
	if err := syscall.Flock(int(lockF.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockF.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("database %q is locked by another process; only one writer may open it at a time", path)
		}
		return nil, fmt.Errorf("lock database file: %w", err)
	}
	s := &FileStore{path: path, lockF: lockF, data: map[string]map[string]json.RawMessage{}}
	fail := func(err error) (*FileStore, error) {
		_ = lockF.Close()
		return nil, err
	}
	// The lock file is never replaced, so its mode matters for the store's
	// whole life: force owner-only access on an over-wide one.
	if err := tightenPerm(lockF); err != nil {
		return fail(fmt.Errorf("check lock file: %w", err))
	}
	// Create the database file when missing so the configured path always
	// exists once the store is open, and tighten an over-wide mode on it.
	dbF, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fail(fmt.Errorf("open database file: %w", err))
	}
	err = tightenPerm(dbF)
	if closeErr := dbF.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fail(fmt.Errorf("check database file: %w", err))
	}
	if err := s.load(); err != nil {
		return fail(err)
	}
	return s, nil
}

// tightenPerm enforces owner-only access on an open store file. A mode that
// grants group or other any access is rewritten to 0600; a narrower mode is
// kept (tightening never widens). The file must be a regular file: devices,
// pipes, or directories at a store path are refused instead of being written
// through.
func tightenPerm(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file (mode %s)", f.Name(), fi.Mode())
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("tighten permissions on %s: %w", f.Name(), err)
	}
	return nil
}

// Close releases the database lock and closes the lock file. The store must
// not be used afterwards. Every commit is already durable on disk, so no data
// is lost by closing without further action.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tx = nil
	if s.lockF == nil {
		return nil
	}
	err := s.lockF.Close()
	s.lockF = nil
	return err
}

// load reads the database file into memory. A missing or empty file is a
// fresh, uninitialized database; malformed content, an unknown schema
// version, or records that fail validation are errors.
func (s *FileStore) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read database file: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	tables, err := decodeDatabase(raw, s.path)
	if err != nil {
		return err
	}
	for table, records := range tables {
		if records == nil {
			records = map[string]json.RawMessage{}
		}
		for key, record := range records {
			if err := validateRecord(table, key, record); err != nil {
				return err
			}
		}
		s.data[table] = records
	}
	return nil
}

// decodeDatabase parses the raw database file into tables. The file is either
// the current versioned envelope (schema_version >= 1) or the legacy bare
// {table: {key: record}} object written before versioning existed (treated as
// implicitly version 0). Unknown versions fail closed.
func decodeDatabase(raw []byte, path string) (map[string]map[string]json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("database file %q is corrupt: %w (restore it from a backup or move it aside; it will not be overwritten silently)", path, err)
	}
	if versionRaw, ok := top["schema_version"]; ok {
		var version int
		if err := json.Unmarshal(versionRaw, &version); err != nil {
			return nil, fmt.Errorf("database %q: schema_version is not a number; refusing to load (restore it from a backup or move it aside; it will not be overwritten silently)", path)
		}
		return migrateDatabase(version, top, path)
	}
	return migrateLegacyDatabase(top, path)
}

// migrateDatabase is the schema migration entry point. version selects the
// on-disk layout: the current version decodes directly, and older versions
// are upgraded here step by step as they appear. Any other version fails
// closed rather than being loaded with guessed semantics. The data payload
// must be present and a real object, and every table inside it must be one:
// an envelope that lost its data (or a table reduced to JSON null) is
// refused instead of loading as an empty database, so the next commit can
// never overwrite the original data.
func migrateDatabase(version int, top map[string]json.RawMessage, path string) (map[string]map[string]json.RawMessage, error) {
	switch version {
	case dbSchemaVersion:
		dataRaw, ok := top["data"]
		if !ok {
			return nil, fmt.Errorf("database %q has no data payload; the envelope is incomplete and is refused (restore it from a backup or move it aside; it will not be overwritten silently)", path)
		}
		var env dbEnvelope
		if err := json.Unmarshal(dataRaw, &env.Data); err != nil {
			return nil, fmt.Errorf("database %q: data payload is corrupt: %w", path, err)
		}
		if env.Data == nil {
			return nil, fmt.Errorf("database %q: data payload is null; the database is corrupt and is refused (restore it from a backup or move it aside; it will not be overwritten silently)", path)
		}
		for table, records := range env.Data {
			if records == nil {
				return nil, fmt.Errorf("database %q: table %q is null; the database is corrupt and is refused (restore it from a backup or move it aside; it will not be overwritten silently)", path, table)
			}
		}
		return env.Data, nil
	default:
		return nil, fmt.Errorf("database %q uses schema version %d; this build supports schema version %d and refuses to load unknown versions", path, version, dbSchemaVersion)
	}
}

// migrateLegacyDatabase converts the pre-envelope format (a bare
// {table: {key: record}} object, implicitly schema version 0) to the current
// in-memory form. The next commit persists it in the versioned envelope. A
// table value of JSON null is refused instead of being normalized to an
// empty table, so corrupt content never becomes a loadable database.
func migrateLegacyDatabase(top map[string]json.RawMessage, path string) (map[string]map[string]json.RawMessage, error) {
	tables := make(map[string]map[string]json.RawMessage, len(top))
	for table, rawTable := range top {
		var records map[string]json.RawMessage
		if err := json.Unmarshal(rawTable, &records); err != nil {
			return nil, fmt.Errorf("database file %q is corrupt: table %q is not a record map: %w", path, table, err)
		}
		if records == nil {
			return nil, fmt.Errorf("database file %q: table %q is null; the database is corrupt and is refused (restore it from a backup or move it aside; it will not be overwritten silently)", path, table)
		}
		tables[table] = records
	}
	return tables, nil
}

// validateRecord checks one record read from disk. Every record must be a
// JSON object; a JSON null record is refused outright. For tables whose
// records carry a primary-key field, a record that declares those fields must
// agree with the key it is stored under, so an edited or corrupted database
// fails to load instead of serving mismatched state. Records that do not
// declare the fields are only structure-checked: the store is generic, and a
// table's exact record shape belongs to its writer.
func validateRecord(table, key string, raw json.RawMessage) error {
	if trimmed := bytes.TrimSpace(raw); bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("database record %q/%q is null; the database is corrupt (restore it from a backup or move it aside; it will not be overwritten silently)", table, key)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Errorf("database record %q/%q is not a JSON object: %w", table, key, err)
	}
	switch table {
	case "chats":
		if _, ok := obj["chat_id"]; ok {
			var rec persistedChatRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return fmt.Errorf("database record chats/%q: %w", key, err)
			}
			if strconv.FormatInt(rec.ChatID, 10) != key {
				return fmt.Errorf("database record chats/%q does not match its chat_id %d", key, rec.ChatID)
			}
		}
	case "cards":
		_, chatOK := obj["chat_id"]
		_, userOK := obj["user_id"]
		if chatOK && userOK {
			var rec Card
			if err := json.Unmarshal(raw, &rec); err != nil {
				return fmt.Errorf("database record cards/%q: %w", key, err)
			}
			if want := cardKey(rec.ChatID, rec.UserID); want != key {
				return fmt.Errorf("database record cards/%q does not match its chat_id %d and user_id %d", key, rec.ChatID, rec.UserID)
			}
		}
	case "publish_targets":
		if _, ok := obj["chat_id"]; ok {
			var rec PublishTarget
			if err := json.Unmarshal(raw, &rec); err != nil {
				return fmt.Errorf("database record publish_targets/%q: %w", key, err)
			}
			if want := targetKey(rec.ChatID); want != key {
				return fmt.Errorf("database record publish_targets/%q does not match its chat_id %d", key, rec.ChatID)
			}
		}
	case "publishes":
		if _, ok := obj["id"]; ok {
			var rec PublishRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return fmt.Errorf("database record publishes/%q: %w", key, err)
			}
			if want := publishKey(rec.ID); want != key {
				return fmt.Errorf("database record publishes/%q does not match its id %d", key, rec.ID)
			}
		}
	case "profiles":
		if _, err := strconv.ParseInt(key, 10, 64); err != nil {
			return fmt.Errorf("database record profiles/%q: key is not a chat id", key)
		}
	case "templates":
		// Namespaced keys ("<owner>/<name>") must agree with the record's own
		// owner and name. Bare-name keys are legacy records; their owner is
		// checked when the store attaches (see loadTemplatesLocked).
		if strings.Contains(key, "/") {
			var rec persistedTemplateRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return fmt.Errorf("database record templates/%q: %w", key, err)
			}
			if rec.Owner == 0 || rec.Name == "" || templateKey(rec.Owner, rec.Name) != key {
				return fmt.Errorf("database record templates/%q does not match its owner %d and name %q", key, rec.Owner, rec.Name)
			}
		}
	}
	return nil
}

func checkKey(table, key string) error {
	if table == "" {
		return errors.New("table name must not be empty")
	}
	if key == "" {
		return errors.New("record key must not be empty")
	}
	return nil
}

func encode(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode record: %w", err)
	}
	return json.RawMessage(raw), nil
}

// Put inserts or replaces one record and commits immediately. value may be any
// JSON-marshalable value (typically a struct); use Get with a pointer of the
// same type to read it back. It fails while a transaction is active.
func (s *FileStore) Put(table, key string, value any) error {
	if err := checkKey(table, key); err != nil {
		return err
	}
	raw, err := encode(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tx != nil {
		return errors.New("a transaction is in progress; write through the transaction")
	}
	if s.data[table] == nil {
		s.data[table] = map[string]json.RawMessage{}
	}
	old, hadOld := s.data[table][key]
	s.data[table][key] = raw
	if err := s.writeSnapshotLocked(s.data); err != nil {
		if hadOld {
			s.data[table][key] = old
		} else {
			delete(s.data[table], key)
		}
		return err
	}
	return nil
}

// Get reads one record into out (a pointer). It reports whether the record
// exists. The record is JSON-decoded into out, so out must be a pointer to the
// type written by Put.
func (s *FileStore) Get(table, key string, out any) (bool, error) {
	if err := checkKey(table, key); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records := s.data[table]
	if records == nil {
		return false, nil
	}
	raw, ok := records[key]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, fmt.Errorf("decode record %q/%q: %w", table, key, err)
	}
	return true, nil
}

// Delete removes one record and commits immediately. Deleting a missing record
// is a no-op. It fails while a transaction is active.
func (s *FileStore) Delete(table, key string) error {
	if err := checkKey(table, key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tx != nil {
		return errors.New("a transaction is in progress; write through the transaction")
	}
	if _, ok := s.data[table][key]; !ok {
		return nil
	}
	old := s.data[table][key]
	delete(s.data[table], key)
	if err := s.writeSnapshotLocked(s.data); err != nil {
		s.data[table][key] = old
		return err
	}
	return nil
}

// Keys returns the sorted record keys of a table. A missing table yields an
// empty slice. The view is the snapshot before any active transaction.
func (s *FileStore) Keys(table string) []string {
	if table == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records := s.data[table]
	keys := make([]string, 0, len(records))
	for k := range records {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Begin starts a transaction. Only one transaction may be active at a time.
func (s *FileStore) Begin() (*FileTx, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tx != nil {
		return nil, errors.New("a transaction is already in progress")
	}
	shadow := make(map[string]map[string]json.RawMessage, len(s.data))
	for table, records := range s.data {
		clone := make(map[string]json.RawMessage, len(records))
		for k, v := range records {
			clone[k] = v
		}
		shadow[table] = clone
	}
	s.tx = &FileTx{store: s, shadow: shadow}
	return s.tx, nil
}

// Put stages a write inside the transaction. It becomes visible to the
// transaction's own reads immediately and to other readers only after Commit.
func (tx *FileTx) Put(table, key string, value any) error {
	if err := checkKey(table, key); err != nil {
		return err
	}
	raw, err := encode(value)
	if err != nil {
		return err
	}
	if tx.shadow[table] == nil {
		tx.shadow[table] = map[string]json.RawMessage{}
	}
	tx.shadow[table][key] = raw
	return nil
}

// Get reads one record inside the transaction, seeing staged writes on top of
// the snapshot the transaction began from. A staged delete reads back as a
// clean not-found.
func (tx *FileTx) Get(table, key string, out any) (bool, error) {
	if err := checkKey(table, key); err != nil {
		return false, err
	}
	raw, ok := tx.shadow[table][key]
	if !ok || raw == nil {
		return false, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, fmt.Errorf("decode record %q/%q: %w", table, key, err)
	}
	return true, nil
}

// Delete stages a removal inside the transaction. The shadow marks the key
// with a nil record; Commit drops it from the applied snapshot, so a committed
// delete removes the record instead of persisting a JSON null.
func (tx *FileTx) Delete(table, key string) error {
	if err := checkKey(table, key); err != nil {
		return err
	}
	if tx.shadow[table] == nil {
		tx.shadow[table] = map[string]json.RawMessage{}
	}
	tx.shadow[table][key] = nil
	return nil
}

// Keys returns the sorted keys visible inside the transaction.
func (tx *FileTx) Keys(table string) []string {
	if table == "" {
		return nil
	}
	keys := make([]string, 0, len(tx.shadow[table]))
	for k, v := range tx.shadow[table] {
		if v != nil {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// Commit applies the staged writes atomically. A staged delete removes its
// record: the committed snapshot never carries a JSON null. On any failure the
// transaction is rolled back: neither the database file nor the in-memory
// state changes, so a failed transaction never leaves partial writes behind.
func (tx *FileTx) Commit() error {
	s := tx.store
	s.mu.Lock()
	defer s.mu.Unlock()
	applied := make(map[string]map[string]json.RawMessage, len(tx.shadow))
	for table, records := range tx.shadow {
		clone := make(map[string]json.RawMessage, len(records))
		for key, raw := range records {
			if raw == nil {
				continue // staged delete: the record is removed, not written
			}
			clone[key] = raw
		}
		applied[table] = clone
	}
	if err := s.writeSnapshotLocked(applied); err != nil {
		s.tx = nil
		return err
	}
	s.data = applied
	s.tx = nil
	return nil
}

// Rollback discards the staged writes. The store returns to the state it had
// before Begin.
func (tx *FileTx) Rollback() {
	s := tx.store
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tx = nil
}

// writeSnapshotLocked serializes snapshot inside the versioned envelope,
// writes it to a fresh temporary file in the database's directory, fsyncs it,
// and atomically renames it over the database file. The temporary file gets a
// random name created O_EXCL (os.CreateTemp), is forced to mode 0600, and is
// verified to be a regular file, so a pre-planted symlink or an over-wide
// leftover can never be hijacked or read. Every failure path removes the
// temporary file and leaves the database file untouched. The caller must hold
// s.mu.
func (s *FileStore) writeSnapshotLocked(snapshot map[string]map[string]json.RawMessage) error {
	raw, err := json.Marshal(dbEnvelope{SchemaVersion: dbSchemaVersion, Data: snapshot})
	if err != nil {
		return fmt.Errorf("encode database snapshot: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmp := f.Name()
	committed := false
	defer func() {
		if !committed {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("set temporary file permissions: %w", err)
	}
	if fi, err := f.Stat(); err != nil {
		return fmt.Errorf("stat temporary file: %w", err)
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("temporary file %s is not a regular file", tmp)
	}
	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync temporary file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace database file: %w", err)
	}
	committed = true
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
