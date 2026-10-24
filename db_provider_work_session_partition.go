// The provider work session journal in weekly observed_at partitions
// (PARTITION-MAINTENANCE-PLAN.md C4). provider_work_session_event is cut over
// by an owner-run command (MigrateProviderWorkSessionEventsToPartitions); this
// file holds what works on both its plain and its partitioned shape, and the
// family the partition registry maintains once it is partitioned.
//
// # Sequence identity without the old primary key
//
// The partitioned primary key is (client_id, sequence, observed_at), which
// alone would admit one sequence twice. It does not need to, because the
// journal's only writer is provider_work_session_append, and its generator is
// monotonic per client:
//   - every append advances the client's provider_work_session_head row by
//     one (INSERT ... ON CONFLICT DO UPDATE SET sequence = sequence + 1) and
//     takes the event's sequence from it, under that row's lock and the
//     client's endpoint fence;
//   - the append starts a client's head at 1, and only when the client has
//     none (the head's primary key);
//   - provider_work_session_head_guard refuses every update but +1 and every
//     delete, and its truncate guard refuses truncate, so a head never
//     repeats a value;
//   - the cutover copies nothing: the old table attaches as the legacy
//     partition with its rows as they are.
//
// So a client's sequences are 1, 2, 3, ... exactly once each, across every
// partition, and no separate identity relation is kept.
//
// # The receipt's reference
//
// provider_work_session_receipt referenced its event through a foreign key on
// (client_id, sequence). A key into the partitioned table would have to carry
// observed_at, and it would make every partition detach run a referencing-row
// check against the whole receipt table while holding the parent's ACCESS
// EXCLUSIVE lock, which every connection open and close waits behind. It would
// also refuse to drop a week while any receipt referenced it, though a receipt
// is the signed, self-contained original of its event, and the endpoint chain
// replay (providerWorkEndpointHeadInTx) and the payout originals
// (ListProviderWorkOriginals) read receipts only, from sequence 1. So the
// reference is enforced by provider_work_session_receipt_reference when the
// receipt is inserted, the only time one can be (receipts and events are both
// append-only), and the cutover drops the foreign key. A week's events are
// dropped after 13 weeks; their receipts stay.
//
// # The drop
//
// A partition drop bypasses the append-only guards, since neither DETACH nor
// DROP fires a row or statement trigger (and event triggers need a superuser).
// So the family's drop fence asks provider_work_session_event_drop_guard,
// which refuses unless the transaction opted in with
// urnetwork.provider_work_session_retention, which only the registry's
// guarded drop sets (DropGuardSetting), and unless the leaf's upper bound is 13
// weeks past by the database's clock. The legacy partition's upper bound is the
// cutover week, so it becomes old enough 13 weeks after it. The registry also
// retains any leaf whose admissions are still needed by open connections.
package server

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/glog"
)

const providerWorkSessionEventTable = "provider_work_session_event"

// Events are kept at least 13 weeks after their week ends, as the contracts they
// describe are (ContractRetention; decided 2026-10-24).
const ProviderWorkSessionEventRetention = 13 * 7 * 24 * time.Hour

// The setting the registry's drop transaction sets on before the drop fence.
const providerWorkSessionRetentionSetting = "urnetwork.provider_work_session_retention"

// The retire lookup's floor. A connection's admits are appended once its row
// exists, after server.NewId() minted its id, whose first 48 bits are the mint
// millisecond; a day below that absorbs clock skew between the minting server
// and the database. So the bounded lookup sees every admit of the connection,
// and a lookup that finds none (its week dropped, an id from no clock) falls
// back to every partition.
const providerWorkSessionAdmitFloorSql = `
CREATE OR REPLACE FUNCTION provider_work_session_admit_floor(connection uuid) RETURNS timestamp
 LANGUAGE sql IMMUTABLE STRICT AS $admit_floor$
 SELECT (to_timestamp(('x'||lpad(substr(replace(connection::text,'-',''),1,12),16,'0'))::bit(64)::bigint/1000.0) AT TIME ZONE 'UTC')-interval '1 day'
$admit_floor$;
`

// The v776 append with the retire lookup bounded by its connection's floor, so
// it prunes to the weeks since the connection was minted.
const providerWorkSessionPartitionAppendSql = `
CREATE OR REPLACE FUNCTION provider_work_session_append(client uuid,connection uuid,event_kind text,extender uuid)
 RETURNS bigint LANGUAGE plpgsql AS $session_append$
DECLARE next_sequence bigint; owner_network uuid;
BEGIN
 PERFORM provider_work_endpoint_lock(client);
 INSERT INTO provider_work_session_head(client_id,sequence) VALUES(client,1)
 ON CONFLICT(client_id) DO UPDATE SET sequence=provider_work_session_head.sequence+1
 RETURNING sequence INTO next_sequence;
 IF event_kind='retire' THEN
  SELECT network_id INTO owner_network FROM provider_work_session_event
   WHERE client_id=client AND connection_id=connection AND kind='admit'
   AND observed_at>=provider_work_session_admit_floor(connection) ORDER BY sequence DESC LIMIT 1;
  IF NOT FOUND THEN
   SELECT network_id INTO owner_network FROM provider_work_session_event
    WHERE client_id=client AND connection_id=connection AND kind='admit' ORDER BY sequence DESC LIMIT 1;
  END IF;
 ELSE
  SELECT network_id INTO owner_network FROM network_client WHERE client_id=client;
 END IF;
 INSERT INTO provider_work_session_event(client_id,sequence,network_id,connection_id,kind,observed_at,extender_id,transaction_id)
 VALUES(client,next_sequence,owner_network,connection,event_kind,clock_timestamp() AT TIME ZONE 'UTC',extender,txid_current());
 RETURN next_sequence;
END;
$session_append$;
`

