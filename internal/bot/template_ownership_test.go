package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/viceor/tele-admin/internal/i18n"
	"github.com/viceor/tele-admin/internal/store"
)

// sentTextsForChat returns every text the mock sent to one chat, so
// multi-chat tests can tell replies apart.
func sentTextsForChat(mock *mockTelegram, chatID int64) []string {
	want := strconv.FormatInt(chatID, 10)
	var out []string
	for _, m := range mock.sent() {
		if id, _ := m["chat_id"].(string); id == want {
			if s, ok := m["text"].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func sentContainsTextForChat(mock *mockTelegram, chatID int64, substr string) bool {
	for _, s := range sentTextsForChat(mock, chatID) {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// TestTemplateCrossChatAccessDenied covers MYC-67 M-1 and MYC-83 end to end: a
// template saved by chat 500 is invisible and unusable for chat 600. Export and
// apply answer exactly like an unknown template (no content leak, no state
// change). A re-save or import under the same name by chat 600 lands in chat
// 600's own namespace and never touches the owner's record. The owner chat
// keeps full access.
func TestTemplateCrossChatAccessDenied(t *testing.T) {
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
			Welcome:  store.WelcomeProfile{Enabled: true, Message: "chat 500 hello"},
		},
		Rules: store.ProfileRules{Keywords: []string{"rule500"}},
	}
	if _, err := b.store.ApplyProfile(500, content); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	// The owner chat saves the template.
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/template save shared"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, 500, "Template shared saved")
	})
	original, version, ok := b.store.Template(500, "shared")
	if !ok || version != 1 || original.Config.Welcome.Message != "chat 500 hello" {
		t.Fatalf("Template(500, shared) = (%+v, %d, %v), want version 1 with the chat 500 content", original, version, ok)
	}

	// Cross-chat export is refused like an unknown template: no bundle text
	// leaves the bot for chat 600.
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(600, 7, "/template export shared"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, 600, "No template named shared")
	})
	for _, s := range sentTextsForChat(mock, 600) {
		if strings.HasPrefix(s, `{"format":1`) {
			t.Fatalf("cross-chat export leaked a template bundle: %s", s)
		}
	}

	// Cross-chat apply is refused and stores nothing in chat 600.
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(600, 7, "/template apply shared"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, 600, "No template named shared")
	})
	if _, ok := b.store.Profile(600); ok {
		t.Fatal("cross-chat apply stored a profile in chat 600")
	}

	// Cross-chat re-save under the same name creates chat 600's own template
	// in its namespace (MYC-83). It does not touch the owner's record, and
	// the reply reveals nothing about chat 500's name.
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(600, 7, "/template save shared"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, 600, "Template shared saved")
	})
	after, afterVersion, ok := b.store.Template(500, "shared")
	if !ok || afterVersion != 1 || after.Config.Welcome.Message != original.Config.Welcome.Message {
		t.Errorf("template after foreign re-save = (%+v, %d, %v), want the owner's version 1 unchanged", after, afterVersion, ok)
	}
	own600, own600Version, ok := b.store.Template(600, "shared")
	if !ok || own600Version != 1 || own600.Config.Timezone != "UTC" {
		t.Errorf("chat 600 namespace after its save = (%+v, %d, %v), want its own version 1", own600, own600Version, ok)
	}

	// Cross-chat import under the same name lands in chat 600's namespace
	// with chat 600's content; chat 500's content stays as it was.
	hijack := store.ProfileContent{
		Config: store.ProfileConfig{
			Language: string(i18n.English),
			Timezone: "UTC",
			Welcome:  store.WelcomeProfile{Enabled: true, Message: "hijacked"},
		},
		Rules: store.ProfileRules{Keywords: []string{"rule600"}},
	}
	payload, err := store.EncodeTemplateBundle(store.TemplateContent{Name: "shared", Version: 1, Content: hijack})
	if err != nil {
		t.Fatalf("EncodeTemplateBundle: %v", err)
	}
	savedBeforeImport := countSent(mock, "Template shared saved")
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(600, 7, "/template import shared "+payload))
	waitFor(t, 2*time.Second, func() bool {
		return countSent(mock, "Template shared saved") > savedBeforeImport
	})
	afterImport, afterImportVersion, ok := b.store.Template(500, "shared")
	if !ok || afterImportVersion != 1 || afterImport.Config.Welcome.Message != original.Config.Welcome.Message {
		t.Errorf("template after foreign import = (%+v, %d, %v), want the owner's version 1 unchanged", afterImport, afterImportVersion, ok)
	}
	imported, importedVersion, ok := b.store.Template(600, "shared")
	if !ok || importedVersion != 2 || imported.Config.Welcome.Message != "hijacked" {
		t.Errorf("chat 600 namespace after import = (%+v, %d, %v), want the imported content at version 2", imported, importedVersion, ok)
	}

	// The owner chat can still export, apply, and overwrite its template.
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/template apply shared"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, 500, "Template shared applied")
	})
	if got := b.store.Timezone(500); got != "UTC" {
		t.Errorf("owner apply changed the timezone to %q, want UTC", got)
	}
}

