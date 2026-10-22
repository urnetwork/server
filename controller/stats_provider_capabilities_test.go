package controller

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/model"
)

// A failed read withdraws the whole census, and a later read replaces every
// series, so a key exchange group that left the fleet stops publishing.
func TestStatsProviderCapabilitiesWithdrawFailureAndReplaceGroups(t *testing.T) {
	registry := prometheus.NewRegistry()
	pairs := newStatsGaugeVec("test_provider_capabilities", "test", "e2e", "window")
	ages := newStatsGaugeVec("test_provider_capability_evidence_age", "test", "age")
	keyExchanges := newStatsGaugeVec("test_provider_capability_key_exchanges", "test", "key_exchange")
	observed := newStatsGauge("test_provider_capabilities_observed", "test")
	registry.MustRegister(pairs.gauge, ages.gauge, keyExchanges.gauge, observed.gauge)
	pairs.registered, ages.registered, keyExchanges.registered, observed.registered = true, true, true, true

	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	stats := func(keyExchange string, observedAt time.Time) *model.ProviderCapabilityStats {
		s := &model.ProviderCapabilityStats{
			Pairs:        map[[2]string]int64{},
			Ages:         map[string]int64{},
			KeyExchanges: map[string]int64{keyExchange: 2, model.ProviderCapabilityKeyExchangeOther: 0},
			ObservedAt:   observedAt,
		}
		for _, pair := range model.ProviderCapabilityPairs {
			s.Pairs[pair] = 0
		}
		s.Pairs[[2]string{model.ProviderCapabilityE2ePostQuantum, model.ProviderCapabilityWindowResizable}] = 2
		for _, age := range model.ProviderCapabilityAges {
			s.Ages[age] = 1
		}
		return s
	}

	statsPublishProviderCapabilities(pairs, ages, keyExchanges, observed, stats("X25519MLKEM768", now), nil)
	if testutil.CollectAndCount(pairs.gauge) != len(model.ProviderCapabilityPairs) || testutil.ToFloat64(pairs.gauge.WithLabelValues("post_quantum", "resizable")) != 2 {
		t.Fatal("census pairs not published with zeros")
	}
	if testutil.CollectAndCount(ages.gauge) != len(model.ProviderCapabilityAges) || testutil.ToFloat64(observed.gauge) != float64(now.Unix()) {
		t.Fatal("census ages or snapshot time not published")
	}

	statsPublishProviderCapabilities(pairs, ages, keyExchanges, observed, stats("X25519", now.Add(time.Minute)), nil)
	if testutil.CollectAndCount(keyExchanges.gauge) != 2 || testutil.ToFloat64(keyExchanges.gauge.WithLabelValues("X25519")) != 2 {
		t.Fatal("departed key exchange group kept publishing")
	}

	statsPublishProviderCapabilities(pairs, ages, keyExchanges, observed, nil, errors.New("synthetic read unavailable"))
	if testutil.CollectAndCount(pairs.gauge) != 0 || testutil.CollectAndCount(ages.gauge) != 0 || testutil.CollectAndCount(keyExchanges.gauge) != 0 || !math.IsNaN(testutil.ToFloat64(observed.gauge)) {
		t.Fatal("failed read retained a publishable census")
	}

	statsPublishProviderCapabilities(pairs, ages, keyExchanges, observed, stats("X25519MLKEM768", now.Add(2*time.Minute)), nil)
	if testutil.CollectAndCount(pairs.gauge) != len(model.ProviderCapabilityPairs) || testutil.ToFloat64(observed.gauge) != float64(now.Add(2*time.Minute).Unix()) {
		t.Fatal("successful read did not restore the census")
	}
}