// The receipt's reference to its event (see the file comment). A receipt is
// signed in its event's transaction (providerWorkSignSessionEventsInTx), so
// its event was appended at or after the transaction began: the first lookup
// prunes to the last day's weeks, and only a receipt for an older event reads
// every partition. 23503 is the foreign key's own error.
const providerWorkSessionReceiptReferenceSql = `
CREATE OR REPLACE FUNCTION provider_work_session_receipt_reference() RETURNS trigger LANGUAGE plpgsql AS $receipt_reference$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM provider_work_session_event WHERE client_id=NEW.client_id AND sequence=NEW.sequence
  AND observed_at>=(now() AT TIME ZONE 'UTC')-interval '1 day') THEN
  IF NOT EXISTS(SELECT 1 FROM provider_work_session_event WHERE client_id=NEW.client_id AND sequence=NEW.sequence) THEN
   RAISE EXCEPTION 'provider work session receipt has no event' USING ERRCODE='23503';
  END IF;
 END IF;
 RETURN NEW;
END
$receipt_reference$;
CREATE OR REPLACE TRIGGER provider_work_session_receipt_reference BEFORE INSERT ON provider_work_session_receipt
 FOR EACH ROW EXECUTE FUNCTION provider_work_session_receipt_reference();
`

// Refuses the drop of a provider_work_session_event leaf unless the
// transaction opted in and the leaf's upper bound, read back from the catalog
// in this session, is 13 weeks (91 days) past. A DEFAULT, MAXVALUE or foreign
// relation has no upper bound here and is refused.
const providerWorkSessionEventDropGuardSql = `
CREATE OR REPLACE FUNCTION provider_work_session_event_drop_guard(leaf text) RETURNS void LANGUAGE plpgsql AS $event_drop_guard$
DECLARE upper_bound timestamp;
BEGIN
 IF current_setting('urnetwork.provider_work_session_retention',true) IS DISTINCT FROM 'on' THEN
  RAISE EXCEPTION 'provider work session events are dropped only by their retention';
 END IF;
 SELECT (regexp_match(pg_get_expr(c.relpartbound,c.oid),'TO \(''([^'']*)''\)$'))[1]::timestamp INTO upper_bound
 FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
 WHERE i.inhparent=to_regclass('public.provider_work_session_event') AND c.oid=to_regclass('public.'||quote_ident(leaf));
 IF upper_bound IS NULL OR (now() AT TIME ZONE 'UTC')-interval '91 days'<upper_bound THEN
  RAISE EXCEPTION 'provider work session events are kept 13 weeks';
 END IF;
END
$event_drop_guard$;
`

// The partition preparation of the session journal, correct on the plain and
// the partitioned table alike. It replaces functions and adds a trigger to the
// receipt table only, so it takes no lock on the event table; the receipt
// foreign key stays until the cutover drops it.
const providerWorkSessionPartitionSchemaSql = `
SET LOCAL lock_timeout = '5s';
` + providerWorkSessionAdmitFloorSql + providerWorkSessionPartitionAppendSql + providerWorkSessionReceiptReferenceSql + providerWorkSessionEventDropGuardSql

// The version that publishes providerWorkSessionPartitionSchemaSql: its
// one-based position in the migration list, which the migration signal's
// contract for it (and the 824 contract it supersedes) take, so the number
// follows the migration wherever it lands.
func ProviderWorkSessionPartitionMigrationVersion() int {
	for index, migration := range migrations {
		if sql, ok := migration.(*SqlMigration); ok && sql.sql == providerWorkSessionPartitionSchemaSql {
			return index + 1
		}
	}
	panic("the provider work session partition migration is missing from this binary")
}

// The registry's family for the partitioned journal. Before the cutover only
// its BeforeCutover hook runs, abandoning a prepared cutover that could no
// longer be finalized in time.
var providerWorkSessionEventPartitionFamily = RegisterPartitionFamily(&PartitionFamily{
	Name:        "provider_work_session_event",
	Description: "provider work session journal: weekly observed_at partitions dropped 13 weeks after their week (PARTITION-MAINTENANCE-PLAN.md C4)",
	Tables:      []string{providerWorkSessionEventTable},
	KeyColumn:   "observed_at",
	KeyKind:     PartitionKeyTimestamp,
	Granularity: PartitionGranularityWeekly,
	// events take the database's clock, so the runway only has to stay ahead
	// of now; the monitor pages below two weeks
	AheadRanges:      4,
	Retention:        ProviderWorkSessionEventRetention,
	DropGuardSetting: providerWorkSessionRetentionSetting,
	// late events never land in a closed week, so one analyze settles it
	LeafAnalyzeDelay: 24 * time.Hour,
	ReindexExempt:    true,
	DropFence:        providerWorkSessionEventDropFence,
	DropPrecondition: providerWorkSessionEventDropBlocker,
	BeforeCutover:    abandonStaleProviderWorkSessionCutover,
})

