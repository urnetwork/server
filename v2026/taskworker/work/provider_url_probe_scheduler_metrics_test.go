package work

import (
	"context"
	"errors"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

func TestUrlProbeSchedulerMetricsCoherentLivePhases(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		metrics := newProviderUrlProbeSchedulerMetrics()
		a, b := metrics.begin(), metrics.begin()
		time.Sleep(time.Second)
		a.enter(urlSchedulerDue)
		b.enter(urlSchedulerWait)
		time.Sleep(2 * time.Second)
		before := metrics.snapshot()
		if before.active[urlSchedulerDue] != 1 || before.active[urlSchedulerWait] != 1 || before.seconds[urlSchedulerPrepare] != 2 || before.seconds[urlSchedulerDue] != 2 || before.seconds[urlSchedulerWait] != 2 {
			t.Fatalf("coherent live phases lost ownership: %+v", before)
		}
		a.stopAdmission(urlSchedulerReserve)
		a.stopAdmission(urlSchedulerCanceled)
		a.close()
		a.close()
		a.enter(urlSchedulerDispatch)
		a.claim(urlClaimAdmitted, 100)
		time.Sleep(time.Second)
		after := metrics.snapshot()
		if after.active[urlSchedulerDue] != 0 || after.active[urlSchedulerWait] != 1 || after.stops[urlSchedulerReserve] != 1 || after.stops[urlSchedulerCanceled] != 0 || after.claims[urlClaimAdmitted] != 0 || after.seconds[urlSchedulerWait] != 3 {
			t.Fatalf("retiring owner changed sibling or late callback: %+v", after)
		}
		b.stopAdmission(urlSchedulerEmpty)
		b.close()
		end := metrics.snapshot()
		var sum float64
		for _, value := range end.seconds {
			sum += value
		}
		if end.active != [7]int64{} || sum != 7 {
			t.Fatalf("phase residence/retirement lost: %+v sum=%v", end, sum)
		}
	})
}

func TestUrlProbeSchedulerMetricsPredeclareFiniteSchema(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	metrics := newProviderUrlProbeSchedulerMetrics()
	registry.MustRegister(metrics)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, family := range families {
		for _, metric := range family.Metric {
			count++
			if len(metric.Label) > 1 {
				t.Fatalf("crossed labels in %s", family.GetName())
			}
			for _, label := range metric.Label {
				name := label.GetName()
				if name != "phase" && name != "reason" && name != "outcome" && name != "disposition" {
					t.Fatalf("unexpected identity label %s", name)
				}
			}
		}
	}
	if count != 39 || len(families) != 7 {
		t.Fatalf("fixed metrics changed: series%d families%d", count, len(families))
	}
}

func TestUrlProbeSchedulerMetricsDescribeActualClaimAndFinalization(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, _ := urlAdmissionPass()
		before := urlProbeSchedulerMetrics.snapshot()
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) { time.Sleep(3 * time.Second); return nil, nil }
		pass.refreshFleet = func(context.Context) { time.Sleep(2 * time.Second) }
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		result, err := pass.run(ctx, args)
		after := urlProbeSchedulerMetrics.snapshot()
		if err != nil || result.Attempted != 0 || after.active != before.active || after.dueCalls[urlDueEmpty] != before.dueCalls[urlDueEmpty]+1 || after.stops[urlSchedulerEmpty] != before.stops[urlSchedulerEmpty]+1 {
			t.Fatalf("actual scheduler boundary not represented: %+v %v %+v", result, err, after)
		}
		if math.Abs(after.seconds[urlSchedulerDue]-before.seconds[urlSchedulerDue]-3) > 1e-9 || math.Abs(after.seconds[urlSchedulerFinalize]-before.seconds[urlSchedulerFinalize]-2) > 1e-9 || math.Abs(after.dueSeconds[urlDueEmpty]-before.dueSeconds[urlDueEmpty]-3) > 1e-9 {
			t.Fatal("due/finalize times moved into worker or other phase")
		}
	})
}

func TestUrlProbeSchedulerMetricsKeepPrepareFailureDistinct(t *testing.T) {
	pass, args, _ := urlAdmissionPass()
	before := urlProbeSchedulerMetrics.snapshot()
	want := errors.New("synthetic catalog failure")
	pass.loadPool = func(context.Context) (*egresshealth.Pool, error) { return nil, want }
	_, err := pass.run(t.Context(), args)
	after := urlProbeSchedulerMetrics.snapshot()
	if !errors.Is(err, want) || after.stops[urlSchedulerOther] != before.stops[urlSchedulerOther]+1 || after.dueCalls != before.dueCalls || after.active != before.active {
		t.Fatalf("prepare error invented claims or left owner: %v %+v", err, after)
	}
}
