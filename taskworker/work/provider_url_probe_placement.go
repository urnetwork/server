// URL shard passes are placed on taskworker instances by their configured
// geometry. Each pass owns its own worker pool, shard identity, funded balance
// and transport budget; instances only bound how many of those pools they
// hold at once.
package work

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog"
)

// Two passes of the historical 64-worker geometry. On Main a 64-worker pass
// added about 4-7k goroutines and under one core to its taskworker.
const defaultProviderUrlProbeInstanceConcurrency = 128

var urlProbeInstancePassLimit = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_url_probe_instance_pass_limit",
	Help: "URL shard passes this taskworker instance may hold at once, read from the probe geometry at startup",
})

func init() {
	prometheus.MustRegister(urlProbeInstancePassLimit)
}

// The claim limit for URL shard passes on one taskworker instance: as many
// passes as fit in instance_url_concurrency at the configured per-pass
// concurrency, at most the shard count. A fixed limit of one let the fleet run
// only as many passes as it had instances, so a geometry with more shards than
// live instances lost the remaining shards' workers without any error.
//
// A pass larger than the instance concurrency still runs alone, as before.
// Disabled, legacy-only or unreadable settings keep the one-pass limit. The
// task worker snapshots claim limits when it starts, so a changed geometry
// takes effect at the next taskworker restart.
func ProviderEgressProbeInstancePassLimit() int {
	settings, err := getProviderEgressProbeSettings()
	limit := 1
	if err != nil {
		glog.Errorf("[egress]URL probe placement keeps one pass per instance; settings are unavailable: %v\n", err)
	} else {
		limit = providerUrlProbeInstancePassLimit(settings)
	}
	urlProbeInstancePassLimit.Set(float64(limit))
	return limit
}

// The pure placement rule over one validated settings snapshot.
func providerUrlProbeInstancePassLimit(settings providerEgressProbeSettings) int {
	if !settings.Enabled || settings.UrlProbe == nil {
		return 1
	}
	concurrency := settings.UrlProbe.Concurrency
	if concurrency < 1 || settings.InstanceUrlConcurrency < concurrency || settings.ShardCount < 1 {
		return 1
	}
	return min(settings.InstanceUrlConcurrency/concurrency, settings.ShardCount)
}
