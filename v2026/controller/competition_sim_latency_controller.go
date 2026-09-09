package controller

// This file owns the deterministic providers.yml generator shared by the
// control-plane API and the simulator command. Round creation runs in the API
// container, so it cannot depend on evaluator-host executables or storage.

import (
	"errors"
	"fmt"
	"math"
	mathrand "math/rand"
	"net/netip"

	"github.com/urnetwork/server/v2026"
	"gopkg.in/yaml.v3"
)

// A closed interval sampled uniformly while constructing one provider.
type simLatencyRange struct {
	Min float64 `yaml:"min"`
	Max float64 `yaml:"max"`
}

// One weighted mode in the provider population.
type simLatencyMixtureComponent struct {
	Name                   string          `yaml:"name"`
	Weight                 float64         `yaml:"weight"`
	UserType               string          `yaml:"user_type"`
	LatencyMillis          simLatencyRange `yaml:"latency_ms"`
	JitterMillis           simLatencyRange `yaml:"jitter_ms"`
	BandwidthMbps          simLatencyRange `yaml:"bandwidth_mbps"`
	Loss                   simLatencyRange `yaml:"loss"`
	MaxConnections         simLatencyRange `yaml:"max_connections"`
	UptimeSeconds          simLatencyRange `yaml:"uptime_s"`
	DowntimeSeconds        simLatencyRange `yaml:"downtime_s"`
	DegradedFraction       simLatencyRange `yaml:"degraded_fraction"`
	DegradedLatencyScale   simLatencyRange `yaml:"degraded_latency_scale"`
	DegradedBandwidthScale simLatencyRange `yaml:"degraded_bandwidth_scale"`
	DegradedLossAdd        simLatencyRange `yaml:"degraded_loss_add"`
}

// The single synthetic region used by the simulator.
type simLatencyRegionConfig struct {
	Country     string `yaml:"country"`
	CountryCode string `yaml:"country_code"`
	Region      string `yaml:"region"`
	City        string `yaml:"city"`
}

// The disjoint benchmarking subnets used for providers and clients.
type simLatencySubnetConfig struct {
	Provider string `yaml:"provider"`
	Client   string `yaml:"client"`
}

// The synthetic origin-site distribution.
type simLatencySiteConfig struct {
	MeanDepth         float64 `yaml:"mean_depth"`
	Branching         float64 `yaml:"branching"`
	MinBodyBytes      int     `yaml:"min_body_bytes"`
	MaxBodyBytes      int     `yaml:"max_body_bytes"`
	LargeFraction     float64 `yaml:"large_fraction,omitempty"`
	LargeMinBodyBytes int     `yaml:"large_min_body_bytes,omitempty"`
	LargeMaxBodyBytes int     `yaml:"large_max_body_bytes,omitempty"`
}

// The fixed client pool and arrival process.
type simLatencyClientsConfig struct {
	PoolSize            int     `yaml:"pool_size"`
	MeanPerMinute       float64 `yaml:"mean_per_minute"`
	BalanceBytes        int64   `yaml:"balance_bytes"`
	ConnectionsPerCrawl int     `yaml:"connections_per_crawl"`
	QualityWindowSize   int     `yaml:"quality_window_size,omitempty"`
}

// The population size, grouping, and weighted network-condition modes.
type simLatencyProvidersConfig struct {
	Count                int                          `yaml:"count"`
	FleetNetworkFraction float64                      `yaml:"fleet_network_fraction"`
	FleetNetworkCount    int                          `yaml:"fleet_network_count"`
	Mixture              []simLatencyMixtureComponent `yaml:"mixture"`
}

// One fully sampled provider identity and network profile.
type simLatencyProviderEntry struct {
	Index                  int     `yaml:"i"`
	Ip                     string  `yaml:"ip"`
	ClientId               string  `yaml:"client_id"`
	NetworkId              string  `yaml:"network_id"`
	DeviceId               string  `yaml:"device_id"`
	UserId                 string  `yaml:"user_id"`
	UserType               string  `yaml:"user_type"`
	Component              string  `yaml:"component"`
	Seed                   int64   `yaml:"seed"`
	LatencyMillis          float64 `yaml:"latency_ms"`
	JitterMillis           float64 `yaml:"jitter_ms"`
	BandwidthBps           int64   `yaml:"bandwidth_bps"`
	Loss                   float64 `yaml:"loss"`
	MaxConnections         int     `yaml:"max_connections"`
	UptimeSeconds          float64 `yaml:"uptime_s"`
	DowntimeSeconds        float64 `yaml:"downtime_s"`
	DegradedFraction       float64 `yaml:"degraded_fraction"`
	DegradedLatencyScale   float64 `yaml:"degraded_latency_scale"`
	DegradedBandwidthScale float64 `yaml:"degraded_bandwidth_scale"`
	DegradedLossAdd        float64 `yaml:"degraded_loss_add"`
}

