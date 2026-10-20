package store

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/viceor/tele-admin/internal/i18n"
)

// validProfile returns structurally valid profile content for tests.
func validProfile() ProfileContent {
	return ProfileContent{
		Config: ProfileConfig{
			Language: string(i18n.English),
			Timezone: "UTC",
			Welcome:  WelcomeProfile{Enabled: true, Message: "hi {name}"},
		},
		Rules: ProfileRules{Keywords: []string{"spam"}},
	}
}

func TestValidateProfileAcceptsValidContent(t *testing.T) {
	if err := ValidateProfile(validProfile()); err != nil {
		t.Fatalf("ValidateProfile(valid) = %v, want nil", err)
	}
}

func TestValidateProfileRejectsInvalidStructures(t *testing.T) {
	cases := map[string]func(*ProfileContent){
		"unsupported language": func(p *ProfileContent) { p.Config.Language = "fr" },
		"empty language":       func(p *ProfileContent) { p.Config.Language = "" },
		"bad timezone":         func(p *ProfileContent) { p.Config.Timezone = "Mars/Refinery" },
		"welcome too long":     func(p *ProfileContent) { p.Config.Welcome.Message = string(make([]rune, profileWelcomeMaxLen+1)) },
		"too many keywords":    func(p *ProfileContent) { p.Rules.Keywords = make([]string, profileMaxKeywords+1) },
		"keyword too long":     func(p *ProfileContent) { p.Rules.Keywords = []string{string(make([]rune, profileMaxKeywordLen+1))} },
		"empty keyword":        func(p *ProfileContent) { p.Rules.Keywords = []string{""} },
	}
	for name, mutate := range cases {
		content := validProfile()
		mutate(&content)
		if err := ValidateProfile(content); err == nil {
			t.Errorf("ValidateProfile(%s) = nil, want error", name)
		}
	}
}

func TestApplyProfileStoresContentAndUpdatesSettings(t *testing.T) {
	s := New(20, 0)
	content := ProfileContent{
		Config: ProfileConfig{
			Language: string(i18n.Chinese),
			Timezone: "Asia/Shanghai",
			Welcome:  WelcomeProfile{Enabled: true, Message: "welcome {name}"},
		},
		Rules: ProfileRules{Keywords: []string{"foo"}},
	}
	version, err := s.ApplyProfile(500, content)
	if err != nil {
		t.Fatalf("ApplyProfile: %v", err)
	}
	if version != 1 {
		t.Errorf("first ApplyProfile version = %d, want 1", version)
	}
	got, ok := s.Profile(500)
	if !ok {
		t.Fatal("Profile(500) = false after apply")
	}
	if got.Config.Language != string(i18n.Chinese) || got.Config.Timezone != "Asia/Shanghai" {
		t.Errorf("applied config = %+v, want zh/Asia/Shanghai", got.Config)
	}
	if len(got.Rules.Keywords) != 1 || got.Rules.Keywords[0] != "foo" {
		t.Errorf("applied keywords = %v, want [foo]", got.Rules.Keywords)
	}
	if s.Language(500) != i18n.Chinese || s.Timezone(500) != "Asia/Shanghai" {
		t.Error("ApplyProfile did not update the chat settings")
	}
	// The clone is deep: mutating the returned keyword slice never reaches
	// the store.
	got.Rules.Keywords[0] = "changed"
	if again, _ := s.Profile(500); again.Rules.Keywords[0] != "foo" {
		t.Error("Profile returned a reference into stored state")
	}
}

func TestApplyProfileInvalidLeavesNothingBehind(t *testing.T) {
	s := New(20, 0)
	s.SetLanguage(500, i18n.English)
	invalid := validProfile()
	invalid.Config.Language = "xx"
	if _, err := s.ApplyProfile(500, invalid); err == nil {
		t.Fatal("ApplyProfile(invalid) = nil error, want error")
	}
	if _, ok := s.Profile(500); ok {
		t.Error("invalid apply stored a profile record; want none")
	}
	if got := s.Language(500); got != i18n.English {
		t.Errorf("Language after refused apply = %v, want English", got)
	}
}

