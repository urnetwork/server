// Reservation revisions commit with PostgreSQL changes. Cache publishers read
// the revision and reservation in one snapshot, so older posts cannot reverse
// newer reconciliation. Revision tombstones survive billing retention.
package server

// Serialize overlapping balance revisions in a stable order. No historical
// escrow scan or backfill is needed: an unversioned balance starts at zero.
const NetEscrowAdvanceRevisionFunctionBodySql = `
DECLARE
	selected_balance_id uuid;
BEGIN
	FOR selected_balance_id IN
		SELECT DISTINCT balance_id FROM unnest(balance_ids) AS balances(balance_id)
		WHERE balance_id IS NOT NULL ORDER BY balance_id
	LOOP
		INSERT INTO transfer_balance_net_escrow_revision (balance_id, revision)
		VALUES (selected_balance_id, 1)
		ON CONFLICT (balance_id) DO UPDATE
		SET revision = transfer_balance_net_escrow_revision.revision + 1;
	END LOOP;
END
`

// Escrow rows can precede their contract in the same transaction. Only
// nonzero unsettled rows can affect reservations; settled-history cleanup and
// zero-byte anchors must not amplify hot-path writes.
const NetEscrowRowsRevisionFunctionBodySql = `
BEGIN
	IF TG_OP = 'INSERT' THEN
		PERFORM advance_net_escrow_revision(ARRAY(SELECT balance_id FROM new_escrow_rows WHERE NOT settled AND balance_byte_count <> 0));
	ELSIF TG_OP = 'DELETE' THEN
		PERFORM advance_net_escrow_revision(ARRAY(SELECT balance_id FROM old_escrow_rows WHERE NOT settled AND balance_byte_count <> 0));
	ELSE
		PERFORM advance_net_escrow_revision(ARRAY(
			SELECT balance_id FROM old_escrow_rows WHERE NOT settled AND balance_byte_count <> 0
			UNION SELECT balance_id FROM new_escrow_rows WHERE NOT settled AND balance_byte_count <> 0
		));
	END IF;
	RETURN NULL;
END
`

// Keep migration 747 byte-identical. The appended function replacement below
// supplies the contract-key optimization boundary without rewriting history.
// NetEscrowContractsRevisionLegacyFunctionBodySql is the byte-exact body
// published by migration 747. The monitor uses it until migration 753 replaces
// the function; keeping both bodies prevents a current-source false drift.
const NetEscrowContractsRevisionLegacyFunctionBodySql = `
BEGIN
	IF TG_OP = 'INSERT' THEN
		PERFORM advance_net_escrow_revision(ARRAY(
			SELECT escrow.balance_id FROM transfer_escrow AS escrow
			JOIN new_escrow_contracts AS contract USING (contract_id)
			WHERE contract.outcome IS NULL AND NOT escrow.settled AND escrow.balance_byte_count <> 0
		));
	ELSIF TG_OP = 'DELETE' THEN
		PERFORM advance_net_escrow_revision(ARRAY(
			SELECT escrow.balance_id FROM transfer_escrow AS escrow
			JOIN old_escrow_contracts AS contract USING (contract_id)
			WHERE contract.outcome IS NULL AND NOT escrow.settled AND escrow.balance_byte_count <> 0
		));
	ELSE
		PERFORM advance_net_escrow_revision(ARRAY(
			SELECT escrow.balance_id FROM transfer_escrow AS escrow
			JOIN (
				SELECT COALESCE(old_contract.contract_id, new_contract.contract_id) AS contract_id
				FROM old_escrow_contracts AS old_contract
				FULL JOIN new_escrow_contracts AS new_contract USING (contract_id)
				WHERE old_contract.contract_id IS NULL OR new_contract.contract_id IS NULL
					OR (old_contract.outcome IS NULL) <> (new_contract.outcome IS NULL)
			) AS changed USING (contract_id)
			WHERE NOT escrow.settled AND escrow.balance_byte_count <> 0
		));
	END IF;
	RETURN NULL;
END
`

