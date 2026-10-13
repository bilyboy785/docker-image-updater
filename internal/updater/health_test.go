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
		state(true, false, 0, "", 2), state(true, false, 0, "", 2), state(true, false, 0, "", 3))
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

func TestGateCompletesWhenTheCallerIsCancelledMidGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	u, f, _, s, _ := gateSetup(60*time.Second, running("starting"), running("healthy"))
	inner := f.inspectNew
	f.inspectNew = func(n int) (*docker.ContainerInfo, error) {
		if n == 1 {
			cancel()
		}
		return inner(n)
	}
	u.RunOnce(ctx)
	if got := strings.Join(f.calls, "|"); got != beforeGate+"|inspect newid|inspect newid|remove a" {
		t.Fatalf("got %s", got)
	}
	if s.Snapshot().Updates != 1 {
		t.Fatalf("%+v", s.Snapshot())
	}
}

func TestGateCancelledMidGateStillRollsBackOnFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	u, f, _, s, _ := gateSetup(60*time.Second, running("starting"), running("unhealthy"))
	inner := f.inspectNew
	f.inspectNew = func(n int) (*docker.ContainerInfo, error) {
		if n == 1 {
			cancel()
		}
		return inner(n)
	}
	u.RunOnce(ctx)
	if !strings.HasSuffix(strings.Join(f.calls, "|"), "remove newid|rename a a|start a") || s.Snapshot().Rollbacks != 1 {
		t.Fatalf("%v %+v", f.calls, s.Snapshot())
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
