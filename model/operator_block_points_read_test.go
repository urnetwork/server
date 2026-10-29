package model

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A completed operator block whose award runs every bounded read: traffic in
// the block, and the reliability evidence of two valid clients of the provider
// in two countries, one with a country multiplier.
func newOperatorBlockPointsReadFixture(t testing.TB, ctx context.Context, now time.Time) (block int64) {
	t.Helper()
	block = int64(SubnetBlockNumber(now)) - 2
	blockStart := operatorBlockStart(block)
	useOperatorBlockPointsTransition(t, blockStart.Add(-2*24*time.Hour))

	payer := newOperatorBlockPointsPayer(t, ctx)
	provider, providerClient := operatorBlockPointsProvider(ctx)
	inBlock := blockStart.Add(24 * time.Hour)
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
	return block
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
		block := newOperatorBlockPointsReadFixture(t, ctx, now)

		// as the dry run runs it
		server.RequirePgReadOnly()
		defer server.Testing_ReleasePgReadOnly()
		observation, uninstall := server.Testing_ObserveTxIo()
		award, err := ComputeOperatorBlockPoints(ctx, operatorBlockPointsTestConfig(), block, now)
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
