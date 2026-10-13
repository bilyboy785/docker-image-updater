package updater

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bilyboy785/docker-image-updater/internal/docker"
	"github.com/bilyboy785/docker-image-updater/internal/stats"
)

// gateClock makes the health gate deterministic: sleeping only moves the clock forward.
type gateClock struct{ t time.Time }

func (c *gateClock) now() time.Time                           { return c.t }
func (c *gateClock) sleep(_ context.Context, d time.Duration) { c.t = c.t.Add(d) }

type probe struct {
	info *docker.ContainerInfo
	err  error
}

func state(running, restarting bool, exit int, health string, restarts int) probe {
	info := &docker.ContainerInfo{RestartCount: restarts}
	info.State.Running = running
	info.State.Restarting = restarting
	info.State.ExitCode = exit
	switch {
	case restarting:
		info.State.Status = "restarting"
	case running:
		info.State.Status = "running"
	default:
		info.State.Status = "exited"
	}
	if health != "" {
		info.State.Health = &struct{ Status string }{health}
	}
	return probe{info: info}
}

func running(health string) probe { return state(true, false, 0, health, 0) }

// gateSetup builds an updater whose new container is inspected through the given probes (the last
// one repeats forever) with a fake clock, and a health wait of `wait`.
func gateSetup(wait time.Duration, probes ...probe) (*Updater, *fakeDocker, *fakeNotifier, *stats.Stats, *gateClock) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	f.inspectNew = func(n int) (*docker.ContainerInfo, error) {
		p := probes[len(probes)-1]
		if n <= len(probes) {
			p = probes[n-1]
		}
		return p.info, p.err
	}
	u, n, s := newUpdater(f, false)
	clk := &gateClock{t: time.Unix(1000, 0)}
	u.hnow, u.hsleep = clk.now, clk.sleep
	u.WithHealthWait(wait)
	return u, f, n, s, clk
}

const beforeGate = "pull nginx:latest|rename a a_old|stop a|create a|start newid"

func indexOf(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}

func TestGateHealthyQuicklyKeepsUpdateAndRemovesOldAfterTheGate(t *testing.T) {
	u, f, n, s, _ := gateSetup(60*time.Second, running("starting"), running("healthy"))
	u.RunOnce(context.Background())
	want := beforeGate + "|inspect newid|inspect newid|remove a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	snap := s.Snapshot()
	if snap.Updates != 1 || snap.Rollbacks != 0 || snap.Events[0].Status != stats.StatusUpdated {
		t.Fatalf("%+v", snap)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "Updated") || strings.Contains(n.msgs[0], "still starting") {
		t.Fatalf("%v", n.msgs)
	}
}

func TestGateUnhealthyRollsBackAndNeverRemovesTheOldContainer(t *testing.T) {
	u, f, n, s, _ := gateSetup(60*time.Second, running("starting"), running("unhealthy"))
	u.RunOnce(context.Background())
	want := beforeGate + "|inspect newid|inspect newid|remove newid|rename a a|start a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if indexOf(f.calls, "remove a") != -1 {
		t.Fatalf("the old container must never be removed: %v", f.calls)
	}
	snap := s.Snapshot()
	ev := snap.Events[0]
	if snap.Updates != 0 || snap.Rollbacks != 1 || ev.Status != stats.StatusRolledBack ||
		!strings.Contains(ev.Error, "new container failed health gate: ") || !strings.Contains(ev.Error, "unhealthy") {
		t.Fatalf("%+v", snap)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "rolled back") ||
		!strings.Contains(n.msgs[0], "Rolled back to the previous image: ") || !strings.Contains(n.msgs[0], "unhealthy") {
		t.Fatalf("%v", n.msgs)
	}
}

func TestGateStillStartingAtTheDeadlineIsAcceptedWithANote(t *testing.T) {
	u, f, n, s, clk := gateSetup(5*time.Second, running("starting"))
	start := clk.t
	u.RunOnce(context.Background())
	if f.calls[len(f.calls)-1] != "remove a" || indexOf(f.calls, "remove newid") != -1 {
		t.Fatalf("update must be kept: %v", f.calls)
	}
	if got := clk.t.Sub(start); got != 5*time.Second {
		t.Fatalf("waited %v", got)
	}
	if s.Snapshot().Events[0].Status != stats.StatusUpdated {
		t.Fatalf("%+v", s.Snapshot())
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "healthcheck still starting after 5s, update kept; raise HEALTH_WAIT_SECONDS for slow starters") {
		t.Fatalf("%v", n.msgs)
	}
}

