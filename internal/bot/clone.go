package bot

import (
	"context"
	"errors"
	"strconv"
	"strings"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/viceor/tele-admin/internal/governance"
	"github.com/viceor/tele-admin/internal/i18n"
	"github.com/viceor/tele-admin/internal/store"
)

// Configuration, rule, and template cloning (slice S4).
//
// Commands:
//
//	/clone <source_chat_id>   copy the source chat's bot configuration and
//	                          rules into this chat
//	/template save <name>     capture this chat's configuration as a template
//	/template apply <name>    apply a template to this chat
//	/template export <name>   show a template as versioned JSON
//	/template import <name> <JSON>   import a versioned JSON template
//
// Authorization is fail-closed on both ends: the caller must be a bot owner,
// chat creator, or administrator of the target chat AND of the source chat.
// A failing permission lookup on either side aborts the clone.
//
// Template payloads are validated in the store before any write; an invalid
// template is refused with no partial state. Applying a profile writes the
// profile record and the chat settings record as one persisted transaction,
// so a commit failure rolls both back (see store.ApplyProfile).
//
// Templates are namespaced per chat (MYC-67, MYC-83): save, import, apply, and
// export resolve the name within the issuing chat only. Two chats may use the
// same name for different templates, and a save never reads or overwrites
// another chat's template. Apply and export report a name outside the issuing
// chat's namespace as not found. Exports are audit-logged.
//
// Export uses the versioned bundle format store.TemplateBundle; an import
// whose bundle declares an unknown format version is rejected.

// chatProfile returns the chat's clonable profile content. Language and
// timezone always come from the chat's current effective settings: they live
// in the chat settings record and can change at any time via /lang and /tz
// after a profile was applied, so the stored profile's captured values would
// go stale (MYC-66). The rest of the content falls back to the chat's
// effective defaults (the global welcome and keyword configuration) when no
// profile has been applied, so a never-configured chat can still be cloned.
func (b *Bot) chatProfile(chatID int64) store.ProfileContent {
	snap := b.store.Settings(chatID)
	keywords := append([]string(nil), b.cfg.Keywords.Blocked...)
	fallback := store.ProfileContent{
		Config: store.ProfileConfig{
			Language: string(snap.Language),
			Timezone: snap.Timezone,
			Welcome: store.WelcomeProfile{
				Enabled: b.cfg.Welcome.Enabled,
				Message: b.cfg.Welcome.Message,
			},
		},
		Rules: store.ProfileRules{Keywords: store.NormalizeKeywords(keywords)},
	}
	content, ok := b.store.Profile(chatID)
	if !ok {
		return fallback
	}
	content.Config.Language = string(snap.Language)
	content.Config.Timezone = snap.Timezone
	return content
}

// requireSourceAdmin verifies the caller may read from the source chat with
// the same owner/creator/administrator semantics as requireAdmin, but for a
// chat the command was not issued in. Any lookup error is a refusal.
func (b *Bot) requireSourceAdmin(ctx context.Context, chatID int64, sender *models.User) bool {
	if sender == nil {
		return false
	}
	ok, err := b.admin.IsAdmin(ctx, chatID, sender.ID)
	if err != nil {
		b.log.Error("source admin check failed", "chat", chatID, "user", sender.ID, "err", err)
		return false
	}
	return ok
}

// handleCloneCmd copies the source chat's bot configuration and rules into
// the issuing chat. Both sides must be administered by the caller.
func (b *Bot) handleCloneCmd(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	chatID := upd.Message.Chat.ID
	lang := b.chatLang(chatID)
	sender := messageSender(upd)

	if !b.requireAdmin(ctx, chatID, sender) {
		return
	}
	fields := strings.Fields(upd.Message.Text)
	if len(fields) < 2 {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCloneUsage, nil))
		return
	}
	source, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCloneUsage, nil))
		return
	}
	if source == chatID {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCloneSame, nil))
		return
	}
	if !b.requireSourceAdmin(ctx, source, sender) {
		b.log.Info("clone refused: caller is not admin of the source chat", "chat", chatID, "source", source, "actor", senderID(sender))
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyCloneNotAdmin, nil))
		return
	}

	content := b.chatProfile(source)
	version, err := b.store.ApplyProfile(chatID, content)
	if err != nil {
		b.log.Error("clone apply failed", "chat", chatID, "source", source, "err", err)
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateInvalid, map[string]string{"reason": err.Error()}))
		return
	}
	b.log.Info("configuration cloned", "chat", chatID, "source", source, "actor", senderID(sender), "version", version)
	b.send(ctx, chatID, i18n.T(lang, i18n.KeyCloneDone, map[string]string{
		"src":     strconv.FormatInt(source, 10),
		"version": strconv.Itoa(version),
	}))
}

