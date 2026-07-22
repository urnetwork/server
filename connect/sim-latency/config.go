package main

// providers.yml: the locked ground truth for a simulation run.
//
// `init` samples the mixture once and writes the full file — the mixture
// definition plus one concrete entry per provider (its ip, identities,
// user type, sampled network conditions, and dynamics seed). `run` and
// `fleet` read it back verbatim, so every run of the same file drives the
// same fleet. Entries are explicit (not regenerated from a seed) so exported
// FindProviders2 samples can be joined against provider ground truth.

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Range is a closed interval a value is sampled uniformly from.
type Range struct {
	Min float64 `yaml:"min"`
	Max float64 `yaml:"max"`
}

func (self Range) sample(r *rng) float64 {
	if self.Max <= self.Min {
		return self.Min
	}
	return self.Min + r.float64()*(self.Max-self.Min)
}

// MixtureComponent is one mode of the provider population: a weighted region
// of the network-condition parameter space. A provider is assigned a
// component by weight, then its concrete parameters are sampled from the
// component's ranges.
type MixtureComponent struct {
	Name   string  `yaml:"name"`
	Weight float64 `yaml:"weight"`
	// hosting/business providers carry a worse net-type score in matchmaking
	UserType string `yaml:"user_type"` // consumer | business | hosting

	LatencyMillis Range `yaml:"latency_ms"`
	JitterMillis  Range `yaml:"jitter_ms"`
	BandwidthMbps Range `yaml:"bandwidth_mbps"`
	// packet loss fraction (modeled as read-stall probability per chunk)
	Loss Range `yaml:"loss"`
	// concurrent connection cap (ulimit): the max simultaneous tunneled
	// flows the provider's egress NAT serves. Over the cap the NAT admits
	// the new flow and lru-evicts the idle-most established flow (which the
	// victim sees as a reset). Hidden ground truth like bandwidth — clients
	// discover capacity through failures, so routing all traffic to the
	// single best provider is penalized. 0 = unlimited.
	MaxConnections Range `yaml:"max_connections"`

	// churn: mean up/down durations (seconds). A provider cycles connected for
	// ~uptime then offline for ~downtime, driving the real reliability machinery.
	UptimeSeconds   Range `yaml:"uptime_s"`
	DowntimeSeconds Range `yaml:"downtime_s"`

	// dynamics: fraction of time the provider sits in a degraded regime, where
	// latency/loss are scaled up and bandwidth down (a 2-state modulation).
	DegradedFraction       Range `yaml:"degraded_fraction"`
	DegradedLatencyScale   Range `yaml:"degraded_latency_scale"`
	DegradedBandwidthScale Range `yaml:"degraded_bandwidth_scale"`
	DegradedLossAdd        Range `yaml:"degraded_loss_add"`
}

// RegionConfig is the single fake region every provider and client lives in.
type RegionConfig struct {
	Country     string `yaml:"country"`
	CountryCode string `yaml:"country_code"`
	Region      string `yaml:"region"`
	City        string `yaml:"city"`
}

// SubnetConfig allocates the testing subnets. Providers and clients get
// unique ips from these, mapped to the fake region by the ip_overrides hook.
type SubnetConfig struct {
	Provider string `yaml:"provider"`
	Client   string `yaml:"client"`
}

// SiteConfig shapes the fake origin site's loading tree.
type SiteConfig struct {
	// mean crawl depth (K): the tree terminates in expectation at this depth
	MeanDepth float64 `yaml:"mean_depth"`
	// mean child links per page
	Branching float64 `yaml:"branching"`
	// response body size range (bytes), sampled per page
	MinBodyBytes int `yaml:"min_body_bytes"`
	MaxBodyBytes int `yaml:"max_body_bytes"`
}

// ClientsConfig shapes the client load.
type ClientsConfig struct {
	// pool of pre-provisioned client identities reused by arrivals
	PoolSize int `yaml:"pool_size"`
	// mean arrivals per minute (M), drawn as a per-second Poisson process
	MeanPerMinute float64 `yaml:"mean_per_minute"`
	// transfer balance granted to each client network (bytes)
	BalanceBytes int64 `yaml:"balance_bytes"`
	// browser-like parallel connections per page crawl
	ConnectionsPerCrawl int `yaml:"connections_per_crawl"`
}

// ProvidersConfig defines the fleet size, network grouping, and the mixture.
type ProvidersConfig struct {
	Count int `yaml:"count"`
	// fraction of providers grouped into shared multi-provider "fleet"
	// networks (the rest are singleton networks). reliability_weight is
	// network-share adjusted, so the grouping shape matters.
	FleetNetworkFraction float64 `yaml:"fleet_network_fraction"`
	FleetNetworkCount    int     `yaml:"fleet_network_count"`

	Mixture []MixtureComponent `yaml:"mixture"`
}

// ProviderEntry is one concrete provider: identity, ip, and sampled network
// conditions. Ids are ULID strings so run and fleet share the exact same
// identities and samples can be joined to ground truth.
type ProviderEntry struct {
	Index     int    `yaml:"i"`
	Ip        string `yaml:"ip"`
	ClientId  string `yaml:"client_id"`
	NetworkId string `yaml:"network_id"`
	DeviceId  string `yaml:"device_id"`
	UserId    string `yaml:"user_id"`
	UserType  string `yaml:"user_type"`
	Component string `yaml:"component"`
	Seed      int64  `yaml:"seed"`

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

// Config is the whole providers.yml.
type Config struct {
	Seed      int64           `yaml:"seed"`
	Region    RegionConfig    `yaml:"region"`
	Subnets   SubnetConfig    `yaml:"subnets"`
	Site      SiteConfig      `yaml:"site"`
	Clients   ClientsConfig   `yaml:"clients"`
	Providers ProvidersConfig `yaml:"providers"`
	Fleet     []ProviderEntry `yaml:"fleet"`
}

func LoadConfig(path string) (*Config, error) {
	configBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config Config
	if err := yaml.Unmarshal(configBytes, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func SaveConfig(path string, config *Config) error {
	configBytes, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	return os.WriteFile(path, configBytes, 0o644)
}

// shard returns the fleet entries this shard is responsible for (of n shards).
func (self *Config) shard(index int, count int) []ProviderEntry {
	if count <= 1 {
		return self.Fleet
	}
	entries := []ProviderEntry{}
	for _, entry := range self.Fleet {
		if entry.Index%count == index {
			entries = append(entries, entry)
		}
	}
	return entries
}

// ipOverridesSettings renders the ip_overrides settings block that maps the
// testing subnets to the fake region, for injection into the site settings.
func (self *Config) ipOverridesSettings() []map[string]any {
	entry := func(subnet string, userType string) map[string]any {
		return map[string]any{
			"subnet":       subnet,
			"country":      self.Region.Country,
			"country_code": self.Region.CountryCode,
			"region":       self.Region.Region,
			"city":         self.Region.City,
			"hosting":      userType == "hosting",
		}
	}
	return []map[string]any{
		entry(self.Subnets.Provider, "consumer"),
		entry(self.Subnets.Client, "consumer"),
	}
}

func (self *Config) validate() error {
	if self.Providers.Count <= 0 {
		return fmt.Errorf("providers.count must be > 0")
	}
	if len(self.Fleet) == 0 {
		return fmt.Errorf("fleet is empty; run `sim-latency init` first")
	}
	if len(self.Providers.Mixture) == 0 {
		return fmt.Errorf("providers.mixture is empty")
	}
	return nil
}
