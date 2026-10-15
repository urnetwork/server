package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// The modern provider gate's readiness signal (SIGNALS.md §2.29), with
// synthetic gauge values and no clock: the windows around the start, the
// WARN and the PAGE before it, the PAGE after it, the schedule the
// taskworkers publish, and the overrides every finding names.

// The owner's start, 2026-10-21 00:00 UTC.
var modernGateReadinessTestStart = time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)

// An observation as the taskworkers publish it while the gate is off with
// the start scheduled: per bucket, providers shown modern and providers
// without evidence, from counts read five minutes before now.
func modernGateReadinessTestObservation(now time.Time, qualityModern float64, qualityNoEvidence float64, speedModern float64, speedNoEvidence float64) modernGateReadinessObservation {
	start := modernGateReadinessTestStart
	observation := modernGateReadinessObservation{
		published:     true,
		on:            !now.Before(start),
		start:         &start,
		countsStarted: now.Add(-5 * time.Minute),
		shadow:        map[string]map[string]float64{},
		buckets:       map[string]float64{},
	}
	for bucket, counts := range map[string][2]float64{
		model.RankModeQuality: {qualityModern, qualityNoEvidence},
		model.RankModeSpeed:   {speedModern, speedNoEvidence},
	} {
		outcomes := map[string]float64{}
		for _, outcome := range model.ProviderModernOutcomes {
			outcomes[outcome] = 0
		}
		outcomes[model.ProviderModernOutcomeModern] = counts[0]
		outcomes[model.ProviderModernOutcomeNoEvidence] = counts[1]
		observation.shadow[bucket] = outcomes
		observation.buckets[bucket] = counts[0] + counts[1]
	}
	return observation
}

// The failing findings, by frame.
func modernGateReadinessTestFailing(t *testing.T, findings []finding) map[string]finding {
	t.Helper()
	failing := map[string]finding{}
	for _, f := range findings {
		if f.healthy {
			continue
		}
		if _, duplicate := failing[f.frame]; duplicate {
			t.Fatalf("two findings for %s", f.frame)
		}
		failing[f.frame] = f
	}
	return failing
}

// Nothing fires before the warning lead or after the hour that follows the
// start, however much the gate would take out or a bucket has lost.
func TestModernGateReadinessIsQuietOutsideItsWindows(t *testing.T) {
	start := modernGateReadinessTestStart
	for _, now := range []time.Time{
		start.Add(-modernGateReadinessWarnLead - time.Second),
		start.AddDate(0, 0, -30),
		start.Add(time.Hour),
		start.AddDate(0, 0, 7),
	} {
		observation := modernGateReadinessTestObservation(now, 10, 90, 10, 90)
		observation.bucketsBefore = map[string]float64{model.RankModeQuality: 1000, model.RankModeSpeed: 1000}
		findings, err := evaluateModernGateReadiness(observation, now)
		if err != nil || len(modernGateReadinessTestFailing(t, findings)) != 0 {
			t.Errorf("at %s: findings %+v err %v, want quiet", now, findings, err)
		}
	}
}

// From 72 hours before the start a gate that would take more than half of a
// bucket out WARNs for that bucket; exactly half does not.
func TestModernGateReadinessWarnsFromThreeDaysBefore(t *testing.T) {
	start := modernGateReadinessTestStart
	for _, now := range []time.Time{
		start.Add(-modernGateReadinessWarnLead),
		start.Add(-modernGateReadinessPageLead - time.Second),
	} {
		findings, err := evaluateModernGateReadiness(modernGateReadinessTestObservation(now, 40, 60, 50, 50), now)
		if err != nil {
			t.Fatal(err)
		}
		failing := modernGateReadinessTestFailing(t, findings)
		quality, ok := failing[model.RankModeQuality]
		if len(failing) != 1 || !ok || quality.tier != tierWarn || quality.class != "modern-gate-not-ready" || quality.target != "modern-provider-gate" {
			t.Fatalf("at %s: findings %+v, want one quality warning", now, findings)
		}
		for _, part := range []string{"removed_share=0.600", "no_evidence=60", "start=2026-10-21T00:00:00Z"} {
			if !strings.Contains(quality.observed, part) {
				t.Errorf("observed %q does not say %q", quality.observed, part)
			}
		}
	}
}

