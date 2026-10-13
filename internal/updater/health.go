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
// replace the old one. The note is non-empty when the update is kept with a caveat.
//
// With a healthcheck the verdict is the one of the healthcheck; a check still "starting" when
// the wait elapses is accepted, since a slow starter must not be rolled back wrongly. Without a
// healthcheck the container must keep running, without restarting, for min(10s, wait).
func (u *Updater) waitHealthy(ctx context.Context, newID string, wait time.Duration) (note string, err error) {
	if wait <= 0 {
		return "", nil
	}
	window := stableWindow
	if wait < window {
		window = wait
	}
	begin := u.hnow()
	deadline := begin.Add(wait)
	var firstRestarts int
	seen := false
	errs := 0
	for {
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
				seen, firstRestarts = true, info.RestartCount
			}
			st := info.State
			switch {
			case st.Restarting:
				return "", &gateError{"the container is restarting"}
			case info.RestartCount > firstRestarts:
				return "", &gateError{fmt.Sprintf("the container restarted (restart count %d -> %d)", firstRestarts, info.RestartCount)}
			case st.Health != nil:
				switch {
				case !st.Running:
					return "", &gateError{fmt.Sprintf("the container exited (exit code %d) before its healthcheck passed", st.ExitCode)}
				case st.Health.Status == "healthy":
					return "", nil
				case st.Health.Status == "unhealthy":
					return "", &gateError{"the healthcheck reports the container unhealthy"}
				}
			case !st.Running:
				if st.ExitCode == 0 {
					return "", nil // graceful one-shot
				}
				return "", &gateError{fmt.Sprintf("the container exited with exit code %d", st.ExitCode)}
			case u.hnow().Sub(begin) >= window:
				return "", nil
			}
		}
		if !u.hnow().Before(deadline) {
			if ierr != nil {
				return "", &gateError{fmt.Sprintf("cannot inspect the container: %v", ierr)}
			}
			return fmt.Sprintf("Note: healthcheck still starting after %ds, update kept; raise HEALTH_WAIT_SECONDS for slow starters.",
				int(wait/time.Second)), nil
		}
		u.hsleep(ctx, healthPollInterval)
	}
}