// Only insertion/removal of an open contract or a change to its outcome can
// change its reservation. Checkpoints and immutable usage writes are not new
// reservation states. Start with each changed contract, then constrain escrow
// by its primary-key prefix. Keep settled/zero filters outside OFFSET 0 so
// false-zero partial-index statistics cannot substitute a global unsettled scan.
const NetEscrowContractsRevisionFunctionBodySql = `
BEGIN
	IF TG_OP = 'INSERT' THEN
		PERFORM advance_net_escrow_revision(ARRAY(
			SELECT escrow.balance_id FROM new_escrow_contracts AS contract
			CROSS JOIN LATERAL (
				SELECT balance_id, settled, balance_byte_count FROM transfer_escrow
				WHERE contract_id = contract.contract_id OFFSET 0
			) AS escrow
			WHERE contract.outcome IS NULL AND NOT escrow.settled AND escrow.balance_byte_count <> 0
		));
	ELSIF TG_OP = 'DELETE' THEN
		PERFORM advance_net_escrow_revision(ARRAY(
			SELECT escrow.balance_id FROM old_escrow_contracts AS contract
			CROSS JOIN LATERAL (
				SELECT balance_id, settled, balance_byte_count FROM transfer_escrow
				WHERE contract_id = contract.contract_id OFFSET 0
			) AS escrow
			WHERE contract.outcome IS NULL AND NOT escrow.settled AND escrow.balance_byte_count <> 0
		));
	ELSE
		PERFORM advance_net_escrow_revision(ARRAY(
			SELECT escrow.balance_id FROM (
				SELECT COALESCE(old_contract.contract_id, new_contract.contract_id) AS contract_id
				FROM old_escrow_contracts AS old_contract
				FULL JOIN new_escrow_contracts AS new_contract USING (contract_id)
				WHERE old_contract.contract_id IS NULL OR new_contract.contract_id IS NULL
					OR (old_contract.outcome IS NULL) <> (new_contract.outcome IS NULL)
			) AS changed
			CROSS JOIN LATERAL (
				SELECT balance_id, settled, balance_byte_count FROM transfer_escrow
				WHERE contract_id = changed.contract_id OFFSET 0
			) AS escrow
			WHERE NOT escrow.settled AND escrow.balance_byte_count <> 0
		));
	END IF;
	RETURN NULL;
END
`

// Function replacement invalidates cached trigger plans without scanning or
// rewriting billing history. Existing statement triggers and lock order remain.
const netEscrowContractsRevisionPointLookupSql = `
	CREATE OR REPLACE FUNCTION transfer_contract_escrow_revision()
	RETURNS trigger LANGUAGE plpgsql AS $contract$` + NetEscrowContractsRevisionFunctionBodySql + `$contract$;
`

// A removed balance is a zero-reservation tombstone, even when old escrow rows
// have not been reaped. Retain its revision if the identity is ever reused.
const NetEscrowBalancesRevisionFunctionBodySql = `
BEGIN
	IF TG_OP = 'DELETE' THEN
		PERFORM advance_net_escrow_revision(ARRAY(SELECT balance_id FROM old_escrow_balances));
	ELSIF TG_OP = 'INSERT' THEN
		PERFORM advance_net_escrow_revision(ARRAY(
			SELECT balance.balance_id FROM new_escrow_balances AS balance
			JOIN transfer_balance_net_escrow_revision USING (balance_id)
		));
	ELSE
		PERFORM advance_net_escrow_revision(ARRAY(
			SELECT COALESCE(old_balance.balance_id, new_balance.balance_id)
			FROM old_escrow_balances AS old_balance
			FULL JOIN new_escrow_balances AS new_balance USING (balance_id)
			WHERE old_balance.balance_id IS NULL OR new_balance.balance_id IS NULL
		));
	END IF;
	RETURN NULL;
END
`

// Revision history cannot be reset by ordinary retention, and bulk truncate
// cannot silently bypass the row transitions used by the reservation snapshot.
const NetEscrowRevisionGuardFunctionBodySql = `
BEGIN
	IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
		RAISE EXCEPTION 'net escrow revision history cannot be discarded';
	ELSIF TG_OP = 'INSERT' THEN
		IF NEW.revision <> 1 THEN
			RAISE EXCEPTION 'net escrow revision must start at one';
		END IF;
	ELSIF NEW.balance_id <> OLD.balance_id OR NEW.revision <> OLD.revision + 1 THEN
		RAISE EXCEPTION 'net escrow revision must advance once for the same balance';
	END IF;
	RETURN NEW;
END
`

