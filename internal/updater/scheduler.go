package updater

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// DefaultMinSpacing is the minimum delay between the start of the previous scan and a manually
// triggered full scan.
const DefaultMinSpacing = 30 * time.Second

// DefaultTargetedSpacing is the minimum delay between the start of the previous scan and a manually
// triggered scan of a single container.
const DefaultTargetedSpacing = 5 * time.Second

// TriggerStatus is the outcome of a manual scan request.
type TriggerStatus int

const (
	// Started means the request was accepted: a scan will run as soon as possible.
	Started TriggerStatus = iota
	// AlreadyRunning means a scan is in progress (or already accepted).
	AlreadyRunning
	// TooSoon means the previous scan started less than the minimum spacing ago.
	TooSoon
)

// TriggerResult is returned by Scheduler.Trigger. RetryAfter is only set for TooSoon.
type TriggerResult struct {
	Status     TriggerStatus
	RetryAfter time.Duration
}

// request is what the goroutine running Run is asked to scan: everything, or one container.
type request struct {
	container string
	targeted  bool
}

// Scheduler runs scans one at a time, in a single goroutine: once at start, then every interval,
// or earlier when Trigger is called. Every full scan restarts the interval; a scan of a single
// container (TriggerContainer) does not move the periodic schedule.
//
// The spacing rules are measured from the start of the previous scan, of either kind: a full forced
// scan needs DefaultMinSpacing since then, a targeted one DefaultTargetedSpacing, so the stricter limit
// that applies to the requested kind wins.
type Scheduler struct {
	run         func(context.Context) error
	runOne      func(context.Context, string) error
	interval    time.Duration
	minSpacing  time.Duration
	oneSpacing  time.Duration
	now         func() time.Time
	log         *slog.Logger
	onScanning  func(bool)
	onScheduled func(next time.Time)

	mu        sync.Mutex
	running   bool
	lastStart time.Time
	trigger   chan request
}

// NewScheduler returns a scheduler calling run for every scan.
func NewScheduler(run func(context.Context) error, interval time.Duration, log *slog.Logger) *Scheduler {
	return &Scheduler{
		run:         run,
		runOne:      func(context.Context, string) error { return errors.New("scanning one container is not supported") },
		oneSpacing:  DefaultTargetedSpacing,
		interval:    interval,
		minSpacing:  DefaultMinSpacing,
		now:         time.Now,
		log:         log,
		onScanning:  func(bool) {},
		onScheduled: func(time.Time) {},
		trigger:     make(chan request, 1),
	}
}

// WithHooks sets the callbacks invoked around each scan (onScanning) and once the next scan is planned (onScheduled).
// It must be called before Run.
func (s *Scheduler) WithHooks(onScanning func(bool), onScheduled func(next time.Time)) *Scheduler {
	s.onScanning, s.onScheduled = onScanning, onScheduled
	return s
}

// WithContainerRun sets the function scanning a single container, used by TriggerContainer.
// It must be called before Run.
func (s *Scheduler) WithContainerRun(runOne func(context.Context, string) error) *Scheduler {
	s.runOne = runOne
	return s
}

// Trigger asks for an immediate full scan without blocking. It is safe for concurrent use and only
// signals the goroutine running Run, it never runs a scan itself.
func (s *Scheduler) Trigger() TriggerResult {
	return s.request(request{}, s.minSpacing)
}

// TriggerContainer asks for an immediate scan of the container called name only, with the same guarantees
// as Trigger. The periodic schedule is not moved by that scan.
func (s *Scheduler) TriggerContainer(name string) TriggerResult {
	return s.request(request{container: name, targeted: true}, s.oneSpacing)
}

func (s *Scheduler) request(req request, spacing time.Duration) TriggerResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return TriggerResult{Status: AlreadyRunning}
	}
	now := s.now()
	if !s.lastStart.IsZero() {
		if elapsed := now.Sub(s.lastStart); elapsed < spacing {
			return TriggerResult{Status: TooSoon, RetryAfter: spacing - elapsed}
		}
	}
	// Mark the scan as running right away so that concurrent triggers see it.
	s.running = true
	s.lastStart = now
	s.trigger <- req // never blocks: the channel is drained whenever a scan begins
	return TriggerResult{Status: Started}
}

// begin marks a scan as running (it may already be, when a trigger was accepted).
func (s *Scheduler) begin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		s.running = true
		s.lastStart = s.now()
	}
	select { // a trigger accepted at the same time as a periodic scan is served by that scan
	case <-s.trigger:
	default:
	}
}

func (s *Scheduler) end() {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
}

// scan runs one scan; the running state and the scanning flag are always reset, even if run panics.
func (s *Scheduler) scan(ctx context.Context, req request) error {
	s.begin()
	s.onScanning(true)
	defer func() {
		s.onScanning(false)
		s.end()
	}()
	if req.targeted {
		return s.runOne(ctx, req.container)
	}
	return s.run(ctx)
}

// Run scans immediately, then waits for the interval or a trigger, until ctx is cancelled. The interval
// is restarted by full scans only: the timer survives the scans of a single container.
func (s *Scheduler) Run(ctx context.Context) {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	req := request{}
	for {
		err := s.scan(ctx, req)
		switch {
		case err == nil || ctx.Err() != nil:
		case errors.Is(err, ErrContainerNotFound):
			s.log.Info("scan of one container skipped", "reason", err)
		default:
			s.log.Error("scan failed", "error", err)
		}
		if !req.targeted {
			if timer != nil {
				timer.Stop()
			}
			s.onScheduled(s.now().Add(s.interval))
			timer = time.NewTimer(s.interval)
		}
		if ctx.Err() != nil {
			s.log.Info("shutting down")
			return
		}
		select {
		case <-ctx.Done():
			s.log.Info("shutting down")
			return
		case <-timer.C:
			req = request{}
		case req = <-s.trigger:
		}
	}
}
