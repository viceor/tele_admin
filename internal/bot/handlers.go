package bot

import (
	"context"
	"strings"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/viceor/tele-admin/internal/i18n"
)

func (b *Bot) send(ctx context.Context, chatID int64, text string) error {
	if _, err := b.client.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: chatID, Text: text}); err != nil {
		b.log.Error("sendMessage failed", "chat", chatID, "err", err)
		return err
	}
	return nil
}

func (b *Bot) chatLang(chatID int64) i18n.Language {
	return b.store.Language(chatID)
}

// messageSender returns the update's sender, or nil when the message has no
// identifiable sender (for example anonymous posts made on behalf of a chat,
// where Telegram omits Message.From).
func messageSender(upd *models.Update) *models.User {
	if upd.Message == nil {
		return nil
	}
	return upd.Message.From
}

// requireAdmin sends the not-admin notice and returns false when the caller
// is not permitted to run an admin-only command. A nil sender (anonymous
// message) is always rejected: admin permission cannot be verified without
// an identifiable user.
func (b *Bot) requireAdmin(ctx context.Context, chatID int64, sender *models.User) bool {
	if sender == nil {
		b.send(ctx, chatID, i18n.T(b.chatLang(chatID), i18n.KeyNotAdmin, nil))
		return false
	}
	ok, err := b.admin.IsAdmin(ctx, chatID, sender.ID)
	if err != nil {
		b.log.Error("admin check failed", "chat", chatID, "user", sender.ID, "err", err)
		return false
	}
	if !ok {
		b.send(ctx, chatID, i18n.T(b.chatLang(chatID), i18n.KeyNotAdmin, nil))
		return false
	}
	return true
}

func (b *Bot) handleStart(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	b.send(ctx, upd.Message.Chat.ID, i18n.T(b.chatLang(upd.Message.Chat.ID), i18n.KeyHelp, nil))
}

func (b *Bot) handleHelp(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	b.send(ctx, upd.Message.Chat.ID, i18n.T(b.chatLang(upd.Message.Chat.ID), i18n.KeyHelp, nil))
}

func (b *Bot) handleWelcomeCmd(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	if !b.requireAdmin(ctx, upd.Message.Chat.ID, messageSender(upd)) {
		return
	}
	b.sendWelcome(ctx, upd.Message.Chat.ID, nil)
}

func (b *Bot) handleLang(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	if !b.requireAdmin(ctx, upd.Message.Chat.ID, messageSender(upd)) {
		return
	}
	fields := strings.Fields(upd.Message.Text)
	if len(fields) < 2 {
		b.send(ctx, upd.Message.Chat.ID, "usage: /lang <en|zh>")
		return
	}
	lang := i18n.Normalize(fields[1])
	if !b.cfg.I18N.HasLanguage(string(lang)) {
		b.send(ctx, upd.Message.Chat.ID, "unsupported language; supported: "+strings.Join(b.cfg.I18N.Supported, ", "))
		return
	}
	// Confirmation only after the change is persisted: a write-through
	// failure is reported as "not saved" instead of a success (MYC-73).
	if err := b.store.SetLanguage(upd.Message.Chat.ID, lang); err != nil {
		b.log.Error("set language not persisted", "chat", upd.Message.Chat.ID, "err", err)
		b.send(ctx, upd.Message.Chat.ID, i18n.T(b.chatLang(upd.Message.Chat.ID), i18n.KeyPersistFailed, nil))
		return
	}
	b.send(ctx, upd.Message.Chat.ID, i18n.T(lang, i18n.KeyLanguageSet, nil))
}

