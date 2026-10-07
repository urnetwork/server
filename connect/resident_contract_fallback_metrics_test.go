// Closed reason controls exercise the admission owner without a database;
// real SQL and constructor wiring are covered by the packet callback fixture.
package connect

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// All metric series are present even at zero, and no caller error or identity
// can become a label. Expected reasons refer only to admitted failed reads.
func requireResidentFallbackMetrics(t testing.TB, registry *prometheus.Registry, sourceReads uint64, expectedReasons ...contractFallbackErrorReason) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil || len(families) != 2 {
		t.Fatalf("fallback metric families=%d error=%v", len(families), err)
	}
	want := map[string]float64{}
	for _, label := range []string{"deadline", "canceled", "physical_error", "unknown", "expired_lease"} {
		want[label] = 0
	}
	for _, reason := range expectedReasons {
		want[[...]string{"deadline", "canceled", "physical_error", "unknown", "expired_lease"}[reason]]++
	}
	for _, family := range families {
		switch family.GetName() {
		case "urnetwork_connect_contract_fallback_errors_total":
			if len(family.Metric) != int(contractFallbackErrorReasonCount) {
				t.Fatal("fallback reason cardinality changed")
			}
			for _, metric := range family.Metric {
				if len(metric.Label) != 1 || metric.Label[0].GetName() != "reason" {
					t.Fatal("fallback reason metric has an unexpected label")
				}
				label := metric.Label[0].GetValue()
				value, ok := want[label]
				if !ok || metric.GetCounter().GetValue() != value {
					t.Fatalf("fallback reason=%s count=%g want=%g known=%t", label, metric.GetCounter().GetValue(), value, ok)
				}
			}
		case "urnetwork_connect_contract_fallback_source_seconds":
			if len(family.Metric) != 1 || len(family.Metric[0].Label) != 0 || family.Metric[0].GetHistogram().GetSampleCount() != sourceReads || len(family.Metric[0].GetHistogram().Bucket) != 10 {
				t.Fatal("fallback source histogram lost an admitted call or changed cardinality")
			}
		default:
			t.Fatal("fallback metrics registered an unexpected family")
		}
	}
}

// Wrapped native errors and caller termination remain distinct from a missing
// or expired lease. None may authorize, even if the source also says positive.
func TestResidentContractFallbackFailureReasons(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, sample := range []struct {
			name   string
			read   func(context.Context, context.CancelFunc) (model.ContractHoleStatus, time.Time, error)
			reason contractFallbackErrorReason
		}{
			{name: "deadline", reason: contractFallbackDeadline, read: func(ctx context.Context, _ context.CancelFunc) (model.ContractHoleStatus, time.Time, error) {
				<-ctx.Done()
				return model.ContractHolePositive, time.Now().Add(time.Hour), errors.New("synthetic late source failure")
			}},
			{name: "wrapped_deadline", reason: contractFallbackDeadline, read: func(context.Context, context.CancelFunc) (model.ContractHoleStatus, time.Time, error) {
				return model.ContractHoleUnknown, time.Time{}, fmt.Errorf("synthetic source: %w", context.DeadlineExceeded)
			}},
			{name: "canceled", reason: contractFallbackCanceled, read: func(_ context.Context, cancel context.CancelFunc) (model.ContractHoleStatus, time.Time, error) {
				cancel()
				return model.ContractHolePositive, time.Now().Add(time.Hour), nil
			}},
			{name: "wrapped_canceled", reason: contractFallbackCanceled, read: func(context.Context, context.CancelFunc) (model.ContractHoleStatus, time.Time, error) {
				return model.ContractHoleUnknown, time.Time{}, fmt.Errorf("synthetic source: %w", context.Canceled)
			}},
			{name: "physical_error", reason: contractFallbackPhysicalError, read: func(context.Context, context.CancelFunc) (model.ContractHoleStatus, time.Time, error) {
				return model.ContractHolePositive, time.Now().Add(time.Hour), errors.New("synthetic physical source failure")
			}},
			{name: "unknown", reason: contractFallbackUnknown, read: func(context.Context, context.CancelFunc) (model.ContractHoleStatus, time.Time, error) {
				return model.ContractHoleUnknown, time.Time{}, nil
			}},
			{name: "unexpected_status", reason: contractFallbackUnknown, read: func(context.Context, context.CancelFunc) (model.ContractHoleStatus, time.Time, error) {
				return model.ContractHoleStatus(255), time.Now().Add(time.Hour), nil
			}},
			{name: "missing_deadline", reason: contractFallbackUnknown, read: func(context.Context, context.CancelFunc) (model.ContractHoleStatus, time.Time, error) {
				return model.ContractHolePositive, time.Time{}, nil
			}},
			{name: "expired_lease", reason: contractFallbackExpiredLease, read: func(context.Context, context.CancelFunc) (model.ContractHoleStatus, time.Time, error) {
				return model.ContractHolePositive, time.Now(), nil
			}},
		} {
			t.Logf("case=%s", sample.name)
			ctx, cancel := context.WithCancel(t.Context())
			registry := prometheus.NewPedanticRegistry()
			fallback := newResidentContractFallback()
			fallback.metrics = newResidentContractFallbackMetrics(registry)
			fallback.readContract = func(ctx context.Context, _, _ server.Id) (model.ContractHoleStatus, time.Time, error) {
				return sample.read(ctx, cancel)
			}
			if fallback.check(ctx, server.NewId(), server.NewId()).active || len(fallback.slots) != 0 {
				t.Fatalf("%s: failed source authorized or retained admission", sample.name)
			}
			cancel()
			requireResidentFallbackMetrics(t, registry, 1, sample.reason)
		}
	})
}

