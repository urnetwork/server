// The session journal's partition family, its published SQL and the cutover's
// statements, without a database (PARTITION-MAINTENANCE-PLAN.md C4). The DB
// tests are in db_provider_work_session_partition_db_test.go.
package server

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The journal's catalog as migrated through v858 (read from PostgreSQL 18 with
// readContractTableShape's queries), with the given primary key.
func testProviderWorkSessionEventShape(primaryKey string) *contractTableShape {
	notNull := func(name string) string {
		return "provider_work_session_event_" + name + "_not_null"
	}
	shape := &contractTableShape{
		table:   providerWorkSessionEventTable,
		relkind: "r",
		columns: []contractColumn{
			{name: "client_id", typeName: "uuid", notNull: true, notNullName: notNull("client_id"), statisticsTarget: -1},
			{name: "sequence", typeName: "bigint", notNull: true, notNullName: notNull("sequence"), statisticsTarget: -1},
			{name: "network_id", typeName: "uuid", statisticsTarget: -1},
			{name: "connection_id", typeName: "uuid", statisticsTarget: -1},
			{name: "kind", typeName: "text", notNull: true, notNullName: notNull("kind"), statisticsTarget: -1},
			{name: "observed_at", typeName: "timestamp without time zone", notNull: true, notNullName: notNull("observed_at"), statisticsTarget: -1},
			{name: "extender_id", typeName: "uuid", statisticsTarget: -1},
			{name: "transaction_id", typeName: "bigint", notNull: true, notNullName: notNull("transaction_id"), statisticsTarget: -1},
		},
		checks: []contractConstraint{
			{name: "provider_work_session_event_kind_check", definition: "CHECK ((kind = ANY (ARRAY['baseline'::text, 'admit'::text, 'retire'::text])))", validated: true},
			{name: "provider_work_session_event_sequence_check", definition: "CHECK ((sequence > 0))", validated: true},
		},
		indexes: []contractIndex{
			{
				name:       "provider_work_session_event_connection",
				definition: "CREATE INDEX provider_work_session_event_connection ON public.provider_work_session_event USING btree (client_id, connection_id, sequence)",
				valid:      true,
				ready:      true,
			},
		},
		triggers: []contractTrigger{
			{
				name:       "original_guard",
				definition: "CREATE TRIGGER original_guard BEFORE DELETE OR UPDATE ON public.provider_work_session_event FOR EACH ROW EXECUTE FUNCTION provider_work_original_guard()",
				enabled:    "O",
				row:        true,
			},
			{
				name:       "original_truncate_guard",
				definition: "CREATE TRIGGER original_truncate_guard BEFORE TRUNCATE ON public.provider_work_session_event FOR EACH STATEMENT EXECUTE FUNCTION provider_work_original_guard()",
				enabled:    "O",
				truncate:   true,
			},
		},
	}
	columns := strings.TrimSuffix(strings.TrimPrefix(primaryKey, "PRIMARY KEY ("), ")")
	shape.indexes = append(shape.indexes, contractIndex{
		name:                 providerWorkSessionEventPrimaryKeyName,
		definition:           "CREATE UNIQUE INDEX provider_work_session_event_pkey ON public.provider_work_session_event USING btree (" + columns + ")",
		valid:                true,
		ready:                true,
		primary:              true,
		unique:               true,
		constraint:           providerWorkSessionEventPrimaryKeyName,
		constraintDefinition: primaryKey,
	})
	if primaryKey == providerWorkSessionEventSequenceKey {
		shape.referencedBy = []contractForeignKey{{
			name:            providerWorkSessionReceiptForeignKeyName,
			table:           providerWorkSessionReceiptTable,
			referencedTable: providerWorkSessionEventTable,
			definition:      "FOREIGN KEY (client_id, sequence) REFERENCES provider_work_session_event(client_id, sequence)",
		}}
	}
	return shape
}

// The staged parent as LIKE ... INCLUDING INDEXES and the renames leave it.
func testProviderWorkSessionStagedShape() *contractTableShape {
	shape := testProviderWorkSessionEventShape(providerWorkSessionEventIdentityKey)
	staged := &contractTableShape{
		table:   providerWorkSessionStagedParent(),
		relkind: "p",
		columns: shape.columns,
		checks:  shape.checks,
	}
	for _, index := range shape.indexes {
		index.name += contractStagedIndexSuffix
		staged.indexes = append(staged.indexes, index)
	}
	return staged
}