func TestApplyProfileVersionIncrements(t *testing.T) {
	s := New(20, 0)
	v1, err := s.ApplyProfile(500, validProfile())
	if err != nil || v1 != 1 {
		t.Fatalf("first apply: version=%d err=%v", v1, err)
	}
	second := validProfile()
	second.Rules.Keywords = []string{"bar"}
	v2, err := s.ApplyProfile(500, second)
	if err != nil || v2 != 2 {
		t.Fatalf("second apply: version=%d err=%v", v2, err)
	}
	if kw, ok := s.Profile(500); !ok || len(kw.Rules.Keywords) != 1 || kw.Rules.Keywords[0] != "bar" {
		t.Errorf("Profile after second apply = %+v, want [bar]", kw.Rules.Keywords)
	}
}

func TestTemplateSaveLoadAndOverwrite(t *testing.T) {
	s := New(20, 0)
	if _, _, ok := s.Template(500, "alpha"); ok {
		t.Fatal("unknown template found")
	}
	v1, err := s.SaveTemplate(500, "alpha", validProfile())
	if err != nil || v1 != 1 {
		t.Fatalf("SaveTemplate: version=%d err=%v", v1, err)
	}
	updated := validProfile()
	updated.Config.Timezone = "Europe/Berlin"
	v2, err := s.SaveTemplate(500, "alpha", updated)
	if err != nil || v2 != 2 {
		t.Fatalf("SaveTemplate overwrite: version=%d err=%v", v2, err)
	}
	content, version, ok := s.Template(500, "alpha")
	if !ok || version != 2 {
		t.Fatalf("Template(alpha) = %v %d %v", content, version, ok)
	}
	if content.Config.Timezone != "Europe/Berlin" {
		t.Errorf("template content timezone = %q, want Europe/Berlin", content.Config.Timezone)
	}
}

// TestTemplateOwnershipEnforced covers MYC-67 M-1 at the store layer, under
// the per-chat namespace of MYC-83: a template is reachable only through its
// owner's namespace. Another chat sees not-found reads, and its save creates a
// separate template instead of overwriting or revealing the owner's.
func TestTemplateOwnershipEnforced(t *testing.T) {
	s := New(20, 0)
	if _, err := s.SaveTemplate(500, "alpha", validProfile()); err != nil {
		t.Fatalf("SaveTemplate: %v", err)
	}
	original, version, ok := s.Template(500, "alpha")
	if !ok || version != 1 {
		t.Fatalf("Template(alpha) = %v %d %v", original, version, ok)
	}

	// Another chat does not see the template, exactly like an unknown name.
	if _, _, ok := s.Template(600, "alpha"); ok {
		t.Error("Template from a foreign chat found the template; want not-found")
	}
	if _, _, ok := s.Template(600, "ghost"); ok {
		t.Error("unknown template found for a foreign chat")
	}

	// Another chat saving the same name creates its own template and cannot
	// touch the owner's record, so no refusal (and no existence oracle) occurs.
	updated := validProfile()
	updated.Config.Timezone = "Europe/Berlin"
	v600, err := s.SaveTemplate(600, "alpha", updated)
	if err != nil || v600 != 1 {
		t.Fatalf("SaveTemplate of the same name by another chat = (%d, %v), want (1, nil) in its own namespace", v600, err)
	}
	after, afterVersion, ok := s.Template(500, "alpha")
	if !ok || afterVersion != 1 || after.Config.Timezone != original.Config.Timezone {
		t.Errorf("owner template after foreign save = (%+v, %d, %v), want the original version 1", after, afterVersion, ok)
	}
	foreign, _, ok := s.Template(600, "alpha")
	if !ok || foreign.Config.Timezone != "Europe/Berlin" {
		t.Errorf("foreign namespace template = (%+v, %v), want its own content", foreign, ok)
	}

	// The owner still re-saves its own template, bumping the revision.
	v2, err := s.SaveTemplate(500, "alpha", updated)
	if err != nil || v2 != 2 {
		t.Errorf("owner re-save = (%d, %v), want (2, nil)", v2, err)
	}

	// An owner-less save is impossible: owner 0 is never a chat. The name is
	// deliberately unoccupied so the refusal must come from the owner guard
	// itself, not from the foreign-owner check on the borrowed alpha name
	// (MYC-81).
	if _, err := s.SaveTemplate(0, "fresh", updated); err == nil {
		t.Error("SaveTemplate with chat id 0 succeeded; every template must have a real owning chat")
	}
	if _, _, ok := s.Template(500, "fresh"); ok {
		t.Error("refused owner-0 save still created a template record")
	}
}

