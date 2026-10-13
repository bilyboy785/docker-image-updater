// Package updater scans running containers and recreates those whose image has a newer version.
package updater

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bilyboy785/docker-image-updater/internal/docker"
	"github.com/bilyboy785/docker-image-updater/internal/notify"
	"github.com/bilyboy785/docker-image-updater/internal/stats"
)

const (
	OptOutLabel = "docker-image-updater.enable"
	SelfLabel   = "docker-image-updater.self"
)

type DockerAPI interface {
	ListContainers(ctx context.Context) ([]docker.Container, error)
	InspectContainer(ctx context.Context, id string) (*docker.ContainerInfo, error)
	PullImage(ctx context.Context, ref string) error
	ImageID(ctx context.Context, ref string) (string, error)
	CreateContainer(ctx context.Context, name string, body map[string]any) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	RenameContainer(ctx context.Context, id, name string) error
	ConnectNetwork(ctx context.Context, network, containerID string, endpoint map[string]any) error
}

type Updater struct {
	docker   DockerAPI
	notifier notify.Notifier
	stats    *stats.Stats
	selfID   string
	dryRun   bool
	log      *slog.Logger
	notified map[string]bool
}

func New(d DockerAPI, n notify.Notifier, s *stats.Stats, selfID string, dryRun bool, log *slog.Logger) *Updater {
	return &Updater{docker: d, notifier: n, stats: s, selfID: selfID, dryRun: dryRun, log: log, notified: map[string]bool{}}
}

var bareID = regexp.MustCompile(`^(sha256:)?[0-9a-f]{12,64}$`)

// isPullable reports whether the image reference can have a newer version (not pinned by digest,
// not a bare image id left behind by an untagged image).
func isPullable(image string) bool {
	return !strings.Contains(image, "@") && !bareID.MatchString(image)
}

func (u *Updater) isSelf(c docker.Container) bool {
	return c.Labels[SelfLabel] == "true" || (u.selfID != "" && strings.HasPrefix(c.ID, u.selfID))
}

// RunOnce performs one full scan.
func (u *Updater) RunOnce(ctx context.Context) error {
	start := time.Now()
	containers, err := u.docker.ListContainers(ctx)
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	groups := map[string][]docker.Container{}
	var refs []string
	watched, skipped := 0, 0
	for _, c := range containers {
		if c.Labels[OptOutLabel] == "false" || !isPullable(c.Image) {
			skipped++
			continue
		}
		watched++
		if _, ok := groups[c.Image]; !ok {
			refs = append(refs, c.Image)
		}
		groups[c.Image] = append(groups[c.Image], c)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		if ctx.Err() != nil {
			break
		}
		u.checkImage(ctx, ref, groups[ref])
	}
	u.stats.RecordScan(start, time.Since(start), watched, skipped)
	return ctx.Err()
}

