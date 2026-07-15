package model

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// testingConnectClientWithLocation connects clientId from clientAddress with the
// given location and returns the client_address_hash. Two clients connected from
// the same ip (any port) share the hash, which drives valid_client_count > 1.
func testingConnectClientWithLocation(
	ctx context.Context,
	t testing.TB,
	networkId server.Id,
	clientId server.Id,
	clientAddress string,
	location *Location,
) [32]byte {
	Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
	connectionId, _, _, clientAddressHash, err := ConnectNetworkClient(ctx, clientId, clientAddress, server.NewId())
	connect.AssertEqual(t, err, nil)
	err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
	connect.AssertEqual(t, err, nil)
	return clientAddressHash
}

// testingSnapshotClientScores flattens client_connection_reliability_score into
// a map keyed by "lookback:client".
func testingSnapshotClientScores(ctx context.Context) map[string]ReliabilityScore {
	out := map[string]ReliabilityScore{}
	for lookbackIndex, clientScores := range GetAllClientReliabilityScores(ctx) {
		for clientId, s := range clientScores {
			out[fmt.Sprintf("%d:%s", lookbackIndex, clientId)] = s
		}
	}
	return out
}

// testingSnapshotWindowScores reads network_connection_reliability_window_score
// into a map keyed by "network:country".
func testingSnapshotWindowScores(ctx context.Context) map[string]ReliabilityScore {
	out := map[string]ReliabilityScore{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_id,
				country_location_id,
				independent_reliability_score,
				independent_reliability_weight,
				reliability_score,
				reliability_weight
			FROM network_connection_reliability_window_score
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var countryLocationId server.Id
				var s ReliabilityScore
				server.Raise(result.Scan(
					&networkId,
					&countryLocationId,
					&s.IndependentReliabilityScore,
					&s.IndependentReliabilityWeight,
					&s.ReliabilityScore,
					&s.ReliabilityWeight,
				))
				out[fmt.Sprintf("%s:%s", networkId, countryLocationId)] = s
			}
		})
	})
	return out
}

func testingReadRunningWindow(ctx context.Context, lookbackIndex int) (w reliabilityRunningWindow) {
	server.Tx(ctx, func(tx server.PgTx) {
		w = readReliabilityRunningWindow(ctx, tx, lookbackIndex)
	})
	return
}

func testingAssertScoreEq(t testing.TB, label string, a ReliabilityScore, b ReliabilityScore) {
	const eps = 1e-9
	closeEq := func(x float64, y float64) bool {
		return math.Abs(x-y) <= eps
	}
	if !closeEq(a.IndependentReliabilityScore, b.IndependentReliabilityScore) ||
		!closeEq(a.IndependentReliabilityWeight, b.IndependentReliabilityWeight) ||
		!closeEq(a.ReliabilityScore, b.ReliabilityScore) ||
		!closeEq(a.ReliabilityWeight, b.ReliabilityWeight) {
		t.Fatalf(
			"%s mismatch:\n rolling=%+v\n recompute=%+v\n",
			label, a, b,
		)
	}
}

func testingAssertScoresEquivalent(
	t testing.TB,
	label string,
	rolling map[string]ReliabilityScore,
	recompute map[string]ReliabilityScore,
) {
	// same set of keys
	if len(rolling) != len(recompute) {
		t.Fatalf("%s: key count differs rolling=%d recompute=%d\n rolling=%v\n recompute=%v\n", label, len(rolling), len(recompute), rolling, recompute)
	}
	for key, r := range rolling {
		c, ok := recompute[key]
		if !ok {
			t.Fatalf("%s: key %s present in rolling but missing in recompute\n", label, key)
		}
		testingAssertScoreEq(t, label+" "+key, r, c)
	}
	for key := range recompute {
		if _, ok := rolling[key]; !ok {
			t.Fatalf("%s: key %s present in recompute but missing in rolling\n", label, key)
		}
	}
}

