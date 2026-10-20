// Package bot wires the Telegram client, governance rules, and command
// handlers into a runnable service.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	tgbot "github.com/go-telegram/bot"
	tgbotmodels "github.com/go-telegram/bot/models"

	"github.com/viceor/tele-admin/internal/config"
	"github.com/viceor/tele-admin/internal/governance"
	"github.com/viceor/tele-admin/internal/i18n"
	"github.com/viceor/tele-admin/internal/store"
)

// Bot is the tele-admin service façade.
type Bot struct {
	client *tgbot.Bot
	cfg    *config.Config
	store  *store.Store
	admin  *governance.AdminChecker
	rules  *governance.RuleEngine
	spam   *governance.SpamChecker
	log    *slog.Logger
	// botMu guards botID, the bot's own user ID, cached lazily for publish
	// permission checks.
	botMu sync.Mutex
	botID int64
}

// New builds a Bot from configuration. It probes the token with a short
// timeout; an unreachable API is non-fatal, so the service starts cleanly
// against a mock Telegram API or before a real token is configured.
func New(cfg *config.Config, logger *slog.Logger) (*Bot, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	if err := validateSecurityPosture(cfg, logger); err != nil {
		return nil, err
	}
	// Library output goes through the structured logger with redaction (see
	// library_log.go). tgbot.WithDebug is deliberately never set: it would
	// log full request and webhook bodies, and log_level=debug must not
	// enable that (MYC-83).
	opts := []tgbot.Option{
		tgbot.WithSkipGetMe(),
		tgbot.WithErrorsHandler(newLibraryErrorsHandler(logger)),
		tgbot.WithDebugHandler(newLibraryDebugHandler(logger)),
		tgbot.WithDefaultHandler(newLibraryDefaultHandler(logger)),
	}
	if cfg.APIServerURL != "" {
		opts = append(opts, tgbot.WithServerURL(cfg.APIServerURL))
	}
	if cfg.Mode == "webhook" {
		// The library only enforces the X-Telegram-Bot-Api-Secret-Token
		// header when a secret is configured, so webhook mode must always
		// carry one; validateSecurityPosture guarantees it is non-empty.
		opts = append(opts, tgbot.WithWebhookSecretToken(cfg.Webhook.SecretToken))
	}
	client, err := tgbot.New(cfg.TelegramToken, opts...)
	if err != nil {
		return nil, err
	}

	st := store.NewWithDefaults(
		cfg.Spam.MaxMessagesPerMinute,
		cfg.Spam.MinMessageIntervalMs,
		i18n.Language(cfg.I18N.DefaultLanguage),
		cfg.Timezone.Default,
	)
	if cfg.DB.Path != "" {
		fs, err := store.OpenFileStore(cfg.DB.Path)
		if err != nil {
			return nil, fmt.Errorf("open database %q: %w", cfg.DB.Path, err)
		}
		if err := st.AttachFileStore(fs, logger); err != nil {
			_ = fs.Close()
			return nil, fmt.Errorf("load database %q: %w", cfg.DB.Path, err)
		}
		logger.Info("persistence enabled", "backend", "file", "path", cfg.DB.Path)
	} else {
		logger.Warn("db.path is not configured; using in-memory storage; all state is lost on restart")
	}
	b := &Bot{
		client: client,
		cfg:    cfg,
		store:  st,
		admin:  governance.NewAdminChecker(client, cfg.Admin.EnforceAdmin, cfg.Admin.OwnerIDs),
		rules:  governance.NewRuleEngine(cfg.Keywords.Blocked),
		spam:   governance.NewSpamChecker(st),
		log:    logger,
	}
	b.registerHandlers()
	probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if me, err := b.client.GetMe(probeCtx); err != nil {
		b.log.Warn("getMe failed; continuing (check token or api_server_url)", "err", err)
	} else {
		b.botID = me.ID
	}
	return b, nil
}

// Close releases the persistence backend (database lock) when one is
// configured. It is not required for data durability — every commit is already
// on disk — but it lets another process take over the same database path
// immediately after shutdown. It is safe to call on a bot without persistence.
func (b *Bot) Close() error {
	if fs := b.store.FileStore(); fs != nil {
		return fs.Close()
	}
	return nil
}

// Client exposes the underlying Telegram client for tests.
func (b *Bot) Client() *tgbot.Bot {
	return b.client
}

// validateSecurityPosture enforces the fail-closed security boundaries that
// must hold no matter how the Config was constructed: webhook mode requires
// a non-empty secret token and a non-wildcard listen address, and admin
// enforcement may only be disabled in explicit local development mode.
func validateSecurityPosture(cfg *config.Config, logger *slog.Logger) error {
	if cfg.Mode == "webhook" {
		if cfg.Webhook.SecretToken == "" {
			return fmt.Errorf("webhook.secret_token is required in webhook mode")
		}
		if err := config.ValidateListenAddress(cfg.Webhook.Listen); err != nil {
			return err
		}
		if err := config.ValidatePublicURL(cfg.Webhook.PublicURL); err != nil {
			return err
		}
	}
	if !cfg.Admin.EnforceAdmin {
		if !config.DevMode() {
			return fmt.Errorf("admin.enforce_admin=false is not allowed outside explicit development mode (set %s=1 for local development)", config.DevModeEnvVar)
		}
		logger.Warn("SECURITY: admin enforcement is disabled because " + config.DevModeEnvVar + " is set; every user can run admin commands; never enable this in production")
	}
	return nil
}

