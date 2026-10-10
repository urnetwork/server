// Observed invalid connection history must survive the writer as a zero score.
package model

import (
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Exercise raw stats, running maintenance, score publication, both common
// gates, published FP2 selection and paced URL admission. Missing history is
// an independent healthy control; no score rows are manufactured by this test.
func TestFp2ReliabilityWriterPreservesObservedZero(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		closed := server.NowUtc().Truncate(ReliabilityBlockDuration).Add(-ReliabilityBlockDuration)
		start := closed.Add(-ClientLookbacks[len(ClientLookbacks)-1])
		valid := ClientReliabilityStats{ConnectionEstablishedCount: 1, ProvideEnabledCount: 1, ReceiveMessageCount: 1}
		invalid := valid
		invalid.ReceiveMessageCount = 0
		providers := []struct {
			name          string
			client        server.Id
			network       server.Id
			wantRawValid  int64
			wantRawCount  int64
			wantAdmission bool
		}{
			{name: "healthy", wantRawValid: 721, wantRawCount: 721, wantAdmission: true},
			{name: "observed_zero", wantRawCount: 721},
			{name: "one_valid", wantRawValid: 1, wantRawCount: 721},
			{name: "missing", wantAdmission: true},
		}
		for index := range providers {
			provider := &providers[index]
			provider.client, provider.network = server.NewId(), server.NewId()
			address := testingConnectClientWithLocation(ctx, t, provider.network, provider.client,
				fmt.Sprintf("192.0.2.%d:0", index+1), city)
			SetProvide(ctx, provider.client, egressTestPublicAndNetwork)
			egressTestHealth(ctx, provider.client, closed, 3, 0)
			switch provider.name {
			case "healthy":
				AddClientReliabilityStatsRange(ctx, provider.network, provider.client, address, start, closed, &valid)
			case "observed_zero":
				AddClientReliabilityStatsRange(ctx, provider.network, provider.client, address, start, closed, &invalid)
			case "one_valid":
				AddClientReliabilityStatsRange(ctx, provider.network, provider.client, address, start, closed.Add(-ReliabilityBlockDuration), &invalid)
				AddClientReliabilityStats(ctx, provider.network, provider.client, address, closed, &valid)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var count, validCount int64
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*),COUNT(*) FILTER(WHERE valid)
					FROM client_reliability WHERE client_id=$1`, provider.client).Scan(&count, &validCount))
				if count != provider.wantRawCount || validCount != provider.wantRawValid {
					t.Fatalf("%s raw fixture: rows=%d valid=%d", provider.name, count, validCount)
				}
			})
		}
		// The supported fixture writer has no coverage gaps or degraded blocks;
		// its inclusive closed windows contain exactly 6, 61 and 721 minutes.
		UpdateClientReliabilityScores(ctx, closed, true)
		scores := GetAllClientReliabilityScores(ctx)
		for _, provider := range providers {
			weights := map[int]float64{}
			for lookback, duration := range ClientLookbacks {
				score, exists := scores[lookback][provider.client]
				if exists {
					weights[lookback] = score.IndependentReliabilityWeight
				}
				if provider.name == "missing" {
					if exists {
						t.Errorf("missing history acquired a lookback %d score", lookback)
					}
					continue
				}
				want := 0.0
				if provider.name == "healthy" {
					want = 1
				} else if provider.name == "one_valid" {
					want = 1 / float64(duration/ReliabilityBlockDuration+1)
				}
				if !exists || score.IndependentReliabilityWeight != want {
					t.Errorf("%s lookback %d: score_present=%t weight=%g want=%g",
						provider.name, lookback, exists, score.IndependentReliabilityWeight, want)
				}
			}
			goAdmission := providerReliabilityPasses(weights, providerReliabilityMinimums())
			var sqlAdmission bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, "SELECT "+providerReliabilityEligibilitySql("$1::uuid"), provider.client).Scan(&sqlAdmission))
			})
			if goAdmission != provider.wantAdmission || sqlAdmission != provider.wantAdmission {
				t.Errorf("%s common reliability admission: Go=%t SQL=%t want=%t",
					provider.name, goAdmission, sqlAdmission, provider.wantAdmission)
			}
		}
		egressTestPasses(ctx, t)
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			for _, forced := range []bool{false, true} {
				found := egressTestFind(ctx, t, []*ProviderSpec{{LocationId: &city.LocationId}}, mode, len(providers), forced, server.NewId())
				seen := map[server.Id]bool{}
				for _, provider := range found {
					seen[provider.ClientId] = true
				}
				for _, provider := range providers {
					if seen[provider.client] != provider.wantAdmission {
						t.Errorf("%s location admission: mode=%s forced=%t admitted=%t want=%t",
							provider.name, mode, forced, seen[provider.client], provider.wantAdmission)
					}
					explicit := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &provider.client}}, mode, 1, forced, server.NewId())
					if (len(explicit) > 0) != provider.wantAdmission {
						t.Errorf("%s explicit admission: mode=%s forced=%t admitted=%t want=%t",
							provider.name, mode, forced, len(explicit) > 0, provider.wantAdmission)
					}
				}
			}
		}
		due := ClaimProviderUrlProbeDue(ctx, server.NowUtc().Add(time.Second), len(providers), 0, 1)
		seenDue := map[server.Id]bool{}
		for _, provider := range due {
			seenDue[provider.ClientId] = true
		}
		for _, provider := range providers {
			if seenDue[provider.client] != provider.wantAdmission {
				t.Errorf("%s URL admission: admitted=%t want=%t", provider.name, seenDue[provider.client], provider.wantAdmission)
			}
		}
	})
}

// Losing the last valid minute must leave a measured zero until the final
// invalid observation expires. Rolling and full recomputation must agree.
func TestReliabilityObservedZeroRollingAndExpiry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		network, failed, healthy := server.NewId(), server.NewId(), server.NewId()
		address := testingConnectClientWithLocation(ctx, t, network, failed, "192.0.2.17:1", city)
		testingConnectClientWithLocation(ctx, t, network, healthy, "192.0.2.17:2", city)
		closed := server.NowUtc().Truncate(ReliabilityBlockDuration).Add(-ReliabilityBlockDuration)
		start := closed.Add(-ClientLookbacks[0])
		valid := ClientReliabilityStats{ConnectionEstablishedCount: 1, ProvideEnabledCount: 1, ReceiveMessageCount: 1}
		invalid := valid
		invalid.ReceiveMessageCount = 0
		AddClientReliabilityStats(ctx, network, failed, address, start, &valid)
		AddClientReliabilityStatsRange(ctx, network, failed, address, start.Add(ReliabilityBlockDuration), closed, &invalid)
		AddClientReliabilityStatsRange(ctx, network, healthy, address, start, closed, &valid)
		UpdateClientReliabilityScores(ctx, closed, true)
		first := GetAllClientReliabilityScores(ctx)[0][failed]
		if first.IndependentReliabilityScore != 1 || first.IndependentReliabilityWeight != 1.0/6 {
			t.Fatalf("initial observed history lost its one valid minute: %+v", first)
		}
		next := closed.Add(ReliabilityBlockDuration)
		AddClientReliabilityStats(ctx, network, failed, address, next, &invalid)
		AddClientReliabilityStats(ctx, network, healthy, address, next, &valid)
		UpdateClientReliabilityScores(ctx, next, false)
		window := testingReadRunningWindow(ctx, 0)
		if window.lastRecomputeBlock >= window.maxBlockNumber {
			t.Fatal("observation maintenance did not exercise the incremental path")
		}
		assertZero := func() {
			scores := GetAllClientReliabilityScores(ctx)[0]
			score, present := scores[failed]
			if !present || score.IndependentReliabilityScore != 0 || score.IndependentReliabilityWeight != 0 || score.ReliabilityWeight != 0 {
				t.Fatalf("last valid minute became missing instead of zero: present=%t score=%+v", present, score)
			}
			if control := scores[healthy]; control.IndependentReliabilityWeight != 1 || control.ReliabilityWeight != 1 {
				t.Fatalf("invalid same-IP observations diluted the healthy numerator: %+v", control)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var count int64
				server.Raise(conn.QueryRow(ctx, `SELECT observed_row_count FROM client_reliability_running
					WHERE client_id=$1 AND lookback_index=0`, failed).Scan(&count))
				if count != 6 {
					t.Fatalf("rolling observed count=%d, want6", count)
				}
			})
		}
		assertZero()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE client_reliability_running_window
				SET last_recompute_block=last_recompute_block-$1`, ReliabilityRunningRecomputeBlocks+1))
		})
		UpdateClientReliabilityScores(ctx, next, false)
		assertZero()
		UpdateClientReliabilityScores(ctx, next.Add(ClientLookbacks[0]+ReliabilityBlockDuration), false)
		if _, present := GetAllClientReliabilityScores(ctx)[0][failed]; present {
			t.Fatal("expired invalid observations prevented return to missing history")
		}
	})
}

