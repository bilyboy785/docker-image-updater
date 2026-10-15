package updater

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type schedEnv struct {
	s         *Scheduler
	clock     *fakeClock
	started   chan struct{} // one value per scan start
	release   chan struct{} // one value lets a blocked scan finish
	block     bool
	scanning  chan bool
	nexts     chan time.Time
	targets   chan string // one value (the container name) per targeted scan start
	targetErr error
	runErr    error
	cancel    context.CancelFunc
	done      chan struct{}
}

func newSchedEnv(t *testing.T, interval time.Duration, block bool) *schedEnv {
	return newSchedEnvErr(t, interval, block, nil)
}

func newSchedEnvErr(t *testing.T, interval time.Duration, block bool, runErr error) *schedEnv {
	t.Helper()
	e := &schedEnv{
		clock:    &fakeClock{t: time.Unix(1_000_000, 0)},
		started:  make(chan struct{}, 100),
		release:  make(chan struct{}, 100),
		block:    block,
		runErr:   runErr,
		scanning: make(chan bool, 100),
		nexts:    make(chan time.Time, 100),
		targets:  make(chan string, 100),
		done:     make(chan struct{}),
	}
	run := func(ctx context.Context) error {
		e.started <- struct{}{}
		if e.block {
			select {
			case <-e.release:
			case <-ctx.Done():
			}
		}
		return e.runErr
	}
	runOne := func(ctx context.Context, name string) error {
		e.targets <- name
		if e.block {
			select {
			case <-e.release:
			case <-ctx.Done():
			}
		}
		return e.targetErr
	}
	e.s = NewScheduler(run, interval, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithContainerRun(runOne).
		WithHooks(func(v bool) { e.scanning <- v }, func(next time.Time) { e.nexts <- next })
	e.s.now = e.clock.Now
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	go func() { e.s.Run(ctx); close(e.done) }()
	t.Cleanup(func() { cancel(); <-e.done })
	return e
}

func waitCh[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
		panic("unreachable")
	}
}

func TestSchedulerRunsInitialScan(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	if v := waitCh(t, e.scanning, "scanning=true"); !v {
		t.Fatal("scanning must be true at scan start")
	}
	if v := waitCh(t, e.scanning, "scanning=false"); v {
		t.Fatal("scanning must be false after the scan")
	}
	if next := waitCh(t, e.nexts, "next scheduled"); !next.Equal(e.clock.Now().Add(time.Hour)) {
		t.Fatalf("next = %v", next)
	}
}

func TestSchedulerTriggerWhileIdleStartsScan(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	waitCh(t, e.nexts, "initial end")
	e.clock.Advance(time.Minute)
	if r := e.s.Trigger(); r.Status != Started {
		t.Fatalf("got %+v", r)
	}
	waitCh(t, e.started, "manual scan")
}

func TestSchedulerTriggerWhileRunning(t *testing.T) {
	e := newSchedEnv(t, time.Hour, true)
	waitCh(t, e.started, "initial scan")
	if r := e.s.Trigger(); r.Status != AlreadyRunning {
		t.Fatalf("got %+v", r)
	}
	e.release <- struct{}{}
	waitCh(t, e.nexts, "end of scan")
}

func TestSchedulerTooSoonAndAcceptedAfterSpacing(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	waitCh(t, e.nexts, "initial end")
	e.clock.Advance(10 * time.Second)
	r := e.s.Trigger()
	if r.Status != TooSoon || r.RetryAfter != 20*time.Second {
		t.Fatalf("got %+v", r)
	}
	e.clock.Advance(20 * time.Second)
	if r := e.s.Trigger(); r.Status != Started {
		t.Fatalf("got %+v", r)
	}
	waitCh(t, e.started, "manual scan")
}

func TestSchedulerSpacingCountsFromScanStart(t *testing.T) {
	e := newSchedEnv(t, time.Hour, true)
	waitCh(t, e.started, "initial scan")
	e.clock.Advance(40 * time.Second) // the scan is long: the spacing is already over once it ends
	e.release <- struct{}{}
	waitCh(t, e.nexts, "end of scan")
	if r := e.s.Trigger(); r.Status != Started {
		t.Fatalf("got %+v", r)
	}
	waitCh(t, e.started, "manual scan")
	e.release <- struct{}{}
}

func TestSchedulerManualScanResetsTimer(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	first := waitCh(t, e.nexts, "initial end")
	e.clock.Advance(45 * time.Minute)
	if r := e.s.Trigger(); r.Status != Started {
		t.Fatalf("got %+v", r)
	}
	waitCh(t, e.started, "manual scan")
	second := waitCh(t, e.nexts, "manual end")
	if want := first.Add(45 * time.Minute); !second.Equal(want) {
		t.Fatalf("next scan not pushed back: %v, want %v", second, want)
	}
}

