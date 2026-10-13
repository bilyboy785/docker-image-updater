// Package updater scans running containers and recreates those whose image has a newer version.
package updater

import (
	"context"
	"errors"
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
	// OptOutLabel set to "false" on a container excludes it from updates.
	OptOutLabel = "docker-image-updater.enable"
	// SelfLabel set to "true" marks the updater's own container, which is never recreated.
	SelfLabel = "docker-image-updater.self"

	defaultAPITimeout  = time.Minute      // list, inspect, image lookups
	defaultPullTimeout = 30 * time.Minute // image pulls
)

// DockerAPI is the subset of the Docker Engine API the updater needs.
type DockerAPI interface {
	ListContainers(ctx context.Context) ([]docker.Container, error)
	InspectContainer(ctx context.Context, id string) (*docker.ContainerInfo, error)
	PullImage(ctx context.Context, ref, registryAuth string) error
	ImageID(ctx context.Context, ref string) (string, error)
	ImageConfig(ctx context.Context, ref string) (map[string]any, error)
	ImageIsLocal(ctx context.Context, id string) (bool, error)
	CreateContainer(ctx context.Context, name string, body map[string]any) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	RenameContainer(ctx context.Context, id, name string) error
	ConnectNetwork(ctx context.Context, network, containerID string, endpoint map[string]any) error
}

// AuthProvider returns the value of the X-Registry-Auth header to use to pull ref, or "" to pull
// anonymously.
type AuthProvider interface {
	AuthHeader(ref string) string
}

// Updater runs scans and recreates containers whose image changed.
type Updater struct {
	docker   DockerAPI
	notifier notify.Notifier
	stats    *stats.Stats
	selfID   string
	dryRun   bool
	log      *slog.Logger
	notified map[string]bool  // dedup keys of notifications already sent since start
	auth     AuthProvider     // registry credentials; nil means anonymous pulls
	now      func() time.Time // clock used to evaluate update windows

	apiTimeout  time.Duration
	pullTimeout time.Duration
}

// New returns an Updater. selfID is the updater's own container id or hostname, used to
// recognise (and never recreate) its own container.
func New(d DockerAPI, n notify.Notifier, s *stats.Stats, selfID string, dryRun bool, log *slog.Logger) *Updater {
	return &Updater{docker: d, notifier: n, stats: s, selfID: selfID, dryRun: dryRun, log: log, notified: map[string]bool{}, now: time.Now,
		apiTimeout: defaultAPITimeout, pullTimeout: defaultPullTimeout}
}

// WithRegistryAuth sets the provider of registry credentials used for pulls (nil: anonymous)
// and returns the updater.
func (u *Updater) WithRegistryAuth(p AuthProvider) *Updater {
	u.auth = p
	return u
}

var bareID = regexp.MustCompile(`^(sha256:)?[0-9a-f]{12,64}$`)

// isPullable reports whether the image reference can have a newer version (not pinned by digest,
// not a bare image id left behind by an untagged image).
func isPullable(image string) bool {
	return !strings.Contains(image, "@") && !bareID.MatchString(image)
}

func (u *Updater) isSelf(c docker.Container) bool {
	return c.Labels[SelfLabel] == "true" || (len(u.selfID) >= 12 && strings.HasPrefix(c.ID, u.selfID))
}

