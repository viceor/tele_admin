package bot

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/viceor/tele-admin/internal/i18n"
	"github.com/viceor/tele-admin/internal/store"
)

// AI business cards (slice S3).
//
// A card belongs to one (chat, user) pair and is owned by that user. Any
// group member may create a card, show cards, and search cards; editing a
// card is restricted to its owner, and a non-owner edit attempt is refused
// with an explicit message. Field values are user-authored: this slice never
// calls an external model service to generate card content.
//
// Commands:
//
//	/card create <key=value>...   create own card (name required)
//	/card show [user_id]          show own card, or another member's by ID
//	/card search <keyword>        search this group's cards by keyword
//	/card edit [user_id] <key=value>...   edit a card (owner only)
//
// Key=value parsing: a token that starts a new field is "<known-key>=";
// any other token is appended to the current field's value, so values may
// contain spaces ("bio=hello world" or "bio=hello" "world").
//
// Accepted fields and length limits (runes):
//
//	name     1-64    required at create
//	title    0-128
//	skills   0-256
//	contact  0-128
//	bio      0-512
//
// Search replies never exceed the Telegram message length limit: at most
// cardSearchResultLimit cards are returned per search and the rendered set
// is split into pages that each stay under cardSearchMsgMax runes, with a
// "there are more results, narrow your keyword" notice when further matches
// exist (MYC-62 P2).
const (
	cardNameMax    = 64
	cardTitleMax   = 128
	cardSkillsMax  = 256
	cardContactMax = 128
	cardBioMax     = 512
)

// cardFieldLimit maps each accepted card field to its rune length limit.
var cardFieldLimit = map[string]int{
	"name":    cardNameMax,
	"title":   cardTitleMax,
	"skills":  cardSkillsMax,
	"contact": cardContactMax,
	"bio":     cardBioMax,
}

// Search reply limits (MYC-62 P2). Runes are the character unit matching how
// Telegram counts message text; every page stays at or below the limit.
const (
	// cardSearchMsgMax is the Bot API send-message text limit (4096
	// characters).
	cardSearchMsgMax = 4096
	// cardSearchResultLimit caps how many cards one search renders. Matches
	// beyond it are held back and a notice names the total, so an oversized
	// result set can never produce unbounded message traffic.
	cardSearchResultLimit = 50
	// cardPageSepRunes is the rendered length of the separator "\n\n" placed
	// between two cards on the same page.
	cardPageSepRunes = 2
)

// cardArgError describes why card arguments were rejected. kind selects the
// user-facing message; key carries the offending field or token.
type cardArgError struct {
	kind string // "unknown", "empty", "too_long", "stray", "missing"
	key  string
}

func (e *cardArgError) Error() string { return "invalid card arguments: " + e.kind + " " + e.key }

// parseCardFields converts command tokens after the subcommand into card
// fields. Tokens must be <key=value> pairs for known fields; a token without
// a recognized key continues the previous field's value. The first token must
// open a field, so a leading stray token is an error.
func parseCardFields(tokens []string) (store.CardFields, *cardArgError) {
	var fields store.CardFields
	set := func(key, value string) *cardArgError {
		switch key {
		case "name":
			fields.Name = value
		case "title":
			fields.Title = value
		case "skills":
			fields.Skills = value
		case "contact":
			fields.Contact = value
		case "bio":
			fields.Bio = value
		default:
			return &cardArgError{kind: "unknown", key: key}
		}
		if value == "" {
			return &cardArgError{kind: "empty", key: key}
		}
		if limit := cardFieldLimit[key]; limit > 0 && len([]rune(value)) > limit {
			return &cardArgError{kind: "too_long", key: key}
		}
		return nil
	}
	current := ""
	for _, tok := range tokens {
		if key, value, found := strings.Cut(tok, "="); found {
			if _, isField := cardFieldLimit[key]; isField {
				if err := set(key, value); err != nil {
					return store.CardFields{}, err
				}
				current = key
				continue
			}
			// "key=" with an unknown key is reported as unknown, not stray:
			// the user clearly meant a field.
			if key != "" {
				return store.CardFields{}, &cardArgError{kind: "unknown", key: key}
			}
			// A bare "=value" token continues the current field.
		}
		if current == "" {
			return store.CardFields{}, &cardArgError{kind: "stray", key: tok}
		}
		if err := set(current, joinCardValue(fieldValue(fields, current), tok)); err != nil {
			return store.CardFields{}, err
		}
	}
	return fields, nil
}

