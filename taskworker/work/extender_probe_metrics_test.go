package work

// The false-revocation rate is visible (extender_probe_metrics.go): the held
// failures and passes, the revocations, and the restores with how long the
// extender was revoked, each counted where the pass decides it.

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The process-wide counters, so a test reads what it caused.
func readTestExtenderProbeMetrics(t testing.TB) map[string]float64 {
	t.Helper()
	values := map[string]float64{}
	for _, ipVersion := range []string{"4", "6"} {
		for _, set := range []string{extenderProbeSetActive, extenderProbeSetRecovery} {
			for _, result := range []string{extenderProbeResultOk, extenderProbeResultFailed, extenderProbeResultHeld} {
				values["results "+ipVersion+" "+set+" "+result] = testutil.ToFloat64(extenderProbeResultsCounter.WithLabelValues(ipVersion, set, result))
			}
		}
		for _, verdict := range []string{extenderProbeVerdictCounted, extenderProbeVerdictCommonMode, extenderProbeVerdictControlFailed} {
			values["passes "+ipVersion+" "+verdict] = testutil.ToFloat64(extenderProbePassesCounter.WithLabelValues(ipVersion, verdict))
		}
		for _, result := range []string{extenderProbeControlOk, extenderProbeControlFailed, extenderProbeControlAbsent, extenderProbeControlUnknown} {
			values["control "+ipVersion+" "+result] = testutil.ToFloat64(extenderProbeControlCounter.WithLabelValues(ipVersion, result))
		}
		values["deactivations "+ipVersion] = testutil.ToFloat64(extenderProbeDeactivationsCounter.WithLabelValues(ipVersion))
		for _, kind := range []string{extenderProbeRestoreAddress, extenderProbeRestoreExtender} {
			values["restores "+ipVersion+" "+kind] = testutil.ToFloat64(extenderProbeRestoresCounter.WithLabelValues(ipVersion, kind))
		}
	}
	values["revocations"] = testutil.ToFloat64(extenderProbeRevocationsCounter)
	histogram := testExtenderProbeHistogram(t, extenderProbeRestoreRevokedSeconds)
	values["restore revoked count"] = float64(histogram.GetSampleCount())
	values["restore revoked sum"] = histogram.GetSampleSum()
	return values
}

func testExtenderProbeHistogram(t testing.TB, histogram prometheus.Histogram) *dto.Histogram {
	t.Helper()
	metric := &dto.Metric{}
	if err := histogram.Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetHistogram()
}

// The change in each counter since before, the unchanged left out.
func testExtenderProbeMetricsDelta(t testing.TB, before map[string]float64) map[string]float64 {
	t.Helper()
	delta := map[string]float64{}
	for name, value := range readTestExtenderProbeMetrics(t) {
		if change := value - before[name]; change != 0 {
			delta[name] = change
		}
	}
	return delta
}

func TestExtenderProbeMetricsCountTheVerdictsRevocationsAndRestores(t *testing.T) {
	world := newTestChurnWorld(t)
	stubTestChurnControl(t, func(ipVersion int) string {
		return testChurnControlAbsent
	})
	networkIds := []server.Id{}
	extenderIds := []server.Id{}
	for range 4 {
		networkId := server.NewId()
		networkIds = append(networkIds, networkId)
		for range 2 {
			extenderIds = append(extenderIds, world.store.addExtender(networkId, testExtenderChurnStart, 4))
		}
	}

	// a healthy pass
	before := readTestExtenderProbeMetrics(t)
	world.pass()
	connect.AssertEqual(t, testExtenderProbeMetricsDelta(t, before), map[string]float64{
		"results 4 active ok": 8,
		"passes 4 counted":    1,
		"control 4 absent":    2,
	})

	// a prober fault: every failure held
	world.setAnswer(testChurnAlwaysFail(errTestChurnProberUnreachable))
	before = readTestExtenderProbeMetrics(t)
	world.pass()
	connect.AssertEqual(t, testExtenderProbeMetricsDelta(t, before), map[string]float64{
		"results 4 active held": 8,
		"passes 4 common_mode":  1,
		"control 4 absent":      2,
	})

	// one operator's two extenders down for half an hour: revoked
	world.setAnswer(func(target *model.NetworkExtenderProbeTarget) error {
		if target.NetworkId == networkIds[0] {
			return errTestChurnExtenderDown
		}
		return nil
	})
	before = readTestExtenderProbeMetrics(t)
	for range ExtenderMaxConsecutiveProbeFailures {
		world.pass()
	}
	connect.AssertEqual(t, testExtenderProbeMetricsDelta(t, before), map[string]float64{
		"results 4 active ok":     6 * 6,
		"results 4 active failed": 6 * 2,
		"passes 4 counted":        6,
		"control 4 absent":        6 * 2,
		"deactivations 4":         2,
		"revocations":             2,
	})

	// back after ten minutes: one pass for their return fails, the next one
	// restores them
	world.pass()
	world.setAnswer(func(target *model.NetworkExtenderProbeTarget) error {
		return nil
	})
	before = readTestExtenderProbeMetrics(t)
	world.pass()
	delta := testExtenderProbeMetricsDelta(t, before)
	connect.AssertEqual(t, delta["results 4 recovery ok"], float64(2))
	connect.AssertEqual(t, delta["restores 4 extender"], float64(2))
	connect.AssertEqual(t, delta["restore revoked count"], float64(2))
	// revoked at the sixth failure, restored two passes later
	connect.AssertEqual(t, delta["restore revoked sum"], float64(2*2*ExtenderProbeTimeout.Seconds()))
	for _, extenderId := range extenderIds {
		connect.AssertEqual(t, world.store.extender(extenderId).active, true)
	}
}
