package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// provider_egress_health_history in daily partitions (PARTITION-MAINTENANCE-PLAN.md
// C2). The table is append-only: one row per accepted probe result, read only
// in windows of eight hours or less, and kept 7 days (owner decision
// 2026-10-24). Partitioned by RANGE (measured_at) with the primary key and the
// conflict target (run_id, measured_at), retention is a partition drop.
//
// A delayed result keeps its measured_at, and the ingest refuses one older
// than MaxIngestAge, so a replay is either refused or meets its own row,
// which retention keeps longer than that: it never credits cycle progress
// twice. The writer (SetProviderEgressHealth) also checks the run id across
// the retained partitions under a lock per run, for a submitter whose time the
// server assigns at arrival (a report without URL evidence), whose resend
// would otherwise repeat the run id with a new time.
//
// The table is plain until the operator's cutover
// (MigrateProviderEgressHealthHistoryToPartitions); the writer, the readers
// and the registry work on either layout.

const providerEgressHealthHistoryTable = "provider_egress_health_history"

// Tunables of the partitioned history.
type ProviderEgressHealthHistorySettings struct {
	// days after today that always have a partition
	AheadDays int
	// a day is dropped once its upper bound is this far past
	Retention time.Duration
	// the ingest refuses a measurement older than this. It must stay well
	// under Retention, so a replay is refused or meets its row, and the
	// registry creates the days back to it
	MaxIngestAge time.Duration
}

// Three days ahead, 7 days of retention, and a day of ingest delay.
func DefaultProviderEgressHealthHistorySettings() *ProviderEgressHealthHistorySettings {
	return &ProviderEgressHealthHistorySettings{
		AheadDays:    3,
		Retention:    7 * 24 * time.Hour,
		MaxIngestAge: 24 * time.Hour,
	}
}

// The history's family in the partition registry: daily by measured_at, 7
// days. The registry creates the days from the ingest's oldest through the
// days ahead and drops the expired ones; nothing else is needed, since the
// table is a log with no precondition on its drop.
func newProviderEgressHealthHistoryPartitionFamily(settings *ProviderEgressHealthHistorySettings) *server.PartitionFamily {
	return &server.PartitionFamily{
		Name:                providerEgressHealthHistoryTable,
		Description:         "provider_egress_health_history, daily by measured_at, kept 7 days; the registry's retention task drops the days",
		Tables:              []string{providerEgressHealthHistoryTable},
		KeyColumn:           "measured_at",
		KeyKind:             server.PartitionKeyTimestamp,
		Granularity:         server.PartitionGranularityDaily,
		AheadRanges:         settings.AheadDays,
		Retention:           settings.Retention,
		ReindexExempt:       true,
		MonitorRunwayRanges: 2,
		// the cutover's staged table before the swap, and the old table until
		// --drop-old
		CutoverTables: []string{providerEgressHealthHistoryStagedTable, providerEgressHealthHistoryOldTable},
		// a delayed result keeps its measured_at, so the days back to the
		// ingest's oldest must exist
		CreateFrom: func(ctx context.Context, now time.Time) time.Time {
			return now.Add(-settings.MaxIngestAge)
		},
	}
}

var providerEgressHealthHistoryPartitionFamily = server.RegisterPartitionFamily(
	newProviderEgressHealthHistoryPartitionFamily(DefaultProviderEgressHealthHistorySettings()),
)
