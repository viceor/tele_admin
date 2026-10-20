package store

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Publish targets and publish records (slice S2).
//
// The target registry lists the chats that may receive /publish deliveries.
// A chat is registered by an administrator with /publish_add in that chat, so
// registration is always tied to a verifiable admin action in the target
// itself. Records persist every publish task with its per-target results, so
// operators can audit what was sent where and what failed.

// PublishTarget is one chat registered to receive published content.
type PublishTarget struct {
	ChatID  int64     `json:"chat_id"`
	AddedBy int64     `json:"added_by"`
	AddedAt time.Time `json:"added_at"`
}

// PublishResult is the outcome for one target of a publish task.
type PublishResult struct {
	ChatID  int64  `json:"chat_id"`
	Success bool   `json:"success"`
	Reason  string `json:"reason,omitempty"`
}

// PublishRecord is one publish task: what was sent, by whom, to which
// targets, and what the per-target outcomes were. Success is derived, not
// stored: it is true only when every per-target result succeeded.
type PublishRecord struct {
	ID           int64           `json:"id"`
	SourceChatID int64           `json:"source_chat_id"`
	SenderID     int64           `json:"sender_id"`
	Kind         string          `json:"kind"`
	Targets      []int64         `json:"targets"`
	Results      []PublishResult `json:"results"`
	CreatedAt    time.Time       `json:"created_at"`
}

// ErrTargetRegistered and ErrTargetNotRegistered are returned when the
// registry mutation is redundant: adding an already-registered chat or
// removing an unregistered one.
var (
	ErrTargetRegistered    = errors.New("publish target already registered")
	ErrTargetNotRegistered = errors.New("publish target is not registered")
)

// targetKey renders the FileStore record key for one target chat.
func targetKey(chatID int64) string { return strconv.FormatInt(chatID, 10) }

// publishKey renders the FileStore record key for one publish record ID.
func publishKey(id int64) string { return "publish_" + strconv.FormatInt(id, 10) }

// loadPersistedPublishStateLocked rebuilds the target registry and the
// publish records from the FileStore while holding s.mu. It fails the attach
// when persisted records cannot be decoded, so the Store never serves
// publish state that contradicts the database.
func (s *Store) loadPersistedPublishStateLocked(fs *FileStore) error {
	s.publishTargets = make(map[int64]PublishTarget)
	for _, key := range fs.Keys("publish_targets") {
		var rec PublishTarget
		ok, err := fs.Get("publish_targets", key, &rec)
		if err != nil {
			return fmt.Errorf("read persisted publish target %q: %w", key, err)
		}
		if !ok {
			return fmt.Errorf("persisted publish target %q vanished during attach", key)
		}
		s.publishTargets[rec.ChatID] = rec
	}
	s.publishes = make(map[int64]PublishRecord)
	for _, key := range fs.Keys("publishes") {
		var rec PublishRecord
		ok, err := fs.Get("publishes", key, &rec)
		if err != nil {
			return fmt.Errorf("read persisted publish record %q: %w", key, err)
		}
		if !ok {
			return fmt.Errorf("persisted publish record %q vanished during attach", key)
		}
		s.publishes[rec.ID] = rec
		if rec.ID >= s.nextPublishID {
			s.nextPublishID = rec.ID + 1
		}
	}
	return nil
}

// persistTargetLocked writes one registry entry through to the FileStore.
// The returned error wraps ErrPersistFailed after the failure is logged; the
// caller must leave the in-memory registry unchanged so it never leads what
// is on disk.
func (s *Store) persistTargetLocked(t PublishTarget) error {
	if s.persist == nil {
		return nil
	}
	if err := s.persist.Put("publish_targets", targetKey(t.ChatID), t); err != nil {
		s.plog.Error("persist publish target failed; change is rolled back", "chat", t.ChatID, "err", err)
		return persistErr(err)
	}
	return nil
}