// An old writer can keep the degraded-block token current while silently
// dropping zero-positive rows. The separate observation token must expose it.
func TestReliabilityObservedZeroRejectsLegacyCheckpoint(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		network, client := server.NewId(), server.NewId()
		address := testingConnectClientWithLocation(ctx, t, network, client, "192.0.2.18:1", city)
		closed := server.NowUtc().Truncate(ReliabilityBlockDuration).Add(-ReliabilityBlockDuration)
		AddClientReliabilityStats(ctx, network, client, address, closed,
			&ClientReliabilityStats{ConnectionEstablishedCount: 1, ProvideEnabledCount: 1})
		UpdateClientReliabilityScores(ctx, closed, true)
		current := testingReadRunningWindow(ctx, 0)
		if !reliabilityRunningObservationCurrent(current) {
			t.Fatal("current writer did not attest observation-aware maintenance")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM client_reliability_running WHERE lookback_index=0 AND independent_sum<0.5`))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE client_reliability_running_window
				SET degraded_classification_write_token=gen_random_uuid() WHERE lookback_index=0`))
		})
		legacy := testingReadRunningWindow(ctx, 0)
		if legacy.degradedClassificationVersion != reliabilityDegradedClassificationVersion ||
			!legacy.degradedClassificationWriteTokenPresent || reliabilityRunningObservationCurrent(legacy) {
			t.Fatalf("legacy writer was not distinguished from current observation maintenance: %+v", legacy)
		}
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			updateClientReliabilityRunningLookbackAtBoundsInTx(tx, ctx,
				reliabilityRunningLookback{lookbackIndex: 0, lookback: ClientLookbacks[0]},
				current.minBlockNumber, current.maxBlockNumber, false)
		})
		if !reliabilityRunningObservationCurrent(testingReadRunningWindow(ctx, 0)) {
			t.Fatal("mandatory observation repair was deferred by maintenance pressure")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int64
			var positive float64
			server.Raise(conn.QueryRow(ctx, `SELECT observed_row_count,independent_sum
				FROM client_reliability_running WHERE client_id=$1 AND lookback_index=0`, client).Scan(&count, &positive))
			if count != 1 || positive != 0 {
				t.Fatalf("mandatory repair fabricated or lost observed zero: count=%d positive=%g", count, positive)
			}
		})
	})
}