func (u *Updater) checkImage(ctx context.Context, ref string, cs []docker.Container) {
	if err := u.docker.PullImage(ctx, ref); err != nil {
		u.log.Warn("pull failed, skipping image", "image", ref, "error", err)
		return
	}
	newID, err := u.docker.ImageID(ctx, ref)
	if err != nil {
		u.log.Warn("cannot read image id", "image", ref, "error", err)
		return
	}
	for _, c := range cs {
		if c.ImageID != newID {
			u.handle(ctx, c, ref, newID)
		}
	}
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func (u *Updater) handle(ctx context.Context, c docker.Container, ref, newID string) {
	ev := stats.Event{Container: c.Name(), Image: ref, OldID: shortID(c.ImageID), NewID: shortID(newID)}
	self := u.isSelf(c)
	if self || u.dryRun {
		key := c.ID + "|" + newID
		if u.notified[key] {
			return
		}
		u.notified[key] = true
		ev.Status = stats.StatusAvailable
		u.stats.RecordEvent(ev)
		note := ""
		if self {
			note = "\nThe updater does not update itself: manual update required."
		}
		u.notify(ctx, fmt.Sprintf("🔔 Update available for <b>%s</b>\nImage: <code>%s</code>\n%s → %s%s",
			html.EscapeString(ev.Container), html.EscapeString(ref), ev.OldID, ev.NewID, note))
		return
	}
	u.log.Info("updating container", "container", ev.Container, "image", ref)
	status, err := u.recreate(ctx, c, newID)
	ev.Status = status
	if err != nil {
		ev.Error = err.Error()
		u.log.Error("update failed", "container", ev.Container, "status", status, "error", err)
	}
	u.stats.RecordEvent(ev)

	head := map[string]string{
		stats.StatusUpdated:    "✅ Updated",
		stats.StatusFailed:     "❌ Update failed for",
		stats.StatusRolledBack: "⚠️ Update failed and was rolled back for",
	}[status]
	msg := fmt.Sprintf("%s <b>%s</b>\nImage: <code>%s</code>\n%s → %s",
		head, html.EscapeString(ev.Container), html.EscapeString(ref), ev.OldID, ev.NewID)
	if err != nil {
		msg += "\nError: " + html.EscapeString(err.Error())
	}
	u.notify(ctx, msg)
}

func (u *Updater) notify(ctx context.Context, text string) {
	if err := u.notifier.Notify(ctx, text); err != nil {
		u.log.Warn("notification failed", "error", err)
	}
}

// recreate replaces container c by a new one running the freshly pulled image.
// The old container is renamed and stopped first so it can be restored if anything fails.
func (u *Updater) recreate(ctx context.Context, c docker.Container, newImageID string) (string, error) {
	info, err := u.docker.InspectContainer(ctx, c.ID)
	if err != nil {
		return stats.StatusFailed, fmt.Errorf("inspect: %w", err)
	}
	spec := buildCreateSpec(info, newImageID)
	name := c.Name()

	if err := u.docker.RenameContainer(ctx, c.ID, name+"_old"); err != nil {
		return stats.StatusFailed, fmt.Errorf("rename old container: %w", err)
	}
	if err := u.docker.StopContainer(ctx, c.ID); err != nil {
		_ = u.docker.RenameContainer(ctx, c.ID, name)
		return stats.StatusFailed, fmt.Errorf("stop old container: %w", err)
	}
	newID, err := u.docker.CreateContainer(ctx, name, spec.Body)
	if err != nil {
		return u.rollback(ctx, c, name, "", fmt.Errorf("create new container: %w", err))
	}
	for _, n := range spec.Extra {
		if err := u.docker.ConnectNetwork(ctx, n.Network, newID, n.Endpoint); err != nil {
			return u.rollback(ctx, c, name, newID, fmt.Errorf("connect network %s: %w", n.Network, err))
		}
	}
	if err := u.docker.StartContainer(ctx, newID); err != nil {
		return u.rollback(ctx, c, name, newID, fmt.Errorf("start new container: %w", err))
	}
	if err := u.docker.RemoveContainer(ctx, c.ID, false); err != nil {
		u.log.Warn("could not remove old container", "container", name+"_old", "error", err)
	}
	return stats.StatusUpdated, nil
}

// rollback restores the old container. It uses a context detached from cancellation so that a
// shutdown in the middle of an update does not leave the service stopped.
func (u *Updater) rollback(ctx context.Context, old docker.Container, name, newID string, cause error) (string, error) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	if newID != "" {
		if err := u.docker.RemoveContainer(rctx, newID, true); err != nil {
			u.log.Warn("rollback: cannot remove new container", "error", err)
		}
	}
	if err := u.docker.RenameContainer(rctx, old.ID, name); err != nil {
		return stats.StatusFailed, fmt.Errorf("%w (rollback failed: rename: %v)", cause, err)
	}
	if err := u.docker.StartContainer(rctx, old.ID); err != nil {
		return stats.StatusFailed, fmt.Errorf("%w (rollback failed: start: %v)", cause, err)
	}
	return stats.StatusRolledBack, cause
}

// Compile-time check that the real Docker client satisfies DockerAPI.
var _ DockerAPI = (*docker.Client)(nil)