// The complete serialized workload consumed by every simulator process.
type simLatencyWorkload struct {
	Seed      int64                     `yaml:"seed"`
	Region    simLatencyRegionConfig    `yaml:"region"`
	Subnets   simLatencySubnetConfig    `yaml:"subnets"`
	Site      simLatencySiteConfig      `yaml:"site"`
	Clients   simLatencyClientsConfig   `yaml:"clients"`
	Providers simLatencyProvidersConfig `yaml:"providers"`
	Fleet     []simLatencyProviderEntry `yaml:"fleet"`
}

// A reproducible random source private to one generated artifact.
type simLatencyWorkloadRng struct {
	random *mathrand.Rand
}

// A sequential address iterator bounded by one benchmark prefix.
type simLatencyIpIterator struct {
	address netip.Addr
	prefix  netip.Prefix
}

// Produces the canonical providers.yml bytes without using evaluator-host
// executables, paths, or mutable state.
func GenerateSimLatencyWorkload(
	seed int64,
	providerCount int,
	clientPoolSize int,
	meanPerMinute float64,
	qualityWindowSize int,
) ([]byte, error) {
	if providerCount <= 0 {
		return nil, errors.New("provider count must be positive")
	}
	if clientPoolSize <= 0 || meanPerMinute <= 0 {
		return nil, errors.New("client pool and arrival rate must be positive")
	}
	if qualityWindowSize < 0 || 32 < qualityWindowSize {
		return nil, errors.New("quality window size must be in 0..32")
	}
	workload := defaultSimLatencyWorkload(seed, providerCount, clientPoolSize, meanPerMinute)
	workload.Clients.QualityWindowSize = qualityWindowSize
	if err := generateSimLatencyFleet(workload); err != nil {
		return nil, err
	}
	return yaml.Marshal(workload)
}

// Defines the frozen provider mixture and the non-random workload shape.
func defaultSimLatencyWorkload(
	seed int64,
	providerCount int,
	clientPoolSize int,
	meanPerMinute float64,
) *simLatencyWorkload {
	return &simLatencyWorkload{
		Seed: seed,
		Region: simLatencyRegionConfig{
			Country: "Sim", CountryCode: "zz", Region: "Sim", City: "Sim",
		},
		Subnets: simLatencySubnetConfig{
			Provider: "198.18.0.0/15", Client: "198.20.0.0/16",
		},
		Site: simLatencySiteConfig{
			MeanDepth: 4, Branching: 3,
			MinBodyBytes: 4 * 1024, MaxBodyBytes: 512 * 1024,
			LargeFraction: 0.25, LargeMinBodyBytes: 2 * 1024 * 1024,
			LargeMaxBodyBytes: 6 * 1024 * 1024,
		},
		Clients: simLatencyClientsConfig{
			PoolSize: clientPoolSize, MeanPerMinute: meanPerMinute,
			BalanceBytes:        int64(1024) * 1024 * 1024 * 1024,
			ConnectionsPerCrawl: 6,
		},
		Providers: simLatencyProvidersConfig{
			Count: providerCount, FleetNetworkFraction: 0.2,
			FleetNetworkCount: 50, Mixture: defaultSimLatencyMixture(),
		},
	}
}

