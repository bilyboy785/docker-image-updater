package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bilyboy785/docker-image-updater/internal/config"
	"github.com/bilyboy785/docker-image-updater/internal/docker"
	"github.com/bilyboy785/docker-image-updater/internal/notify"
	"github.com/bilyboy785/docker-image-updater/internal/stats"
	"github.com/bilyboy785/docker-image-updater/internal/updater"
	"github.com/bilyboy785/docker-image-updater/internal/web"
)

var version = "dev"

func main() {
	cfg, err := config.Load(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(config.Usage())
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	mode := "active"
	if cfg.DryRun {
		mode = "dry-run"
	}
	st, err := stats.New(version, mode, cfg.StateFile)
	if err != nil {
		log.Warn("stats state", "error", err)
	}

	var notifier notify.Notifier = notify.Nop{}
	if cfg.TelegramToken != "" {
		notifier = notify.NewTelegram(cfg.TelegramToken, cfg.TelegramChat)
	}

	host, _ := os.Hostname()
	u := updater.New(docker.New(cfg.Socket), notifier, st, host, cfg.DryRun, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.WebEnabled {
		go func() {
			log.Info("web server listening", "addr", cfg.WebAddr)
			if err := web.Serve(ctx, cfg.WebAddr, web.Handler(st.Snapshot)); err != nil {
				log.Error("web server stopped", "error", err)
			}
		}()
	}

	log.Info("docker-image-updater started", "version", version, "mode", mode, "interval", cfg.Interval.String())
	for {
		if err := u.RunOnce(ctx); err != nil && ctx.Err() == nil {
			log.Error("scan failed", "error", err)
		}
		st.SetNextScan(time.Now().Add(cfg.Interval))
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return
		case <-time.After(cfg.Interval):
		}
	}
}