// Healthy results are timed without errors; admission refusal does not pretend
// to have reached the source. The previous aggregate counters stay separate.
func TestResidentContractFallbackReasonMetricsHealthyAndRefused(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	fallback := newResidentContractFallback()
	fallback.metrics = newResidentContractFallbackMetrics(registry)
	requireResidentFallbackMetrics(t, registry, 0)
	for _, status := range []model.ContractHoleStatus{model.ContractHolePositive, model.ContractHoleNegative} {
		fallback.readContract = func(context.Context, server.Id, server.Id) (model.ContractHoleStatus, time.Time, error) {
			return status, time.Now().Add(time.Hour), nil
		}
		if fallback.check(t.Context(), server.NewId(), server.NewId()).active != (status == model.ContractHolePositive) {
			t.Fatal("healthy source result changed")
		}
	}
	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	if fallback.check(canceledCtx, server.NewId(), server.NewId()).active {
		t.Fatal("canceled admission authorized")
	}
	for range residentContractFallbackMaxConcurrent {
		fallback.slots <- struct{}{}
	}
	if fallback.check(t.Context(), server.NewId(), server.NewId()).active {
		t.Fatal("saturated admission authorized")
	}
	for range residentContractFallbackMaxConcurrent {
		<-fallback.slots
	}
	requireResidentFallbackMetrics(t, registry, 2)
}

// A programming panic retains its original value and releases the exchange
// slot. It is timed but is not mislabeled as a returned physical source error.
func TestResidentContractFallbackReasonMetricsPreservePanic(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	fallback := newResidentContractFallback()
	fallback.metrics = newResidentContractFallbackMetrics(registry)
	failure := errors.New("synthetic source programming panic")
	fallback.readContract = func(context.Context, server.Id, server.Id) (model.ContractHoleStatus, time.Time, error) {
		panic(failure)
	}
	func() {
		defer func() {
			if recover() != failure {
				t.Fatal("source programming panic changed")
			}
		}()
		fallback.check(t.Context(), server.NewId(), server.NewId())
	}()
	if len(fallback.slots) != 0 {
		t.Fatal("panicking source retained admission")
	}
	requireResidentFallbackMetrics(t, registry, 1)
}
