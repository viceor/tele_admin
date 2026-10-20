package bot

import (
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// lastSentText returns the text of the most recently sent message.
func lastSentText(mock *mockTelegram) string {
	msgs := mock.sent()
	return msgs[len(msgs)-1]["text"].(string)
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// cardUsers are the actors used by the card tests.
const (
	cardOwner  int64 = 100 // owns the card created in the success paths
	cardOther  int64 = 200 // never owns a card in chat 500
	cardHunter int64 = 300 // tries to edit another member's card
)

func newCardTestBot(t *testing.T) (*Bot, *mockTelegram) {
	t.Helper()
	b, mock := newMemberTestBot(t, true, nil)
	return b, mock
}

// --- create ----------------------------------------------------------------

func TestCardCreateAndShowOwnCard(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice title=Senior Engineer skills=Go, Python contact=alice@example.com bio=builds things"))

	waitForSent(t, mock, "Card created")
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card show"))

	waitForSent(t, mock, "📇 Alice")
	for _, want := range []string{"Title: Senior Engineer", "Skills: Go, Python", "Contact: alice@example.com", "Bio: builds things"} {
		if !strings.Contains(lastSentText(mock), want) {
			t.Fatalf("card display %q missing %q", lastSentText(mock), want)
		}
	}
}

func TestCardCreateRejectsDuplicate(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice"))
	waitForSent(t, mock, "Card created")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Mallory title=impostor"))

	waitForSent(t, mock, "already have a card")
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card show"))
	waitForSent(t, mock, "📇 Alice")
	if strings.Contains(lastSentText(mock), "Mallory") {
		t.Fatal("duplicate create must not overwrite the existing card")
	}
}

func TestCardCreateRequiresName(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create title=engineer"))

	waitForSent(t, mock, "name")
	waitForSent(t, mock, "must not be empty")
}

func TestCardCreateRejectsEmptyFieldValue(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name= title=ok"))

	waitForSent(t, mock, "must not be empty")
}

func TestCardCreateRejectsLongName(t *testing.T) {
	b, mock := newCardTestBot(t)

	long := strings.Repeat("a", 65)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name="+long))

	waitForSent(t, mock, "too long")
	waitForSent(t, mock, "64")
	if _, ok := b.store.GetCard(500, cardOwner); ok {
		t.Fatal("a rejected create must not store a card")
	}
}

func TestCardCreateRejectsUnknownField(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice nickname=ali"))

	waitForSent(t, mock, "Unknown field")
}

func TestCardCreateRejectsStrayToken(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create justtext name=Alice"))

	waitForSent(t, mock, "Unexpected text")
}

func TestCardCreateValuesMaySpanTokens(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice title=Senior Engineer"))

	waitForSent(t, mock, "Card created")
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card show"))
	waitForSent(t, mock, "Title: Senior Engineer")
}

func TestCardCreateWithoutArgsShowsUsage(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create"))

	waitForSent(t, mock, "Card commands")
}

func TestCardCreateRejectedForAnonymousSender(t *testing.T) {
	b, mock := newCardTestBot(t)
	upd := toJSON(map[string]any{
		"update_id": 9,
		"message": map[string]any{
			"message_id": 10,
			"chat":       map[string]any{"id": 500, "title": "group"},
			"text":       "/card create name=Anon",
			"entities":   []map[string]any{{"type": "bot_command", "offset": 0, "length": 5}},
		},
	})

	postUpdate(t, b.Client().WebhookHandler(), upd)

	waitForSent(t, mock, "must be identified")
	if _, ok := b.store.GetCard(500, 0); ok {
		t.Fatal("anonymous create must not store a card")
	}
}

// --- show ------------------------------------------------------------------

func TestCardShowOtherMemberCard(t *testing.T) {
	b, mock := newCardTestBot(t)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice"))
	waitForSent(t, mock, "Card created")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card show "+itoa(cardOwner)))

	waitForSent(t, mock, "📇 Alice")
}

func TestCardShowNotFound(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card show"))

	waitForSent(t, mock, "No card found")
}

func TestCardShowInvalidTargetShowsUsage(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card show @alice"))

	waitForSent(t, mock, "Card commands")
}

// --- search ----------------------------------------------------------------

func TestCardSearchFindsByKeyword(t *testing.T) {
	b, mock := newCardTestBot(t)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice bio=golang enthusiast"))
	waitForSent(t, mock, "Card created")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card search golang"))

	waitForSent(t, mock, "📇 Alice")
	waitForSent(t, mock, "Bio: golang enthusiast")
}

func TestCardSearchCaseInsensitive(t *testing.T) {
	b, mock := newCardTestBot(t)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice title=Engineer"))
	waitForSent(t, mock, "Card created")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card search ENGI"))

	waitForSent(t, mock, "📇 Alice")
}

func TestCardSearchNoMatch(t *testing.T) {
	b, mock := newCardTestBot(t)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice"))
	waitForSent(t, mock, "Card created")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card search zzzz"))

	waitForSent(t, mock, `No cards match "zzzz"`)
}

func TestCardSearchRequiresKeyword(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card search"))

	waitForSent(t, mock, "Card commands")
}

// TestCardSearchLongResultSetSplitsMessages pins the MYC-62 P2 guarantee on
// a result whose joined text exceeds the Telegram message limit: the answer
// is split into several messages, every one of them at or below the limit
// while still containing whole cards.
func TestCardSearchLongResultSetSplitsMessages(t *testing.T) {
	b, mock := newCardTestBot(t)
	const n = 30
	bio := strings.Repeat("b", 300) // each card lands near 320 runes
	for i := 0; i < n; i++ {
		id := int64(1000 + i)
		postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, id, "/card create name=Alice"+itoa(id)+" bio="+bio))
	}
	waitFor(t, 2*time.Second, func() bool { return countSent(mock, "Card created") == n })

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card search alice"))

	// The pages go out back-to-back, so a snapshot taken after the first two
	// of them can miss the rest of the result set (MYC-94/MYC-96 flake: the
	// count then read 24 of 30 cards). Wait until every created card has been
	// observed across the sent pages before the counting pass below.
	waitFor(t, 2*time.Second, func() bool {
		total := 0
		for _, m := range mock.sent() {
			total += strings.Count(m["text"].(string), "📇 Alice")
		}
		return total >= n
	})
	pages, cards := 0, 0
	for _, m := range mock.sent() {
		text := m["text"].(string)
		if !strings.Contains(text, "📇 Alice") {
			continue
		}
		pages++
		if got := utf8.RuneCountInString(text); got > cardSearchMsgMax {
			t.Fatalf("search reply is %d runes, above the %d limit: %q...", got, cardSearchMsgMax, text[:24])
		}
		cards += strings.Count(text, "📇 Alice")
	}
	if pages < 2 {
		t.Fatalf("expected the long result set to split into multiple messages, got %d", pages)
	}
	if cards != n {
		t.Fatalf("expected all %d cards to be delivered, got %d", n, cards)
	}
}

