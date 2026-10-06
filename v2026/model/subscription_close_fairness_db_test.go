// Native expiry pages exercise the two SQL selectors and their durable bounds.
package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A retained historical head cannot delay post-epoch rows. Equal timestamp
// ties stay in their owning pass, fresh reports remain protected, and a later
// pass revisits those reports without moving the post-epoch lower boundary.
func TestExpiryFairNativeSelectorsKeepBoundariesAndRevisit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{sourceId: networkId, destinationId: networkId})
		epoch := server.NowUtc().Truncate(time.Microsecond).Add(-7 * 24 * time.Hour)
		cutoff := epoch.Add(2 * time.Hour)
		create := func(disputed bool, created, reported time.Time) server.Id {
			id, err := CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 100)
			if err != nil {
				t.Fatal(err)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,dispute=$3 WHERE contract_id=$1`, id, created, disputed))
				if disputed || !reported.IsZero() {
					if reported.IsZero() {
						reported = epoch
					}
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
						VALUES($1,'source',0,$2,$3)`, id, reported, !disputed))
					if disputed {
						server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
							VALUES($1,'destination',0,$2,false)`, id, reported))
					}
				}
			})
			return id
		}
		var historicalIds, quietIds, activeIds, laterIds []server.Id
		for _, disputed := range []bool{false, true} {
			for range 6 {
				// All epoch ties belong to history, even the largest id.
				historicalIds = append(historicalIds, create(disputed, epoch, epoch.Add(120*time.Hour)))
			}
			for range 2 {
				quietIds = append(quietIds, create(disputed, epoch.Add(time.Microsecond), time.Time{}))
			}
			activeIds = append(activeIds, create(disputed, epoch.Add(time.Hour), epoch.Add(24*time.Hour)))
			quietIds = append(quietIds, create(disputed, cutoff, time.Time{}))
			laterIds = append(laterIds, create(disputed, cutoff.Add(time.Microsecond), time.Time{}))
		}
		requireTerminalCount := func(ids []server.Id, expected int) {
			t.Helper()
			var terminal int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1::uuid[]) AND outcome IS NOT NULL`, ids).Scan(&terminal))
			})
			if terminal != expected {
				t.Fatalf("native expiry terminal count=%d want=%d", terminal, expected)
			}
		}
		cursor := &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: epoch}}
		step := func() int64 {
			t.Helper()
			count, next, err := forceCloseContractExpirySweepPage(cutoff, cutoff.Add(12*time.Minute), cursor,
				func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
					return ForceCloseOpenContractIdsPage(ctx, cutoff, 1, 1, 1, 0, position)
				})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(next)
			var stored *ContractExpirySweepCursor
			if err != nil || json.Unmarshal(raw, &stored) != nil {
				t.Fatal("native sweep checkpoint failed JSON round trip")
			}
			cursor = stored
			return count
		}
		if count := step(); count != 2 || cursor == nil || cursor.Historical == nil || cursor.Historical.Open != nil || cursor.Historical.Dispute != nil {
			t.Fatal("post-epoch selectors were not visited before historical backlog")
		}
		requireTerminalCount(historicalIds, 0)
		// Four raw rows and an empty tail in each recent selector, alternating
		// with four of the six retained historical rows in each selector.
		for range 8 {
			cutoff = cutoff.Add(time.Minute)
			step()
		}
		if cursor == nil || cursor.Recent != nil || cursor.Historical == nil || cursor.HistoricalDone || !cursor.RecentAfter.Equal(epoch) {
			t.Fatal("fixed recent pass consumed its future tail or lost unfinished history")
		}
		requireTerminalCount(quietIds, len(quietIds))
		requireTerminalCount(activeIds, 0)
		requireTerminalCount(laterIds, 0)
		requireTerminalCount(historicalIds, 0)
		cutoff = epoch.Add(48 * time.Hour)
		for turn := 0; cursor != nil && turn < 8; turn++ {
			step()
		}
		if cursor != nil {
			t.Fatal("completed finite passes failed to return to idle cadence")
		}
		requireTerminalCount(activeIds, len(activeIds))
		requireTerminalCount(laterIds, len(laterIds))
		requireTerminalCount(historicalIds, 0)
	})
}
