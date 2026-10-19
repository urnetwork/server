package server

// No foreign key takes a shared balance/contract lock from this append-only
// settlement path. Retention explicitly preserves balances with pending rows.
// The worker commits each grouped debit and its applied bit atomically, then
// releases Redis reservations before deleting the recovery records.
const transferDebitJournalSchemaSql = `
 CREATE TABLE transfer_debit_journal (
  contract_id uuid NOT NULL,
  balance_id uuid NOT NULL,
  shard smallint NOT NULL CHECK (shard = get_byte(uuid_send(balance_id),15) % 16),
  debit_byte_count bigint NOT NULL CHECK (debit_byte_count >= 0),
  applied boolean NOT NULL DEFAULT false,
  create_time timestamp NOT NULL DEFAULT (clock_timestamp() AT TIME ZONE 'UTC'),
  PRIMARY KEY (balance_id, contract_id)
 );
 CREATE INDEX transfer_debit_journal_contract ON transfer_debit_journal(contract_id, balance_id);
 CREATE INDEX transfer_debit_journal_shard ON transfer_debit_journal(shard,balance_id,contract_id);
 CREATE INDEX transfer_debit_journal_age ON transfer_debit_journal(shard,applied,create_time);
 CREATE FUNCTION protect_pending_transfer_debit() RETURNS trigger LANGUAGE plpgsql AS $guard$
 BEGIN
  IF EXISTS(SELECT 1 FROM transfer_debit_journal WHERE balance_id=OLD.balance_id) THEN
   RAISE EXCEPTION 'balance has pending asynchronous consumption' USING ERRCODE='55000';
  END IF;
  RETURN OLD;
 END
 $guard$;
 CREATE TRIGGER transfer_balance_pending_debit_guard BEFORE DELETE ON transfer_balance
 FOR EACH ROW EXECUTE FUNCTION protect_pending_transfer_debit();
`