func testProviderWorkSessionCutover() *providerWorkSessionCutover {
	return newProviderWorkSessionCutover(DefaultProviderWorkSessionPartitionSettings(), false, func(string, ...any) {})
}

// The family is registered as decided: weekly observed_at ranges from Monday
// 00:00 UTC, 13 weeks of retention that the legacy partition shares, the opt-in
// drop guard and its fence, and the hook that abandons a stale cutover.
func TestProviderWorkSessionEventFamilyDeclaration(t *testing.T) {
	family := GetPartitionFamily("provider_work_session_event")
	if family == nil || family != providerWorkSessionEventPartitionFamily {
		t.Fatal("the journal's family is not registered")
	}
	if !slices.Equal(family.Tables, []string{"provider_work_session_event"}) || family.KeyColumn != "observed_at" ||
		family.KeyKind != PartitionKeyTimestamp || family.Granularity != PartitionGranularityWeekly {
		t.Fatalf("family shape %+v", family)
	}
	if family.Retention != ContractRetention || family.Retention/(24*time.Hour) != 91 {
		t.Fatalf("retention %s is not the 13 weeks of the contracts", family.Retention)
	}
	if family.AheadRanges < 2 || family.DefaultPartition || family.KeepLegacyPartition {
		t.Fatalf("runway %d, default %t, kept legacy %t", family.AheadRanges, family.DefaultPartition, family.KeepLegacyPartition)
	}
	if family.DropGuardSetting != "urnetwork.provider_work_session_retention" || family.DropFence == nil || family.BeforeCutover == nil {
		t.Fatal("the drop is not behind its opt-in guard and fence")
	}
	if family.AnalyzeByOwnTask || family.RetireByOwnTask {
		t.Fatal("the registry's retention task must be the one that drops")
	}
	if !family.ReindexExempt || !PartitionRegistryReindexExempt("provider_work_session_event_w20261026", false) ||
		!PartitionRegistryReindexExempt("provider_work_session_event_legacy", false) {
		t.Fatal("the journal's leaves stay in the REINDEX rotation")
	}
	if PartitionFamilyOfTable("provider_work_session_event_w20261026") != family || PartitionFamilyOfTable("provider_work_session_receipt") != nil {
		t.Fatal("the family owns the wrong tables")
	}
}

// A week 12 weeks and 6 days past its upper bound is kept and one 13 weeks past
// expires, the bound itself included; the legacy partition, whose upper bound
// is the cutover week, expires 13 weeks after it.
func TestProviderWorkSessionEventRetentionBoundary(t *testing.T) {
	ctx := context.Background()
	family := providerWorkSessionEventPartitionFamily
	week := family.RangeAt(time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC))
	if !week.Lower.Equal(time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)) || !week.Upper.Equal(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("week %+v", week)
	}
	if family.IsExpired(ctx, week.Upper.AddDate(0, 0, 12*7+6), week) {
		t.Error("a week 12 weeks and 6 days past expired")
	}
	if family.IsExpired(ctx, week.Upper.AddDate(0, 0, 13*7).Add(-time.Nanosecond), week) {
		t.Error("a week expired before 13 weeks")
	}
	if !family.IsExpired(ctx, week.Upper.AddDate(0, 0, 13*7), week) {
		t.Error("a week 13 weeks past did not expire")
	}
	cutoverWeek := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	legacy := PartitionRange{Upper: cutoverWeek, Legacy: true}
	if family.IsExpired(ctx, cutoverWeek.AddDate(0, 0, 13*7).Add(-time.Nanosecond), legacy) {
		t.Error("the legacy partition expired before the cutover week plus 13 weeks")
	}
	if !family.IsExpired(ctx, cutoverWeek.AddDate(0, 0, 13*7), legacy) {
		t.Error("the legacy partition did not expire at the cutover week plus 13 weeks")
	}
}

