package model

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// How long an award test's window stays inside client_reliability retention
// on the wall clock after the test starts: the award checks retention again on
// the clock after its reads.
const operatorBlockPointsTestRetentionMargin = time.Hour

// The completed operator block an award test computes at the wall clock now,
// the clock its award runs at, and the earning boundary (cutoff) the test
// sets. The block is the one before the open one, due once its last
// settlements are in (OperatorBlockPointsSettleTime), so an award run within
// that time of the block's end runs at its end. The cutoff is two days before
// the block, unless that would put the block's window past
// client_reliability retention at the award's clock or on the wall clock,
// where the award checks it again: retention is 10 days
// (ClientReliabilityRetention), under two blocks, so the whole block is inside
// it only in the open block's first three days. Later the cutoff moves into
// the block, operatorBlockPointsTestRetentionMargin inside retention, and the
// block's window is its traffic past the cutoff.
func operatorBlockPointsTestBlock(now time.Time) (block int64, awardNow time.Time, cutoff time.Time) {
	block = int64(SubnetBlockNumber(now)) - 1
	settled := operatorBlockStart(block + 1).Add(OperatorBlockPointsSettleTime)
	awardNow = server.MaxTime(now, settled)
	cutoff = operatorBlockStart(block).Add(-2 * 24 * time.Hour)
	// the oldest reliability block retained on the wall clock for the margin
	if retainedFrom := now.Add(-ClientReliabilityRetention + operatorBlockPointsTestRetentionMargin).Truncate(ReliabilityBlockDuration); operatorBlockStart(block).Before(retainedFrom) {
		cutoff = retainedFrom
	}
	return block, awardNow, cutoff
}

// When an award test places the block's traffic: a day into the block, or an
// hour past a cutoff inside it, so the window always holds it.
func operatorBlockPointsTestTrafficTime(block int64, cutoff time.Time) time.Time {
	return server.MaxTime(operatorBlockStart(block).Add(24*time.Hour), cutoff.Add(time.Hour))
}

// Pure: at any wall clock in the open block, the award tests' block is due at
// its award's clock, which is at most the settle time ahead; its window past
// the cutoff is inside retention there and on the wall clock for the margin,
// and holds the tests' traffic (the third contract is two hours after the
// first) with at least two days of traffic time; the block before it is past
// retention. While the whole block is retained, the cutoff stays two days
// before it, as the tests had it with 14 days of retention.
func TestOperatorBlockPointsTestBlockIsAwardable(t *testing.T) {
	open := operatorBlockStart(20)
	walls := []time.Time{
		open.Add(time.Minute),
		open.Add(OperatorBlockPointsSettleTime - time.Minute),
		open.Add(OperatorBlockPointsSettleTime),
	}
	for offset := time.Duration(0); offset < SubnetBlockDuration; offset += 17 * time.Minute {
		walls = append(walls, open.Add(offset))
	}
	walls = append(walls, open.Add(3*24*time.Hour), open.Add(SubnetBlockDuration-time.Minute), open.Add(SubnetBlockDuration-time.Nanosecond))
	for _, wall := range walls {
		block, awardNow, cutoff := operatorBlockPointsTestBlock(wall)
		start, end, ok := OperatorBlockPointsWindow(block, cutoff)
		minBlockNumber, _ := operatorBlockReliabilityBlockWindow(start, end)
		previousStart, previousEnd, _ := OperatorBlockPointsWindow(block-1, time.Time{})
		previousMinBlockNumber, _ := operatorBlockReliabilityBlockWindow(previousStart, previousEnd)
		traffic := operatorBlockPointsTestTrafficTime(block, cutoff)
		wholeBlockRetained := operatorBlockReliabilityRetained(reliabilityBlockNumber(operatorBlockStart(block)), wall.Add(operatorBlockPointsTestRetentionMargin))
		switch {
		case block != 19:
			t.Errorf("at %s: block %d, want 19", wall, block)
		case awardNow.Before(wall) || OperatorBlockPointsSettleTime < awardNow.Sub(wall):
			t.Errorf("at %s: the award runs at %s", wall, awardNow)
		case !ok || !slices.Contains(OperatorBlockPointsDueBlocks(cutoff, awardNow), block):
			t.Errorf("at %s: block %d is not due at %s past the cutoff %s", wall, block, awardNow, cutoff)
		case !operatorBlockReliabilityRetained(minBlockNumber, awardNow) || !operatorBlockReliabilityRetained(minBlockNumber, wall.Add(operatorBlockPointsTestRetentionMargin)):
			t.Errorf("at %s: block %d past the cutoff %s is past retention", wall, block, cutoff)
		case traffic.Before(start) || !traffic.Add(2*time.Hour).Before(end) || end.Sub(start) < 2*24*time.Hour:
			t.Errorf("at %s: the window [%s, %s) does not hold the traffic at %s", wall, start, end, traffic)
		case wholeBlockRetained && !cutoff.Equal(operatorBlockStart(block).Add(-2*24*time.Hour)):
			t.Errorf("at %s: the cutoff moved to %s while the whole block is retained", wall, cutoff)
		case operatorBlockReliabilityRetained(previousMinBlockNumber, wall):
			t.Errorf("at %s: block %d is still retained", wall, block-1)
		}
	}
}