// TestCardSearchTooManyResultsShowsNotice pins the result cap and the
// "there are more results, please narrow your keyword" notice: matches
// beyond the cap are not sent, and exactly one notice states the total.
func TestCardSearchTooManyResultsShowsNotice(t *testing.T) {
	b, mock := newCardTestBot(t)
	total := cardSearchResultLimit + 3
	for i := 0; i < total; i++ {
		id := int64(2000 + i)
		postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, id, "/card create name=Alice"+itoa(id)))
	}
	waitFor(t, 2*time.Second, func() bool { return countSent(mock, "Card created") == total })

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card search alice"))

	waitForSent(t, mock, "Showing 50 of 53")
	shown := 0
	for _, m := range mock.sent() {
		text := m["text"].(string)
		if n := strings.Count(text, "📇 Alice"); n > 0 {
			shown += n
			if got := utf8.RuneCountInString(text); got > cardSearchMsgMax {
				t.Fatalf("search reply is %d runes, above the %d limit", got, cardSearchMsgMax)
			}
		}
	}
	if shown != cardSearchResultLimit {
		t.Fatalf("expected exactly %d shown cards, got %d", cardSearchResultLimit, shown)
	}
	if n := countSent(mock, "Showing 50 of 53 matches; there are more results, please narrow your keyword."); n != 1 {
		t.Fatalf("expected exactly one more-results notice, got %d", n)
	}
	if n := countSent(mock, "📇 Alice"+itoa(int64(2000+cardSearchResultLimit))); n != 0 {
		t.Fatal("cards beyond the result limit must not be sent")
	}
}