// From 24 hours before the start the same condition PAGEs, under the same
// identity, so the warning escalates rather than opening another ticket.
func TestModernGateReadinessPagesFromOneDayBefore(t *testing.T) {
	start := modernGateReadinessTestStart
	for _, now := range []time.Time{
		start.Add(-modernGateReadinessPageLead),
		start.Add(-time.Second),
	} {
		findings, err := evaluateModernGateReadiness(modernGateReadinessTestObservation(now, 40, 60, 10, 90), now)
		if err != nil {
			t.Fatal(err)
		}
		failing := modernGateReadinessTestFailing(t, findings)
		if len(failing) != 2 {
			t.Fatalf("at %s: findings %+v, want both buckets", now, findings)
		}
		for _, bucket := range []string{model.RankModeQuality, model.RankModeSpeed} {
			if f := failing[bucket]; f.tier != tierPage || f.class != "modern-gate-not-ready" {
				t.Errorf("at %s: %s finding %+v, want a page", now, bucket, f)
			}
		}
	}
	// a ready fleet stays quiet at the last second
	findings, err := evaluateModernGateReadiness(modernGateReadinessTestObservation(start.Add(-time.Second), 90, 10, 60, 40), start.Add(-time.Second))
	if err != nil || len(modernGateReadinessTestFailing(t, findings)) != 0 {
		t.Fatalf("a ready fleet: findings %+v err %v", findings, err)
	}
}

// Within the hour after the start a bucket that fell by more than half since
// the last sample before the start PAGEs; exactly half does not. Counts read
// before the start, or by taskworkers that have not flipped, are no verdict
// yet.
func TestModernGateReadinessPagesWhenABucketFallsAfterTheFlip(t *testing.T) {
	start := modernGateReadinessTestStart
	now := start.Add(10 * time.Minute)
	observation := modernGateReadinessTestObservation(now, 40, 60, 50, 50)
	observation.buckets = map[string]float64{model.RankModeQuality: 40, model.RankModeSpeed: 50}
	observation.bucketsBefore = map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}
	findings, err := evaluateModernGateReadiness(observation, now)
	if err != nil {
		t.Fatal(err)
	}
	failing := modernGateReadinessTestFailing(t, findings)
	quality, ok := failing[model.RankModeQuality]
	if len(failing) != 1 || !ok || quality.tier != tierPage || quality.class != "modern-gate-flip-drop" || quality.sustain != 1 {
		t.Fatalf("findings %+v, want one quality page", findings)
	}
	if !strings.Contains(quality.observed, "before=100 now=40") {
		t.Errorf("observed %q does not give both sizes", quality.observed)
	}

	for name, change := range map[string]func(*modernGateReadinessObservation){
		"counts read before the start": func(o *modernGateReadinessObservation) { o.countsStarted = start.Add(-time.Second) },
		"taskworkers not flipped":      func(o *modernGateReadinessObservation) { o.on = false },
	} {
		pending := observation
		change(&pending)
		findings, err := evaluateModernGateReadiness(pending, now)
		if err != nil || len(modernGateReadinessTestFailing(t, findings)) != 0 {
			t.Errorf("%s: findings %+v err %v, want no verdict yet", name, findings, err)
		}
	}
	// a missing sample before the start is a visibility failure, not quiet
	missing := observation
	missing.bucketsBefore = map[string]float64{model.RankModeQuality: 100}
	if _, err := evaluateModernGateReadiness(missing, now); err == nil {
		t.Error("a missing speed sample before the start was judged")
	}
}

