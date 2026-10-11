package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Count the actual authoritative SQL's bound grant IDs on real PostgreSQL.
// A successful first/extended candidate must never census its entire window.
func TestDynamicProberSuccessfulCensusTouchesOnlySelectedGrant(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, extended := range []bool{false, true} {
			clients := newEscrowSelectionTestClients(t, ctx)
			setDynamicProberIdentityForTest(t, ctx, clients)
			now := server.NowUtc()
			count := proberGrantFirstCount
			if extended {
				count += proberGrantExtendedCount
			}
			ids := make([]server.Id, 0, count)
			for i := range count {
				amount := ByteCount(4096)
				if extended && i < proberGrantFirstCount {
					amount = 512
				}
				grant := addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(-time.Duration(i+1)*time.Minute), now.Add(time.Hour), amount)
				ids = append(ids, grant.BalanceId)
			}
			if !extended {
				// Each candidate has 5,120 unsettled rows (512 open, 4,608 closed)
				// plus 5,120 settled historical rows. This mirrors the measured query's
				// per-grant fanout and ensures closed/settled history never consumes credit.
				contracts := make([]server.Id, 10240)
				for i := range contracts {
					contracts[i] = server.NewId()
				}
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
       (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,payer_network_id,outcome,close_time,provider_usage)
       SELECT id,$2,$3,$4,$5,16,$2,CASE WHEN n>512 THEN 'settled' ELSE NULL END,
        CASE WHEN n>512 THEN now() ELSE NULL END,
        CASE WHEN n>512 THEN '{"version":1,"byte_count":0,"providers":[]}'::jsonb ELSE NULL END
       FROM unnest($1::uuid[]) WITH ORDINALITY AS c(id,n)`, contracts, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId))
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count,settled)
       SELECT c.id,b.id,1,c.n>5120
       FROM unnest($1::uuid[]) WITH ORDINALITY AS c(id,n)
       CROSS JOIN unnest($2::uuid[]) AS b(id)`, contracts, ids))
				}, server.TxReadCommitted, server.OptNoRetry())
			}
			started := time.Now()
			server.Tx(ctx, func(tx server.PgTx) {
				counted := &grantHintTestTx{PgTx: tx}
				selected := loadTransferEscrowBalances(ctx, counted, clients.payerNetworkId, clients.payerId, now, 1024)
				expected := ByteCount(4096)
				if !extended {
					expected -= 512
				}
				if len(selected) != 1 || selected[0].balanceByteCount != expected {
					t.Fatal("selected-grant census lost open/closed/settled reservation semantics")
				}
				if counted.reservationReads != 1 || counted.reservationRows != 1 {
					t.Fatalf("extended=%t successful selection censused %d grants in %d exact reads; want one", extended, counted.reservationRows, counted.reservationReads)
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			t.Logf("extended=%t selected_grant_exact_reads=1 grants=1 elapsed=%s", extended, time.Since(started))
		}
	})
}
