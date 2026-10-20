package store

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/viceor/tele-admin/internal/i18n"
)

// this file implements S4 (myc-46 slice 4): bot configuration and rules are recorded per chat as
// versioned profile records; named templates capture a profile, belong to the chat that saved
// them, and can be applied by their owner or imported/exported as versioned parcels.

// WelcomeProfile is the join greeting part of a profile. When a profile has
// been applied to a chat, Enabled/Message replace the global welcome config
// for that chat.
type WelcomeProfile struct {
	Enabled bool   `json:"enabled"`
	Message string `json:"message"`
}

// ProfileConfig is the configuration half of a profile: interface language,
// IANA timezone, and the welcome greeting for the chat.
type ProfileConfig struct {
	Language string         `json:"language"`
	Timezone string         `json:"timezone"`
	Welcome  WelcomeProfile `json:"welcome"`
}

// ProfileRules is the rules half of a profile. Keywords, when not nil, is the
// complete chat blocklist override: nil inherits the global blocklist, an
// empty list blocks nothing, and each term is matched case-insensitively.
type ProfileRules struct {
	Keywords []string `json:"keywords"`
}

// ProfileContent is the clonable payload: chat configuration plus rules.
type ProfileContent struct {
	Config ProfileConfig `json:"config"`
	Rules  ProfileRules  `json:"rules"`
}

// ProfileRecord is the persisted per-chat profile with its record revision.
// The Store stamps Version on every write, so callers can tell revisions apart.
type ProfileRecord struct {
	Version int            `json:"version"`
	Content ProfileContent `json:"content"`
}

// TemplateContent is the envelope of one named template: its name, the
// revision of the content it captured, and the content itself.
type TemplateContent struct {
	Name    string         `json:"name"`
	Version int            `json:"version"`
	Content ProfileContent `json:"content"`
}

// TemplateFormatVersion is the version of the template transfer format used
// by export/import. An import whose bundle carries a different value is
// rejected as an unknown version.
const TemplateFormatVersion = 1

// TemplateBundle is the versioned transfer format for templates. The Format
// field must equal TemplateFormatVersion; Import rejects anything else.
type TemplateBundle struct {
	Format   int             `json:"format"`
	Template TemplateContent `json:"template"`
}

// Structural limits for profiles and template names. They bound the size and
// shape of clonable payloads (also so an exported template fits in one
// Telegram message).
const (
	profileMaxKeywords   = 100
	profileMaxKeywordLen = 64
	profileWelcomeMaxLen = 1024
	profileLanguageMax   = 16
	templateMaxNameLen   = 32
	profileMaxBytes      = 3000
)

var validLanguages = map[i18n.Language]bool{
	i18n.English: true,
	i18n.Chinese: true,
}

// ValidateProfile reports whether content is a structurally valid profile.
// It checks field sizes and supported values; it does not mutate content.
func ValidateProfile(content ProfileContent) error {
	raw, err := json.Marshal(content)
	if err != nil {
		return fmt.Errorf("encode profile: %w", err)
	}
	if len(raw) > profileMaxBytes {
		return fmt.Errorf("profile is too large (%d bytes; max %d)", len(raw), profileMaxBytes)
	}
	if lang := content.Config.Language; !validLanguages[i18n.Language(lang)] {
		return fmt.Errorf("language %q is not supported; supported: en, zh", lang)
	}
	if tz := content.Config.Timezone; tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return fmt.Errorf("timezone %q is not a valid IANA timezone", tz)
		}
	}
	if runes := []rune(content.Config.Welcome.Message); len(runes) > profileWelcomeMaxLen {
		return fmt.Errorf("welcome message is too long (%d runes; max %d)", len(runes), profileWelcomeMaxLen)
	}
	if len(content.Rules.Keywords) > profileMaxKeywords {
		return fmt.Errorf("too many keywords (%d; max %d)", len(content.Rules.Keywords), profileMaxKeywords)
	}
	for _, kw := range content.Rules.Keywords {
		runes := []rune(kw)
		if len(runes) == 0 || len(runes) > profileMaxKeywordLen {
			return fmt.Errorf("keyword %q must be 1-%d characters", kw, profileMaxKeywordLen)
		}
	}
	return nil
}

// ValidateTemplateName reports whether name is usable as a template name:
// 1-32 characters among ASCII letters, digits, '_' and '-'.
func ValidateTemplateName(name string) error {
	if name == "" || len(name) > templateMaxNameLen {
		return fmt.Errorf("template name must be 1-%d characters", templateMaxNameLen)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return fmt.Errorf("template name must contain only letters, digits, '_' or '-'")
		}
	}
	return nil
}

