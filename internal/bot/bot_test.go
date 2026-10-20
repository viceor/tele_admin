package bot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/viceor/tele-admin/internal/config"
)

// mockTelegram is a fake Telegram Bot API server used to exercise the bot
// without real credentials.
type mockTelegram struct {
	mu                sync.Mutex
	sentMessages      []map[string]any
	deletedMessages   []int
	getMeCalls        int
	chatMemberStatus  string
	memberErr         error
	sendErr           error
	setWebhookErr     error
	setWebhookCalls   int
	lastWebhookURL    string
	lastWebhookSecret string
	// memberStatusByID overrides chatMemberStatus for specific user IDs.
	memberStatusByID map[int64]string
	// memberStatusByChatUser overrides chatMemberStatus for one (chat, user)
	// pair; used when the same user must differ across chats.
	memberStatusByChatUser map[memberRef]string
	// memberErrByID makes getChatMember fail for specific user IDs.
	memberErrByID map[int64]error
	// memberRestrictedByUserID carries is_member/can_send_messages details
	// returned with a "restricted" status for specific user IDs.
	memberRestrictedByUserID map[int64]memberRestricted
	// sendErrByChat fails sendMessage for a specific target chat, on top of
	// the global sendErr.
	sendErrByChat map[int64]error
	// restrictCalls records restrictChatMember requests (chat, user, all-denied).
	restrictCalls []restrictCall
	restrictErr   error
	restrictOK    *bool // nil means ok=true
	// mediaCalls records sendPhoto/sendVideo/sendDocument requests.
	mediaCalls []mediaCall
	// mediaErrByKind fails a specific media kind ("photo", "video", "document").
	mediaErrByKind map[string]error
	// chatPerm is the can_send_messages value returned by getChat.
	chatPerm   bool
	getChatErr error
	// chatTypeByChatID overrides the chat type returned by getChat for one
	// chat (publish probes must tell channels apart from groups).
	chatTypeByChatID map[int64]string
	// adminCanPostByUserID carries the can_post_messages flag returned with
	// an "administrator" member status for specific user IDs. Absent by
	// default, as the Bot API reports the flag for channels only.
	adminCanPostByUserID map[int64]bool
	// sendDelayByChat delays the sendMessage response for one chat, applied
	// without holding the mock lock so concurrent requests proceed; used to
	// simulate a slow target.
	sendDelayByChat map[int64]time.Duration
	// memberDelayByUserID delays the getChatMember response for one user,
	// applied without holding the mock lock.
	memberDelayByUserID map[int64]time.Duration
}

type restrictCall struct {
	chat, user int64
	allowAll   bool
}

// mediaCall is one recorded media send: kind ("photo"/"video"/"document"),
// target chat, and the media reference plus caption from the form fields.
type mediaCall struct {
	kind    string
	chat    int64
	ref     string
	caption string
}

// memberRef keys one (chat, user) pair for chat-scoped member statuses.
type memberRef struct {
	chat int64
	user int64
}

// memberRestricted is the restricted-member payload returned by getChatMember
// for one user.
type memberRestricted struct {
	isMember        bool
	canSendMessages bool
}

func newMockTelegram(t *testing.T) *mockTelegram {
	t.Helper()
	m := &mockTelegram{chatMemberStatus: "member"}
	// The bot's own user ID is 1 (returned by getMe); it is an administrator
	// of test chats by default so publish permission probes succeed, unless a
	// test overrides the bot's status.
	m.setMemberStatusFor(1, "administrator")
	return m
}

