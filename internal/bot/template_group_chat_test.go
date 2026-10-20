package bot

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/viceor/tele-admin/internal/store"
)

// TestTemplateCommandsWorkInRealGroups covers MYC-91 end to end: group /
// supergroup / channel chat ids are negative, and the whole /template
// save-export-import-apply cycle must work there. Before the fix, the store
// refused every negative owner and the bot answered "Invalid template name".
func TestTemplateCommandsWorkInRealGroups(t *testing.T) {
	b, mock := newCloneTestBot(t)
	client := b.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.StartWebhook(ctx)

	const group = int64(-1001234567890)
	mock.setMemberStatusForChat(group, 7, "administrator")

	// Save captures this chat's default-effective configuration.
	postUpdate(t, client.WebhookHandler(), textMessageUpdateWithID(group, 7, 1, "/template save alpha"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, group, "Template alpha saved")
	})
	content, version, ok := b.store.Template(group, "alpha")
	if !ok || version != 1 {
		t.Fatalf("Template(group, alpha) = (%+v, %d, %v), want version 1", content, version, ok)
	}

	// Export sends the versioned bundle to the group.
	postUpdate(t, client.WebhookHandler(), textMessageUpdateWithID(group, 7, 2, "/template export alpha"))
	var payload string
	waitFor(t, 2*time.Second, func() bool {
		for _, s := range sentTextsForChat(mock, group) {
			if len(s) > 0 && s[0] == '{' {
				payload = s
				return true
			}
		}
		return false
	})
	if payload == "" {
		t.Fatal("export produced no bundle for the group chat")
	}

	// Import the bundle back under a new name in the same group namespace.
	postUpdate(t, client.WebhookHandler(), textMessageUpdateWithID(group, 7, 3, "/template import beta "+payload))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, group, "Template beta saved")
	})
	if _, _, ok := b.store.Template(group, "beta"); !ok {
		t.Fatal("import stored nothing in the group's namespace")
	}

	// Apply activates it in the group.
	postUpdate(t, client.WebhookHandler(), textMessageUpdateWithID(group, 7, 4, "/template apply beta"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, group, "Template beta applied")
	})
	if _, _, ok := b.store.Template(group, "beta"); !ok {
		t.Fatal("applied template vanished from the namespace")
	}

	// The name conflict checks stay namespaced: the same user's save in
	// another group does not see this group's template.
	const other = int64(-1009999999999)
	mock.setMemberStatusForChat(other, 7, "administrator")
	postUpdate(t, client.WebhookHandler(), textMessageUpdateWithID(other, 7, 5, "/template export alpha"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, other, "No template named alpha")
	})
	for _, s := range sentTextsForChat(mock, other) {
		if len(s) > 0 && s[0] == '{' {
			t.Fatalf("cross-group export leaked a template bundle to chat %s: %s", strconv.FormatInt(other, 10), s)
		}
	}
}

// TestGroupTemplateRoundTripIncludesWelcomeMessage pins the group-chat save
// content: the persisted template carries the chat's current configuration
// (welcome message and keyword override) exactly as a positive chat's would.
// Restart reload of the negative-owner template is covered at the store layer
// by TestGroupChatTemplatesPersistAcrossRestart.
func TestGroupTemplateRoundTripIncludesWelcomeMessage(t *testing.T) {
	b, mock := newCloneTestBot(t)
	client := b.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.StartWebhook(ctx)

	const group = int64(-1001234567890)
	mock.setMemberStatusForChat(group, 7, "administrator")

	content := store.ProfileContent{
		Config: store.ProfileConfig{
			Language: "en",
			Timezone: "UTC",
			Welcome:  store.WelcomeProfile{Enabled: true, Message: "group hello"},
		},
		Rules: store.ProfileRules{Keywords: []string{"spam"}},
	}
	if _, err := b.store.ApplyProfile(group, content); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	postUpdate(t, client.WebhookHandler(), textMessageUpdateWithID(group, 7, 1, "/template save alpha"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, group, "Template alpha saved")
	})
	got, _, ok := b.store.Template(group, "alpha")
	if !ok || got.Config.Welcome.Message != "group hello" || len(got.Rules.Keywords) != 1 || got.Rules.Keywords[0] != "spam" {
		t.Fatalf("group template content = %+v (found %v)", got, ok)
	}
}
