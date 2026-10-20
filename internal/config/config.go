// Package config loads and validates tele-admin runtime configuration.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DevModeEnvVar is the environment variable that explicitly opts into local
// development mode. It is the only way to run with admin enforcement
// disabled; without it, enforce_admin: false is rejected at startup.
const DevModeEnvVar = "TELEADMIN_DEV_MODE"

// DevMode reports whether explicit local development mode is enabled via
// DevModeEnvVar. Development mode permits enforce_admin: false and must
// never be active in production.
func DevMode() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(DevModeEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Config holds all runtime settings for the bot service.
type Config struct {
	TelegramToken string         `yaml:"token"`
	APIServerURL  string         `yaml:"api_server_url"`
	Mode          string         `yaml:"mode"`
	Webhook       WebhookConfig  `yaml:"webhook"`
	LogLevel      string         `yaml:"log_level"`
	DB            DBConfig       `yaml:"db"`
	I18N          I18NConfig     `yaml:"i18n"`
	Timezone      TimezoneConfig `yaml:"timezone"`
	Admin         AdminConfig    `yaml:"admin"`
	Spam          SpamConfig     `yaml:"spam"`
	Welcome       WelcomeConfig  `yaml:"welcome"`
	Keywords      KeywordsConfig `yaml:"keywords"`
	Publish       PublishConfig  `yaml:"publish"`
}

// WebhookConfig configures the webhook reception server.
type WebhookConfig struct {
	Listen      string `yaml:"listen"`
	Path        string `yaml:"path"`
	PublicURL   string `yaml:"public_url"`
	SecretToken string `yaml:"secret_token"`
}

// DBConfig selects the persistence backend.
type DBConfig struct {
	Path string `yaml:"path"`
}

// I18NConfig selects the interface language set.
type I18NConfig struct {
	DefaultLanguage string   `yaml:"default_language"`
	Supported       []string `yaml:"supported"`
}

// TimezoneConfig selects the IANA timezone used for scheduled messages.
type TimezoneConfig struct {
	Default string `yaml:"default"`
}

// AdminConfig controls admin permission enforcement.
type AdminConfig struct {
	EnforceAdmin bool    `yaml:"enforce_admin"`
	OwnerIDs     []int64 `yaml:"owner_ids"`
}

// SpamConfig holds the minimal anti-spam rule chain parameters.
type SpamConfig struct {
	MaxMessagesPerMinute int `yaml:"max_messages_per_minute"`
	MinMessageIntervalMs int `yaml:"min_message_interval_ms"`
}

// WelcomeConfig controls the join greeting.
type WelcomeConfig struct {
	Enabled bool   `yaml:"enabled"`
	Message string `yaml:"message"`
}

// KeywordsConfig holds the blocklist keyword rule.
type KeywordsConfig struct {
	Blocked []string `yaml:"blocked"`
}

// PublishConfig holds the /publish command limits.
type PublishConfig struct {
	// MaxTargets is the maximum number of target chats accepted in one
	// /publish command. Values <= 0 fall back to the default (10).
	MaxTargets int `yaml:"max_targets"`
	// MaxContentLength is the maximum content length (in characters) for one
	// /publish command, for text and media alike. Values <= 0 fall back to
	// the default (4096, Telegram's message length limit).
	MaxContentLength int `yaml:"max_content_length"`
	// PerTargetTimeout bounds one target's permission probe and send during
	// /publish. A target that exceeds it is recorded as a timeout failure.
	// Values <= 0 fall back to the default (10s).
	PerTargetTimeout time.Duration `yaml:"per_target_timeout"`
	// MaxConcurrentSends caps how many /publish targets receive content in
	// parallel, so a slow target never blocks the others. Values <= 0 fall
	// back to the default (4).
	MaxConcurrentSends int `yaml:"max_concurrent_sends"`
}

// Load reads a YAML configuration file and validates it.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate returns an error when required settings are missing or inconsistent.
func (c *Config) Validate() error {
	if c.Mode == "" {
		c.Mode = "polling"
	}
	switch c.Mode {
	case "polling", "webhook":
	default:
		return fmt.Errorf("invalid mode %q: must be polling or webhook", c.Mode)
	}
	if c.TelegramToken == "" {
		return fmt.Errorf("token is required")
	}
	if !c.Admin.EnforceAdmin && !DevMode() {
		return fmt.Errorf("admin.enforce_admin=false is not allowed outside explicit development mode (set %s=1 for local development)", DevModeEnvVar)
	}
	if c.Mode == "webhook" {
		if c.Webhook.Listen == "" {
			return fmt.Errorf("webhook.listen is required in webhook mode")
		}
		if err := ValidateListenAddress(c.Webhook.Listen); err != nil {
			return err
		}
		if c.Webhook.SecretToken == "" {
			return fmt.Errorf("webhook.secret_token is required in webhook mode")
		}
		if err := ValidatePublicURL(c.Webhook.PublicURL); err != nil {
			return err
		}
		if c.Webhook.Path == "" {
			c.Webhook.Path = "/webhook"
		}
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	// An empty db.path means "not configured": the service runs in-memory and
	// state is lost on restart. A non-empty path enables file persistence.
	if len(c.I18N.Supported) == 0 {
		c.I18N.Supported = []string{"en", "zh"}
	}
	if c.I18N.DefaultLanguage == "" {
		c.I18N.DefaultLanguage = "en"
	}
	if !c.I18N.HasLanguage(c.I18N.DefaultLanguage) {
		return fmt.Errorf("unsupported default_language %q", c.I18N.DefaultLanguage)
	}
	if c.Timezone.Default == "" {
		c.Timezone.Default = "UTC"
	}
	if _, err := time.LoadLocation(c.Timezone.Default); err != nil {
		return fmt.Errorf("invalid timezone default %q: %w", c.Timezone.Default, err)
	}
	if c.Spam.MaxMessagesPerMinute <= 0 {
		c.Spam.MaxMessagesPerMinute = 20
	}
	if c.Spam.MinMessageIntervalMs < 0 {
		c.Spam.MinMessageIntervalMs = 0
	}
	if c.Publish.MaxTargets <= 0 {
		c.Publish.MaxTargets = 10
	}
	if c.Publish.MaxContentLength <= 0 {
		c.Publish.MaxContentLength = 4096
	}
	if c.Publish.PerTargetTimeout <= 0 {
		c.Publish.PerTargetTimeout = 10 * time.Second
	}
	if c.Publish.MaxConcurrentSends <= 0 {
		c.Publish.MaxConcurrentSends = 4
	}
	return nil
}

// HasLanguage reports whether a language code is in the supported list.
func (c I18NConfig) HasLanguage(lang string) bool {
	for _, l := range c.Supported {
		if strings.EqualFold(l, lang) {
			return true
		}
	}
	return false
}

// SlogLevel maps the configured log_level to a slog.Level. Recognized values
// are debug, info, warn/warning and error; anything else (including the empty
// string) maps to info.
func (c *Config) SlogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ValidateListenAddress checks a webhook.listen value. The webhook listener
// speaks plaintext HTTP, so it must only bind a loopback, private, or
// link-local interface. Wildcard addresses (empty host, 0.0.0.0, ::), public
// IP addresses, and hostnames are rejected: a public or name-based listener
// would expose unencrypted traffic unless a real TLS terminator is placed in
// front of it. Use a loopback/private IP, or terminate TLS elsewhere and bind
// the loopback address here.
func ValidateListenAddress(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid webhook.listen %q: %w", listen, err)
	}
	if host == "" {
		return fmt.Errorf("webhook.listen %q binds all interfaces; webhook mode requires a loopback or private address behind a TLS-terminating reverse proxy", listen)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("webhook.listen %q uses hostname %q, which has no explicit TLS policy; webhook mode accepts only loopback or private IP addresses because the listener is plaintext HTTP; use an IP address or terminate TLS in front of it", listen, host)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("webhook.listen %q binds all interfaces; webhook mode requires a loopback or private address behind a TLS-terminating reverse proxy", listen)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return nil
	}
	return fmt.Errorf("webhook.listen %q is a public address; webhook mode accepts only loopback or private IP addresses because the listener is plaintext HTTP; use a loopback/private address or terminate TLS in front of it", listen)
}

// ValidatePublicURL checks the webhook.public_url value passed to Telegram's
// setWebhook. Telegram requires an HTTPS URL, so an empty or non-HTTPS value is
// rejected before any network call.
func ValidatePublicURL(url string) error {
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("webhook.public_url is required in webhook mode")
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(url)), "https://") {
		return fmt.Errorf("webhook.public_url %q must be an HTTPS URL", url)
	}
	return nil
}
