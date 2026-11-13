// The country reliability multipliers (network_client_location_reliability_multiplier)
// boost the reliability of providers in countries with little reliable supply.
// Operator block points multiply a network's reliability weight in a country by
// the country's multiplier (readOperatorBlockReliability), and the app shows
// the multipliers of a network's countries (GetNetworkReliability).
//
// The payout planner refreshed them at the end of each weekly plan, from the
// reliability its window folded into network_connection_reliability_score,
// until USDC provider payouts ended (owner decisions 2026-11-12 and
// 2026-11-13). Since then they stood still. A taskworker task refreshes them
// now (RefreshCountryReliabilityMultipliers), by the planner's rule, from the
// newest rolled-up reliability: a country's multiplier is the subsidy config's
// country weight target over the total weight of the country's networks
// (readReliabilityWindowWeights), at least 1 and at most the config's maximum.
// A country weighing no more than 0.1 takes the maximum, as does every country
// with no reliable provider at all.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// A country weighing no more than this takes the maximum multiplier, so a
// near-empty country's tiny weight never makes the division the bound.
const countryReliabilityMultiplierMinWeight = 0.1

// Settings of the multiplier refresh.
type CountryReliabilityMultiplierSettings struct {
	// How long after a deploy that finds no refresh queued the first runs: the
	// multipliers stood still since USDC payouts ended, so soon.
	FirstRunDelay time.Duration
	// How often the multipliers refresh: weekly, as the planner refreshed them
	// at the end of each weekly plan.
	Period time.Duration
	// The reliability the country weights are read from, ending at the newest
	// rolled-up block. A weight is per block, so a window of any length weighs
	// a country alike. A day of blocks is about 96 bounded statements on Main,
	// 8 to 26 minutes; the planner read a week.
	Window time.Duration
	// The refresh task's execution bound, past the longest window read.
	MaxTime time.Duration
}

func DefaultCountryReliabilityMultiplierSettings() *CountryReliabilityMultiplierSettings {
	return &CountryReliabilityMultiplierSettings{
		FirstRunDelay: 15 * time.Minute,
		Period:        7 * 24 * time.Hour,
		Window:        24 * time.Hour,
		MaxTime:       2 * time.Hour,
	}
}

// The multiplier of a country whose networks weigh netWeight. Pure.
func countryReliabilityMultiplier(netWeight float64, weightTarget float64, maxMultiplier float64) float64 {
	multiplier := maxMultiplier
	if countryReliabilityMultiplierMinWeight < netWeight {
		multiplier = min(multiplier, weightTarget/netWeight)
	}
	return max(multiplier, 1.0)
}

// Replaces every country's multiplier from the window that ends at the newest
// rolled-up block before now, with the subsidy config's target and maximum.
// Returns false, changing nothing, when the window has no usable block.
func RefreshCountryReliabilityMultipliers(ctx context.Context, now time.Time, settings *CountryReliabilityMultiplierSettings) (refreshed bool, returnErr error) {
	minBlockNumber, maxBlockNumber := operatorBlockReliabilityBlockWindow(now.Add(-settings.Window), now)
	var coveredBlockCount int64
	var degradedBlockNumbers []int64
	server.Db(ctx, func(conn server.PgConn) {
		// the window ends at the drained blocks, as the fold's did
		shift := reliabilityRollupBlockShift(ctx, conn, maxBlockNumber)
		minBlockNumber -= shift
		maxBlockNumber -= shift
		coveredBlockCount = reliabilityCoveredBlockCount(ctx, conn, minBlockNumber, maxBlockNumber)
		degradedBlockNumbers = reliabilityDegradedBlocks(ctx, conn, minBlockNumber, maxBlockNumber)
	})
	effectiveBlockCount := coveredBlockCount - int64(len(degradedBlockNumbers))
	if effectiveBlockCount <= 0 {
		glog.Infof("[ncr]no usable blocks in country multiplier window [%d, %d); keeping the multipliers\n", minBlockNumber, maxBlockNumber)
		return false, nil
	}
	weights, err := readReliabilityWindowWeights(ctx, minBlockNumber, maxBlockNumber, degradedBlockNumbers, effectiveBlockCount)
	if err != nil {
		return false, err
	}
	countryWeights := map[server.Id]float64{}
	for _, weight := range weights {
		if weight.key.hasCountry {
			countryWeights[weight.key.countryLocationId] += weight.weight
		}
	}

	config := EnvSubsidyConfig()
	server.Tx(ctx, func(tx server.PgTx) {
		multipliers := map[server.Id]float64{}
		result, err := tx.Query(ctx, `SELECT location_id FROM location WHERE location_type = 'country'`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var countryLocationId server.Id
				server.Raise(result.Scan(&countryLocationId))
				multipliers[countryLocationId] = countryReliabilityMultiplier(0, config.CountryReliabilityWeightTarget, config.MaxCountryReliabilityMultiplier)
			}
		})
		for countryLocationId, netWeight := range countryWeights {
			multipliers[countryLocationId] = countryReliabilityMultiplier(netWeight, config.CountryReliabilityWeightTarget, config.MaxCountryReliabilityMultiplier)
		}

		server.CreateTempJoinTableInTx(
			ctx,
			tx,
			"temp_reliability_multiplier(country_location_id uuid -> reliability_multiplier double precision)",
			multipliers,
		)
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_location_reliability_multiplier`))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_client_location_reliability_multiplier (
				country_location_id,
				reliability_multiplier
			)
			SELECT
				country_location_id,
				reliability_multiplier
			FROM temp_reliability_multiplier
			`,
		))
	})
	glog.Infof("[ncr]country multipliers refreshed from blocks [%d, %d): %d countries weighed\n", minBlockNumber, maxBlockNumber, len(countryWeights))
	return true, nil
}