// Defines the calibrated population used by the competition baseline.
func defaultSimLatencyMixture() []simLatencyMixtureComponent {
	return []simLatencyMixtureComponent{
		{
			Name: "residential-good", Weight: 0.45, UserType: "consumer",
			LatencyMillis: simLatencyRange{Min: 10, Max: 40}, JitterMillis: simLatencyRange{Min: 0, Max: 5},
			BandwidthMbps: simLatencyRange{Min: 20, Max: 150}, Loss: simLatencyRange{Min: 0, Max: 0.001},
			MaxConnections: simLatencyRange{Min: 8, Max: 32},
			UptimeSeconds:  simLatencyRange{Min: 1800, Max: 14400}, DowntimeSeconds: simLatencyRange{Min: 5, Max: 60},
			DegradedFraction:     simLatencyRange{Min: 0, Max: 0.1},
			DegradedLatencyScale: simLatencyRange{Min: 1.5, Max: 3}, DegradedBandwidthScale: simLatencyRange{Min: 0.3, Max: 0.7},
			DegradedLossAdd: simLatencyRange{Min: 0, Max: 0.01},
		},
		{
			Name: "mobile-variable", Weight: 0.25, UserType: "consumer",
			LatencyMillis: simLatencyRange{Min: 40, Max: 150}, JitterMillis: simLatencyRange{Min: 5, Max: 40},
			BandwidthMbps: simLatencyRange{Min: 2, Max: 40}, Loss: simLatencyRange{Min: 0.001, Max: 0.02},
			MaxConnections: simLatencyRange{Min: 4, Max: 12},
			UptimeSeconds:  simLatencyRange{Min: 300, Max: 3600}, DowntimeSeconds: simLatencyRange{Min: 10, Max: 120},
			DegradedFraction:     simLatencyRange{Min: 0.1, Max: 0.4},
			DegradedLatencyScale: simLatencyRange{Min: 2, Max: 5}, DegradedBandwidthScale: simLatencyRange{Min: 0.1, Max: 0.5},
			DegradedLossAdd: simLatencyRange{Min: 0.01, Max: 0.05},
		},
		{
			Name: "business-fiber", Weight: 0.15, UserType: "business",
			LatencyMillis: simLatencyRange{Min: 5, Max: 25}, JitterMillis: simLatencyRange{Min: 0, Max: 4},
			BandwidthMbps: simLatencyRange{Min: 50, Max: 250}, Loss: simLatencyRange{Min: 0, Max: 0.001},
			MaxConnections: simLatencyRange{Min: 24, Max: 64},
			UptimeSeconds:  simLatencyRange{Min: 3600, Max: 28800}, DowntimeSeconds: simLatencyRange{Min: 5, Max: 60},
			DegradedFraction:     simLatencyRange{Min: 0, Max: 0.08},
			DegradedLatencyScale: simLatencyRange{Min: 1.3, Max: 2.5}, DegradedBandwidthScale: simLatencyRange{Min: 0.4, Max: 0.8},
			DegradedLossAdd: simLatencyRange{Min: 0, Max: 0.005},
		},
		{
			Name: "hosting-fast", Weight: 0.15, UserType: "hosting",
			LatencyMillis: simLatencyRange{Min: 2, Max: 20}, JitterMillis: simLatencyRange{Min: 0, Max: 3},
			BandwidthMbps: simLatencyRange{Min: 200, Max: 600}, Loss: simLatencyRange{Min: 0, Max: 0.0005},
			MaxConnections: simLatencyRange{Min: 32, Max: 96},
			UptimeSeconds:  simLatencyRange{Min: 7200, Max: 86400}, DowntimeSeconds: simLatencyRange{Min: 2, Max: 20},
			DegradedFraction:     simLatencyRange{Min: 0, Max: 0.05},
			DegradedLatencyScale: simLatencyRange{Min: 1.2, Max: 2}, DegradedBandwidthScale: simLatencyRange{Min: 0.5, Max: 0.9},
			DegradedLossAdd: simLatencyRange{Min: 0, Max: 0.002},
		},
	}
}

