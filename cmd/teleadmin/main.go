// tele-admin is a Telegram group management bot.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/viceor/tele-admin/internal/bot"
	"github.com/viceor/tele-admin/internal/config"
	"github.com/viceor/tele-admin/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to configuration file")
	dbDiagnose := flag.Bool("db-diagnose", false, "report template record ownership in db.path, then exit (stop the service first)")
	dbQuarantine := flag.Bool("db-quarantine-ownerless-templates", false, "move ownerless template records to the templates_quarantine table (content kept), then exit (stop the service first)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("failed to load config", "err", err)
		os.Exit(1)
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.SlogLevel()}))
	logger.Info("config loaded", "mode", cfg.Mode, "log_level", cfg.LogLevel)

	if *dbDiagnose || *dbQuarantine {
		if cfg.DB.Path == "" {
			logger.Error("db.path is not configured; nothing to inspect")
			os.Exit(1)
		}
		if err := store.RunTemplateMaintenance(cfg.DB.Path, *dbQuarantine, os.Stdout); err != nil {
			logger.Error("template maintenance failed", "err", err)
			os.Exit(1)
		}
		return
	}

	b, err := bot.New(cfg, logger)
	if err != nil {
		logger.Error("failed to create bot", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Info("starting bot")
	if err := b.Start(ctx); err != nil && err != context.Canceled {
		logger.Error("bot stopped with error", "err", err)
		os.Exit(1)
	}
	if err := b.Close(); err != nil {
		logger.Error("failed to release persistence backend", "err", err)
		os.Exit(1)
	}
	logger.Info("bot stopped")
}
