package model

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Three clients of two networks, with reliability in the blocks before now:
// A's one client in "us" for one block, and B's two, which share their
// reliability reports' address, in "us" for two blocks and in "de" for three.
// Returns the networks and the country location ids by country code.
func seedReliabilityWindowTestClients(t testing.TB, ctx context.Context, now time.Time) (networkA server.Id, networkB server.Id, countries map[string]server.Id) {
	t.Helper()
	type client struct {
		network, client server.Id
		// the connection's address, and the address its reliability reports
		// carry
		connection, reliability string
		country                 string
		blocks                  int
	}
	networkA, networkB = server.NewId(), server.NewId()
	clients := []*client{
		{networkA, server.NewId(), "192.0.2.1", "198.51.100.1", "us", 1},
		{networkB, server.NewId(), "192.0.2.2", "198.51.100.2", "us", 2},
		{networkB, server.NewId(), "192.0.2.3", "198.51.100.2", "de", 3},
	}
	countries = map[string]server.Id{}
	for _, c := range clients {
		Testing_CreateDevice(ctx, c.network, server.NewId(), c.client, "", "")
		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, c.client, c.connection+":20000", handlerId)
		if err != nil {
			t.Fatal(err)
		}
		location := &Location{
			LocationType: LocationTypeCity,
			City:         "city-" + c.client.String(),
			Region:       "region-" + c.client.String(),
			Country:      "country-" + c.country,
			CountryCode:  c.country,
		}
		CreateLocation(ctx, location)
		countries[c.country] = location.CountryLocationId
		if err := SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{}); err != nil {
			t.Fatal(err)
		}
		for i := range c.blocks {
			AddClientReliabilityStats(ctx, c.network, c.client, server.ClientIpHashForAddr(netip.MustParseAddr(c.reliability)), now.Add(-time.Duration(i)*ReliabilityBlockDuration), &ClientReliabilityStats{
				ConnectionEstablishedCount: 1,
				ProvideEnabledCount:        1,
				ReceiveMessageCount:        1,
				ReceiveByteCount:           1024,
				SendMessageCount:           1,
				SendByteCount:              1024,
			})
		}
	}
	return
}

// The weights of the half-open window [start, end) by the statement the payout
// plan folded them into network_connection_reliability_score with, read back
// as that table's float8, in the award's key order.
func planFoldTestWeights(t testing.TB, ctx context.Context, start time.Time, end time.Time) []*reliabilityWindowWeight {
	t.Helper()
	minBlockNumber, maxBlockNumber := operatorBlockReliabilityBlockWindow(start, end)
	weights := []*reliabilityWindowWeight{}
	server.Db(ctx, func(conn server.PgConn) {
		degradedBlockNumbers := reliabilityDegradedBlocks(ctx, conn, minBlockNumber, maxBlockNumber)
		effectiveBlockCount := reliabilityCoveredBlockCount(ctx, conn, minBlockNumber, maxBlockNumber) - int64(len(degradedBlockNumbers))
		result, err := conn.Query(
			ctx,
			`
			SELECT
				valid_counts.network_id,
				network_client_location_reliability.country_location_id,
				(SUM(1.0/valid_counts.valid_client_count) / $3::bigint)::float8
			FROM (
				SELECT
					network_id,
					client_id,
					COUNT(*) OVER (PARTITION BY block_number, client_address_hash) AS valid_client_count
				FROM client_reliability
				WHERE
					valid = true AND
					$1 <= block_number AND
					block_number < $2 AND
					NOT (block_number = ANY($4::bigint[]))
			) valid_counts
			INNER JOIN network_client_location_reliability ON
				network_client_location_reliability.client_id = valid_counts.client_id AND
				network_client_location_reliability.valid = true
			GROUP BY
				valid_counts.network_id,
				network_client_location_reliability.country_location_id
			`,
			minBlockNumber,
			maxBlockNumber,
			effectiveBlockCount,
			degradedBlockNumbers,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				weight := &reliabilityWindowWeight{}
				var countryLocationId *server.Id
				server.Raise(result.Scan(&weight.key.networkId, &countryLocationId, &weight.weight))
				if countryLocationId != nil {
					weight.key.countryLocationId = *countryLocationId
					weight.key.hasCountry = true
				}
				weights = append(weights, weight)
			}
		})
	})
	slices.SortFunc(weights, func(a *reliabilityWindowWeight, b *reliabilityWindowWeight) int {
		if c := a.key.networkId.Cmp(b.key.networkId); c != 0 {
			return c
		}
		if a.key.hasCountry != b.key.hasCountry {
			if a.key.hasCountry {
				return 1
			}
			return -1
		}
		return a.key.countryLocationId.Cmp(b.key.countryLocationId)
	})
	return weights
}

