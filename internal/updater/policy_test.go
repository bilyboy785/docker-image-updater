package updater

import (
	"strings"
	"testing"
	"time"
)

func lbl(policy, window string) map[string]string {
	m := map[string]string{}
	if policy != "" {
		m[PolicyLabel] = policy
	}
	if window != "" {
		m[WindowLabel] = window
	}
	return m
}

func TestParsePolicyValid(t *testing.T) {
	cases := []struct {
		name    string
		labels  map[string]string
		notify  bool
		windows int
	}{
		{"no labels", nil, false, 0},
		{"auto", lbl("auto", ""), false, 0},
		{"notify", lbl("notify", ""), true, 0},
		{"uppercase and spaces", lbl("  NOTIFY ", ""), true, 0},
		{"empty policy value is auto", map[string]string{PolicyLabel: ""}, false, 0},
		{"one window", lbl("", "02:00-05:00"), false, 1},
		{"two windows", lbl("auto", "02:00-03:00,23:00-23:59"), false, 2},
		{"spaces everywhere", lbl("", " 02:00 - 03:00 , 23:00-23:59 "), false, 2},
		{"wrap", lbl("", "22:00-02:00"), false, 1},
		{"notify with window", lbl("notify", "01:00-02:00"), true, 1},
	}
	for _, tc := range cases {
		p, err := ParsePolicy(tc.labels)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if p.Notify != tc.notify || len(p.Windows) != tc.windows {
			t.Errorf("%s: %+v", tc.name, p)
		}
	}
}

func TestParsePolicyInvalid(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"unknown policy", lbl("manual", ""), "policy"},
		{"hours out of range", lbl("", "25:00-26:00"), "window"},
		{"minutes out of range", lbl("", "02:60-03:00"), "window"},
		{"single time", lbl("", "02:00"), "window"},
		{"missing end", lbl("", "02:00-"), "window"},
		{"missing start", lbl("", "-03:00"), "window"},
		{"empty window", map[string]string{WindowLabel: ""}, "window"},
		{"blank window", map[string]string{WindowLabel: "   "}, "window"},
		{"empty range in list", lbl("", "02:00-03:00,"), "window"},
		{"no zero padding", lbl("", "2:00-3:00"), "window"},
		{"letters", lbl("", "ab:cd-ef:gh"), "window"},
		{"three parts", lbl("", "01:00-02:00-03:00"), "window"},
		{"equal bounds", lbl("", "02:00-02:00"), "window"},
		{"seconds", lbl("", "02:00:00-03:00:00"), "window"},
	}
	for _, tc := range cases {
		_, err := ParsePolicy(tc.labels)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v", tc.name, err)
		}
	}
}

func at(h, m int) time.Time { return time.Date(2026, 1, 2, h, m, 0, 0, time.UTC) }

func TestInWindow(t *testing.T) {
	cases := []struct {
		window string
		t      time.Time
		want   bool
	}{
		{"", at(12, 0), true},
		{"02:00-05:00", at(1, 59), false},
		{"02:00-05:00", at(2, 0), true},
		{"02:00-05:00", at(4, 59), true},
		{"02:00-05:00", at(5, 0), false},
		{"22:00-02:00", at(21, 59), false},
		{"22:00-02:00", at(22, 0), true},
		{"22:00-02:00", at(23, 59), true},
		{"22:00-02:00", at(0, 0), true},
		{"22:00-02:00", at(1, 59), true},
		{"22:00-02:00", at(2, 0), false},
		{"02:00-03:00,23:00-23:59", at(2, 30), true},
		{"02:00-03:00,23:00-23:59", at(23, 30), true},
		{"02:00-03:00,23:00-23:59", at(12, 0), false},
		{"02:00-03:00,23:00-23:59", at(23, 59), false},
	}
	for _, tc := range cases {
		p, err := ParsePolicy(lbl("", tc.window))
		if err != nil {
			t.Fatal(err)
		}
		if got := p.InWindow(tc.t); got != tc.want {
			t.Errorf("%q at %s: got %v", tc.window, tc.t.Format("15:04"), got)
		}
	}
}
