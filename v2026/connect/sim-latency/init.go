package main

// This file keeps the simulator's internal workload constructors routed
// through the control-plane generator. The single implementation guarantees
// that API-created round artifacts and `sim-latency init` are byte-compatible.

import "fmt"

// Builds the default workload used by tests and local simulator components.
func defaultConfig(seed int64, providerCount int, clientPoolSize int, meanPerMinute float64) *Config {
	config, err := generatedConfig(seed, providerCount, clientPoolSize, meanPerMinute, 0)
	if err != nil {
		panic(fmt.Sprintf("default sim-latency workload: %s", err))
	}
	return config
}

// Populates a config that does not already contain its complete sampled fleet.
func generateFleet(config *Config) error {
	if config == nil {
		return fmt.Errorf("config is nil")
	}
	if len(config.Fleet) == config.Providers.Count && 0 < len(config.Fleet) {
		return nil
	}
	generated, err := generatedConfig(
		config.Seed,
		config.Providers.Count,
		config.Clients.PoolSize,
		config.Clients.MeanPerMinute,
		config.Clients.QualityWindowSize,
	)
	if err != nil {
		return err
	}
	config.Fleet = generated.Fleet
	return nil
}