func TestGateNoHealthcheckStableForTenSecondsIsOK(t *testing.T) {
	u, f, _, s, clk := gateSetup(60*time.Second, running(""))
	start := clk.t
	u.RunOnce(context.Background())
	if got := clk.t.Sub(start); got != 10*time.Second {
		t.Fatalf("stability window: %v", got)
	}
	if f.calls[len(f.calls)-1] != "remove a" || s.Snapshot().Updates != 1 {
		t.Fatalf("%v %+v", f.calls, s.Snapshot())
	}
}

func TestGateNoHealthcheckStabilityWindowIsCappedByTheWait(t *testing.T) {
	u, _, _, s, clk := gateSetup(3*time.Second, running(""))
	start := clk.t
	u.RunOnce(context.Background())
	if got := clk.t.Sub(start); got != 3*time.Second || s.Snapshot().Updates != 1 {
		t.Fatalf("%v %+v", got, s.Snapshot())
	}
}

func TestGateRestartCountIncreaseFails(t *testing.T) {
	u, f, _, s, _ := gateSetup(60*time.Second,
		state(true, false, 0, "", 0), state(true, false, 0, "", 0), state(true, false, 0, "", 1))
	u.RunOnce(context.Background())
	ev := s.Snapshot().Events[0]
	if ev.Status != stats.StatusRolledBack || !strings.Contains(ev.Error, "restart") || indexOf(f.calls, "remove a") != -1 {
		t.Fatalf("%+v %v", ev, f.calls)
	}
}

func TestGateRestartingFails(t *testing.T) {
	u, _, _, s, _ := gateSetup(60*time.Second, running(""), state(false, true, 1, "", 0))
	u.RunOnce(context.Background())
	if ev := s.Snapshot().Events[0]; ev.Status != stats.StatusRolledBack || !strings.Contains(ev.Error, "restarting") {
		t.Fatalf("%+v", ev)
	}
}

func TestGateExitCodeZeroIsAGracefulOneShot(t *testing.T) {
	u, f, _, s, _ := gateSetup(60*time.Second, state(false, false, 0, "", 0))
	u.RunOnce(context.Background())
	if f.calls[len(f.calls)-1] != "remove a" || s.Snapshot().Updates != 1 {
		t.Fatalf("%v %+v", f.calls, s.Snapshot())
	}
}

func TestGateNonZeroExitFails(t *testing.T) {
	u, f, _, s, _ := gateSetup(60*time.Second, running(""), state(false, false, 3, "", 0))
	u.RunOnce(context.Background())
	ev := s.Snapshot().Events[0]
	if ev.Status != stats.StatusRolledBack || !strings.Contains(ev.Error, "exit code 3") || indexOf(f.calls, "remove a") != -1 {
		t.Fatalf("%+v %v", ev, f.calls)
	}
}

func TestGateExitWhileHealthcheckIsStartingFails(t *testing.T) {
	u, _, _, s, _ := gateSetup(60*time.Second, running("starting"), state(false, false, 0, "starting", 0))
	u.RunOnce(context.Background())
	if ev := s.Snapshot().Events[0]; ev.Status != stats.StatusRolledBack || !strings.Contains(ev.Error, "exited") {
		t.Fatalf("%+v", ev)
	}
}

func TestGateVanishedContainerFails(t *testing.T) {
	u, _, _, s, _ := gateSetup(60*time.Second, running(""), probe{err: &docker.APIError{Status: 404, Message: "no such container"}})
	u.RunOnce(context.Background())
	if ev := s.Snapshot().Events[0]; ev.Status != stats.StatusRolledBack || !strings.Contains(ev.Error, "disappeared") {
		t.Fatalf("%+v", ev)
	}
}

func TestGateToleratesThreeConsecutiveInspectErrors(t *testing.T) {
	boom := probe{err: errors.New("daemon busy")}
	u, _, _, s, _ := gateSetup(60*time.Second, boom, boom, boom, running("healthy"))
	u.RunOnce(context.Background())
	if s.Snapshot().Updates != 1 {
		t.Fatalf("%+v", s.Snapshot())
	}
}

