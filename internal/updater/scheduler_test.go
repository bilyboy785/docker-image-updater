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
	s        *Scheduler
	clock    *fakeClock
	started  chan struct{} // one value per scan start
	release  chan struct{} // one value lets a blocked scan finish
	block    bool
	scanning chan bool
	nexts    chan time.Time
	runErr   error
	cancel   context.CancelFunc
	done     chan struct{}
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
	e.s = NewScheduler(run, interval, slog.New(slog.NewTextHandler(io.Discard, nil))).
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
