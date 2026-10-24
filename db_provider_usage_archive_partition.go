// The usage archive partitioned by contract week (PARTITION-MAINTENANCE-PLAN.md
// C6, decided 2026-10-24): RANGE (contract_id) partitions on the contract
// tables' weekly bounds (Monday 00:00 UTC, through contract_id_floor), each
// dropped once its week ended a year ago.
//
// Billing and contract retention archive a contract's proof when they delete
// the contract, so writes land in weeks long past. The weeks exist from the
// oldest contract onward, and the partition registry keeps the current week
// and the four after it, as the contract tables do. A DEFAULT partition takes
// an id that no week covers (an id outside every week, or of a week already
// dropped), so a capture never fails for want of a partition; it is never
// dropped.
//
// The archive's guard admits only an exact copy of a live terminal contract,
// so an archived row whose contract is gone can never be inserted again. The
// cutover therefore copies into a staged table that gets its guard only after
// the copy is checked, inside one transaction no other session can see into.
//
// Dropping a week bypasses the row and TRUNCATE guards, so only the registry's
// guarded drop drops one: its transaction sets the retention opt-in, and the
// family's fence refuses the drop without it, or before the week's upper bound
// is a year past by the database's clock. PostgreSQL refuses a plain DROP
// TABLE only through an event trigger, which only a superuser can create, and
// the application role is not one; the archive's DDL stays the migrations' and
// the registry's alone, as it was before the cutover.
package server

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// A week's archived proof is dropped once the week ended this long ago (owner
// decision 2026-10-24: one year).
const providerUsageArchiveRetention = 52 * 7 * 24 * time.Hour

// The opt-in the guarded drop sets (SET LOCAL <setting> = 'on'). The family's
// fence refuses a drop transaction without it.
const providerUsageArchiveRetentionSetting = "urnetwork.usage_archive_retention"