func okResult(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (m *mockTelegram) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/bot")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		method := parts[1]

		// The go-telegram client encodes parameters as multipart/form-data,
		// not JSON, so the mock must read form values.
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			_ = r.ParseForm()
		}

		m.mu.Lock()
		defer m.mu.Unlock()
		switch method {
		case "getMe":
			m.getMeCalls++
			okResult(w, map[string]any{"id": 1, "is_bot": true, "first_name": "tele-admin"})
		case "sendMessage":
			chatIDStr := r.FormValue("chat_id")
			chatIDNum, _ := strconv.ParseInt(chatIDStr, 10, 64)
			if d := m.sendDelayByChat[chatIDNum]; d > 0 {
				// Simulate a slow target without holding the mock lock so
				// sends to other chats proceed concurrently; the request
				// context is done as soon as the client gives up (publish
				// per-target deadline), which also keeps server shutdown
				// from waiting out the delay.
				m.mu.Unlock()
				select {
				case <-time.After(d):
				case <-r.Context().Done():
				}
				m.mu.Lock()
			}
			if err, ok := m.sendErrByChat[chatIDNum]; ok {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": err.Error()})
				return
			}
			if m.sendErr != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": m.sendErr.Error()})
				return
			}
			chatID := r.FormValue("chat_id")
			text := r.FormValue("text")
			m.sentMessages = append(m.sentMessages, map[string]any{"chat_id": chatID, "text": text})
			// chat.id must be numeric so the client can unmarshal it into
			// models.Chat; a string would make SendMessage return an error.
			id, _ := strconv.ParseInt(chatID, 10, 64)
			okResult(w, map[string]any{"message_id": len(m.sentMessages), "chat": map[string]any{"id": id, "type": "group"}, "text": text})
		case "setWebhook":
			m.setWebhookCalls++
			m.lastWebhookURL = r.FormValue("url")
			m.lastWebhookSecret = r.FormValue("secret_token")
			if m.setWebhookErr != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": m.setWebhookErr.Error()})
				return
			}
			okResult(w, true)
		case "deleteMessage":
			msgID, _ := strconv.Atoi(r.FormValue("message_id"))
			m.deletedMessages = append(m.deletedMessages, msgID)
			okResult(w, true)
		case "getChatMember":
			if m.memberErr != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": m.memberErr.Error()})
				return
			}
			uid, _ := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
			if err, ok := m.memberErrByID[uid]; ok {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": err.Error()})
				return
			}
			if d := m.memberDelayByUserID[uid]; d > 0 {
				// Sleep without holding the mock lock; the request context
				// fires when the client gives up.
				m.mu.Unlock()
				select {
				case <-time.After(d):
				case <-r.Context().Done():
				}
				m.mu.Lock()
			}
			status := m.chatMemberStatus
			if s, ok := m.memberStatusByID[uid]; ok {
				status = s
			}
			chatID, _ := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
			if s, ok := m.memberStatusByChatUser[memberRef{chat: chatID, user: uid}]; ok {
				status = s
			}
			result := map[string]any{
				"status": status,
				"user":   map[string]any{"id": uid, "first_name": "member"},
			}
			if status == "administrator" {
				if canPost, ok := m.adminCanPostByUserID[uid]; ok {
					result["can_post_messages"] = canPost
				}
			}
			if status == "restricted" {
				extra := memberRestricted{}
				if e, ok := m.memberRestrictedByUserID[uid]; ok {
					extra = e
				}
				result["is_member"] = extra.isMember
				result["can_send_messages"] = extra.canSendMessages
			}
			okResult(w, result)
		case "getChat":
			if m.getChatErr != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": m.getChatErr.Error()})
				return
			}
			id, _ := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
			chatType := "supergroup"
			if s, ok := m.chatTypeByChatID[id]; ok {
				chatType = s
			}
			okResult(w, map[string]any{
				"id":          id,
				"type":        chatType,
				"title":       "group",
				"permissions": map[string]any{"can_send_messages": m.chatPerm},
			})
		case "sendPhoto", "sendVideo", "sendDocument":
			kind := strings.ToLower(strings.TrimPrefix(method, "send"))
			if err, ok := m.mediaErrByKind[kind]; ok {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": err.Error()})
				return
			}
			chatVal, _ := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
			call := mediaCall{
				kind:    kind,
				chat:    chatVal,
				ref:     r.FormValue(kind),
				caption: r.FormValue("caption"),
			}
			m.mediaCalls = append(m.mediaCalls, call)
			okResult(w, map[string]any{"message_id": len(m.mediaCalls), "chat": map[string]any{"id": chatVal, "type": "supergroup"}})
		case "restrictChatMember":
			if m.restrictErr != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": m.restrictErr.Error()})
				return
			}
			chat, _ := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
			uid, _ := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
			allowAll := strings.Contains(r.FormValue("permissions"), `"can_send_messages":true`)
			m.restrictCalls = append(m.restrictCalls, restrictCall{chat: chat, user: uid, allowAll: allowAll})
			ok := true
			if m.restrictOK != nil {
				ok = *m.restrictOK
			}
			okResult(w, ok)
		default:
			okResult(w, map[string]any{})
		}
	}
}

func (m *mockTelegram) sent() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]map[string]any, len(m.sentMessages))
	copy(out, m.sentMessages)
	return out
}

func (m *mockTelegram) deleted() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]int, len(m.deletedMessages))
	copy(out, m.deletedMessages)
	return out
}

func (m *mockTelegram) setMemberStatus(status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chatMemberStatus = status
}

func (m *mockTelegram) setMemberErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.memberErr = err
}

