// The reliability weight of each network in each country over a window of
// reliability blocks, as the payout plan's fold computed it into
// network_connection_reliability_score: for each valid client's block, one
// over the count of valid clients sharing its address, summed per network and
// country of the client's valid location, over the window's effective block
// count. Operator block points multiply it by the country multipliers, and the
// country multiplier refresh sums it per country
// (network_client_location_reliability_multiplier.go).
package model

import (
	"context"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// A network's clients in one country; hasCountry is false for a valid client
// location without a country.
type reliabilityWindowKey struct {
	networkId         server.Id
	countryLocationId server.Id
	hasCountry        bool
}

// One network's weight in one country.
type reliabilityWindowWeight struct {
	key    reliabilityWindowKey
	weight float64
}

// Reads the weights of the blocks [minBlockNumber, maxBlockNumber), less the
// degraded ones, over effectiveBlockCount, in bounded read-only statements,
// none in a transaction: the plan's chunk reads
// (readPaymentPlanReliabilityPartials), the valid client locations, the exact
// numeric sums per network and country, and the division in SQL as the fold
// does it. Ordered by network, then country, a network's countryless weight
// first.
func readReliabilityWindowWeights(
	ctx context.Context,
	minBlockNumber int64,
	maxBlockNumber int64,
	degradedBlockNumbers []int64,
	effectiveBlockCount int64,
) ([]*reliabilityWindowWeight, error) {
	sums := map[reliabilityWindowKey]*reliabilityExactSum{}
	// client -> its valid location's country; absent for an invalid location
	clientCountries := map[server.Id]*server.Id{}
	clientRead := map[server.Id]bool{}
	for chunkMin := minBlockNumber; chunkMin < maxBlockNumber; chunkMin += paymentPlanReliabilityChunkBlockCount {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunkMax := min(chunkMin+paymentPlanReliabilityChunkBlockCount, maxBlockNumber)
		partials := readPaymentPlanReliabilityPartials(ctx, chunkMin, chunkMax, degradedBlockNumbers)

		unread := []server.Id{}
		for key := range partials {
			if !clientRead[key.clientId] {
				clientRead[key.clientId] = true
				unread = append(unread, key.clientId)
			}
		}
		slices.SortFunc(unread, server.Id.Cmp)
		for batch := range slices.Chunk(unread, operatorBlockPointsLocationBatchSize) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT client_id, country_location_id
					FROM network_client_location_reliability
					WHERE client_id = ANY($1::uuid[]) AND valid = true
					`,
					batch,
				)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var clientId server.Id
						var countryLocationId *server.Id
						server.Raise(result.Scan(&clientId, &countryLocationId))
						clientCountries[clientId] = countryLocationId
					}
				})
				boundedReadDone("client_locations")
			})
		}

		for partialKey, partial := range partials {
			countryLocationId, valid := clientCountries[partialKey.clientId]
			if !valid {
				continue
			}
			key := reliabilityWindowKey{networkId: partialKey.networkId}
			if countryLocationId != nil {
				key.countryLocationId = *countryLocationId
				key.hasCountry = true
			}
			sum, ok := sums[key]
			if !ok {
				sum = &reliabilityExactSum{}
				sums[key] = sum
			}
			sum.add(partial.reliabilitySum.numeric())
		}
		// progress: a week of blocks takes hours on Main
		glog.Infof("[reliability]window blocks [%d, %d) read to %d: %d clients\n", minBlockNumber, maxBlockNumber, chunkMax, len(clientRead))
	}
	if len(sums) == 0 {
		return []*reliabilityWindowWeight{}, nil
	}

	keys := slices.SortedFunc(maps.Keys(sums), func(a reliabilityWindowKey, b reliabilityWindowKey) int {
		if c := a.networkId.Cmp(b.networkId); c != 0 {
			return c
		}
		if a.hasCountry != b.hasCountry {
			if a.hasCountry {
				return 1
			}
			return -1
		}
		return a.countryLocationId.Cmp(b.countryLocationId)
	})
	numerics := make([]pgtype.Numeric, len(keys))
	weights := make([]*reliabilityWindowWeight, len(keys))
	for i, key := range keys {
		numerics[i] = sums[key].numeric()
		weights[i] = &reliabilityWindowWeight{key: key}
	}
	server.Db(ctx, func(conn server.PgConn) {
		// the fold's division and its float8 store, exactly
		result, err := conn.Query(
			ctx,
			`
			SELECT t.i, (t.s / $1::bigint)::float8
			FROM unnest($2::numeric[]) WITH ORDINALITY AS t(s, i)
			ORDER BY t.i
			`,
			effectiveBlockCount,
			numerics,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var i int64
				var weight float64
				server.Raise(result.Scan(&i, &weight))
				weights[i-1].weight = weight
			}
		})
		boundedReadDone("reliability_weights")
	})
	return weights, nil
}