// The cutover to weekly partitions, in one transaction, so an interruption at
// any point leaves the plain table as it was and a rerun starts over:
//
//  1. Read committed, so the copy after the fence sees every row committed
//     before it. Had any statement of this transaction already read, the SET
//     would fail and stop the migration rather than copy from an older
//     snapshot.
//  2. The write fence: EXCLUSIVE stops archive inserts, so the capture of a
//     contract deleted meanwhile waits until the commit; reads continue.
//  3. A staged parent with the published columns, checks, keys and the plain
//     table's grants, and a partition per week from the oldest live or
//     archived contract's week (2026-06-01 on main; never earlier, and never
//     later than the current week) through four weeks after the current one,
//     then the DEFAULT partition, which takes ids outside every week.
//  4. The copy, before the staged parent has its guard. The guard admits only
//     an exact copy of a live contract, so it would refuse every row whose
//     contract is gone; no other session can see the staged parent before the
//     commit, so the bypass covers this copy alone.
//  5. The exact check, week by week: the row count, the rows without proof,
//     the usage bytes and a hash of every row's proof fields must match the
//     source.
//  6. The guards: the row guard on the parent, cloned onto every partition,
//     and the TRUNCATE guard on the parent and repeated on every partition (a
//     statement trigger on the parent does not fire for a statement that names
//     a partition).
//  7. The swap under ACCESS EXCLUSIVE (readers wait from here to the commit):
//     the plain table is dropped and the staged parent and its indexes take
//     the published names. A capture that waited at the fence resolves the
//     name again and inserts into the partitioned table.
//
// Each lock wait is bounded by lock_timeout; a busy archive fails the
// migration, and the next migration run retries it. The copy of the archive
// on main (1.5 GB) can outlast transaction_timeout, so this transaction is
// exempt. A repeatable read transaction whose snapshot predates the commit and
// that first reads the archive after it would see the copied rows as not yet
// committed; the usage readers are single statements, so none does.
const providerUsageArchivePartitionSql = `
SET TRANSACTION ISOLATION LEVEL READ COMMITTED;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = 0;
SET LOCAL transaction_timeout = 0;

LOCK TABLE st_provider_usage_archive IN EXCLUSIVE MODE;

CREATE TABLE st_provider_usage_archive_partitioned (
	contract_id uuid CONSTRAINT st_provider_usage_archive_contract_id_not_null NOT NULL,
	outcome varchar CONSTRAINT st_provider_usage_archive_outcome_not_null NOT NULL,
	close_time timestamp NULL,
	provider_usage jsonb NULL,
	CONSTRAINT st_provider_usage_archive_outcome_check
		CHECK (outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')),
	CONSTRAINT st_provider_usage_archive_partitioned_pkey PRIMARY KEY (contract_id)
) PARTITION BY RANGE (contract_id);
CREATE INDEX st_provider_usage_archive_partitioned_close_time
ON st_provider_usage_archive_partitioned (close_time, contract_id);
DO $archive_grants$
DECLARE
	granted record;
BEGIN
	FOR granted IN
		SELECT acl.privilege_type, acl.grantee, acl.is_grantable
		FROM pg_class AS relation, aclexplode(relation.relacl) AS acl
		WHERE relation.oid = 'public.st_provider_usage_archive'::regclass AND acl.grantee <> relation.relowner
	LOOP
		EXECUTE format(
			'GRANT %s ON public.st_provider_usage_archive_partitioned TO %s%s',
			granted.privilege_type,
			CASE WHEN granted.grantee = 0 THEN 'PUBLIC' ELSE quote_ident(pg_get_userbyid(granted.grantee)) END,
			CASE WHEN granted.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END
		);
	END LOOP;
END
$archive_grants$;

DO $archive_weeks$
DECLARE
	current_week timestamp := date_trunc('week', now() AT TIME ZONE 'UTC');
	first_week timestamp;
	partition_week timestamp;
BEGIN
	SELECT coalesce(min(calendar.week_start), current_week)
	INTO first_week
	FROM generate_series(timestamp '2026-06-01', current_week, interval '1 week') AS calendar(week_start)
	WHERE (
		SELECT least(
			(SELECT contract_id FROM st_provider_usage_archive ORDER BY contract_id LIMIT 1),
			(SELECT contract_id FROM transfer_contract ORDER BY contract_id LIMIT 1)
		)
	) < contract_id_floor((calendar.week_start + interval '1 week') AT TIME ZONE 'UTC');
	FOR partition_week IN
		SELECT calendar.week_start
		FROM generate_series(first_week, current_week + interval '4 weeks', interval '1 week') AS calendar(week_start)
	LOOP
		EXECUTE format(
			'CREATE TABLE public.%I PARTITION OF st_provider_usage_archive_partitioned FOR VALUES FROM (%L) TO (%L)',
			'st_provider_usage_archive_w' || to_char(partition_week, 'YYYYMMDD'),
			contract_id_floor(partition_week AT TIME ZONE 'UTC'),
			contract_id_floor((partition_week + interval '1 week') AT TIME ZONE 'UTC')
		);
	END LOOP;
END
$archive_weeks$;
CREATE TABLE st_provider_usage_archive_default PARTITION OF st_provider_usage_archive_partitioned DEFAULT;

INSERT INTO st_provider_usage_archive_partitioned (contract_id, outcome, close_time, provider_usage)
SELECT contract_id, outcome, close_time, provider_usage
FROM st_provider_usage_archive;

DO $archive_copy_check$
BEGIN
	IF EXISTS (
		SELECT 1
		FROM (
			SELECT 1 AS side, contract_id, outcome, close_time, provider_usage
			FROM st_provider_usage_archive
			UNION ALL
			SELECT -1 AS side, contract_id, outcome, close_time, provider_usage
			FROM st_provider_usage_archive_partitioned
		) AS archived
		GROUP BY date_trunc(
			'week',
			timestamp 'epoch' + ('x' || left(replace(archived.contract_id::text, '-', ''), 12))::bit(48)::bigint * interval '1 millisecond'
		)
		HAVING sum(archived.side) <> 0
			OR coalesce(sum(archived.side) FILTER (WHERE archived.provider_usage IS NULL), 0) <> 0
			OR coalesce(sum(archived.side * (archived.provider_usage->>'byte_count')::numeric)
				FILTER (WHERE jsonb_typeof(archived.provider_usage->'byte_count') = 'number'), 0) <> 0
			OR sum(archived.side * hashtextextended(
				ROW(archived.contract_id, archived.outcome, archived.close_time, archived.provider_usage)::text,
				0
			)::numeric) <> 0
	) THEN
		RAISE EXCEPTION 'st_provider_usage_archive copy differs from its source';
	END IF;
END
$archive_copy_check$;

CREATE TRIGGER st_provider_usage_archive_guard
BEFORE INSERT OR UPDATE OR DELETE ON st_provider_usage_archive_partitioned
FOR EACH ROW EXECUTE FUNCTION st_provider_usage_archive_guard();
CREATE TRIGGER st_provider_usage_archive_truncate_guard
BEFORE TRUNCATE ON st_provider_usage_archive_partitioned
FOR EACH STATEMENT EXECUTE FUNCTION st_provider_usage_archive_guard();
DO $archive_leaf_guards$
DECLARE
	leaf regclass;
BEGIN
	FOR leaf IN
		SELECT inheritance.inhrelid::regclass
		FROM pg_inherits AS inheritance
		WHERE inheritance.inhparent = 'public.st_provider_usage_archive_partitioned'::regclass
	LOOP
		EXECUTE format(
			'CREATE TRIGGER st_provider_usage_archive_truncate_guard BEFORE TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION st_provider_usage_archive_guard()',
			leaf
		);
	END LOOP;
END
$archive_leaf_guards$;

LOCK TABLE st_provider_usage_archive IN ACCESS EXCLUSIVE MODE;
DROP TABLE st_provider_usage_archive;
ALTER TABLE st_provider_usage_archive_partitioned RENAME TO st_provider_usage_archive;
ALTER INDEX st_provider_usage_archive_partitioned_pkey RENAME TO st_provider_usage_archive_pkey;
ALTER INDEX st_provider_usage_archive_partitioned_close_time RENAME TO st_provider_usage_archive_close_time;
`

