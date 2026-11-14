package monitor

// SIGNALS.md §17.7: operator blocks the daily points award recorded expired.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Answers the preflight with the migration head and table presence, and the
// observation with rows; counts the observations.
func operatorBlockPointsTestSource(t testing.TB, head int, present bool, rows []Row) (*syntheticSource, *int) {
	observationCount := 0
	presence := "f"
	if present {
		presence = "t"
	}
	return &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "monitor-signal-17.7-operator-block-points-schema"):
			return []Row{{fmt.Sprintf("%d", head), presence}}, nil
		case strings.Contains(query, "monitor-signal-17.7-operator-block-points"):
			observationCount++
			return rows, nil
		}
		t.Fatalf("unexpected query: %s", query)
		return nil, nil
	}}, &observationCount
}

// Below the migration nothing is recorded and the observation is never
// issued; at or past it the table must exist.
func TestOperatorBlockPointsMigrationGate(t *testing.T) {
	version := server.OperatorBlockPointsExpiredVersion()
	if version <= 0 {
		t.Fatal("no operator block points expiry migration")
	}
	source, observationCount := operatorBlockPointsTestSource(t, version-1, false, nil)
	alerts, err := NewOperatorBlockPointsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 || *observationCount != 0 {
		t.Fatalf("below the migration: alerts=%+v err=%v observations=%d", alerts, err, *observationCount)
	}
	source, _ = operatorBlockPointsTestSource(t, version, false, nil)
	if _, err := NewOperatorBlockPointsSignal().Run(context.Background(), syntheticSettings(source)); err == nil {
		t.Fatal("a missing table at the migration head was not an observation failure")
	}
}

// Each block recorded expired inside the window warns once, under its own
// identity, and says that no backfill can award it, how to award the next
// block by hand before its deadline, and where to look for why the award did
// not run. No record is healthy.
func TestOperatorBlockPointsExpiredBlockWarns(t *testing.T) {
	version := server.OperatorBlockPointsExpiredVersion()
	source, _ := operatorBlockPointsTestSource(t, version, true, []Row{
		{"21", "2026-11-15T00:00:00Z", "2026-11-22T00:00:00Z", "29387520", "29391840", "7200"},
		{"22", "2026-11-22T00:00:00Z", "2026-11-29T00:00:00Z", "29397600", "29401920", "60"},
	})
	alerts, err := NewOperatorBlockPointsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if count := requireAlertClassCount(alerts, "operator-block-points-expired"); count != 2 {
		t.Fatalf("expired alerts = %d, want one per block", count)
	}
	alert := requireAlertClass(t, alerts, "operator-block-points-expired")
	if alert.Severity != SeverityWarn || alert.Target != "operator-block-21" {
		t.Fatalf("expired alert = %s %s, want warn operator-block-21", alert.Severity, alert.Target)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"Operator block 21 [2026-11-15T00:00:00Z, 2026-11-22T00:00:00Z) expired without its points",
		"block=21 window_start=2026-11-15T00:00:00Z window_end=2026-11-22T00:00:00Z reliability_min_block=29387520 retention_min_block=29391840 recorded_age_hours=2",
		"No backfill exists for an expired block",
		"bringyourctl account-points backfill-blocks --blocks=<block> --file=<file>",
		"bringyourctl account-points backfill-blocks --commit --expect=<digest> --file=<file>",
		"award_operator_block_points",
		"urnetwork_operator_block_points_expired_blocks_total",
		"SIGNALS.md §17.7",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("expired alert lacks %q:\n%s", want, markdown)
		}
	}

	source, _ = operatorBlockPointsTestSource(t, version, true, []Row{})
	alerts, err = NewOperatorBlockPointsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || requireAlertClassCount(alerts, "operator-block-points-expired") != 0 {
		t.Fatalf("no record: alerts=%+v err=%v", alerts, err)
	}
}

func TestOperatorBlockPointsRejectsMalformedRecords(t *testing.T) {
	version := server.OperatorBlockPointsExpiredVersion()
	valid := Row{"21", "2026-11-15T00:00:00Z", "2026-11-22T00:00:00Z", "29387520", "29391840", "7200"}
	for name, rows := range map[string][]Row{
		"wrong columns":           {valid[:5]},
		"zero block":              {{"0", valid[1], valid[2], valid[3], valid[4], valid[5]}},
		"blocks out of order":     {{"22", valid[1], valid[2], valid[3], valid[4], valid[5]}, valid},
		"repeated block":          {valid, valid},
		"unparsed window":         {{valid[0], "2026-11-15 00:00", valid[2], valid[3], valid[4], valid[5]}},
		"empty window":            {{valid[0], valid[2], valid[2], valid[3], valid[4], valid[5]}},
		"retained reliability":    {{valid[0], valid[1], valid[2], valid[3], valid[3], valid[5]}},
		"non-numeric reliability": {{valid[0], valid[1], valid[2], "soon", valid[4], valid[5]}},
		"negative age":            {{valid[0], valid[1], valid[2], valid[3], valid[4], "-1"}},
	} {
		source, _ := operatorBlockPointsTestSource(t, version, true, rows)
		alerts, err := NewOperatorBlockPointsSignal().Run(context.Background(), syntheticSettings(source))
		if requireAlertClassCount(alerts, "operator-block-points-expired") != 0 {
			t.Errorf("%s: a malformed record alerted as expired", name)
		}
		if err == nil {
			t.Errorf("%s: a malformed record was not an observation failure", name)
		}
	}
}

// The observation reads the expiry records written inside the window, at the
// probe's settings, in block order and bounded, and exports only block
// numbers, times and reliability block numbers.
func TestOperatorBlockPointsQueryContract(t *testing.T) {
	query := operatorBlockPointsQueryWith(&OperatorBlockPointsSettings{RecentWindow: 3 * 24 * time.Hour, MaxBlocks: 4})
	for _, required := range []string{
		"/* monitor-signal-17.7-operator-block-points */",
		"FROM operator_block_points_expired",
		"WHERE create_time > (now() AT TIME ZONE 'UTC') - interval '259200 seconds'",
		"ORDER BY block_number",
		"LIMIT 4",
	} {
		if !strings.Contains(query, required) {
			t.Errorf("operator block points query lacks %q", required)
		}
	}
	for _, forbidden := range []string{"network_id", "payment_id", "account_point", "INSERT", "UPDATE", "DELETE"} {
		if strings.Contains(query, forbidden) {
			t.Errorf("operator block points query names %q", forbidden)
		}
	}
	defaults := DefaultOperatorBlockPointsSettings()
	if defaults.RecentWindow != 7*24*time.Hour || defaults.MaxBlocks < 1 || DefaultOperatorBlockPointsSettings() == defaults {
		t.Fatalf("defaults %+v", defaults)
	}
}
