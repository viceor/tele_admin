package store

import (
	"path/filepath"
	"testing"
)

// TestGroupChannelChatIDsOwnTemplates covers MYC-91: Telegram supergroup and
// channel chat ids are negative (e.g. -1001234567890), so a negative owner is
// a real chat and uses save/re-own/export/apply/import normally. Only owner 0
// counts as ownerless.
func TestGroupChannelChatIDsOwnTemplates(t *testing.T) {
	s := New(20, 0)
	const supergroup = int64(-1001234567890)
	const channel = int64(-1009999999999)

	// Save works for a negative owner and versions like any other chat.
	v1, err := s.SaveTemplate(supergroup, "alpha", validProfile())
	if err != nil || v1 != 1 {
		t.Fatalf("SaveTemplate(supergroup) = (%d, %v), want (1, nil)", v1, err)
	}
	// The same name in a second negative namespace stays separate.
	v2, err := s.SaveTemplate(channel, "alpha", validProfile())
	if err != nil || v2 != 1 {
		t.Fatalf("SaveTemplate(channel) = (%d, %v), want (1, nil)", v2, err)
	}
	// Cross-namespace reads stay isolated, positive chats see not-found, and
	// a positive/zero owner can never reach a negative owner's template.
	if _, _, ok := s.Template(supergroup, "nope"); ok {
		t.Error("unknown template found in the supergroup namespace")
	}
	if _, _, ok := s.Template(supergroup+1, "alpha"); ok {
		t.Error("positive chat found the supergroup template")
	}

	// Export round trip: the negative owner's template encodes and decodes
	// back, then imports into another chat's namespace without touching the
	// original.
	content, version, ok := s.Template(supergroup, "alpha")
	if !ok || version != 1 {
		t.Fatalf("Template(supergroup, alpha) = (%+v, %d, %v)", content, version, ok)
	}
	payload, err := EncodeTemplateBundle(TemplateContent{Name: "alpha", Version: version, Content: content})
	if err != nil {
		t.Fatalf("EncodeTemplateBundle: %v", err)
	}
	importedName, importedContent, err := DecodeTemplateBundle(payload)
	if err != nil || importedName != "alpha" || !equalProfileContent(importedContent, content) {
		t.Fatalf("DecodeTemplateBundle = (%q, %+v, %v)", importedName, importedContent, err)
	}
	v3, err := s.SaveTemplate(700, importedName, importedContent)
	if err != nil || v3 != 1 {
		t.Fatalf("import into chat 700 = (%d, %v), want (1, nil)", v3, err)
	}
	after, afterVersion, ok := s.Template(supergroup, "alpha")
	if !ok || afterVersion != 1 || !equalProfileContent(after, content) {
		t.Errorf("supergroup template after import elsewhere = (%+v, %d, %v), want version 1 unchanged", after, afterVersion, ok)
	}

	// Apply works from a negative owner's template.
	applied, err := s.ApplyProfile(700, after)
	if err != nil || applied != 1 {
		t.Fatalf("ApplyProfile from supergroup template = (%d, %v)", applied, err)
	}

	// Owner re-save bumps the revision in its own namespace.
	if v4, err := s.SaveTemplate(supergroup, "alpha", validProfile()); err != nil || v4 != 2 {
		t.Errorf("supergroup re-save = (%d, %v), want (2, nil)", v4, err)
	}
}

// equalProfileContent reports whether two profile contents are deep-equal for
// the fields this package persists.
func equalProfileContent(a, b ProfileContent) bool {
	if a.Config != b.Config {
		return false
	}
	if len(a.Rules.Keywords) != len(b.Rules.Keywords) {
		return false
	}
	for i, kw := range a.Rules.Keywords {
		if kw != b.Rules.Keywords[i] {
			return false
		}
	}
	if len(a.Rules.Keywords) == 0 && len(b.Rules.Keywords) == 0 && (a.Rules.Keywords == nil) != (b.Rules.Keywords == nil) {
		return false
	}
	return true
}

// TestGroupChatTemplatesPersistAcrossRestart covers MYC-91 at the persistence
// layer: a negative owner's namespaced template survives a restart and must
// not be refused by loadTemplatesLocked as ownerless.
func TestGroupChatTemplatesPersistAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	const supergroup = int64(-1001234567890)

	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	s1 := New(20, 0)
	if err := s1.AttachFileStore(fs, nil); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if v, err := s1.SaveTemplate(supergroup, "alpha", validProfile()); err != nil || v != 1 {
		t.Fatalf("SaveTemplate(supergroup) = (%d, %v)", v, err)
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
		t.Fatalf("attach after restart refused the negative chat id: %v", err)
	}
	content, version, ok := s2.Template(supergroup, "alpha")
	if !ok || version != 1 || content.Config.Timezone != "UTC" {
		t.Fatalf("Template after restart = (%+v, %d, %v), want version 1 with content", content, version, ok)
	}
	if keys := reopened.Keys("templates"); len(keys) != 1 || keys[0] != "-1001234567890/alpha" {
		t.Errorf("stored keys = %v, want [-1001234567890/alpha]", keys)
	}
}

// TestDiagnoseClassifiesNegativeOwnerAsNamespaced covers MYC-91: diagnose must
// classify a negative-owner record as a namespaced (loadable) template, not as
// ownerless, so quarantine has nothing to move. Quarantine must not touch it.
func TestDiagnoseClassifiesNegativeOwnerAsNamespaced(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "teleadmin.db")
	const supergroup = int64(-1001234567890)

	fs, err := OpenFileStore(dbPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	defer fs.Close()
	s := New(20, 0)
	if err := s.AttachFileStore(fs, nil); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := s.SaveTemplate(supergroup, "alpha", validProfile()); err != nil {
		t.Fatalf("SaveTemplate: %v", err)
	}

	rep := DiagnoseTemplates(fs)
	if !equalStrings(rep.Namespaced, []string{"-1001234567890/alpha"}) {
		t.Fatalf("report = %+v, want namespaced [-1001234567890/alpha]", rep)
	}
	if len(rep.Ownerless) != 0 || len(rep.Malformed) != 0 || len(rep.Legacy) != 0 || len(rep.Quarantined) != 0 {
		t.Errorf("report misclassified the negative-owner record: %+v", rep)
	}
	moved, err := QuarantineOwnerlessTemplates(fs)
	if err != nil || len(moved) != 0 {
		t.Fatalf("quarantine moved %v (err %v), want nothing moved", moved, err)
	}
	if keys := fs.Keys(templateQuarantineTable); len(keys) != 0 {
		t.Errorf("quarantine table holds %v; a negative-owner record was mis-quarantined", keys)
	}
}
