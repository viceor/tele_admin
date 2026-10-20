package bot

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/viceor/tele-admin/internal/store"
)

// persistingBot bundles a bot with file persistence enabled plus its mock
// Telegram server, for MYC-73 failure-path tests.
type persistingBot struct {
	b      *Bot
	mock   *mockTelegram
	dbPath string
}

func newPersistingBot(t *testing.T) *persistingBot {
	t.Helper()
	mock := newMockTelegram(t)
	mock.setMemberStatus("administrator")
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	b, err := New(testConfigWithDB(srv.URL, dbPath), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.client.StartWebhook(ctx)
	return &persistingBot{b: b, mock: mock, dbPath: dbPath}
}

// blockCommit replaces the database file with a directory so every FileStore
// commit fails (rename over a directory is refused even for root), while the
// bot itself keeps running on its in-memory snapshot.
func (p *persistingBot) blockCommit(t *testing.T) {
	t.Helper()
	if err := os.Remove(p.dbPath); err != nil {
		t.Fatalf("remove database file: %v", err)
	}
	if err := os.Mkdir(p.dbPath, 0o700); err != nil {
		t.Fatalf("create blocking directory: %v", err)
	}
}

func (p *persistingBot) unblockCommit(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(p.dbPath); err != nil {
		t.Fatalf("remove blocking directory: %v", err)
	}
}

// replies contains every text the mock received for a chat.
func (p *persistingBot) replies(chatID int64) []string {
	p.mock.mu.Lock()
	defer p.mock.mu.Unlock()
	var out []string
	for _, m := range p.mock.sentMessages {
		s, _ := m["chat_id"].(string)
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id != chatID {
			continue
		}
		out = append(out, m["text"].(string))
	}
	return out
}

// waitReplies waits until a reply for chatID contains want.
func (p *persistingBot) waitReplies(t *testing.T, chatID int64, want string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, text := range p.replies(chatID) {
			if strings.Contains(text, want) {
				return text
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no reply containing %q within 2s; replies so far: %v", want, p.replies(chatID))
	return ""
}

// assertReplyFailsPersistence checks the reply names the persistence failure
// and never reads as the success text.
func (p *persistingBot) assertReplyFailsPersistence(t *testing.T, chatID int64, want string, notWants ...string) {
	t.Helper()
	text := p.waitReplies(t, chatID, want)
	for _, notWanted := range notWants {
		if strings.Contains(text, notWanted) {
			t.Errorf("reply %q must not contain %q", text, notWanted)
		}
	}
}

// TestLangTimezoneNotSetWhenPersistFails covers MYC-51 #3: with the database
// blocked, /lang and /tz must answer "not saved" and never confirm the set.
func TestLangTimezoneNotSetWhenPersistFails(t *testing.T) {
	p := newPersistingBot(t)
	p.blockCommit(t)
	defer p.unblockCommit(t)

	postUpdate(t, p.b.client.WebhookHandler(), textMessageUpdate(500, 7, "/lang zh"))
	p.assertReplyFailsPersistence(t, 500, "Persistence failed", "Language set", "已设置")
	if got := p.b.store.Language(500); got == "zh" {
		t.Error("language memory state shows the failed change; it must not lead the disk")
	}

	postUpdate(t, p.b.client.WebhookHandler(), textMessageUpdate(500, 7, "/tz Asia/Shanghai"))
	p.assertReplyFailsPersistence(t, 500, "Persistence failed", "Timezone set", "已设置")
}

// TestCardCreateAndUpdateNotAppliedWhenPersistFails covers MYC-62 P1: with the
// database blocked, both card mutations answer with an explicit failure.
func TestCardCreateAndUpdateNotAppliedWhenPersistFails(t *testing.T) {
	p := newPersistingBot(t)
	if err := p.b.store.CreateCard(500, 7, store.CardFields{Name: "Alice"}); err != nil {
		t.Fatalf("seed CreateCard: %v", err)
	}
	p.blockCommit(t)
	defer p.unblockCommit(t)

	postUpdate(t, p.b.client.WebhookHandler(), textMessageUpdate(500, 8, "/card create name=Bob"))
	p.assertReplyFailsPersistence(t, 500, "card action failed", "Card created", "已创建")

	postUpdate(t, p.b.client.WebhookHandler(), textMessageUpdate(500, 7, "/card edit bio=changed"))
	p.assertReplyFailsPersistence(t, 500, "card action failed", "Card updated", "已更新")
}

// TestPublishRegistryNotAppliedWhenPersistFails covers MYC-58 P1: with the
// database blocked, /publish_add and /publish_remove answer with an explicit
// failure instead of a registration confirmation.
func TestPublishRegistryNotAppliedWhenPersistFails(t *testing.T) {
	p := newPersistingBot(t)
	// Block first: /publish_add on an unregistered chat must fail with the
	// persistence message, not the already-registered one.
	p.blockCommit(t)
	postUpdate(t, p.b.client.WebhookHandler(), textMessageUpdate(500, 7, "/publish_add"))
	p.assertReplyFailsPersistence(t, 500, "Persistence failed", "registered as a publish target", "已注册")
	p.unblockCommit(t)

	// Register for real, block again, then remove must fail with the
	// persistence message rather than the removed confirmation.
	postUpdate(t, p.b.client.WebhookHandler(), textMessageUpdate(500, 7, "/publish_add"))
	p.waitReplies(t, 500, "registered as a publish target")
	p.blockCommit(t)
	defer p.unblockCommit(t)
	postUpdate(t, p.b.client.WebhookHandler(), textMessageUpdate(500, 7, "/publish_remove"))
	p.assertReplyFailsPersistence(t, 500, "Persistence failed", "no longer a publish target", "已不再是")
}

// TestPublishSentButNotRecorded covers MYC-73's publish rule: messages are
// already delivered when the record write fails, so the user must see an
// explicit "sent but not recorded" answer and never "Publish succeeded".
func TestPublishSentButNotRecorded(t *testing.T) {
	p := newPersistingBot(t)
	if err := p.b.store.AddPublishTarget(600, 7); err != nil {
		t.Fatalf("AddPublishTarget 600: %v", err)
	}
	p.blockCommit(t)
	defer p.unblockCommit(t)

	postUpdate(t, p.b.client.WebhookHandler(), textMessageUpdate(500, 7, "/publish text 600 hello from the manager"))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		text := p.waitReplies(t, 500, "not recorded")
		if strings.Contains(text, "were sent") {
			// success and not-recorded conclusions
			if strings.Contains(text, "Publish succeeded") {
				t.Errorf("reply %q must not claim the publish succeeded", text)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no sent-but-not-recorded reply within 2s")
}

// TestTemplateSaveImportNotSavedWhenPersistFails covers MYC-66 P2: with the
// database blocked, /template save and /template import answer with an
// explicit persistence failure instead of "saved (revision n)".
func TestTemplateSaveImportNotSavedWhenPersistFails(t *testing.T) {
	p := newPersistingBot(t)
	p.blockCommit(t)
	defer p.unblockCommit(t)

	postUpdate(t, p.b.client.WebhookHandler(), textMessageUpdate(500, 7, "/template save cfg"))
	p.assertReplyFailsPersistence(t, 500, "Persistence failed", "saved (revision", "已保存")

	payload, err := store.EncodeTemplateBundle(store.TemplateContent{
		Name:    "cfg2",
		Version: 1,
		Content: store.ProfileContent{Config: store.ProfileConfig{Language: "en", Timezone: "UTC"}},
	})
	if err != nil {
		t.Fatalf("EncodeTemplateBundle: %v", err)
	}
	postUpdate(t, p.b.client.WebhookHandler(), textMessageUpdate(500, 7, "/template import cfg2 "+payload))
	p.assertReplyFailsPersistence(t, 500, "Persistence failed", "saved (revision", "已保存")
}
