// Private version distributions for the providers dashboard.
package controller

import (
	"context"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server/model"
)

var statsDeviceVersionsGauge = newStatsGaugeVec(
	"device_versions",
	"Connected top-level clients by population and reported device app version; top 50 plus unknown and other",
	"population", "version",
)

var statsDeviceVersionsObservedGauge = newStatsGauge(
	"device_versions_observed_at_seconds",
	"Database snapshot time of the connected device version distribution",
)

func statsRefreshDeviceVersions(ctx context.Context) {
	counts, observedAt, err := model.ReadDeviceVersionStats(ctx)
	statsPublishDeviceVersions(statsDeviceVersionsGauge, statsDeviceVersionsObservedGauge, counts, observedAt, err)
}

func statsPublishDeviceVersions(versions *statsGaugeVec, observed *statsGauge, counts []model.DeviceVersionCount, observedAt time.Time, err error) {
	if err != nil {
		versions.replace(nil)
		observed.unset()
		glog.Infof("[stats]device versions unavailable: %v\n", err)
		return
	}
	values := make([]statsLabeledValue, 0, len(counts))
	for _, count := range counts {
		values = append(values, statsLabeledValue{labelValues: []string{count.Population, count.Version}, value: float64(count.Count)})
	}
	versions.replace(values)
	observed.set(float64(observedAt.UnixMicro()) / 1e6)
}