// The windows follow the schedule the taskworkers publish: a set switch
// schedules no flip and silences the signal, a moved start moves the
// windows, and no published schedule inside the default windows is a
// visibility failure (taskworkers without the flip) while outside them it is
// quiet.
func TestModernGateReadinessFollowsThePublishedSchedule(t *testing.T) {
	start := modernGateReadinessTestStart
	now := start.Add(-time.Hour)

	switched := modernGateReadinessTestObservation(now, 10, 90, 10, 90)
	switched.start = nil
	if findings, err := evaluateModernGateReadiness(switched, now); err != nil || len(modernGateReadinessTestFailing(t, findings)) != 0 {
		t.Errorf("a set switch: findings %+v err %v, want quiet", findings, err)
	}

	moved := modernGateReadinessTestObservation(now, 10, 90, 10, 90)
	movedStart := start.AddDate(0, 0, 7)
	moved.start = &movedStart
	if findings, err := evaluateModernGateReadiness(moved, now); err != nil || len(modernGateReadinessTestFailing(t, findings)) != 0 {
		t.Errorf("a start moved a week on: findings %+v err %v, want quiet", findings, err)
	}
	movedNow := movedStart.Add(-time.Hour)
	moved.countsStarted = movedNow.Add(-5 * time.Minute)
	if findings, err := evaluateModernGateReadiness(moved, movedNow); err != nil || len(modernGateReadinessTestFailing(t, findings)) != 2 {
		t.Errorf("an hour before the moved start: findings %+v err %v, want both buckets", findings, err)
	}

	unpublished := modernGateReadinessObservation{}
	if _, err := evaluateModernGateReadiness(unpublished, now); err == nil || !strings.Contains(err.Error(), "urnetwork_stats_provider_modern_gate_on") {
		t.Errorf("no schedule inside the default windows: err %v, want a visibility failure", err)
	}
	if findings, err := evaluateModernGateReadiness(unpublished, start.AddDate(0, 0, -10)); err != nil || len(modernGateReadinessTestFailing(t, findings)) != 0 {
		t.Errorf("no schedule ten days before: findings %+v err %v, want quiet", findings, err)
	}
}

// Inside the windows the shadow is judged only on dashboard counts no older
// than half an hour and carrying every outcome.
func TestModernGateReadinessNeedsCurrentCounts(t *testing.T) {
	now := modernGateReadinessTestStart.Add(-time.Hour)
	for name, change := range map[string]func(*modernGateReadinessObservation){
		"stale counts": func(o *modernGateReadinessObservation) {
			o.countsStarted = now.Add(-modernGateReadinessMaxCountsAge - time.Second)
		},
		"unknown age": func(o *modernGateReadinessObservation) { o.countsStarted = time.Time{} },
		"missing outcomes": func(o *modernGateReadinessObservation) {
			delete(o.shadow[model.RankModeSpeed], model.ProviderModernOutcomeStaleEvidence)
		},
		"missing bucket": func(o *modernGateReadinessObservation) { delete(o.shadow, model.RankModeSpeed) },
	} {
		observation := modernGateReadinessTestObservation(now, 90, 10, 90, 10)
		change(&observation)
		if _, err := evaluateModernGateReadiness(observation, now); err == nil {
			t.Errorf("%s: judged", name)
		}
	}
}

// One Mimir answer for the readiness query: the schedule and counts' start,
// the shadow and the buckets, each as its part.
func modernGateReadinessTestPayload(t testing.TB, at time.Time, start *time.Time, on float64, countsStarted time.Time, shadow map[string]map[string]float64, buckets map[string]float64) string {
	t.Helper()
	result := []map[string]any{}
	add := func(labels map[string]string, value float64) {
		result = append(result, map[string]any{
			"metric": labels,
			"value":  []any{float64(at.Unix()), strconv.FormatFloat(value, 'f', -1, 64)},
		})
	}
	add(map[string]string{"monitor_modern_gate_part": "on"}, on)
	if start != nil {
		add(map[string]string{"monitor_modern_gate_part": "start"}, float64(start.Unix()))
	}
	add(map[string]string{"monitor_modern_gate_part": "counts_started"}, float64(countsStarted.Unix()))
	for bucket, outcomes := range shadow {
		for outcome, value := range outcomes {
			add(map[string]string{"monitor_modern_gate_part": "shadow", "bucket": bucket, "outcome": outcome}, value)
		}
	}
	for bucket, value := range buckets {
		add(map[string]string{"monitor_modern_gate_part": "bucket", "bucket": bucket}, value)
	}
	payload, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// Settings whose one services host answers each query by its evaluation time.
func modernGateReadinessTestSettings(t testing.TB, now time.Time, payloads map[int64]string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" || !strings.Contains(command, "127.0.0.1:3100/prometheus/api/v1/query") || !strings.Contains(command, "urnetwork_stats_provider_egress_index") {
			return "", fmt.Errorf("unexpected modern gate readiness command on %s", host.Name)
		}
		for at, payload := range payloads {
			if strings.Contains(command, "'time="+strconv.FormatInt(at, 10)+"'") {
				return payload, nil
			}
		}
		return "", fmt.Errorf("no payload for the command's time")
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	return settings
}

