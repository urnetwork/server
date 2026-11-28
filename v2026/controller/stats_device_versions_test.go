package controller

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026/model"
)

// Counts disappear on failure or population changes; unknown is a counted
// bucket, never fabricated as zero coverage by losing a missing-device row.
func TestStatsDeviceVersionsWithdrawFailureAndExpiredVersions(t *testing.T) {
	registry := prometheus.NewRegistry()
	versions := newStatsGaugeVec("test_device_versions", "test", "population", "version")
	observed := newStatsGauge("test_device_versions_observed", "test")
	registry.MustRegister(versions.gauge, observed.gauge)
	versions.registered, observed.registered = true, true
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	counts := []model.DeviceVersionCount{{Population: "provider", Version: "2026.10.20-1", Count: 2}, {Population: "provider", Version: "unknown", Count: 3}}
	statsPublishDeviceVersions(versions, observed, counts, now, nil)
	if testutil.ToFloat64(versions.gauge.WithLabelValues("provider", "unknown")) != 3 {
		t.Fatal("unknown provider coverage disappeared")
	}
	statsPublishDeviceVersions(versions, observed, counts[1:], now.Add(time.Minute), nil)
	if testutil.CollectAndCount(versions.gauge) != 1 {
		t.Fatal("disconnected version kept being published")
	}
	statsPublishDeviceVersions(versions, observed, nil, time.Time{}, errors.New("synthetic read unavailable"))
	if testutil.CollectAndCount(versions.gauge) != 0 || !math.IsNaN(testutil.ToFloat64(observed.gauge)) {
		t.Fatal("failed read retained a publishable distribution")
	}
	statsPublishDeviceVersions(versions, observed, counts, now.Add(2*time.Minute), nil)
	if testutil.CollectAndCount(versions.gauge) != 2 || testutil.ToFloat64(observed.gauge) != float64(now.Add(2*time.Minute).Unix()) {
		t.Fatal("successful read did not restore version distribution")
	}
}
