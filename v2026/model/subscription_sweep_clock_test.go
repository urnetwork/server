package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// sweep_time is a timestamp without zone on the UTC storage clock. Run the
// actual payout INSERT under non-UTC sessions, including its conflict path.
func TestParticipantSweepTimestampUsesUTC(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, zone := range []string{"UTC", "America/Los_Angeles", "Asia/Tokyo"} {
			contractId, balanceId, networkId, destinationId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('TimeZone',$1,true)`, zone))
				server.RaisePgResult(tx.Exec(ctx, participantSweepInsertSQL, contractId, balanceId, networkId, 1024, 17000, destinationId, nil))
				var offsetSeconds float64
				var recent NanoCents
				server.Raise(tx.QueryRow(ctx, `
					SELECT extract(epoch FROM (sweep_time-(current_timestamp AT TIME ZONE 'UTC')))::float8,
						CASE WHEN sweep_time >= $4 THEN payout_net_revenue_nano_cents ELSE 0 END
					FROM transfer_escrow_sweep WHERE contract_id=$1 AND balance_id=$2 AND network_id=$3
				`, contractId, balanceId, networkId, server.NowUtc().Add(-time.Hour)).Scan(&offsetSeconds, &recent))
				if offsetSeconds != 0 || recent != 17000 {
					t.Errorf("zone=%s sweep UTC offset=%.0fs recent payout=%d", zone, offsetSeconds, recent)
				}
				// Replaying captured metadata must preserve the original payout
				// clock, even when today's transaction has a different timestamp.
				original := server.NowUtc().Add(-20 * time.Minute).Truncate(time.Microsecond)
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow_sweep SET sweep_time=$4 WHERE contract_id=$1 AND balance_id=$2 AND network_id=$3`, contractId, balanceId, networkId, original))
				server.RaisePgResult(tx.Exec(ctx, participantSweepInsertSQL, contractId, balanceId, networkId, 1024, 17000, destinationId, nil))
				var replayTime time.Time
				var amount NanoCents
				var rows int
				server.Raise(tx.QueryRow(ctx, `SELECT min(sweep_time),sum(payout_net_revenue_nano_cents),count(*) FROM transfer_escrow_sweep WHERE contract_id=$1 AND balance_id=$2 AND network_id=$3`, contractId, balanceId, networkId).Scan(&replayTime, &amount, &rows))
				if !replayTime.Equal(original) || amount != 17000 || rows != 1 {
					t.Errorf("zone=%s replay changed payout clock, amount or cardinality", zone)
				}
			})
		}
	})
}