// RunOnce performs one full scan.
func (u *Updater) RunOnce(ctx context.Context) error {
	start := time.Now()
	lctx, lcancel := context.WithTimeout(ctx, u.apiTimeout)
	containers, err := u.docker.ListContainers(lctx)
	lcancel()
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	groups := map[string][]docker.Container{}
	var refs []string
	watched, skipped := 0, 0
	for _, c := range containers {
		if c.Labels[OptOutLabel] == "false" {
			skipped++
			continue
		}
		if bareID.MatchString(c.Image) {
			// The daemon reports the bare image id when the tag now points to another image
			// (after a failed update, a rollback or a dry-run): recover the real reference.
			ref := u.configImage(ctx, c.ID)
			if ref == "" {
				skipped++
				continue
			}
			c.Image = ref
		}
		if !isPullable(c.Image) {
			skipped++
			continue
		}
		if _, err := ParsePolicy(c.Labels); err != nil {
			u.warnBadPolicy(c, err)
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
		n := u.checkImage(ctx, ref, groups[ref], containers)
		watched -= n
		skipped += n
	}
	u.stats.RecordScan(start, time.Since(start), watched, skipped)
	return ctx.Err()
}

// warnBadPolicy logs invalid policy labels once per container and label values.
func (u *Updater) warnBadPolicy(c docker.Container, err error) {
	key := "policy|" + c.ID + "|" + c.Labels[PolicyLabel] + "|" + c.Labels[WindowLabel]
	if u.notified[key] {
		return
	}
	u.notified[key] = true
	u.log.Warn("invalid update policy labels, this container will not be updated",
		"container", c.Name(), "error", err)
}

// configImage returns the image reference a container was created with, or "" if unknown or not pullable.
func (u *Updater) configImage(ctx context.Context, id string) string {
	ictx, cancel := context.WithTimeout(ctx, u.apiTimeout)
	defer cancel()
	info, err := u.docker.InspectContainer(ictx, id)
	if err != nil || info == nil {
		if err != nil && ctx.Err() == nil {
			u.log.Warn("cannot inspect container to resolve its image", "container", shortID(id), "error", err)
		}
		return ""
	}
	ref, _ := info.Config["Image"].(string)
	if ref == "" || !isPullable(ref) {
		return ""
	}
	return ref
}

// checkImage pulls ref and updates every container in cs running an older image. all is the
// full container list of the scan, used to find containers depending on an updated one.
// It returns the number of containers it skipped because their image was built locally.
func (u *Updater) checkImage(ctx context.Context, ref string, cs, all []docker.Container) int {
	if u.isLocalImage(ctx, ref, cs) {
		return len(cs)
	}
	// Read the config of the images currently run before the pull: with some image stores the pull
	// deletes an image whose tag it replaces, and the config is needed to recreate the containers.
	cfgs := u.oldConfigs(ctx, cs)
	pctx, pcancel := context.WithTimeout(ctx, u.pullTimeout)
	auth := ""
	if u.auth != nil {
		auth = u.auth.AuthHeader(ref)
	}
	err := u.docker.PullImage(pctx, ref, auth)
	pcancel()
	if err != nil {
		u.reportImageFailure(ctx, "pull", ref, cs, err)
		return 0
	}
	ictx, icancel := context.WithTimeout(ctx, u.apiTimeout)
	newID, err := u.docker.ImageID(ictx, ref)
	icancel()
	if err != nil {
		u.reportImageFailure(ctx, "image id", ref, cs, err)
		return 0
	}
	for _, c := range cs {
		if ctx.Err() != nil {
			return 0 // shutting down: do not turn that into a failed update
		}
		if c.ImageID != newID {
			u.handle(ctx, c, ref, newID, all, cfgs)
		}
	}
	return 0
}

// oldConfigs reads, best effort, the config of every distinct image the containers of cs run.
func (u *Updater) oldConfigs(ctx context.Context, cs []docker.Container) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, c := range cs {
		if _, ok := out[c.ImageID]; ok {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, u.apiTimeout)
		cfg, err := u.docker.ImageConfig(cctx, c.ImageID)
		cancel()
		if err == nil {
			out[c.ImageID] = cfg
		}
	}
	return out
}

// isLocalImage reports whether every image run by the containers of a group never came from a
// registry (built locally or loaded from an archive) and so cannot be pulled. The group shares one
// reference but its containers may run different images, so all distinct image ids are checked.
// When any image is not local, or any lookup fails, the group is treated as pullable: a wrong skip
// would silently stop updates, while a useless pull only makes noise.
func (u *Updater) isLocalImage(ctx context.Context, ref string, cs []docker.Container) bool {
	seen := map[string]bool{}
	for _, c := range cs {
		if seen[c.ImageID] {
			continue
		}
		seen[c.ImageID] = true
		ictx, cancel := context.WithTimeout(ctx, u.apiTimeout)
		local, err := u.docker.ImageIsLocal(ictx, c.ImageID)
		cancel()
		if err != nil {
			if ctx.Err() == nil && !u.notified["image-origin|"+c.ImageID] {
				u.notified["image-origin|"+c.ImageID] = true
				u.log.Warn("cannot tell whether the image was built locally, checking it anyway",
					"image", ref, "image_id", shortID(c.ImageID), "error", err)
			}
			return false
		}
		if !local {
			return false
		}
	}
	u.log.Debug("ignoring locally built image", "image", ref)
	return true
}