// TestCardSearchTooManyResultsNoticeInChinese checks the zh catalog string
// for the more-results notice against a capped result set.
func TestCardSearchTooManyResultsNoticeInChinese(t *testing.T) {
	b, mock := newCardTestBot(t)
	b.store.SetLanguage(500, "zh")
	total := cardSearchResultLimit + 1
	for i := 0; i < total; i++ {
		id := int64(3000 + i)
		postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, id, "/card create name=Alice"+itoa(id)))
	}
	waitFor(t, 2*time.Second, func() bool { return countSent(mock, "名片已创建") == total })

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card search alice"))

	waitForSent(t, mock, "还有更多结果，请缩小关键字")
	if n := countSent(mock, "已展示 50 条，共 51 条匹配；还有更多结果，请缩小关键字。"); n != 1 {
		t.Fatalf("expected exactly one zh more-results notice, got %d", n)
	}
}

// --- search packing unit tests ---------------------------------------------

// TestPackCardSearchPagesSinglePageMatchesJoin keeps the happy path identical
// to the previous extraction: one fitting page is exactly the joined text.
func TestPackCardSearchPagesSinglePageMatchesJoin(t *testing.T) {
	rendered := []string{"card one", "card two"}
	pages := packCardSearchPages(rendered, cardSearchMsgMax, "")
	got := strings.Join(pages, "<and>")
	if got != "card one\n\ncard two" {
		t.Fatalf("single-page pack = %q, want the plain join", got)
	}
}

// TestPackCardSearchPagesUnderRuneBudget packs a set that would overflow one
// message and pins every page, including the multibyte notice reserved on the
// last page, at or below the rune limit.
func TestPackCardSearchPagesUnderRuneBudget(t *testing.T) {
	tail := "还有更多结果，请缩小关键字。"
	rendered := []string{strings.Repeat("名", 3000), strings.Repeat("乙", 3000), strings.Repeat("丙", 3000)}
	pages := packCardSearchPages(rendered, cardSearchMsgMax, tail)
	if len(pages) < 2 {
		t.Fatalf("expected multiple pages, got %d", len(pages))
	}
	for i, p := range pages {
		if n := utf8.RuneCountInString(p); n > cardSearchMsgMax {
			t.Fatalf("page %d is %d runes, above the %d limit", i, n, cardSearchMsgMax)
		}
	}
	if !strings.HasSuffix(pages[len(pages)-1], tail) {
		t.Fatalf("last page must end with the notice: %q...", pages[len(pages)-1][len(pages[len(pages)-1])-80:])
	}
}

// TestTruncateRunesRuneBoundary pins truncation in runes, not bytes, and the
// ellipsis suffix on a cut.
func TestTruncateRunesRuneBoundary(t *testing.T) {
	long := strings.Repeat("名", 300)
	if got := truncateRunes(long, 100); utf8.RuneCountInString(got) != 100 || !strings.HasSuffix(got, string([]rune{'\u2026'})) {
		t.Fatalf("truncateRunes did not cut to 100 runes with an ellipsis: %d runes", utf8.RuneCountInString(got))
	}
	if got := truncateRunes(long, 300); got != long {
		t.Fatal("strings at or under the limit must not be touched")
	}
}

// --- edit ------------------------------------------------------------------