// TestClientReliabilityRunningRollingEquivalence is the correctness proof for the
// rolling incremental reliability-score maintenance: driving the running sums
// forward block-by-block (the ROLLING path) produces the exact same client
// scores (#1) and network-window scores (#3) as a single FULL RECOMPUTE of the
// same final window. It also proves the add-then-subtract cancellation: a client
// whose only reported block has slid out of the short window (via the SUBTRACT
// path during a roll) is gone, exactly as a recompute that never counted it.
func TestClientReliabilityRunningRollingEquivalence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// prod defaults to recompute-every-cycle (the INCLUDE index made the
		// full recompute ~10s, so no drift can accumulate); force a long
		// rolling horizon here so this test still exercises and proves the
		// rolling add/subtract path
		defer func(prev int64) { ReliabilityRunningRecomputeBlocks = prev }(ReliabilityRunningRecomputeBlocks)
		ReliabilityRunningRecomputeBlocks = int64(4 * time.Hour / ReliabilityBlockDuration)

		us := &Location{
			City:        "us_city",
			Region:      "us_region",
			Country:     "United States",
			CountryCode: "us",
		}
		ca := &Location{
			City:        "ca_city",
			Region:      "ca_region",
			Country:     "Canada",
			CountryCode: "ca",
		}
		CreateLocation(ctx, us)
		CreateLocation(ctx, ca)

		networkA := server.NewId()
		networkB := server.NewId()

		clientA1 := server.NewId()
		clientA2 := server.NewId()
		clientA3 := server.NewId()
		clientB1 := server.NewId()

		// A1 and A2 share one ip -> valid_client_count = 2 (each contributes 1/2
		// per block). A3 is alone on its ip. B1 is a different network/country.
		hashA12 := testingConnectClientWithLocation(ctx, t, networkA, clientA1, "10.1.1.1:20001", us)
		testingConnectClientWithLocation(ctx, t, networkA, clientA2, "10.1.1.1:20002", us)
		hashA3 := testingConnectClientWithLocation(ctx, t, networkA, clientA3, "10.3.3.3:20003", us)
		hashB1 := testingConnectClientWithLocation(ctx, t, networkB, clientB1, "10.2.2.2:20004", ca)

		stats := &ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        1,
			ReceiveByteCount:           1024,
			SendMessageCount:           1,
			SendByteCount:              1024,
		}

		// K blocks of data. A1, A2, B1 report in every block; A3 reports only in
		// block a3Block, so it enters the short window during the rolling drive and
		// then slides out (exercising the subtract path).
		const k = 24
		const a3Block = 10
		startTime := server.NowUtc()
		blockTime := func(i int) time.Time {
			return startTime.Add(time.Duration(i) * ReliabilityBlockDuration)
		}

		for i := 0; i < k; i += 1 {
			AddClientReliabilityStats(ctx, networkA, clientA1, hashA12, blockTime(i), stats)
			AddClientReliabilityStats(ctx, networkA, clientA2, hashA12, blockTime(i), stats)
			AddClientReliabilityStats(ctx, networkB, clientB1, hashB1, blockTime(i), stats)
		}
		AddClientReliabilityStats(ctx, networkA, clientA3, hashA3, blockTime(a3Block), stats)

		// populate network_client_location_reliability for the connected clients
		// (the score writers join it at write time for location; it is maintained
		// separately from the score/running tables, exactly as UpdateReliabilities
		// does in production). All four clients are connected with one ip and one
		// location, so all four are location-valid.
		UpdateClientLocationReliabilities(ctx, startTime, blockTime(k))

		// ROLLING: drive maxTime forward one block at a time. The first call (no
		// prior running window) recomputes; every later call rolls the window
		// forward by the entering/leaving blocks. The short lookback (5 blocks)
		// slides across the data, so A3's single block enters and then leaves.
		const firstM = a3Block // first window already contains A3's block
		const lastM = k - 1
		for m := firstM; m <= lastM; m += 1 {
			maxTime := blockTime(m)
			UpdateClientReliabilityScores(ctx, maxTime, false)
			UpdateNetworkReliabilityWindowScores(ctx, maxTime, false)
		}
		finalMaxTime := blockTime(lastM)

		// Prove the drive actually ROLLED (did not just recompute every step): the
		// last-recompute marker for the short lookback must still sit at the first
		// call's max, well below the current window max.
		lookback0Window := testingReadRunningWindow(ctx, 0)
		connect.AssertEqual(t, lookback0Window.exists, true)
		if !(lookback0Window.lastRecomputeBlock < lookback0Window.maxBlockNumber) {
			t.Fatalf(
				"expected rolling (lastRecomputeBlock < maxBlockNumber), got window=%+v -- the drive recomputed every step and did not exercise the rolling path",
				lookback0Window,
			)
		}

		rollingClientScores := testingSnapshotClientScores(ctx)
		rollingWindowScores := testingSnapshotWindowScores(ctx)

		// A3's only block (block 10) has slid out of the short (5-block) window by
		// the final step, so it must be gone from lookback 0 -- removed by the
		// SUBTRACT path during a roll, not just excluded by a recompute. It is
		// still inside the 60-block lookback 1, so it must still have a score there.
		if _, ok := rollingClientScores[fmt.Sprintf("0:%s", clientA3)]; ok {
			t.Fatalf("clientA3 should have left the short lookback-0 window (add-then-subtract must cancel), but a score row remains")
		}
		if _, ok := rollingClientScores[fmt.Sprintf("1:%s", clientA3)]; !ok {
			t.Fatalf("clientA3 should still be inside the longer lookback-1 window")
		}

		// FORCE FULL RECOMPUTE of the same final window: push the recompute marker
		// far enough back that the next maintenance re-anchors every lookback with a
		// fresh full scan of [min, max) instead of rolling.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE client_reliability_running_window SET last_recompute_block = last_recompute_block - $1`,
				ReliabilityRunningRecomputeBlocks+1000,
			))
		})
		UpdateClientReliabilityScores(ctx, finalMaxTime, false)
		UpdateNetworkReliabilityWindowScores(ctx, finalMaxTime, false)

		// the marker moved forward again to the window max -> that pass recomputed
		recomputedWindow := testingReadRunningWindow(ctx, 0)
		connect.AssertEqual(t, recomputedWindow.lastRecomputeBlock, recomputedWindow.maxBlockNumber)

		recomputeClientScores := testingSnapshotClientScores(ctx)
		recomputeWindowScores := testingSnapshotWindowScores(ctx)

		// EQUIVALENCE: rolling == recompute for both score tables.
		testingAssertScoresEquivalent(t, "client scores", rollingClientScores, recomputeClientScores)
		testingAssertScoresEquivalent(t, "network window scores", rollingWindowScores, recomputeWindowScores)

		// sanity on the actual values of the final short window: A1 and B1 both
		// reported in every block of the window, so their independent counts match;
		// A1 shares its ip with A2 (valid_client_count 2) so its per-block
		// reliability is exactly 1/2 of independent, while B1 is alone so its
		// reliability equals its independent count. 1.0 and 0.5 are exactly
		// representable, so rolling and recompute agree bit-for-bit.
		eps := 1e-9
		a1 := recomputeClientScores[fmt.Sprintf("0:%s", clientA1)]
		b1 := recomputeClientScores[fmt.Sprintf("0:%s", clientB1)]
		if a1.IndependentReliabilityScore <= 0 {
			t.Fatalf("clientA1 lookback0 has no data: %+v", a1)
		}
		if math.Abs(a1.IndependentReliabilityScore-b1.IndependentReliabilityScore) > eps {
			t.Fatalf("clientA1 and clientB1 reported every block, so independent counts should match: a1=%+v b1=%+v", a1, b1)
		}
		if math.Abs(a1.ReliabilityScore-a1.IndependentReliabilityScore/2) > eps {
			t.Fatalf("clientA1 shares its ip so reliability should be half of independent: %+v", a1)
		}
		if math.Abs(b1.ReliabilityScore-b1.IndependentReliabilityScore) > eps {
			t.Fatalf("clientB1 is alone on its ip so reliability should equal independent: %+v", b1)
		}
	})
}