// reportImageFailure logs a pull or image lookup failure and, once per image reference and error
// text, records it and sends a notification so that a permanently failing image does not repeat
// on every scan.
func (u *Updater) reportImageFailure(ctx context.Context, what, ref string, cs []docker.Container, err error) {
	if ctx.Err() != nil {
		return // shutdown, not a real failure
	}
	u.log.Warn("cannot check image, skipping it", "step", what, "image", ref, "error", err)
	key := what + "|" + ref + "|" + err.Error()
	if u.notified[key] {
		return
	}
	u.notified[key] = true
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Name()
	}
	joined := strings.Join(names, ", ")
	u.stats.RecordEvent(stats.Event{Container: joined, Image: ref, Status: stats.StatusFailed, Error: err.Error()})
	u.notify(ctx, fmt.Sprintf("❌ Cannot check image <code>%s</code> (%s step) used by <b>%s</b>\nError: %s",
		html.EscapeString(ref), what, html.EscapeString(joined), html.EscapeString(err.Error())))
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// announceAvailable records and notifies, once per container and new image, that an update is
// available but will not be applied automatically. It does nothing on repeated calls.
func (u *Updater) announceAvailable(ctx context.Context, ev stats.Event, c docker.Container, newID, note string) {
	key := c.ID + "|" + newID
	if u.notified[key] {
		return
	}
	u.notified[key] = true
	ev.Status = stats.StatusAvailable
	u.stats.RecordEvent(ev)
	u.notify(ctx, fmt.Sprintf("🔔 Update available for <b>%s</b>\nImage: <code>%s</code>\n%s → %s%s",
		html.EscapeString(ev.Container), html.EscapeString(ev.Image), ev.OldID, ev.NewID, note))
}

func (u *Updater) handle(ctx context.Context, c docker.Container, ref, newID string, all []docker.Container, cfgs map[string]map[string]any) {
	ev := stats.Event{Container: c.Name(), Image: ref, OldID: shortID(c.ImageID), NewID: shortID(newID)}
	self := u.isSelf(c)
	if self || u.dryRun {
		note := ""
		if self {
			note = "\nThe updater does not update itself: manual update required."
		}
		u.announceAvailable(ctx, ev, c, newID, note)
		return
	}
	pol, perr := ParsePolicy(c.Labels)
	if perr != nil {
		// Fail closed: a typo must never turn into an update outside the intended window.
		u.warnBadPolicy(c, perr)
		return
	}
	if pol.Notify {
		u.announceAvailable(ctx, ev, c, newID, "\n(policy: notify) The update is not applied automatically.")
		return
	}
	if !pol.InWindow(u.now().In(time.Local)) {
		u.log.Debug("outside the update window, postponing", "container", ev.Container, "window", c.Labels[WindowLabel])
		return
	}
	ictx, icancel := context.WithTimeout(ctx, u.apiTimeout)
	info, err := u.docker.InspectContainer(ictx, c.ID)
	icancel()
	if err == nil && info == nil {
		err = errors.New("empty inspect result")
	}
	if err != nil {
		u.finish(ctx, ev, ref, stats.StatusFailed, "", fmt.Errorf("inspect: %w", err), nil)
		return
	}
	if autoRemove, _ := info.HostConfig["AutoRemove"].(bool); autoRemove {
		// Stopping such a container makes the daemon delete it, so it could not be rolled back.
		u.announceAvailable(ctx, ev, c, newID,
			"\nThe container uses AutoRemove (--rm) and cannot be updated safely: manual update required.")
		return
	}
	u.log.Info("updating container", "container", ev.Container, "image", ref)
	status, warn, err := u.recreate(ctx, c, info, newID, cfgs)
	var dependants []string
	if status == stats.StatusUpdated {
		dependants = dependantsOf(c, all)
	}
	u.finish(ctx, ev, ref, status, warn, err, dependants)
}

// dependantsOf returns the names of containers sharing the network stack of c
// (network_mode: container:X).
func dependantsOf(c docker.Container, all []docker.Container) []string {
	var out []string
	for _, o := range all {
		if o.ID != c.ID && (o.NetworkMode == "container:"+c.ID || o.NetworkMode == "container:"+c.Name()) {
			out = append(out, o.Name())
		}
	}
	return out
}

// finish records the outcome of an update attempt and sends the notification.
func (u *Updater) finish(ctx context.Context, ev stats.Event, ref, status, warn string, err error, dependants []string) {
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
	if warn != "" {
		msg += "\n" + html.EscapeString(warn)
	}
	if len(dependants) > 0 {
		msg += "\nShares its network stack with: " + html.EscapeString(strings.Join(dependants, ", ")) +
			". These containers must be restarted or recreated manually."
	}
	// The outcome must be reported even if shutdown started while we were working.
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	u.notify(nctx, msg)
}