func TestSchedulerPeriodicScans(t *testing.T) {
	e := newSchedEnv(t, 5*time.Millisecond, false)
	waitCh(t, e.started, "first scan")
	waitCh(t, e.started, "second scan")
	waitCh(t, e.started, "third scan")
}

func TestSchedulerErrorsDoNotStopLoop(t *testing.T) {
	e := newSchedEnvErr(t, 5*time.Millisecond, false, errors.New("boom"))
	waitCh(t, e.started, "first (failing) scan")
	waitCh(t, e.started, "scan after a failing one")
	waitCh(t, e.started, "another scan after a failing one")
}

func TestSchedulerCancelStopsRunPromptly(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	waitCh(t, e.nexts, "initial end")
	e.cancel()
	waitCh(t, e.done, "Run to return")
}

func TestSchedulerCancelDuringScan(t *testing.T) {
	e := newSchedEnv(t, time.Hour, true)
	waitCh(t, e.started, "initial scan")
	e.cancel()
	waitCh(t, e.done, "Run to return")
}

func TestSchedulerSingleTokenPerTrigger(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	waitCh(t, e.nexts, "initial end")
	e.clock.Advance(time.Minute)
	e.s.Trigger()
	waitCh(t, e.started, "manual scan")
	waitCh(t, e.nexts, "manual end")
	select {
	case <-e.started:
		t.Fatal("a single trigger must run exactly one scan")
	case <-time.After(20 * time.Millisecond):
	}
}

// Targeted scans.

// retryWhileRunning calls trigger until it stops answering AlreadyRunning: the scanning flag drops
// just before the scheduler marks the scan as finished, so a trigger sent right after the flag may
// still see the end of the scan.
func retryWhileRunning(trigger func() TriggerResult) TriggerResult {
	r := trigger()
	for i := 0; i < 200 && r.Status == AlreadyRunning; i++ {
		time.Sleep(5 * time.Millisecond)
		r = trigger()
	}
	return r
}

// A targeted request accepted while the periodic scan begins is served by that full scan.
func TestSchedulerPeriodicScanServesAPendingTargetedRequest(t *testing.T) {
	var full, one int
	s := NewScheduler(func(context.Context) error { full++; return nil }, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithContainerRun(func(context.Context, string) error { one++; return nil })
	if r := s.TriggerContainer("web"); r.Status != Started {
		t.Fatalf("got %+v", r)
	}
	if err := s.scan(context.Background(), request{}); err != nil { // the periodic scan wins the race
		t.Fatal(err)
	}
	if full != 1 || one != 0 || len(s.trigger) != 0 {
		t.Fatalf("full=%d targeted=%d pending=%d", full, one, len(s.trigger))
	}
}

func noValue[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("unexpected %s", what)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestSchedulerTargetedTriggerRunsTheTargetedScanOnly(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	waitCh(t, e.nexts, "initial end")
	e.clock.Advance(10 * time.Second)
	if r := e.s.TriggerContainer("web"); r.Status != Started {
		t.Fatalf("got %+v", r)
	}
	if got := waitCh(t, e.targets, "targeted scan"); got != "web" {
		t.Fatalf("got %q", got)
	}
	noValue(t, e.started, "full scan")
}

func TestSchedulerTargetedScanUpdatesTheScanningFlagButNotTheSchedule(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	waitCh(t, e.nexts, "initial end")
	for i := 0; i < 2; i++ { // initial scan: true then false
		waitCh(t, e.scanning, "initial flag")
	}
	e.clock.Advance(10 * time.Second)
	e.s.TriggerContainer("web")
	waitCh(t, e.targets, "targeted scan")
	if v := waitCh(t, e.scanning, "flag on"); !v {
		t.Fatal("scanning must be true during a targeted scan")
	}
	if v := waitCh(t, e.scanning, "flag off"); v {
		t.Fatal("scanning must be false after a targeted scan")
	}
	noValue(t, e.nexts, "reschedule after a targeted scan")
}

func TestSchedulerTargetedScansDoNotPushBackThePeriodicScan(t *testing.T) {
	e := newSchedEnv(t, 250*time.Millisecond, false)
	waitCh(t, e.started, "initial scan")
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case <-e.started:
			return // the periodic scan ran although targeted scans kept coming
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the periodic scan was starved by targeted scans")
		}
		e.clock.Advance(time.Minute)
		e.s.TriggerContainer("web")
		time.Sleep(15 * time.Millisecond)
	}
}

