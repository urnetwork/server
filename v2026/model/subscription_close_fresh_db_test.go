// Native selection proves fresh admission and delayed-report re-entry.
package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Raw pages use their actual SQL and ordinary expiry owner. Logical budget
// expiry yields exactly one complete subpage without canceling financial posts.
func TestExpiryFreshNativeVisitsNewerThanBothPassesAndRevisitsQuiet(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		cutoff := server.NowUtc().Truncate(time.Microsecond).Add(-12 * time.Minute)
		epoch, middleUpper := cutoff.Add(-72*time.Hour), cutoff.Add(-24*time.Hour)
		var history, middle []server.Id
		createUnfunded := func(created, reported time.Time) server.Id {
			id, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
			server.Raise(err)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, id, created))
				if !reported.IsZero() {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
						VALUES($1,'source',0,$2,true)`, id, reported))
				}
			})
			return id
		}
		for index := range 12 {
			history = append(history, createUnfunded(epoch.Add(-time.Hour+time.Duration(index)*time.Second), cutoff.Add(24*time.Hour)))
			middle = append(middle, createUnfunded(epoch.Add(time.Hour+time.Duration(index)*time.Second), cutoff.Add(24*time.Hour)))
		}
		// This gap is above the original middle pass and outside the fresh
		// window. A later complete pass must still cover it; it is never dropped.
		gap := createUnfunded(cutoff.Add(-6*time.Hour), time.Time{})
		createFunded := func(created, reported time.Time) server.Id {
			escrow := createRedisAdmissionTest(ctx, f, 100)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, escrow.ContractId, created))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
					VALUES($1,'source',17,$2,true),($1,'destination',11,$2,true)`, escrow.ContractId, reported))
			})
			return escrow.ContractId
		}
		active := createFunded(cutoff.Add(-3*time.Minute), cutoff.Add(time.Second))
		quiet := createFunded(cutoff.Add(-2*time.Minute), cutoff.Add(-2*time.Minute))
		neighbor := createRedisAdmissionTest(ctx, f, 37)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				VALUES($1,'source',0,$2,true)`, neighbor.ContractId, cutoff.Add(24*time.Hour)))
		})
		neighborBefore := readRedisExpiryRepairTestState(ctx, neighbor.ContractId)
		cursor := &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: epoch},
			Recent: &ContractExpiryCursor{ScanBefore: middleUpper}, RecentAfter: epoch}
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
			if err != nil {
				t.Fatal("native fresh subpage failed its original financial owner", err)
			}
			raw, _ := json.Marshal(next)
			var stored *ContractExpirySweepCursor
			server.Raise(json.Unmarshal(raw, &stored))
			cursor = stored
			return count
		}
		closed := int64(0)
		for range 8 {
			closed += step(cutoff)
			if cursor != nil && cursor.Fresh == nil && cursor.FreshBefore.Equal(cutoff) {
				break
			}
		}
		if _, terminal := GetContractClose(ctx, quiet); !terminal || closed != 1 {
			t.Fatal("eligible contract beyond both fixed upper bounds was not closed by fresh admission")
		}
		if _, terminal := GetContractClose(ctx, active); terminal {
			t.Fatal("fresh tail ignored the authenticated recent-report quiet period")
		}
		if cursor == nil || cursor.Historical == nil || cursor.Recent == nil || cursor.Recent.Open == nil ||
			!cursor.Historical.ScanBefore.Equal(epoch) || !cursor.Recent.ScanBefore.Equal(middleUpper) || !cursor.RecentAfter.Equal(epoch) {
			t.Fatal("fresh completion reset or abandoned the unfinished historical/middle passes")
		}
		// No report is edited: logical time alone now makes the retained
		// authenticated report quiet, after the first fresh upper bound passed.
		later := cutoff.Add(13 * time.Minute)
		for range 8 {
			closed += step(later)
			if _, terminal := GetContractClose(ctx, active); terminal {
				break
			}
		}
		if _, terminal := GetContractClose(ctx, active); !terminal || closed != 2 {
			t.Fatal("recent-report skip was stranded behind the full backlog after the fresh watermark advanced")
		}
		for _, id := range []server.Id{quiet, active} {
			_, snapshot := readContractExpiryTestSnapshot(t, ctx, id)
			if snapshot.ByteCount != 11 || snapshot.Expiry == nil || snapshot.Expiry.Reports[ContractPartySource].ByteCount != 17 ||
				!snapshot.Expiry.Reports[ContractPartySource].Checkpoint || snapshot.Expiry.Reports[ContractPartyDestination].ByteCount != 11 ||
				!snapshot.Expiry.Reports[ContractPartyDestination].Checkpoint {
				t.Fatal("fresh scheduling changed the original report proof")
			}
		}
		// Finish the original finite ranges before asking a new full pass to
		// cover their gap. Each original raw row can consume a visit in each
		// lane; empty tails and the remaining fresh pass consume extra turns.
		// The bound follows this fixture's row count, not elapsed time.
		passTurns := 2 * (len(history) + len(middle) + 4)
		for turn := 0; cursor != nil && turn < passTurns; turn++ {
			step(later)
		}
		if cursor != nil {
			t.Fatal("fresh scheduling prevented the original finite backlog passes from completing")
		}
		// A new historical pass includes every original middle row. Empty
		// recent turns alternate with it while the active old rows stay open.
		for range passTurns {
			step(later)
			if _, terminal := GetContractClose(ctx, gap); terminal {
				break
			}
		}
		if _, terminal := GetContractClose(ctx, gap); !terminal {
			t.Fatal("fresh window discarded an older middle contract from complete coverage")
		}
		flushed, err := FlushTransferDebits(ctx, int(f.balanceId[15])%TransferDebitShardCount, nil, 1)
		if err != nil || flushed.Applied != 2 || flushed.Released != 2 || flushed.Failed != 0 {
			t.Fatal("fresh admission lost the exact durable debit owners")
		}
		replayed, err := FlushTransferDebits(ctx, int(f.balanceId[15])%TransferDebitShardCount, nil, 1)
		if err != nil || replayed.Applied != 0 || replayed.Released != 0 || replayed.Failed != 0 {
			t.Fatal("fresh revisit repeated a settled financial debit")
		}
		projectLegacyProviderTotalsForTest(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var conserved bool
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT balance_byte_count=972 FROM transfer_balance WHERE balance_id=$1) AND
				(SELECT sum(payout_byte_count)=28 FROM transfer_escrow_sweep WHERE contract_id=ANY($2)) AND
				(SELECT provided_byte_count=28 FROM account_balance WHERE network_id=$3) AND
				(SELECT count(*)=0 FROM transfer_contract WHERE contract_id=ANY($4) AND outcome IS NOT NULL)`,
				f.balanceId, []server.Id{quiet, active}, f.destinationNetworkId, append(history, middle...)).Scan(&conserved))
			if !conserved {
				t.Fatal("fresh service changed exact debit, provider total, or still-active backlog state")
			}
		})
		if string(neighborBefore) != string(readRedisExpiryRepairTestState(ctx, neighbor.ContractId)) || Testing_NetEscrowByteCount(ctx, f.balanceId) != 37 {
			t.Fatal("fresh service or worker recovery changed the active neighbor")
		}
		requireRedisExpiryClock(t, ctx, "22")
	})
}

// Parent cancellation grants no fresh or backlog checkpoint, including after
// a completed-looking fresh callback. This is independent of wall-clock time.
func TestExpiryFreshParentCancellationKeepsOriginalState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	after := &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: epoch}}
	_, next, err := forceCloseContractPagesBudgeted(ctx, 128, after, time.Second, 1, func() time.Time { return epoch },
		func(_ int, cursor *ContractExpirySweepCursor) (int64, *ContractExpirySweepCursor, error) {
			return forceCloseContractExpiryFreshPage(epoch.Add(time.Hour), epoch.Add(time.Hour), cursor,
				func(*ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
					cancel()
					return 1, nil, nil
				})
		})
	if err == nil || next != after || after.Fresh != nil || !after.FreshBefore.IsZero() {
		t.Fatal("parent cancellation acquired fresh scan checkpoint authority")
	}
}
