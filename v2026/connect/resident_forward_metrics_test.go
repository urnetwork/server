package connect

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026/model"
)

func TestResidentForwardLookupMetricsFixedQuietCells(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newResidentForwardLookupMetrics(registry)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 2 {
		t.Fatalf("families=%d, want two", len(families))
	}
	scalars := 0
	for _, family := range families {
		if len(family.Metric) != 4 {
			t.Errorf("%s has %d cells", family.GetName(), len(family.Metric))
		}
		for _, metric := range family.Metric {
			if len(metric.Label) != 2 {
				t.Error("lookup metric has an unexpected dimension")
			}
			for _, label := range metric.Label {
				if label.GetName() == "phase" && label.GetValue() != "initial" && label.GetValue() != "reconnect" {
					t.Error("unbounded phase")
				}
				if label.GetName() == "queue" && label.GetValue() != "empty" && label.GetValue() != "pending" {
					t.Error("unbounded queue")
				}
				if label.GetName() != "phase" && label.GetName() != "queue" {
					t.Error("unexpected label")
				}
			}
			if metric.Counter != nil {
				scalars++
				if metric.Counter.GetValue() != 0 {
					t.Error("quiet counter nonzero")
				}
			} else if metric.Summary != nil {
				scalars += 2
				if metric.Summary.GetSampleCount() != 0 || metric.Summary.GetSampleSum() != 0 || len(metric.Summary.Quantile) != 0 {
					t.Error("quiet summary is not bounded zero sum/count")
				}
			} else {
				t.Error("unexpected metric type")
			}
		}
	}
	if scalars != 12 {
		t.Errorf("scalar series=%d", scalars)
	}
	if metrics.now == nil {
		t.Error("clock missing")
	}
}

func TestResidentForwardLookupMetricsPreserveResultAndPanic(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newResidentForwardLookupMetrics(registry)
	now := time.Unix(1000, 0)
	metrics.now = func() time.Time { return now }
	resident := &model.NetworkClientResident{}
	for _, initial := range []bool{true, false} {
		for _, pending := range []bool{false, true} {
			got := metrics.observe(initial, pending, func() *model.NetworkClientResident { now = now.Add(time.Second); return resident })
			if got != resident {
				t.Error("observation changed result")
			}
		}
	}
	func() {
		defer func() {
			if got := recover(); got != "synthetic cancellation unwind" {
				t.Errorf("panic changed: %v", got)
			}
		}()
		metrics.observe(false, false, func() *model.NetworkClientResident {
			now = now.Add(2 * time.Second)
			panic("synthetic cancellation unwind")
		})
	}()
	if got := testutil.ToFloat64(metrics.collectors[1][0].attempts); got != 2 {
		t.Errorf("reconnect/empty attempts=%v", got)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var count uint64
	var sum float64
	for _, family := range families {
		for _, metric := range family.Metric {
			if metric.Summary != nil {
				count += metric.Summary.GetSampleCount()
				sum += metric.Summary.GetSampleSum()
			}
		}
	}
	if count != 5 || sum != 6 {
		t.Errorf("lookup count=%d residence=%v", count, sum)
	}
}
