package bot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/viceor/tele-admin/internal/i18n"
	"github.com/viceor/tele-admin/internal/store"
)

// textMessageUpdateWithID is like textMessageUpdate but with a caller-chosen
// message_id, so deletion assertions can tell messages apart.
func textMessageUpdateWithID(chatID, userID int64, msgID int, text string) string {
	message := map[string]any{
		"message_id": msgID,
		"from":       map[string]any{"id": userID, "first_name": "alice"},
		"chat":       map[string]any{"id": chatID, "title": "group"},
		"text":       text,
	}
	if cmd, ok := commandToken(text); ok {
		message["entities"] = []map[string]any{
			{"type": "bot_command", "offset": 0, "length": len(cmd)},
		}
	}
	return toJSON(map[string]any{"update_id": 1, "message": message})
}

// anonymousMessageUpdate already exists in security_test.go.

func newCloneTestBot(t *testing.T) (*Bot, *mockTelegram) {
	t.Helper()
	mock := newMockTelegram(t)
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	b, err := New(testConfig(srv.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b, mock
}

// sentTexts returns every sent message text.
func sentTexts(mock *mockTelegram) []string {
	var out []string
	for _, m := range mock.sent() {
		if s, ok := m["text"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func sentContainsText(mock *mockTelegram, substr string) bool {
	for _, s := range sentTexts(mock) {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// TestCloneRequiresAdminOnBothSides covers the fail-closed permission gates:
// an anonymous sender, a plain member, and an admin on only one side are all
// refused with no cloned state; only an admin of both chats may clone.
func TestCloneRequiresAdminOnBothSides(t *testing.T) {
	b, mock := newCloneTestBot(t)
	client := b.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.StartWebhook(ctx)

	// Anonymous messages cannot clone.
	postUpdate(t, client.WebhookHandler(), anonymousMessageUpdate(500, "/clone 600"))
	waitFor(t, 2*time.Second, func() bool { return sentContainsText(mock, "Only group administrators") })
	if _, ok := b.store.Profile(500); ok {
		t.Fatal("anonymous clone stored a profile")
	}

	// A plain member of both chats is refused by the target-side check.
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/clone 600"))
	waitFor(t, 2*time.Second, func() bool { return sentContainsText(mock, "Only group administrators may use this command.") })
	if _, ok := b.store.Profile(500); ok {
		t.Fatal("member clone stored a profile")
	}

	// Admin on the target chat but not on the source chat: the clone is
	// refused by the source-side check. The chat-scoped mock statuses make
	// the same user's status differ between the two chats.
	mock.setMemberStatusForChat(500, 7, "administrator")
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/clone 600"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsText(mock, "administrator rights in both the source chat")
	})
	if _, ok := b.store.Profile(500); ok {
		t.Fatal("one-sided admin clone stored a profile")
	}

	// Admin on both sides: the clone succeeds.
	mock.setMemberStatusForChat(600, 7, "administrator")
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/clone 600"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsText(mock, "Configuration cloned from chat 600")
	})
	if _, ok := b.store.Profile(500); !ok {
		t.Fatal("both-admin clone did not store a profile")
	}
}

// TestCloneCopiesRulesAndWelcome verifies the cloned profile actually
// governs the target chat afterwards: the copied keyword blocklist replaces
// the global one and the copied welcome message is used.
func TestCloneCopiesRulesAndWelcome(t *testing.T) {
	b, mock := newCloneTestBot(t)
	client := b.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.StartWebhook(ctx)

	mock.setMemberStatusForChat(500, 7, "administrator")
	mock.setMemberStatusForChat(600, 7, "administrator")

	content := store.ProfileContent{
		Config: store.ProfileConfig{
			Language: string(i18n.English),
			Timezone: "UTC",
			Welcome:  store.WelcomeProfile{Enabled: true, Message: "cloned hello {name}"},
		},
		Rules: store.ProfileRules{Keywords: []string{"foo"}},
	}
	if _, err := b.store.ApplyProfile(600, content); err != nil {
		t.Fatalf("seed source profile: %v", err)
	}

	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/clone 600"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsText(mock, "Configuration cloned from chat 600")
	})

	// The global keyword ("spam" in testConfig) is no longer enforced in the
	// target chat; the cloned keyword is.
	postUpdate(t, client.WebhookHandler(), textMessageUpdateWithID(500, 7, 22, "buy cheap spam now"))
	postUpdate(t, client.WebhookHandler(), textMessageUpdateWithID(500, 7, 23, "foo attack"))
	waitFor(t, 2*time.Second, func() bool {
		for _, id := range mock.deleted() {
			if id == 23 {
				return true
			}
		}
		return false
	})
	time.Sleep(300 * time.Millisecond)
	for _, id := range mock.deleted() {
		if id == 22 {
			t.Fatal("global keyword still enforced after the clone replaced the blocklist")
		}
	}

	// The cloned welcome message overrides the global one.
	postUpdate(t, client.WebhookHandler(), joinUpdate(500, 42, "bob"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsText(mock, "cloned hello")
	})
}

// TestTemplateImportRejectsUnknownVersionAndBadJSON is the version-mismatch
// and malformed-payload test: an import carrying an unknown bundle version
// or unparseable JSON is refused and nothing is stored.
func TestTemplateImportRejectsUnknownVersionAndBadJSON(t *testing.T) {
	b, mock := newCloneTestBot(t)
	client := b.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.StartWebhook(ctx)
	mock.setMemberStatusForChat(500, 7, "administrator")

	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, `/template import bad {oops`))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsText(mock, "not valid JSON")
	})
	if _, _, ok := b.store.Template(500, "bad"); ok {
		t.Fatal("bad JSON import stored a template")
	}

	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, `/template import evil {"format":99,"template":{}}`))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsText(mock, "Unknown template version")
	})
	if _, _, ok := b.store.Template(500, "evil"); ok {
		t.Fatal("unknown-version import stored a template")
	}

	// A valid version-1 bundle is accepted.
	content := store.ProfileContent{}
	content.Config = store.ProfileConfig{Language: string(i18n.English), Timezone: "UTC", Welcome: store.WelcomeProfile{Enabled: true}}
	content.Rules = store.ProfileRules{Keywords: []string{"foo"}}
	payload, err := store.EncodeTemplateBundle(store.TemplateContent{Name: "alice", Version: 1, Content: content})
	if err != nil {
		t.Fatalf("EncodeTemplateBundle: %v", err)
	}
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/template import alice "+payload))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsText(mock, "Template alice saved")
	})
	got, _, ok := b.store.Template(500, "alice")
	if !ok {
		t.Fatal("valid import did not store the template")
	}
	if len(got.Rules.Keywords) != 1 || got.Rules.Keywords[0] != "foo" {
		t.Errorf("imported template keywords = %v, want [foo]", got.Rules.Keywords)
	}
}