// A country's multiplier is the target over its networks' weight, at least 1
// and at most the maximum, and the maximum when the weight is at most 0.1.
func TestCountryReliabilityMultiplier(t *testing.T) {
	for _, c := range []struct {
		netWeight float64
		want      float64
	}{
		{netWeight: 0, want: 8},
		{netWeight: 0.1, want: 8},
		{netWeight: 3, want: 8},
		{netWeight: 6, want: 5},
		{netWeight: 30, want: 1},
		{netWeight: 300, want: 1},
	} {
		if got := countryReliabilityMultiplier(c.netWeight, 30, 8); got != c.want {
			t.Errorf("multiplier of weight %v = %v, want %v", c.netWeight, got, c.want)
		}
	}
}

// USDC provider payouts ended (owner decisions 2026-11-12 and 2026-11-13), and
// with them the payout planner that refreshed the country multipliers from its
// window's reliability; they stood still since. The refresh weighs each
// country by its networks' reliability in the newest window, as the plan's
// fold did, and replaces every country's multiplier: "us" and "de" by their
// weights, and a country with no reliable provider, and one whose stale
// multiplier the refresh replaces, at the maximum.
func TestRefreshCountryReliabilityMultipliersWeighsTheNewestReliability(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		_, _, countries := seedReliabilityWindowTestClients(t, ctx, now)
		empty := &Location{
			LocationType: LocationTypeCity,
			City:         "city-fr",
			Region:       "region-fr",
			Country:      "country-fr",
			CountryCode:  "fr",
		}
		CreateLocation(ctx, empty)
		// a stale multiplier, as the planner's last plan left it
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability_multiplier (country_location_id, reliability_multiplier) VALUES ($1, 1.0)`, empty.CountryLocationId))
		})

		config := EnvSubsidyConfig()
		previous := *config
		defer func() { *config = previous }()
		config.CountryReliabilityWeightTarget = 1.5
		config.MaxCountryReliabilityMultiplier = 8

		settings := DefaultCountryReliabilityMultiplierSettings()
		settings.Window = 4 * ReliabilityBlockDuration
		UpdateClientLocationReliabilities(ctx, now.Add(-settings.Window), now)
		refreshed, err := RefreshCountryReliabilityMultipliers(ctx, now.Add(time.Microsecond), settings)
		if err != nil || !refreshed {
			t.Fatal("no refresh", refreshed, err)
		}

		countryWeights := map[server.Id]float64{}
		for _, weight := range planFoldTestWeights(t, ctx, now.Add(time.Microsecond).Add(-settings.Window), now.Add(time.Microsecond)) {
			if weight.key.hasCountry {
				countryWeights[weight.key.countryLocationId] += weight.weight
			}
		}
		multipliers := GetAllClientLocationReliabilityMultipliers(ctx)
		for _, code := range []string{"us", "de"} {
			countryLocationId := countries[code]
			want := countryReliabilityMultiplier(countryWeights[countryLocationId], 1.5, 8)
			if countryWeights[countryLocationId] <= 0 || multipliers[countryLocationId] == nil || multipliers[countryLocationId].ReliabilityMultiplier != want {
				t.Errorf("%s: weight %v, multiplier %+v, want %v", code, countryWeights[countryLocationId], multipliers[countryLocationId], want)
			}
		}
		if multipliers[empty.CountryLocationId] == nil || multipliers[empty.CountryLocationId].ReliabilityMultiplier != 8 {
			t.Errorf("a country with no reliable provider: %+v", multipliers[empty.CountryLocationId])
		}
	})
}
