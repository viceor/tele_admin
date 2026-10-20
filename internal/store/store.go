// Package store keeps per-chat settings and anti-spam rate windows in memory,
// optionally writing chat settings through to a FileStore so they survive a
// restart (see FileStore and Store.AttachFileStore).
package store

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/viceor/tele-admin/internal/i18n"
)

// ErrPersistFailed marks a write-through persistence failure. It wraps the
// underlying cause and is returned by mutating methods when the FileStore
// could not commit the record; callers must tell the user the change was not
// persisted instead of reporting success. Memory and disk never disagree
// after a failed mutation: the in-memory change is rolled back.
var ErrPersistFailed = errors.New("persistence failed")

// persistErr wraps cause in ErrPersistFailed for returning to callers.
func persistErr(cause error) error {
	return fmt.Errorf("%w: %v", ErrPersistFailed, cause)
}

// SettingsSnapshot is an immutable copy of one chat's settings. It is safe to
// read without holding the Store lock, and mutating it never affects the
// Store. Obtain one with Store.Settings; never write settings through it.
type SettingsSnapshot struct {
	ChatID   int64
	Language i18n.Language
	Timezone string
	// Welcomed is a copy of the set of user IDs already welcomed in the chat.
	Welcomed map[int64]bool
}

// chatSettings is the store's internal mutable per-chat state. Every field
// must only be read or written while holding Store.mu; callers outside the
// package can only observe it through a SettingsSnapshot.
type chatSettings struct {
	chatID   int64
	language i18n.Language
	timezone string
	welcomed map[int64]bool
	// welcoming tracks users with an in-flight welcome send. It prevents two
	// concurrent duplicate join events from both sending a welcome, while
	// still allowing a retry after a failed send releases the reservation.
	welcoming map[int64]bool
}

// snapshot returns a deep copy of cs that callers may use outside the lock.
func (cs *chatSettings) snapshot() SettingsSnapshot {
	welcomed := make(map[int64]bool, len(cs.welcomed))
	for id, ok := range cs.welcomed {
		welcomed[id] = ok
	}
	return SettingsSnapshot{
		ChatID:   cs.chatID,
		Language: cs.language,
		Timezone: cs.timezone,
		Welcomed: welcomed,
	}
}

// rateWindow tracks recent message timestamps for one user.
type rateWindow struct {
	timestamps []time.Time
}

// rateWindowRetention is the minimum time a recorded timestamp can influence a
// decision. It is a floor; the effective retention also grows with the
// configured minimum interval so that the min_interval rule keeps its evidence
// (see Store.retention).
const rateWindowRetention = time.Minute

// defaultMaxRateWindows bounds how many per-user rate windows are tracked
// before RecordMessage sweeps inert ones. It keeps memory proportional to the
// number of users active in the retention window instead of every user ever
// seen.
const defaultMaxRateWindows = 4096

// Store is a concurrency-safe in-memory state container. When a FileStore is
// attached (AttachFileStore), chat settings and cards are written through to
// it on every mutation, so language, timezone, welcome state, and AI business
// cards survive a restart. Rate windows and in-flight welcome reservations are
// deliberately not persisted: they are transient, and losing them on restart
// only relaxes anti-spam enforcement and allows one welcome retry.
type Store struct {
	mu              sync.RWMutex
	chats           map[int64]*chatSettings
	cards           map[int64]map[int64]*cardEntry
	publishTargets  map[int64]PublishTarget
	publishes       map[int64]PublishRecord
	nextPublishID   int64
	profiles        map[int64]*ProfileRecord
	templates       map[string]*templateEntry
	rates           map[int64]*rateWindow
	maxPer          int
	minGap          time.Duration
	nowFunc         func() time.Time
	maxRateWindow   int
	defaultLanguage i18n.Language
	defaultTimezone string
	persist         *FileStore
	plog            *slog.Logger
}

// persistedChatRecord is the on-disk shape of one chat's settings in the
// "chats" table of the FileStore.
type persistedChatRecord struct {
	ChatID   int64   `json:"chat_id"`
	Language string  `json:"language"`
	Timezone string  `json:"timezone"`
	Welcomed []int64 `json:"welcomed"`
}

// New creates a Store with the given anti-spam limits and the built-in
// defaults (English, UTC). Use NewWithDefaults to apply configured defaults.
func New(maxPerMinute int, minIntervalMs int) *Store {
	return NewWithDefaults(maxPerMinute, minIntervalMs, i18n.English, "UTC")
}

// NewWithDefaults creates a Store with the given anti-spam limits and the
// per-chat defaults applied to any chat before it is explicitly configured.
func NewWithDefaults(maxPerMinute int, minIntervalMs int, defaultLanguage i18n.Language, defaultTimezone string) *Store {
	if defaultLanguage == "" {
		defaultLanguage = i18n.English
	}
	if defaultTimezone == "" {
		defaultTimezone = "UTC"
	}
	return &Store{
		chats:           make(map[int64]*chatSettings),
		cards:           make(map[int64]map[int64]*cardEntry),
		publishTargets:  make(map[int64]PublishTarget),
		publishes:       make(map[int64]PublishRecord),
		nextPublishID:   1,
		profiles:        make(map[int64]*ProfileRecord),
		templates:       make(map[string]*templateEntry),
		rates:           make(map[int64]*rateWindow),
		maxPer:          maxPerMinute,
		minGap:          time.Duration(minIntervalMs) * time.Millisecond,
		nowFunc:         time.Now,
		maxRateWindow:   defaultMaxRateWindows,
		defaultLanguage: defaultLanguage,
		defaultTimezone: defaultTimezone,
		plog:            slog.Default(),
	}
}

