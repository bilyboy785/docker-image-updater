package updater

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bilyboy785/docker-image-updater/internal/docker"
)

const (
	healthPollInterval = time.Second
	// stableWindow is how long a container without healthcheck must run without restarting.
	stableWindow = 10 * time.Second
	// maxInspectErrors is how many consecutive failed inspections the gate tolerates.
	maxInspectErrors = 3
)

// gateError is the reason the new container did not pass the post-start health gate.
type gateError struct{ reason string }

func (e *gateError) Error() string { return "new container failed health gate: " + e.reason }

// WithHealthWait sets how long a freshly started container is observed before the update is
// considered successful (0 disables the gate) and returns the updater.
func (u *Updater) WithHealthWait(d time.Duration) *Updater {
	u.healthWait = d
	return u
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// waitHealthy watches the new container for up to wait and returns a *gateError when it must not
// replace the old one. The note is non-empty when the update is kept with a caveat. ctx bounds the
// work and is detached from the caller; caller is the caller's own context: when it is cancelled
// while the gate runs (the updater is stopping) the gate is abandoned at once and the update is
// rolled back, so that the old container is back quickly. A caller already cancelled when the
// gate starts does not abort it.
//
// With a healthcheck the verdict is the one of the healthcheck; a check still "starting" when
// the wait elapses is accepted, since a slow starter must not be rolled back wrongly. Without a
// healthcheck the container must keep running, without restarting, for min(10s, wait) from the
// first successful observation. A failed inspection on the last poll does not fail the gate: the
// last valid observation is used.
func (u *Updater) waitHealthy(ctx, caller context.Context, newID string, wait time.Duration) (note string, err error) {
	if wait <= 0 {
		return "", nil
	}
	window := stableWindow
	if wait < window {
		window = wait
	}
	deadline := u.hnow().Add(wait)
	watch := caller.Err() == nil
	var first time.Time
	seen, lastHealth := false, false
	errs := 0
	for {
		if watch && caller.Err() != nil {
			return "", &gateError{"updater stopping during health gate"}
		}
		ictx, cancel := context.WithTimeout(ctx, u.apiTimeout)
		info, ierr := u.docker.InspectContainer(ictx, newID)
		cancel()
		if ierr == nil && info == nil {
			ierr = errors.New("empty inspect result")
		}
		var ae *docker.APIError
		switch {
		case errors.As(ierr, &ae) && ae.Status == 404:
			return "", &gateError{"the container disappeared"}
		case ierr != nil:
			if ctx.Err() != nil {
				return "", &gateError{"gave up waiting: " + ctx.Err().Error()}
			}
			errs++
			if errs > maxInspectErrors {
				return "", &gateError{fmt.Sprintf("cannot inspect the container (%d consecutive errors): %v", errs, ierr)}
			}
		default:
			errs = 0
			if !seen {
				seen, first = true, u.hnow()
			}
			st := info.State
			hasHealth := st.Health != nil && st.Health.Status != "" && st.Health.Status != "none"
			lastHealth = hasHealth
			switch {
			case st.Restarting:
				return "", &gateError{"the container is restarting"}
			case info.RestartCount > 0:
				// A freshly created container starts with a restart count of 0.
				return "", &gateError{fmt.Sprintf("the container restarted (restart count %d)", info.RestartCount)}
			case hasHealth:
				switch {
				case !st.Running:
					return "", &gateError{fmt.Sprintf("the container is not running (state %s, exit code %d) before its healthcheck passed", st.Status, st.ExitCode)}
				case st.Health.Status == "healthy":
					return "", nil
				case st.Health.Status == "unhealthy":
					return "", &gateError{"the healthcheck reports the container unhealthy"}
				}
			case !st.Running:
				if st.Status != "exited" {
					return "", &gateError{fmt.Sprintf("the container is not running (state %s)", st.Status)}
				}
				if st.ExitCode == 0 {
					return "", nil // graceful one-shot
				}
				return "", &gateError{fmt.Sprintf("the container exited with exit code %d", st.ExitCode)}
			case u.hnow().Sub(first) >= window:
				return "", nil
			}
		}
		if !u.hnow().Before(deadline) {
			if !seen {
				return "", &gateError{fmt.Sprintf("cannot inspect the container: %v", ierr)}
			}
			if lastHealth {
				return fmt.Sprintf("Note: healthcheck still starting after %ds, update kept; raise HEALTH_WAIT_SECONDS for slow starters.",
					int(wait/time.Second)), nil
			}
			return "", nil
		}
		if watch {
			u.hsleep(caller, healthPollInterval)
		} else {
			u.hsleep(ctx, healthPollInterval)
		}
	}
}
