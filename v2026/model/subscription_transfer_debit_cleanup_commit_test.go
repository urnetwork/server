package model

import (
	"testing"

	"github.com/urnetwork/server/v2026"
)

// A cleanup DELETE can succeed and then be rolled back at Commit. The worker
// reports only journal removals whose commit returned successfully.
func TestAsyncDebitCleanupCommitRefusalKeepsReleasedCountZero(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, fixture, 100)
		_ = asyncDebitTestSettle(ctx, contract.ContractId, 11)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_cleanup_commit_failure() RETURNS trigger
				LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic cleanup commit failure'; END $$`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE CONSTRAINT TRIGGER synthetic_commit_failure
				AFTER DELETE ON transfer_debit_journal DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW EXECUTE FUNCTION synthetic_cleanup_commit_failure()`))
		})
		shard := transferDebitShard(fixture.balanceId)
		result, err := FlushTransferDebits(ctx, shard, nil, 1)
		credit, pending, applied := asyncDebitTestState(t, ctx, fixture.balanceId)
		if err != nil || result.Failed != 1 || result.Applied != 1 || credit != 989 ||
			pending != 0 || applied != 1 || Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 0 {
			t.Fatal("deferred cleanup failure did not retain precisely the committed debit")
		}
		if result.Released != 0 {
			t.Fatalf("unacknowledged cleanup reported %d released journal rows", result.Released)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "DROP TRIGGER synthetic_commit_failure ON transfer_debit_journal"))
		})
		result, err = FlushTransferDebits(ctx, shard, nil, 1)
		credit, pending, applied = asyncDebitTestState(t, ctx, fixture.balanceId)
		if err != nil || result.Failed != 0 || result.Released != 1 || result.Applied != 0 ||
			credit != 989 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 0 {
			t.Fatal("replay did not report exactly one acknowledged release without a second debit")
		}
	})
}
