package bot

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/viceor/tele-admin/internal/i18n"
	"github.com/viceor/tele-admin/internal/store"
)

// Text/media multi-target publishing (slice S2).
//
// Commands:
//
//	/publish text <targets> <text>
//	/publish photo|video|document <targets> <file_id|URL> [caption]
//	/publish_add        register this chat as a publish target (admin only)
//	/publish_remove     unregister this chat (admin only)
//
// Targets are a comma-separated list of registered chat IDs, or "all" for
// every registered chat. Authorization is fail-closed:
//
//   - only bot owners, chat creators, or administrators may publish
//     (requireAdmin); anonymous messages are treated as unauthorized;
//   - commands sent from a private chat are rejected unless the sender is a
//     configured bot owner: a private chat's human participant resolves to
//     creator status in the Bot API, so without this check any user could
//     broadcast to every registered target from a private chat;
//   - only registered chats may be targets; an unregistered explicit target
//     aborts the whole command before anything is sent;
//   - before sending, the bot's own send permission is verified in each
//     target chat (fail-closed on lookup errors); a channel administrator
//     additionally needs can_post_messages;
//   - targets are sent concurrently, bounded by a worker cap, and every
//     target is bounded by a per-target deadline (probe + send); a slow or
//     hung target never blocks the others and a deadline hit is recorded as
//     that target's timeout failure;
//   - every target reports its own result; the summary claims success only
//     when all targets succeeded, so a partial failure is never reported as
//     an overall success. Results are aggregated in input order regardless
//     of completion order.
//
// Media is addressed by Telegram file_id or by http(s) URL. Anything that
// looks like a filesystem path (path separators, drive letters, leading
// dots) is rejected: the bot never reads local files.
//
// Limits (configurable, see PublishConfig): target count per command and
// content length (text, and media captions capped to Telegram's 1024).
//
// Every publish is recorded in the store with per-target results.

// Per-target failure reason codes stored in publish records.
const (
	publishReasonNoRight = "no_send_right"
	publishReasonLookup  = "lookup_failed"
	publishReasonSend    = "send_failed"
	publishReasonTimeout = "timeout"
)

// telegramCaptionMax is the Bot API caption limit for photo/video/document.
const telegramCaptionMax = 1024

// publishDefaultMaxTargets and publishDefaultMaxContent mirror the config
// defaults so the limits stay sane even for a Config that skipped Validate
// (for example a hand-built test config).
const (
	publishDefaultMaxTargets = 10
	publishDefaultMaxContent = 4096
)

// publishDefaultPerTargetTimeout and publishDefaultMaxConcurrentSends mirror
// the config defaults for the per-target deadline and the send worker cap.
const (
	publishDefaultPerTargetTimeout   = 10 * time.Second
	publishDefaultMaxConcurrentSends = 4
)

// publishMaxTargets returns the effective per-command target limit.
func (b *Bot) publishMaxTargets() int {
	if v := b.cfg.Publish.MaxTargets; v > 0 {
		return v
	}
	return publishDefaultMaxTargets
}

// publishMaxContent returns the effective content length limit (runes).
func (b *Bot) publishMaxContent() int {
	if v := b.cfg.Publish.MaxContentLength; v > 0 {
		return v
	}
	return publishDefaultMaxContent
}

// publishPerTargetTimeout returns the effective deadline for one target's
// permission probe and send.
func (b *Bot) publishPerTargetTimeout() time.Duration {
	if v := b.cfg.Publish.PerTargetTimeout; v > 0 {
		return v
	}
	return publishDefaultPerTargetTimeout
}

// publishMaxConcurrentSends returns the effective cap on how many targets
// receive content in parallel.
func (b *Bot) publishMaxConcurrentSends() int {
	if v := b.cfg.Publish.MaxConcurrentSends; v > 0 {
		return v
	}
	return publishDefaultMaxConcurrentSends
}

// cutToken splits the leading whitespace-delimited token off s and returns
// the token plus the remainder without its leading whitespace.
func cutToken(s string) (token, rest string) {
	s = strings.TrimLeft(s, " \t")
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeft(s[i:], " \t")
}

// normalizePublishKind maps the kind token to a canonical kind, or "" when
// the token is not a supported kind.
func normalizePublishKind(token string) string {
	switch strings.ToLower(token) {
	case "text":
		return "text"
	case "photo":
		return "photo"
	case "video":
		return "video"
	case "document":
		return "document"
	}
	return ""
}