func TestGateFailsOnFourConsecutiveInspectErrors(t *testing.T) {
	boom := probe{err: errors.New("daemon busy")}
	u, f, _, s, _ := gateSetup(60*time.Second, boom, boom, boom, boom, running("healthy"))
	u.RunOnce(context.Background())
	ev := s.Snapshot().Events[0]
	if ev.Status != stats.StatusRolledBack || !strings.Contains(ev.Error, "daemon busy") || f.inspectNewCalls != 4 {
		t.Fatalf("%+v calls=%d", ev, f.inspectNewCalls)
	}
}

func TestGateErrorStreakIsResetBySuccess(t *testing.T) {
	boom := probe{err: errors.New("daemon busy")}
	u, _, _, s, _ := gateSetup(60*time.Second, boom, boom, boom, running("starting"), boom, boom, boom, running("healthy"))
	u.RunOnce(context.Background())
	if s.Snapshot().Updates != 1 {
		t.Fatalf("%+v", s.Snapshot())
	}
}

func TestGateDisabledSkipsInspection(t *testing.T) {
	u, f, _, s, _ := gateSetup(0, running("unhealthy"))
	u.RunOnce(context.Background())
	if got := strings.Join(f.calls, "|"); got != fullSequence || f.inspectNewCalls != 0 {
		t.Fatalf("got  %s\nwant %s", got, fullSequence)
	}
	if s.Snapshot().Updates != 1 {
		t.Fatalf("%+v", s.Snapshot())
	}
}

// cancelOnInspectNew cancels the caller context from inside the inspection number n of the new container.
func cancelOnInspectNew(f *fakeDocker, cancel func(), n int) {
	inner := f.inspectNew
	f.inspectNew = func(i int) (*docker.ContainerInfo, error) {
		if i == n {
			cancel()
		}
		return inner(i)
	}
}

func TestGateAbortsAndRollsBackWhenTheUpdaterIsStoppedMidGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	u, f, n, s, _ := gateSetup(60*time.Second, running("starting"), running("healthy"))
	cancelOnInspectNew(f, cancel, 1)
	u.RunOnce(ctx)
	want := beforeGate + "|inspect newid|remove newid|rename a a|start a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	ev := s.Snapshot().Events[0]
	if ev.Status != stats.StatusRolledBack || !strings.Contains(ev.Error, "updater stopping during health gate") {
		t.Fatalf("%+v", ev)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "Rolled back to the previous image: updater stopping during health gate") {
		t.Fatalf("the outcome must be notified: %v", n.msgs)
	}
}

func TestGateAbortsAtOnceWhenTheUpdaterWasAlreadyStoppingWhenItStarted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	u, f, n, s, _ := gateSetup(60*time.Second, running("starting"), running("healthy"))
	u.docker = &cancelOnStart{fakeDocker: f, cancel: cancel}
	u.RunOnce(ctx)
	want := beforeGate + "|remove newid|rename a a|start a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	ev := s.Snapshot().Events[0]
	if ev.Status != stats.StatusRolledBack || !strings.Contains(ev.Error, "updater stopping during health gate") {
		t.Fatalf("%+v", ev)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "will not be retried until a new image is published or the updater is restarted") {
		t.Fatalf("%v", n.msgs)
	}
}

func TestGateVerdictWinsOverACancelDuringTheLastInspection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	u, f, _, s, _ := gateSetup(60*time.Second, running("starting"), running("healthy"))
	cancelOnInspectNew(f, cancel, 2)
	u.RunOnce(ctx)
	if f.calls[len(f.calls)-1] != "remove a" || s.Snapshot().Updates != 1 {
		t.Fatalf("%v %+v", f.calls, s.Snapshot())
	}
}

func TestGateNonRunningStatesOtherThanExitedFail(t *testing.T) {
	for _, status := range []string{"created", "dead", "removing", "paused"} {
		p := state(false, false, 0, "", 0)
		p.info.State.Status = status
		u, _, _, s, _ := gateSetup(60*time.Second, p)
		u.RunOnce(context.Background())
		if ev := s.Snapshot().Events[0]; ev.Status != stats.StatusRolledBack || !strings.Contains(ev.Error, status) {
			t.Errorf("%s: %+v", status, ev)
		}
	}
}

