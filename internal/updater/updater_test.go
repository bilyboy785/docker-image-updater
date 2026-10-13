package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/bilyboy785/docker-image-updater/internal/docker"
	"github.com/bilyboy785/docker-image-updater/internal/stats"
)

type fakeDocker struct {
	containers []docker.Container
	info       map[string]*docker.ContainerInfo
	images     map[string]string // ref -> image id after pull
	pullErr    map[string]error
	failOn     map[string]error // method name -> error
	calls      []string
	onPull     func() // optional hook, runs inside PullImage
	onStop     func() // optional hook, runs inside StopContainer
}

func (f *fakeDocker) rec(s string)        { f.calls = append(f.calls, s) }
func (f *fakeDocker) fail(m string) error { return f.failOn[m] }

// Every method honours an already-cancelled context, like the real client does.

func (f *fakeDocker) ListContainers(ctx context.Context) ([]docker.Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.containers, f.fail("List")
}
func (f *fakeDocker) InspectContainer(ctx context.Context, id string) (*docker.ContainerInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.info[id], f.fail("Inspect")
}
func (f *fakeDocker) PullImage(ctx context.Context, ref string) error {
	f.rec("pull " + ref)
	if f.onPull != nil {
		f.onPull()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.pullErr[ref]
}
func (f *fakeDocker) ImageID(ctx context.Context, ref string) (string, error) {
	return f.images[ref], ctx.Err()
}
func (f *fakeDocker) CreateContainer(ctx context.Context, name string, _ map[string]any) (string, error) {
	f.rec("create " + name)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "newid", f.fail("Create")
}
func (f *fakeDocker) StartContainer(ctx context.Context, id string) error {
	f.rec("start " + id)
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "newid" {
		return f.fail("StartNew")
	}
	return nil
}
func (f *fakeDocker) StopContainer(ctx context.Context, id string) error {
	f.rec("stop " + id)
	if f.onStop != nil {
		f.onStop()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.fail("Stop")
}
func (f *fakeDocker) RemoveContainer(ctx context.Context, id string, force bool) error {
	f.rec("remove " + id)
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "a" {
		return f.fail("RemoveOld")
	}
	return nil
}
func (f *fakeDocker) RenameContainer(ctx context.Context, id, name string) error {
	f.rec(fmt.Sprintf("rename %s %s", id, name))
	return ctx.Err()
}
func (f *fakeDocker) ConnectNetwork(ctx context.Context, network, id string, _ map[string]any) error {
	f.rec("connect " + network)
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.fail("Connect")
}

type fakeNotifier struct{ msgs []string }

func (n *fakeNotifier) Notify(_ context.Context, text string) error {
	n.msgs = append(n.msgs, text)
	return nil
}

func newUpdater(f *fakeDocker, dry bool) (*Updater, *fakeNotifier, *stats.Stats) {
	n := &fakeNotifier{}
	s, _ := stats.New("test", "active", "")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(f, n, s, "selfself0000", dry, log), n, s
}

func web(id, image, imageID string, labels map[string]string) docker.Container {
	return docker.Container{ID: id, Names: []string{"/" + id}, Image: image, ImageID: imageID, Labels: labels}
}

func basicInfo(id string) *docker.ContainerInfo {
	info := &docker.ContainerInfo{ID: id + "0000000000", Name: "/" + id,
		Config:     map[string]any{"Image": "nginx:latest"},
		HostConfig: map[string]any{"NetworkMode": "bridge"}}
	info.NetworkSettings.Networks = map[string]map[string]any{"bridge": {}}
	return info
}

func TestUpToDateContainerIsUntouched(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		images: map[string]string{"nginx:latest": "sha256:1"}}
	u, n, s := newUpdater(f, false)
	if err := u.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.msgs) != 0 || s.Snapshot().Updates != 0 || s.Snapshot().Scans != 1 || s.Snapshot().Watched != 1 {
		t.Fatalf("%v %+v", n.msgs, s.Snapshot())
	}
}

func TestSuccessfulUpdateSequence(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	u, n, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	want := "pull nginx:latest|rename a a_old|stop a|create a|start newid|remove a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	snap := s.Snapshot()
	if snap.Updates != 1 || snap.Events[0].Status != stats.StatusUpdated || snap.Events[0].NewID != "2" {
		t.Fatalf("%+v", snap)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "nginx:latest") {
		t.Fatalf("%v", n.msgs)
	}
}

