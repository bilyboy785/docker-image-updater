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
	_ "time/tzdata" // embedded zone database: the scratch image has none, TZ must still work

	"github.com/bilyboy785/docker-image-updater/internal/config"
	"github.com/bilyboy785/docker-image-updater/internal/docker"
	"github.com/bilyboy785/docker-image-updater/internal/notify"
	"github.com/bilyboy785/docker-image-updater/internal/registry"
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

	if tzUnrecognised(os.Getenv("TZ"), time.Local.String()) {
		log.Warn("TZ value not recognised, using UTC", "TZ", os.Getenv("TZ"))
	}

	mode := "active"
	if cfg.DryRun {
		mode = "dry-run"
	}
	st, err := stats.New(version, mode, cfg.StateFile)
	if err != nil {
		log.Warn("stats state", "error", err)
	}

	var notifier notify.Notifier = notify.Nop{}
	var queue *notify.Queue
	if cfg.TelegramToken != "" {
		queue = notify.NewQueue(notify.NewTelegram(cfg.TelegramToken, cfg.TelegramChat), log)
		notifier = queue
	}

	host, _ := os.Hostname()
	creds := registry.NewStore(cfg.DockerConfig, func(msg string) { log.Warn(msg) })
	u := updater.New(docker.New(cfg.Socket), notifier, st, host, cfg.DryRun, log).WithRegistryAuth(creds).WithHealthWait(cfg.HealthWait)
	if cfg.DockerConfig != "" {
		log.Info("registry credentials enabled", "config_dir", cfg.DockerConfig)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sched := updater.NewScheduler(u.RunOnce, cfg.Interval, log).WithHooks(st.SetScanning, st.SetNextScan)

	if cfg.WebEnabled {
		go func() {
			log.Info("web server listening", "addr", cfg.WebAddr)
			if err := web.Serve(ctx, cfg.WebAddr, web.Handler(st.Snapshot, func() web.ScanResult { return triggerScan(sched) })); err != nil {
				log.Error("web server stopped", "error", err)
			}
		}()
	}

	log.Info("docker-image-updater started", "version", version, "mode", mode, "interval", cfg.Interval.String())
	sched.Run(ctx)

	// Restore default signal handling so that a second SIGINT/SIGTERM during
	// the flush terminates the process.
	stop()

	// Best-effort flush of pending notifications. main returns normally after
	// this point (no os.Exit), so nothing skips it.
	if queue != nil {
		queue.Close(10 * time.Second)
	}
}

// tzUnrecognised reports whether the TZ variable is set to a zone the runtime could not load and
// silently replaced by UTC.
func tzUnrecognised(tz, local string) bool {
	return tz != "" && tz != "UTC" && tz != "Etc/UTC" && local == "UTC"
}

// triggerScan adapts the scheduler to the web package.
func triggerScan(s *updater.Scheduler) web.ScanResult {
	r := s.Trigger()
	switch r.Status {
	case updater.Started:
		return web.ScanResult{Status: web.ScanStarted}
	case updater.AlreadyRunning:
		return web.ScanResult{Status: web.ScanAlreadyRunning}
	default:
		return web.ScanResult{Status: web.ScanTooSoon, RetryAfter: r.RetryAfter}
	}
}