// The fence names leaves as the registry does.
func TestProviderWorkSessionEventLeafNames(t *testing.T) {
	family := providerWorkSessionEventPartitionFamily
	for at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); at.Before(time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)); at = at.Add(29 * time.Hour) {
		r := family.RangeAt(at)
		if name := providerWorkSessionEventLeaf(r); name != family.LeafName(providerWorkSessionEventTable, r) {
			t.Fatalf("%s names %s, the registry %s", at, name, family.LeafName(providerWorkSessionEventTable, r))
		}
	}
	if name := providerWorkSessionEventLeaf(family.RangeAt(time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))); name != "provider_work_session_event_w20261026" {
		t.Errorf("weekly leaf %s", name)
	}
	legacy := PartitionRange{Upper: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Legacy: true}
	if name := providerWorkSessionEventLeaf(legacy); name != "provider_work_session_event_legacy" || name != family.LeafName(providerWorkSessionEventTable, legacy) {
		t.Errorf("legacy leaf %s", name)
	}
	if r, ok := family.ParseLeafName(providerWorkSessionEventTable, "provider_work_session_event_w20261026"); !ok || !r.Lower.Equal(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("parse %+v %t", r, ok)
	}
}

// Each published body is delimited exactly once, and the migration changes
// functions and the receipt table only: no statement takes a lock on the
// event table.
func TestProviderWorkSessionPartitionPublishedSql(t *testing.T) {
	for _, tag := range []string{"$admit_floor$", "$session_append$", "$receipt_reference$", "$event_drop_guard$"} {
		if count := strings.Count(providerWorkSessionPartitionSchemaSql, tag); count != 2 {
			t.Fatalf("%s occurs %d times", tag, count)
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(providerWorkSessionPartitionSchemaSql), "SET LOCAL lock_timeout = '5s';") {
		t.Fatal("the migration does not bound its lock waits first")
	}
	for _, statement := range []string{"ALTER TABLE", "LOCK TABLE", "DROP CONSTRAINT", "CREATE INDEX", "DROP TRIGGER"} {
		if strings.Contains(providerWorkSessionPartitionSchemaSql, statement) {
			t.Fatalf("the migration runs %s", statement)
		}
	}
	admitFloor, sessionAppend, receiptReference, dropGuard := ProviderWorkSessionPartitionBodiesSql()
	if !strings.Contains(admitFloor, "interval '1 day'") || !strings.Contains(admitFloor, "substr(replace(connection::text,'-',''),1,12)") {
		t.Fatalf("admit floor body %q", admitFloor)
	}
	if !strings.Contains(receiptReference, "RAISE EXCEPTION") || !strings.Contains(receiptReference, "ERRCODE='23503'") {
		t.Fatalf("receipt reference body %q", receiptReference)
	}
	days := int(ProviderWorkSessionEventRetention / (24 * time.Hour))
	if !strings.Contains(dropGuard, "interval '"+strconv.Itoa(days)+" days'") || !strings.Contains(dropGuard, "'"+providerWorkSessionRetentionSetting+"'") {
		t.Fatalf("the drop guard does not hold the family's %d days behind its opt-in: %q", days, dropGuard)
	}

	// the append is v776's but for the bounded retire lookup and its fallback
	original := strings.Split(providerWorkSessionSchemaSql, "$session_append$")[1]
	bounded := `  SELECT network_id INTO owner_network FROM provider_work_session_event
   WHERE client_id=client AND connection_id=connection AND kind='admit'
   AND observed_at>=provider_work_session_admit_floor(connection) ORDER BY sequence DESC LIMIT 1;
  IF NOT FOUND THEN
   SELECT network_id INTO owner_network FROM provider_work_session_event
    WHERE client_id=client AND connection_id=connection AND kind='admit' ORDER BY sequence DESC LIMIT 1;
  END IF;
`
	unbounded := `  SELECT network_id INTO owner_network FROM provider_work_session_event
   WHERE client_id=client AND connection_id=connection AND kind='admit' ORDER BY sequence DESC LIMIT 1;
`
	if strings.Count(sessionAppend, bounded) != 1 || strings.Replace(sessionAppend, bounded, unbounded, 1) != original {
		t.Fatalf("the append differs from v776 beyond its retire lookup:\n%s", sessionAppend)
	}
}

// The floor the SQL function computes is the connection id's mint millisecond
// a day back: the Go side of server.NewId agrees.
func TestProviderWorkSessionAdmitFloorMatchesIdTime(t *testing.T) {
	at := time.Date(2026, 10, 24, 13, 14, 15, 678000000, time.UTC)
	id := testContractIdAt(at)
	if got := ContractIdTime(id); !got.Equal(at) {
		t.Fatalf("id time %s, want %s", got, at)
	}
	// the function reads the id's first 12 hex digits: its 48-bit millisecond
	hexDigits := strings.ReplaceAll(id.String(), "-", "")[:12]
	var ms int64
	for _, digit := range hexDigits {
		ms = ms*16 + int64(strings.IndexRune("0123456789abcdef", digit))
	}
	if ms != at.UnixMilli() {
		t.Fatalf("first 12 hex digits %s are %d, want %d", hexDigits, ms, at.UnixMilli())
	}
}

// The cutover's statements for the journal as migrated: the identity swap
// locks in the writers' order and drops the receipt key before the primary
// key it depends on; staging clones every index; a week's leaf carries the
// TRUNCATE guard and the registry's name and bounds; the swap drops only the
// row trigger before the attach and recreates both triggers on the parent.
func TestProviderWorkSessionCutoverSql(t *testing.T) {
	cutover := testProviderWorkSessionCutover()
	cutoverWeek := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	bounds := []string{"SET LOCAL lock_timeout = '3000ms'", "SET LOCAL statement_timeout = '30000ms'"}

	swap := cutover.identitySwapSql()
	if want := append(slices.Clone(bounds),
		`LOCK TABLE public."provider_work_session_event", public."provider_work_session_receipt" IN ACCESS EXCLUSIVE MODE`,
		`ALTER TABLE public."provider_work_session_receipt" DROP CONSTRAINT IF EXISTS "provider_work_session_receipt_client_id_sequence_fkey"`,
		`ALTER TABLE public."provider_work_session_event" DROP CONSTRAINT "provider_work_session_event_pkey"`,
		`ALTER TABLE public."provider_work_session_event" ADD CONSTRAINT "provider_work_session_event_pkey" PRIMARY KEY USING INDEX "provider_work_session_event_identity"`,
	); !slices.Equal(swap, want) {
		t.Fatalf("identity swap:\n%s", strings.Join(swap, "\n"))
	}
	drop, build := providerWorkSessionIdentityIndexSql()
	if drop != `DROP INDEX CONCURRENTLY IF EXISTS public."provider_work_session_event_identity"` ||
		build != `CREATE UNIQUE INDEX CONCURRENTLY "provider_work_session_event_identity" ON public."provider_work_session_event" USING btree (client_id, sequence, observed_at)` {
		t.Fatalf("identity index:\n%s\n%s", drop, build)
	}

	shape := testProviderWorkSessionEventShape(providerWorkSessionEventIdentityKey)
	staging := providerWorkSessionStagedParentSql(shape)
	if want := []string{
		`CREATE TABLE public."provider_work_session_event_p" (LIKE public."provider_work_session_event" INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING INDEXES INCLUDING STORAGE INCLUDING COMPRESSION INCLUDING COMMENTS) PARTITION BY RANGE (observed_at)`,
		`ALTER TABLE public."provider_work_session_event_p" ADD CONSTRAINT "provider_work_session_event_kind_check" CHECK ((kind = ANY (ARRAY['baseline'::text, 'admit'::text, 'retire'::text])))`,
		`ALTER TABLE public."provider_work_session_event_p" ADD CONSTRAINT "provider_work_session_event_sequence_check" CHECK ((sequence > 0))`,
	}; !slices.Equal(staging, want) {
		t.Fatalf("staging:\n%s", strings.Join(staging, "\n"))
	}

	staged := testProviderWorkSessionStagedShape()
	leaf, err := providerWorkSessionWeekLeafSql(staged.table, staged, shape, cutoverWeek)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(leaf[0], `CREATE TABLE public."provider_work_session_event_w20261102" (`) ||
		!slices.Contains(leaf, `CREATE TRIGGER original_truncate_guard BEFORE TRUNCATE ON public."provider_work_session_event_w20261102" FOR EACH STATEMENT EXECUTE FUNCTION provider_work_original_guard()`) ||
		leaf[len(leaf)-1] != `ALTER TABLE public."provider_work_session_event_p" ATTACH PARTITION public."provider_work_session_event_w20261102" FOR VALUES FROM ('2026-11-02 00:00:00') TO ('2026-11-09 00:00:00')` {
		t.Fatalf("week leaf:\n%s", strings.Join(leaf, "\n"))
	}
	for _, check := range shape.checks {
		if !slices.ContainsFunc(leaf, func(statement string) bool { return strings.Contains(statement, check.definition) }) {
			t.Fatalf("week leaf lacks %s", check.name)
		}
	}
	if slices.ContainsFunc(leaf, func(statement string) bool { return strings.Contains(statement, "original_guard BEFORE") }) {
		t.Fatal("the week leaf repeats the row guard, which the parent clones on attach")
	}

	if bound := providerWorkSessionLegacyBoundSql(cutoverWeek); bound != `ALTER TABLE public."provider_work_session_event" ADD CONSTRAINT "provider_work_session_event_legacy_bound" CHECK (observed_at < '2026-11-02 00:00:00') NOT VALID` {
		t.Fatalf("legacy bound: %s", bound)
	}

	statements := cutover.swapSql(shape, staged, cutoverWeek)
	if want := append(slices.Clone(bounds),
		`LOCK TABLE public."provider_work_session_event", public."provider_work_session_event_p" IN ACCESS EXCLUSIVE MODE`,
		`DROP TRIGGER "original_guard" ON public."provider_work_session_event"`,
		`ALTER TABLE public."provider_work_session_event" RENAME TO "provider_work_session_event_legacy"`,
		`ALTER INDEX public."provider_work_session_event_connection" RENAME TO "provider_work_session_event_connection_l"`,
		`ALTER INDEX public."provider_work_session_event_pkey" RENAME TO "provider_work_session_event_pkey_l"`,
		`ALTER TABLE public."provider_work_session_event_p" ATTACH PARTITION public."provider_work_session_event_legacy" FOR VALUES FROM (MINVALUE) TO ('2026-11-02 00:00:00')`,
		`ALTER TABLE public."provider_work_session_event_p" RENAME TO "provider_work_session_event"`,
		`ALTER INDEX public."provider_work_session_event_connection_p" RENAME TO "provider_work_session_event_connection"`,
		`ALTER INDEX public."provider_work_session_event_pkey_p" RENAME TO "provider_work_session_event_pkey"`,
		`CREATE TRIGGER original_guard BEFORE DELETE OR UPDATE ON public.provider_work_session_event FOR EACH ROW EXECUTE FUNCTION provider_work_original_guard()`,
		`CREATE TRIGGER original_truncate_guard BEFORE TRUNCATE ON public.provider_work_session_event FOR EACH STATEMENT EXECUTE FUNCTION provider_work_original_guard()`,
	); !slices.Equal(statements, want) {
		t.Fatalf("swap:\n%s", strings.Join(statements, "\n"))
	}

	if abort := cutover.abortSql(); !slices.Equal(abort, append(slices.Clone(bounds),
		`DROP TABLE IF EXISTS public."provider_work_session_event_p"`,
		`ALTER TABLE public."provider_work_session_event" DROP CONSTRAINT IF EXISTS "provider_work_session_event_legacy_bound"`,
	)) {
		t.Fatalf("abort:\n%s", strings.Join(abort, "\n"))
	}
}

// The journal as migrated and as prepared can become the legacy partition; a
// partitioned table, an invalid index, another unique index, an unknown
// primary key or another foreign key cannot. The identity index an
// interrupted build left invalid is rebuilt, so it is no problem.
func TestProviderWorkSessionCutoverProblems(t *testing.T) {
	for _, primaryKey := range []string{providerWorkSessionEventSequenceKey, providerWorkSessionEventIdentityKey} {
		if problems := providerWorkSessionCutoverProblems(testProviderWorkSessionEventShape(primaryKey)); len(problems) != 0 {
			t.Fatalf("%s: %v", primaryKey, problems)
		}
	}
	interrupted := testProviderWorkSessionEventShape(providerWorkSessionEventSequenceKey)
	interrupted.indexes = append(interrupted.indexes, contractIndex{
		name:       providerWorkSessionEventIdentityIndexName,
		definition: "CREATE UNIQUE INDEX provider_work_session_event_identity ON public.provider_work_session_event USING btree (client_id, sequence, observed_at)",
		unique:     true,
	})
	if problems := providerWorkSessionCutoverProblems(interrupted); len(problems) != 0 {
		t.Fatalf("an interrupted identity build: %v", problems)
	}
	for _, test := range []struct {
		name   string
		modify func(shape *contractTableShape)
	}{
		{"partitioned", func(shape *contractTableShape) { shape.relkind = "p" }},
		{"an invalid index", func(shape *contractTableShape) { shape.indexes[0].valid = false }},
		{"another unique index", func(shape *contractTableShape) {
			shape.indexes = append(shape.indexes, contractIndex{name: "provider_work_session_event_other", definition: "CREATE UNIQUE INDEX provider_work_session_event_other ON public.provider_work_session_event USING btree (transaction_id)", valid: true, ready: true, unique: true})
		}},
		{"an unknown primary key", func(shape *contractTableShape) {
			shape.indexes[1].constraintDefinition = "PRIMARY KEY (client_id)"
		}},
		{"a nullable observed_at", func(shape *contractTableShape) { shape.columns[5].notNull = false }},
		{"a foreign key into the journal", func(shape *contractTableShape) {
			shape.referencedBy = append(shape.referencedBy, contractForeignKey{name: "synthetic_fkey", table: "synthetic_child", referencedTable: providerWorkSessionEventTable})
		}},
		{"a foreign key out of the journal", func(shape *contractTableShape) {
			shape.foreignKeys = []contractForeignKey{{name: "synthetic_fkey", table: providerWorkSessionEventTable, referencedTable: "network_client"}}
		}},
		{"a constraint trigger", func(shape *contractTableShape) { shape.triggers[0].constraint = true }},
	} {
		shape := testProviderWorkSessionEventShape(providerWorkSessionEventSequenceKey)
		test.modify(shape)
		if problems := providerWorkSessionCutoverProblems(shape); len(problems) == 0 {
			t.Errorf("%s is no problem", test.name)
		}
	}
}

// Rollback removes every weekly leaf and the parent, restores the table under
// its name, indexes and row guard, and keeps the legacy partition's own
// TRUNCATE guard.
func TestProviderWorkSessionRollbackRestoresNames(t *testing.T) {
	cutover := testProviderWorkSessionCutover()
	if lock := cutover.exclusiveBoundsSql(); len(lock) != 2 {
		t.Fatalf("bounds %v", lock)
	}
	// the statements the rollback would run, from the catalog the swap leaves
	parent := testProviderWorkSessionEventShape(providerWorkSessionEventIdentityKey)
	parent.relkind = "p"
	legacy := testProviderWorkSessionEventShape(providerWorkSessionEventIdentityKey)
	legacy.table = PartitionLegacyLeafName(providerWorkSessionEventTable)
	for i := range legacy.indexes {
		legacy.indexes[i].name += contractLegacyIndexSuffix
	}
	legacy.triggers = legacy.triggers[1:]
	statements, err := providerWorkSessionRollbackSql(parent, legacy, []string{"provider_work_session_event_w20261102"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{
		`DROP TRIGGER "original_guard" ON public."provider_work_session_event"`,
		`DROP TRIGGER "original_truncate_guard" ON public."provider_work_session_event"`,
		`ALTER TABLE public."provider_work_session_event" DETACH PARTITION public."provider_work_session_event_w20261102"`,
		`DROP TABLE public."provider_work_session_event_w20261102"`,
		`ALTER TABLE public."provider_work_session_event" DETACH PARTITION public."provider_work_session_event_legacy"`,
		`DROP TABLE public."provider_work_session_event"`,
		`ALTER TABLE public."provider_work_session_event_legacy" RENAME TO "provider_work_session_event"`,
		`ALTER TABLE public."provider_work_session_event" DROP CONSTRAINT "provider_work_session_event_legacy_bound"`,
		`ALTER INDEX public."provider_work_session_event_connection_l" RENAME TO "provider_work_session_event_connection"`,
		`ALTER INDEX public."provider_work_session_event_pkey_l" RENAME TO "provider_work_session_event_pkey"`,
		`CREATE TRIGGER original_guard BEFORE DELETE OR UPDATE ON public.provider_work_session_event FOR EACH ROW EXECUTE FUNCTION provider_work_original_guard()`,
	}; !slices.Equal(statements, want) {
		t.Fatalf("rollback:\n%s", strings.Join(statements, "\n"))
	}
}

// Defaults are valid; a timeout under a millisecond, a non-positive wait or
// attempt count, and margins that do not nest are refused.
func TestProviderWorkSessionPartitionSettingsValidate(t *testing.T) {
	if err := DefaultProviderWorkSessionPartitionSettings().validate(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		modify func(settings *ProviderWorkSessionPartitionSettings)
	}{
		{"a zero lock timeout", func(settings *ProviderWorkSessionPartitionSettings) { settings.LockTimeout = 0 }},
		{"a sub-millisecond build timeout", func(settings *ProviderWorkSessionPartitionSettings) { settings.BuildLockTimeout = time.Microsecond }},
		{"no attempts", func(settings *ProviderWorkSessionPartitionSettings) { settings.LockAttempts = 0 }},
		{"no validation attempts", func(settings *ProviderWorkSessionPartitionSettings) { settings.ValidateAttempts = 0 }},
		{"a zero retry interval", func(settings *ProviderWorkSessionPartitionSettings) { settings.LockRetryInterval = 0 }},
		{"a lead inside the abandon margin", func(settings *ProviderWorkSessionPartitionSettings) { settings.PrepareLead = settings.AbandonMargin }},
		{"an abandon margin inside the finalize margin", func(settings *ProviderWorkSessionPartitionSettings) { settings.FinalizeMargin = settings.AbandonMargin }},
	} {
		settings := DefaultProviderWorkSessionPartitionSettings()
		test.modify(settings)
		if err := settings.validate(); err == nil {
			t.Errorf("%s validated", test.name)
		}
	}
}

// Two actions at once, or settings that do not validate, are refused before
// any database is read.
func TestMigrateProviderWorkSessionEventsRefusesBadOptions(t *testing.T) {
	ctx := context.Background()
	if err := MigrateProviderWorkSessionEventsToPartitions(ctx, ProviderWorkSessionPartitionMigrateOptions{Finalize: true, Abort: true}); err == nil {
		t.Fatal("finalize and abort together were accepted")
	}
	settings := DefaultProviderWorkSessionPartitionSettings()
	settings.LockAttempts = 0
	if err := MigrateProviderWorkSessionEventsToPartitions(ctx, ProviderWorkSessionPartitionMigrateOptions{Rollback: true, Settings: settings}); err == nil {
		t.Fatal("invalid settings were accepted")
	}
	if err := testProviderWorkSessionCutover().prepare(ctx, time.Time{}); err == nil {
		t.Fatal("prepare without a cutover week was accepted")
	}
	if err := testProviderWorkSessionCutover().prepare(ctx, PartitionGranularityWeekly.RangeStart(NowUtc())); err == nil {
		t.Fatal("prepare for the current week was accepted")
	}
}

// Once the journal has the identity key or is partitioned, the receipt key the
// cutover dropped is not drift, nor are the staged parent, the identity index
// of an unfinished build or the legacy bound; the key missing beside the v776
// primary key still is.
func TestAuditSchemaAcceptsProviderWorkSessionCutover(t *testing.T) {
	receiptKey := auditConstraint{name: providerWorkSessionReceiptForeignKeyName, typ: "f", def: "FOREIGN KEY (client_id, sequence) REFERENCES provider_work_session_event(client_id, sequence)"}
	snapshot := func(primaryKey string, receiptKeys []auditConstraint) *SchemaSnapshot {
		return &SchemaSnapshot{
			tables: map[string]bool{providerWorkSessionEventTable: true, providerWorkSessionReceiptTable: true},
			columns: map[string][]auditColumn{
				providerWorkSessionEventTable:   {{name: "client_id", typ: "uuid", notNull: true}},
				providerWorkSessionReceiptTable: {{name: "client_id", typ: "uuid", notNull: true}},
			},
			indexes: map[string]auditIndex{},
			constraints: map[string][]auditConstraint{
				providerWorkSessionEventTable:   {{name: providerWorkSessionEventPrimaryKeyName, typ: "p", def: primaryKey}},
				providerWorkSessionReceiptTable: receiptKeys,
			},
			partitioned: map[string]bool{},
		}
	}
	expected := snapshot(providerWorkSessionEventSequenceKey, []auditConstraint{receiptKey})

	if d := diffSchemas(expected, snapshot(providerWorkSessionEventSequenceKey, nil)); !d.HasDifferences() {
		t.Fatal("a receipt key missing beside the v776 primary key is not drift")
	}

	prepared := snapshot(providerWorkSessionEventIdentityKey, nil)
	prepared.tables[providerWorkSessionStagedParent()] = true
	prepared.partitioned[providerWorkSessionStagedParent()] = true
	prepared.indexes["provider_work_session_event_connection_p"] = auditIndex{table: providerWorkSessionStagedParent(), def: "CREATE INDEX provider_work_session_event_connection_p ON ONLY public.provider_work_session_event_p USING btree (client_id, connection_id, sequence)"}
	prepared.constraints[providerWorkSessionEventTable] = append(prepared.constraints[providerWorkSessionEventTable], auditConstraint{name: providerWorkSessionLegacyBound(), typ: "c", def: "CHECK ((observed_at < '2026-11-02 00:00:00'::timestamp without time zone))"})
	if d := diffSchemas(expected, prepared); d.HasDifferences() {
		t.Fatalf("a prepared cutover drifts:\n%s", d.Report())
	}

	building := snapshot(providerWorkSessionEventSequenceKey, []auditConstraint{receiptKey})
	building.indexes[providerWorkSessionEventIdentityIndexName] = auditIndex{table: providerWorkSessionEventTable, def: "CREATE UNIQUE INDEX provider_work_session_event_identity ON public.provider_work_session_event USING btree (client_id, sequence, observed_at)"}
	if d := diffSchemas(expected, building); d.HasDifferences() {
		t.Fatalf("an identity build drifts:\n%s", d.Report())
	}

	partitioned := snapshot(providerWorkSessionEventIdentityKey, nil)
	partitioned.partitioned[providerWorkSessionEventTable] = true
	if d := diffSchemas(expected, partitioned); d.HasDifferences() {
		t.Fatalf("the partitioned journal drifts:\n%s", d.Report())
	}

	// another table's stray index of the same name is still drift
	stray := snapshot(providerWorkSessionEventSequenceKey, []auditConstraint{receiptKey})
	stray.tables["synthetic_other"] = true
	stray.indexes[providerWorkSessionEventIdentityIndexName] = auditIndex{table: "synthetic_other", def: "CREATE INDEX provider_work_session_event_identity ON public.synthetic_other USING btree (id)"}
	expected.tables["synthetic_other"] = true
	if d := diffSchemas(expected, stray); !d.HasDifferences() {
		t.Fatal("a stray index named like the identity index is not drift")
	}
}

// The monitor's version for the partition preparation is that migration's own
// position, after the close evidence retention it was appended behind.
func TestProviderWorkSessionPartitionMigrationVersionIsItsPosition(t *testing.T) {
	version := ProviderWorkSessionPartitionMigrationVersion()
	migration, ok := migrations[version-1].(*SqlMigration)
	if !ok || migration.sql != providerWorkSessionPartitionSchemaSql {
		t.Fatalf("version %d is not the partition preparation", version)
	}
	for index, other := range migrations {
		if sql, ok := other.(*SqlMigration); ok && sql.sql == contractCloseReportEvidenceRetentionGuardSql && version <= index+1 {
			t.Fatalf("the partition preparation (version %d) is not after the close evidence retention (version %d)", version, index+1)
		}
	}
	if MigrationCount() < version {
		t.Fatalf("version %d is past the head %d", version, MigrationCount())
	}
}
