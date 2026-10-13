package main

import "testing"

func TestTZUnrecognised(t *testing.T) {
	cases := []struct {
		tz, local string
		want      bool
	}{
		{"", "UTC", false},
		{"", "Local", false},
		{"UTC", "UTC", false},
		{"Etc/UTC", "UTC", false},
		{"Europe/Paris", "Europe/Paris", false},
		{"Europe/Paris", "Local", false},
		{"Europe/Pariss", "UTC", true},
		{"garbage", "UTC", true},
	}
	for _, c := range cases {
		if got := tzUnrecognised(c.tz, c.local); got != c.want {
			t.Errorf("TZ=%q local=%q: got %v", c.tz, c.local, got)
		}
	}
}
