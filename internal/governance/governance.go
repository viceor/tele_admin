// Package governance implements admin permission verification, keyword
// blocklists, and the minimal anti-spam rule chain.
package governance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/viceor/tele-admin/internal/store"
)

// TelegramClient is the subset of the Bot API the governance layer depends on.
// *bot.Bot satisfies this interface, and tests can provide a mock.
type TelegramClient interface {
	GetChatMember(ctx context.Context, params *bot.GetChatMemberParams) (*models.ChatMember, error)
	SendMessage(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error)
	DeleteMessage(ctx context.Context, params *bot.DeleteMessageParams) (bool, error)
}

// AdminChecker verifies Telegram administrator permissions.
//
// Final authorization semantics (MYC-32 M-3):
//   - enforceAdmin == false: every user is treated as an administrator. This
//     is only permitted in explicit local development mode; production
//     deployments must keep enforcement on.
//   - enforceAdmin == true: a user is authorized when they are a configured
//     bot owner (owner_ids) OR the Bot API reports them as the chat creator
//     or an administrator. Owners bypass the chat-level check, so a missing
//     or failing getChatMember call never locks an owner out.
type AdminChecker struct {
	client       TelegramClient
	enforceAdmin bool
	owners       map[int64]bool
}

// NewAdminChecker builds an AdminChecker. When enforceAdmin is false every
// user is treated as an administrator (explicit local development only).
func NewAdminChecker(client TelegramClient, enforceAdmin bool, ownerIDs []int64) *AdminChecker {
	owners := make(map[int64]bool, len(ownerIDs))
	for _, id := range ownerIDs {
		owners[id] = true
	}
	return &AdminChecker{client: client, enforceAdmin: enforceAdmin, owners: owners}
}

// IsOwner reports whether the user is a configured bot owner.
func (a *AdminChecker) IsOwner(userID int64) bool {
	return a.owners[userID]
}

// IsAdmin reports whether the user is authorized to run admin commands:
// a configured bot owner, or a chat creator/administrator per the Bot API.
// It returns an error when the Bot API call fails.
func (a *AdminChecker) IsAdmin(ctx context.Context, chatID, userID int64) (bool, error) {
	if !a.enforceAdmin {
		return true, nil
	}
	if a.IsOwner(userID) {
		return true, nil
	}
	member, err := a.client.GetChatMember(ctx, &bot.GetChatMemberParams{ChatID: chatID, UserID: userID})
	if err != nil {
		return false, err
	}
	// A nil member with a nil error must be rejected, not dereferenced
	// (MYC-54 #1): the caller cannot be authorized on an unreadable result.
	if member == nil {
		return false, fmt.Errorf("getChatMember returned no member for chat %d user %d", chatID, userID)
	}
	return member.Type == models.ChatMemberTypeOwner || member.Type == models.ChatMemberTypeAdministrator, nil
}

// RuleEngine applies keyword blocklist rules to message text.
type RuleEngine struct {
	blocked []string
}

// NewRuleEngine builds a RuleEngine. Blocked terms are matched
// case-insensitively on whitespace-split tokens and substrings.
func NewRuleEngine(blocked []string) *RuleEngine {
	terms := make([]string, 0, len(blocked))
	for _, b := range blocked {
		if t := strings.ToLower(strings.TrimSpace(b)); t != "" {
			terms = append(terms, t)
		}
	}
	return &RuleEngine{blocked: terms}
}

// Check reports whether text contains a blocked term and returns the term.
func (r *RuleEngine) Check(text string) (blocked bool, matched string) {
	lower := strings.ToLower(text)
	for _, term := range r.blocked {
		if strings.Contains(lower, term) {
			return true, term
		}
	}
	return false, ""
}

// SpamChecker rate-limits users using the shared message store.
type SpamChecker struct {
	st *store.Store
}

// NewSpamChecker builds a SpamChecker backed by a Store.
func NewSpamChecker(st *store.Store) *SpamChecker {
	return &SpamChecker{st: st}
}

// Check records a message and reports whether it is allowed.
func (c *SpamChecker) Check(userID int64, now time.Time) (bool, string) {
	return c.st.RecordMessage(userID, now)
}
