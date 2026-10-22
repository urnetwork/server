// The provider capability census for the capability probes dashboard
// (grafana/dashboards/capability-probes.json): the connected, valid public
// providers by what the capability probe last read of them (capprobe). Every
// label is a bounded enum, and the key exchange groups are capped, so the
// series count does not grow with the fleet.
package controller

import (
	"context"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server/model"
)

var statsProviderCapabilitiesGauge = newStatsGaugeVec(
	"provider_capabilities",
	"Connected valid public providers by the capability probe's end-to-end reading (post_quantum, classical, no_responder, no_evidence, stale_evidence) and window reading (resizable, fixed, unknown without fresh evidence)",
	"e2e", "window",
)

var statsProviderCapabilityAgeGauge = newStatsGaugeVec(
	"provider_capability_evidence_age",
	"Connected valid public providers by the age of their capability evidence: 1d, 3d, 7d, stale (seven days or older) or none",
	"age",
)

var statsProviderCapabilityKeyExchangeGauge = newStatsGaugeVec(
	"provider_capability_key_exchanges",
	"Connected valid public providers with fresh negotiated end-to-end sessions by key exchange group; the eight largest plus other",
	"key_exchange",
)

var statsProviderCapabilitiesObservedGauge = newStatsGauge(
	"provider_capabilities_observed_at_seconds",
	"Database snapshot time of the provider capability census",
)

func statsRefreshProviderCapabilities(ctx context.Context) {
	stats, err := model.ReadProviderCapabilityStats(ctx)
	statsPublishProviderCapabilities(
		statsProviderCapabilitiesGauge,
		statsProviderCapabilityAgeGauge,
		statsProviderCapabilityKeyExchangeGauge,
		statsProviderCapabilitiesObservedGauge,
		stats,
		err,
	)
}

// A failed read withdraws the whole census rather than leaving the last one
// standing as if it were current.
func statsPublishProviderCapabilities(
	pairs *statsGaugeVec,
	ages *statsGaugeVec,
	keyExchanges *statsGaugeVec,
	observed *statsGauge,
	stats *model.ProviderCapabilityStats,
	err error,
) {
	if err != nil || stats == nil {
		pairs.replace(nil)
		ages.replace(nil)
		keyExchanges.replace(nil)
		observed.unset()
		if err != nil {
			glog.Infof("[stats]provider capabilities unavailable: %v\n", err)
		}
		return
	}
	pairValues := make([]statsLabeledValue, 0, len(stats.Pairs))
	for pair, count := range stats.Pairs {
		pairValues = append(pairValues, statsLabeledValue{labelValues: []string{pair[0], pair[1]}, value: float64(count)})
	}
	pairs.replace(pairValues)
	ageValues := make([]statsLabeledValue, 0, len(stats.Ages))
	for age, count := range stats.Ages {
		ageValues = append(ageValues, statsLabeledValue{labelValues: []string{age}, value: float64(count)})
	}
	ages.replace(ageValues)
	keyExchangeValues := make([]statsLabeledValue, 0, len(stats.KeyExchanges))
	for keyExchange, count := range stats.KeyExchanges {
		keyExchangeValues = append(keyExchangeValues, statsLabeledValue{labelValues: []string{keyExchange}, value: float64(count)})
	}
	keyExchanges.replace(keyExchangeValues)
	observed.set(float64(stats.ObservedAt.UnixMicro()) / float64(time.Second/time.Microsecond))
}