// registerHandlers installs the command and default handlers. MatchTypeCommand
// patterns omit the leading slash because the library compares the entity text
// after the slash (for example "start" in "/start").
func (b *Bot) registerHandlers() {
	b.client.RegisterHandler(tgbot.HandlerTypeMessageText, "start", tgbot.MatchTypeCommand, b.handleStart)
	b.client.RegisterHandler(tgbot.HandlerTypeMessageText, "help", tgbot.MatchTypeCommand, b.handleHelp)
	b.client.RegisterHandler(tgbot.HandlerTypeMessageText, "welcome", tgbot.MatchTypeCommand, b.handleWelcomeCmd)
	b.client.RegisterHandler(tgbot.HandlerTypeMessageText, "lang", tgbot.MatchTypeCommand, b.handleLang)
	b.client.RegisterHandler(tgbot.HandlerTypeMessageText, "tz", tgbot.MatchTypeCommand, b.handleTimezone)
	b.client.RegisterHandler(tgbot.HandlerTypeMessageText, "restrict", tgbot.MatchTypeCommand, b.handleRestrictCmd)
	b.client.RegisterHandler(tgbot.HandlerTypeMessageText, "unrestrict", tgbot.MatchTypeCommand, b.handleUnrestrictCmd)
	b.client.RegisterHandler(tgbot.HandlerTypeMessageText, "card", tgbot.MatchTypeCommand, b.handleCardCmd)
	b.client.RegisterHandler(tgbot.HandlerTypeMessageText, "publish", tgbot.MatchTypeCommand, b.handlePublishCmd)
	b.client.RegisterHandler(tgbot.HandlerTypeMessageText, "publish_add", tgbot.MatchTypeCommand, b.handlePublishAdd)
	b.client.RegisterHandler(tgbot.HandlerTypeMessageText, "publish_remove", tgbot.MatchTypeCommand, b.handlePublishRemove)
	b.client.RegisterHandler(tgbot.HandlerTypeMessageText, "clone", tgbot.MatchTypeCommand, b.handleCloneCmd)
	b.client.RegisterHandler(tgbot.HandlerTypeMessageText, "template", tgbot.MatchTypeCommand, b.handleTemplateCmd)
	b.client.RegisterHandlerMatchFunc(func(*tgbotmodels.Update) bool { return true }, b.handleGovernance)
}

// Start runs the bot until the context is cancelled.
func (b *Bot) Start(ctx context.Context) error {
	if b.cfg.Mode == "webhook" {
		return b.startWebhook(ctx)
	}
	b.client.Start(ctx)
	return ctx.Err()
}

// registerWebhook tells Telegram where to deliver updates, using the
// configured public_url and secret_token. It must run only after the local
// listener is bound, and a failure is returned so Start can abort without
// leaving a running listener that receives nothing.
func (b *Bot) registerWebhook(ctx context.Context) error {
	url := strings.TrimSpace(b.cfg.Webhook.PublicURL)
	if err := config.ValidatePublicURL(url); err != nil {
		return err
	}
	if _, err := b.client.SetWebhook(ctx, &tgbot.SetWebhookParams{
		URL:         url,
		SecretToken: b.cfg.Webhook.SecretToken,
	}); err != nil {
		return fmt.Errorf("setWebhook %q: %w", url, err)
	}
	b.log.Info("webhook registered", "url", url)
	return nil
}

func (b *Bot) startWebhook(ctx context.Context) error {
	// Bind the listener first so a bind failure is reported to the caller
	// synchronously and never leaves a remote webhook pointing at this
	// service. ListenAndServe in a goroutine would only log the error and
	// leave Start blocked on ctx.Done.
	ln, err := net.Listen("tcp", b.cfg.Webhook.Listen)
	if err != nil {
		return fmt.Errorf("webhook listen %q: %w", b.cfg.Webhook.Listen, err)
	}
	// Register with Telegram only after the listener is bound. If registration
	// fails, close the listener so no running listener is left behind while the
	// remote webhook still points at this service.
	if err := b.registerWebhook(ctx); err != nil {
		_ = ln.Close()
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc(b.cfg.Webhook.Path, b.client.WebhookHandler())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		b.log.Info("webhook listening", "addr", ln.Addr().String(), "path", b.cfg.Webhook.Path)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			b.log.Error("webhook server error", "err", err)
		}
	}()
	go b.client.StartWebhook(ctx)
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return ctx.Err()
}
