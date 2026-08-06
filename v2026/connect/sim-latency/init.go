package main

// `sim-latency init`: sample the mixture once and write providers.yml.

import (
	"fmt"
	"math"
	"net/netip"

	"github.com/urnetwork/server/v2026"
)

// defaultMixture is the competition's provider population (v2, 2026-07-31):
// a residential bulk, a variable mobile mode, and a continuous fast upper
// band — business fiber bridging into a narrowed hosting range — holding 30%
// of the population. The upper band is deliberately dense and capped tighter
// so the fast tail of per-request throughput samples many lanes (a stable
// p95) instead of a handful of golden hosting lanes: with the v1 mixture
// (hosting 100–1000 Mbps at weight 0.15, caps 64–256) the p95 sat on a thin
// tail whose mass was a per-run warm-up lottery, measuring at 47% CV between
// identical runs (see README "Measured noise floor"). Users edit the mixture
// in providers.yml and re-run init to re-sample.
func defaultMixture() []MixtureComponent {
	return []MixtureComponent{
		{
			Name: "residential-good", Weight: 0.45, UserType: "consumer",
			LatencyMillis: Range{10, 40}, JitterMillis: Range{0, 5},
			BandwidthMbps: Range{20, 150}, Loss: Range{0, 0.001},
			MaxConnections: Range{8, 32},
			UptimeSeconds:  Range{1800, 14400}, DowntimeSeconds: Range{5, 60},
			DegradedFraction:     Range{0, 0.1},
			DegradedLatencyScale: Range{1.5, 3}, DegradedBandwidthScale: Range{0.3, 0.7}, DegradedLossAdd: Range{0, 0.01},
		},
		{
			Name: "mobile-variable", Weight: 0.25, UserType: "consumer",
			LatencyMillis: Range{40, 150}, JitterMillis: Range{5, 40},
			BandwidthMbps: Range{2, 40}, Loss: Range{0.001, 0.02},
			MaxConnections: Range{4, 12},
			UptimeSeconds:  Range{300, 3600}, DowntimeSeconds: Range{10, 120},
			DegradedFraction:     Range{0.1, 0.4},
			DegradedLatencyScale: Range{2, 5}, DegradedBandwidthScale: Range{0.1, 0.5}, DegradedLossAdd: Range{0.01, 0.05},
		},
		{
			Name: "business-fiber", Weight: 0.15, UserType: "business",
			LatencyMillis: Range{5, 25}, JitterMillis: Range{0, 4},
			BandwidthMbps: Range{50, 250}, Loss: Range{0, 0.001},
			MaxConnections: Range{24, 64},
			UptimeSeconds:  Range{3600, 28800}, DowntimeSeconds: Range{5, 60},
			DegradedFraction:     Range{0, 0.08},
			DegradedLatencyScale: Range{1.3, 2.5}, DegradedBandwidthScale: Range{0.4, 0.8}, DegradedLossAdd: Range{0, 0.005},
		},
		{
			Name: "hosting-fast", Weight: 0.15, UserType: "hosting",
			LatencyMillis: Range{2, 20}, JitterMillis: Range{0, 3},
			BandwidthMbps: Range{200, 600}, Loss: Range{0, 0.0005},
			MaxConnections: Range{32, 96},
			UptimeSeconds:  Range{7200, 86400}, DowntimeSeconds: Range{2, 20},
			DegradedFraction:     Range{0, 0.05},
			DegradedLatencyScale: Range{1.2, 2}, DegradedBandwidthScale: Range{0.5, 0.9}, DegradedLossAdd: Range{0, 0.002},
		},
	}
}

