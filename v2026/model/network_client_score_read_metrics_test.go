package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/urnetwork/server/v2026"
)

// Metric reads do not replace the process-global collectors or alter loader policy.
func clientScoreReadMetricValues(t testing.TB) (float64, float64) {
	t.Helper()
	read := func(counter prometheus.Counter) float64 {
		t.Helper()
		var metric dto.Metric
		if err := counter.Write(&metric); err != nil {
			t.Fatal(err)
		}
		return metric.GetCounter().GetValue()
	}
	return read(clientScoreReadMetrics.counts), read(clientScoreReadMetrics.samples)
}

// A real local Redis command error must count once in its owning phase only.
func assertClientScoreReadFailureMetric(t *testing.T, mode string, countsDelta, samplesDelta float64) {
	t.Helper()
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		beforeCounts, beforeSamples := clientScoreReadMetricValues(t)
		scores, err, expected, _ := clientScoreReadFailureFixture(t, mode)
		afterCounts, afterSamples := clientScoreReadMetricValues(t)
		if expected == nil || !errors.Is(err, expected) || scores != nil {
			t.Fatal("metric fixture changed the exact backend-error result")
		}
		if afterCounts-beforeCounts != countsDelta || afterSamples-beforeSamples != samplesDelta {
			t.Fatalf("read failure phase deltas=(%v,%v), want=(%v,%v)", afterCounts-beforeCounts, afterSamples-beforeSamples, countsDelta, samplesDelta)
		}
	})
}

func TestClientScoreReadMetricsCountPipelineFailure(t *testing.T) {
	assertClientScoreReadFailureMetric(t, "counts-pipeline", 1, 0)
}

func TestClientScoreReadMetricsMaskedCountFailure(t *testing.T) {
	assertClientScoreReadFailureMetric(t, "counts-command", 1, 0)
}

func TestClientScoreReadMetricsSamplePipelineFailure(t *testing.T) {
	assertClientScoreReadFailureMetric(t, "samples-pipeline", 0, 1)
}

func TestClientScoreReadMetricsMaskedSampleFailure(t *testing.T) {
	assertClientScoreReadFailureMetric(t, "samples-command", 0, 1)
}

func TestClientScoreReadMetricsPartialSampleFailure(t *testing.T) {
	assertClientScoreReadFailureMetric(t, "partial-samples", 0, 1)
}

func TestClientScoreReadMetricsAliasFailure(t *testing.T) {
	assertClientScoreReadFailureMetric(t, "alias-command", 1, 0)
}

func TestClientScoreReadMetricsCountFailedPipelineNotFailedKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		locationId := server.NewId()
		keys := []string{
			clientScoreLocationCountsKey(false, RankModeQuality, locationId, server.Id{}),
			clientScoreLocationFacetCountsKey(false, RankModeQuality, locationId, server.Id{}, ipFamilyFacetV4Only),
		}
		defer server.Redis(ctx, func(client server.RedisClient) {
			pipe := client.Pipeline()
			for _, key := range keys {
				pipe.Del(ctx, key)
			}
			_, err := pipe.Exec(ctx)
			server.Raise(err)
		})
		var expected error
		server.Redis(ctx, func(client server.RedisClient) {
			for _, key := range keys {
				server.Raise(client.RPush(ctx, key, "synthetic-non-string").Err())
				server.Raise(client.Expire(ctx, key, time.Minute).Err())
				if err := client.Get(ctx, key).Err(); err == nil {
					t.Fatal("fixture did not produce a Redis GET error")
				} else {
					expected = err
				}
			}
		})
		beforeCounts, beforeSamples := clientScoreReadMetricValues(t)
		scores, err := loadClientScores(false, RankModeQuality, ctx,
			map[server.Id]bool{locationId: true}, map[server.Id]bool{}, server.Id{}, 100, []ipFamilyFacet{ipFamilyFacetV4Only})
		afterCounts, afterSamples := clientScoreReadMetricValues(t)
		if !errors.Is(err, expected) || scores != nil || afterCounts-beforeCounts != 1 || afterSamples != beforeSamples {
			t.Fatal("two failed keys were counted as multiple batches or hidden by a partial result")
		}
	})
}

// These controls do not assert cache health; they prove legitimate absence is not an error event.
func TestClientScoreReadMetricsMissingKeysDoNotCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		beforeCounts, beforeSamples := clientScoreReadMetricValues(t)
		for _, mode := range []string{"missing", "encoded-empty", "alias-fallback"} {
			_, err, expected, _ := clientScoreReadFailureFixture(t, mode)
			if err != nil || expected != nil {
				t.Fatalf("healthy cache-miss control %s failed", mode)
			}
		}
		afterCounts, afterSamples := clientScoreReadMetricValues(t)
		if afterCounts != beforeCounts || afterSamples != beforeSamples {
			t.Fatal("legitimate missing or empty score payload was counted as a backend error")
		}
	})
}

func TestClientScoreReadMetricsValidAndAliasReadsDoNotCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		beforeCounts, beforeSamples := clientScoreReadMetricValues(t)
		for _, mode := range []string{"valid", "alias-baseline"} {
			scores, err, expected, candidateId := clientScoreReadFailureFixture(t, mode)
			if err != nil || expected != nil || scores[candidateId] == nil {
				t.Fatalf("healthy provider control %s failed", mode)
			}
		}
		afterCounts, afterSamples := clientScoreReadMetricValues(t)
		if afterCounts != beforeCounts || afterSamples != beforeSamples {
			t.Fatal("a successful score read counted as a backend error")
		}
	})
}

func TestClientScoreReadMetricsPreinitializeExactlyTwoPrivateSafeChildren(t *testing.T) {
	metrics := newClientScoreReadMetricSet()
	registry := prometheus.NewRegistry()
	if err := registry.Register(metrics.errors); err != nil {
		t.Fatal(err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].GetName() != "urnetwork_client_score_read_errors_total" ||
		families[0].GetType() != dto.MetricType_COUNTER || len(families[0].Metric) != 2 {
		t.Fatal("score-read metric family or bounded cardinality changed")
	}
	seen := map[string]bool{}
	for _, metric := range families[0].Metric {
		if len(metric.Label) != 1 || metric.Label[0].GetName() != "phase" ||
			metric.GetCounter().GetValue() != 0 || metric.Counter.Exemplar != nil {
			t.Fatal("metric includes unexpected labels, an exemplar, or nonzero initial state")
		}
		phase := metric.Label[0].GetValue()
		if (phase != "counts" && phase != "samples") || seen[phase] {
			t.Fatal("metric has an unknown or duplicate phase")
		}
		seen[phase] = true
	}
}