// persistPublishLocked writes one publish record through to the FileStore.
// The returned error wraps ErrPersistFailed after the failure is logged.
func (s *Store) persistPublishLocked(rec PublishRecord) error {
	if s.persist == nil {
		return nil
	}
	if err := s.persist.Put("publishes", publishKey(rec.ID), rec); err != nil {
		s.plog.Error("persist publish record failed; change is rolled back", "id", rec.ID, "err", err)
		return persistErr(err)
	}
	return nil
}

// AddPublishTarget registers chatID as a publish target, attributed to
// addedBy. Re-registering an existing target is a no-op that fails with
// ErrTargetRegistered instead of overwriting the original attribution. The
// record is written to disk before the registry changes, so a persistence
// failure (error wrapping ErrPersistFailed) leaves both sides unregistered.
func (s *Store) AddPublishTarget(chatID, addedBy int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.publishTargets[chatID]; ok {
		return ErrTargetRegistered
	}
	t := PublishTarget{ChatID: chatID, AddedBy: addedBy, AddedAt: s.nowFunc()}
	if err := s.persistTargetLocked(t); err != nil {
		return err
	}
	if s.publishTargets == nil {
		s.publishTargets = make(map[int64]PublishTarget)
	}
	s.publishTargets[chatID] = t
	return nil
}

// RemovePublishTarget unregisters chatID. Removing an unregistered chat
// fails with ErrTargetNotRegistered. The disk record is deleted before the
// in-memory registry changes, so a persistence failure (error wrapping
// ErrPersistFailed) keeps the chat registered on both sides.
func (s *Store) RemovePublishTarget(chatID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.publishTargets[chatID]; !ok {
		return ErrTargetNotRegistered
	}
	if s.persist == nil {
		delete(s.publishTargets, chatID)
		return nil
	}
	if err := s.persist.Delete("publish_targets", targetKey(chatID)); err != nil {
		s.plog.Error("remove persisted publish target failed; change is rolled back", "chat", chatID, "err", err)
		return persistErr(err)
	}
	delete(s.publishTargets, chatID)
	return nil
}

// IsPublishTarget reports whether chatID is registered to receive publishes.
func (s *Store) IsPublishTarget(chatID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.publishTargets[chatID]
	return ok
}

// PublishTargets returns the registered target chat IDs in ascending order.
func (s *Store) PublishTargets() []int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]int64, 0, len(s.publishTargets))
	for id := range s.publishTargets {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// PublishTargetCount returns how many chats are registered as targets.
func (s *Store) PublishTargetCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.publishTargets)
}

// AddPublish stores one publish task with its per-target results and assigns
// a monotonically increasing ID. targets is copied, so later mutation of the
// caller's slice cannot change the stored record. The record is written to
// disk before in-memory state changes; when the write fails the returned
// error wraps ErrPersistFailed, the ID is not consumed, and the record is
// registered nowhere — the caller must make clear that messages may have
// been sent but the send is not recorded.
func (s *Store) AddPublish(sourceChatID, senderID int64, kind string, targets []int64, results []PublishResult) (PublishRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := PublishRecord{
		ID:           s.nextPublishID,
		SourceChatID: sourceChatID,
		SenderID:     senderID,
		Kind:         kind,
		Targets:      append([]int64(nil), targets...),
		Results:      append([]PublishResult(nil), results...),
		CreatedAt:    s.nowFunc(),
	}
	if err := s.persistPublishLocked(rec); err != nil {
		return rec, err
	}
	s.nextPublishID++
	if s.publishes == nil {
		s.publishes = make(map[int64]PublishRecord)
	}
	s.publishes[rec.ID] = rec
	return rec, nil
}

// PublishRecords returns the stored publish records with ID <= maxID, newest
// first, at most limit of them (0 means no limit). The slice order is
// deterministic for display and auditing.
func (s *Store) PublishRecords(maxID int64, limit int) []PublishRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]int64, 0, len(s.publishes))
	for id := range s.publishes {
		if maxID <= 0 || id <= maxID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	out := make([]PublishRecord, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.publishes[id])
	}
	return out
}
