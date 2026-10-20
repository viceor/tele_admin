package governance

import (
	"context"
	"errors"
	"testing"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/viceor/tele-admin/internal/store"
)

func TestRuleEngineBlocksTerm(t *testing.T) {
	re := NewRuleEngine([]string{"spam", "Casino"})
	if blocked, term := re.Check("visit my CASINO now"); !blocked || term != "casino" {
		t.Errorf("expected casino block, got blocked=%v term=%q", blocked, term)
	}
	if blocked, _ := re.Check("hello world"); blocked {
		t.Error("clean message should not be blocked")
	}
}

func TestRuleEngineEmpty(t *testing.T) {
	re := NewRuleEngine(nil)
	if blocked, _ := re.Check("anything"); blocked {
		t.Error("empty engine should not block")
	}
}

type mockClient struct {
	member *models.ChatMember
	err    error
}

func (m *mockClient) GetChatMember(_ context.Context, _ *tgbot.GetChatMemberParams) (*models.ChatMember, error) {
	return m.member, m.err
}
func (m *mockClient) SendMessage(_ context.Context, _ *tgbot.SendMessageParams) (*models.Message, error) {
	return &models.Message{}, nil
}
func (m *mockClient) DeleteMessage(_ context.Context, _ *tgbot.DeleteMessageParams) (bool, error) {
	return true, nil
}

func memberWithStatus(t *testing.T, status models.ChatMemberType) *models.ChatMember {
	t.Helper()
	m := &models.ChatMember{Type: status}
	switch status {
	case models.ChatMemberTypeOwner:
		m.Owner = &models.ChatMemberOwner{}
	case models.ChatMemberTypeAdministrator:
		m.Administrator = &models.ChatMemberAdministrator{}
	default:
		m.Member = &models.ChatMemberMember{}
	}
	return m
}

func TestIsAdminOwner(t *testing.T) {
	c := &mockClient{member: memberWithStatus(t, models.ChatMemberTypeOwner)}
	ac := NewAdminChecker(c, true, nil)
	ok, err := ac.IsAdmin(context.Background(), 1, 2)
	if err != nil || !ok {
		t.Errorf("owner should be admin, ok=%v err=%v", ok, err)
	}
}

func TestIsAdminAdministrator(t *testing.T) {
	c := &mockClient{member: memberWithStatus(t, models.ChatMemberTypeAdministrator)}
	ac := NewAdminChecker(c, true, nil)
	ok, err := ac.IsAdmin(context.Background(), 1, 2)
	if err != nil || !ok {
		t.Errorf("administrator should be admin, ok=%v err=%v", ok, err)
	}
}

func TestIsAdminMemberDenied(t *testing.T) {
	c := &mockClient{member: memberWithStatus(t, models.ChatMemberTypeMember)}
	ac := NewAdminChecker(c, true, nil)
	ok, err := ac.IsAdmin(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Error("plain member should not be admin")
	}
}

func TestIsAdminEnforceDisabled(t *testing.T) {
	c := &mockClient{member: memberWithStatus(t, models.ChatMemberTypeMember)}
	ac := NewAdminChecker(c, false, nil)
	ok, err := ac.IsAdmin(context.Background(), 1, 2)
	if err != nil || !ok {
		t.Errorf("disabled enforcement should allow all, ok=%v err=%v", ok, err)
	}
}

func TestIsAdminConfiguredOwnerAllowed(t *testing.T) {
	c := &mockClient{member: memberWithStatus(t, models.ChatMemberTypeMember)}
	ac := NewAdminChecker(c, true, []int64{42})
	ok, err := ac.IsAdmin(context.Background(), 1, 42)
	if err != nil || !ok {
		t.Errorf("configured owner should be admin, ok=%v err=%v", ok, err)
	}
	if ok, _ := ac.IsAdmin(context.Background(), 1, 7); ok {
		t.Error("non-owner member should not be admin")
	}
}

func TestRuleEngineIgnoresBlankTerms(t *testing.T) {
	re := NewRuleEngine([]string{"", "  ", "Casino"})
	if blocked, _ := re.Check("nothing here"); blocked {
		t.Error("blank terms must not match every message")
	}
	if blocked, term := re.Check("CASINO night"); !blocked || term != "casino" {
		t.Errorf("expected casino block, got blocked=%v term=%q", blocked, term)
	}
}

func TestIsAdminClientError(t *testing.T) {
	c := &mockClient{err: errors.New("network down")}
	ac := NewAdminChecker(c, true, nil)
	if _, err := ac.IsAdmin(context.Background(), 1, 2); err == nil {
		t.Error("expected error to propagate")
	}
}

// A nil member returned with a nil error must be rejected instead of
// dereferenced (MYC-54 #1): authorization fails closed with an error.
func TestIsAdminNilMemberRejected(t *testing.T) {
	c := &mockClient{} // member=nil, err=nil
	ac := NewAdminChecker(c, true, nil)
	ok, err := ac.IsAdmin(context.Background(), 1, 2)
	if err == nil {
		t.Fatal("expected an error when getChatMember returns nil with no error")
	}
	if ok {
		t.Error("a nil member result must not authorize the caller")
	}
}

func TestIsOwner(t *testing.T) {
	ac := NewAdminChecker(&mockClient{}, true, []int64{42})
	if !ac.IsOwner(42) {
		t.Error("42 should be owner")
	}
	if ac.IsOwner(7) {
		t.Error("7 should not be owner")
	}
}

func TestIsAdminOwnerShortCircuitsFailingApi(t *testing.T) {
	c := &mockClient{err: errors.New("network down")}
	ac := NewAdminChecker(c, true, []int64{42})
	ok, err := ac.IsAdmin(context.Background(), 1, 42)
	if err != nil || !ok {
		t.Errorf("owner must be authorized even when getChatMember fails, ok=%v err=%v", ok, err)
	}
}

func TestIsAdminNonOwnerDeniedWhenApiFails(t *testing.T) {
	c := &mockClient{err: errors.New("network down")}
	ac := NewAdminChecker(c, true, []int64{42})
	ok, err := ac.IsAdmin(context.Background(), 1, 7)
	if err == nil {
		t.Fatal("expected error to propagate for non-owner")
	}
	if ok {
		t.Error("non-owner must not be authorized when the admin check fails")
	}
}

func TestSpamChecker(t *testing.T) {
	st := store.New(2, 0)
	sc := NewSpamChecker(st)
	now := time.Now()
	if ok, _ := sc.Check(1, now); !ok {
		t.Fatal("first should be allowed")
	}
	if ok, _ := sc.Check(1, now.Add(time.Second)); !ok {
		t.Fatal("second should be allowed")
	}
	if ok, _ := sc.Check(1, now.Add(2*time.Second)); ok {
		t.Error("third should be blocked")
	}
}

func TestSpamCheckerIsolatesUsers(t *testing.T) {
	st := store.New(1, 0)
	sc := NewSpamChecker(st)
	now := time.Now()
	if ok, _ := sc.Check(1, now); !ok {
		t.Fatal("user1 first should be allowed")
	}
	if ok, _ := sc.Check(2, now); !ok {
		t.Error("user2 should be independent of user1")
	}
}