func (m *mockTelegram) setMemberStatusFor(userID int64, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.memberStatusByID == nil {
		m.memberStatusByID = map[int64]string{}
	}
	m.memberStatusByID[userID] = status
}

// setMemberStatusForChat overrides the member status of one (chat, user)
// pair; the chat-scoped value wins over per-user and global statuses.
func (m *mockTelegram) setMemberStatusForChat(chatID, userID int64, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.memberStatusByChatUser == nil {
		m.memberStatusByChatUser = map[memberRef]string{}
	}
	m.memberStatusByChatUser[memberRef{chat: chatID, user: userID}] = status
}

func (m *mockTelegram) setMemberErrFor(userID int64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.memberErrByID == nil {
		m.memberErrByID = map[int64]error{}
	}
	m.memberErrByID[userID] = err
}

// setRestrictedFor makes getChatMember report a "restricted" status for the
// user with explicit is_member and can_send_messages flags.
func (m *mockTelegram) setRestrictedFor(userID int64, isMember, canSend bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.memberStatusByID == nil {
		m.memberStatusByID = map[int64]string{}
	}
	if m.memberRestrictedByUserID == nil {
		m.memberRestrictedByUserID = map[int64]memberRestricted{}
	}
	m.memberStatusByID[userID] = "restricted"
	m.memberRestrictedByUserID[userID] = memberRestricted{isMember: isMember, canSendMessages: canSend}
}

// setSendErrForChat fails sendMessage for one chat only.
func (m *mockTelegram) setSendErrForChat(chatID int64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendErrByChat == nil {
		m.sendErrByChat = map[int64]error{}
	}
	m.sendErrByChat[chatID] = err
}

func (m *mockTelegram) setRestrictErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restrictErr = err
}

func (m *mockTelegram) setRestrictOK(ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restrictOK = &ok
}

func (m *mockTelegram) restricts() []restrictCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]restrictCall, len(m.restrictCalls))
	copy(out, m.restrictCalls)
	return out
}

func (m *mockTelegram) setSendErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendErr = err
}

func (m *mockTelegram) setSetWebhookErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setWebhookErr = err
}

func (m *mockTelegram) webhookRegistration() (calls int, url, secret string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.setWebhookCalls, m.lastWebhookURL, m.lastWebhookSecret
}

func (m *mockTelegram) media() []mediaCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mediaCall, len(m.mediaCalls))
	copy(out, m.mediaCalls)
	return out
}

// setMediaErr fails a specific media kind with a Telegram-style error.
func (m *mockTelegram) setMediaErr(kind string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mediaErrByKind == nil {
		m.mediaErrByKind = map[string]error{}
	}
	m.mediaErrByKind[kind] = err
}

func (m *mockTelegram) setChatPerm(canSend bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chatPerm = canSend
}

// setChatTypeFor overrides the chat type returned by getChat for one chat.
func (m *mockTelegram) setChatTypeFor(chatID int64, chatType string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.chatTypeByChatID == nil {
		m.chatTypeByChatID = map[int64]string{}
	}
	m.chatTypeByChatID[chatID] = chatType
}

// setAdminCanPostFor sets the can_post_messages flag returned for an
// administrator member lookup of one user.
func (m *mockTelegram) setAdminCanPostFor(userID int64, canPost bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.adminCanPostByUserID == nil {
		m.adminCanPostByUserID = map[int64]bool{}
	}
	m.adminCanPostByUserID[userID] = canPost
}

// setSendDelayFor delays sendMessage responses for one chat, without holding
// the mock lock while sleeping.
func (m *mockTelegram) setSendDelayFor(chatID int64, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendDelayByChat == nil {
		m.sendDelayByChat = map[int64]time.Duration{}
	}
	m.sendDelayByChat[chatID] = d
}

// setMemberDelayFor delays getChatMember responses for one user, without
// holding the mock lock while sleeping.
func (m *mockTelegram) setMemberDelayFor(userID int64, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.memberDelayByUserID == nil {
		m.memberDelayByUserID = map[int64]time.Duration{}
	}
	m.memberDelayByUserID[userID] = d
}

func (m *mockTelegram) setGetChatErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getChatErr = err
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