// joinCardValue appends a continued token to a field value with one space.
func joinCardValue(prev, tok string) string {
	if prev == "" {
		return tok
	}
	return prev + " " + tok
}

// fieldValue reads one parsed field for continuation joins.
func fieldValue(f store.CardFields, key string) string {
	switch key {
	case "name":
		return f.Name
	case "title":
		return f.Title
	case "skills":
		return f.Skills
	case "contact":
		return f.Contact
	case "bio":
		return f.Bio
	}
	return ""
}

// handleCardCmd dispatches the /card command family.
func (b *Bot) handleCardCmd(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	fields := strings.Fields(upd.Message.Text)
	if len(fields) < 2 {
		b.send(ctx, upd.Message.Chat.ID, i18n.T(b.chatLang(upd.Message.Chat.ID), i18n.KeyCardUsage, nil))
		return
	}
	chatID := upd.Message.Chat.ID
	lang := b.chatLang(chatID)
	switch fields[1] {
	case "create":
		b.handleCardCreate(ctx, chatID, lang, upd, fields[2:])
	case "show":
		b.handleCardShow(ctx, chatID, lang, upd, fields[2:])
	case "search":
		b.handleCardSearch(ctx, chatID, lang, upd)
	case "edit":
		b.handleCardEdit(ctx, chatID, lang, upd, fields[2:])
	default:
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardUsage, nil))
	}
}

// cardUsage sends the card command usage text.
func (b *Bot) cardUsage(ctx context.Context, chatID int64, lang i18n.Language) {
	b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardUsage, nil))
}

// sendCardArgError maps a parse/validation failure to its message.
func (b *Bot) sendCardArgError(ctx context.Context, chatID int64, lang i18n.Language, err *cardArgError) {
	var key i18n.MessageKey
	args := map[string]string{"field": err.key}
	switch err.kind {
	case "unknown":
		key = i18n.KeyCardUnknownField
	case "empty":
		key = i18n.KeyCardFieldEmpty
	case "too_long":
		key = i18n.KeyCardFieldTooLong
		args["max"] = strconv.Itoa(cardFieldLimit[err.key])
	case "stray":
		key = i18n.KeyCardStrayToken
	case "missing":
		key = i18n.KeyCardFieldEmpty
	default:
		key = i18n.KeyCardUsage
	}
	b.send(ctx, chatID, i18n.T(lang, key, args))
}

func (b *Bot) handleCardCreate(ctx context.Context, chatID int64, lang i18n.Language, upd *models.Update, tokens []string) {
	sender := messageSender(upd)
	if sender == nil {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardAnonymous, nil))
		return
	}
	if len(tokens) == 0 {
		b.cardUsage(ctx, chatID, lang)
		return
	}
	fields, argErr := parseCardFields(tokens)
	if argErr != nil {
		b.sendCardArgError(ctx, chatID, lang, argErr)
		return
	}
	if fields.Name == "" {
		b.sendCardArgError(ctx, chatID, lang, &cardArgError{kind: "empty", key: "name"})
		return
	}
	if err := b.store.CreateCard(chatID, sender.ID, fields); err != nil {
		if errors.Is(err, store.ErrCardExists) {
			b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardExists, nil))
			return
		}
		b.log.Error("create card failed", "chat", chatID, "user", sender.ID, "err", err)
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardFailed, nil))
		return
	}
	b.log.Info("card created", "chat", chatID, "user", sender.ID)
	b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardCreated, nil))
}

