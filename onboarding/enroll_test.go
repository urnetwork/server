package onboarding

import (
	"testing"
	"time"
)

func TestEnroll(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour)
	old := now.Add(-EnrollmentHorizon - time.Hour)
	cases := []struct {
		name          string
		hasEmailLogin bool
		viaDevice     bool
		createdAt     time.Time
		want          bool
	}{
		{"account path enrolls an email login", true, false, fresh, true},
		{"account path leaves a seed-phrase account to the device path", false, false, fresh, false},
		{"device path enrolls a seed-phrase account's first device", false, true, fresh, true},
		{"device path leaves an email login to the account path", true, true, fresh, false},
		{"device path does not enroll an old account", false, true, old, false},
		{"device path enrolls right at the horizon", false, true, now.Add(-EnrollmentHorizon), true},
	}
	for _, c := range cases {
		if got := Enroll(c.hasEmailLogin, c.viaDevice, c.createdAt, now); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
