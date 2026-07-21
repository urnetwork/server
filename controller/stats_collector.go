package controller

// the public network stats collector.
// periodically refreshes the urnetwork_stats_* gauges behind the warp
// grafana front's public /stats.json feed (warp repo, grafana/stats.go),
// which implements the network operator stats contract consumed by ur.xyz:
//
//	urnetwork_stats_total_networks                   total networks (db)
//	urnetwork_stats_block_users                      unique top-level identities with contract usage this block (db)
//	urnetwork_stats_countries                        countries with connected valid providers (db)
//	urnetwork_stats_staked_alpha                     cumulative α staked in the ST contract (chain)
//	urnetwork_stats_block_demand_deposits_alpha      demand deposits this block (st_event mirror)
//	urnetwork_stats_block_miner_emissions_alpha      miner emission captured this block (st_event mirror)
//	urnetwork_stats_alpha_usd                        α price in USD (geckoterminal, sourced server side)
//	urnetwork_stats_prev_block_users                 users of the last finished block (redis snapshot)
//	urnetwork_stats_prev_block_demand_deposits_alpha demand deposits of the last finished block
//	urnetwork_stats_prev_block_miner_emissions_alpha miner emission of the last finished block
//
// the block accumulators reset when a block rolls over, so the feed also
// serves the finished block as a stable reference. deposits and emissions
// recompute exactly from the st_event mirror by chain block range; users
// come from a per-block redis snapshot frozen forward by this collector
// (a last-seen activity marker cannot reconstruct a past window), so
// prev_block_users starts publishing at the first rollover after deploy.
//
// StartStatsCollector is called only by the taskworker
// (cli/taskworker/main.go), so only taskworker registries export these
// series, and every taskworker host recomputes the same values on its own
// clock — the feed reads each gauge with max across hosts. a gauge
// registers with the default registry (pushed by server.StartStatsPusher)
// on its first set, so a stat that never has a value here (st disabled,
// no netuid) is never exported at all rather than exported as 0.
//
// the "block" here is the subnet block clock (model/sn_model.go): 1-based,
// 7 days per block, every block opening and closing at 00:00 UTC Sunday.
// it is unrelated to the warp deploy block label and to the ST contract
// epoch; st_event rows are windowed by mapping the block open time to a
// chain block through the ~12s/block head anchor.

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const statsCollectorInterval = 60 * time.Second

// the db counts scan the hot network_client table, so they refresh on
// every statsCollectorDbTicks'th tick rather than every tick
const statsCollectorDbTicks = 5

// statsGauge registers with the default registry on first set, so an
// unset gauge is never pushed. set only from the collector goroutine
type statsGauge struct {
	gauge      prometheus.Gauge
	registered bool
}

func newStatsGauge(name string, help string) *statsGauge {
	return &statsGauge{
		gauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "stats",
			Name:      name,
			Help:      help,
		}),
	}
}

func (self *statsGauge) set(value float64) {
	if !self.registered {
		prometheus.MustRegister(self.gauge)
		self.registered = true
	}
	self.gauge.Set(value)
}

var statsTotalNetworksGauge = newStatsGauge(
	"total_networks",
	"Total networks at the operator",
)
var statsBlockUsersGauge = newStatsGauge(
	"block_users",
	"Unique top-level clients with contract usage this block",
)
var statsCountriesGauge = newStatsGauge(
	"countries",
	"Countries with a connected valid provider",
)
var statsStakedAlphaGauge = newStatsGauge(
	"staked_alpha",
	"Cumulative alpha staked in the ST contract (buybackTotal)",
)
var statsBlockDemandDepositsAlphaGauge = newStatsGauge(
	"block_demand_deposits_alpha",
	"Alpha demand deposits this block",
)
var statsBlockMinerEmissionsAlphaGauge = newStatsGauge(
	"block_miner_emissions_alpha",
	"Alpha miner emission captured this block",
)
var statsAlphaUsdGauge = newStatsGauge(
	"alpha_usd",
	"Alpha price in USD as the operator observes it",
)
var statsPrevBlockUsersGauge = newStatsGauge(
	"prev_block_users",
	"Unique top-level clients with contract usage in the last finished block",
)
var statsPrevBlockDemandDepositsAlphaGauge = newStatsGauge(
	"prev_block_demand_deposits_alpha",
	"Alpha demand deposits in the last finished block",
)
var statsPrevBlockMinerEmissionsAlphaGauge = newStatsGauge(
	"prev_block_miner_emissions_alpha",
	"Alpha miner emission captured in the last finished block",
)

// statsRaoToAlpha converts rao (1e-9 α) to whole α
func statsRaoToAlpha(rao *big.Int) float64 {
	value, _ := new(big.Float).Quo(new(big.Float).SetInt(rao), big.NewFloat(1e9)).Float64()
	return value
}

