package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestUrlDueTimingFixedCardinalityAndAggregation(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	metrics := newProviderUrlProbeDueTimingCollectors(registry)
	if count, err := testutil.GatherAndCount(registry); err != nil || count != 49 {
		t.Fatalf("idle series=%d, want49", count)
	}
	var observation model.ProviderUrlProbeDueObservation
	observation.Phases[model.ProviderUrlProbeDueClaimBody] = server.DbTimingSample{Count: 2, Duration: 7 * time.Millisecond}
	observation.ClaimDatabase.Phases[server.DbTimingAcquire] = server.DbTimingSample{Count: 2, Duration: 3 * time.Millisecond}
	observation.RetentionDatabase.Phases[server.DbTimingAcquire] = server.DbTimingSample{Count: 1, Duration: time.Millisecond}
	metrics.observe(observation, 15*time.Millisecond, server.DbTimingSample{Count: 1, Duration: time.Millisecond}, urlDueTimingReturned)
	metrics.observe(model.ProviderUrlProbeDueObservation{}, time.Millisecond, server.DbTimingSample{}, urlDueTimingPanic)
	if got := testutil.ToFloat64(metrics.observations.WithLabelValues("claim_body")); got != 2 {
		t.Fatalf("rolled-back attempts disappeared: %v", got)
	}
	if got := testutil.ToFloat64(metrics.seconds.WithLabelValues("claim_db_acquire")); got != .003 {
		t.Fatalf("claim acquisition time=%v", got)
	}
	if got := testutil.ToFloat64(metrics.seconds.WithLabelValues("retention_db_acquire")); got != .001 {
		t.Fatalf("retention acquisition time=%v", got)
	}
	if count, err := testutil.GatherAndCount(registry); err != nil || count != 49 {
		t.Fatalf("observation changed series domain to%d", count)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	allowedPhases := map[string]bool{"admitted_handler": true, "encode": true}
	for _, phase := range urlDueModelPhaseLabels {
		allowedPhases[phase] = true
	}
	for _, phase := range urlDueDatabasePhaseLabels {
		allowedPhases["claim_db_"+phase], allowedPhases["retention_db_"+phase] = true, true
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			switch family.GetName() {
			case "urnetwork_url_due_phase_seconds_total", "urnetwork_url_due_phase_observations_total":
				if len(metric.Label) != 1 || metric.Label[0].GetName() != "phase" || !allowedPhases[metric.Label[0].GetValue()] {
					t.Fatalf("unbounded phase labels: %+v", metric.Label)
				}
			case "urnetwork_url_due_requests_total":
				if len(metric.Label) != 1 || metric.Label[0].GetName() != "outcome" {
					t.Fatal("unexpected request labels")
				}
				value := metric.Label[0].GetValue()
				if value != "returned" && value != "maintenance" && value != "encode_error" && value != "panic" {
					t.Fatal("unbounded outcome")
				}
			case "urnetwork_url_due_timing_enabled":
				if len(metric.Label) != 0 || metric.Gauge.GetValue() != 1 {
					t.Fatal("invalid idle capability")
				}
			default:
				t.Fatalf("unexpected family %s", family.GetName())
			}
		}
	}
}

type urlDueErrorWriter struct{ header http.Header }

func (self *urlDueErrorWriter) Header() http.Header { return self.header }
func (self *urlDueErrorWriter) WriteHeader(int)     {}
func (self *urlDueErrorWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic response write failure")
}

func TestUrlDueTimingHandlerPreservesResultsAndFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		outcome string
	}{
		{"success", "returned"}, {"maintenance", "maintenance"},
		{"encode_error", "encode_error"}, {"panic", "panic"},
	} {
		t.Run(test.name, func(t *testing.T) {
			metrics := newProviderUrlProbeDueTimingCollectors(prometheus.NewPedanticRegistry())
			request := httptest.NewRequest(http.MethodGet, "/network/provider-egress-due?limit=2&shard_index=3&shard_count=8", nil)
			recorder := httptest.NewRecorder()
			var writer http.ResponseWriter = recorder
			if test.name == "encode_error" {
				writer = &urlDueErrorWriter{header: http.Header{}}
			}
			want := model.ProviderUrlProbeDueResult{Providers: []model.ProviderUrlProbeDue{{ClientId: server.NewId(), ClaimOrdinal: 17, RunsNeeded: 10}}}
			if test.name == "maintenance" {
				want.Providers, want.PriorityMaintenancePending = []model.ProviderUrlProbeDue{}, true
			}
			original := errors.New("synthetic model failure")
			calls := 0
			claim := func(ctx context.Context, now time.Time, limit, shardIndex, shardCount int, observation *model.ProviderUrlProbeDueObservation) model.ProviderUrlProbeDueResult {
				calls++
				if ctx != request.Context() || now.Location() != time.UTC || limit != 2 || shardIndex != 3 || shardCount != 8 {
					t.Fatal("handler changed model authority or arguments")
				}
				observation.Phases[model.ProviderUrlProbeDueExpiry] = server.DbTimingSample{Count: 1, Duration: time.Millisecond}
				if test.name == "panic" {
					panic(original)
				}
				return want
			}
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				respondProviderUrlProbeDue(writer, request, 2, 3, 8, claim, metrics)
			}()
			if calls != 1 || (test.name == "panic" && recovered != original) || (test.name != "panic" && recovered != nil) {
				t.Fatalf("model outcome changed: calls=%d panic=%v", calls, recovered)
			}
			if testutil.ToFloat64(metrics.requests.WithLabelValues(test.outcome)) != 1 || testutil.ToFloat64(metrics.observations.WithLabelValues("expiry")) != 1 || testutil.ToFloat64(metrics.observations.WithLabelValues("admitted_handler")) != 1 {
				t.Fatal("terminal outcome or partial model observation missing")
			}
			if test.name == "panic" {
				if testutil.ToFloat64(metrics.observations.WithLabelValues("encode")) != 0 {
					t.Fatal("model panic invented encoding")
				}
			} else if testutil.ToFloat64(metrics.observations.WithLabelValues("encode")) != 1 {
				t.Fatal("encoding attempt missing")
			}
			if test.name == "success" || test.name == "maintenance" {
				var got model.ProviderUrlProbeDueResult
				if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil || !reflect.DeepEqual(got, want) || recorder.Code != 200 || recorder.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("wire response changed: err=%v status=%d result=%+v", err, recorder.Code, got)
				}
			}
		})
	}
}

func TestUrlDueTimingRejectsDoNotRunObservedModel(t *testing.T) {
	defer withStubOperatorIngestSecret("synthetic-secret")()
	before := testutil.ToFloat64(providerUrlProbeDueTiming.observations.WithLabelValues("admitted_handler"))
	for _, path := range []string{"/network/provider-egress-due", "/network/provider-egress-due?limit=0"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if path != "/network/provider-egress-due" {
			request.Header.Set(operatorSecretHeader, "synthetic-secret")
		}
		recorder := httptest.NewRecorder()
		ProviderEgressLocationDue(recorder, request)
		if recorder.Code != http.StatusUnauthorized && recorder.Code != http.StatusBadRequest {
			t.Fatalf("reject status changed to%d", recorder.Code)
		}
	}
	if testutil.ToFloat64(providerUrlProbeDueTiming.observations.WithLabelValues("admitted_handler")) != before {
		t.Fatal("rejected request entered model timing")
	}
}
