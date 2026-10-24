// Runs the partition registry signals' real catalog query (SIGNALS.md
// §2.4p-§2.4q) on the PostgreSQL 18 TestEnv against a synthetic family whose
// tables the test makes: plain, partitioned by the registry's own runway, and
// with partitions dropped or kept past their retention. The psql/SSH
// transport stays outside (contractPartitionNativeSource).
package monitor

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A daily timestamp family and a weekly id family on tables the test makes.
func partitionRegistryNativeFamilies() []*server.PartitionFamily {
	return []*server.PartitionFamily{
		{
			Name:        "synthetic_native_history",
			Description: "a synthetic native daily history",
			Tables:      []string{"synthetic_native_history"},
			KeyColumn:   "measured_at",
			KeyKind:     server.PartitionKeyTimestamp,
			Granularity: server.PartitionGranularityDaily,
			AheadRanges: 3,
			Retention:   7 * 24 * time.Hour,
		},
		{
			Name:        "synthetic_native_weeks",
			Description: "a synthetic native weekly table",
			Tables:      []string{"synthetic_native_weeks"},
			KeyColumn:   "report_id",
			KeyKind:     server.PartitionKeyId,
			Granularity: server.PartitionGranularityWeekly,
			AheadRanges: 4,
			Retention:   91 * 24 * time.Hour,
		},
	}
}

func partitionRegistryNativeSignals(families []*server.PartitionFamily, retention time.Duration) []Signal {
	runwaySettings := DefaultPartitionRunwaySettings()
	runwaySettings.Families = func() []*server.PartitionFamily { return families }
	retentionSettings := DefaultPartitionRetentionSettings()
	retentionSettings.Families = func() []*server.PartitionFamily {
		shortened := []*server.PartitionFamily{}
		for _, family := range families {
			copied := *family
			if 0 < retention {
				copied.Retention = retention
			}
			shortened = append(shortened, &copied)
		}
		return shortened
	}
	return []Signal{NewPartitionRunwaySignalWithSettings(runwaySettings), NewPartitionRetentionBlockedSignalWithSettings(retentionSettings)}
}

// Plain tables are healthy; the registry's runway makes them healthy once
// partitioned; dropping the days ahead pages; a day kept past a shortened
// retention is blocked; the real bounds of both key kinds parse.
func TestPartitionRegistryNativeCatalog(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		families := partitionRegistryNativeFamilies()
		contractPartitionNativeExec(t, ctx, `CREATE TABLE synthetic_native_history (run_id uuid NOT NULL, measured_at timestamp NOT NULL, PRIMARY KEY (run_id, measured_at))`)
		contractPartitionNativeExec(t, ctx, `CREATE TABLE synthetic_native_weeks (report_id uuid NOT NULL PRIMARY KEY)`)
		run := func(signal Signal) Alerts {
			return contractPartitionNativeRun(t, ctx, signal)
		}
		for _, signal := range partitionRegistryNativeSignals(families, 0) {
			if alerts := run(signal); len(alerts) != 0 {
				t.Fatalf("plain tables: %s alerted: %s %s", signal.Key(), alerts[0].Class, alerts[0].Observed)
			}
		}

		contractPartitionNativeExec(t, ctx, `DROP TABLE synthetic_native_history, synthetic_native_weeks`)
		contractPartitionNativeExec(t, ctx, `CREATE TABLE synthetic_native_history (run_id uuid NOT NULL, measured_at timestamp NOT NULL, PRIMARY KEY (run_id, measured_at)) PARTITION BY RANGE (measured_at)`)
		contractPartitionNativeExec(t, ctx, `CREATE TABLE synthetic_native_weeks (report_id uuid NOT NULL PRIMARY KEY) PARTITION BY RANGE (report_id)`)
		now := server.NowUtc()
		for _, family := range families {
			// a day of history before today, so the retention has one to judge
			report := family.EnsureRunway(ctx, now.AddDate(0, 0, -10))
			if 0 < len(report.Errors) {
				t.Fatalf("%s runway: %v", family.Name, report.Errors)
			}
			report = family.EnsureRunway(ctx, now)
			if 0 < len(report.Errors) || !report.CurrentCovered || report.FutureRanges < family.AheadRanges {
				t.Fatalf("%s runway: %+v", family.Name, report)
			}
		}
		for _, signal := range partitionRegistryNativeSignals(families, 0) {
			if alerts := run(signal); len(alerts) != 0 {
				t.Fatalf("partitioned: %s alerted: %s %s", signal.Key(), alerts[0].Class, alerts[0].Observed)
			}
		}

		// the history's days ahead dropped: one day left after today pages
		for i := 2; i <= families[0].AheadRanges; i += 1 {
			day := families[0].RangeAt(now.AddDate(0, 0, i))
			contractPartitionNativeExec(t, ctx, `DROP TABLE `+families[0].LeafName("synthetic_native_history", day))
		}
		alert := contractPartitionNativeAlert(t, run(partitionRegistryNativeSignals(families, 0)[0]), "partition-runway")
		if alert.Frame != "synthetic_native_history" || !strings.Contains(alert.Observed, "worst_future_ranges=1 ") {
			t.Fatalf("a short runway was not found: %s %s", alert.Frame, alert.Observed)
		}

		// a retention of a day leaves the days ten days back blocked
		alert = contractPartitionNativeAlert(t, run(partitionRegistryNativeSignals(families[:1], 24*time.Hour)[1]), "partition-retention-blocked")
		if !strings.Contains(alert.Observed, "family=synthetic_native_history") {
			t.Fatalf("a day past its retention was not blocked: %s", alert.Observed)
		}
	})
}

// The registry probes read nothing but the catalog: the query names no family
// table outside pg_catalog lookups, and binds the hostname once.
func TestPartitionRegistryNativeQueryShape(t *testing.T) {
	query := partitionRegistryCatalogQuery(DefaultPartitionRegistryCatalogSettings(), partitionRegistryNativeFamilies())
	if strings.Contains(query, "FROM synthetic_native_history") || strings.Contains(query, "FROM public.") {
		t.Fatal("the catalog read reads a family table")
	}
	contractPartitionNativeSql(t, query)
	if !strings.Contains(query, "('synthetic_native_history',true),('synthetic_native_weeks',false)") {
		t.Fatalf("the family tables are not bound by kind: %s", query)
	}
}
