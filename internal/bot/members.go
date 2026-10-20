package bot

import (
	"context"
	"strconv"
	"strings"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/viceor/tele-admin/internal/i18n"
)

// Member management (MYC-46 slice 1): /restrict and /unrestrict.
//
// Target resolution, in order:
//  1. a numeric user ID argument: "/restrict 42";
//  2. the sender of the replied-to message (reply + "/restrict").
//
// Usernames are not resolved: the Bot API cannot look up an arbitrary
// @username, so accepting one would silently act on the wrong account.
//
// Authorization is fail-closed:
//   - the caller must pass requireAdmin (owner, creator, or administrator);
//   - the target must not be a bot owner, chat creator, or administrator;
//   - the target's status must be readable via getChatMember; a failed lookup
//     aborts the action;
//   - the target status must match the action (MYC-55 SEC-1): /restrict
//     applies only to member and restricted targets, /unrestrict only to
//     restricted ones; kicked/left targets are refused as not applicable.
//     Unban is out of scope and must not be emulated via restrictChatMember;
//   - success is reported only when restrictChatMember returns ok=true with no
//     error. Any error is reported as a failure.
//
// Restriction state lives in Telegram, so this slice needs no local storage.

// memberTarget returns the target user ID of a member command.
func memberTarget(upd *models.Update, args []string) (int64, bool) {
	if len(args) >= 2 {
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 {
			return 0, false
		}
		return id, true
	}
	if r := upd.Message.ReplyToMessage; r != nil && r.From != nil {
		return r.From.ID, true
	}
	return 0, false
}

// allPermissions returns a ChatPermissions value that grants every action.
func allPermissions() *models.ChatPermissions {
	return &models.ChatPermissions{
		CanSendMessages:       true,
		CanSendAudios:         true,
		CanSendDocuments:      true,
		CanSendPhotos:         true,
		CanSendVideos:         true,
		CanSendVideoNotes:     true,
		CanSendVoiceNotes:     true,
		CanSendPolls:          true,
		CanSendOtherMessages:  true,
		CanAddWebPagePreviews: true,
		CanChangeInfo:         true,
		CanInviteUsers:        true,
		CanPinMessages:        true,
		CanManageTopics:       true,
	}
}

// noPermissions returns a ChatPermissions value that denies every action.
func noPermissions() *models.ChatPermissions {
	return &models.ChatPermissions{}
}

func (b *Bot) handleRestrictCmd(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
	b.handleMemberAction(ctx, upd, false)
}

func (b *Bot) handleUnrestrictCmd(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
	b.handleMemberAction(ctx, upd, true)
}

// handleMemberAction runs /restrict (lift=false) or /unrestrict (lift=true).
func (b *Bot) handleMemberAction(ctx context.Context, upd *models.Update, lift bool) {
	if upd.Message == nil {
		return
	}
	chatID := upd.Message.Chat.ID
	lang := b.chatLang(chatID)

	// The actor is resolved once and checked before any use. requireAdmin
	// already rejects a nil sender; the explicit nil return below keeps the
	// audit log from dereferencing From if that guard ever changes (MYC-83).
	actor := messageSender(upd)
	if !b.requireAdmin(ctx, chatID, actor) {
		return
	}
	if actor == nil {
		return
	}

	target, ok := memberTarget(upd, strings.Fields(upd.Message.Text))
	if !ok {
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyMemberUsage, nil))
		return
	}

	// Refuse before any mutation: protected targets (owner/admin/unverifiable
	// status) and statuses the action does not apply to. A failed status
	// lookup is treated as "cannot prove the target is unprotected", so the
	// action is refused as well.
	switch b.checkMemberTarget(ctx, chatID, target, lift) {
	case targetProtected:
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyMemberProtected, nil))
		return
	case targetNotApplicable:
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyMemberNotApplicable, nil))
		return
	}

	perms := noPermissions()
	if lift {
		perms = allPermissions()
	}
	done, err := b.client.RestrictChatMember(ctx, &tgbot.RestrictChatMemberParams{
		ChatID:      chatID,
		UserID:      target,
		Permissions: perms,
	})
	if err != nil || !done {
		b.log.Error("restrictChatMember failed", "chat", chatID, "target", target, "lift", lift, "ok", done, "err", err)
		b.send(ctx, chatID, i18n.T(lang, i18n.KeyMemberFailed, nil))
		return
	}

	b.log.Info("member permissions changed", "chat", chatID, "actor", actor.ID, "target", target, "lift", lift)
	key := i18n.KeyMemberRestricted
	if lift {
		key = i18n.KeyMemberUnrestrict
	}
	b.send(ctx, chatID, i18n.T(lang, key, map[string]string{"id": strconv.FormatInt(target, 10)}))
}

// targetCheck classifies the outcome of the pre-mutation target status check.
type targetCheck int

const (
	// targetOK means the target status matches the action; proceed.
	targetOK targetCheck = iota
	// targetProtected means the target must not be acted on at all: a bot
	// owner, a chat creator/administrator, or an unreadable status
	// (fail-closed).
	targetProtected
	// targetNotApplicable means the action does not apply to this status, for
	// example /unrestrict on a plain member or either command on a
	// kicked/left (banned or non-member) user (MYC-55 SEC-1).
	targetNotApplicable
)

// checkMemberTarget verifies the target's chat status before a member action.
// /restrict applies to member and restricted targets; /unrestrict applies to
// restricted targets only. Protected and unreadable statuses are refused as
// protected; any other status is refused as not applicable.
func (b *Bot) checkMemberTarget(ctx context.Context, chatID, target int64, lift bool) targetCheck {
	if b.admin.IsOwner(target) {
		return targetProtected
	}
	member, err := b.client.GetChatMember(ctx, &tgbot.GetChatMemberParams{ChatID: chatID, UserID: target})
	if err != nil || member == nil {
		b.log.Error("target status lookup failed; refusing action", "chat", chatID, "target", target, "err", err)
		return targetProtected
	}
	switch member.Type {
	case models.ChatMemberTypeOwner, models.ChatMemberTypeAdministrator:
		return targetProtected
	case models.ChatMemberTypeMember:
		if lift {
			return targetNotApplicable
		}
		return targetOK
	case models.ChatMemberTypeRestricted:
		return targetOK
	default:
		return targetNotApplicable
	}
}