func (b *Bot) handleTimezone(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	if !b.requireAdmin(ctx, upd.Message.Chat.ID, messageSender(upd)) {
		return
	}
	fields := strings.Fields(upd.Message.Text)
	if len(fields) < 2 {
		b.send(ctx, upd.Message.Chat.ID, "usage: /tz <IANA-timezone>")
		return
	}
	tz := fields[1]
	if _, err := time.LoadLocation(tz); err != nil {
		b.send(ctx, upd.Message.Chat.ID, "invalid IANA timezone: "+tz)
		return
	}
	if err := b.store.SetTimezone(upd.Message.Chat.ID, tz); err != nil {
		b.log.Error("set timezone not persisted", "chat", upd.Message.Chat.ID, "err", err)
		b.send(ctx, upd.Message.Chat.ID, i18n.T(b.chatLang(upd.Message.Chat.ID), i18n.KeyPersistFailed, nil))
		return
	}
	b.send(ctx, upd.Message.Chat.ID, i18n.T(b.chatLang(upd.Message.Chat.ID), i18n.KeyTimezoneSet, map[string]string{"tz": tz}))
}

// handleGovernance is the default handler: it welcomes new members, enforces
// the keyword blocklist, and applies the anti-spam rule chain.
func (b *Bot) handleGovernance(ctx context.Context, client *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	chatID := upd.Message.Chat.ID

	for _, m := range upd.Message.NewChatMembers {
		if m.IsBot {
			continue
		}
		// An applied profile overrides the global welcome config per chat.
		welcomeEnabled, _ := b.welcomeConfigFor(chatID)
		if !welcomeEnabled {
			continue
		}
		// Reserve the send atomically so concurrent duplicate join events do
		// not all greet the member, then mark the member welcomed only after
		// Telegram confirms the send. A failed send releases the reservation
		// so a later event can retry.
		if !b.store.BeginWelcome(chatID, m.ID) {
			continue
		}
		if b.sendWelcome(ctx, chatID, &m) {
			if err := b.store.CompleteWelcome(chatID, m.ID); err != nil {
				// The greeting is already out; nothing can unsend it. The
				// welcomed flag is rolled back in memory, so a later join
				// event may re-greet the member after a restart rather than
				// serve state the disk does not have.
				b.log.Error("welcome delivered but not persisted", "chat", chatID, "user", m.ID, "err", err)
			}
		} else {
			b.store.AbortWelcome(chatID, m.ID)
		}
	}

	text := upd.Message.Text
	if text == "" {
		return
	}
	// An applied profile overrides the global keyword blocklist per chat.
	if blocked, _ := b.rulesForChat(chatID).Check(text); blocked {
		if _, err := client.DeleteMessage(ctx, &tgbot.DeleteMessageParams{ChatID: chatID, MessageID: upd.Message.ID}); err != nil {
			b.log.Error("deleteMessage failed", "chat", chatID, "msg", upd.Message.ID, "err", err)
		}
		b.send(ctx, chatID, i18n.T(b.chatLang(chatID), i18n.KeyBlockedKeyword, nil))
		return
	}
	if upd.Message.From != nil {
		if allowed, _ := b.spam.Check(upd.Message.From.ID, time.Now()); !allowed {
			b.send(ctx, chatID, i18n.T(b.chatLang(chatID), i18n.KeySpamWarning, map[string]string{"name": upd.Message.From.FirstName}))
		}
	}
}

// sendWelcome sends the configured or default greeting and reports whether
// Telegram accepted it. Callers that mark a member as welcomed must only do so
// when it returns true.
func (b *Bot) sendWelcome(ctx context.Context, chatID int64, m *models.User) bool {
	enabled, msg := b.welcomeConfigFor(chatID)
	if !enabled {
		return false
	}
	if msg == "" {
		return b.send(ctx, chatID, i18n.T(b.chatLang(chatID), i18n.KeyWelcomeNewMember, map[string]string{"name": chatLabel(m)})) == nil
	}
	return b.send(ctx, chatID, strings.ReplaceAll(msg, "{name}", chatLabel(m))) == nil
}

func chatLabel(m *models.User) string {
	if m == nil {
		return "new member"
	}
	if m.Username != "" {
		return m.Username
	}
	return m.FirstName
}
