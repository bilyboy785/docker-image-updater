package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
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
		os.Exit(configErrorExitCode(os.Args[1:]))
	}
	if cfg.HealthCheck {
		if err := runHealthcheck(cfg, 3*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		return
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
	dc := docker.New(cfg.Socket)
	// The container's own hostname is its id: use HOST_NAME, else ask the daemon for the hostname of the host.
	hostName := cfg.HostName
	if hostName == "" {
		hctx, hcancel := context.WithTimeout(context.Background(), 10*time.Second)
		var herr error
		hostName, herr = dc.HostName(hctx)
		hcancel()
		if herr != nil {
			log.Warn("docker host name unavailable, notifications will not name the host (set HOST_NAME to force it)", "error", herr)
		}
	}
	u := updater.New(dc, notifier, st, host, cfg.DryRun, log).WithHostName(hostName).WithRegistryAuth(creds).WithHealthWait(cfg.HealthWait).WithCleanupImages(cfg.CleanupImages)
	if cfg.DockerConfig != "" {
		log.Info("registry credentials enabled", "config_dir", cfg.DockerConfig)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sched := updater.NewScheduler(u.RunOnce, cfg.Interval, log).WithContainerRun(u.RunContainer).WithHooks(st.SetScanning, st.SetNextScan)

	if cfg.WebEnabled {
		go func() {
			log.Info("web server listening", "addr", cfg.WebAddr)
			if err := web.Serve(ctx, cfg.WebAddr, web.Handler(st.Snapshot, func(r web.ScanRequest) web.ScanResult { return triggerScan(sched, r) }, web.WithHealth(cfg.Interval))); err != nil {
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

// configErrorExitCode is the exit code for a configuration error: 2, or 1 in -healthcheck mode,
// where every failure, a misconfigured probe included, must read as "unhealthy" (exit 1).
func configErrorExitCode(args []string) int {
	for _, a := range args {
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if len(a)-len(strings.TrimLeft(a, "-")) > 2 || name != "healthcheck" || !strings.HasPrefix(a, "-") {
			continue
		}
		if !hasValue {
			return 1
		}
		if on, err := strconv.ParseBool(value); err == nil && on {
			return 1
		}
	}
	return 2
}

// runHealthcheck probes the local web server's /healthz. With the web server disabled there is
// nothing to check and it succeeds.
func runHealthcheck(cfg config.Config, timeout time.Duration) error {
	if !cfg.WebEnabled {
		return nil
	}
	return web.CheckHealth(cfg.WebAddr, timeout)
}

// tzUnrecognised reports whether the TZ variable is set to a zone the runtime could not load and
// silently replaced by UTC.
func tzUnrecognised(tz, local string) bool {
	tz = strings.TrimPrefix(tz, ":") // POSIX allows a leading colon, as in TZ=":UTC"
	return tz != "" && tz != "UTC" && tz != "Etc/UTC" && local == "UTC"
}

// triggerScan adapts the scheduler to the web package.
func triggerScan(s *updater.Scheduler, req web.ScanRequest) web.ScanResult {
	var r updater.TriggerResult
	if req.Container != "" {
		r = s.TriggerContainer(req.Container)
	} else {
		r = s.Trigger()
	}
	switch r.Status {
	case updater.Started:
		return web.ScanResult{Status: web.ScanStarted}
	case updater.AlreadyRunning:
		return web.ScanResult{Status: web.ScanAlreadyRunning}
	default:
		return web.ScanResult{Status: web.ScanTooSoon, RetryAfter: r.RetryAfter}
	}
}
