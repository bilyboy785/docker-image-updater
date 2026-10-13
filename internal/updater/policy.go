package updater

import (
	"fmt"
	"strings"
	"time"
)

const (
	// PolicyLabel selects what happens when a newer image exists: "auto" (default) recreates the
	// container, "notify" only reports that an update is available.
	PolicyLabel = "docker-image-updater.policy"
	// WindowLabel restricts automatic updates to one or more HH:MM-HH:MM ranges separated by commas.
	WindowLabel = "docker-image-updater.window"
)

// timeRange is a daily range in minutes since midnight: start inclusive, end exclusive. A range
// whose end is before its start wraps past midnight.
type timeRange struct{ start, end int }

// Policy is the update policy of one container, read from its labels.
type Policy struct {
	// Notify is true for the "notify" policy: updates are reported, never applied.
	Notify bool
	// Windows lists the allowed update ranges; empty means any time.
	Windows []timeRange
}

// ParsePolicy reads the policy and window labels. An unknown policy or a malformed window is an
// error, so that callers can refuse to update rather than guess.
func ParsePolicy(labels map[string]string) (Policy, error) {
	var p Policy
	if v, ok := labels[PolicyLabel]; ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "", "auto":
		case "notify":
			p.Notify = true
		default:
			return Policy{}, fmt.Errorf("invalid %s value %q: expected auto or notify", PolicyLabel, v)
		}
	}
	if v, ok := labels[WindowLabel]; ok {
		for _, part := range strings.Split(v, ",") {
			r, err := parseRange(part)
			if err != nil {
				return Policy{}, fmt.Errorf("invalid %s value %q: %w", WindowLabel, v, err)
			}
			p.Windows = append(p.Windows, r)
		}
	}
	return p, nil
}

func parseRange(s string) (timeRange, error) {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return timeRange{}, fmt.Errorf("window %q is not of the form HH:MM-HH:MM", strings.TrimSpace(s))
	}
	start, err := parseClock(parts[0])
	if err != nil {
		return timeRange{}, err
	}
	end, err := parseClock(parts[1])
	if err != nil {
		return timeRange{}, err
	}
	if start == end {
		return timeRange{}, fmt.Errorf("window %q is empty: start and end are equal", strings.TrimSpace(s))
	}
	return timeRange{start, end}, nil
}

// parseClock parses a strict 24-hour HH:MM time into minutes since midnight.
func parseClock(s string) (int, error) {
	s = strings.TrimSpace(s)
	if len(s) != 5 || s[2] != ':' {
		return 0, fmt.Errorf("window time %q is not of the form HH:MM", s)
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("window time %q is not of the form HH:MM", s)
		}
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if h > 23 || m > 59 {
		return 0, fmt.Errorf("window time %q is out of range", s)
	}
	return h*60 + m, nil
}

// InWindow reports whether the wall-clock time of t falls in one of the windows; with no windows
// it is always true. The caller chooses the time zone by converting t beforehand.
func (p Policy) InWindow(t time.Time) bool {
	if len(p.Windows) == 0 {
		return true
	}
	now := t.Hour()*60 + t.Minute()
	for _, r := range p.Windows {
		if r.start < r.end {
			if now >= r.start && now < r.end {
				return true
			}
		} else if now >= r.start || now < r.end {
			return true
		}
	}
	return false
}
