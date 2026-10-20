package store

import (
	"errors"
	"testing"
	"time"
)

func testCardFields(name string) CardFields {
	return CardFields{Name: name, Title: "engineer", Skills: "Go, Python", Contact: "alice@example.com", Bio: "builds things"}
}

func TestCardCreateGetUpdate(t *testing.T) {
	s := New(20, 0)
	fields := testCardFields("alice")

	if err := s.CreateCard(500, 7, fields); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	card, ok := s.GetCard(500, 7)
	if !ok {
		t.Fatal("GetCard: card not found after create")
	}
	if card.Fields != fields {
		t.Fatalf("fields = %+v, want %+v", card.Fields, fields)
	}
	if card.CreatedAt.IsZero() || card.UpdatedAt.IsZero() {
		t.Fatal("timestamps must be set on create")
	}
	if !card.CreatedAt.Equal(card.UpdatedAt) {
		t.Fatalf("on create CreatedAt (%v) must equal UpdatedAt (%v)", card.CreatedAt, card.UpdatedAt)
	}

	merged := card.Fields
	merged.Title = "staff engineer"
	merged.Bio = ""
	if err := s.UpdateCard(500, 7, merged); err != nil {
		t.Fatalf("UpdateCard: %v", err)
	}
	card, _ = s.GetCard(500, 7)
	if card.Fields.Title != "staff engineer" || card.Fields.Name != "alice" {
		t.Fatalf("update did not apply: %+v", card.Fields)
	}
	if !card.UpdatedAt.After(card.CreatedAt) && card.UpdatedAt != card.CreatedAt {
		t.Fatalf("UpdatedAt (%v) must be >= CreatedAt (%v)", card.UpdatedAt, card.CreatedAt)
	}
}

func TestCardDuplicateCreateKeepsOriginal(t *testing.T) {
	s := New(20, 0)
	if err := s.CreateCard(500, 7, testCardFields("alice")); err != nil {
		t.Fatalf("first CreateCard: %v", err)
	}
	err := s.CreateCard(500, 7, testCardFields("mallory"))
	if !errors.Is(err, ErrCardExists) {
		t.Fatalf("second CreateCard err = %v, want ErrCardExists", err)
	}
	card, _ := s.GetCard(500, 7)
	if card.Fields.Name != "alice" {
		t.Fatalf("duplicate create overwrote the original card: %+v", card.Fields)
	}
}

func TestCardUpdateMissing(t *testing.T) {
	s := New(20, 0)
	err := s.UpdateCard(500, 7, testCardFields("alice"))
	if !errors.Is(err, ErrCardNotFound) {
		t.Fatalf("UpdateCard err = %v, want ErrCardNotFound", err)
	}
}

func TestCardGetMissing(t *testing.T) {
	s := New(20, 0)
	if _, ok := s.GetCard(500, 7); ok {
		t.Fatal("GetCard reported a card that was never created")
	}
}

// Cards are per chat: the same user may hold a card in two chats, and one
// chat's cards are invisible to another.
func TestCardScopedByChat(t *testing.T) {
	s := New(20, 0)
	if err := s.CreateCard(500, 7, testCardFields("alice")); err != nil {
		t.Fatalf("CreateCard chat 500: %v", err)
	}
	if _, ok := s.GetCard(600, 7); ok {
		t.Fatal("card from chat 500 leaked into chat 600")
	}
	if got := s.SearchCards(600, "alice"); len(got) != 0 {
		t.Fatalf("search in chat 600 found %d cards, want 0", len(got))
	}
	if err := s.CreateCard(600, 7, testCardFields("bob")); err != nil {
		t.Fatalf("CreateCard chat 600: %v", err)
	}
}

func TestCardSearchMatchesFieldsCaseInsensitive(t *testing.T) {
	s := New(20, 0)
	if err := s.CreateCard(500, 7, testCardFields("alice")); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	for _, kw := range []string{"alice", "ALICE", "engineer", "Python", "example", "builds"} {
		got := s.SearchCards(500, kw)
		if len(got) != 1 || got[0].Fields.Name != "alice" {
			t.Fatalf("SearchCards(%q) = %+v, want the alice card", kw, got)
		}
	}
	if got := s.SearchCards(500, "mallory"); len(got) != 0 {
		t.Fatalf("SearchCards(\"mallory\") = %+v, want no matches", got)
	}
	if got := s.SearchCards(500, ""); len(got) != 0 {
		t.Fatalf("empty keyword matched %d cards, want 0", len(got))
	}
}

func TestCardSearchSortedByUserID(t *testing.T) {
	s := New(20, 0)
	for _, id := range []int64{30, 10, 20} {
		if err := s.CreateCard(500, id, testCardFields("card"+string(rune('a'+id)))); err != nil {
			t.Fatalf("CreateCard %d: %v", id, err)
		}
	}
	got := s.SearchCards(500, "card")
	want := []int64{10, 20, 30}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d", len(got), len(want))
	}
	for i, c := range got {
		if c.UserID != want[i] {
			t.Fatalf("result %d: UserID = %d, want %d", i, c.UserID, want[i])
		}
	}
}

func TestCardPersistAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/cards.db"
	fs, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	s := New(20, 0)
	if err := s.AttachFileStore(fs, nil); err != nil {
		t.Fatalf("AttachFileStore: %v", err)
	}
	if err := s.CreateCard(500, 7, testCardFields("alice")); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fs, err = OpenFileStore(path)
	if err != nil {
		t.Fatalf("reopen OpenFileStore: %v", err)
	}
	defer fs.Close()
	s2 := New(20, 0)
	if err := s2.AttachFileStore(fs, nil); err != nil {
		t.Fatalf("reattach: %v", err)
	}
	card, ok := s2.GetCard(500, 7)
	if !ok {
		t.Fatal("card did not survive reopen")
	}
	if card.Fields.Name != "alice" || card.Fields.Bio != "builds things" {
		t.Fatalf("card after reopen = %+v, want the persisted fields", card.Fields)
	}
	if card.CreatedAt.IsZero() || card.UpdatedAt.IsZero() {
		t.Fatal("timestamps must be restored from the database")
	}
	if got := s2.SearchCards(500, "alice"); len(got) != 1 {
		t.Fatalf("search after reopen found %d cards, want 1", len(got))
	}
}

func TestCardTimestampsUseNowFunc(t *testing.T) {
	s := New(20, 0)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return base }
	if err := s.CreateCard(500, 7, testCardFields("alice")); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	later := base.Add(time.Hour)
	s.nowFunc = func() time.Time { return later }
	f := testCardFields("alice")
	f.Title = "changed"
	if err := s.UpdateCard(500, 7, f); err != nil {
		t.Fatalf("UpdateCard: %v", err)
	}
	card, _ := s.GetCard(500, 7)
	if !card.CreatedAt.Equal(base) {
		t.Fatalf("CreatedAt = %v, want %v", card.CreatedAt, base)
	}
	if !card.UpdatedAt.Equal(later) {
		t.Fatalf("UpdatedAt = %v, want %v", card.UpdatedAt, later)
	}
}
