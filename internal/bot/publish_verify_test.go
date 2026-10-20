package bot

import (
	"strings"
	"testing"
	"time"
)

// Independent acceptance probes for slice S2 (multi-target publishing).
//
// These tests do not duplicate the author's suite: they assert the properties
// the acceptance criteria claim but the original tests only check indirectly,
// in particular that rejection paths deliver nothing at all and that send
// permission is probed per target rather than once per command.

func deliveriesToChat(mock *mockTelegram, chatID int64, exact string) int {
	n := 0
	for _, s := range mock.sent() {
		if s["text"] == exact && atoi(s["chat_id"]) == chatID {
			n++
		}
	}
	return n
}

func mediaToChat(mock *mockTelegram, chatID int64) []mediaCall {
	var out []mediaCall
	for _, c := range mock.media() {
		if c.chat == chatID {
			out = append(out, c)
		}
	}
	return out
}

// An explicit unregistered target must abort the whole command before the
// already-registered target receives anything.
func TestVerifyUnregisteredTargetAbortsBeforeDelivery(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600,999 vfy-unregistered"))

	waitForSent(t, mock, "not a registered publish target")
	assertDeliveredExactly(t, mock, "vfy-unregistered", 0)
	assertNoPublishText(t, mock)
}

// Exceeding the target cap must abort before any target receives content.
func TestVerifyTooManyTargetsAbortsBeforeDelivery(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)
	b.cfg.Publish.MaxTargets = 2
	if err := b.store.AddPublishTarget(800, publishAdmin); err != nil {
		t.Fatalf("AddPublishTarget: %v", err)
	}

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600,700,800 vfy-too-many"))

	waitForSent(t, mock, "Too many targets (3)")
	assertDeliveredExactly(t, mock, "vfy-too-many", 0)
	assertNoMediaSends(t, mock)
}

// A non-admin caller must not cause any delivery.
func TestVerifyNonAdminDeliversNothing(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setMemberStatusFor(publishMember, "member")
	})

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishMember, "/publish text 600,700 vfy-denied"))

	waitForSent(t, mock, "Only group administrators")
	assertDeliveredExactly(t, mock, "vfy-denied", 0)
	assertNoMediaSends(t, mock)
}

// An anonymous sender must not cause any delivery (including media).
func TestVerifyAnonymousDeliversNothing(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)

	upd := toJSON(map[string]any{
		"update_id": 91,
		"message": map[string]any{
			"message_id": 10,
			"chat":       map[string]any{"id": 500, "title": "group"},
			"text":       "/publish photo 600,700 BogusFileId vfy-anon-caption",
			"entities":   []map[string]any{{"type": "bot_command", "offset": 0, "length": 8}},
		},
	})
	postUpdate(t, b.Client().WebhookHandler(), upd)

	waitForSent(t, mock, "Only group administrators")
	assertNoMediaSends(t, mock)
	assertDeliveredExactly(t, mock, "vfy-anon-caption", 0)
}

// Send permission is probed per target: an allowed first target must not grant
// a second target that the bot cannot post to.
func TestVerifySendRightIsCheckedPerTarget(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setRestrictedFor(1, true, false)                // bot cannot send by default
		m.setMemberStatusForChat(600, 1, "administrator") // ... except in 600
	})

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600,700 vfy-per-target"))

	waitForDelivered(t, mock, "vfy-per-target", 1)
	assertDeliveredExactly(t, mock, "vfy-per-target", 1)
	if n := deliveriesToChat(mock, 700, "vfy-per-target"); n != 0 {
		t.Fatalf("target 700 received %d messages, want 0", n)
	}
	waitFor(t, 2*time.Second, func() bool {
		for _, s := range mock.sent() {
			text := s["text"].(string)
			if strings.Contains(text, "failures (1/2)") && strings.Contains(text, "\u2713 600") && strings.Contains(text, "\u2717 700: bot cannot send") {
				return true
			}
		}
		return false
	})
	recs := b.store.PublishRecords(0, 1)
	if len(recs) != 1 || len(recs[0].Results) != 2 {
		t.Fatalf("records = %+v", recs)
	}
	if !recs[0].Results[0].Success || recs[0].Results[1].Success || recs[0].Results[1].Reason != "no_send_right" {
		t.Fatalf("per-target results = %+v", recs[0].Results)
	}
}