func TestOptOutAndDigestAndBareIDAreSkipped(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{
		web("a", "nginx:latest", "sha256:1", map[string]string{OptOutLabel: "false"}),
		web("b", "nginx@sha256:abcdef", "sha256:1", nil),
		web("c", "0123456789ab", "sha256:1", nil),
		web("d", "sha256:0123456789abcdef", "sha256:1", nil),
	}, images: map[string]string{}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if len(f.calls) != 0 {
		t.Fatalf("nothing should be pulled: %v", f.calls)
	}
	if snap := s.Snapshot(); snap.Skipped != 4 || snap.Watched != 0 {
		t.Fatalf("%+v", snap)
	}
}

func TestImagePulledOnceForSeveralContainers(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{
		web("a", "nginx:latest", "sha256:1", nil), web("b", "nginx:latest", "sha256:1", nil)},
		images: map[string]string{"nginx:latest": "sha256:1"}}
	u, _, _ := newUpdater(f, false)
	u.RunOnce(context.Background())
	if len(f.calls) != 1 {
		t.Fatalf("%v", f.calls)
	}
}

func TestSelfIsNotRecreatedOnlyNotifiedOnce(t *testing.T) {
	self := web("selfself0000aaaa", "ghcr.io/x/updater:latest", "sha256:1", nil)
	f := &fakeDocker{containers: []docker.Container{self}, images: map[string]string{"ghcr.io/x/updater:latest": "sha256:2"}}
	u, n, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	u.RunOnce(context.Background())
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "pull") {
			t.Fatalf("self must never be touched: %v", f.calls)
		}
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "manual") {
		t.Fatalf("expected exactly one manual-update notification, got %v", n.msgs)
	}
	if s.Snapshot().Events[0].Status != stats.StatusAvailable {
		t.Fatalf("%+v", s.Snapshot().Events)
	}
}

func TestSelfDetectedByLabel(t *testing.T) {
	c := web("other", "ghcr.io/x/updater:latest", "sha256:1", map[string]string{SelfLabel: "true"})
	f := &fakeDocker{containers: []docker.Container{c}, images: map[string]string{"ghcr.io/x/updater:latest": "sha256:2"}}
	u, _, _ := newUpdater(f, false)
	u.RunOnce(context.Background())
	if len(f.calls) != 1 {
		t.Fatalf("%v", f.calls)
	}
}

func TestDryRunNotifiesWithoutRecreating(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	u, n, _ := newUpdater(f, true)
	u.RunOnce(context.Background())
	u.RunOnce(context.Background())
	if len(f.calls) != 2 || len(n.msgs) != 1 {
		t.Fatalf("calls=%v msgs=%v", f.calls, n.msgs)
	}
}

func TestPullFailureDoesNotStopOtherImages(t *testing.T) {
	f := &fakeDocker{
		containers: []docker.Container{web("a", "local/only:1", "sha256:1", nil), web("b", "nginx:latest", "sha256:1", nil)},
		info:       map[string]*docker.ContainerInfo{"b": basicInfo("b")},
		images:     map[string]string{"nginx:latest": "sha256:2"},
		pullErr:    map[string]error{"local/only:1": errors.New("pull access denied")},
	}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if s.Snapshot().Updates != 1 {
		t.Fatalf("second image must still be updated: %v", f.calls)
	}
}

func TestStartFailureRollsBack(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"StartNew": errors.New("port already allocated")}}
	u, n, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	want := "pull nginx:latest|rename a a_old|stop a|create a|start newid|remove newid|rename a a|start a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	snap := s.Snapshot()
	if snap.Rollbacks != 1 || snap.Events[0].Status != stats.StatusRolledBack || !strings.Contains(snap.Events[0].Error, "port already allocated") {
		t.Fatalf("%+v", snap)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "rolled back") {
		t.Fatalf("%v", n.msgs)
	}
}

func TestCreateFailureRollsBackWithoutRemovingNew(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"Create": errors.New("invalid config")}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	want := "pull nginx:latest|rename a a_old|stop a|create a|rename a a|start a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if s.Snapshot().Rollbacks != 1 {
		t.Fatalf("%+v", s.Snapshot())
	}
}

func TestStopFailureRestoresNameAndRestarts(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"Stop": errors.New("timeout")}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	want := "pull nginx:latest|rename a a_old|stop a|rename a a|start a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if snap := s.Snapshot(); snap.Rollbacks != 1 || snap.Events[0].Status != stats.StatusRolledBack {
		t.Fatalf("%+v", snap)
	}
}

func TestRollbackStillRunsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"StartNew": errors.New("boom")}}
	u, _, _ := newUpdater(f, false)
	u.docker = &cancelOnStart{fakeDocker: f, cancel: cancel}
	u.RunOnce(ctx)
	if !strings.HasSuffix(strings.Join(f.calls, "|"), "rename a a|start a") {
		t.Fatalf("rollback must complete despite cancellation: %v", f.calls)
	}
}

type cancelOnStart struct {
	*fakeDocker
	cancel func()
}

