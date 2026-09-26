// Cross the real coverage/HMAC SQL and public reducers with fixed synthetic
// clocks. Retention changes neither scheduler cadence nor measurement status.
package monitor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// Current measurement and current darkness must use the same model authority.
func TestEgressCoverageEightHourSqlPolicy(t *testing.T) {
	query := egressCoverageActivityQuery(1, model.DefaultProviderEgressRules())
	for _, want := range []string{
		"c.blackhole_measured AND c.checked_at >= c.now_utc - interval '28800 seconds'",
		"lifecycle_clock.now_utc - interval '28800 seconds' <= pbc.checked_at",
		"COALESCE(pbc.next_due_at, pbc.checked_at + interval '5400 seconds')",
		"NOT c.blackhole_measured OR c.checked_at < c.now_utc - interval '5400 seconds'",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("coverage SQL missing independent policy boundary %q", want)
		}
	}
	if strings.Contains(query, "{{") || strings.Contains(query, "interval '3 hours'") {
		t.Fatal("coverage SQL retained an unresolved or obsolete max-age boundary")
	}
}

// A seven-hour and exact eight-hour measured pass are current. One microsecond
// older is not; scheduler-ready and refresh exposure remain separate columns.
func TestEgressCoverageEightHourMeasuredBoundary(t *testing.T) {
	clock := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		age     time.Duration
		current string
	}{
		{age: 7 * time.Hour, current: "1"},
		{age: 8 * time.Hour, current: "1"},
		{age: 8*time.Hour + time.Microsecond, current: "0"},
	} {
		_, row := egressClockFixture(t, clock, &model.ProviderBlackholeCheck{CheckedAt: clock.Add(-test.age), OK: true})
		if row[11] != test.current || row[7] != "1" || egressClockExposure(t, row) != 1 {
			t.Errorf("age %s: current=%s ready=%s, want current=%s ready=1 with exposure", test.age, row[11], row[7], test.current)
		}
	}
}

// An unmeasured retry defers its schedule without erasing still-current measured
// coverage or clearing old-evidence exposure. It adds no latest-hour check.
func TestEgressCoverageEightHourNotMeasuredClockSeparation(t *testing.T) {
	clock := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	previous := &model.ProviderBlackholeCheck{CheckedAt: clock.Add(-7 * time.Hour), OK: true}
	next, changed := model.NextProviderBlackholeCheck(previous, model.ProviderBlackholeCheckReport{
		CheckedAt: clock.Add(-time.Minute), NotMeasured: true,
	}, clock, model.DefaultProviderEgressRules())
	if !changed || !next.CheckedAt.Equal(previous.CheckedAt) {
		t.Fatal("model fixture did not preserve the measured clock")
	}
	_, row := egressClockFixture(t, clock, next)
	if row[7] != "0" || row[11] != "1" || row[13] != "0" || egressClockExposure(t, row) != 1 {
		t.Fatalf("retry lost independent clocks: ready=%s current=%s hour=%s", row[7], row[11], row[13])
	}
}

// The legacy NULL schedule remains ninety minutes, not half the new maximum.
func TestEgressCoverageEightHourKeepsNinetyMinuteSqlCadence(t *testing.T) {
	clock := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		age   time.Duration
		ready string
	}{
		{age: 90*time.Minute - time.Microsecond, ready: "0"},
		{age: 90 * time.Minute, ready: "1"},
		{age: 90*time.Minute + time.Microsecond, ready: "1"},
	} {
		_, row := egressClockFixture(t, clock, &model.ProviderBlackholeCheck{CheckedAt: clock.Add(-test.age), OK: true})
		if row[7] != test.ready || row[11] != "1" {
			t.Errorf("age %s: ready=%s current=%s", test.age, row[7], row[11])
		}
	}
}

// Retention is never a license to count an unknown first row as measurement.
func TestEgressCoverageEightHourFirstNotMeasuredRemainsUnknown(t *testing.T) {
	clock := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	unknown, _ := model.NextProviderBlackholeCheck(nil, model.ProviderBlackholeCheckReport{
		CheckedAt: clock.Add(-time.Minute), NotMeasured: true,
	}, clock, model.DefaultProviderEgressRules())
	alerts, row := egressClockFixture(t, clock, unknown)
	if row[9] != "-1" || row[11] != "0" || row[13] != "0" {
		t.Fatal("first unmeasured row manufactured coverage or throughput")
	}
	requireAlertClass(t, alerts, "egress-blackhole-stalled")
}

