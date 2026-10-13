package updater

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultMinSpacing is the minimum delay between the starts of two scans when one is triggered manually.
const DefaultMinSpacing = 30 * time.Second

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

// Scheduler runs scans one at a time, in a single goroutine: once at start, then every interval,
// or earlier when Trigger is called. Every scan restarts the interval.
type Scheduler struct {
	run         func(context.Context) error
	interval    time.Duration
	minSpacing  time.Duration
	now         func() time.Time
	log         *slog.Logger
	onScanning  func(bool)
	onScheduled func(next time.Time)

	mu        sync.Mutex
	running   bool
	lastStart time.Time
	trigger   chan struct{}
}

// NewScheduler returns a scheduler calling run for every scan.
func NewScheduler(run func(context.Context) error, interval time.Duration, log *slog.Logger) *Scheduler {
	return &Scheduler{
		run:         run,
		interval:    interval,
		minSpacing:  DefaultMinSpacing,
		now:         time.Now,
		log:         log,
		onScanning:  func(bool) {},
		onScheduled: func(time.Time) {},
		trigger:     make(chan struct{}, 1),
	}
}

// WithHooks sets the callbacks invoked around each scan (onScanning) and once the next scan is planned (onScheduled).
// It must be called before Run.
func (s *Scheduler) WithHooks(onScanning func(bool), onScheduled func(next time.Time)) *Scheduler {
	s.onScanning, s.onScheduled = onScanning, onScheduled
	return s
}

// Trigger asks for an immediate scan without blocking. It is safe for concurrent use and only
// signals the goroutine running Run, it never runs a scan itself.
func (s *Scheduler) Trigger() TriggerResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return TriggerResult{Status: AlreadyRunning}
	}
	now := s.now()
	if !s.lastStart.IsZero() {
		if elapsed := now.Sub(s.lastStart); elapsed < s.minSpacing {
			return TriggerResult{Status: TooSoon, RetryAfter: s.minSpacing - elapsed}
		}
	}
	// Mark the scan as running right away so that concurrent triggers see it.
	s.running = true
	s.lastStart = now
	s.trigger <- struct{}{} // never blocks: the channel is drained whenever a scan begins
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

// Run scans immediately, then waits for the interval or a trigger, until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		s.begin()
		s.onScanning(true)
		err := s.run(ctx)
		s.onScanning(false)
		s.end()
		if err != nil && ctx.Err() == nil {
			s.log.Error("scan failed", "error", err)
		}
		s.onScheduled(s.now().Add(s.interval))
		if ctx.Err() != nil {
			s.log.Info("shutting down")
			return
		}
		timer := time.NewTimer(s.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			s.log.Info("shutting down")
			return
		case <-timer.C:
		case <-s.trigger:
			timer.Stop()
		}
	}
}