// TestTemplateNamespacesPersistAcrossRestart checks that two chats holding the
// same name keep separate records on disk and reload as separate templates.
func TestTemplateNamespacesPersistAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	s1 := New(20, 0)
	if err := s1.AttachFileStore(fs, nil); err != nil {
		t.Fatalf("AttachFileStore: %v", err)
	}
	a := validProfile()
	a.Config.Welcome.Message = "from 500"
	b := validProfile()
	b.Config.Welcome.Message = "from 600"
	if _, err := s1.SaveTemplate(500, "shared", a); err != nil {
		t.Fatalf("save 500: %v", err)
	}
	if _, err := s1.SaveTemplate(600, "shared", b); err != nil {
		t.Fatalf("save 600: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	s2 := New(20, 0)
	if err := s2.AttachFileStore(reopened, nil); err != nil {
		t.Fatalf("attach after restart: %v", err)
	}
	got500, _, ok := s2.Template(500, "shared")
	got600, _, ok2 := s2.Template(600, "shared")
	if !ok || !ok2 || got500.Config.Welcome.Message != "from 500" || got600.Config.Welcome.Message != "from 600" {
		t.Errorf("reloaded namespaces = (%q, %v) and (%q, %v), want each chat's own content", got500.Config.Welcome.Message, ok, got600.Config.Welcome.Message, ok2)
	}
	if got := reopened.Keys("templates"); len(got) != 2 || got[0] != "500/shared" || got[1] != "600/shared" {
		t.Errorf("stored template keys = %v, want [500/shared 600/shared]", got)
	}
}

// TestSaveTemplateRejectsInvalidOwner covers the MYC-81 blocker 2 fix at the
// store layer: a save with owner 0 must be refused outright. A stored owner-0
// template record would fail the next AttachFileStore (loadTemplatesLocked
// refuses ownerless records), so such a save must never reach memory or disk.
// Only 0 is invalid: negative owners are valid supergroup/channel chat ids
// (MYC-91) and are covered by TestGroupChannelChatIDsOwnTemplates.
func TestSaveTemplateRejectsInvalidOwner(t *testing.T) {
	s := New(20, 0)
	if _, err := s.SaveTemplate(0, "fresh", validProfile()); err == nil {
		t.Error("SaveTemplate(owner=0) = nil error, want error")
	}
	if _, _, ok := s.Template(500, "fresh"); ok {
		t.Error("refused save with owner 0 created the template anyway")
	}
}

// TestSaveTemplateInvalidOwnerWritesNothing asserts the no-write side of the
// MYC-81 blocker 2 fix against a real FileStore: a refused owner-0 save must
// not write a templates record, so the database must reopen and attach
// cleanly afterwards.
func TestSaveTemplateInvalidOwnerWritesNothing(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	s1 := NewWithDefaults(20, 0, i18n.English, "UTC")
	if err := s1.AttachFileStore(fs, nil); err != nil {
		t.Fatalf("AttachFileStore: %v", err)
	}
	if _, err := s1.SaveTemplate(0, "fresh", validProfile()); err == nil {
		t.Fatal("SaveTemplate with owner 0 succeeded; want error and no write")
	}
	if keys := fs.Keys("templates"); len(keys) != 0 {
		t.Fatalf("refused save persisted templates records %v; want none", keys)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	s2 := NewWithDefaults(20, 0, i18n.English, "UTC")
	if err := s2.AttachFileStore(reopened, nil); err != nil {
		t.Fatalf("AttachFileStore after refused save: %v", err)
	}
	if _, _, ok := s2.Template(500, "fresh"); ok {
		t.Error("template found after a refused owner-0 save; want not-found")
	}
}

func TestSaveTemplateRejectsBadName(t *testing.T) {
	s := New(20, 0)
	for _, name := range []string{"", "bad name", "bad/name", string(make([]rune, templateMaxNameLen+1))} {
		if _, err := s.SaveTemplate(500, name, validProfile()); err == nil {
			t.Errorf("SaveTemplate(%q) = nil error, want error", name)
		}
	}
}

func TestBundleDecodeRejectsUnknownVersionAndBadJSON(t *testing.T) {
	if _, _, err := DecodeTemplateBundle("not json"); err == nil {
		t.Error("DecodeTemplateBundle(not json) = nil error")
	}
	if _, _, err := DecodeTemplateBundle(`{"format":99,"template":{}}`); err == nil {
		t.Error("DecodeTemplateBundle(format 99) = nil error, want unknown-version error")
	}
}

func TestBundleEncodeDecodeRoundtrip(t *testing.T) {
	content := validProfile()
	raw, err := EncodeTemplateBundle(TemplateContent{Name: "beta", Version: 3, Content: content})
	if err != nil {
		t.Fatalf("EncodeTemplateBundle: %v", err)
	}
	name, got, err := DecodeTemplateBundle(raw)
	if err != nil {
		t.Fatalf("DecodeTemplateBundle: %v", err)
	}
	if name != "beta" {
		t.Errorf("decoded name = %q, want beta", name)
	}
	if got.Config != content.Config || len(got.Rules.Keywords) != 1 || got.Rules.Keywords[0] != "spam" {
		t.Errorf("decoded content = %+v, want original", got)
	}
}

func TestNormalizeKeywords(t *testing.T) {
	got := NormalizeKeywords([]string{"  SPAM ", "", "RoboCall"})
	want := []string{"spam", "robocall"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("NormalizeKeywords = %v, want %v", got, want)
	}
	if NormalizeKeywords(nil) != nil {
		t.Error("NormalizeKeywords(nil) should stay nil")
	}
}

func TestApplyProfilePersistenceRoundtrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	s1 := NewWithDefaults(20, 0, i18n.English, "UTC")
	if err := s1.AttachFileStore(fs, nil); err != nil {
		t.Fatalf("AttachFileStore: %v", err)
	}
	content := validProfile()
	content.Config.Language = string(i18n.Chinese)
	if _, err := s1.ApplyProfile(500, content); err != nil {
		t.Fatalf("ApplyProfile: %v", err)
	}
	updated := validProfile()
	updated.Rules.Keywords = []string{"bar"}
	if _, err := s1.SaveTemplate(500, "beta", updated); err != nil {
		t.Fatalf("SaveTemplate: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	s2 := NewWithDefaults(20, 0, i18n.English, "UTC")
	if err := s2.AttachFileStore(reopened, nil); err != nil {
		t.Fatalf("AttachFileStore after restart: %v", err)
	}
	got, ok := s2.Profile(500)
	if !ok {
		t.Fatal("profile lost across restart")
	}
	if got.Config.Language != string(i18n.Chinese) {
		t.Errorf("profile language after restart = %q, want zh", got.Config.Language)
	}
	if s2.Language(500) != i18n.Chinese {
		t.Errorf("chat language after restart = %v, want zh", s2.Language(500))
	}
	tpl, version, ok := s2.Template(500, "beta")
	if !ok || version != 1 {
		t.Fatalf("template after restart = version %d ok %v, want version 1", version, ok)
	}
	if len(tpl.Rules.Keywords) != 1 || tpl.Rules.Keywords[0] != "bar" {
		t.Errorf("template keywords after restart = %v, want [bar]", tpl.Rules.Keywords)
	}
	// The persisted ownership survives the restart: another chat still gets
	// not-found after it (MYC-67).
	if _, _, ok := s2.Template(600, "beta"); ok {
		t.Error("template ownership lost across restart; a foreign chat found the template")
	}
	// The revision continues after a restart instead of starting over.
	v2, err := s2.ApplyProfile(500, validProfile())
	if err != nil {
		t.Fatalf("ApplyProfile after restart: %v", err)
	}
	if v2 != 2 {
		t.Errorf("revision after restart = %d, want 2", v2)
	}
}

// TestApplyProfileRollsBackOnCommitFailure is the atomicity test: a
// persistence commit failure must leave neither the database nor in-memory
// state changed, and no partial write may remain on disk.
func TestApplyProfileRollsBackOnCommitFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("commit failure injection requires a non-root user")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	s := NewWithDefaults(20, 0, i18n.English, "UTC")
	if err := s.AttachFileStore(fs, nil); err != nil {
		t.Fatalf("AttachFileStore: %v", err)
	}

	first := validProfile()
	first.Config.Language = string(i18n.Chinese)
	if _, err := s.ApplyProfile(500, first); err != nil {
		t.Fatalf("first ApplyProfile: %v", err)
	}

	// Make the database directory unwritable so the transaction's snapshot
	// rename cannot complete; the second apply must fail and roll back.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	second := validProfile()
	second.Config.Language = string(i18n.English)
	second.Rules.Keywords = []string{"bar"}
	if _, err := s.ApplyProfile(500, second); err == nil {
		t.Fatal("second ApplyProfile succeeded despite an unwritable database directory; want commit failure")
	}

	// Memory is unchanged.
	if got := s.Language(500); got != i18n.Chinese {
		t.Errorf("Language after failed apply = %v, want Chinese", got)
	}
	content, ok := s.Profile(500)
	if !ok || content.Config.Language != string(i18n.Chinese) || len(content.Rules.Keywords) != 1 || content.Rules.Keywords[0] != "spam" {
		t.Errorf("profile after failed apply = %+v, want the first applied content unchanged", content)
	}

	// Disk is unchanged: reopening the file yields the first content with
	// revision 1, and no partial record was written. The still-open store
	// must be closed first: the database is single-writer, and the lock now
	// outlives commits.
	_ = os.Chmod(dir, 0o700)
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen after failed apply: %v", err)
	}
	defer reopened.Close()
	s2 := NewWithDefaults(20, 0, i18n.English, "UTC")
	if err := s2.AttachFileStore(reopened, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("AttachFileStore: %v", err)
	}
	got, ok := s2.Profile(500)
	if !ok || got.Config.Language != string(i18n.Chinese) {
		t.Errorf("reopened profile = %+v ok %v, want the first applied content", got, ok)
	}
	if s2.Language(500) != i18n.Chinese {
		t.Errorf("reopened chat language = %v, want Chinese", s2.Language(500))
	}
}

// TestAttachRefusesOwnerlessTemplateRecord covers the fail-closed load of
// template records predating template ownership: such a record cannot prove
// who it belongs to, so the attach is aborted instead of serving it to
// whoever asks first.
func TestAttachRefusesOwnerlessTemplateRecord(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	// Write a template record in the pre-ownership shape (no owner field).
	now := time.Now().UTC()
	legacy := map[string]any{
		"version":    1,
		"content":    validProfile(),
		"created_at": now,
		"updated_at": now,
	}
	if err := fs.Put("templates", "legacy", legacy); err != nil {
		t.Fatalf("Put legacy template record: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	s := New(20, 0)
	if err := s.AttachFileStore(reopened, nil); err == nil {
		t.Fatal("AttachFileStore accepted a template record without an owning chat; want fail-closed error")
	}
}
