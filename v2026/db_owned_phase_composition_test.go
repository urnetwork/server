package server

import (
	"context"
	"testing"
)

// The current nonwaiting API retains both its bool result and phase option.
// A known refusal never enters the callback; a later real commit is acknowledged.
func TestTryOwnedTxPhasePreservesBusyAndAcknowledgedOutcome(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		key := NewPgOwnershipKey("synthetic-try-phase", NewId())
		phase := &DbPhaseObservation{}
		calls := 0
		OwnedTx(ctx, []PgOwnershipKey{key}, func(PgTx) {
			if TryOwnedTx(ctx, []PgOwnershipKey{key}, func(PgTx) { calls++ }, TxReadCommitted, OptNoRetry(), phase) ||
				calls != 0 || phase.Phase() != DbOperationAdmission {
				t.Fatal("phase composition changed known busy admission or entered callback")
			}
		}, TxReadCommitted, OptNoRetry())
		if !TryOwnedTx(ctx, []PgOwnershipKey{key}, func(tx PgTx) {
			calls++
			if phase.Phase() != DbOperationCallback || !TxOwnsKeys(tx, []PgOwnershipKey{key}) {
				t.Fatal("admitted callback lost source phase or ownership")
			}
			RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,19)`))
		}, TxReadCommitted, OptNoRetry(), phase) || calls != 1 || phase.Phase() != DbOperationAcknowledged {
			t.Fatal("phase option changed acknowledged TryOwnedTx outcome")
		}
		Db(ctx, func(conn PgConn) {
			var exact bool
			Raise(conn.QueryRow(ctx, `SELECT count(*)=1 AND sum(amount)=19 FROM owned_tx_effect`).Scan(&exact))
			if !exact {
				t.Fatal("phase option changed the committed effect")
			}
		}, OptNoRetry())
	})
}