func TestReliabilityObservedZeroClearsSharedIpRoundoff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		network, client := server.NewId(), server.NewId()
		address := testingConnectClientWithLocation(ctx, t, network, client, "192.0.2.19:1", city)
		closed := server.NowUtc().Truncate(ReliabilityBlockDuration).Add(-ReliabilityBlockDuration)
		start := closed.Add(-ClientLookbacks[0])
		valid := ClientReliabilityStats{ConnectionEstablishedCount: 1, ProvideEnabledCount: 1, ReceiveMessageCount: 1}
		invalid := valid
		invalid.ReceiveMessageCount = 0
		AddClientReliabilityStatsRange(ctx, network, client, address, start, start.Add(2*ReliabilityBlockDuration), &valid)
		AddClientReliabilityStatsRange(ctx, network, client, address, start.Add(3*ReliabilityBlockDuration), closed.Add(3*ReliabilityBlockDuration), &invalid)
		for index := range 2 {
			peer := server.NewId()
			testingConnectClientWithLocation(ctx, t, network, peer, fmt.Sprintf("192.0.2.19:%d", index+2), city)
			AddClientReliabilityStatsRange(ctx, network, peer, address, start, closed.Add(3*ReliabilityBlockDuration), &valid)
		}
		UpdateClientReliabilityScores(ctx, closed, true)
		initial := GetAllClientReliabilityScores(ctx)[0][client]
		if initial.IndependentReliabilityScore != 3 || initial.ReliabilityScore != 1 {
			t.Fatalf("shared-IP control: %+v", initial)
		}
		// Sequential subtraction of three float8 thirds can leave a residue.
		// Observed-invalid rows now survive and therefore must carry exact zero.
		for minute := 1; minute <= 3; minute++ {
			UpdateClientReliabilityScores(ctx, closed.Add(time.Duration(minute)*ReliabilityBlockDuration), false)
		}
		score, present := GetAllClientReliabilityScores(ctx)[0][client]
		if !present || score.IndependentReliabilityScore != 0 || score.ReliabilityScore != 0 || score.ReliabilityWeight != 0 {
			t.Fatalf("last shared-IP contribution did not become exact observed zero: present=%t score=%+v", present, score)
		}
	})
}
