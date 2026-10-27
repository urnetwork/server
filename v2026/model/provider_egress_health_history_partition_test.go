package model

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server/v2026"
)

// The history's pure rules (PARTITION-MAINTENANCE-PLAN.md C2): the family's
// declaration, the retention outliving every measurement the ingest takes,
// the missing-partition error, and the cutover's statements. No database is
// read; provider_egress_health_history_partition_db_test.go runs the rest.

// The family is registered daily by measured_at, 7 days, out of the REINDEX
// rotation, with the days back to the ingest's oldest in its creation window
// and the cutover's tables declared.
func TestProviderEgressHealthHistoryFamilyDeclaration(t *testing.T) {
	family := server.GetPartitionFamily(providerEgressHealthHistoryTable)
	if family == nil || family != providerEgressHealthHistoryPartitionFamily {
		t.Fatal("the history's family is not registered")
	}
	settings := DefaultProviderEgressHealthHistorySettings()
	if !slices.Equal(family.Tables, []string{providerEgressHealthHistoryTable}) || family.KeyColumn != "measured_at" ||
		family.KeyKind != server.PartitionKeyTimestamp || family.Granularity != server.PartitionGranularityDaily ||
		family.Retention != 7*24*time.Hour || family.AheadRanges != settings.AheadDays || !family.ReindexExempt ||
		family.RetireByOwnTask || family.AnalyzeByOwnTask || family.DropPrecondition != nil || family.RunwaySignal != "" || family.RetentionSignal != "" {
		t.Fatalf("the history's family is not declared as decided: %+v", family)
	}
	if !slices.Equal(family.CutoverTables, []string{providerEgressHealthHistoryStagedTable, providerEgressHealthHistoryOldTable}) {
		t.Fatalf("cutover tables %v", family.CutoverTables)
	}
	now := time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC)
	if from := family.CreateFrom(context.Background(), now); !from.Equal(now.Add(-settings.MaxIngestAge)) {
		t.Fatalf("the creation window starts %s", from)
	}
	if !server.PartitionRegistryReindexExempt("provider_egress_health_history_p20261024", false) || !server.PartitionRegistryReindexExempt(providerEgressHealthHistoryTable, true) {
		t.Fatal("the history's leaves joined the REINDEX rotation")
	}
	if server.PartitionRegistryReindexExempt(providerEgressHealthHistoryTable, false) {
		t.Fatal("the plain history left the REINDEX rotation before its cutover")
	}
}

// Whatever the ingest takes at a clock (measured from MaxIngestAge back to a
// minute ahead), its day is not expired at that clock, so a replay the ingest
// accepts always meets its row; and a day expires exactly at its upper bound
// plus 7 days, when everything the ingest takes is newer than it.
func TestProviderEgressHealthHistoryRetentionOutlivesTheIngest(t *testing.T) {
	ctx := context.Background()
	family := providerEgressHealthHistoryPartitionFamily
	settings := DefaultProviderEgressHealthHistorySettings()
	if settings.Retention <= settings.MaxIngestAge+24*time.Hour {
		t.Fatalf("retention %s does not outlive the ingest's %s and a day", settings.Retention, settings.MaxIngestAge)
	}
	start := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	for now := start; now.Before(start.AddDate(0, 0, 8)); now = now.Add(17 * time.Minute) {
		for measured := now.Add(-settings.MaxIngestAge); !measured.After(now.Add(time.Minute)); measured = measured.Add(23 * time.Minute) {
			if family.IsExpired(ctx, now, family.RangeAt(measured)) {
				t.Fatalf("at %s a measurement of %s the ingest takes is in an expired day", now, measured)
			}
		}
		oldest := now.Add(-settings.MaxIngestAge)
		if family.IsExpired(ctx, now, family.RangeAt(oldest)) {
			t.Fatalf("at %s the ingest's oldest day is expired", now)
		}
	}
	day := family.RangeAt(start)
	cut := day.Upper.Add(settings.Retention)
	if family.IsExpired(ctx, cut.Add(-time.Microsecond), day) || !family.IsExpired(ctx, cut, day) {
		t.Fatal("a day does not expire exactly 7 days after it ends")
	}
	if !day.Upper.Before(cut.Add(-settings.MaxIngestAge)) {
		t.Fatal("at a day's expiry the ingest still takes its measurements")
	}
}

