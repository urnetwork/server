package work

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestUrlProbeFleetReadIdleHasOnlyCapability(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	collector := newProviderUrlProbeFleetReadCollector(registry)
	if count, err := testutil.GatherAndCount(registry); err != nil || count != 1 || collector.snapshot.Load() != nil {
		t.Fatalf("idle read observations invented a census attempt: count=%d err=%v", count, err)
	}
}

func TestUrlProbeFleetReadCancellationKeepsAdmittedPhaseAndOldQuota(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	readMetrics := newProviderUrlProbeFleetReadCollector(registry)
	collector := newProviderUrlProbeFleetCollector()
	collector.readMetrics = readMetrics
	collector.refreshMetrics = newProviderUrlProbeFleetRefreshCollectors(prometheus.NewRegistry())
	previous := &providerUrlProbeFleetSnapshot{fleet: model.ProviderUrlProbeFleet{Eligible: 17}, observedAt: time.Unix(1, 0)}
	collector.snapshot.Store(previous)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	joined := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		joined <- collector.refreshRead(ctx, time.Unix(2, 0), func(readCtx context.Context, at time.Time, observation *server.DbReadObservation) model.ProviderUrlProbeFleet {
			observation.BeginAcquire()
			observation.FinishAcquire(true)
			observation.BeginQuery()
			defer observation.FinishQuery(false)
			close(entered)
			<-readCtx.Done()
			panic(readCtx.Err())
		})
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("read observation test owner did not join")
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic query did not reach its explicit barrier")
	}
	before := readMetrics.snapshot.Load().observation.Snapshot()
	if before.Phase != server.DbReadQueryBegin || before.AcquireSucceeded != 1 || before.PhaseCounts[server.DbReadQueryDone] != 0 || before.Rows != 0 || before.Finished {
		cancel()
		<-joined
		t.Fatalf("live admitted query was misclassified: %+v", before)
	}
	cancel()
	select {
	case err := <-joined:
		if err == nil {
			t.Fatal("canceled census returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled census did not join")
	}
	after := readMetrics.snapshot.Load().observation.Snapshot()
	if !after.Finished || after.Phase != server.DbReadError || after.PhaseCounts[server.DbReadError] != 1 || after.PhaseCounts[server.DbReadComplete] != 0 || after.PhaseCounts[server.DbReadQueryDone] != 1 || after.QuerySucceeded != 0 || after.Rows != 0 || collector.snapshot.Load() != previous {
		t.Fatalf("joined failure lost phases or refreshed old quota: %+v", after)
	}
	if count, err := testutil.GatherAndCount(registry); err != nil || count != 27 {
		t.Fatalf("read phase vocabulary is not bounded: count=%d err=%v", count, err)
	}
}

func TestUrlProbeFleetReadSuccessAndFailuresHaveFixedPrivateFreeDomain(t *testing.T) {
	for _, test := range []struct{ success, late, panicRead bool }{
		{success: true}, {late: true}, {panicRead: true},
	} {
		registry := prometheus.NewPedanticRegistry()
		readMetrics := newProviderUrlProbeFleetReadCollector(registry)
		collector := newProviderUrlProbeFleetCollector()
		collector.readMetrics = readMetrics
		collector.refreshMetrics = newProviderUrlProbeFleetRefreshCollectors(prometheus.NewRegistry())
		ctx := t.Context()
		if test.late {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			defer cancel()
		}
		err := collector.refreshRead(ctx, time.Unix(5, 0), func(readCtx context.Context, at time.Time, observation *server.DbReadObservation) model.ProviderUrlProbeFleet {
			if test.panicRead {
				panic(errors.New("synthetic-private.example/query-text"))
			}
			observation.BeginAcquire()
			observation.FinishAcquire(true)
			observation.BeginQuery()
			observation.Row()
			observation.FinishQuery(true)
			return model.ProviderUrlProbeFleet{Eligible: 3}
		})
		read := readMetrics.snapshot.Load().observation.Snapshot()
		if (err == nil) != test.success || !read.Finished || (read.Phase == server.DbReadComplete) != test.success || (collector.snapshot.Load() != nil) != test.success {
			t.Fatalf("result phases changed census acceptance: case=%+v error=%v read=%+v", test, err, read)
		}
		families, gatherErr := registry.Gather()
		if gatherErr != nil {
			t.Fatal(gatherErr)
		}
		series := 0
		for _, family := range families {
			for _, metric := range family.Metric {
				series++
				value := metric.GetGauge().GetValue()
				if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || strings.Contains(metric.String(), "synthetic-private") || strings.Contains(metric.String(), "query-text") {
					t.Fatal("private or invalid numeric data escaped into phase metrics")
				}
				for _, label := range metric.Label {
					if label.GetName() != "phase" && label.GetName() != "state" {
						t.Fatal("unbounded metric label")
					}
				}
			}
		}
		if series != 27 {
			t.Fatalf("phase metric domain changed: %d", series)
		}
	}
}

func TestUrlProbeFleetReadScrapeAndLateOwnerKeepOneAttempt(t *testing.T) {
	collector := newProviderUrlProbeFleetReadCollector(prometheus.NewPedanticRegistry())
	old := collector.begin(time.Unix(1, 0))
	old.BeginAcquire()
	metricKey := func(metric prometheus.Metric) (string, float64) {
		var value dto.Metric
		if err := metric.Write(&value); err != nil {
			t.Fatal(err)
		}
		key := metric.Desc().String()
		for _, label := range value.Label {
			key += "/" + label.GetName() + "=" + label.GetValue()
		}
		return key, value.GetGauge().GetValue()
	}
	expected := map[string]float64{}
	buffer := make(chan prometheus.Metric, 27)
	collector.Collect(buffer)
	close(buffer)
	for metric := range buffer {
		key, value := metricKey(metric)
		expected[key] = value
	}
	metrics := make(chan prometheus.Metric)
	go func() { collector.Collect(metrics); close(metrics) }()
	first := <-metrics
	second := <-metrics // The collector has now loaded its one observation.
	current := collector.begin(time.Unix(9, 0))
	current.BeginAcquire()
	current.FinishAcquire(true)
	current.BeginQuery()
	current.Row()
	current.FinishQuery(true)
	current.Finish(true)
	old.Finish(false)
	scrape := []prometheus.Metric{first, second}
	for metric := range metrics {
		scrape = append(scrape, metric)
	}
	if len(scrape) != 27 || collector.snapshot.Load().observation != current {
		t.Fatal("late owner replaced the current attempt or changed fixed scrape shape")
	}
	for _, metric := range scrape {
		key, value := metricKey(metric)
		if want, exists := expected[key]; !exists || value != want {
			t.Fatal("one scrape mixed old and new attempt phases or clocks")
		}
	}
}