// The incompatibility aggregate uses the same inclusive lifetime for TLS
// negatives and passing controls, without treating a single failure as dark.
func TestHmacCutoverEightHourSqlBoundary(t *testing.T) {
	clock := storedContractHMACCutover().Add(24 * time.Hour)
	for _, test := range []struct {
		age     time.Duration
		checked int64
	}{
		{age: 7 * time.Hour, checked: 20},
		{age: 8 * time.Hour, checked: 20},
		{age: 8*time.Hour + time.Microsecond, checked: 0},
	} {
		checkedAt := clock.Add(-test.age)
		dark := &model.ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: model.ProviderBlackholeTlsAuthenticationFailure}
		pass := &model.ProviderBlackholeCheck{CheckedAt: checkedAt, OK: true}
		_, snapshot := runHmacCutoverSqlFixture(t, clock,
			hmacCutoverSqlCheckRows(1, 20, dark), hmacCutoverSqlCheckRows(21, 40, pass), false)
		if snapshot.legacyChecked != test.checked || snapshot.legacyDark != test.checked || snapshot.compatibleChecked != test.checked || snapshot.compatibleOK != test.checked {
			t.Errorf("age %s: legacy checked/dark=%d/%d compatible checked/ok=%d/%d want %d each", test.age, snapshot.legacyChecked, snapshot.legacyDark, snapshot.compatibleChecked, snapshot.compatibleOK, test.checked)
		}
	}
}

// The capacity floor changes with retention, not with the independent due
// cadence. Exact eight-hour turnover is healthy; a slower sweep still pages.
func TestEgressCoverageEightHourCapacityFloor(t *testing.T) {
	for _, test := range []struct {
		eligible int64
		alert    bool
	}{
		{eligible: 800, alert: false},
		{eligible: 801, alert: true},
	} {
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			switch {
			case strings.Contains(query, "pg_attribute"):
				return []Row{{"t", "t", "t"}}, nil
			case strings.Contains(query, "FROM pending_task"):
				return []Row{syntheticEgressCoverageTask(t, 0, 1)}, nil
			case strings.Contains(query, "WITH lifecycle_clock AS"):
				return []Row{syntheticEgressCoverageActivity(egressCoverageSnapshot{
					shardIndex: 0, eligible: test.eligible, blackholeDue: 1, blackholeVerdictDue: 1,
					fullAgeSeconds: 10, blackholeAgeSeconds: 10, fullCurrent: 1, blackholeCurrent: 200,
					fullAttemptsLastHour: 1, blackholeLastHour: 100,
					staleLocationOldestAgeSeconds: -1, staleHealthOldestAgeSeconds: -1,
				})}, nil
			default:
				t.Fatal("unexpected capacity fixture query")
				return nil, nil
			}
		}}
		alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if !test.alert {
			if requireAlertClassCount(alerts, "egress-blackhole-capacity") != 0 ||
				requireAlertClassCount(alerts, "egress-blackhole-headroom") != 1 {
				t.Errorf("exact eight-hour capacity boundary must warn for headroom without paging: %+v", alerts)
			}
			continue
		}
		alert := requireAlertClass(t, alerts, "egress-blackhole-capacity")
		for _, want := range []string{"required_per_hour=101", "verdict_max_age=8h0m0s", "projected_sweep=8h0m36s"} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Errorf("capacity evidence missing %q", want)
			}
		}
	}
}

// Document policy, rollout and clock authority without rewriting historical
// evidence or claiming that retaining older rows is new measured throughput.
func TestEgressCoverageEightHourCatalogBoundary(t *testing.T) {
	catalogBytes, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := strings.Join(strings.Fields(string(catalogBytes)), " ")
	for _, want := range []string{
		"eight-hour policy extends measured-verdict retention only",
		"Passing checks still become due after 90 minutes",
		"NotMeasured never refreshes a retained measured clock",
		"No migration, data rewrite, or cleanup-policy change is needed",
		"API selection/ingest/due readers, Taskworker provider-filter/cache publication",
		"does not require a Connect transport rollout",
		"policy reclassification, not throughput recovery or fresh success",
		"required_per_hour = ceil(eligible / max_age_hours)",
		"historical incident above used the then-current three-hour policy",
		"its eight-hour lifetime expires",
	} {
		if !strings.Contains(catalog, want) {
			t.Errorf("catalog missing policy qualification %q", want)
		}
	}
}