// Only the insert's no-partition failure on the history becomes the
// missing-day error; a check constraint's violation, or another table's,
// stays the database's error.
func TestProviderEgressHealthInsertErrorClassification(t *testing.T) {
	measuredAt := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	raised := func(err error) (r any) {
		defer func() {
			r = recover()
		}()
		raiseProviderEgressHealthInsertErr(err, measuredAt)
		return nil
	}
	var missing *providerEgressHealthPartitionMissingError
	r := raised(&pgconn.PgError{Code: pgerrcode.CheckViolation, TableName: providerEgressHealthHistoryTable})
	if err, ok := r.(error); !ok || !errors.As(err, &missing) || !missing.measuredAt.Equal(measuredAt) {
		t.Fatalf("the missing day was not classified: %v", r)
	}
	for _, other := range []*pgconn.PgError{
		{Code: pgerrcode.CheckViolation, TableName: providerEgressHealthHistoryTable, ConstraintName: "provider_egress_health_history_ok_count_check"},
		{Code: pgerrcode.CheckViolation, TableName: "network_ping"},
		{Code: pgerrcode.UniqueViolation, TableName: providerEgressHealthHistoryTable},
	} {
		r := raised(other)
		if err, ok := r.(error); !ok || errors.As(err, &missing) {
			t.Fatalf("%+v was classified as a missing day: %v", other, r)
		}
	}
}

