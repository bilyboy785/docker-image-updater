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

// maxPendingImages caps the number of distinct images waiting for an image scan.
const maxPendingImages = 64

// TriggerStatus is the outcome of a manual scan request.
type TriggerStatus int

const (
	// Started means the request was accepted: a scan will run as soon as possible.
	Started TriggerStatus = iota
	// AlreadyRunning means a scan is in progress (or already accepted).
	AlreadyRunning
	// TooSoon means the previous scan started less than the minimum spacing ago.
	TooSoon
	// Queued means an image request was accepted: it is served as soon as the scan in progress, if any, ends.
	Queued
	// QueueFull means too many distinct images are already waiting for a scan.
	QueueFull
)

// TriggerResult is returned by Scheduler.Trigger. RetryAfter is only set for TooSoon.
type TriggerResult struct {
	Status     TriggerStatus
	RetryAfter time.Duration
}

// request is what the goroutine running Run is asked to scan: everything, one container, or the
// containers running one image (image is then set, and targeted too).
type request struct {
	container string
	image     string
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
	runImage    func(context.Context, string) error
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
	pending   []string      // images waiting for an image scan, in arrival order, without duplicates
	imageWake chan struct{} // holds a value exactly when pending is not empty
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
		imageWake:   make(chan struct{}, 1),
		runImage:    func(context.Context, string) error { return errors.New("scanning one image is not supported") },
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

// WithImageRun sets the function updating the containers of one image, used by TriggerImage.
// It must be called before Run.
func (s *Scheduler) WithImageRun(runImage func(context.Context, string) error) *Scheduler {
	s.runImage = runImage
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

// TriggerImage asks for the containers running ref to be updated, without blocking. Unlike the manual
// scans it is never refused because of the spacing or because a scan is running: the request waits for its
// turn, so that a burst of pushes is served entirely. A request for an image already waiting is merged into
// it, since the scan will pull the newest version anyway. The periodic schedule is not moved. ref is
// compared as is, pass it through registry.CanonicalRef first.
func (s *Scheduler) TriggerImage(ref string) TriggerResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pending {
		if p == ref {
			return TriggerResult{Status: Queued}
		}
	}
	if len(s.pending) >= maxPendingImages {
		return TriggerResult{Status: QueueFull}
	}
	s.pending = append(s.pending, ref)
	select {
	case s.imageWake <- struct{}{}:
	default:
	}
	return TriggerResult{Status: Queued}
}

// popImage removes and returns the image waiting the longest.
func (s *Scheduler) popImage() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return "", false
	}
	ref := s.pending[0]
	s.pending = append([]string(nil), s.pending[1:]...)
	if len(s.pending) == 0 {
		select { // keep imageWake set only while something is pending
		case <-s.imageWake:
		default:
		}
	}
	return ref, true
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

// begin marks a scan as running (it may already be, when a trigger was accepted). dropPending tells
// whether the scan serves the manual request that may be waiting: not the case of an image scan, which
// only covers its own image.
func (s *Scheduler) begin(dropPending bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		s.running = true
		s.lastStart = s.now()
	}
	// A request accepted at the same time as a periodic scan is served by that scan: a full scan covers
	// the targeted container too, and the request must not run a second scan right after.
	if dropPending {
		select {
		case <-s.trigger:
		default:
		}
	}
}

func (s *Scheduler) end() {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
}

// scan runs one scan; the running state and the scanning flag are always reset, even if run panics.
func (s *Scheduler) scan(ctx context.Context, req request) error {
	s.begin(req.image == "")
	s.onScanning(true)
	defer func() {
		s.onScanning(false)
		s.end()
	}()
	if req.image != "" {
		return s.runImage(ctx, req.image)
	}
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
	wait:
		for {
			if ref, ok := s.popImage(); ok { // an explicit request for an image comes first
				req = request{image: ref, targeted: true}
				break wait
			}
			select {
			case <-ctx.Done():
				s.log.Info("shutting down")
				return
			case <-timer.C:
				req = request{}
				break wait
			case req = <-s.trigger:
				break wait
			case <-s.imageWake: // popImage picks the request up on the next turn
			}
		}
	}
}
