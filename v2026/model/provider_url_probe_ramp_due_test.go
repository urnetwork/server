// The eight-shard capacity ramp preserves durable affinity and bounded claims.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Old four-shard reservations cannot reappear during an eight-shard rollout.
func TestUrlProbeRampEightShardAffinityAndSixtyFourDueCap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now, 1024)
		seen := map[server.Id]bool{}
		for shard := range 4 {
			due := ClaimProviderUrlProbeDue(ctx, now, 1, shard, 4)
			if len(due) != 1 || seen[due[0].ClientId] {
				t.Fatal("old geometry did not reserve one distinct provider per shard")
			}
			seen[due[0].ClientId] = true
		}
		for shard := range 8 {
			want := []server.Id{}
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT client_id FROM provider_egress_probe_cycle
					WHERE slot_id%8=$1 AND next_attempt_at<=$2 ORDER BY next_attempt_at,client_id`, shard, now)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var id server.Id
						server.Raise(rows.Scan(&id))
						want = append(want, id)
					}
				})
			})
			if len(want) < 65 {
				t.Fatal("synthetic shard must exceed one sixty-four-provider claim")
			}
			received := 0
			for received < len(want) {
				due := ClaimProviderUrlProbeDue(ctx, now, 64, shard, 8)
				if len(due) != min(64, len(want)-received) {
					t.Fatalf("bounded shard claim changed: shard=%d got=%d remaining=%d", shard, len(due), len(want)-received)
				}
				for _, provider := range due {
					if provider.ClientId != want[received] || seen[provider.ClientId] ||
						!provider.CycleStartedAt.Equal(now) || provider.OutcomeCount != 0 || provider.RunsNeeded != ProviderUrlProbeRunTarget {
						t.Fatal("ramp changed slot affinity, order, lease exclusion, or durable quota identity")
					}
					seen[provider.ClientId] = true
					received++
				}
			}
			if duplicate := ClaimProviderUrlProbeDue(ctx, now, 64, shard, 8); len(duplicate) != 0 {
				t.Fatal("claimed shard returned outstanding reservations")
			}
		}
		if len(seen) != 1024 {
			t.Fatalf("ramp lost or duplicated synthetic providers: got=%d", len(seen))
		}
		for shard := range 8 {
			due := ClaimProviderUrlProbeDue(ctx, now.Add(ProviderEgressProbeAttemptBackoff), 64, shard, 8)
			if len(due) != 64 {
				t.Fatal("normal lease expiry did not refill the bounded shard")
			}
			for _, provider := range due {
				if !provider.CycleStartedAt.Equal(now) || provider.OutcomeCount != 0 || provider.RunsNeeded != ProviderUrlProbeRunTarget {
					t.Fatal("lease expiry reset or manufactured URL progress")
				}
			}
		}
	})
}
