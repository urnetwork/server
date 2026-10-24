package model

import (
	"testing"

	"github.com/urnetwork/server"
)

// A legacy-escrow contract records the receiver's final exactly like a current
// contract. The second final settles it inline, not through the deadline
// closer.
func TestTwoPartyFinalCloseSettlesLegacyEscrowInline(t *testing.T) {
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
			server.Raise(conn.QueryRow(ctx, `SELECT checkpoint,used_transfer_byte_count
                FROM contract_close WHERE contract_id=$1 AND party=$2`, contractId, ContractPartyDestination).Scan(&destinationCheckpoint, &destinationByteCount))
			if destinationCheckpoint || destinationByteCount != byteCount {
				t.Fatalf("legacy receiver final recorded checkpoint=%t bytes=%d, want final %d", destinationCheckpoint, destinationByteCount, byteCount)
			}
		})
		requireLegacySettlementTestState(t, ctx, f, contractId, true, 1000-byteCount, 0)
		requireLegacyProviderDurability(t, ctx, f, contractId, int64(byteCount))
	})
}
