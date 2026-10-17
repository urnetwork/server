package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The maintenance writer stores a timestamp without zone. Exercise the same
// batch SQL under different session zones without changing its retention clock,
// protected-payment predicate, strict cutoff, row bound or replay behavior.
func TestStragglerReapTimestampUsesUTC(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			for _, zone := range []string{"UTC", "America/Los_Angeles", "Asia/Tokyo"} {
				server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('TimeZone',$1,true)`, zone))
				var clock time.Time
				server.Raise(tx.QueryRow(ctx, `SELECT now() AT TIME ZONE 'UTC'`).Scan(&clock))
				cutoff := clock.Add(-StragglerContractExpiration)
				networkId := server.NewId()
				usage := &contractUsageSnapshot{Version: 1, ByteCount: 1024,
					Providers: []contractProviderUsage{{ClientId: networkId, NetworkId: networkId, ByteCount: 1024}}}
				insert := func(created time.Time, closed bool, reaped *time.Time) server.Id {
					id := server.NewId()
					var closeTime *time.Time
					var outcome *ContractOutcome
					if closed {
						closeTime = &created
						settled := ContractOutcomeSettled
						outcome = &settled
					}
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract (
						contract_id,source_network_id,source_id,destination_network_id,destination_id,
						transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source,reap_time
					) VALUES ($1,$2,$2,$2,$2,1024,$3,$4,$5,$6,TRUE,$7)`,
						id, networkId, created.UTC(), closeTime, outcome, usage, reaped))
					return id
				}
				eligible := []server.Id{
					insert(cutoff.Add(-3*time.Hour), true, nil),
					insert(cutoff.Add(-2*time.Hour), true, nil),
					insert(cutoff.Add(-time.Hour), true, nil),
				}
				originalReap := clock.Add(time.Hour)
				alreadyReaped := insert(cutoff.Add(-time.Hour), true, &originalReap)
				protected := []server.Id{
					insert(cutoff.Add(-time.Hour), false, nil),
					insert(cutoff.Add(time.Hour), true, nil),
					insert(cutoff, true, nil), // the eligibility boundary is strict
				}
				held := insert(cutoff.Add(-time.Hour), true, nil)
				protected = append(protected, held)
				paymentId := server.NewId()
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment (
					payment_id,payment_plan_id,network_id,payout_byte_count,payout_nano_cents,min_sweep_time,
					completed,contract_retention_pending
				) VALUES ($1,$2,$3,1024,1,$4,TRUE,TRUE)`, paymentId, server.NewId(), networkId, cutoff.UTC()))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep (
					contract_id,balance_id,network_id,payout_byte_count,payout_net_revenue_nano_cents,payment_id
				) VALUES ($1,$2,$3,1024,1,$4)`, held, server.NewId(), networkId, paymentId))
				for _, want := range []int64{2, 1, 0} {
					got := server.RaisePgResult(tx.Exec(ctx, assignStragglerReapTimeSQL, cutoff.UTC(), 2)).RowsAffected()
					if got != want {
						t.Fatalf("zone=%s assigned=%d, want bounded batch %d", zone, got, want)
					}
				}
				for _, id := range eligible {
					var reaped time.Time
					server.Raise(tx.QueryRow(ctx, `SELECT reap_time FROM transfer_contract WHERE contract_id=$1`, id).Scan(&reaped))
					if !reaped.Equal(clock) {
						t.Errorf("zone=%s reap timestamp offset from UTC transaction clock=%s", zone, reaped.Sub(clock))
					}
				}
				var protectedStamped int
				server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND reap_time IS NOT NULL`, protected).Scan(&protectedStamped))
				var preserved time.Time
				server.Raise(tx.QueryRow(ctx, `SELECT reap_time FROM transfer_contract WHERE contract_id=$1`, alreadyReaped).Scan(&preserved))
				if protectedStamped != 0 || !preserved.Equal(originalReap) {
					t.Fatalf("zone=%s retention protection or original reap timestamp changed", zone)
				}
			}
		})
	})
}