// The same per-target permission rule applies to media sends.
func TestVerifyMediaSendRightIsCheckedPerTarget(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setRestrictedFor(1, true, false)
		m.setMemberStatusForChat(600, 1, "administrator")
	})

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish photo 600,700 VfyPhotoRef vfy-caption"))

	calls := waitForMedia(t, mock, 1)
	time.Sleep(150 * time.Millisecond)
	if len(mock.media()) != 1 {
		t.Fatalf("media sends = %+v, want exactly one (to 600)", mock.media())
	}
	if calls[0].chat != 600 || calls[0].kind != "photo" || calls[0].ref != "VfyPhotoRef" || calls[0].caption != "vfy-caption" {
		t.Fatalf("media call = %+v", calls[0])
	}
	if got := mediaToChat(mock, 700); len(got) != 0 {
		t.Fatalf("target 700 received media: %+v", got)
	}
	waitFor(t, 2*time.Second, func() bool {
		for _, s := range mock.sent() {
			if strings.Contains(s["text"].(string), "\u2717 700: bot cannot send") {
				return true
			}
		}
		return false
	})
}

// "all" with one failing target must also report a partial failure, never an
// overall success.
func TestVerifyAllPartialFailureNotSuccess(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, func(m *mockTelegram) {
		m.setSendErrForChat(700, errString("Bad Request: not enough rights"))
	})

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text all vfy-all-partial"))

	waitForDelivered(t, mock, "vfy-all-partial", 1)
	assertDeliveredExactly(t, mock, "vfy-all-partial", 1)
	var summary string
	waitFor(t, 2*time.Second, func() bool {
		for _, s := range mock.sent() {
			text := s["text"].(string)
			if strings.Contains(text, "\u2717") {
				summary = text
				return true
			}
		}
		return false
	})
	if !strings.Contains(summary, "failures (1/2)") || strings.Contains(summary, "Publish succeeded") {
		t.Fatalf("summary = %q", summary)
	}
}

// The target cap is inclusive: exactly max targets must be accepted.
func TestVerifyTargetLimitBoundaryIsInclusive(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)
	b.cfg.Publish.MaxTargets = 2

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600,700 vfy-at-limit"))

	waitForDelivered(t, mock, "vfy-at-limit", 2)
	assertDeliveredExactly(t, mock, "vfy-at-limit", 2)
}

// Repeating a target in the command must not duplicate the delivery.
func TestVerifyDuplicateTargetsDeliverOnce(t *testing.T) {
	b, mock := newPublishTestBot(t, nil, nil)

	postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600,600,600 vfy-dedup"))

	waitForDelivered(t, mock, "vfy-dedup", 1)
	time.Sleep(100 * time.Millisecond)
	assertDeliveredExactly(t, mock, "vfy-dedup", 1)
}

// Length limits count runes, not bytes.
func TestVerifyTextLimitCountsRunes(t *testing.T) {
	t.Run("at-limit", func(t *testing.T) {
		b, mock := newPublishTestBot(t, nil, nil)
		b.cfg.Publish.MaxContentLength = 3

		postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600 你好啊"))

		waitForDelivered(t, mock, "你好啊", 1)
	})
	t.Run("over-limit", func(t *testing.T) {
		b, mock := newPublishTestBot(t, nil, nil)
		b.cfg.Publish.MaxContentLength = 3

		postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish text 600 你好啊呀"))

		waitForSent(t, mock, "text is too long")
		assertDeliveredExactly(t, mock, "你好啊呀", 0)
	})
}

// Captions are capped at Telegram's 1024 characters, independent of the
// configured content limit.
func TestVerifyCaptionLimitBoundary(t *testing.T) {
	t.Run("1024 accepted", func(t *testing.T) {
		b, mock := newPublishTestBot(t, nil, nil)
		caption := strings.Repeat("c", 1024)

		postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish photo 600 VfyPhotoRef "+caption))

		calls := waitForMedia(t, mock, 1)
		if got := len([]rune(calls[0].caption)); got != 1024 {
			t.Fatalf("caption runes = %d, want 1024", got)
		}
	})
	t.Run("1025 rejected", func(t *testing.T) {
		b, mock := newPublishTestBot(t, nil, nil)
		caption := strings.Repeat("c", 1025)

		postUpdate(t, b.Client().WebhookHandler(), textMessageUpdate(500, publishAdmin, "/publish photo 600 VfyPhotoRef "+caption))

		waitForSent(t, mock, "caption is too long")
		assertNoMediaSends(t, mock)
	})
}