// The registry's name for the journal's leaf of r (PartitionFamily.LeafName),
// computed without the family so its hooks can name leaves.
func providerWorkSessionEventLeaf(r PartitionRange) string {
	if r.Legacy {
		return PartitionLegacyLeafName(providerWorkSessionEventTable)
	}
	return fmt.Sprintf("%s_%s%s", providerWorkSessionEventTable, PartitionGranularityWeekly.namePrefix(), r.Lower.UTC().Format("20060102"))
}

// The policy check in the drop transaction, after the registry set the opt-in:
// the database refuses a leaf whose upper bound is not 13 weeks past.
func providerWorkSessionEventDropFence(ctx context.Context, tx PgTx, r PartitionRange) {
	RaisePgResult(tx.Exec(ctx, `SELECT provider_work_session_event_drop_guard($1)`, providerWorkSessionEventLeaf(r)))
}

// An open connection still needs its admit's network identity when it retires.
// Read the named leaf: the registry repeats this check after detaching it under
// the event parent's write fence. A concurrent retirement either committed its
// event already or still appears connected here, so neither order loses its
// admit. This also covers the legacy leaf and clients removed from the directory.
func providerWorkSessionEventDropBlocker(ctx context.Context, conn PgQuerier, r PartitionRange) string {
	var needed bool
	Raise(conn.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (
		SELECT 1 FROM network_client_connection connection
		WHERE connection.connected AND EXISTS (
			SELECT 1 FROM public.%s event
			WHERE event.client_id = connection.client_id
			AND event.connection_id = connection.connection_id AND event.kind = 'admit'
			AND NOT EXISTS (
				SELECT 1 FROM provider_work_session_event newer
				WHERE newer.client_id = event.client_id AND newer.connection_id = event.connection_id
				AND newer.kind = 'admit' AND newer.sequence > event.sequence
			)
		)
	)`, quoteIdentifier(providerWorkSessionEventLeaf(r)))).Scan(&needed))
	if needed {
		return "an open connection still needs its admission event"
	}
	return ""
}

// Abandons a prepared cutover once its cutover week is less than the abandon
// margin away: the prepared check refuses every event from that week on, and
// every connection open and close appends one.
func abandonStaleProviderWorkSessionCutover(ctx context.Context, now time.Time) error {
	settings := DefaultProviderWorkSessionPartitionSettings()
	cutoverWeek, prepared, err := preparedProviderWorkSessionCutoverWeek(ctx)
	if err != nil {
		return err
	}
	if !prepared || now.Before(cutoverWeek.Add(-settings.AbandonMargin)) {
		return nil
	}
	glog.Errorf(
		"[pws]abandoning the provider work session partition cutover prepared for %s: it was not finalized by %s, and its legacy bound would refuse every event from that week\n",
		cutoverWeek.Format(time.DateOnly),
		cutoverWeek.Add(-settings.AbandonMargin).Format(time.RFC3339),
	)
	// one attempt per pass; the runway pass retries every few minutes
	settings.LockAttempts = 1
	cutover := newProviderWorkSessionCutover(settings, false, func(format string, args ...any) {
		glog.Infof("[pws]"+format+"\n", args...)
	})
	return cutover.abort(ctx)
}

// Whether the journal is still the plain table and its legacy bound check is
// in place, and the cutover week that check names, read back in this session.
func preparedProviderWorkSessionCutoverWeek(ctx context.Context) (cutoverWeek time.Time, prepared bool, err error) {
	err = partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			result, queryErr := conn.Query(
				ctx,
				`
				SELECT (regexp_match(pg_get_constraintdef(constraint_record.oid), '''([^'']*)''::timestamp'))[1]::timestamp
				FROM pg_constraint constraint_record
				JOIN pg_class relation ON relation.oid = constraint_record.conrelid
				WHERE constraint_record.conrelid = to_regclass($1)
					AND constraint_record.conname = $2
					AND relation.relkind = 'r'
				`,
				"public."+providerWorkSessionEventTable,
				contractLegacyBoundName(providerWorkSessionEventTable),
			)
			WithPgResult(result, queryErr, func() {
				if result.Next() {
					var bound *time.Time
					Raise(result.Scan(&bound))
					if bound == nil {
						panic(fmt.Errorf("unexpected %s", contractLegacyBoundName(providerWorkSessionEventTable)))
					}
					cutoverWeek = bound.UTC()
					prepared = true
				}
			})
		})
	})
	return
}