func TestSchedulerTargetedSpacing(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	waitCh(t, e.nexts, "initial end")
	e.clock.Advance(3 * time.Second)
	r := e.s.TriggerContainer("web")
	if r.Status != TooSoon || r.RetryAfter != 2*time.Second {
		t.Fatalf("got %+v", r)
	}
	noValue(t, e.targets, "targeted scan")
	e.clock.Advance(2 * time.Second)
	if r := e.s.TriggerContainer("web"); r.Status != Started {
		t.Fatalf("got %+v", r)
	}
	waitCh(t, e.targets, "targeted scan")
}

func TestSchedulerSpacingIsMeasuredFromTheLastStartOfEitherKind(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	waitCh(t, e.nexts, "initial end")
	e.clock.Advance(10 * time.Second)
	if r := e.s.TriggerContainer("web"); r.Status != Started {
		t.Fatalf("got %+v", r)
	}
	waitCh(t, e.targets, "targeted scan")
	waitCh(t, e.scanning, "x")
	waitCh(t, e.scanning, "x")
	waitCh(t, e.scanning, "x")
	waitCh(t, e.scanning, "x")
	e.clock.Advance(10 * time.Second) // 10 s after the targeted scan: enough for another targeted one, not for a full one
	if r := retryWhileRunning(e.s.Trigger); r.Status != TooSoon || r.RetryAfter != 20*time.Second {
		t.Fatalf("a full forced scan keeps its 30 s rule: %+v", r)
	}
	e.clock.Advance(20 * time.Second)
	if r := e.s.Trigger(); r.Status != Started {
		t.Fatalf("got %+v", r)
	}
	waitCh(t, e.started, "full scan")
	// And the other way round: a targeted scan 5 s after a full one is accepted.
	waitCh(t, e.nexts, "full end")
	e.clock.Advance(5 * time.Second)
	if r := e.s.TriggerContainer("web"); r.Status != Started {
		t.Fatalf("got %+v", r)
	}
}

func TestSchedulerFullForcedScanStillResetsTheTimer(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	first := waitCh(t, e.nexts, "initial end")
	e.clock.Advance(10 * time.Second)
	e.s.TriggerContainer("web")
	waitCh(t, e.targets, "targeted scan")
	e.clock.Advance(40 * time.Second)
	retryWhileRunning(e.s.Trigger)
	waitCh(t, e.started, "full scan")
	second := waitCh(t, e.nexts, "full end")
	if want := first.Add(50 * time.Second); !second.Equal(want) {
		t.Fatalf("got %v want %v", second, want)
	}
}

func TestSchedulerAnyRunningScanRejectsTheOtherKind(t *testing.T) {
	e := newSchedEnv(t, time.Hour, true)
	waitCh(t, e.started, "initial scan")
	if r := e.s.TriggerContainer("web"); r.Status != AlreadyRunning {
		t.Fatalf("got %+v", r)
	}
	e.release <- struct{}{}
	waitCh(t, e.nexts, "end of scan")
	e.clock.Advance(time.Minute)
	if r := e.s.TriggerContainer("web"); r.Status != Started {
		t.Fatalf("got %+v", r)
	}
	waitCh(t, e.targets, "targeted scan")
	if r := e.s.Trigger(); r.Status != AlreadyRunning {
		t.Fatalf("got %+v", r)
	}
	if r := e.s.TriggerContainer("other"); r.Status != AlreadyRunning {
		t.Fatalf("got %+v", r)
	}
	e.release <- struct{}{}
}

func TestSchedulerTargetedTriggerRunsExactlyOneScan(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	waitCh(t, e.nexts, "initial end")
	e.clock.Advance(time.Minute)
	e.s.TriggerContainer("web")
	waitCh(t, e.targets, "targeted scan")
	noValue(t, e.targets, "second targeted scan")
	noValue(t, e.started, "full scan")
}

func TestSchedulerTargetedErrorsDoNotStopTheLoop(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	e.targetErr = ErrContainerNotFound
	waitCh(t, e.started, "initial scan")
	waitCh(t, e.nexts, "initial end")
	e.clock.Advance(time.Minute)
	e.s.TriggerContainer("gone")
	waitCh(t, e.targets, "targeted scan")
	e.clock.Advance(time.Minute)
	if r := retryWhileRunning(e.s.Trigger); r.Status != Started {
		t.Fatalf("got %+v", r)
	}
	waitCh(t, e.started, "full scan after a targeted error")
}

func TestSchedulerCancelDuringTargetedScan(t *testing.T) {
	e := newSchedEnv(t, time.Hour, true)
	waitCh(t, e.started, "initial scan")
	e.release <- struct{}{}
	waitCh(t, e.nexts, "initial end")
	e.clock.Advance(time.Minute)
	e.s.TriggerContainer("web")
	waitCh(t, e.targets, "targeted scan")
	e.cancel()
	waitCh(t, e.done, "Run to return")
}