// FileStore returns the attached persistence backend, or nil when the store
// runs in memory only.
func (s *Store) FileStore() *FileStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.persist
}

// AttachFileStore switches the store to write-through persistence on fs and
// loads the persisted chat settings into memory. It must be called before the
// Store serves traffic, and only once. It fails when persistence is already
// attached or when the stored chat records cannot be read or decoded; a decode
// failure aborts the attach rather than risk serving state that contradicts
// the database.
func (s *Store) AttachFileStore(fs *FileStore, logger *slog.Logger) error {
	if logger != nil {
		s.plog = logger
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persist != nil {
		return errors.New("persistence is already attached")
	}
	for _, key := range fs.Keys("chats") {
		var rec persistedChatRecord
		ok, err := fs.Get("chats", key, &rec)
		if err != nil {
			return fmt.Errorf("read persisted chat %q: %w", key, err)
		}
		if !ok {
			return fmt.Errorf("persisted chat %q vanished during attach", key)
		}
		welcomed := make(map[int64]bool, len(rec.Welcomed))
		for _, id := range rec.Welcomed {
			welcomed[id] = true
		}
		s.chats[rec.ChatID] = &chatSettings{
			chatID:    rec.ChatID,
			language:  i18n.Language(rec.Language),
			timezone:  rec.Timezone,
			welcomed:  welcomed,
			welcoming: make(map[int64]bool),
		}
	}
	if err := s.loadPersistedCardsLocked(fs); err != nil {
		return err
	}
	if err := s.loadPersistedPublishStateLocked(fs); err != nil {
		return err
	}
	if err := s.loadProfilesLocked(fs); err != nil {
		return err
	}
	if err := s.loadTemplatesLocked(fs); err != nil {
		return err
	}
	s.persist = fs
	return nil
}

// persistChatLocked writes one chat's settings through to the FileStore. It is
// called while the Store lock is held. The returned error wraps
// ErrPersistFailed after the failure is logged; the caller must roll the
// in-memory mutation back so memory never leads what is on disk.
func (s *Store) persistChatLocked(cs *chatSettings) error {
	if s.persist == nil {
		return nil
	}
	rec := persistedChatRecord{
		ChatID:   cs.chatID,
		Language: string(cs.language),
		Timezone: cs.timezone,
		Welcomed: welcomedIDs(cs.welcomed),
	}
	if err := s.persist.Put("chats", strconv.FormatInt(cs.chatID, 10), rec); err != nil {
		s.plog.Error("persist chat settings failed; change is rolled back", "chat", cs.chatID, "err", err)
		return persistErr(err)
	}
	return nil
}

// welcomedIDs renders the welcomed set as a sorted slice so the persisted form
// is deterministic.
func welcomedIDs(welcomed map[int64]bool) []int64 {
	ids := make([]int64, 0, len(welcomed))
	for id := range welcomed {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// retention reports how long a rate-window timestamp must be kept. It covers
// the one-minute max_per_minute window and at least twice the minimum interval,
// so the min_interval rule can still see the previous message even when the
// interval exceeds one minute.
func (s *Store) retention() time.Duration {
	r := rateWindowRetention
	if d := 2 * s.minGap; d > r {
		r = d
	}
	return r
}

// Settings returns an isolated snapshot of the settings for a chat, creating
// the chat's state if it is absent. The returned value is a copy: mutating it
// (including its Welcomed map) never changes the Store, so the caller cannot
// race with concurrent readers or writers.
func (s *Store) Settings(chatID int64) SettingsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settingsLocked(chatID).snapshot()
}

// Language returns the stored interface language for a chat. It reads under
// the lock and creates default settings for an unknown chat.
func (s *Store) Language(chatID int64) i18n.Language {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settingsLocked(chatID).language
}

// Timezone returns the stored IANA timezone for a chat. It reads under the
// lock and creates default settings for an unknown chat.
func (s *Store) Timezone(chatID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settingsLocked(chatID).timezone
}

// settingsLocked returns the internal settings for a chat, creating them if
// absent. The caller must hold s.mu.
func (s *Store) settingsLocked(chatID int64) *chatSettings {
	cs, ok := s.chats[chatID]
	if !ok {
		cs = &chatSettings{
			chatID:    chatID,
			language:  s.defaultLanguage,
			timezone:  s.defaultTimezone,
			welcomed:  make(map[int64]bool),
			welcoming: make(map[int64]bool),
		}
		s.chats[chatID] = cs
	}
	return cs
}

// SetLanguage stores the interface language for a chat and persists it. When
// the write-through fails, the in-memory change is rolled back and the
// returned error wraps ErrPersistFailed; the change was not persisted.
func (s *Store) SetLanguage(chatID int64, lang i18n.Language) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.settingsLocked(chatID)
	old := cs.language
	cs.language = lang
	if err := s.persistChatLocked(cs); err != nil {
		cs.language = old
		return err
	}
	return nil
}

// SetTimezone stores the IANA timezone for a chat and persists it. When the
// write-through fails, the in-memory change is rolled back and the returned
// error wraps ErrPersistFailed; the change was not persisted.
func (s *Store) SetTimezone(chatID int64, tz string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.settingsLocked(chatID)
	old := cs.timezone
	cs.timezone = tz
	if err := s.persistChatLocked(cs); err != nil {
		cs.timezone = old
		return err
	}
	return nil
}

// RecordMessage records a message from a user and reports whether it is allowed.
// allowed is false when the user exceeds the rate limit or posts too quickly.
func (s *Store) RecordMessage(userID int64, now time.Time) (allowed bool, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxRateWindow > 0 && len(s.rates) >= s.maxRateWindow {
		s.pruneRatesLocked(now)
	}
	rw, ok := s.rates[userID]
	if !ok {
		rw = &rateWindow{}
		s.rates[userID] = rw
	}
	rw.timestamps = append(rw.timestamps, now)
	if s.minGap > 0 {
		for len(rw.timestamps) > 1 && now.Sub(rw.timestamps[0]) > s.minGap*2 {
			rw.timestamps = rw.timestamps[1:]
		}
	}
	if len(rw.timestamps) > 1 {
		last := rw.timestamps[len(rw.timestamps)-2]
		if now.Sub(last) < s.minGap {
			rw.timestamps = rw.timestamps[:len(rw.timestamps)-1]
			return false, "min_interval"
		}
	}
	cutoff := now.Add(-time.Minute)
	kept := rw.timestamps[:0]
	for _, t := range rw.timestamps {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	rw.timestamps = kept
	if len(rw.timestamps) > s.maxPer {
		rw.timestamps = rw.timestamps[:len(rw.timestamps)-1]
		return false, "max_per_minute"
	}
	return true, ""
}

// pruneRatesLocked deletes rate windows that can no longer affect a decision
// because their newest timestamp is at least the retention period old. It
// returns the number of windows removed. The caller must hold s.mu.
func (s *Store) pruneRatesLocked(now time.Time) int {
	retention := s.retention()
	removed := 0
	for id, rw := range s.rates {
		if len(rw.timestamps) == 0 || now.Sub(rw.timestamps[len(rw.timestamps)-1]) >= retention {
			delete(s.rates, id)
			removed++
		}
	}
	return removed
}

// MarkWelcomed records that a user has already been welcomed in a chat.
//
// The welcomed set grows with the number of distinct members ever welcomed in
// a chat and is intentionally not evicted: dropping an entry would re-send a
// welcome to a member who may still be present, which is a visible behavior
// change. A bounded follow-up (see README) is to consume left_chat_member
// updates and delete the departing member's entry, which would cap the set at
// the current membership.
func (s *Store) MarkWelcomed(chatID, userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.settingsLocked(chatID)
	prev, had := cs.welcomed[userID]
	cs.welcomed[userID] = true
	if err := s.persistChatLocked(cs); err != nil {
		if had {
			cs.welcomed[userID] = prev
		} else {
			delete(cs.welcomed, userID)
		}
		return err
	}
	return nil
}

// WasWelcomed reports whether a user already received a welcome message.
// It returns false for a chat that has no settings yet.
func (s *Store) WasWelcomed(chatID, userID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cs, ok := s.chats[chatID]
	if !ok {
		return false
	}
	return cs.welcomed[userID]
}

// BeginWelcome atomically reserves the right to send a welcome message for
// (chatID, userID). It returns true for exactly one caller when the user has
// not yet been welcomed and no send is already in flight; concurrent duplicate
// join events therefore do not all send a welcome. The caller must finish the
// reservation with CompleteWelcome on success or AbortWelcome on failure.
func (s *Store) BeginWelcome(chatID, userID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.settingsLocked(chatID)
	if cs.welcomed[userID] || cs.welcoming[userID] {
		return false
	}
	cs.welcoming[userID] = true
	return true
}

// CompleteWelcome records that a welcome message was delivered successfully and
// clears the in-flight reservation. Only after this call does WasWelcomed
// report true, so a failed send is never treated as already welcomed. When the
// write-through fails, the welcomed flag is rolled back (an in-flight
// reservation is re-opened) and the returned error wraps ErrPersistFailed.
func (s *Store) CompleteWelcome(chatID, userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.settingsLocked(chatID)
	delete(cs.welcoming, userID)
	cs.welcomed[userID] = true
	if err := s.persistChatLocked(cs); err != nil {
		delete(cs.welcomed, userID)
		cs.welcoming[userID] = true
		return err
	}
	return nil
}

// AbortWelcome releases an in-flight welcome reservation after a failed send.
// It does not mark the user as welcomed, so a later join event can retry.
func (s *Store) AbortWelcome(chatID, userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.settingsLocked(chatID).welcoming, userID)
}