// The cutover's statements: each secondary index is staged under its staged
// name with its definition, a unique or invalid one is refused, renames keep
// each index's suffix and rename the key through its constraint, the mirror
// copies its window with every column, and the count and span rules hold.
func TestProviderEgressHealthHistoryCutoverStatements(t *testing.T) {
	statement, err := stagedProviderEgressHealthHistoryIndexSql(providerEgressHealthHistoryIndex{
		name:       "provider_egress_health_history_url_run",
		definition: "CREATE INDEX provider_egress_health_history_url_run ON public.provider_egress_health_history USING btree (client_id, measured_at DESC) WHERE (url_probe AND (url_probe_policy_version = 1) AND (total_count = 1) AND ((ok_count = 0) OR (ok_count = 1)))",
		valid:      true,
	})
	if err != nil || statement != `CREATE INDEX "provider_egress_health_history_new_url_run" ON public."provider_egress_health_history_new" USING btree (client_id, measured_at DESC) WHERE (url_probe AND (url_probe_policy_version = 1) AND (total_count = 1) AND ((ok_count = 0) OR (ok_count = 1)))` {
		t.Fatalf("staged index %q %v", statement, err)
	}
	for _, index := range []providerEgressHealthHistoryIndex{
		{name: "provider_egress_health_history_time", definition: "CREATE INDEX provider_egress_health_history_time ON public.provider_egress_health_history USING btree (measured_at)"},
		{name: "provider_egress_health_history_run", definition: "CREATE UNIQUE INDEX provider_egress_health_history_run ON public.provider_egress_health_history USING btree (run_id)", unique: true, valid: true},
		{name: "by_hand", definition: "CREATE INDEX by_hand ON public.provider_egress_health_history USING btree (measured_at)", valid: true},
	} {
		if _, err := stagedProviderEgressHealthHistoryIndexSql(index); err == nil {
			t.Fatalf("%s was staged", index.name)
		}
	}

	renames, err := providerEgressHealthHistoryRenameSql([]providerEgressHealthHistoryIndex{
		{name: "provider_egress_health_history_pkey", primary: true, unique: true},
		{name: "provider_egress_health_history_time"},
	}, providerEgressHealthHistoryTable, providerEgressHealthHistoryOldTable)
	if err != nil || !slices.Equal(renames, []string{
		`ALTER TABLE public."provider_egress_health_history" RENAME TO "provider_egress_health_history_old"`,
		`ALTER TABLE public."provider_egress_health_history_old" RENAME CONSTRAINT "provider_egress_health_history_pkey" TO "provider_egress_health_history_old_pkey"`,
		`ALTER INDEX public."provider_egress_health_history_time" RENAME TO "provider_egress_health_history_old_time"`,
	}) {
		t.Fatalf("renames %v %v", renames, err)
	}
	back, err := providerEgressHealthHistoryRenameSql([]providerEgressHealthHistoryIndex{{name: "provider_egress_health_history_new_time"}}, providerEgressHealthHistoryStagedTable, providerEgressHealthHistoryTable)
	if err != nil || back[1] != `ALTER INDEX public."provider_egress_health_history_new_time" RENAME TO "provider_egress_health_history_time"` {
		t.Fatalf("staged renames %v %v", back, err)
	}

	from := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	mirror := providerEgressHealthHistoryMirrorSql(providerEgressHealthHistoryMirror, providerEgressHealthHistoryStagedTable, []providerEgressHealthHistoryColumn{
		{name: "run_id", typeName: "uuid", notNull: true},
		{name: "measured_at", typeName: "timestamp without time zone", notNull: true},
	}, from, until)
	for _, want := range []string{
		"CREATE OR REPLACE FUNCTION provider_egress_health_history_cutover_mirror() RETURNS trigger",
		"IF '2026-10-23 00:00:00'::timestamp <= NEW.measured_at AND NEW.measured_at < '2026-11-01 00:00:00'::timestamp THEN",
		`INSERT INTO public."provider_egress_health_history_new" ("run_id", "measured_at") VALUES (NEW."run_id", NEW."measured_at") ON CONFLICT DO NOTHING`,
		"RETURN NULL",
	} {
		if !strings.Contains(mirror, want) {
			t.Fatalf("mirror lost %q:\n%s", want, mirror)
		}
	}

	hour := func(h int) time.Time { return from.Add(time.Duration(h) * time.Hour) }
	differ := providerEgressHealthHistoryCountDifferences(
		map[time.Time]int64{hour(0): 3, hour(1): 2, hour(3): 1},
		map[time.Time]int64{hour(0): 3, hour(1): 1, hour(2): 4, hour(4): 0},
	)
	if !slices.Equal(differ, []time.Time{hour(1), hour(2), hour(3)}) {
		t.Fatalf("differing hours %v", differ)
	}

	day := func(offset int) server.PartitionLeaf {
		r := server.PartitionGranularityDaily.RangeOf(from.AddDate(0, 0, offset))
		return server.PartitionLeaf{Table: providerEgressHealthHistoryTable, Name: providerEgressHealthHistoryPartitionFamily.LeafName(providerEgressHealthHistoryTable, r), Range: r, Canonical: true}
	}
	spanFrom, spanUntil, ok := providerEgressHealthHistoryLeafSpan([]server.PartitionLeaf{day(2), day(0), day(1), day(4)})
	if !ok || !spanFrom.Equal(from) || !spanUntil.Equal(from.AddDate(0, 0, 3)) {
		t.Fatalf("span %s %s %t", spanFrom, spanUntil, ok)
	}
	if _, _, ok := providerEgressHealthHistoryLeafSpan(nil); ok {
		t.Fatal("no leaves had a span")
	}
}

// Steps are exclusive and the copy window holds the ingest's day; both are
// refused before any database is read.
func TestProviderEgressHealthHistoryCutoverOptions(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC)
	if err := MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{Swap: true, Rollback: true}); err == nil {
		t.Fatal("two steps ran together")
	}
	if err := MigrateProviderEgressHealthHistoryToPartitions(ctx, now, ProviderEgressHealthHistoryCutoverOptions{CopyDays: 1}); err == nil || !strings.Contains(err.Error(), "at least 2") {
		t.Fatalf("a one-day window was taken: %v", err)
	}
	settings := DefaultProviderEgressHealthHistoryCutoverSettings()
	if settings.StagedAheadDays <= DefaultProviderEgressHealthHistorySettings().AheadDays || settings.LockTimeout <= 0 || settings.SwapStatementTimeout <= settings.LockTimeout ||
		settings.LockAttempts < 1 || settings.CopyChunk <= 0 {
		t.Fatalf("cutover settings %+v", settings)
	}
}