func TestCardEditOwnCardPartialUpdate(t *testing.T) {
	b, mock := newCardTestBot(t)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice title=Engineer bio=hello"))
	waitForSent(t, mock, "Card created")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card edit title=Staff Engineer"))

	waitForSent(t, mock, "Card updated")
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card show"))
	waitForSent(t, mock, "Title: Staff Engineer")
	if !strings.Contains(lastSentText(mock), "📇 Alice") || !strings.Contains(lastSentText(mock), "Bio: hello") {
		t.Fatalf("partial edit must keep unmentioned fields: %q", lastSentText(mock))
	}
}

func TestCardEditByNonOwnerRefused(t *testing.T) {
	b, mock := newCardTestBot(t)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice title=Engineer"))
	waitForSent(t, mock, "Card created")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardHunter, "/card edit "+itoa(cardOwner)+" name=Mallory"))

	waitForSent(t, mock, "Only the card owner can edit it")
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card show"))
	waitForSent(t, mock, "📇 Alice")
	if strings.Contains(lastSentText(mock), "Mallory") {
		t.Fatal("a refused edit must not modify the card")
	}
}

func TestCardEditByNonOwnerRefusedWithoutArgs(t *testing.T) {
	b, mock := newCardTestBot(t)
	// cardHunter has no card; editing "their own" card reports not found.
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardHunter, "/card edit title=x"))

	waitForSent(t, mock, "No card found")
}

func TestCardEditNonExistentCard(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card edit title=x"))

	waitForSent(t, mock, "No card found")
}

func TestCardEditRejectsInvalidField(t *testing.T) {
	b, mock := newCardTestBot(t)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice"))
	waitForSent(t, mock, "Card created")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card edit nickname=ali"))

	waitForSent(t, mock, "Unknown field")
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card show"))
	waitForSent(t, mock, "📇 Alice")
}

func TestCardEditAnonymousRejected(t *testing.T) {
	b, mock := newCardTestBot(t)
	upd := toJSON(map[string]any{
		"update_id": 10,
		"message": map[string]any{
			"message_id": 10,
			"chat":       map[string]any{"id": 500, "title": "group"},
			"text":       "/card edit name=Anon",
			"entities":   []map[string]any{{"type": "bot_command", "offset": 0, "length": 5}},
		},
	})

	postUpdate(t, b.Client().WebhookHandler(), upd)

	waitForSent(t, mock, "must be identified")
}

// --- misc ------------------------------------------------------------------

func TestCardBareCommandShowsUsage(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card"))

	waitForSent(t, mock, "Card commands")
}

func TestCardUnknownSubcommandShowsUsage(t *testing.T) {
	b, mock := newCardTestBot(t)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card frobnicate"))

	waitForSent(t, mock, "Card commands")
}

// Card replies follow the chat language, so a zh chat gets zh messages.
func TestCardRepliesInChineseWhenChatIsChinese(t *testing.T) {
	b, mock := newCardTestBot(t)
	b.store.SetLanguage(500, "zh")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice"))

	waitForSent(t, mock, "名片已创建")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOther, "/card search zzzz"))
	waitForSent(t, mock, "没有匹配")

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardHunter, "/card edit "+itoa(cardOwner)+" name=X"))
	waitForSent(t, mock, "只有名片所有者才能编辑")
}

// End-to-end persistence (P0 integration): a card written to db.path survives
// closing and reopening the bot on the same database file.
func TestCardPersistsAcrossRestart(t *testing.T) {
	mock := newMockTelegram(t)
	cfg := testConfig("")
	cfg.DB.Path = t.TempDir() + "/cards.db"

	b := startTestBot(t, cfg, mock)
	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card create name=Alice title=Engineer"))
	waitForSent(t, mock, "Card created")

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b2 := startTestBot(t, cfg, mock)
	defer b2.Close()
	postUpdate(t, b2.Client().WebhookHandler(), textMessageUpdate(500, cardOwner, "/card show"))
	waitForSent(t, mock, "📇 Alice")
	waitForSent(t, mock, "Title: Engineer")
}