// notify sends a message and only logs a failure to do so.
func (u *Updater) notify(ctx context.Context, text string) {
	if err := u.notifier.Notify(ctx, text); err != nil {
		u.log.Warn("notification failed", "error", err)
	}
}

// recreate replaces container c (inspected as info) by a new one running the freshly pulled image.
// The old container is renamed and stopped first so it can be restored if anything fails.
// Once the old container is touched the work must finish or be rolled back, so every step runs
// on a context detached from the caller's cancellation (bounded by a timeout). The returned
// warning is non-empty when the update succeeded but left something to clean up.
func (u *Updater) recreate(ctx context.Context, c docker.Container, info *docker.ContainerInfo, newImageID string, cfgs map[string]map[string]any) (status, warn string, err error) {
	spec := buildCreateSpec(info, newImageID)
	name := c.Name()

	opctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()

	// The inspected config merges the old image's defaults with the container's own settings:
	// drop the former so the new image's defaults apply. This is best effort.
	if imgCfg, ok := cfgs[c.ImageID]; ok {
		stripImageDefaults(spec.Body, imgCfg)
	} else {
		cctx, ccancel := context.WithTimeout(opctx, u.apiTimeout)
		imgCfg, cfgErr := u.docker.ImageConfig(cctx, c.ImageID)
		ccancel()
		if cfgErr != nil {
			u.log.Warn("cannot read the old image config, recreating with the inspected config as is",
				"container", name, "error", cfgErr)
		} else {
			stripImageDefaults(spec.Body, imgCfg)
		}
	}

	if err := u.docker.RenameContainer(opctx, c.ID, name+"_old"); err != nil {
		return stats.StatusFailed, "", fmt.Errorf("rename old container: %w", err)
	}
	if err := u.docker.StopContainer(opctx, c.ID); err != nil {
		// The daemon may already have stopped it: restore the name and make sure it runs.
		st, e := u.rollback(opctx, c, name, "", fmt.Errorf("stop old container: %w", err))
		return st, "", e
	}
	newID, err := u.docker.CreateContainer(opctx, name, spec.Body)
	if err != nil {
		st, e := u.rollback(opctx, c, name, "", fmt.Errorf("create new container: %w", err))
		return st, "", e
	}
	for _, n := range spec.Extra {
		if err := u.docker.ConnectNetwork(opctx, n.Network, newID, n.Endpoint); err != nil {
			st, e := u.rollback(opctx, c, name, newID, fmt.Errorf("connect network %s: %w", n.Network, err))
			return st, "", e
		}
	}
	if err := u.docker.StartContainer(opctx, newID); err != nil {
		st, e := u.rollback(opctx, c, name, newID, fmt.Errorf("start new container: %w", err))
		return st, "", e
	}
	if err := u.docker.RemoveContainer(opctx, c.ID, false); err != nil {
		u.log.Warn("could not remove old container", "container", name+"_old", "error", err)
		warn = fmt.Sprintf("Warning: the old container %s_old could not be removed and must be removed manually.", name)
	}
	return stats.StatusUpdated, warn, nil
}

// rollback restores the old container. It uses a context detached from cancellation so that a
// shutdown in the middle of an update does not leave the service stopped.
func (u *Updater) rollback(ctx context.Context, old docker.Container, name, newID string, cause error) (string, error) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second) // never cancelled by the caller
	defer cancel()
	if newID != "" {
		if err := u.docker.RemoveContainer(rctx, newID, true); err != nil {
			u.log.Warn("rollback: cannot remove new container", "error", err)
		}
	}
	if err := u.docker.RenameContainer(rctx, old.ID, name); err != nil {
		return stats.StatusFailed, fmt.Errorf("%w (rollback failed: could not rename the old container back: %v; "+
			"the service may be DOWN, the stopped old container is named %s_old)", cause, err, name)
	}
	if err := u.docker.StartContainer(rctx, old.ID); err != nil {
		return stats.StatusFailed, fmt.Errorf("%w (rollback failed: could not restart the old container: %v; "+
			"the service may be DOWN, the stopped old container is named %s and is the one to start)", cause, err, name)
	}
	return stats.StatusRolledBack, cause
}

// Compile-time check that the real Docker client satisfies DockerAPI.
var _ DockerAPI = (*docker.Client)(nil)