// parsePublishCommand splits raw command text into kind, the target token,
// and the raw content remainder. ok is false when any of the three tokens is
// missing.
func parsePublishCommand(text string) (kind, targetToken, content string, ok bool) {
	rest := strings.TrimSpace(text)
	var cmd string
	cmd, rest = cutToken(rest) // "/publish"
	if cmd == "" {
		return "", "", "", false
	}
	kind, rest = cutToken(rest)
	if kind == "" {
		return "", "", "", false
	}
	targetToken, rest = cutToken(rest)
	if targetToken == "" {
		return "", "", "", false
	}
	return normalizePublishKind(kind), targetToken, strings.TrimSpace(rest), normalizePublishKind(kind) != ""
}

// parsePublishTargets parses the target token: "all" selects every
// registered chat; otherwise a comma-separated list of chat IDs (which may
// be negative for supergroups/channels; zero is never valid).
func parsePublishTargets(token string) (targets []int64, all, valid bool) {
	if strings.EqualFold(strings.TrimSpace(token), "all") {
		return nil, true, true
	}
	seen := make(map[int64]bool)
	for _, part := range strings.Split(token, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, false, false
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id == 0 {
			return nil, false, false
		}
		if !seen[id] {
			seen[id] = true
			targets = append(targets, id)
		}
	}
	return targets, false, len(targets) > 0
}

// validMediaRef reports whether ref is acceptable media content: an
// http(s) URL with a host, or a Telegram file_id (an opaque token without
// path-like characters). Filesystem paths are rejected.
func validMediaRef(ref string, maxLen int) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" || len([]rune(ref)) > maxLen {
		return false
	}
	lower := strings.ToLower(ref)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		u, err := url.Parse(ref)
		return err == nil && u.Host != ""
	}
	if strings.ContainsAny(ref, "/\\:") || strings.ContainsAny(ref, " \t\n\r") {
		return false
	}
	return !strings.HasPrefix(ref, ".")
}

// botUserID returns the bot's own user ID, fetching and caching it on first
// use so a startup-time getMe failure does not lock publishing out forever.
func (b *Bot) botUserID(ctx context.Context) (int64, error) {
	b.botMu.Lock()
	defer b.botMu.Unlock()
	if b.botID != 0 {
		return b.botID, nil
	}
	me, err := b.client.GetMe(ctx)
	if err != nil {
		return 0, err
	}
	b.botID = me.ID
	return b.botID, nil
}

// botCanSendIn verifies that the bot itself may send messages in chatID. It
// fails closed: any lookup error means "not proven", and the target is
// reported as failed instead of being sent to.
func (b *Bot) botCanSendIn(ctx context.Context, botID, chatID int64) (bool, string) {
	member, err := b.client.GetChatMember(ctx, &tgbot.GetChatMemberParams{ChatID: chatID, UserID: botID})
	if err != nil || member == nil {
		return false, publishReasonLookup
	}
	switch member.Type {
	case models.ChatMemberTypeOwner:
		// The chat owner holds every admin right, including posting.
		return true, ""
	case models.ChatMemberTypeAdministrator:
		// In channels an administrator may hold no can_post_messages right;
		// in groups and supergroups administrators can always post, and the
		// Bot API does not report the flag there. The chat type decides
		// which rule applies, and a failed lookup fails closed.
		chat, err := b.client.GetChat(ctx, &tgbot.GetChatParams{ChatID: chatID})
		if err != nil || chat == nil {
			return false, publishReasonLookup
		}
		if chat.Type == models.ChatTypeChannel {
			admin := member.Administrator
			if admin == nil || !admin.CanPostMessages {
				return false, publishReasonNoRight
			}
		}
		return true, ""
	case models.ChatMemberTypeRestricted:
		r := member.Restricted
		if r != nil && r.IsMember && r.CanSendMessages {
			return true, ""
		}
		return false, publishReasonNoRight
	case models.ChatMemberTypeMember:
		// A plain member's rights depend on the chat's default permissions.
		chat, err := b.client.GetChat(ctx, &tgbot.GetChatParams{ChatID: chatID})
		if err != nil || chat == nil {
			return false, publishReasonLookup
		}
		if chat.Permissions != nil && chat.Permissions.CanSendMessages {
			return true, ""
		}
		return false, publishReasonNoRight
	default:
		return false, publishReasonNoRight
	}
}