var providerUsageArchivePartitionMigration = newSqlMigration(providerUsageArchivePartitionSql)

// The version a database reaches once migration has applied: its position in
// the migration list plus one. The monitor's positional catalog and the tests
// read versions this way, so nothing names a migration by its index.
func migrationVersion(migration any) int {
	for i, m := range migrations {
		if m == migration {
			return i + 1
		}
	}
	panic(fmt.Errorf("%T is not in the migration list", migration))
}

// The version that partitions the archive by contract week.
func ProviderUsageArchivePartitionMigrationVersion() int {
	return migrationVersion(providerUsageArchivePartitionMigration)
}

// Refuses a drop that bypasses the retention policy: a transaction without the
// retention opt-in, a range that is not one canonical week, or a week whose
// upper bound is not yet a year past by the database's clock, whatever clock
// the caller judged expiry by. It runs first in the drop transaction, before
// any detach.
func providerUsageArchiveDropFence(ctx context.Context, tx PgTx, r PartitionRange) {
	week := PartitionGranularityWeekly.RangeOf(r.Lower)
	if r.Default || r.Legacy || r.Lower.IsZero() || !week.Lower.Equal(r.Lower) || !week.Upper.Equal(r.Upper) {
		panic(fmt.Errorf("st_provider_usage_archive drops whole weeks only, not %+v", r))
	}
	var optedIn bool
	var due bool
	Raise(tx.QueryRow(
		ctx,
		`SELECT coalesce(current_setting($1, true) = 'on', false), $2::timestamp <= now() AT TIME ZONE 'UTC'`,
		providerUsageArchiveRetentionSetting,
		r.Upper.Add(providerUsageArchiveRetention),
	).Scan(&optedIn, &due))
	if !optedIn {
		panic(errors.New("archived provider usage is append-only: a week drops only under the retention opt-in"))
	}
	if !due {
		panic(fmt.Errorf("archived provider usage is append-only: the week of %s is kept until %s", r.Lower.Format(time.DateOnly), r.Upper.Add(providerUsageArchiveRetention).Format(time.RFC3339)))
	}
}

// The archive in the partition registry, from its cutover on. Weeks run four
// ahead, as the contract weeks do, so a contract's archive week exists long
// before retention deletes the contract. A week is dropped under the retention
// opt-in once it ended 52 weeks ago; nothing else keeps it, since a contract
// archived after its week was dropped lands in the DEFAULT partition. The
// leaves are dropped whole, so they stay out of the REINDEX rotation, and
// autovacuum analyzes them as their weeks fill.
var providerUsageArchivePartitionFamily = RegisterPartitionFamily(&PartitionFamily{
	Name:             "st_provider_usage_archive",
	Description:      "archived subnet usage proof of deleted contracts, weekly by contract week, each week kept 52 weeks after it ends",
	Tables:           []string{"st_provider_usage_archive"},
	KeyColumn:        "contract_id",
	KeyKind:          PartitionKeyId,
	Granularity:      PartitionGranularityWeekly,
	AheadRanges:      ContractPartitionAheadWeeks,
	Retention:        providerUsageArchiveRetention,
	DefaultPartition: true,
	DropGuardSetting: providerUsageArchiveRetentionSetting,
	ReindexExempt:    true,
	DropFence:        providerUsageArchiveDropFence,
})