// A completed operator block whose award runs every bounded read: traffic in
// the block, and the reliability evidence of two valid clients of the provider
// in two countries, one with a country multiplier. The award runs at awardNow
// past the cutoff (operatorBlockPointsTestBlock).
func newOperatorBlockPointsReadFixture(t testing.TB, ctx context.Context, now time.Time) (block int64, awardNow time.Time) {
	t.Helper()
	block, awardNow, cutoff := operatorBlockPointsTestBlock(now)
	useOperatorBlockPointsTransition(t, cutoff)

	payer := newOperatorBlockPointsPayer(t, ctx)
	provider, providerClient := operatorBlockPointsProvider(ctx)
	inBlock := operatorBlockPointsTestTrafficTime(block, cutoff)
	payer.contract(t, ctx, provider, providerClient, inBlock, inBlock, 1024*1024*1024, 0)

	countries := map[string]server.Id{}
	for i, country := range []string{"us", "de"} {
		clientId := server.NewId()
		Testing_CreateDevice(ctx, provider, server.NewId(), clientId, "", "")
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, fmt.Sprintf("192.0.2.%d:20000", i+1), CreateNetworkClientHandler(ctx))
		if err != nil {
			t.Fatal(err)
		}
		location := &Location{
			LocationType: LocationTypeCity,
			City:         "city-" + clientId.String(),
			Region:       "region-" + clientId.String(),
			Country:      "country-" + country,
			CountryCode:  country,
		}
		CreateLocation(ctx, location)
		countries[country] = location.CountryLocationId
		if err := SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{}); err != nil {
			t.Fatal(err)
		}
		addressHash := server.ClientIpHashForAddr(netip.MustParseAddr(fmt.Sprintf("198.51.100.%d", i+1)))
		for j := range i + 2 {
			AddClientReliabilityStats(ctx, provider, clientId, addressHash, inBlock.Add(time.Duration(j)*ReliabilityBlockDuration), &ClientReliabilityStats{
				ConnectionEstablishedCount: 1,
				ProvideEnabledCount:        1,
				ReceiveMessageCount:        1,
				ReceiveByteCount:           1024,
				SendMessageCount:           1,
				SendByteCount:              1024,
			})
		}
	}
	UpdateClientLocationReliabilities(ctx, now.Add(-4*ReliabilityBlockDuration), now)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability_multiplier (country_location_id, reliability_multiplier) VALUES ($1, 1.5)`, countries["de"]))
	})
	return block, awardNow
}

// The root cause of the backfill dry run's failure on Main (2026-10-29,
// "FATAL: terminating connection due to idle-in-transaction timeout (SQLSTATE
// 25P03)" 2.5 h into block 15): each bounded read of an award ran in a
// transaction of its own, so the server sat idle in that transaction while a
// slow client drained the read's rows, and Main ends a session idle in a
// transaction after 60 s. An award begins no transaction at all, so no read
// can leave one idle, however slow the client.
func TestOperatorBlockPointsAwardBeginsNoTransaction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		block, awardNow := newOperatorBlockPointsReadFixture(t, ctx, now)

		// as the dry run runs it
		server.RequirePgReadOnly()
		defer server.Testing_ReleasePgReadOnly()
		observation, uninstall := server.Testing_ObserveTxIo()
		award, err := ComputeOperatorBlockPoints(ctx, operatorBlockPointsTestConfig(), block, awardNow)
		uninstall()
		if err != nil {
			t.Fatal(err)
		}
		if award.SweepCount != 1 || award.ReliabilityNetworkCount != 1 || award.ReliabilityEffectiveBlockCount <= 0 {
			t.Fatalf("award %+v", award)
		}
		if statements := observation.FirstStatements(); len(statements) != 0 {
			for _, statement := range statements {
				t.Errorf("transaction of %s: first statement %s", statement.TxSite, statement.Site)
			}
			t.Fatalf("the award began %d transactions", len(statements))
		}
	})
}