// cloneContent returns a deep copy of content so callers outside the store
// lock can never mutate the stored Keyword slice through a reference.
func cloneContent(content ProfileContent) ProfileContent {
	if content.Rules.Keywords == nil {
		return content
	}
	clone := content
	clone.Rules.Keywords = append([]string(nil), content.Rules.Keywords...)
	return clone
}

// templateKey is the namespaced identity of a template: "<owner chat id>/<name>".
// Templates are scoped to the chat that saved them (MYC-83): two chats may
// hold the same name, and a save, import, apply, or export only ever touches
// the caller's own namespace. Because a validated name never contains "/",
// the key is unambiguous. A single global name space would let one chat probe
// whether another chat uses a name, since a refused save would reveal that
// the name exists (MYC-80 R5).
func templateKey(owner int64, name string) string {
	return strconv.FormatInt(owner, 10) + "/" + name
}

// templateEntry is the in-memory state behind one named template. Owner is
// the chat that captured the template; the template is reachable only through
// that owner's namespace.
type templateEntry struct {
	owner     int64
	content   ProfileContent
	version   int
	createdAt time.Time
	updatedAt time.Time
}

// persistedTemplateRecord is the on-disk shape of a template record ("templates" table).
// Records are keyed by templateKey(owner, name). Name is stored alongside so
// the record can be checked against its key; records written before the
// namespace change have no name and a bare-name key (see loadTemplatesLocked).
type persistedTemplateRecord struct {
	Name      string         `json:"name,omitempty"`
	Owner     int64          `json:"owner"`
	Version   int            `json:"version"`
	Content   ProfileContent `json:"content"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// ApplyProfile validates content and applies it to chatID atomically: the
// persisted profile record and the chat settings record are written as one
// transaction, and the in-memory state is updated only after the write
// succeeds. A validation failure or commit failure leaves nothing behind.
// It returns the revision stamped on the chat's profile record.
func (s *Store) ApplyProfile(chatID int64, content ProfileContent) (int, error) {
	if err := ValidateProfile(content); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.settingsLocked(chatID)
	version := 1
	if rec, ok := s.profiles[chatID]; ok {
		version = rec.Version + 1
	}
	rec := ProfileRecord{Version: version, Content: cloneContent(content)}
	if s.persist != nil {
		if err := s.commitProfileLocked(chatID, cs, rec); err != nil {
			return 0, err
		}
	}
	// The persistence commit (if any) has succeeded; nothing below can fail,
	// so memory and disk can never disagree about an applied profile.
	s.profiles[chatID] = &rec
	cs.language = i18n.Language(content.Config.Language)
	cs.timezone = content.Config.Timezone
	return version, nil
}

// commitProfileLocked writes the profile record and the chat settings record
// in one FileStore transaction, so a failure rolls both back. The caller must
// hold s.mu.
func (s *Store) commitProfileLocked(chatID int64, cs *chatSettings, rec ProfileRecord) error {
	tx, err := s.persist.Begin()
	if err != nil {
		return fmt.Errorf("begin profile transaction: %w", err)
	}
	rollback := func(cause error) error {
		tx.Rollback()
		return fmt.Errorf("stage profile write: %w", cause)
	}
	if err := tx.Put("profiles", strconv.FormatInt(chatID, 10), rec); err != nil {
		return rollback(err)
	}
	chatRec := persistedChatRecord{
		ChatID:   chatID,
		Language: rec.Content.Config.Language,
		Timezone: rec.Content.Config.Timezone,
		Welcomed: welcomedIDs(cs.welcomed),
	}
	if err := tx.Put("chats", strconv.FormatInt(chatID, 10), chatRec); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit profile: %w", err)
	}
	return nil
}

// Profile returns the content applied to chatID and whether one exists. The
// returned content is a copy; mutating it never changes the store.
func (s *Store) Profile(chatID int64) (ProfileContent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.profiles[chatID]
	if !ok {
		return ProfileContent{}, false
	}
	return cloneContent(rec.Content), true
}

// SaveTemplate captures content under name in the namespace of the owning
// chat. owner must be a real chat id (non-zero): positive ids are users and
// basic groups, negative ids are supergroups and channels, and every one of
// them can own templates. A save with owner 0 (owner-less) is refused before
// anything is written, so the store can never persist a template record that
// the next AttachFileStore would reject. Saving again
// under the same name in the same namespace overwrites it and bumps the
// template's revision; it never merges. A save from one chat never reads,
// reports, or overwrites another chat's template: the same name in another
// namespace is simply a different template. It returns the revision stamped on
// the template record. The record is written to disk before the in-memory
// template changes; on a persistence failure the existing template (if any)
// is untouched and the error wraps ErrPersistFailed.
func (s *Store) SaveTemplate(owner int64, name string, content ProfileContent) (int, error) {
	if owner == 0 {
		return 0, fmt.Errorf("template owner %d is not a valid chat id; every template must have a real owning chat", owner)
	}
	if err := ValidateTemplateName(name); err != nil {
		return 0, err
	}
	if err := ValidateProfile(content); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := templateKey(owner, name)
	version := 1
	var createdAt time.Time
	if old, ok := s.templates[key]; ok {
		version = old.version + 1
		createdAt = old.createdAt
	}
	now := s.nowFunc()
	if createdAt.IsZero() {
		createdAt = now
	}
	entry := &templateEntry{owner: owner, content: cloneContent(content), version: version, createdAt: createdAt, updatedAt: now}
	if s.persist != nil {
		rec := persistedTemplateRecord{Name: name, Owner: owner, Version: version, Content: entry.content, CreatedAt: createdAt, UpdatedAt: now}
		if err := s.persist.Put("templates", key, rec); err != nil {
			s.plog.Error("persist template failed; change is rolled back", "owner", owner, "name", name, "err", err)
			return 0, persistErr(err)
		}
	}
	s.templates[key] = entry
	return version, nil
}

// Template returns the captured content and revision of the template named
// name in owner's namespace. Templates of other chats are invisible here, so
// a name that exists only elsewhere is reported exactly like an unknown name.
// The returned content is a copy; mutating it never changes the store.
func (s *Store) Template(owner int64, name string) (ProfileContent, int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.templates[templateKey(owner, name)]
	if !ok || entry.owner != owner {
		return ProfileContent{}, 0, false
	}
	return cloneContent(entry.content), entry.version, true
}

// Bundle decode failure phases, used by callers to pick a user-facing message.
const (
	BundlePhaseJSON    = "json"
	BundlePhaseVersion = "version"
	BundlePhaseName    = "name"
	BundlePhaseContent = "content"
)

// TemplateBundleError explains why a template bundle was refused. Phase is
// one of the BundlePhase constants so callers can classify the failure.
type TemplateBundleError struct {
	Phase  string
	Reason string
}

func (e *TemplateBundleError) Error() string { return e.Reason }

// DecodeTemplateBundle parses an exported template parcel and validates the
// bundle format version and the profile structure. An unknown format version
// is rejected: an import can only proceed when both sides speak the same
// versioned format.
func DecodeTemplateBundle(payload string) (string, ProfileContent, error) {
	var bundle TemplateBundle
	if err := json.Unmarshal([]byte(payload), &bundle); err != nil {
		return "", ProfileContent{}, &TemplateBundleError{Phase: BundlePhaseJSON, Reason: fmt.Sprintf("template payload is not valid JSON: %s", err.Error())}
	}
	if bundle.Format != TemplateFormatVersion {
		return "", ProfileContent{}, &TemplateBundleError{Phase: BundlePhaseVersion, Reason: fmt.Sprintf("unknown template version %d; expected format version %d", bundle.Format, TemplateFormatVersion)}
	}
	name := bundle.Template.Name
	if err := ValidateTemplateName(name); err != nil {
		return "", ProfileContent{}, &TemplateBundleError{Phase: BundlePhaseName, Reason: fmt.Sprintf("template %s", err.Error())}
	}
	if err := ValidateProfile(bundle.Template.Content); err != nil {
		return "", ProfileContent{}, &TemplateBundleError{Phase: BundlePhaseContent, Reason: fmt.Sprintf("template is invalid: %s", err.Error())}
	}
	return name, bundle.Template.Content, nil
}

// EncodeTemplateBundle renders a template as the versioned transfer format.
func EncodeTemplateBundle(content TemplateContent) (string, error) {
	raw, err := json.Marshal(TemplateBundle{Format: TemplateFormatVersion, Template: content})
	if err != nil {
		return "", fmt.Errorf("encode template bundle: %w", err)
	}
	return string(raw), nil
}

// loadProfilesLocked rebuilds profile records from the FileStore while
// holding s.mu. It fails the attach when a record cannot be decoded, so the
// store never serves profiles that contradict the database.
func (s *Store) loadProfilesLocked(fs *FileStore) error {
	for _, key := range fs.Keys("profiles") {
		var rec ProfileRecord
		ok, err := fs.Get("profiles", key, &rec)
		if err != nil {
			return fmt.Errorf("read persisted profile %q: %w", key, err)
		}
		if !ok {
			return fmt.Errorf("persisted profile %q vanished during attach", key)
		}
		chatID, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			return fmt.Errorf("persisted profile key %q is not a chat id", key)
		}
		s.profiles[chatID] = &ProfileRecord{Version: rec.Version, Content: rec.Content}
	}
	return nil
}

// loadTemplatesLocked rebuilds template records from the FileStore while
// holding s.mu. Namespaced records ("<owner>/<name>") load directly. A legacy
// record keyed by a bare name but carrying an owner is upgraded to the
// namespaced key in one transaction (see migrateLegacyTemplates). Ownerless
// records (owner 0; negative owners are valid group/channel chats) fail the
// attach: ownership is what protects other chats' templates, so a record that
// so a record that cannot prove its owner is refused, not served. The error
// names the diagnose and quarantine commands; nothing is deleted.
func (s *Store) loadTemplatesLocked(fs *FileStore) error {
	var legacy []legacyTemplate
	for _, key := range fs.Keys("templates") {
		var rec persistedTemplateRecord
		ok, err := fs.Get("templates", key, &rec)
		if err != nil {
			return fmt.Errorf("read persisted template %q: %w", key, err)
		}
		if !ok {
			return fmt.Errorf("persisted template %q vanished during attach", key)
		}
		if rec.Owner == 0 {
			return fmt.Errorf("persisted template %q has no owning chat; it predates template ownership and is refused (fail-closed). Review it with 'teleadmin -config <file> -db-diagnose' and move it aside with '-db-quarantine-ownerless-templates' (records are kept in table %q, not deleted)", key, templateQuarantineTable)
		}
		if !strings.Contains(key, "/") {
			legacy = append(legacy, legacyTemplate{name: key, rec: rec})
			continue
		}
		if rec.Name == "" || templateKey(rec.Owner, rec.Name) != key {
			return fmt.Errorf("persisted template %q does not match its owner %d and name %q", key, rec.Owner, rec.Name)
		}
		s.templates[key] = &templateEntry{owner: rec.Owner, content: rec.Content, version: rec.Version, createdAt: rec.CreatedAt, updatedAt: rec.UpdatedAt}
	}
	if len(legacy) == 0 {
		return nil
	}
	return s.migrateLegacyTemplates(fs, legacy)
}

// legacyTemplate is a template record stored under a bare name, as written
// before the per-chat namespace existed.
type legacyTemplate struct {
	name string
	rec  persistedTemplateRecord
}

// migrateLegacyTemplates moves bare-name template records to their namespaced
// keys in a single FileStore transaction. Each record is written under
// templateKey(owner, name) before its bare-name key is deleted, so a failed
// commit leaves the database unchanged. A namespaced key that already exists,
// or an invalid bare name, refuses the attach instead of guessing which record
// wins. On success the in-memory state holds the namespaced keys.
func (s *Store) migrateLegacyTemplates(fs *FileStore, legacy []legacyTemplate) error {
	existing := make(map[string]bool)
	for _, key := range fs.Keys("templates") {
		existing[key] = true
	}
	tx, err := fs.Begin()
	if err != nil {
		return fmt.Errorf("begin legacy template upgrade: %w", err)
	}
	upgraded := make(map[string]*templateEntry, len(legacy))
	for _, l := range legacy {
		if err := ValidateTemplateName(l.name); err != nil {
			tx.Rollback()
			return fmt.Errorf("legacy template %q has an invalid name and is refused: %w", l.name, err)
		}
		key := templateKey(l.rec.Owner, l.name)
		if existing[key] {
			tx.Rollback()
			return fmt.Errorf("legacy template %q collides with existing record %q; resolve it manually before starting", l.name, key)
		}
		rec := l.rec
		rec.Name = l.name
		if err := tx.Put("templates", key, rec); err != nil {
			tx.Rollback()
			return fmt.Errorf("stage legacy template %q upgrade: %w", l.name, err)
		}
		if err := tx.Delete("templates", l.name); err != nil {
			tx.Rollback()
			return fmt.Errorf("stage legacy template %q removal: %w", l.name, err)
		}
		upgraded[key] = &templateEntry{owner: l.rec.Owner, content: l.rec.Content, version: l.rec.Version, createdAt: l.rec.CreatedAt, updatedAt: l.rec.UpdatedAt}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy template upgrade: %w", err)
	}
	for key, entry := range upgraded {
		s.templates[key] = entry
	}
	return nil
}

// NormalizeKeywords trims and lowercases blocklist terms, matching the
// governance.NewRuleEngine matching semantics, so stored profiles hold
// already-normalized terms. A nil input stays nil.
func NormalizeKeywords(terms []string) []string {
	if terms == nil {
		return nil
	}
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		if v := strings.ToLower(strings.TrimSpace(t)); v != "" {
			out = append(out, v)
		}
	}
	return out
}