func defaultConfig(seed int64, providerCount int, clientPoolSize int, meanPerMinute float64) *Config {
	return &Config{
		Seed: seed,
		Region: RegionConfig{
			Country: "Sim", CountryCode: "zz", Region: "Sim", City: "Sim",
		},
		Subnets: SubnetConfig{
			// RFC 2544 benchmarking range (198.18.0.0/15 = 131072 addrs) for
			// providers, a separate /16 for clients.
			Provider: "198.18.0.0/15",
			Client:   "198.20.0.0/16",
		},
		Site: SiteConfig{
			MeanDepth: 4, Branching: 3,
			// web tier: most pages. download tier: the large pages the
			// throughput metrics measure (>= results.go throughputMinBytes),
			// sized so transfer time dominates ttfb and bytes/total honestly
			// approximates lane bandwidth. The 512 KiB..2 MiB gap keeps the
			// gate unambiguous.
			MinBodyBytes: 4 * 1024, MaxBodyBytes: 512 * 1024,
			LargeFraction:     0.25,
			LargeMinBodyBytes: 2 * 1024 * 1024,
			LargeMaxBodyBytes: 6 * 1024 * 1024,
		},
		Clients: ClientsConfig{
			PoolSize:            clientPoolSize,
			MeanPerMinute:       meanPerMinute,
			BalanceBytes:        int64(1024) * 1024 * 1024 * 1024, // 1 TiB
			ConnectionsPerCrawl: 6,
		},
		Providers: ProvidersConfig{
			Count:                providerCount,
			FleetNetworkFraction: 0.2,
			FleetNetworkCount:    50,
			Mixture:              defaultMixture(),
		},
	}
}

// generateFleet samples one entry per provider from the mixture and assigns
// ips, identities, and network grouping.
func generateFleet(config *Config) error {
	providerPrefix, err := netip.ParsePrefix(config.Subnets.Provider)
	if err != nil {
		return fmt.Errorf("provider subnet: %w", err)
	}

	totalWeight := 0.0
	for _, component := range config.Providers.Mixture {
		totalWeight += component.Weight
	}
	if totalWeight <= 0 {
		return fmt.Errorf("mixture weights sum to 0")
	}

	r := newRng(config.Seed)

	// pre-mint the shared fleet network ids (deterministic from the seed)
	fleetNetworkIds := make([]server.Id, config.Providers.FleetNetworkCount)
	for i := range fleetNetworkIds {
		fleetNetworkIds[i] = r.id()
	}

	ips := newIpIterator(providerPrefix)

	fleet := make([]ProviderEntry, 0, config.Providers.Count)
	for i := 0; i < config.Providers.Count; i += 1 {
		ip, ok := ips.next()
		if !ok {
			return fmt.Errorf("provider subnet %s exhausted at %d providers", config.Subnets.Provider, i)
		}

		// pick a mixture component by weight
		component := pickComponent(config.Providers.Mixture, totalWeight, r)

		// network assignment: shared fleet network or singleton
		var networkId server.Id
		if r.float64() < config.Providers.FleetNetworkFraction && 0 < len(fleetNetworkIds) {
			networkId = fleetNetworkIds[r.intn(len(fleetNetworkIds))]
		} else {
			networkId = r.id()
		}

		userType := component.UserType
		if userType == "" {
			userType = "consumer"
		}

		fleet = append(fleet, ProviderEntry{
			Index:     i,
			Ip:        ip.String(),
			ClientId:  r.id().String(),
			NetworkId: networkId.String(),
			DeviceId:  r.id().String(),
			UserId:    r.id().String(),
			UserType:  userType,
			Component: component.Name,
			Seed:      r.int63(),

			LatencyMillis:          component.LatencyMillis.sample(r),
			JitterMillis:           component.JitterMillis.sample(r),
			BandwidthBps:           int64(component.BandwidthMbps.sample(r) * 1e6 / 8),
			Loss:                   component.Loss.sample(r),
			MaxConnections:         int(math.Round(component.MaxConnections.sample(r))),
			UptimeSeconds:          component.UptimeSeconds.sample(r),
			DowntimeSeconds:        component.DowntimeSeconds.sample(r),
			DegradedFraction:       component.DegradedFraction.sample(r),
			DegradedLatencyScale:   scaleOrOne(component.DegradedLatencyScale.sample(r)),
			DegradedBandwidthScale: scaleOrOne(component.DegradedBandwidthScale.sample(r)),
			DegradedLossAdd:        component.DegradedLossAdd.sample(r),
		})
	}
	config.Fleet = fleet
	return nil
}

func pickComponent(mixture []MixtureComponent, totalWeight float64, r *rng) MixtureComponent {
	target := r.float64() * totalWeight
	net := 0.0
	for _, component := range mixture {
		net += component.Weight
		if target < net {
			return component
		}
	}
	return mixture[len(mixture)-1]
}

func scaleOrOne(v float64) float64 {
	if v <= 0 {
		return 1
	}
	return v
}