// cardTarget resolves the user whose card a show command addresses: an
// explicit numeric argument, or the sender when omitted. A non-numeric
// argument is rejected: usernames cannot be resolved and acting on a guess
// would be worse than asking for an ID.
func cardTarget(upd *models.Update, args []string) (int64, bool) {
	if len(args) >= 1 {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id <= 0 {
			return 0, false
		}
		return id, true
	}
	if sender := messageSender(upd); sender != nil {
		return sender.ID, true
	}
	return 0, false
}

// cardEditTarget resolves the edit target and the remaining field tokens. A
// numeric first argument addresses that member's card and is consumed as the
// target; without one the sender's own card is addressed and every argument
// is a field token.
func cardEditTarget(upd *models.Update, args []string) (int64, []string, bool) {
	if len(args) >= 1 {
		if id, err := strconv.ParseInt(args[0], 10, 64); err == nil && id > 0 {
			return id, args[1:], true
		}
		if sender := messageSender(upd); sender != nil {
			return sender.ID, args, true
		}
		return 0, nil, false
	}
	if sender := messageSender(upd); sender != nil {
		return sender.ID, nil, true
	}
	return 0, nil, false
}

func (b *Bot) handleCardShow(ctx context.Context, chatID int64, lang i18n.Language, upd *models.Update, args []string) {
	if messageSender(upd) == nil {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardAnonymous, nil))
		return
	}
	target, ok := cardTarget(upd, args)
	if !ok {
		b.cardUsage(ctx, chatID, lang)
		return
	}
	card, ok := b.store.GetCard(chatID, target)
	if !ok {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardNotFound, nil))
		return
	}
	b.send(ctx, chatID, formatCard(card))
}

// handleCardSearch searches this chat's cards by keyword. An empty keyword is
// a usage error; no matches get an explicit not-found message. Results are
// capped at cardSearchResultLimit and packed into messages that never exceed
// cardSearchMsgMax runes; when further matches exist, the last message ends
// with a notice that names the total and asks the user to narrow the keyword.
func (b *Bot) handleCardSearch(ctx context.Context, chatID int64, lang i18n.Language, upd *models.Update) {
	if messageSender(upd) == nil {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardAnonymous, nil))
		return
	}
	text := upd.Message.Text
	keyword := strings.TrimSpace(strings.TrimPrefix(text, "/card"))
	keyword = strings.TrimSpace(strings.TrimPrefix(keyword, "search"))
	if keyword == "" {
		b.cardUsage(ctx, chatID, lang)
		return
	}
	results := b.store.SearchCards(chatID, keyword)
	if len(results) == 0 {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardSearchNone, map[string]string{"kw": keyword}))
		return
	}
	shown := results
	if len(shown) > cardSearchResultLimit {
		shown = shown[:cardSearchResultLimit]
	}
	rendered := make([]string, 0, len(shown))
	for _, c := range shown {
		rendered = append(rendered, formatCard(c))
	}
	tail := ""
	if len(shown) < len(results) {
		tail = i18n.T(lang, i18n.KeyCardSearchMore, map[string]string{
			"shown": strconv.Itoa(len(shown)),
			"total": strconv.Itoa(len(results)),
		})
	}
	for _, page := range packCardSearchPages(rendered, cardSearchMsgMax, tail) {
		b.send(ctx, chatID, page)
	}
}

