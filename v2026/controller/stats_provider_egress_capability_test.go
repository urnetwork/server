package controller

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026/model"
)

// The classified decision's capability and post-quantum gauges
// (FP2-PROTOCOL.md §8.1): every bucket, label pair and signal is published
// each refresh, zero included, with the snapshot's counts.
func TestStatsPublishProviderEgressCapabilityAndPostQuantum(t *testing.T) {
	counts := statsProviderEgressTestCounts()
	counts.CapabilityCounts[model.RankModeSpeed][model.ProviderCapabilityIpIpv4][model.ProviderCapabilityProtocolTcp] = 20657
	counts.CapabilityCounts[model.RankModeQuality][model.ProviderCapabilityIpDualstack][model.ProviderCapabilityProtocolDualproto] = 280
	counts.PostQuantumCounts[model.ProviderEgressBucketOnline][model.ProviderPostQuantumSignalPositive] = 37100
	statsPublishProviderEgressCounts(counts)

	capability := func(bucket string, ip string, protocol string) float64 {
		return testutil.ToFloat64(statsProviderEgressCapabilityGauge.gauge.WithLabelValues(bucket, ip, protocol))
	}
	if got := capability(model.RankModeSpeed, model.ProviderCapabilityIpIpv4, model.ProviderCapabilityProtocolTcp); got != 20657 {
		t.Errorf("speed ipv4+tcp %v", got)
	}
	if got := capability(model.RankModeQuality, model.ProviderCapabilityIpDualstack, model.ProviderCapabilityProtocolDualproto); got != 280 {
		t.Errorf("quality dualstack+dualproto %v", got)
	}
	if got := capability(model.ProviderEgressBucketOnline, model.ProviderCapabilityIpDualstack, model.ProviderCapabilityProtocolTcp); got != 0 {
		t.Errorf("online dualstack+tcp %v, want a published zero", got)
	}
	if series := testutil.CollectAndCount(statsProviderEgressCapabilityGauge.gauge, "urnetwork_stats_provider_egress_capability"); series != 3*2*2 {
		t.Errorf("%d capability series, want 12", series)
	}

	postQuantum := func(bucket string, signal string) float64 {
		return testutil.ToFloat64(statsProviderPostQuantumGauge.gauge.WithLabelValues(bucket, signal))
	}
	if got := postQuantum(model.ProviderEgressBucketOnline, model.ProviderPostQuantumSignalPositive); got != 37100 {
		t.Errorf("online positive %v", got)
	}
	if got := postQuantum(model.RankModeQuality, model.ProviderPostQuantumSignalNone); got != 0 {
		t.Errorf("quality none %v, want a published zero", got)
	}
	if series := testutil.CollectAndCount(statsProviderPostQuantumGauge.gauge, "urnetwork_stats_provider_post_quantum"); series != 3*3 {
		t.Errorf("%d post-quantum series, want 9", series)
	}

	// a later refresh replaces every value, a zero included
	counts.CapabilityCounts[model.RankModeSpeed][model.ProviderCapabilityIpIpv4][model.ProviderCapabilityProtocolTcp] = 0
	statsPublishProviderEgressCounts(counts)
	if got := capability(model.RankModeSpeed, model.ProviderCapabilityIpIpv4, model.ProviderCapabilityProtocolTcp); got != 0 {
		t.Errorf("speed ipv4+tcp %v after the refresh, want 0", got)
	}
	if series := testutil.CollectAndCount(statsProviderEgressCapabilityGauge.gauge, "urnetwork_stats_provider_egress_capability"); series != 12 {
		t.Errorf("%d capability series after the refresh, want 12", series)
	}
}
