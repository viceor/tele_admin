package bot

// Independent verification tests for slice S3 (AI business cards), authored by
// the test engineer (MYC-61). These probe the acceptance boundaries listed in
// the S3 row of the roadmap: ownership checks, validation failure, not-found,
// and unauthorized (non-owner) edits, with emphasis on edge cases the feature
// tests in cards_test.go do not exercise (rune-vs-byte limits, every field
// limit, anonymous show/search, cross-chat isolation, owner edit by own ID).

import (
	"strings"
	"testing"
	"time"
)

// anonCardUpdate builds a card command message with no sender, mirroring a
// channel post Telegram delivers with Message.From unset.
func anonCardUpdate(text string) string {
	cmd, _ := commandToken(text)
	return toJSON(map[string]any{
		"update_id": 42,
		"message": map[string]any{
			"message_id": 10,
			"chat":       map[string]any{"id": 500, "title": "group"},
			"text":       text,
			"entities":   []map[string]any{{"type": "bot_command", "offset": 0, "length": len(cmd)}},
		},
	})
}

// TestVerifyCardNameRuneBoundary pins the limit to runes, not bytes: a 64-rune
// multibyte name is accepted and stored intact, and a 65-rune name is rejected
// without storing anything.
func TestVerifyCardNameRuneBoundary(t *testing.T) {
	b, mock := newCardTestBot(t)

	name64 := strings.Repeat("名", 64)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name="+name64))
	waitForSent(t, mock, "Card created")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card create name="+strings.Repeat("名", 65)))
	waitForSent(t, mock, "too long")
	if _, ok := b.store.GetCard(500, cardOther); ok {
		t.Fatal("65-rune name must not be stored")
	}

	card, ok := b.store.GetCard(500, cardOwner)
	if !ok || card.Fields.Name != name64 {
		t.Fatalf("64-rune multibyte name not stored intact: %+v", card.Fields)
	}
}

// TestVerifyCardFieldRuneLimits checks every optional field's documented limit:
// the exact limit is accepted and limit+1 is rejected without a store write.
func TestVerifyCardFieldRuneLimits(t *testing.T) {
	cases := []struct {
		key string
		max int
	}{
		{"title", 128},
		{"skills", 256},
		{"contact", 128},
		{"bio", 512},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			b, mock := newCardTestBot(t)

			atLimit := strings.Repeat("a", tc.max)
			postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice "+tc.key+"="+atLimit))
			waitForSent(t, mock, "Card created")

			overLimit := strings.Repeat("a", tc.max+1)
			postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card create name=Bob "+tc.key+"="+overLimit))
			waitForSent(t, mock, "too long")
			if _, ok := b.store.GetCard(500, cardOther); ok {
				t.Fatalf("%s over limit must not be stored", tc.key)
			}
		})
	}
}

// TestVerifyCardOwnerEditByExplicitOwnID is the positive ownership case for an
// explicit numeric target: naming your own ID is allowed.
func TestVerifyCardOwnerEditByExplicitOwnID(t *testing.T) {
	b, mock := newCardTestBot(t)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice title=Engineer"))
	waitForSent(t, mock, "Card created")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card edit "+itoa(cardOwner)+" title=Staff"))
	waitForSent(t, mock, "Card updated")

	card, _ := b.store.GetCard(500, cardOwner)
	if card.Fields.Title != "Staff" {
		t.Fatalf("owner edit by own ID did not apply: %+v", card.Fields)
	}
}

// TestVerifyCardNonOwnerWithOwnCardCannotEditOthers confirms the ownership
// check keys on the target's owner, not on whether the actor has a card.
func TestVerifyCardNonOwnerWithOwnCardCannotEditOthers(t *testing.T) {
	b, mock := newCardTestBot(t)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice"))
	waitForSent(t, mock, "Card created")
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardHunter, "/card create name=Hunter"))
	waitForSent(t, mock, "Card created")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardHunter, "/card edit "+itoa(cardOwner)+" name=Mallory"))
	waitForSent(t, mock, "Only the card owner can edit it")

	owner, _ := b.store.GetCard(500, cardOwner)
	hunter, _ := b.store.GetCard(500, cardHunter)
	if owner.Fields.Name != "Alice" || hunter.Fields.Name != "Hunter" {
		t.Fatalf("non-owner edit mutated cards: owner=%+v hunter=%+v", owner.Fields, hunter.Fields)
	}
}

// TestVerifyCardAnonymousShowSearchRejected covers the anonymous path for the
// read commands (the feature tests only cover anonymous create/edit).
func TestVerifyCardAnonymousShowSearchRejected(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), anonCardUpdate("/card show"))
	postUpdate(t, b.Client().WebhookHandler(), anonCardUpdate("/card search alice"))

	waitFor(t, 2*time.Second, func() bool { return countSent(mock, "must be identified") >= 2 })
}

// TestVerifyCardShowInvalidNumericTarget rejects non-positive IDs as usage
// errors rather than resolving them to a user.
func TestVerifyCardShowInvalidNumericTarget(t *testing.T) {
	b, mock := newCardTestBot(t)

	for _, arg := range []string{"0", "-5"} {
		postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card show "+arg))
	}
	waitFor(t, 2*time.Second, func() bool { return countSent(mock, "Card commands") >= 2 })
}

// TestVerifyCardCrossChatIsolation confirms cards never leak across chats.
func TestVerifyCardCrossChatIsolation(t *testing.T) {
	b, mock := newCardTestBot(t)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice"))
	waitForSent(t, mock, "Card created")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(600, cardOwner, "/card show"))
	waitForSent(t, mock, "No card found")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(600, cardOther, "/card search Alice"))
	waitForSent(t, mock, "No cards match")
}

// TestVerifyCardEditRejectsEmptyValue confirms an explicit empty field is a
// validation error and leaves the stored card untouched.
func TestVerifyCardEditRejectsEmptyValue(t *testing.T) {
	b, mock := newCardTestBot(t)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice bio=hello"))
	waitForSent(t, mock, "Card created")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card edit bio="))
	waitForSent(t, mock, "must not be empty")

	card, _ := b.store.GetCard(500, cardOwner)
	if card.Fields.Bio != "hello" {
		t.Fatalf("rejected empty edit changed the card: %+v", card.Fields)
	}
}