// packCardSearchPages joins rendered cards into reply pages. Every page stays
// at or below maxRunes runes, the character count Telegram applies to message
// text. A trailing notice is appended to the last page; its length is
// reserved again while packing, so the guarantee holds with or without the
// notice even for multi-page results. A single card longer than the page
// budget — impossible under the documented field limits, guarded for anyway —
// is truncated with an ellipsis so one result can never break the limit.
func packCardSearchPages(rendered []string, maxRunes int, tail string) []string {
	budget := maxRunes - cardPageSepRunes - utf8.RuneCountInString(tail)
	if budget < 1 {
		tail = truncateRunes(tail, maxRunes-cardPageSepRunes-1)
		budget = maxRunes - cardPageSepRunes - utf8.RuneCountInString(tail)
	}
	pages := make([]string, 0, 1)
	page := ""
	pageLen := 0
	flush := func() {
		if pageLen > 0 {
			pages = append(pages, page)
			page = ""
			pageLen = 0
		}
	}
	for _, card := range rendered {
		n := utf8.RuneCountInString(card)
		if n > budget {
			card = truncateRunes(card, budget)
			n = budget
		}
		if pageLen == 0 {
			page, pageLen = card, n
			continue
		}
		if pageLen+cardPageSepRunes+n > budget {
			flush()
			page, pageLen = card, n
			continue
		}
		page += "\n\n" + card
		pageLen += cardPageSepRunes + n
	}
	flush()
	if tail != "" && len(pages) > 0 {
		pages[len(pages)-1] += "\n\n" + tail
	}
	return pages
}

// truncateRunes cuts s to at most max runes, closing with an ellipsis when a
// cut happened.
func truncateRunes(s string, max int) string {
	if max < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// handleCardEdit edits a card. Without a user ID argument the caller edits
// their own card; with one, only the card owner may proceed. Provided fields
// replace the stored values, omitted fields are kept (partial update).
func (b *Bot) handleCardEdit(ctx context.Context, chatID int64, lang i18n.Language, upd *models.Update, args []string) {
	sender := messageSender(upd)
	if sender == nil {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardAnonymous, nil))
		return
	}
	target, tokens, ok := cardEditTarget(upd, args)
	if !ok {
		b.cardUsage(ctx, chatID, lang)
		return
	}
	card, ok := b.store.GetCard(chatID, target)
	if !ok {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardNotFound, nil))
		return
	}
	if card.UserID != sender.ID {
		b.log.Info("card edit refused: not owner", "chat", chatID, "actor", sender.ID, "owner", card.UserID)
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardForbidden, nil))
		return
	}
	if len(tokens) == 0 {
		b.cardUsage(ctx, chatID, lang)
		return
	}
	parsed, argErr := parseCardFields(tokens)
	if argErr != nil {
		b.sendCardArgError(ctx, chatID, lang, argErr)
		return
	}
	merged := card.Fields
	applyCardFields(&merged, parsed)
	if err := b.store.UpdateCard(chatID, target, merged); err != nil {
		b.log.Error("update card failed", "chat", chatID, "user", target, "err", err)
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardFailed, nil))
		return
	}
	b.log.Info("card updated", "chat", chatID, "user", target, "actor", sender.ID)
	b.send(ctx, chatID, i18n.T(lang, i18n.KeyCardUpdated, nil))
}

// applyCardFields overwrites the non-empty fields of dst with src's values,
// implementing partial update semantics for /card edit.
func applyCardFields(dst *store.CardFields, src store.CardFields) {
	if src.Name != "" {
		dst.Name = src.Name
	}
	if src.Title != "" {
		dst.Title = src.Title
	}
	if src.Skills != "" {
		dst.Skills = src.Skills
	}
	if src.Contact != "" {
		dst.Contact = src.Contact
	}
	if src.Bio != "" {
		dst.Bio = src.Bio
	}
}

// formatCard renders one card for display, skipping empty optional fields.
func formatCard(c store.Card) string {
	var sb strings.Builder
	sb.WriteString("📇 ")
	sb.WriteString(c.Fields.Name)
	for _, line := range []struct{ label, value string }{
		{"Title", c.Fields.Title},
		{"Skills", c.Fields.Skills},
		{"Contact", c.Fields.Contact},
		{"Bio", c.Fields.Bio},
	} {
		if line.value != "" {
			sb.WriteString("\n" + line.label + ": " + line.value)
		}
	}
	return sb.String()
}
