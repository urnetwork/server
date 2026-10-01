// Hold measured retention, dark expiry and refresh cadence to independent
// policy boundaries. No external service or real provider identity is used.
package model

import (
	"testing"
	"time"
)

// A retention change must not silently slow the passing-check scheduler.
func TestProviderBlackholeEightHourPolicy(t *testing.T) {
	if ProviderBlackholeCheckMaxAge != 8*time.Hour {
		t.Fatalf("verdict retention = %s, want eight hours", ProviderBlackholeCheckMaxAge)
	}
	if ProviderBlackholeCheckDueAge != 90*time.Minute {
		t.Fatalf("passing refresh cadence = %s, want ninety minutes", ProviderBlackholeCheckDueAge)
	}
}

// Ordinary negative evidence keeps its existing streak/span rule at the new
// inclusive age boundary; a single failed check does not become dark.
func TestProviderBlackholeEightHourDarkBoundary(t *testing.T) {
	clock := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	rules := DefaultProviderEgressRules()
	for _, test := range []struct {
		age  time.Duration
		dark bool
	}{
		{age: 7 * time.Hour, dark: true},
		{age: 8 * time.Hour, dark: true},
		{age: 8*time.Hour + time.Microsecond, dark: false},
	} {
		checkedAt := clock.Add(-test.age)
		firstFailed := checkedAt.Add(-rules.DarkMinimumSpan())
		check := &ProviderBlackholeCheck{
			CheckedAt: checkedAt, Failure: "all_destinations_failed",
			ConsecutiveFailures: rules.DarkConsecutiveFailures, FirstFailedAt: &firstFailed,
		}
		if got := check.IsDark(clock, rules); got != test.dark {
			t.Errorf("age %s: dark=%t, want %t", test.age, got, test.dark)
		}
		check.ConsecutiveFailures = 1
		if check.IsDark(clock, rules) {
			t.Errorf("age %s: single ordinary failure became dark", test.age)
		}
	}
}

// TLS remains immediate evidence, but does not outlive measured retention.
func TestProviderBlackholeEightHourTlsBoundary(t *testing.T) {
	clock := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	rules := DefaultProviderEgressRules()
	for _, test := range []struct {
		age  time.Duration
		dark bool
	}{
		{age: 7 * time.Hour, dark: true},
		{age: 8 * time.Hour, dark: true},
		{age: 8*time.Hour + time.Microsecond, dark: false},
	} {
		check := &ProviderBlackholeCheck{CheckedAt: clock.Add(-test.age), Failure: ProviderBlackholeTlsAuthenticationFailure}
		if got := check.IsDark(clock, rules); got != test.dark {
			t.Errorf("TLS age %s: dark=%t, want %t", test.age, got, test.dark)
		}
	}
}

// Passing scheduling uses publication time, not the earlier check start, and
// remains ninety minutes even when the check itself took a long time.
func TestProviderBlackholeEightHourPassKeepsNinetyMinuteCadence(t *testing.T) {
	clock := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	next, changed := NextProviderBlackholeCheck(nil, ProviderBlackholeCheckReport{
		CheckedAt: clock.Add(-40 * time.Minute), Ok: true,
	}, clock, DefaultProviderEgressRules())
	if !changed || next.NextDueAt == nil || !next.NextDueAt.Equal(clock.Add(90*time.Minute)) {
		t.Fatal("pass no longer schedules ninety minutes after ingest")
	}
	if !next.CheckedAt.Equal(clock.Add(-40*time.Minute)) || next.IsDark(clock, DefaultProviderEgressRules()) {
		t.Fatal("passing measurement time or dark classification changed")
	}
}

// An unmeasured retry preserves current negative evidence instead of minting a
// new measurement or shortening the newly authorized retention window.
func TestProviderBlackholeEightHourNotMeasuredPreservesCurrent(t *testing.T) {
	clock := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	rules := DefaultProviderEgressRules()
	previous := &ProviderBlackholeCheck{CheckedAt: clock.Add(-7 * time.Hour), Failure: ProviderBlackholeTlsAuthenticationFailure}
	next, changed := NextProviderBlackholeCheck(previous, ProviderBlackholeCheckReport{
		CheckedAt: clock.Add(-time.Minute), NotMeasured: true,
	}, clock, rules)
	if !changed || !next.CheckedAt.Equal(previous.CheckedAt) || next.Failure != previous.Failure || !next.IsDark(clock, rules) {
		t.Fatal("unmeasured retry lost current seven-hour TLS evidence")
	}
	if next.NextDueAt == nil || !next.NextDueAt.Equal(clock.Add(rules.DarkBackoff(0))) {
		t.Fatal("unmeasured retry backoff changed")
	}
}

// Publication of an unmeasured result never revives an expired measured row.
func TestProviderBlackholeEightHourNotMeasuredDoesNotRefreshExpired(t *testing.T) {
	clock := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	previous := &ProviderBlackholeCheck{CheckedAt: clock.Add(-8*time.Hour - time.Microsecond), Failure: ProviderBlackholeTlsAuthenticationFailure}
	next, changed := NextProviderBlackholeCheck(previous, ProviderBlackholeCheckReport{
		CheckedAt: clock.Add(-time.Minute), NotMeasured: true,
	}, clock, DefaultProviderEgressRules())
	if !changed || !next.CheckedAt.Equal(previous.CheckedAt) || next.IsDark(clock, DefaultProviderEgressRules()) {
		t.Fatal("unmeasured retry refreshed expired evidence")
	}
}

// First unmeasured rows and measured passes must not become dark at any age.
func TestProviderBlackholeEightHourHealthyAndUnknownControls(t *testing.T) {
	clock := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	rules := DefaultProviderEgressRules()
	unknown, _ := NextProviderBlackholeCheck(nil, ProviderBlackholeCheckReport{
		CheckedAt: clock, NotMeasured: true,
	}, clock, rules)
	if unknown.IsDark(clock, rules) || unknown.Failure != ProviderBlackholeNotMeasuredFailure {
		t.Fatal("unknown first row became a measured negative verdict")
	}
	for _, age := range []time.Duration{time.Minute, 7 * time.Hour, 8 * time.Hour, 9 * time.Hour} {
		if (&ProviderBlackholeCheck{CheckedAt: clock.Add(-age), OK: true}).IsDark(clock, rules) {
			t.Errorf("passing row became dark at age %s", age)
		}
	}
}