func (c *cancelOnStart) StartContainer(ctx context.Context, id string) error {
	if id == "newid" {
		c.cancel()
	}
	return c.fakeDocker.StartContainer(ctx, id)
}

func TestMessagesAreHTMLEscaped(t *testing.T) {
	c := web("a<b>&", "nginx:latest", "sha256:1", nil)
	f := &fakeDocker{containers: []docker.Container{c}, images: map[string]string{"nginx:latest": "sha256:2"}}
	u, n, _ := newUpdater(f, true)
	u.RunOnce(context.Background())
	if strings.Contains(n.msgs[0], "<b>&") || !strings.Contains(n.msgs[0], "a&lt;b&gt;&amp;") {
		t.Fatalf("%s", n.msgs[0])
	}
}

func TestListFailureIsReturned(t *testing.T) {
	f := &fakeDocker{failOn: map[string]error{"List": errors.New("socket gone")}}
	u, _, _ := newUpdater(f, false)
	if err := u.RunOnce(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

const fullSequence = "pull nginx:latest|rename a a_old|stop a|create a|start newid|remove a"

// noCtxCheckPull lets inspect, pull and image-id lookup succeed even with a cancelled context, so
// the scenario "ctx cancelled right as the cycle reaches recreate" can be exercised.
type noCtxCheckPull struct{ *fakeDocker }

func (c *noCtxCheckPull) PullImage(_ context.Context, ref string) error {
	return c.fakeDocker.PullImage(context.Background(), ref)
}
func (c *noCtxCheckPull) ImageID(_ context.Context, ref string) (string, error) {
	return c.fakeDocker.ImageID(context.Background(), ref)
}
func (c *noCtxCheckPull) InspectContainer(_ context.Context, id string) (*docker.ContainerInfo, error) {
	return c.fakeDocker.InspectContainer(context.Background(), id)
}

func TestCancelledContextBeforeRecreateStillCompletesUpdate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	f.onPull = cancel // the cycle then proceeds into recreate with a dead caller context
	u, n, s := newUpdater(f, false)
	u.docker = &noCtxCheckPull{f}
	u.RunOnce(ctx)
	if got := strings.Join(f.calls, "|"); got != fullSequence {
		t.Fatalf("got  %s\nwant %s", got, fullSequence)
	}
	if s.Snapshot().Updates != 1 || len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "Updated") {
		t.Fatalf("%+v %v", s.Snapshot(), n.msgs)
	}
}

func TestCancelDuringStopStillCompletesUpdate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	f.onStop = cancel
	u, n, s := newUpdater(f, false)
	u.RunOnce(ctx)
	if got := strings.Join(f.calls, "|"); got != fullSequence {
		t.Fatalf("got  %s\nwant %s", got, fullSequence)
	}
	if s.Snapshot().Updates != 1 || len(n.msgs) != 1 {
		t.Fatalf("%+v %v", s.Snapshot(), n.msgs)
	}
}

func TestOldContainerRemovalFailureIsReported(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"RemoveOld": errors.New("busy")}}
	u, n, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if s.Snapshot().Events[0].Status != stats.StatusUpdated || len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "a_old") {
		t.Fatalf("%+v %v", s.Snapshot(), n.msgs)
	}
}

func TestBareImageIDIsResolvedFromContainerConfig(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "sha256:0123456789abcdef", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if snap := s.Snapshot(); snap.Updates != 1 || snap.Watched != 1 || snap.Events[0].Image != "nginx:latest" {
		t.Fatalf("%+v %v", s.Snapshot(), f.calls)
	}
}

func TestBareImageIDWithBareConfigImageIsSkipped(t *testing.T) {
	info := basicInfo("a")
	info.Config["Image"] = "sha256:0123456789abcdef"
	f := &fakeDocker{containers: []docker.Container{web("a", "0123456789ab", "sha256:1", nil)},
		info: map[string]*docker.ContainerInfo{"a": info}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if snap := s.Snapshot(); snap.Skipped != 1 || snap.Watched != 0 || len(f.calls) != 0 {
		t.Fatalf("%+v %v", snap, f.calls)
	}
}

func TestConnectNetworkFailureRollsBack(t *testing.T) {
	info := basicInfo("a")
	info.NetworkSettings.Networks = map[string]map[string]any{"bridge": {}, "backend": {}}
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": info},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"Connect": errors.New("no such network")}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	got := strings.Join(f.calls, "|")
	if !strings.Contains(got, "create a|connect ") || !strings.HasSuffix(got, "|remove newid|rename a a|start a") {
		t.Fatalf("got %s", got)
	}
	if snap := s.Snapshot(); snap.Rollbacks != 1 || snap.Events[0].Status != stats.StatusRolledBack {
		t.Fatalf("%+v", snap)
	}
}
