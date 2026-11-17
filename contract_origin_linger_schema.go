// The lingering origins' side table (OPEN-SOURCE-ID-RETIRE.md §2; owner
// 2026-11-16, "accept recommendations"). A closed origin keeps anchoring
// companions until its deadline (model.CompanionOriginCloseLinger), and the
// reads of those closed origins were the last that needed
// transfer_contract_open_source_id (323 GiB on Main). A trigger on
// transfer_contract writes a row here on every close before the deadline, so
// the reads take the pair's lingering contract ids from this small table and
// then read each contract by its primary key, keeping every original
// condition: the table is only an access path.
//
// The trigger. AFTER INSERT OR UPDATE OF outcome, dispute, close_time and
// expiration_time, with a WHEN clause, so no other update enters plpgsql. It
// upserts a row for a closed contract (a dispute or an outcome) with a close
// time whose deadline is still ahead of its close time or of the database
// clock. Only such a row can satisfy a closed origin arm, which needs an
// unexpired deadline at a time after the close: the database clock bound
// keeps a close stamped by an application clock that runs ahead of the
// database, and the close time bound keeps every close the original arm could
// read. It covers every path that closes a contract, and any added later. A
// row that reopens keeps its side row until the sweep, and the reads' own open
// condition leaves it to the open arm.
//
// The prune time is the contract's deadline at the last capture, the reads'
// own unexpired bound; the taskworker sweep deletes rows past it
// (model.RemoveExpiredContractOriginLinger), so the table holds about an hour
// of early closes, about 3-4M rows on Main.
//
// The state row. The reads use the side table only from its complete time:
// once every lingering origin closed after the trigger existed. The operator
// creates the trigger on Main with the state row in one transaction, its
// complete time the longest linger plus the contract id margin later
// (ContractOriginLingerOperatorSql); until then, and while the row is absent,
// the reads keep their previous statements.
//
// The migration. The two tables, their indexes and the function are
// additive: nothing existing is locked, scanned or rewritten, and every one is
// created only when absent. CREATE TRIGGER takes SHARE ROW EXCLUSIVE on
// transfer_contract, which waits for any vacuum and queues every writer behind
// it, so on a database with contracts the migration leaves the trigger to the
// operator, who creates it by hand in a vacuum gap with a short lock timeout,
// after db migrate (the tables and the function belong to the migration's
// role). On a new database, whose transfer_contract holds no contract, the
// migration creates the trigger and the state row itself, complete at once.
package server

// The function each capture runs, as the migration publishes it.
const contractOriginLingerCaptureBodySql = `
BEGIN
	INSERT INTO contract_origin_linger (source_id, destination_id, contract_id, prune_time)
	VALUES (
		NEW.source_id,
		NEW.destination_id,
		NEW.contract_id,
		COALESCE(NEW.expiration_time, NEW.create_time + interval '60 minutes')
	)
	ON CONFLICT (source_id, destination_id, contract_id) DO UPDATE
	SET prune_time = EXCLUDED.prune_time
	WHERE contract_origin_linger.prune_time IS DISTINCT FROM EXCLUDED.prune_time;
	RETURN NULL;
END
`

// The capture's published body, for the catalog contracts.
func ContractOriginLingerCaptureBodySql() string {
	return contractOriginLingerCaptureBodySql
}

// The trigger, as the migration and the operator create it. The WHEN clause
// mirrors the closed arm: closed, with a close time, and a deadline ahead of
// the close or of the database clock.
const ContractOriginLingerTriggerSql = `CREATE TRIGGER transfer_contract_origin_linger
	AFTER INSERT OR UPDATE OF outcome, dispute, close_time, expiration_time ON transfer_contract
	FOR EACH ROW
	WHEN (
		(NEW.dispute OR NEW.outcome IS NOT NULL) AND
		NEW.close_time IS NOT NULL AND
		COALESCE(NEW.expiration_time, NEW.create_time + interval '60 minutes') >
			LEAST(NEW.close_time, clock_timestamp() AT TIME ZONE 'UTC')
	)
	EXECUTE FUNCTION contract_origin_linger_capture()`

// The trigger's name on transfer_contract.
const ContractOriginLingerTriggerName = "transfer_contract_origin_linger"

// How long after the operator creates the trigger the side table holds every
// lingering origin: an origin that closed before has a deadline at most the
// contract lifetime (60 minutes) after its creation, and so after its close,
// and the contract id margin (15 minutes) covers the clocks that minted it.
// model.CompanionOriginCloseLinger plus model.ContractIdCreateTimeMargin.
const contractOriginLingerOperatorWarmup = `interval '75 minutes'`

// The operator's statements on Main, in one transaction after db migrate: the
// trigger, and the state row whose complete time starts the reads on the side
// table once the warmup has passed. The session sets its own lock bounds.
const ContractOriginLingerOperatorSql = ContractOriginLingerTriggerSql + `;
	INSERT INTO contract_origin_linger_state (singleton, complete_time)
	VALUES (true, clock_timestamp() AT TIME ZONE 'UTC' + ` + contractOriginLingerOperatorWarmup + `)
	ON CONFLICT (singleton) DO NOTHING`

// The migration's statements.
const contractOriginLingerSchemaSql = `
	SET LOCAL lock_timeout = '5s';
	CREATE TABLE IF NOT EXISTS contract_origin_linger (
		source_id uuid NOT NULL,
		destination_id uuid NOT NULL,
		contract_id uuid NOT NULL,
		prune_time timestamp NOT NULL,
		PRIMARY KEY (source_id, destination_id, contract_id)
	) WITH (
		vacuum_truncate = false,
		autovacuum_vacuum_scale_factor = 0.01,
		autovacuum_vacuum_cost_delay = 0,
		autovacuum_analyze_scale_factor = 0.02
	);
	CREATE INDEX IF NOT EXISTS contract_origin_linger_prune_time ON contract_origin_linger (prune_time);
	CREATE TABLE IF NOT EXISTS contract_origin_linger_state (
		singleton boolean NOT NULL DEFAULT true CHECK (singleton),
		complete_time timestamp NOT NULL,
		PRIMARY KEY (singleton)
	);
	CREATE OR REPLACE FUNCTION contract_origin_linger_capture()
	RETURNS trigger LANGUAGE plpgsql AS $contract_origin_linger_capture$` + contractOriginLingerCaptureBodySql + `$contract_origin_linger_capture$;
	DO $contract_origin_linger_trigger$
	BEGIN
		IF NOT EXISTS (
			SELECT 1 FROM pg_trigger
			WHERE
				tgrelid = 'public.transfer_contract'::regclass AND
				tgname = 'transfer_contract_origin_linger' AND
				NOT tgisinternal
		) THEN
			-- the lowest key, a probe of the primary key, never a heap scan
			PERFORM 1 FROM transfer_contract ORDER BY contract_id LIMIT 1;
			IF FOUND THEN
				RAISE NOTICE 'transfer_contract holds contracts: the operator creates transfer_contract_origin_linger (server.ContractOriginLingerOperatorSql)';
			ELSE
				` + ContractOriginLingerTriggerSql + `;
				INSERT INTO contract_origin_linger_state (singleton, complete_time)
				VALUES (true, clock_timestamp() AT TIME ZONE 'UTC')
				ON CONFLICT (singleton) DO NOTHING;
			END IF;
		END IF;
	END
	$contract_origin_linger_trigger$;
`

var contractOriginLingerMigration = newSqlMigration(contractOriginLingerSchemaSql)

// The migration's version: its position in the list, from 1.
func ContractOriginLingerMigrationVersion() int {
	return migrationVersionOf(contractOriginLingerSchemaSql)
}