func testConfig(serverURL string) *config.Config {
	return &config.Config{
		TelegramToken: "test-token",
		APIServerURL:  serverURL,
		Mode:          "webhook",
		LogLevel:      "error",
		Webhook: config.WebhookConfig{
			Listen:      "127.0.0.1:18443",
			Path:        "/webhook",
			PublicURL:   "https://example.com/webhook",
			SecretToken: "test-secret",
		},
		I18N:     config.I18NConfig{DefaultLanguage: "en", Supported: []string{"en", "zh"}},
		Timezone: config.TimezoneConfig{Default: "UTC"},
		Admin:    config.AdminConfig{EnforceAdmin: true},
		Spam:     config.SpamConfig{MaxMessagesPerMinute: 20, MinMessageIntervalMs: 0},
		Welcome:  config.WelcomeConfig{Enabled: true},
		Keywords: config.KeywordsConfig{Blocked: []string{"spam"}},
	}
}

func postUpdate(t *testing.T, h http.HandlerFunc, updateJSON string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(updateJSON))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "test-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook handler status = %d", rec.Code)
	}
}

func textMessageUpdate(chatID int64, userID int64, text string) string {
	message := map[string]any{
		"message_id": 10,
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

// commandToken returns the leading /command token when text is a bot command,
// mirroring the bot_command entity Telegram attaches to command messages.
func commandToken(text string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	token := text
	if i := strings.IndexAny(text, " \t\n"); i >= 0 {
		token = text[:i]
	}
	if idx := strings.IndexByte(token, '@'); idx >= 0 {
		token = token[:idx]
	}
	if len(token) < 2 {
		return "", false
	}
	return token, true
}

func joinUpdate(chatID int64, newID int64, name string) string {
	return toJSON(map[string]any{
		"update_id": 2,
		"message": map[string]any{
			"message_id": 11,
			"from":       map[string]any{"id": 999, "first_name": "inviter"},
			"chat":       map[string]any{"id": chatID},
			"new_chat_members": []map[string]any{
				{"id": newID, "first_name": name, "is_bot": false},
			},
		},
	})
}

func toJSON(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestWebhookStartCommand(t *testing.T) {
	mock := newMockTelegram(t)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	b, err := New(testConfig(srv.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := b.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.StartWebhook(ctx)

	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/start"))

	waitFor(t, 2*time.Second, func() bool {
		for _, m := range mock.sent() {
			if strings.Contains(m["text"].(string), "help") || strings.Contains(m["text"].(string), "/start") {
				return true
			}
		}
		return false
	})
}

func TestWebhookWelcomeNewMember(t *testing.T) {
	mock := newMockTelegram(t)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	b, _ := New(testConfig(srv.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))
	client := b.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.StartWebhook(ctx)

	postUpdate(t, client.WebhookHandler(), joinUpdate(500, 42, "bob"))

	waitFor(t, 2*time.Second, func() bool {
		for _, m := range mock.sent() {
			if strings.Contains(m["text"].(string), "Welcome") {
				return true
			}
		}
		return false
	})
}

func TestWebhookBlockedKeywordDeletesMessage(t *testing.T) {
	mock := newMockTelegram(t)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	b, _ := New(testConfig(srv.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))
	client := b.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.StartWebhook(ctx)

	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "buy cheap spam now"))

	waitFor(t, 2*time.Second, func() bool {
		return len(mock.deleted()) > 0
	})
}

func TestWebhookLanguageSetAdmin(t *testing.T) {
	mock := newMockTelegram(t)
	mock.setMemberStatus("administrator")
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	b, _ := New(testConfig(srv.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))
	client := b.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.StartWebhook(ctx)

	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/lang zh"))

	waitFor(t, 2*time.Second, func() bool {
		for _, m := range mock.sent() {
			if strings.Contains(m["text"].(string), "Language set") || strings.Contains(m["text"].(string), "语言") {
				return true
			}
		}
		return false
	})
}

func TestWebhookLanguageDeniedForNonAdmin(t *testing.T) {
	mock := newMockTelegram(t)
	mock.setMemberStatus("member")
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	b, _ := New(testConfig(srv.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))
	client := b.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.StartWebhook(ctx)

	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "/lang zh"))

	waitFor(t, 2*time.Second, func() bool {
		for _, m := range mock.sent() {
			if strings.Contains(m["text"].(string), "administrator") {
				return true
			}
		}
		return false
	})
}

func TestWebhookSpamWarning(t *testing.T) {
	mock := newMockTelegram(t)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.Spam.MaxMessagesPerMinute = 1
	b, _ := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client := b.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.StartWebhook(ctx)

	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "hello"))
	postUpdate(t, client.WebhookHandler(), textMessageUpdate(500, 7, "world"))

	waitFor(t, 2*time.Second, func() bool {
		for _, m := range mock.sent() {
			if strings.Contains(m["text"].(string), "slow down") {
				return true
			}
		}
		return false
	})
}