func TestGateTreatsEmptyOrNoneHealthAsNoHealthcheck(t *testing.T) {
	for _, h := range []string{"", "none"} {
		p := running("")
		if h != "" {
			p = running(h)
		} else {
			p.info.State.Health = &struct{ Status string }{""}
		}
		u, _, _, s, clk := gateSetup(60*time.Second, p)
		start := clk.t
		u.RunOnce(context.Background())
		if got := clk.t.Sub(start); got != 10*time.Second || s.Snapshot().Updates != 1 {
			t.Errorf("%q: waited %v, %+v", h, got, s.Snapshot())
		}
	}
}

func TestGateTransientErrorOnTheLastPollKeepsTheLastObservation(t *testing.T) {
	boom := probe{err: errors.New("daemon busy")}
	u, f, n, s, _ := gateSetup(3*time.Second, running("starting"), running("starting"), running("starting"), boom)
	u.RunOnce(context.Background())
	if f.calls[len(f.calls)-1] != "remove a" || s.Snapshot().Updates != 1 {
		t.Fatalf("%v %+v", f.calls, s.Snapshot())
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "still starting after 3s") {
		t.Fatalf("%v", n.msgs)
	}
}

func TestGateWithoutAnyValidObservationFailsAtTheDeadline(t *testing.T) {
	boom := probe{err: errors.New("daemon busy")}
	u, _, _, s, _ := gateSetup(2*time.Second, boom)
	u.RunOnce(context.Background())
	if ev := s.Snapshot().Events[0]; ev.Status != stats.StatusRolledBack || !strings.Contains(ev.Error, "daemon busy") {
		t.Fatalf("%+v", ev)
	}
}

func TestGateRestartBeforeTheFirstObservationCounts(t *testing.T) {
	u, _, _, s, _ := gateSetup(60*time.Second, state(true, false, 0, "", 1))
	u.RunOnce(context.Background())
	if ev := s.Snapshot().Events[0]; ev.Status != stats.StatusRolledBack || !strings.Contains(ev.Error, "restart") {
		t.Fatalf("%+v", ev)
	}
}

func TestGateStabilityWindowStartsAtTheFirstSuccessfulObservation(t *testing.T) {
	boom := probe{err: errors.New("daemon busy")}
	u, _, _, s, clk := gateSetup(60*time.Second, boom, boom, running(""))
	start := clk.t
	u.RunOnce(context.Background())
	if got := clk.t.Sub(start); got != 12*time.Second || s.Snapshot().Updates != 1 {
		t.Fatalf("waited %v, %+v", got, s.Snapshot())
	}
}

// A defective image must not be retried at every scan.

func TestGateRollbackIsRememberedAndTheSameImageIsNotRetried(t *testing.T) {
	u, f, n, s, _ := gateSetup(60*time.Second, running("unhealthy"))
	u.RunOnce(context.Background())
	f.calls = nil
	u.RunOnce(context.Background())
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "pull") {
			t.Fatalf("no recreate cycle expected for a known bad image: %v", f.calls)
		}
	}
	snap := s.Snapshot()
	if snap.Rollbacks != 1 || len(snap.Events) != 1 || len(n.msgs) != 1 {
		t.Fatalf("%+v %v", snap, n.msgs)
	}
}

func TestGateANewImageIDIsRetried(t *testing.T) {
	u, f, _, s, _ := gateSetup(60*time.Second, running("unhealthy"))
	u.RunOnce(context.Background())
	f.images["nginx:latest"] = "sha256:3"
	f.calls = nil
	u.RunOnce(context.Background())
	if indexOf(f.calls, "create a") == -1 || s.Snapshot().Rollbacks != 2 {
		t.Fatalf("%v %+v", f.calls, s.Snapshot())
	}
}

func TestOtherRollbacksAreStillRetried(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"StartNew": errors.New("port already allocated")}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	u.RunOnce(context.Background())
	if s.Snapshot().Rollbacks != 2 {
		t.Fatalf("%+v", s.Snapshot())
	}
}

func TestGateRollbackFailureStillMeansServiceDown(t *testing.T) {
	u, f, _, s, _ := gateSetup(60*time.Second, running("unhealthy"))
	u.docker = &renameBackFails{f}
	u.RunOnce(context.Background())
	ev := s.Snapshot().Events[0]
	if ev.Status != stats.StatusFailed || !strings.Contains(ev.Error, "DOWN") || !strings.Contains(ev.Error, "health gate") {
		t.Fatalf("%+v", ev)
	}
}
