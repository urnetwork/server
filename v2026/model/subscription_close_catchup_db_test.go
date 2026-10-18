// Native gap admission retains the ordinary expiry proof and financial owners.
package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Quiet funded contracts between the retained recent and fresh passes close
// while both old prefixes remain unfinished. An authenticated active sibling
// returns on a later logical cutoff and settles once through the same owner.
func TestExpiryCatchupNativeGapClosesAndRevisitsQuiet(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		cutoff := server.NowUtc().Truncate(time.Microsecond).Add(-12 * time.Minute)
		epoch, recentUpper := cutoff.Add(-72*time.Hour), cutoff.Add(-24*time.Hour)
		var retainedIds []server.Id
		for _, start := range []time.Time{epoch.Add(-time.Hour), epoch.Add(time.Hour), cutoff.Add(-time.Minute)} {
			for index := range 12 {
				id, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
				server.Raise(err)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, id, start.Add(time.Duration(index)*time.Second)))
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
						VALUES($1,'source',0,$2,true)`, id, cutoff.Add(24*time.Hour)))
				})
				retainedIds = append(retainedIds, id)
			}
		}
		createFunded := func(created, reported time.Time) server.Id {
			escrow := createRedisAdmissionTest(ctx, f, 100)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, escrow.ContractId, created))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
					VALUES($1,'source',17,$2,true),($1,'destination',11,$2,true)`, escrow.ContractId, reported))
			})
			return escrow.ContractId
		}
		quiet := createFunded(cutoff.Add(-6*time.Hour), cutoff.Add(-time.Minute))
		active := createFunded(cutoff.Add(-5*time.Hour), cutoff.Add(time.Minute))
		neighbor := createRedisAdmissionTest(ctx, f, 37)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				VALUES($1,'source',0,$2,true)`, neighbor.ContractId, cutoff.Add(24*time.Hour)))
		})
		neighborBefore := readRedisExpiryRepairTestState(ctx, neighbor.ContractId)
		freshLower := &ContractExpiryPosition{CreateTime: cutoff.Add(-time.Hour), ContractId: server.Id{
			255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255,
		}}
		recentLower := &ContractExpiryPosition{CreateTime: epoch, ContractId: freshLower.ContractId}
		cursor := &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: epoch}, RecentAfter: epoch,
			Recent: &ContractExpiryCursor{ScanBefore: recentUpper, Open: recentLower, Dispute: recentLower},
			Fresh:  &ContractExpiryCursor{ScanBefore: cutoff, Open: freshLower, Dispute: freshLower}}
		step := func(bound time.Time) int64 {
			t.Helper()
			clock := bound
			count, next, err := forceCloseContractPagesBudgeted(ctx, 128, cursor, time.Second, 1,
				func() time.Time { return clock }, func(size int, after *ContractExpirySweepCursor) (int64, *ContractExpirySweepCursor, error) {
					return forceCloseContractExpiryFreshPage(bound, bound.Add(12*time.Minute), after,
						func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
							count, next, err := ForceCloseOpenContractIdsPage(ctx, bound, size, 1, 1, 0, position)
							clock = clock.Add(2 * time.Second)
							return count, next, err
						})
				})
			if err != nil || next == nil {
				t.Fatal("native gap page lost its financial owner or unfinished prefixes", err)
			}
			raw, _ := json.Marshal(next)
			var stored *ContractExpirySweepCursor
			server.Raise(json.Unmarshal(raw, &stored))
			cursor = stored
			return count
		}
		closed := int64(0)
		for range 18 {
			closed += step(cutoff)
			if cursor.Catchup == nil && cursor.CatchupBefore.Equal(cutoff.Add(-time.Hour)) {
				break
			}
		}
		if _, terminal := GetContractClose(ctx, quiet); !terminal || closed != 1 {
			t.Fatal("quiet funded gap contract was omitted behind the retained recent upper")
		}
		if _, terminal := GetContractClose(ctx, active); terminal {
			t.Fatal("gap admission bypassed the authenticated report quiet period")
		}
		if cursor.Historical == nil || cursor.Recent == nil || cursor.Fresh == nil ||
			cursor.Historical.Open == nil || cursor.Recent.Open == nil || cursor.Fresh.Open == nil ||
			!cursor.Historical.ScanBefore.Equal(epoch) || !cursor.Recent.ScanBefore.Equal(recentUpper) || !cursor.Fresh.ScanBefore.Equal(cutoff) {
			t.Fatal("gap completion dropped or rewound a retained original pass")
		}
		for range 18 {
			closed += step(cutoff.Add(13 * time.Minute))
			if _, terminal := GetContractClose(ctx, active); terminal {
				break
			}
		}
		if _, terminal := GetContractClose(ctx, active); !terminal || closed != 2 {
			t.Fatal("gap report that became quiet was stranded after its first catch-up pass")
		}
		for _, id := range []server.Id{quiet, active} {
			_, snapshot := readContractExpiryTestSnapshot(t, ctx, id)
			if snapshot.ByteCount != 11 || snapshot.Expiry == nil || snapshot.Expiry.Reports[ContractPartySource].ByteCount != 17 ||
				!snapshot.Expiry.Reports[ContractPartySource].Checkpoint || snapshot.Expiry.Reports[ContractPartyDestination].ByteCount != 11 ||
				!snapshot.Expiry.Reports[ContractPartyDestination].Checkpoint {
				t.Fatal("catch-up changed the original authenticated expiry proof")
			}
		}
		flushed, err := FlushTransferDebits(ctx, int(f.balanceId[15])%TransferDebitShardCount, nil, 1)
		if err != nil || flushed.Applied != 2 || flushed.Released != 2 || flushed.Failed != 0 {
			t.Fatal("gap close lost an exact durable debit owner", err)
		}
		replayed, err := FlushTransferDebits(ctx, int(f.balanceId[15])%TransferDebitShardCount, nil, 1)
		if err != nil || replayed.Applied != 0 || replayed.Released != 0 || replayed.Failed != 0 {
			t.Fatal("gap recheck repeated a settled financial debit", err)
		}
		projectLegacyProviderTotalsForTest(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var conserved bool
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT balance_byte_count=972 FROM transfer_balance WHERE balance_id=$1) AND
				(SELECT sum(payout_byte_count)=28 FROM transfer_escrow_sweep WHERE contract_id=ANY($2)) AND
				(SELECT provided_byte_count=28 FROM account_balance WHERE network_id=$3) AND
				(SELECT count(*)=0 FROM transfer_contract WHERE contract_id=ANY($4) AND outcome IS NOT NULL)`,
				f.balanceId, []server.Id{quiet, active}, f.destinationNetworkId, retainedIds).Scan(&conserved))
			if !conserved {
				t.Fatal("gap service changed exact debit, payout, provider total, or active original rows")
			}
		})
		if string(neighborBefore) != string(readRedisExpiryRepairTestState(ctx, neighbor.ContractId)) || Testing_NetEscrowByteCount(ctx, f.balanceId) != 37 {
			t.Fatal("gap service changed an active neighboring reservation")
		}
		requireRedisExpiryClock(t, ctx, "22")
	})
}
