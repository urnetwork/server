package monitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// A future business clock cannot prove either healthy counts or a bucket
// failure, even when every count and the Mimir evaluation time are valid.
func TestModernGateReadinessFutureCountsCannotEstablishBucketVerdict(t *testing.T) {
	for _, now := range []time.Time{
		modernGateReadinessTestStart.Add(-time.Hour),
		modernGateReadinessTestStart.Add(10 * time.Minute),
	} {
		for _, modern := range []float64{90, 30} {
			for _, future := range []time.Duration{30*time.Second + time.Nanosecond, 24 * time.Hour} {
				observation := modernGateReadinessTestObservation(now, modern, 100-modern, 90, 10)
				observation.countsStarted = now.Add(future)
				observation.buckets = map[string]float64{model.RankModeQuality: modern, model.RankModeSpeed: 90}
				observation.bucketsBefore = map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}
				findings, err := evaluateModernGateReadiness(observation, now)
				if err == nil || !strings.Contains(err.Error(), "in the future") || len(findings) != 0 {
					t.Fatalf("at %s modern=%g future=%s: future source clock established a bucket verdict: findings %+v err %v", now, modern, future, findings, err)
				}
			}
		}
	}
}

// The query/parser boundary accepts the exact monitor clock allowance and
// rejects its next whole second without changing valid healthy or page results.
func TestModernGateReadinessSignalCountsClockBoundary(t *testing.T) {
	start := modernGateReadinessTestStart
	for _, now := range []time.Time{start.Add(-time.Hour), start.Add(10 * time.Minute)} {
		on := !now.Before(start)
		for _, modern := range []float64{90, 30} {
			for _, future := range []time.Duration{30 * time.Second, 31 * time.Second} {
				observation := modernGateReadinessTestObservation(now, modern, 100-modern, 90, 10)
				before := start.Add(-time.Second)
				payloads := map[int64]string{
					now.Unix():    modernGateReadinessTestPayload(t, now, modernGateReadinessTestProcesses(4, now, on, &start), now.Add(future), observation.shadow, map[string]float64{model.RankModeQuality: modern, model.RankModeSpeed: 90}),
					before.Unix(): modernGateReadinessTestPayload(t, before, nil, before, nil, map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}),
				}
				alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, payloads))
				if future == 31*time.Second {
					if err == nil || !strings.Contains(err.Error(), "in the future") || len(alerts) != 0 {
						t.Fatalf("at %s modern=%g: source beyond allowance established a verdict: alerts %+v err %v", now, modern, alerts, err)
					}
					continue
				}
				if modern == 90 {
					if err != nil || len(alerts) != 0 {
						t.Fatalf("at %s: exact allowance lost healthy counts: alerts %+v err %v", now, alerts, err)
					}
					continue
				}
				if err != nil || len(alerts) != 1 || alerts[0].Severity != SeverityPage || alerts[0].Frame != model.RankModeQuality {
					t.Fatalf("at %s: exact allowance lost the quality page: alerts %+v err %v", now, alerts, err)
				}
				wantClass := "modern-gate-not-ready"
				if on {
					wantClass = "modern-gate-flip-drop"
				}
				requireAlertClass(t, alerts, wantClass)
			}
		}
	}
}

// The pre-start freshness interval is closed at both valid edges. Missing
// source time and the first instant beyond either edge remain unknown.
func TestModernGateReadinessCountsClockBoundaries(t *testing.T) {
	now := modernGateReadinessTestStart.Add(-time.Hour)
	for _, c := range []struct {
		name    string
		started time.Time
		unknown bool
	}{
		{name: "missing", unknown: true},
		{name: "stale", started: now.Add(-30*time.Minute - time.Nanosecond), unknown: true},
		{name: "oldest valid", started: now.Add(-30 * time.Minute)},
		{name: "current snapshot", started: now.Add(-5 * time.Minute)},
		{name: "now", started: now},
		{name: "future allowance", started: now.Add(30 * time.Second)},
		{name: "beyond future allowance", started: now.Add(30*time.Second + time.Nanosecond), unknown: true},
	} {
		observation := modernGateReadinessTestObservation(now, 90, 10, 90, 10)
		observation.countsStarted = c.started
		findings, err := evaluateModernGateReadiness(observation, now)
		if c.unknown {
			if err == nil || len(findings) != 0 {
				t.Errorf("%s: findings %+v err %v, want unknown", c.name, findings, err)
			}
		} else if err != nil || len(findings) != 1 || !findings[0].healthy {
			t.Errorf("%s: findings %+v err %v, want healthy current counts", c.name, findings, err)
		}
	}
}

// Post-start eligibility still begins at the start, accepts the monitor
// clock allowance, and ends when every taskworker disables the gate.
func TestModernGateReadinessPostFlipCountsClockControls(t *testing.T) {
	start := modernGateReadinessTestStart
	now := start.Add(10 * time.Minute)
	for _, c := range []struct {
		name     string
		started  time.Time
		gateOff  bool
		wantPage bool
	}{
		{name: "before start", started: start.Add(-time.Nanosecond)},
		{name: "at start", started: start, wantPage: true},
		{name: "current snapshot", started: now.Add(-5 * time.Minute), wantPage: true},
		{name: "now", started: now, wantPage: true},
		{name: "future allowance", started: now.Add(30 * time.Second), wantPage: true},
		{name: "disabled gate does not use counts", started: now.Add(24 * time.Hour), gateOff: true},
	} {
		observation := modernGateReadinessTestObservation(now, 30, 70, 90, 10)
		observation.countsStarted = c.started
		observation.buckets = map[string]float64{model.RankModeQuality: 30, model.RankModeSpeed: 90}
		observation.bucketsBefore = map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}
		if c.gateOff {
			observation.settled.onEvery, observation.settled.onAny = false, false
		}
		findings, err := evaluateModernGateReadiness(observation, now)
		if err != nil || len(findings) != 1 {
			t.Fatalf("%s: findings %+v err %v, want one finding", c.name, findings, err)
		}
		if c.wantPage {
			if findings[0].healthy || findings[0].class != "modern-gate-flip-drop" || findings[0].frame != model.RankModeQuality || findings[0].tier != tierPage {
				t.Errorf("%s: findings %+v, want the observed quality drop", c.name, findings)
			}
		} else if !findings[0].healthy {
			t.Errorf("%s: findings %+v, want no bucket verdict yet", c.name, findings)
		}
	}
}