// TestTemplateSaveExportImportRoundtrip covers saving the chat's effective
// configuration as a template, exporting it as a versioned bundle, and
// importing it back under a new name; applying the imported template then
// reconfigures the chat.
func TestTemplateSaveExportImportRoundtrip(t *testing.T) {
	b, mock := newCloneTestBot(t)
	client := b.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.StartWebhook(ctx)
	mock.setMemberStatusForChat(500, 7, "administrator")

	content := store.ProfileContent{
		Config: store.ProfileConfig{
			Language: string(i18n.English),
			Timezone: "Asia/Tokyo",
			Welcome:  store.WelcomeProfile{Enabled: true, Message: "hello"},
		},
		Rules: store.ProfileRules{Keywords: []string{"foo"}},
	}
	if _, err := b.store.ApplyProfile(500, content); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/template save mine"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsText(mock, "Template mine saved")
	})

	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/template export mine"))
	var exported string
	waitFor(t, 2*time.Second, func() bool {
		for _, s := range sentTexts(mock) {
			if strings.HasPrefix(s, "{\"format\":1") {
				exported = s
				return true
			}
		}
		return false
	})
	var bundle store.TemplateBundle
	if err := json.Unmarshal([]byte(exported), &bundle); err != nil {
		t.Fatalf("exported bundle is not valid JSON: %v", err)
	}
	if len(bundle.Template.Content.Rules.Keywords) != 1 || bundle.Template.Content.Rules.Keywords[0] != "foo" {
		t.Errorf("exported template keywords = %v, want [foo]", bundle.Template.Content.Rules.Keywords)
	}

	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/template import mine2 "+exported))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsText(mock, "Template mine2 saved")
	})

	// Applying the imported template keeps enforcing the same configuration
	// and stamps a new revision on the chat profile.
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/template apply mine2"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsText(mock, "Template mine2 applied")
	})
	if got := b.store.Timezone(500); got != "Asia/Tokyo" {
		t.Errorf("Timezone after apply = %q, want Asia/Tokyo", got)
	}
}

// TestTemplateApplyNotFoundAndUsage covers unknown templates and missing
// arguments with explicit messages and no state change.
func TestTemplateApplyNotFoundAndUsage(t *testing.T) {
	b, mock := newCloneTestBot(t)
	client := b.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.StartWebhook(ctx)
	mock.setMemberStatusForChat(500, 7, "administrator")

	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/template"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsText(mock, "Template commands:")
	})
	if _, _, ok := b.store.Template(500, "nope"); ok {
		t.Fatal("usage response stored a template")
	}

	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/template apply nope"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsText(mock, "No template named nope")
	})
	if _, ok := b.store.Profile(500); ok {
		t.Fatal("apply of an unknown template stored a profile")
	}
}