// handlePublishCmd runs /publish.
func (b *Bot) handlePublishCmd(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	chatID := upd.Message.Chat.ID
	lang := b.chatLang(chatID)
	sender := messageSender(upd)

	if !b.requireAdmin(ctx, chatID, sender) {
		return
	}
	if upd.Message.Chat.Type == models.ChatTypePrivate && !b.admin.IsOwner(sender.ID) {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyNotAdmin, nil))
		return
	}

	kind, targetToken, content, ok := parsePublishCommand(upd.Message.Text)
	if !ok {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishUsage, nil))
		return
	}

	targets, all, valid := parsePublishTargets(targetToken)
	if !valid {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishUsage, nil))
		return
	}
	if all {
		targets = b.store.PublishTargets()
		if len(targets) == 0 {
			b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishNoTargets, nil))
			return
		}
	} else {
		for _, t := range targets {
			if !b.store.IsPublishTarget(t) {
				b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishUnregistered, map[string]string{"id": strconv.FormatInt(t, 10)}))
				return
			}
		}
	}
	if len(targets) > b.publishMaxTargets() {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishTooManyTargets, map[string]string{
			"n":   strconv.Itoa(len(targets)),
			"max": strconv.Itoa(b.publishMaxTargets()),
		}))
		return
	}

	ref, caption := "", ""
	switch kind {
	case "text":
		if content == "" {
			b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishUsage, nil))
			return
		}
		if n := len([]rune(content)); n > b.publishMaxContent() {
			b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishTextTooLong, map[string]string{
				"n":   strconv.Itoa(n),
				"max": strconv.Itoa(b.publishMaxContent()),
			}))
			return
		}
	case "photo", "video", "document":
		ref, caption = cutToken(content)
		if !validMediaRef(ref, b.publishMaxContent()) {
			b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishBadMediaRef, nil))
			return
		}
		if n := len([]rune(caption)); n > min(b.publishMaxContent(), telegramCaptionMax) {
			b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishCaptionTooLong, map[string]string{
				"n":   strconv.Itoa(n),
				"max": strconv.Itoa(min(b.publishMaxContent(), telegramCaptionMax)),
			}))
			return
		}
	}

	botID, err := b.botUserID(ctx)
	if err != nil {
		b.log.Error("publish aborted: bot identity unavailable", "chat", chatID, "err", err)
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishFailed, nil))
		return
	}

	results := b.publishToTargets(ctx, botID, targets, kind, content, ref, caption)

	rec, recErr := b.store.AddPublish(chatID, sender.ID, kind, targets, results)
	if recErr != nil {
		// Messages have already left; nothing can unsend them. Distinguish
		// this state explicitly instead of claiming a completed publish:
		// the send is not recorded anywhere (MYC-73).
		b.log.Error("publish delivered but the record was not persisted", "chat", chatID, "actor", sender.ID, "kind", kind, "targets", targets, "err", recErr)
		var sb strings.Builder
		sb.WriteString(i18n.T(lang, i18n.KeyPublishNotRecorded, nil))
		for _, target := range results {
			id := strconv.FormatInt(target.ChatID, 10)
			if target.Success {
				sb.WriteString("\n\u2713 " + id + " (sent, not recorded)")
				continue
			}
			sb.WriteString("\n\u2717 " + id + ": " + i18n.T(lang, publishReasonKey(target.Reason), nil))
		}
		b.send(ctx, chatID, sb.String())
		return
	}
	b.log.Info("publish finished", "id", rec.ID, "chat", chatID, "actor", rec.SenderID, "kind", kind, "ok", countPublishSuccesses(results), "total", len(results))
	b.sendPublishSummary(ctx, chatID, lang, results)
}

// publishToTargets delivers the content to every target with bounded
// concurrency: at most publishMaxConcurrentSends workers run at once, so a
// slow target never starves the others. Every target is bounded by its own
// deadline, and the returned slice holds exactly one result per target in
// input order, whatever the completion order is.
func (b *Bot) publishToTargets(ctx context.Context, botID int64, targets []int64, kind, content, ref, caption string) []store.PublishResult {
	results := make([]store.PublishResult, len(targets))
	workers := min(b.publishMaxConcurrentSends(), len(targets))
	if workers < 1 {
		workers = 1
	}
	timeout := b.publishPerTargetTimeout()

	queue := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for i := range queue {
				results[i] = b.publishToTarget(ctx, botID, targets[i], kind, content, ref, caption, timeout)
			}
		}()
	}
	for i := range targets {
		queue <- i
	}
	close(queue)
	wg.Wait()
	return results
}

// publishToTarget probes and sends to one target under the per-target
// deadline. A deadline hit during the probe or the send is recorded as a
// timeout failure for this target alone; other failure modes keep their own
// reason codes.
func (b *Bot) publishToTarget(parent context.Context, botID, chatID int64, kind, content, ref, caption string, timeout time.Duration) store.PublishResult {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	if allowed, reason := b.botCanSendIn(ctx, botID, chatID); !allowed {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = publishReasonTimeout
		}
		return store.PublishResult{ChatID: chatID, Success: false, Reason: reason}
	}
	if err := b.sendPublishContent(ctx, chatID, kind, content, ref, caption); err != nil {
		b.log.Error("publish send failed", "target", chatID, "kind", kind, "err", err)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return store.PublishResult{ChatID: chatID, Success: false, Reason: publishReasonTimeout}
		}
		return store.PublishResult{ChatID: chatID, Success: false, Reason: publishReasonSend}
	}
	return store.PublishResult{ChatID: chatID, Success: true}
}