// StartStatsCollector refreshes the public stats gauges until the context
// is done. each refresh is isolated so a transient db, chain, or fetch
// error never stops the collector
func StartStatsCollector(ctx context.Context) {
	go server.HandleError(func() {
		dbTick := 0
		for {
			if dbTick == 0 {
				server.HandleError(func() {
					statsRefreshDb(ctx)
				})
			}
			dbTick = (dbTick + 1) % statsCollectorDbTicks
			server.HandleError(func() {
				statsRefreshChain(ctx)
			})
			server.HandleError(func() {
				statsRefreshPrice(ctx)
			})
			select {
			case <-ctx.Done():
				return
			case <-time.After(statsCollectorInterval):
			}
		}
	})
}

func statsRefreshDb(ctx context.Context) {
	now := time.Now()
	blockNumber := model.SubnetBlockNumber(now)

	statsTotalNetworksGauge.set(float64(model.CountNetworks(ctx)))
	statsCountriesGauge.set(float64(model.CountProviderCountries(ctx)))

	users := model.CountTopLevelClientsWithContractSince(ctx, model.SubnetBlockStart(now))
	statsBlockUsersGauge.set(float64(users))
	// freeze the running count forward; the last write before rollover is
	// the finished block's final value (see model.SetBlockUsersSnapshot)
	model.SetBlockUsersSnapshot(ctx, blockNumber, users)
	if prevUsers, ok := model.GetBlockUsersSnapshot(ctx, blockNumber-1); ok {
		statsPrevBlockUsersGauge.set(float64(prevUsers))
	}
}

func statsRefreshChain(ctx context.Context) {
	if !StEnabled() {
		return
	}
	cfg, client, err := stRequire()
	if err != nil {
		return
	}

	buybackRao, err := client.BuybackTotal(ctx)
	if err != nil {
		glog.Infof("[stats]buybackTotal read error (%s)\n", err)
		return
	}
	statsStakedAlphaGauge.set(statsRaoToAlpha(buybackRao))

	// map the block open time to a chain block through the head anchor to
	// window the mirrored events
	state, err := client.Epoch(ctx)
	if err != nil {
		glog.Infof("[stats]chain head read error (%s)\n", err)
		return
	}
	// chain blocks back from the head for a wall-clock time, clamped to
	// the chain genesis
	now := time.Now()
	chainBlockAt := func(t time.Time) uint64 {
		blocksBack := uint64(int64(now.Sub(t)/time.Second) / cfg.BlockSeconds)
		if state.HeadBlock <= blocksBack {
			return 0
		}
		return state.HeadBlock - blocksBack
	}

	blockStart := model.SubnetBlockStart(now)
	windowStartBlock := chainBlockAt(blockStart)
	statsBlockDemandDepositsAlphaGauge.set(statsRaoToAlpha(model.SumStDepositedInBlockRangeRao(ctx, windowStartBlock, state.HeadBlock+1)))
	statsBlockMinerEmissionsAlphaGauge.set(statsRaoToAlpha(model.SumStPoolSweptMeasuredInBlockRangeRao(ctx, windowStartBlock, state.HeadBlock+1)))

	// the last finished block, recomputed exactly from the event mirror.
	// block 1 has no predecessor
	if model.SubnetBlockGenesis.Before(blockStart) {
		prevStartBlock := chainBlockAt(blockStart.Add(-model.SubnetBlockDuration))
		statsPrevBlockDemandDepositsAlphaGauge.set(statsRaoToAlpha(model.SumStDepositedInBlockRangeRao(ctx, prevStartBlock, windowStartBlock)))
		statsPrevBlockMinerEmissionsAlphaGauge.set(statsRaoToAlpha(model.SumStPoolSweptMeasuredInBlockRangeRao(ctx, prevStartBlock, windowStartBlock)))
	}
}

func statsRefreshPrice(ctx context.Context) {
	cfg := stConfig()
	if cfg == nil || cfg.Netuid == 0 {
		return
	}
	// the subnet pool on geckoterminal; the site's browser-side fallback
	// reads the same pool through coingecko
	type poolResponse struct {
		Data struct {
			Attributes struct {
				BaseTokenPriceUsd string `json:"base_token_price_usd"`
			} `json:"attributes"`
		} `json:"data"`
	}
	url := fmt.Sprintf("https://api.geckoterminal.com/api/v2/networks/bittensor/pools/0-%d", cfg.Netuid)
	response, err := server.HttpGetRequireStatusOk(ctx, url, server.NoCustomHeaders, server.ResponseJsonObject[poolResponse])
	if err != nil {
		// keep the last good value; the series only goes stale if the
		// pusher itself stops
		glog.Infof("[stats]alpha price fetch error (%s)\n", err)
		return
	}
	price, err := strconv.ParseFloat(response.Data.Attributes.BaseTokenPriceUsd, 64)
	if err != nil || price <= 0 {
		return
	}
	statsAlphaUsdGauge.set(price)
}