// Samples every concrete provider from the workload's weighted mixture.
func generateSimLatencyFleet(workload *simLatencyWorkload) error {
	providerPrefix, err := netip.ParsePrefix(workload.Subnets.Provider)
	if err != nil {
		return fmt.Errorf("provider subnet: %w", err)
	}
	totalWeight := 0.0
	for _, component := range workload.Providers.Mixture {
		totalWeight += component.Weight
	}
	if totalWeight <= 0 {
		return errors.New("mixture weights sum to zero")
	}
	random := newSimLatencyWorkloadRng(int64(workload.Seed))
	fleetNetworkIds := make([]server.Id, workload.Providers.FleetNetworkCount)
	for i := range fleetNetworkIds {
		fleetNetworkIds[i] = random.id()
	}
	addresses := newSimLatencyIpIterator(providerPrefix)
	workload.Fleet = make([]simLatencyProviderEntry, 0, workload.Providers.Count)
	for i := 0; i < workload.Providers.Count; i++ {
		address, ok := addresses.next()
		if !ok {
			return fmt.Errorf("provider subnet %s exhausted at %d providers", workload.Subnets.Provider, i)
		}
		component := pickSimLatencyComponent(workload.Providers.Mixture, totalWeight, random)
		var networkId server.Id
		if random.float64() < workload.Providers.FleetNetworkFraction && 0 < len(fleetNetworkIds) {
			networkId = fleetNetworkIds[random.intn(len(fleetNetworkIds))]
		} else {
			networkId = random.id()
		}
		userType := component.UserType
		if userType == "" {
			userType = "consumer"
		}
		workload.Fleet = append(workload.Fleet, simLatencyProviderEntry{
			Index: i, Ip: address.String(), ClientId: random.id().String(), NetworkId: networkId.String(),
			DeviceId: random.id().String(), UserId: random.id().String(), UserType: userType,
			Component: component.Name, Seed: random.int63(),
			LatencyMillis: component.LatencyMillis.sample(random), JitterMillis: component.JitterMillis.sample(random),
			BandwidthBps: int64(component.BandwidthMbps.sample(random) * 1e6 / 8), Loss: component.Loss.sample(random),
			MaxConnections: int(math.Round(component.MaxConnections.sample(random))),
			UptimeSeconds:  component.UptimeSeconds.sample(random), DowntimeSeconds: component.DowntimeSeconds.sample(random),
			DegradedFraction:       component.DegradedFraction.sample(random),
			DegradedLatencyScale:   scaleSimLatencyOrOne(component.DegradedLatencyScale.sample(random)),
			DegradedBandwidthScale: scaleSimLatencyOrOne(component.DegradedBandwidthScale.sample(random)),
			DegradedLossAdd:        component.DegradedLossAdd.sample(random),
		})
	}
	return nil
}

// Samples one value from a closed interval.
func (self simLatencyRange) sample(random *simLatencyWorkloadRng) float64 {
	if self.Max <= self.Min {
		return self.Min
	}
	return self.Min + random.float64()*(self.Max-self.Min)
}

// Constructs the isolated deterministic random source.
func newSimLatencyWorkloadRng(seed int64) *simLatencyWorkloadRng {
	return &simLatencyWorkloadRng{random: mathrand.New(mathrand.NewSource(seed))}
}

// Returns the next uniform float.
func (self *simLatencyWorkloadRng) float64() float64 {
	return self.random.Float64()
}

// Returns the next non-negative 63-bit integer.
func (self *simLatencyWorkloadRng) int63() int64 {
	return self.random.Int63()
}

// Returns a deterministic opaque id.
func (self *simLatencyWorkloadRng) id() server.Id {
	var bytes [16]byte
	for i := range bytes {
		bytes[i] = byte(self.random.Intn(256))
	}
	id, _ := server.IdFromBytes(bytes[:])
	return id
}

// Returns a bounded integer and treats a non-positive bound as zero.
func (self *simLatencyWorkloadRng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return self.random.Intn(n)
}

// Constructs an iterator starting at the first host address.
func newSimLatencyIpIterator(prefix netip.Prefix) *simLatencyIpIterator {
	return &simLatencyIpIterator{address: prefix.Masked().Addr().Next(), prefix: prefix}
}

// Returns one address while it remains within the configured prefix.
func (self *simLatencyIpIterator) next() (netip.Addr, bool) {
	if !self.prefix.Contains(self.address) {
		return netip.Addr{}, false
	}
	address := self.address
	self.address = self.address.Next()
	return address, true
}

// Selects one component by cumulative weight.
func pickSimLatencyComponent(
	mixture []simLatencyMixtureComponent,
	totalWeight float64,
	random *simLatencyWorkloadRng,
) simLatencyMixtureComponent {
	target := random.float64() * totalWeight
	currentWeight := 0.0
	for _, component := range mixture {
		currentWeight += component.Weight
		if target < currentWeight {
			return component
		}
	}
	return mixture[len(mixture)-1]
}

// Keeps invalid non-positive degradation scales neutral.
func scaleSimLatencyOrOne(value float64) float64 {
	if value <= 0 {
		return 1
	}
	return value
}
