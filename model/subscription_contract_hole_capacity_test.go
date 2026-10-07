package model

// This bounded real-source profile measures refresh capacity locally. It is not
// a Main fleet estimate: rollout still needs its actual cohort and task latency.

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Two complete passes refresh 128 ordinary pairs plus a heavy pair. Explicitly
// aging Redis TTLs to half lifetime drives the cadence boundary without sleeps.
func TestContractHoleRefreshLargeCohortHalfTtlCoverage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		type pair struct {
			source, destination server.Id
			members             int64
		}
		pairs := make([]pair, 129)
		for index := range pairs {
			members := int64(64)
			if index == len(pairs)-1 {
				members = 4096
			}
			pairs[index] = pair{source: server.NewId(), destination: server.NewId(), members: members}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			for _, pair := range pairs {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count)
 SELECT gen_random_uuid(),$1,$2,$3,$4,0 FROM generate_series(1,$5)`,
					server.NewId(), pair.source, server.NewId(), pair.destination, pair.members))
			}
		})
		for pass := range 2 {
			if pass != 0 {
				server.Redis(ctx, func(client server.RedisClient) {
					for _, pair := range pairs {
						for _, key := range contractHoleKeys(pair.source, pair.destination)[:2] {
							server.Raise(client.PExpire(ctx, key, ContractHoleRefreshInterval).Err())
						}
					}
				})
			}
			receipt := &ContractHoleReadiness{Version: 1, PassStarted: server.NowUtc()}
			var cursor *ContractHoleCursor
			for {
				page, err := RefreshContractHolesPage(ctx, cursor)
				if err != nil || page == nil {
					t.Fatalf("pass=%d candidate page=%d error=%v", pass, receipt.Pages, err)
				}
				receipt.Pages++
				receipt.PairVisits += page.Pairs
				receipt.SuccessfulPairs += page.Pairs - page.FailedPairs
				receipt.UnknownPairs += page.FailedPairs
				receipt.PositivePairs += page.PositivePairs
				if page.EarliestPositive != nil && (receipt.EarliestPositive == nil || page.EarliestPositive.SourceStarted.Before(receipt.EarliestPositive.SourceStarted)) {
					receipt.EarliestPositive = page.EarliestPositive
				}
				if page.Cursor == nil {
					break
				}
				if receipt.Pages > 49 {
					t.Fatal("candidate cursor did not finish the fixed cohort")
				}
				cursor = page.Cursor
			}
			receipt.PassCompleted = server.NowUtc()
			ready, err := PublishContractHoleReadiness(ctx, receipt)
			if err != nil || !ready || receipt.Pages != 49 || receipt.PairVisits < len(pairs) || receipt.UnknownPairs != 0 {
				t.Fatalf("cohort readiness=%t error=%v receipt=%+v", ready, err, receipt)
			}
			minimum := ContractHoleTtl
			server.Redis(ctx, func(client server.RedisClient) {
				for _, pair := range pairs {
					keys := contractHoleKeys(pair.source, pair.destination)
					count, err := client.Get(ctx, keys[0]).Int64()
					if err != nil || count != pair.members {
						t.Fatalf("pair count=%d want=%d error=%v", count, pair.members, err)
					}
					members, err := client.ZCard(ctx, keys[1]).Result()
					if err != nil || members != count {
						t.Fatalf("pair membership=%d count=%d error=%v", members, count, err)
					}
					for _, key := range keys[:2] {
						ttl, err := client.PTTL(ctx, key).Result()
						if err != nil || ttl <= ContractHoleRefreshInterval {
							t.Fatalf("pair missed half-ttl margin: ttl=%s error=%v", ttl, err)
						}
						minimum = min(minimum, ttl)
					}
				}
			})
			observed, err := ReadContractHoleReadiness(ctx)
			if err != nil || observed == nil || observed.MinimumRemainingTtlMillis <= ContractHoleRefreshInterval.Milliseconds() {
				t.Fatalf("earliest witness failed after census: receipt=%+v error=%v", observed, err)
			}
			t.Logf("contract-hole local capacity: pass=%d distinct_pairs=%d source_contracts=%d pages=%d pair_visits=%d successful=%d unknown=%d duration=%s observed_min_ttl=%s conservative_remaining=%s earliest_witness_remaining=%s",
				pass, len(pairs), 128*64+4096, receipt.Pages, receipt.PairVisits, receipt.SuccessfulPairs, receipt.UnknownPairs,
				receipt.PassCompleted.Sub(receipt.PassStarted), minimum,
				time.Duration(observed.MinimumRemainingTtlMillis)*time.Millisecond, time.Duration(observed.WitnessRemainingTtlMillis)*time.Millisecond)
		}
	})
}
