package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const validConfig = `
token: "123456:test-token"
mode: polling
log_level: debug
admin:
  enforce_admin: true
i18n:
  default_language: zh
  supported: [en, zh]
timezone:
  default: Asia/Shanghai
`

func TestLoadValid(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != "polling" {
		t.Errorf("mode = %q, want polling", cfg.Mode)
	}
	if cfg.TelegramToken != "123456:test-token" {
		t.Errorf("token = %q", cfg.TelegramToken)
	}
	if cfg.I18N.DefaultLanguage != "zh" {
		t.Errorf("default_language = %q, want zh", cfg.I18N.DefaultLanguage)
	}
	if cfg.Timezone.Default != "Asia/Shanghai" {
		t.Errorf("timezone = %q", cfg.Timezone.Default)
	}
	if cfg.Spam.MaxMessagesPerMinute != 20 {
		t.Errorf("default max_per_minute = %d, want 20", cfg.Spam.MaxMessagesPerMinute)
	}
}

func TestLoadMissingToken(t *testing.T) {
	body := "mode: polling\n"
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestLoadInvalidMode(t *testing.T) {
	body := "token: x\nmode: carrier-pigeon\n"
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestLoadInvalidTimezone(t *testing.T) {
	body := "token: x\nadmin:\n  enforce_admin: true\ntimezone:\n  default: Not/AZone\n"
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("expected error for invalid timezone")
	}
}

func TestLoadWebhookRequiresListen(t *testing.T) {
	body := "token: x\nadmin:\n  enforce_admin: true\nmode: webhook\n"
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("expected error when webhook.listen is missing")
	}
}

func TestLoadWebhookDefaultsPath(t *testing.T) {
	body := "token: x\nadmin:\n  enforce_admin: true\nmode: webhook\nwebhook:\n  listen: 127.0.0.1:8443\n  public_url: https://example.com/webhook\n  secret_token: s3cret\n"
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Webhook.Path != "/webhook" {
		t.Errorf("webhook path = %q, want /webhook", cfg.Webhook.Path)
	}
}

func TestLoadUnsupportedDefaultLanguage(t *testing.T) {
	body := "token: x\nadmin:\n  enforce_admin: true\ni18n:\n  default_language: fr\n  supported: [en, zh]\n"
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("expected error for unsupported default_language")
	}
}

func TestLoadSampleConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("shipped config.example.yaml must load: %v", err)
	}
	if cfg.TelegramToken == "" {
		t.Error("sample config should carry a placeholder token")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	if _, err := Load(writeConfig(t, "token: [unterminated\n")); err == nil {
		t.Fatal("expected error for malformed yaml")
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "token: 123:abc\nadmin:\n  enforce_admin: true\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != "polling" {
		t.Errorf("mode = %q, want polling default", cfg.Mode)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level = %q, want info default", cfg.LogLevel)
	}
	if cfg.DB.Path != "" {
		t.Errorf("db.path = %q, want empty (in-memory mode) when not configured", cfg.DB.Path)
	}
	if cfg.I18N.DefaultLanguage != "en" {
		t.Errorf("default_language = %q, want en default", cfg.I18N.DefaultLanguage)
	}
	want := []string{"en", "zh"}
	if len(cfg.I18N.Supported) != len(want) {
		t.Fatalf("supported = %v, want %v", cfg.I18N.Supported, want)
	}
	for i := range want {
		if cfg.I18N.Supported[i] != want[i] {
			t.Errorf("supported = %v, want %v", cfg.I18N.Supported, want)
		}
	}
	if cfg.Timezone.Default != "UTC" {
		t.Errorf("timezone.default = %q, want UTC", cfg.Timezone.Default)
	}
	if cfg.Spam.MaxMessagesPerMinute != 20 {
		t.Errorf("max_messages_per_minute = %d, want 20", cfg.Spam.MaxMessagesPerMinute)
	}
}

func TestLoadClampsNegativeInterval(t *testing.T) {
	body := "token: x\nadmin:\n  enforce_admin: true\nspam:\n  max_messages_per_minute: -1\n  min_message_interval_ms: -5\n"
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Spam.MaxMessagesPerMinute != 20 {
		t.Errorf("max_messages_per_minute = %d, want clamped default 20", cfg.Spam.MaxMessagesPerMinute)
	}
	if cfg.Spam.MinMessageIntervalMs != 0 {
		t.Errorf("min_message_interval_ms = %d, want clamped 0", cfg.Spam.MinMessageIntervalMs)
	}
}

// TestLoadWebhookRequiresSecretToken sets enforce_admin explicitly so the
// webhook secret-token rule, not the fail-closed M-1 admin rule, is what
// rejects the config.
func TestLoadWebhookRequiresSecretToken(t *testing.T) {
	t.Setenv(DevModeEnvVar, "")
	body := "token: x\nadmin:\n  enforce_admin: true\nmode: webhook\nwebhook:\n  listen: 127.0.0.1:8443\n  secret_token: \"\"\n"
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("expected error when webhook.secret_token is empty")
	}
	if !strings.Contains(err.Error(), "webhook.secret_token") {
		t.Fatalf("error = %v, want webhook.secret_token validation", err)
	}
}

func TestLoadWebhookRejectsWildcardListen(t *testing.T) {
	t.Setenv(DevModeEnvVar, "")
	for _, listen := range []string{"0.0.0.0:8443", "[::]:8443", ":8443"} {
		body := "token: x\nadmin:\n  enforce_admin: true\nmode: webhook\nwebhook:\n  listen: \"" + listen + "\"\n  secret_token: s3cret\n"
		_, err := Load(writeConfig(t, body))
		if err == nil {
			t.Errorf("expected error for wildcard webhook.listen %q", listen)
			continue
		}
		if !strings.Contains(err.Error(), "webhook.listen") {
			t.Errorf("error for %q = %v, want webhook.listen validation", listen, err)
		}
	}
}