// senderID is a small helper for logging.
func senderID(u *models.User) int64 {
	if u == nil {
		return 0
	}
	return u.ID
}

// chatKeywords returns the chat's cloned blocklist override when a profile
// carrying a keyword list has been applied; nil otherwise.
func (b *Bot) chatKeywords(chatID int64) []string {
	content, ok := b.store.Profile(chatID)
	if !ok || content.Rules.Keywords == nil {
		return nil
	}
	return content.Rules.Keywords
}

// welcomeConfigFor returns the welcome enabled flag and message for a chat:
// the applied profile's values when one exists, else the global config.
func (b *Bot) welcomeConfigFor(chatID int64) (enabled bool, message string) {
	if content, ok := b.store.Profile(chatID); ok {
		return content.Config.Welcome.Enabled, content.Config.Welcome.Message
	}
	return b.cfg.Welcome.Enabled, b.cfg.Welcome.Message
}

// rulesForChat selects the keyword rule engine for a chat: the cloned
// blocklist when a profile with keywords was applied, else the global one.
func (b *Bot) rulesForChat(chatID int64) *governance.RuleEngine {
	if kw := b.chatKeywords(chatID); kw != nil {
		return governance.NewRuleEngine(kw)
	}
	return b.rules
}

// handleTemplateCmd dispatches the /template command family (admins only).
func (b *Bot) handleTemplateCmd(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	chatID := upd.Message.Chat.ID
	lang := b.chatLang(chatID)
	sender := messageSender(upd)

	if !b.requireAdmin(ctx, chatID, sender) {
		return
	}
	actor := senderID(sender)
	fields := strings.Fields(upd.Message.Text)
	if len(fields) < 2 {
		b.sendTemplateUsage(ctx, chatID, lang)
		return
	}
	switch fields[1] {
	case "save":
		b.handleTemplateSave(ctx, chatID, lang, actor, fields[2:])
	case "apply":
		b.handleTemplateApply(ctx, chatID, lang, actor, fields[2:])
	case "export":
		b.handleTemplateExport(ctx, chatID, lang, actor, fields[2:])
	case "import":
		b.handleTemplateImport(ctx, chatID, lang, actor, upd, fields[2:])
	default:
		b.sendTemplateUsage(ctx, chatID, lang)
	}
}

func (b *Bot) sendTemplateUsage(ctx context.Context, chatID int64, lang i18n.Language) {
	b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateUsage, nil))
}

// templateNameOrUsage resolves the template name argument or reports which
// usage/refusal message applies, ok=false means the caller must return.
func (b *Bot) templateNameOrUsage(ctx context.Context, chatID int64, lang i18n.Language, args []string) (string, bool) {
	if len(args) < 1 {
		b.sendTemplateUsage(ctx, chatID, lang)
		return "", false
	}
	return args[0], true
}

// handleTemplateSave captures this chat's configuration and rules as a named
// template in this chat's namespace. Re-saving under the same name overwrites
// it with a new revision; the same name in another chat is a separate template.
func (b *Bot) handleTemplateSave(ctx context.Context, chatID int64, lang i18n.Language, actor int64, args []string) {
	name, ok := b.templateNameOrUsage(ctx, chatID, lang, args)
	if !ok {
		return
	}
	version, err := b.store.SaveTemplate(chatID, name, b.chatProfile(chatID))
	if err != nil {
		b.log.Error("template save failed", "chat", chatID, "name", name, "actor", actor, "err", err)
		if errors.Is(err, store.ErrPersistFailed) {
			b.send(ctx, chatID, i18n.T(lang, i18n.KeyPersistFailed, nil))
			return
		}
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateNameBad, nil))
		return
	}
	b.log.Info("template saved", "chat", chatID, "name", name, "actor", actor, "version", version)
	b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateSaved, map[string]string{
		"name":    name,
		"version": strconv.Itoa(version),
	}))
}

