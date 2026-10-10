// Transaction retries must discard accounting accumulated by a rolled-back attempt.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The sequence forces one serialization failure on the second grant. Sequence
// increments survive rollback, so the next transaction deterministically succeeds.
func TestReferralGrantSerializationRetryReturnsOnlyCommittedAmounts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		config := Pro()
		previous := *config
		defer func() { *config = previous }()
		config.MaxReferrals = 10
		referrerId, refereeId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, referrerId, "synthetic-referrer", server.NewId())
		Testing_CreateNetwork(ctx, refereeId, "synthetic-referee", server.NewId())
		code := CreateNetworkReferralCode(ctx, referrerId)
		if CreateNetworkReferral(ctx, refereeId, code.ReferralCode) == nil {
			t.Fatal("synthetic referral rejected")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE SEQUENCE synthetic_grant_attempt;
				CREATE FUNCTION synthetic_retry_grant() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
				IF nextval('synthetic_grant_attempt')=2 THEN
					RAISE EXCEPTION 'synthetic serialization failure' USING ERRCODE='40001';
				END IF; RETURN NEW; END $$;
				CREATE TRIGGER synthetic_retry_grant BEFORE INSERT ON transfer_balance
				FOR EACH ROW EXECUTE FUNCTION synthetic_retry_grant()`))
		})
		at := server.NowUtc()
		runId := server.NewId()
		added := AddReferralBonusesToAllNetworks(ctx, at, at.Add(24*time.Hour), 200, 300, runId)
		if len(added) != 2 || added[referrerId] != 200 || added[refereeId] != 300 {
			t.Fatal("rolled-back grant leaked into committed result", added)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var attempts, receipts int
			var total ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT last_value FROM synthetic_grant_attempt`).Scan(&attempts))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_balance_grant_run WHERE run_id=$1`, runId).Scan(&receipts))
			server.Raise(conn.QueryRow(ctx, `SELECT sum(start_balance_byte_count) FROM transfer_balance`).Scan(&total))
			if attempts != 4 || receipts != 1 || total != 500 {
				t.Fatal("retry lost atomic grants and receipt", attempts, receipts, total)
			}
		})
	})
}