func TestLoadWebhookAcceptsLoopbackListen(t *testing.T) {
	body := "token: x\nadmin:\n  enforce_admin: true\nmode: webhook\nwebhook:\n  listen: 127.0.0.1:8443\n  public_url: https://example.com/webhook\n  secret_token: s3cret\n"
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Webhook.SecretToken != "s3cret" {
		t.Errorf("secret_token = %q", cfg.Webhook.SecretToken)
	}
}

// TestLoadWebhookRejectsInvalidListen sets enforce_admin explicitly so the
// unparseable-listen rule, not the M-1 admin rule, is what rejects the config.
func TestLoadWebhookRejectsInvalidListen(t *testing.T) {
	t.Setenv(DevModeEnvVar, "")
	body := "token: x\nadmin:\n  enforce_admin: true\nmode: webhook\nwebhook:\n  listen: \"not-an-address\"\n  secret_token: s3cret\n"
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("expected error for unparseable webhook.listen")
	}
	if !strings.Contains(err.Error(), "invalid webhook.listen") {
		t.Fatalf("error = %v, want invalid webhook.listen validation", err)
	}
}

func TestLoadEnforceAdminFalseRejectedWithoutDevMode(t *testing.T) {
	t.Setenv(DevModeEnvVar, "")
	body := "token: x\nadmin:\n  enforce_admin: false\n"
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("expected error for enforce_admin=false outside development mode")
	}
}

func TestLoadEnforceAdminFalseRejectedWhenUnset(t *testing.T) {
	t.Setenv(DevModeEnvVar, "")
	body := "token: x\n"
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("expected error when enforce_admin is unset (fail-closed default)")
	}
}

func TestLoadEnforceAdminFalseAllowedInDevMode(t *testing.T) {
	t.Setenv(DevModeEnvVar, "1")
	body := "token: x\nadmin:\n  enforce_admin: false\n"
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Admin.EnforceAdmin {
		t.Error("enforce_admin should remain false in development mode")
	}
}

func TestLoadEnforceAdminTrueAccepted(t *testing.T) {
	t.Setenv(DevModeEnvVar, "")
	body := "token: x\nadmin:\n  enforce_admin: true\n"
	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// TestValidateListenAddressAcceptsNonPublic covers the loopback, RFC1918,
// IPv6 ULA, and link-local addresses the plaintext listener may bind.
func TestValidateListenAddressAcceptsNonPublic(t *testing.T) {
	for _, listen := range []string{
		"127.0.0.1:8443",
		"[::1]:8443",
		"10.0.0.5:8443",
		"172.16.4.2:8443",
		"192.168.1.10:8443",
		"[fd00::1]:8443",
		"[fe80::1]:8443",
	} {
		if err := ValidateListenAddress(listen); err != nil {
			t.Errorf("ValidateListenAddress(%q) = %v, want nil", listen, err)
		}
	}
}

// TestValidateListenAddressRejectsPublicAndHostname covers wildcard, public,
// and hostname listeners that would expose plaintext HTTP without an explicit
// TLS terminator.
func TestValidateListenAddressRejectsPublicAndHostname(t *testing.T) {
	for _, listen := range []string{
		"0.0.0.0:8443",
		"[::]:8443",
		":8443",
		"",
		"8.8.8.8:8443",
		"203.0.113.7:8443",
		"[2001:4860:4860::8888]:8443",
		"localhost:8443",
		"example.com:8443",
		"not-an-address",
	} {
		if err := ValidateListenAddress(listen); err == nil {
			t.Errorf("ValidateListenAddress(%q) = nil, want error", listen)
		}
	}
}

func TestValidatePublicURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://example.com/webhook", true},
		{"HTTPS://example.com/webhook", true},
		{"http://example.com/webhook", false},
		{"ftp://example.com/webhook", false},
		{"", false},
		{"   ", false},
	} {
		err := ValidatePublicURL(tc.url)
		if tc.ok && err != nil {
			t.Errorf("ValidatePublicURL(%q) = %v, want nil", tc.url, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("ValidatePublicURL(%q) = nil, want error", tc.url)
		}
	}
}

func TestLoadWebhookRequiresPublicURL(t *testing.T) {
	t.Setenv(DevModeEnvVar, "")
	body := "token: x\nadmin:\n  enforce_admin: true\nmode: webhook\nwebhook:\n  listen: 127.0.0.1:8443\n  secret_token: s3cret\n"
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("expected error when webhook.public_url is missing")
	}
	if !strings.Contains(err.Error(), "public_url") {
		t.Fatalf("error = %v, want webhook.public_url validation", err)
	}
}

func TestLoadWebhookRequiresHTTPSPublicURL(t *testing.T) {
	t.Setenv(DevModeEnvVar, "")
	body := "token: x\nadmin:\n  enforce_admin: true\nmode: webhook\nwebhook:\n  listen: 127.0.0.1:8443\n  public_url: http://example.com/webhook\n  secret_token: s3cret\n"
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("expected error when webhook.public_url is not HTTPS")
	}
	if !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("error = %v, want HTTPS requirement", err)
	}
}

func TestSlogLevel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"verbose", slog.LevelInfo},
	} {
		cfg := &Config{LogLevel: tc.in}
		if got := cfg.SlogLevel(); got != tc.want {
			t.Errorf("SlogLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