// sendPublishContent delivers one content item to one target chat. It
// reports whether Telegram accepted the send.
func (b *Bot) sendPublishContent(ctx context.Context, chatID int64, kind, text, ref, caption string) error {
	switch kind {
	case "text":
		_, err := b.client.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: chatID, Text: text})
		return err
	case "photo":
		_, err := b.client.SendPhoto(ctx, &tgbot.SendPhotoParams{ChatID: chatID, Photo: &models.InputFileString{Data: ref}, Caption: caption})
		return err
	case "video":
		_, err := b.client.SendVideo(ctx, &tgbot.SendVideoParams{ChatID: chatID, Video: &models.InputFileString{Data: ref}, Caption: caption})
		return err
	case "document":
		_, err := b.client.SendDocument(ctx, &tgbot.SendDocumentParams{ChatID: chatID, Document: &models.InputFileString{Data: ref}, Caption: caption})
		return err
	}
	return errors.New("unknown publish kind " + kind)
}

// countPublishSuccesses returns how many per-target results succeeded.
func countPublishSuccesses(results []store.PublishResult) int {
	n := 0
	for _, r := range results {
		if r.Success {
			n++
		}
	}
	return n
}

// publishReasonKey maps a stored failure reason code to its message key.
func publishReasonKey(reason string) i18n.MessageKey {
	switch reason {
	case publishReasonNoRight:
		return i18n.KeyPublishReasonNoRight
	case publishReasonLookup:
		return i18n.KeyPublishReasonLookup
	case publishReasonSend:
		return i18n.KeyPublishReasonSend
	case publishReasonTimeout:
		return i18n.KeyPublishReasonTimeout
	}
	return i18n.KeyPublishReasonSend
}

// sendPublishSummary reports per-target outcomes. The overall line claims
// success only when every target succeeded; a partial failure is labeled as
// such.
func (b *Bot) sendPublishSummary(ctx context.Context, chatID int64, lang i18n.Language, results []store.PublishResult) {
	ok := countPublishSuccesses(results)
	total := len(results)
	header := i18n.KeyPublishAllOk
	if ok < total {
		header = i18n.KeyPublishPartial
	}
	var sb strings.Builder
	sb.WriteString(i18n.T(lang, header, map[string]string{
		"ok": strconv.Itoa(ok),
		"n":  strconv.Itoa(total),
	}))
	for _, r := range results {
		id := strconv.FormatInt(r.ChatID, 10)
		if r.Success {
			sb.WriteString("\n\u2713 " + id)
			continue
		}
		sb.WriteString("\n\u2717 " + id + ": " + i18n.T(lang, publishReasonKey(r.Reason), nil))
	}
	b.send(ctx, chatID, sb.String())
}

// handlePublishAdd registers the current chat as a publish target. The
// caller must be an admin of this chat; private chats are refused because
// publish targets must be groups or channels the bot can verify.
func (b *Bot) handlePublishAdd(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
	b.handlePublishRegistry(ctx, upd, true)
}

// handlePublishRemove unregisters the current chat as a publish target.
func (b *Bot) handlePublishRemove(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
	b.handlePublishRegistry(ctx, upd, false)
}

func (b *Bot) handlePublishRegistry(ctx context.Context, upd *models.Update, add bool) {
	if upd.Message == nil {
		return
	}
	chatID := upd.Message.Chat.ID
	lang := b.chatLang(chatID)
	sender := messageSender(upd)
	if sender == nil {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyNotAdmin, nil))
		return
	}
	if !b.requireAdmin(ctx, chatID, sender) {
		return
	}
	if upd.Message.Chat.Type == models.ChatTypePrivate {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishPrivateChat, nil))
		return
	}
	var err error
	if add {
		err = b.store.AddPublishTarget(chatID, sender.ID)
	} else {
		err = b.store.RemovePublishTarget(chatID)
	}
	switch {
	case err == nil:
		if add {
			b.log.Info("publish target registered", "chat", chatID, "actor", sender.ID)
			b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishRegistered, nil))
		} else {
			b.log.Info("publish target removed", "chat", chatID, "actor", sender.ID)
			b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishRemoveOk, nil))
		}
	case errors.Is(err, store.ErrTargetRegistered):
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishAlreadyReg, nil))
	case errors.Is(err, store.ErrTargetNotRegistered):
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishNotRegistered, nil))
	default:
		b.log.Error("publish registry update failed", "chat", chatID, "add", add, "err", err)
		if errors.Is(err, store.ErrPersistFailed) {
			b.send(ctx, chatID, i18n.T(lang, i18n.KeyPersistFailed, nil))
			return
		}
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyPublishFailed, nil))
	}
}
