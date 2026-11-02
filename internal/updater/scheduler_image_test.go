package updater

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"
)

// Image scans: requested by an external caller, queued rather than refused.

func TestSchedulerImageRequestWhileIdleRunsAnImageScanAtOnce(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	waitCh(t, e.nexts, "initial end")
	// No clock advance: unlike a manual scan, an image request is never refused for being too soon.
	if r := e.s.TriggerImage("docker.io/library/app:latest"); r.Status != Queued {
		t.Fatalf("got %+v", r)
	}
	if got := waitCh(t, e.images, "image scan"); got != "docker.io/library/app:latest" {
		t.Fatalf("got %q", got)
	}
	noValue(t, e.started, "full scan")
	noValue(t, e.images, "second image scan")
}

func TestSchedulerImageRequestDoesNotMoveThePeriodicSchedule(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	waitCh(t, e.nexts, "initial end")
	e.s.TriggerImage("app:1")
	waitCh(t, e.images, "image scan")
	noValue(t, e.nexts, "reschedule after an image scan")
}

func TestSchedulerImageRequestDuringAScanIsQueuedNotRefused(t *testing.T) {
	e := newSchedEnv(t, time.Hour, true)
	waitCh(t, e.started, "initial scan")
	if r := e.s.TriggerImage("app:1"); r.Status != Queued {
		t.Fatalf("got %+v", r)
	}
	noValue(t, e.images, "image scan before the running scan ends")
	e.release <- struct{}{} // ends the initial scan
	e.release <- struct{}{} // lets the image scan finish
	if got := waitCh(t, e.images, "queued image scan"); got != "app:1" {
		t.Fatalf("got %q", got)
	}
}

func TestSchedulerQueuedImagesRunOnceEachInOrder(t *testing.T) {
	e := newSchedEnv(t, time.Hour, true)
	waitCh(t, e.started, "initial scan")
	for _, ref := range []string{"a:1", "b:1", "a:1", "c:1", "b:1"} {
		if r := e.s.TriggerImage(ref); r.Status != Queued {
			t.Fatalf("%s: got %+v", ref, r)
		}
	}
	for i := 0; i < 4; i++ {
		e.release <- struct{}{}
	}
	for _, want := range []string{"a:1", "b:1", "c:1"} {
		if got := waitCh(t, e.images, "image scan"); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	noValue(t, e.images, "duplicate image scan")
}

func TestSchedulerImageRequestIsServedAfterTheSameImageRanAlready(t *testing.T) {
	e := newSchedEnv(t, time.Hour, false)
	waitCh(t, e.started, "initial scan")
	waitCh(t, e.nexts, "initial end")
	e.s.TriggerImage("app:1")
	waitCh(t, e.images, "first image scan")
	e.s.TriggerImage("app:1") // a new push of the same image: it must run again
	waitCh(t, e.images, "second image scan")
}

func TestSchedulerImageQueueIsBounded(t *testing.T) {
	e := newSchedEnv(t, time.Hour, true)
	waitCh(t, e.started, "initial scan")
	for i := 0; i < maxPendingImages; i++ {
		if r := e.s.TriggerImage(fmt.Sprintf("app:%d", i)); r.Status != Queued {
			t.Fatalf("request %d: got %+v", i, r)
		}
	}
	if r := e.s.TriggerImage("one-too-many:1"); r.Status != QueueFull {
		t.Fatalf("got %+v", r)
	}
	if r := e.s.TriggerImage("app:0"); r.Status != Queued {
		t.Fatalf("an image already queued must be accepted again: %+v", r)
	}
}

func TestSchedulerAnImageScanRejectsManualScansWhileItRuns(t *testing.T) {
	e := newSchedEnv(t, time.Hour, true)
	waitCh(t, e.started, "initial scan")
	e.s.TriggerImage("app:1")
	e.release <- struct{}{} // ends the initial scan, the image scan starts and blocks
	waitCh(t, e.images, "image scan")
	if r := e.s.Trigger(); r.Status != AlreadyRunning {
		t.Fatalf("got %+v", r)
	}
	if r := e.s.TriggerContainer("web"); r.Status != AlreadyRunning {
		t.Fatalf("got %+v", r)
	}
	e.release <- struct{}{}
}

func TestSchedulerCancelDuringImageScan(t *testing.T) {
	e := newSchedEnv(t, time.Hour, true)
	waitCh(t, e.started, "initial scan")
	e.s.TriggerImage("app:1")
	e.release <- struct{}{}
	waitCh(t, e.images, "image scan")
	e.cancel()
	waitCh(t, e.done, "Run to return")
}

// A full scan accepted but not started yet must not be swallowed by an image scan, which does not cover it.
func TestSchedulerImageScanKeepsAPendingFullScanRequest(t *testing.T) {
	s := NewScheduler(func(context.Context) error { return nil }, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithImageRun(func(context.Context, string) error { return nil })
	if r := s.Trigger(); r.Status != Started {
		t.Fatalf("got %+v", r)
	}
	if err := s.scan(context.Background(), request{image: "app:1", targeted: true}); err != nil {
		t.Fatal(err)
	}
	if len(s.trigger) != 1 {
		t.Fatalf("the pending full scan request was dropped: %d", len(s.trigger))
	}
}

// The reverse: a periodic full scan does not make the queued image request pointless, it is not forced.
func TestSchedulerFullScanKeepsAQueuedImageRequest(t *testing.T) {
	s := NewScheduler(func(context.Context) error { return nil }, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithImageRun(func(context.Context, string) error { return nil })
	s.TriggerImage("app:1")
	if err := s.scan(context.Background(), request{}); err != nil {
		t.Fatal(err)
	}
	if ref, ok := s.popImage(); !ok || ref != "app:1" {
		t.Fatalf("the queued image request was dropped: %q %v", ref, ok)
	}
}
