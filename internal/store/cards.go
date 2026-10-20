package store

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CardFields holds the user-authored content of one AI business card. All
// fields are optional except Name, which identifies the owner in listings.
type CardFields struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Skills  string `json:"skills"`
	Contact string `json:"contact"`
	Bio     string `json:"bio"`
}

// Card is one member's card in one chat, with audit timestamps.
type Card struct {
	ChatID    int64      `json:"chat_id"`
	UserID    int64      `json:"user_id"`
	Fields    CardFields `json:"fields"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Card ownership errors. Callers map these to user-facing messages; they are
// sentinels so errors.Is works across package boundaries.
var (
	ErrCardExists   = errors.New("card already exists")
	ErrCardNotFound = errors.New("card not found")
)

// cardEntry is the in-memory card state behind Store.cards. All access is
// guarded by Store.mu; persistence goes through Store.persistCardLocked.
type cardEntry struct {
	fields    CardFields
	createdAt time.Time
	updatedAt time.Time
}

// cardKey renders the FileStore record key for one (chat, user) pair. Cards
// are unique per chat: the same user may hold a different card in each chat.
func cardKey(chatID, userID int64) string {
	return strconv.FormatInt(chatID, 10) + "_" + strconv.FormatInt(userID, 10)
}

// cardChat returns the per-chat card map, creating it if absent. The caller
// must hold s.mu.
func (s *Store) cardChat(chatID int64) map[int64]*cardEntry {
	m, ok := s.cards[chatID]
	if !ok {
		m = make(map[int64]*cardEntry)
		s.cards[chatID] = m
	}
	return m
}

// cardEntry looks up one (chat, user) card without creating state. The caller
// must hold s.mu.
func (s *Store) cardEntry(chatID, userID int64) (*cardEntry, bool) {
	m, ok := s.cards[chatID]
	if !ok {
		return nil, false
	}
	e, ok := m[userID]
	return e, ok
}

// persistCardLocked writes one card through to the FileStore. The returned
// error wraps ErrPersistFailed after the failure is logged; the caller must
// leave the in-memory card state unchanged so it never leads what is on disk.
func (s *Store) persistCardLocked(chatID, userID int64, e *cardEntry) error {
	if s.persist == nil {
		return nil
	}
	rec := Card{
		ChatID:    chatID,
		UserID:    userID,
		Fields:    e.fields,
		CreatedAt: e.createdAt,
		UpdatedAt: e.updatedAt,
	}
	if err := s.persist.Put("cards", cardKey(chatID, userID), rec); err != nil {
		s.plog.Error("persist card failed; change is rolled back", "chat", chatID, "user", userID, "err", err)
		return persistErr(err)
	}
	return nil
}

// loadPersistedCardsLocked rebuilds in-memory card state from the FileStore
// while holding s.mu. It fails the attach when persisted records cannot be
// decoded, so the Store never serves card state that contradicts the database.
func (s *Store) loadPersistedCardsLocked(fs *FileStore) error {
	for _, key := range fs.Keys("cards") {
		var rec Card
		ok, err := fs.Get("cards", key, &rec)
		if err != nil {
			return fmt.Errorf("read persisted card %q: %w", key, err)
		}
		if !ok {
			return fmt.Errorf("persisted card %q vanished during attach", key)
		}
		s.cardChat(rec.ChatID)[rec.UserID] = &cardEntry{
			fields:    rec.Fields,
			createdAt: rec.CreatedAt,
			updatedAt: rec.UpdatedAt,
		}
	}
	return nil
}

// CreateCard inserts a new card for (chatID, userID). It fails with
// ErrCardExists when the user already has a card in this chat; creation is
// idempotent-safe: a duplicate attempt never overwrites the existing card.
// The record is written to disk before the in-memory card appears; on a
// persistence failure the error wraps ErrPersistFailed and no card exists.
func (s *Store) CreateCard(chatID, userID int64, fields CardFields) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.cardEntry(chatID, userID); exists {
		return ErrCardExists
	}
	now := s.nowFunc()
	e := &cardEntry{fields: fields, createdAt: now, updatedAt: now}
	if err := s.persistCardLocked(chatID, userID, e); err != nil {
		return err
	}
	s.cardChat(chatID)[userID] = e
	return nil
}

// GetCard returns the card of (chatID, userID) and whether it exists.
func (s *Store) GetCard(chatID, userID int64) (Card, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.cardEntry(chatID, userID)
	if !ok {
		return Card{}, false
	}
	return Card{ChatID: chatID, UserID: userID, Fields: e.fields, CreatedAt: e.createdAt, UpdatedAt: e.updatedAt}, true
}

// UpdateCard replaces the stored fields of (chatID, userID). It fails with
// ErrCardNotFound when no card exists; update is never an implicit create.
// When the write-through fails the stored fields are rolled back and the
// error wraps ErrPersistFailed; the card keeps its previous values.
func (s *Store) UpdateCard(chatID, userID int64, fields CardFields) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cardEntry(chatID, userID)
	if !ok {
		return ErrCardNotFound
	}
	oldFields, oldUpdatedAt := e.fields, e.updatedAt
	e.fields = fields
	e.updatedAt = s.nowFunc()
	if err := s.persistCardLocked(chatID, userID, e); err != nil {
		e.fields, e.updatedAt = oldFields, oldUpdatedAt
		return err
	}
	return nil
}

// SearchCards returns the chat's cards whose fields contain keyword as a
// case-insensitive substring (matched against name, title, skills, contact,
// and bio), ordered by user ID for deterministic output. An empty keyword
// matches nothing.
func (s *Store) SearchCards(chatID int64, keyword string) []Card {
	s.mu.RLock()
	defer s.mu.RUnlock()
	needle := strings.ToLower(strings.TrimSpace(keyword))
	if needle == "" {
		return nil
	}
	var out []Card
	m, ok := s.cards[chatID]
	if !ok {
		return nil
	}
	for _, userID := range sortedUserIDs(m) {
		e := m[userID]
		if cardMatches(e.fields, needle) {
			out = append(out, Card{ChatID: chatID, UserID: userID, Fields: e.fields, CreatedAt: e.createdAt, UpdatedAt: e.updatedAt})
		}
	}
	return out
}

// sortedUserIDs returns the keys of a per-chat card map in ascending order.
func sortedUserIDs(m map[int64]*cardEntry) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// cardMatches reports whether any card field contains the already-lowered
// keyword.
func cardMatches(f CardFields, lowerKeyword string) bool {
	for _, v := range []string{f.Name, f.Title, f.Skills, f.Contact, f.Bio} {
		if strings.Contains(strings.ToLower(v), lowerKeyword) {
			return true
		}
	}
	return false
}