// Appended migration 747 does not alter balances, provider attribution, or
// terminal history. All cache writers must be upgraded together before work
// resumes; legacy additive posts must never coexist with fenced snapshots.
const netEscrowRevisionSchemaSql = `
	CREATE TABLE transfer_balance_net_escrow_revision (
		balance_id uuid PRIMARY KEY,
		revision bigint NOT NULL CHECK (revision > 0)
	);
	CREATE FUNCTION net_escrow_revision_guard()
	RETURNS trigger LANGUAGE plpgsql AS $guard$` + NetEscrowRevisionGuardFunctionBodySql + `$guard$;
	CREATE TRIGGER net_escrow_revision_guard BEFORE INSERT OR UPDATE OR DELETE ON transfer_balance_net_escrow_revision
	FOR EACH ROW EXECUTE FUNCTION net_escrow_revision_guard();
	CREATE TRIGGER net_escrow_revision_truncate_guard BEFORE TRUNCATE ON transfer_balance_net_escrow_revision
	FOR EACH STATEMENT EXECUTE FUNCTION net_escrow_revision_guard();
	CREATE TRIGGER transfer_escrow_revision_truncate_guard BEFORE TRUNCATE ON transfer_escrow
	FOR EACH STATEMENT EXECUTE FUNCTION net_escrow_revision_guard();
	CREATE TRIGGER transfer_balance_revision_truncate_guard BEFORE TRUNCATE ON transfer_balance
	FOR EACH STATEMENT EXECUTE FUNCTION net_escrow_revision_guard();
	CREATE FUNCTION advance_net_escrow_revision(balance_ids uuid[])
	RETURNS void LANGUAGE plpgsql AS $advance$` + NetEscrowAdvanceRevisionFunctionBodySql + `$advance$;
	CREATE FUNCTION transfer_escrow_revision()
	RETURNS trigger LANGUAGE plpgsql AS $escrow$` + NetEscrowRowsRevisionFunctionBodySql + `$escrow$;
	CREATE TRIGGER transfer_escrow_revision_insert AFTER INSERT ON transfer_escrow
	REFERENCING NEW TABLE AS new_escrow_rows FOR EACH STATEMENT EXECUTE FUNCTION transfer_escrow_revision();
	CREATE TRIGGER transfer_escrow_revision_update AFTER UPDATE ON transfer_escrow
	REFERENCING OLD TABLE AS old_escrow_rows NEW TABLE AS new_escrow_rows FOR EACH STATEMENT EXECUTE FUNCTION transfer_escrow_revision();
	CREATE TRIGGER transfer_escrow_revision_delete AFTER DELETE ON transfer_escrow
	REFERENCING OLD TABLE AS old_escrow_rows FOR EACH STATEMENT EXECUTE FUNCTION transfer_escrow_revision();
	CREATE FUNCTION transfer_contract_escrow_revision()
	RETURNS trigger LANGUAGE plpgsql AS $contract$` + NetEscrowContractsRevisionLegacyFunctionBodySql + `$contract$;
	CREATE TRIGGER transfer_contract_escrow_revision_insert AFTER INSERT ON transfer_contract
	REFERENCING NEW TABLE AS new_escrow_contracts FOR EACH STATEMENT EXECUTE FUNCTION transfer_contract_escrow_revision();
	CREATE TRIGGER transfer_contract_escrow_revision_update AFTER UPDATE ON transfer_contract
	REFERENCING OLD TABLE AS old_escrow_contracts NEW TABLE AS new_escrow_contracts FOR EACH STATEMENT EXECUTE FUNCTION transfer_contract_escrow_revision();
	CREATE TRIGGER transfer_contract_escrow_revision_delete AFTER DELETE ON transfer_contract
	REFERENCING OLD TABLE AS old_escrow_contracts FOR EACH STATEMENT EXECUTE FUNCTION transfer_contract_escrow_revision();
	CREATE FUNCTION transfer_balance_escrow_revision()
	RETURNS trigger LANGUAGE plpgsql AS $balance$` + NetEscrowBalancesRevisionFunctionBodySql + `$balance$;
	CREATE TRIGGER transfer_balance_escrow_revision_insert AFTER INSERT ON transfer_balance
	REFERENCING NEW TABLE AS new_escrow_balances FOR EACH STATEMENT EXECUTE FUNCTION transfer_balance_escrow_revision();
	CREATE TRIGGER transfer_balance_escrow_revision_delete AFTER DELETE ON transfer_balance
	REFERENCING OLD TABLE AS old_escrow_balances FOR EACH STATEMENT EXECUTE FUNCTION transfer_balance_escrow_revision();
	CREATE TRIGGER transfer_balance_escrow_revision_update AFTER UPDATE ON transfer_balance
	REFERENCING OLD TABLE AS old_escrow_balances NEW TABLE AS new_escrow_balances FOR EACH STATEMENT EXECUTE FUNCTION transfer_balance_escrow_revision();
`

// Lazily populated only from exact revision-fenced reservation snapshots.
// Existing writers need no change: every revision advance invalidates an old
// cached amount. Retained revision tombstones also fence balance-id reuse.
// No history scan, backfill or populated index build is required.
const netEscrowAdmissionSnapshotSchemaSql = `
 CREATE TABLE transfer_balance_net_escrow_snapshot (
     balance_id uuid PRIMARY KEY,
     revision bigint NOT NULL CHECK (revision >= 0),
     reserved_byte_count bigint NOT NULL CHECK (reserved_byte_count >= 0)
 );
`