// A missing sibling component does not invalidate a bucket whose source
// clock and own evidence are valid; correcting the sibling clears unknown.
func TestModernGateReadinessValidClockPreservesIndependentBucket(t *testing.T) {
	for _, now := range []time.Time{
		modernGateReadinessTestStart.Add(-time.Hour),
		modernGateReadinessTestStart.Add(10 * time.Minute),
	} {
		for _, modern := range []float64{90, 30} {
			observation := modernGateReadinessTestObservation(now, modern, 100-modern, 90, 10)
			observation.countsStarted = now.Add(5 * time.Second)
			observation.buckets = map[string]float64{model.RankModeQuality: modern, model.RankModeSpeed: 90}
			observation.bucketsBefore = map[string]float64{model.RankModeQuality: 100}
			delete(observation.shadow[model.RankModeSpeed], model.ProviderModernOutcomeStaleEvidence)
			findings, err := evaluateModernGateReadiness(observation, now)
			if modern == 90 {
				if err == nil || len(findings) != 0 {
					t.Fatalf("at %s: healthy quality hid unknown speed: findings %+v err %v", now, findings, err)
				}
			} else {
				wantClass := "modern-gate-not-ready"
				if !now.Before(modernGateReadinessTestStart) {
					wantClass = "modern-gate-flip-drop"
				}
				if err != nil || len(findings) != 2 || findings[0].class != wantClass || findings[0].frame != model.RankModeQuality || findings[0].tier != tierPage || findings[1].class != "cannot-observe" {
					t.Fatalf("at %s: partial speed discarded the independent quality page: findings %+v err %v", now, findings, err)
				}
			}
			observation.shadow[model.RankModeSpeed][model.ProviderModernOutcomeStaleEvidence] = 0
			observation.bucketsBefore[model.RankModeSpeed] = 100
			findings, err = evaluateModernGateReadiness(observation, now)
			if err != nil || len(findings) != 1 || findings[0].class == "cannot-observe" || findings[0].healthy != (modern == 90) {
				t.Errorf("at %s modern=%g: corrected sibling retained unknown: findings %+v err %v", now, modern, findings, err)
			}
		}
	}
}

// The real signal query, parser, reducer and adapter must distinguish the
// fresh evaluation timestamp from the future source timestamp. An independent
// settled-fleet disagreement survives with its original severity and identity.
func TestModernGateReadinessSignalFutureCountsRetainIndependentFleet(t *testing.T) {
	start := modernGateReadinessTestStart
	for _, now := range []time.Time{start.Add(-time.Hour), start.Add(10 * time.Minute)} {
		on := !now.Before(start)
		for _, disagree := range []bool{false, true} {
			for _, modern := range []float64{90, 30} {
				observation := modernGateReadinessTestObservation(now, modern, 100-modern, 90, 10)
				processes := modernGateReadinessTestProcesses(4, now, on, &start)
				if disagree {
					processes[3].on, processes[3].start = nil, nil
				}
				before := start.Add(-time.Second)
				payloads := map[int64]string{
					now.Unix():    modernGateReadinessTestPayload(t, now, processes, now.Add(24*time.Hour), observation.shadow, map[string]float64{model.RankModeQuality: modern, model.RankModeSpeed: 90}),
					before.Unix(): modernGateReadinessTestPayload(t, before, nil, before, nil, map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}),
				}
				alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, payloads))
				if !disagree {
					if err == nil || !strings.Contains(err.Error(), "in the future") || len(alerts) != 0 {
						t.Fatalf("at %s modern=%g: fresh evaluation hid future source time: alerts %+v err %v", now, modern, alerts, err)
					}
					continue
				}
				if err != nil || len(alerts) != 2 {
					t.Fatalf("at %s modern=%g: future counts discarded fleet evidence: alerts %+v err %v", now, modern, alerts, err)
				}
				fleet := requireAlertClass(t, alerts, "modern-gate-fleet-disagrees")
				wantSeverity := SeverityWarn
				if on {
					wantSeverity = SeverityPage
				}
				if fleet.Severity != wantSeverity || fleet.SignalID != modernGateReadinessProbeId || fleet.Target != modernGateReadinessTarget || fleet.Frame != "" || fleet.SignalNumber != "2.29" || fleet.SignalKey != "modern-gate-readiness" {
					t.Errorf("at %s: independent fleet identity or severity changed: %+v", now, fleet)
				}
				visibility := requireAlertClass(t, alerts, "cannot-observe")
				if visibility.Severity != SeverityWarn || !isVisibilityAlert(visibility) {
					t.Errorf("at %s: future counts lost their unknown status: %+v", now, visibility)
				}
			}
		}
	}
}
