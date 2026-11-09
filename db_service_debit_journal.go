package server

import "strings"

// §20.14's service_debit_journal guards its balances as transfer_debit_journal does
// (db_transfer_debit_journal.go): a balance with a journal row, applied or not, cannot be
// deleted, since the flush releases the row's Redis token and deletes the row only after
// the debit commits (model's FlushServiceDebits). transfer_debit_journal's guard function
// is a registered artifact the monitor compares byte for byte, so this journal takes a
// trigger and function of its own rather than an edit to that one.
//
// The guard is permissive for a release that never writes the journal: it refuses
// nothing while the journal is empty, and costs a delete of a balance one primary-key
// probe. Creating the trigger takes transfer_balance's SHARE ROW EXCLUSIVE lock, which
// queues behind every open writer and ahead of later ones, so its wait is bounded as
// MG0's ALTERs are: a timeout fails `db migrate` with nothing applied, and running it
// again resumes here. Every reader that keeps a balance from deletion because
// transfer_debit_journal holds a row of it reads this journal too once MG2 is linked
// (model: balance retention, the redeemed code move, the probe shard reap), so none of
// them asks the guard to refuse a whole batch.
const serviceDebitJournalGuardSql = `
 SET LOCAL lock_timeout = '2s';
 CREATE FUNCTION protect_pending_service_debit() RETURNS trigger LANGUAGE plpgsql AS $guard$
 BEGIN
  IF EXISTS(SELECT 1 FROM service_debit_journal WHERE balance_id=OLD.balance_id) THEN
   RAISE EXCEPTION 'balance has pending service debits' USING ERRCODE='55000';
  END IF;
  RETURN OLD;
 END
 $guard$;
 CREATE TRIGGER transfer_balance_pending_service_debit_guard BEFORE DELETE ON transfer_balance
 FOR EACH ROW EXECUTE FUNCTION protect_pending_service_debit();
`

// The guard function's body, for a catalog check that compares it as published, as
// TransferDebitGuardBodySql does.
func ServiceDebitGuardBodySql() string {
	return strings.Split(serviceDebitJournalGuardSql, "$guard$")[1]
}