// handleTemplateApply applies a named template owned by this chat to it. The
// store resolves owner-scoped: another chat's template is reported like an
// unknown name, so apply cannot read or hijack foreign templates. The store
// validates and writes atomically; a failure leaves nothing behind.
func (b *Bot) handleTemplateApply(ctx context.Context, chatID int64, lang i18n.Language, actor int64, args []string) {
	name, ok := b.templateNameOrUsage(ctx, chatID, lang, args)
	if !ok {
		return
	}
	content, _, found := b.store.Template(chatID, name)
	if !found {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateNotFound, map[string]string{"name": name}))
		return
	}
	version, err := b.store.ApplyProfile(chatID, content)
	if err != nil {
		b.log.Error("template apply failed", "chat", chatID, "name", name, "actor", actor, "err", err)
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateInvalid, map[string]string{"reason": err.Error()}))
		return
	}
	b.log.Info("template applied", "chat", chatID, "name", name, "actor", actor, "version", version)
	b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateApplied, map[string]string{
		"name":    name,
		"version": strconv.Itoa(version),
	}))
}

// handleTemplateExport renders a stored template owned by this chat as its
// versioned JSON bundle and sends the bundle text. The export — and every
// lookup that finds no template in this chat's namespace — is audit-logged (MYC-67).
func (b *Bot) handleTemplateExport(ctx context.Context, chatID int64, lang i18n.Language, actor int64, args []string) {
	name, ok := b.templateNameOrUsage(ctx, chatID, lang, args)
	if !ok {
		return
	}
	content, version, found := b.store.Template(chatID, name)
	if !found {
		b.log.Info("template export refused: name not owned by this chat", "chat", chatID, "name", name, "actor", actor)
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateNotFound, map[string]string{"name": name}))
		return
	}
	b.log.Info("template exported", "chat", chatID, "name", name, "actor", actor, "version", version)
	data, err := store.EncodeTemplateBundle(store.TemplateContent{Name: name, Version: version, Content: content})
	if err != nil {
		b.log.Error("template export failed", "chat", chatID, "name", name, "err", err)
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateInvalid, map[string]string{"reason": err.Error()}))
		return
	}
	b.send(ctx, chatID, data)
}

// handleTemplateImport parses a versioned bundle, validates it against the
// expected format version and profile structure, and stores it under name in
// this chat's namespace. An import cannot reach another chat's template. An
// unknown version or invalid payload is refused without saving.
func (b *Bot) handleTemplateImport(ctx context.Context, chatID int64, lang i18n.Language, actor int64, upd *models.Update, args []string) {
	name, ok := b.templateNameOrUsage(ctx, chatID, lang, args)
	if !ok {
		return
	}
	prefix := "/template import " + name + " "
	payload := ""
	if rest := strings.TrimPrefix(upd.Message.Text, prefix); rest != upd.Message.Text {
		payload = strings.TrimSpace(rest)
	} else if len(args) > 1 {
		payload = strings.TrimSpace(strings.Join(args[1:], " "))
	}
	if payload == "" {
		b.sendTemplateUsage(ctx, chatID, lang)
		return
	}
	_, content, err := store.DecodeTemplateBundle(payload)
	if err != nil {
		b.log.Info("template import refused", "chat", chatID, "name", name, "actor", actor, "err", err)
		b.sendBundleError(ctx, chatID, lang, name, err)
		return
	}
	version, err := b.store.SaveTemplate(chatID, name, content)
	if err != nil {
		b.log.Error("template import save failed", "chat", chatID, "name", name, "actor", actor, "err", err)
		if errors.Is(err, store.ErrPersistFailed) {
			b.send(ctx, chatID, i18n.T(lang, i18n.KeyPersistFailed, nil))
			return
		}
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateNameBad, nil))
		return
	}
	b.log.Info("template imported", "chat", chatID, "name", name, "actor", actor, "version", version)
	b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateSaved, map[string]string{
		"name":    name,
		"version": strconv.Itoa(version),
	}))
}

// sendBundleError maps a decode failure to its message: bad JSON and unknown
// format versions get dedicated wording; structure problems carry the reason.
func (b *Bot) sendBundleError(ctx context.Context, chatID int64, lang i18n.Language, name string, err error) {
	var bundleErr *store.TemplateBundleError
	if !errors.As(err, &bundleErr) {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateInvalid, map[string]string{"reason": err.Error()}))
		return
	}
	switch bundleErr.Phase {
	case store.BundlePhaseJSON:
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateBadJSON, nil))
	case store.BundlePhaseVersion:
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateBadVersion, nil))
	default:
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyTemplateInvalid, map[string]string{"reason": bundleErr.Reason}))
	}
}
