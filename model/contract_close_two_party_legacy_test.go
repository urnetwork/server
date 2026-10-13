package model

import (
	"testing"

	"github.com/urnetwork/server"
)

// A legacy-escrow contract records the receiver's final exactly like a current
// contract. Its settlement is deferred to a durable legacy intent, not to the
// deadline closer, and the intent exists as soon as both parties are final.
func TestTwoPartyFinalCloseQueuesLegacyEscrowSettlement(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		escrow, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		contractId := escrow.ContractId
		var redisReserved bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(bool_or(redis_reserved),false) FROM transfer_escrow WHERE contract_id=$1`, contractId).Scan(&redisReserved))
		})
		if redisReserved {
			t.Fatal("fixture escrow is Redis reserved, not legacy")
		}
		const byteCount = ByteCount(11)
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: contractId, ClientId: f.sourceId, AckedByteCount: byteCount}); err != nil {
			t.Fatal(err)
		}
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: contractId, ClientId: f.destinationId, AckedByteCount: byteCount}); err != nil {
			t.Fatal(err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var destinationCheckpoint bool
			var destinationByteCount ByteCount
			var pending bool
			server.Raise(conn.QueryRow(ctx, `SELECT checkpoint,used_transfer_byte_count,
                EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
                FROM contract_close WHERE contract_id=$1 AND party=$2`, contractId, ContractPartyDestination).Scan(&destinationCheckpoint, &destinationByteCount, &pending))
			if destinationCheckpoint || destinationByteCount != byteCount {
				t.Fatalf("legacy receiver final recorded checkpoint=%t bytes=%d, want final %d", destinationCheckpoint, destinationByteCount, byteCount)
			}
			if !pending {
				t.Fatal("both finals did not queue the legacy settlement intent")
			}
		})
	})
}