// TestTemplateExportAuditLogged verifies the export audit trail (MYC-67):
// each successful export and each refusal through a foreign name is logged
// with the chat, name, actor, and revision.
func TestTemplateExportAuditLogged(t *testing.T) {
	var buf bytes.Buffer
	mock := newMockTelegram(t)
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	cfg := testConfig(srv.URL)
	b, err := New(cfg, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := b.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.StartWebhook(ctx)

	mock.setMemberStatusForChat(500, 7, "administrator")
	mock.setMemberStatusForChat(600, 7, "administrator")

	content := store.ProfileContent{
		Config: store.ProfileConfig{Language: string(i18n.English), Timezone: "UTC"},
		Rules:  store.ProfileRules{Keywords: []string{"rule500"}},
	}
	if _, err := b.store.ApplyProfile(500, content); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/template save shared"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, 500, "Template shared saved")
	})

	// A foreign chat's export attempt is refused and logged.
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(600, 7, "/template export shared"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, 600, "No template named shared")
	})

	// The owner's export succeeds and is logged.
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/template export shared"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, 500, `{"format":1`)
	})

	waitFor(t, 2*time.Second, func() bool {
		exported, refused := false, false
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			switch {
			case strings.Contains(line, `msg="template exported"`):
				if strings.Contains(line, "chat=500") && strings.Contains(line, "name=shared") &&
					strings.Contains(line, "actor=7") && strings.Contains(line, "version=1") {
					exported = true
				}
			case strings.Contains(line, `msg="template export refused: name not owned by this chat"`):
				if strings.Contains(line, "chat=600") && strings.Contains(line, "name=shared") &&
					strings.Contains(line, "actor=7") {
					refused = true
				}
			}
		}
		return exported && refused
	})
}

// TestCloneAndTemplateCarryCurrentLangTimezone covers MYC-66 P2: after a
// profile is applied and the chat then changes language and timezone with
// /lang and /tz, both /template save and /clone must capture the current
// effective values, not the stale ones recorded at apply time.
func TestCloneAndTemplateCarryCurrentLangTimezone(t *testing.T) {
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
			Welcome:  store.WelcomeProfile{Enabled: true, Message: "hello"},
		},
		Rules: store.ProfileRules{Keywords: []string{"foo"}},
	}
	if _, err := b.store.ApplyProfile(500, content); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	// The chat reconfigures itself after the profile was applied.
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/lang zh"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, 500, "语言已设置")
	})
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/tz Asia/Tokyo"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, 500, "时区已设置")
	})

	// The stored profile record itself keeps the values captured at apply
	// time; only the clone/save content must use the fresh ones.
	stale, ok := b.store.Profile(500)
	if !ok || stale.Config.Language != string(i18n.English) || stale.Config.Timezone != "UTC" {
		t.Errorf("stored profile = %+v, want the applied en/UTC values untouched", stale)
	}

	// /template save must capture zh and Asia/Tokyo.
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/template save fresh"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, 500, "已保存")
	})
	saved, version, ok := b.store.Template(500, "fresh")
	if !ok || version != 1 {
		t.Fatalf("Template(500, fresh) = (%v, %d, %v), want version 1", saved, version, ok)
	}
	if saved.Config.Language != string(i18n.Chinese) || saved.Config.Timezone != "Asia/Tokyo" {
		t.Errorf("saved template config = %+v, want zh/Asia-Tokyo", saved.Config)
	}

	// Exporting the saved template must carry the fresh values as well.
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/template export fresh"))
	var exported string
	waitFor(t, 2*time.Second, func() bool {
		for _, s := range sentTextsForChat(mock, 500) {
			if strings.HasPrefix(s, `{"format":1`) {
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
	if bundle.Template.Content.Config.Language != string(i18n.Chinese) || bundle.Template.Content.Config.Timezone != "Asia/Tokyo" {
		t.Errorf("exported template config = %+v, want zh/Asia-Tokyo", bundle.Template.Content.Config)
	}

	// /clone from chat 600 takes the fresh values too: they become chat 600's
	// effective language and timezone.
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(600, 7, "/clone 500"))
	waitFor(t, 2*time.Second, func() bool {
		return sentContainsTextForChat(mock, 600, "Configuration cloned from chat 500")
	})
	if got := b.store.Language(600); got != i18n.Chinese {
		t.Errorf("Language(600) after clone = %v, want Chinese", got)
	}
	if got := b.store.Timezone(600); got != "Asia/Tokyo" {
		t.Errorf("Timezone(600) after clone = %q, want Asia/Tokyo", got)
	}
}