// The shadow of a fleet the gate would mostly empty: per bucket, 30 modern
// and 70 without evidence.
func modernGateReadinessTestGuttedShadow() map[string]map[string]float64 {
	shadow := map[string]map[string]float64{}
	for _, bucket := range []string{model.RankModeQuality, model.RankModeSpeed} {
		outcomes := map[string]float64{}
		for _, outcome := range model.ProviderModernOutcomes {
			outcomes[outcome] = 0
		}
		outcomes[model.ProviderModernOutcomeModern] = 30
		outcomes[model.ProviderModernOutcomeNoEvidence] = 70
		shadow[bucket] = outcomes
	}
	return shadow
}

// Through the signal's own query and the Mimir answer, a day before the
// start: a page per bucket that names both provider.yml overrides, the
// capability probe, and this catalog section.
func TestModernGateReadinessSignalPagesNamingTheOverrides(t *testing.T) {
	start := modernGateReadinessTestStart
	now := start.Add(-23 * time.Hour)
	payload := modernGateReadinessTestPayload(t, now, &start, 0, now.Add(-4*time.Minute), modernGateReadinessTestGuttedShadow(), map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100})
	alerts, err := NewModernGateReadinessSignal().Run(context.Background(), modernGateReadinessTestSettings(t, now, map[int64]string{now.Unix(): payload}))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts %+v, want one per bucket", alerts)
	}
	alert := requireAlertClass(t, alerts, "modern-gate-not-ready")
	if alert.Severity != SeverityPage || alert.SignalNumber != "2.29" || alert.SignalKey != "modern-gate-readiness" {
		t.Fatalf("alert %+v, want a §2.29 page", alert)
	}
	for _, part := range []string{"quality_speed_requires_modern_provider: false", "quality_speed_modern_provider_start", "server/capprobe", "SIGNALS.md §2.29", "removed_share=0.700"} {
		if !strings.Contains(alert.Markdown(), part) {
			t.Errorf("alert does not say %q:\n%s", part, alert.Markdown())
		}
	}
}

// Through the signal, ten minutes after the start: the second query reads the
// buckets a second before the start, and the fallen one pages.
func TestModernGateReadinessSignalPagesAFallAfterTheFlip(t *testing.T) {
	start := modernGateReadinessTestStart
	now := start.Add(10 * time.Minute)
	before := start.Add(-time.Second)
	payloads := map[int64]string{
		now.Unix():    modernGateReadinessTestPayload(t, now, &start, 1, start.Add(time.Minute), modernGateReadinessTestGuttedShadow(), map[string]float64{model.RankModeQuality: 30, model.RankModeSpeed: 60}),
		before.Unix(): modernGateReadinessTestPayload(t, before, nil, 0, before, nil, map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}),
	}
	alerts, err := NewModernGateReadinessSignal().Run(context.Background(), modernGateReadinessTestSettings(t, now, payloads))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts %+v, want the quality bucket alone", alerts)
	}
	alert := requireAlertClass(t, alerts, "modern-gate-flip-drop")
	if alert.Severity != SeverityPage || alert.Frame != model.RankModeQuality || !strings.Contains(alert.Markdown(), "quality_speed_requires_modern_provider: false") {
		t.Fatalf("alert %+v, want a quality page naming the kill switch", alert)
	}
}

// The query reads every part from the configured environment only.
func TestModernGateReadinessQueryReadsEveryPart(t *testing.T) {
	query := modernGateReadinessQuery("synthetic")
	for _, part := range []string{
		`max(urnetwork_stats_provider_modern_gate_on{env="synthetic"})`,
		`max(urnetwork_stats_provider_modern_gate_start_seconds{env="synthetic"})`,
		`max(urnetwork_stats_provider_egress_source_started_seconds{env="synthetic"})`,
		`max by (bucket, outcome) (urnetwork_stats_provider_modern_gate_shadow{env="synthetic",bucket=~"quality|speed"})`,
		`sum by (bucket) (max by (bucket, index) (urnetwork_stats_provider_egress_index{env="synthetic",bucket=~"quality|speed"}))`,
	} {
		if !strings.Contains(query, part) {
			t.Errorf("query %q does not read %q", query, part)
		}
	}
	if strings.Count(query, "env=") != 5 {
		t.Errorf("query %q reads a part outside the environment", query)
	}
}
