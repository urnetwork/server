package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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

// An observation as four settled taskworkers publish it, all on the same
// schedule and the gate off until the start: per bucket, providers shown
// modern and providers without evidence, from counts read five minutes
// before now.
func modernGateReadinessTestObservation(now time.Time, qualityModern float64, qualityNoEvidence float64, speedModern float64, speedNoEvidence float64) modernGateReadinessObservation {
	start := modernGateReadinessTestStart
	on := !now.Before(start)
	observation := modernGateReadinessObservation{
		publishing:  4,
		taskworkers: 4,
		start:       &start,
		settled: modernGateReadinessFleet{
			taskworkers: 4, publishing: 4, onEvery: on, onAny: on,
			scheduling: 4, earliestStart: &start, latestStart: &start,
		},
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

// Nothing fires before the warning lead or after the watch that follows the
// start, 18:00 UTC on the start's day, however much the gate would take out
// or a bucket has lost, while the fleet decides the gate alike.
func TestModernGateReadinessIsQuietOutsideItsWindows(t *testing.T) {
	start := modernGateReadinessTestStart
	for _, now := range []time.Time{
		start.Add(-modernGateReadinessWarnLead - time.Second),
		start.AddDate(0, 0, -30),
		start.Add(18 * time.Hour),
		start.AddDate(0, 0, 7),
	} {
		observation := modernGateReadinessTestObservation(now, 10, 90, 10, 90)
		observation.bucketsBefore = map[string]float64{model.RankModeQuality: 1000, model.RankModeSpeed: 1000}
		observation.settled.scheduling = 3
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

// After the start a bucket that fell by more than half since the last sample
// before the start PAGEs; exactly half does not. Counts read before the
// start, or by a fleet whose gate is off everywhere, the kill switch, are no
// verdict.
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
		"gate off everywhere":          func(o *modernGateReadinessObservation) { o.settled.onEvery, o.settled.onAny = false, false },
	} {
		pending := observation
		change(&pending)
		findings, err := evaluateModernGateReadiness(pending, now)
		if err != nil || len(modernGateReadinessTestFailing(t, findings)) != 0 {
			t.Errorf("%s: findings %+v err %v, want no verdict yet", name, findings, err)
		}
	}
	// a missing sample is a visibility failure, not quiet, and a bucket that
	// could be judged keeps its page beside it
	missingSpeed := observation
	missingSpeed.bucketsBefore = map[string]float64{model.RankModeQuality: 100}
	findings, err = evaluateModernGateReadiness(missingSpeed, now)
	classes := []string{}
	for _, f := range findings {
		classes = append(classes, f.class+"/"+f.frame)
	}
	if err != nil || strings.Join(classes, ",") != "modern-gate-flip-drop/quality,cannot-observe/" {
		t.Errorf("a missing speed sample before the start: classes %v err %v, want the quality page and cannot-observe", classes, err)
	}
	missingQuality := observation
	missingQuality.buckets = map[string]float64{model.RankModeSpeed: 50}
	if _, err := evaluateModernGateReadiness(missingQuality, now); err == nil {
		t.Error("a missing quality sample now was judged")
	}
}

// A fallen bucket keeps paging after the first hour, while the kill switch
// can still be released: until 18:00 UTC on the start's day, or an hour
// after a start later in the day. It stops once the bucket recovers or the
// gate is off everywhere.
func TestModernGateReadinessWatchesUntilSixPmOnTheStartDay(t *testing.T) {
	fallen := func(start time.Time, now time.Time) modernGateReadinessObservation {
		observation := modernGateReadinessTestObservation(now, 40, 60, 50, 50)
		observation.start, observation.settled.earliestStart, observation.settled.latestStart = &start, &start, &start
		observation.settled.onEvery, observation.settled.onAny = !now.Before(start), !now.Before(start)
		observation.countsStarted = start.Add(time.Minute)
		observation.buckets = map[string]float64{model.RankModeQuality: 40, model.RankModeSpeed: 100}
		observation.bucketsBefore = map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}
		return observation
	}
	midnight := modernGateReadinessTestStart
	evening := time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		start time.Time
		now   time.Time
		page  bool
	}{
		{name: "five hours after a midnight start", start: midnight, now: midnight.Add(5 * time.Hour), page: true},
		{name: "a second before 18:00", start: midnight, now: midnight.Add(18*time.Hour - time.Second), page: true},
		{name: "at 18:00", start: midnight, now: midnight.Add(18 * time.Hour)},
		{name: "59 minutes after a start at 20:00", start: evening, now: evening.Add(59 * time.Minute), page: true},
		{name: "an hour after a start at 20:00", start: evening, now: evening.Add(time.Hour)},
	} {
		findings, err := evaluateModernGateReadiness(fallen(c.start, c.now), c.now)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		failing := modernGateReadinessTestFailing(t, findings)
		if quality, ok := failing[model.RankModeQuality]; ok != c.page || ok && (quality.class != "modern-gate-flip-drop" || quality.tier != tierPage) {
			t.Errorf("%s: findings %+v, want a quality page %t", c.name, findings, c.page)
		}
	}

	now := midnight.Add(5 * time.Hour)
	recovered := fallen(midnight, now)
	recovered.buckets[model.RankModeQuality] = 60
	off := fallen(midnight, now)
	off.settled.onEvery, off.settled.onAny = false, false
	for name, observation := range map[string]modernGateReadinessObservation{"recovered": recovered, "gate off everywhere": off} {
		if findings, err := evaluateModernGateReadiness(observation, now); err != nil || len(modernGateReadinessTestFailing(t, findings)) != 0 {
			t.Errorf("%s: findings %+v err %v, want quiet", name, findings, err)
		}
	}
}

// A fleet whose settled taskworkers do not all publish the gate, decide it
// differently, or schedule different starts WARNs before the earliest start,
// since some will not flip with the rest, and PAGEs from it until the watch
// after the latest ends: an older taskworker still deciding the gate off is
// caught by the min, not hidden by the max, and one still on a start that was
// moved later flips early. A fleet that agrees is quiet, a schedule that
// differs no longer matters once every start has passed, and a fleet without
// taskworker process series cannot be judged.
func TestModernGateReadinessCatchesADisagreeingFleet(t *testing.T) {
	start := modernGateReadinessTestStart
	moved := start.AddDate(0, 0, 7)
	for _, c := range []struct {
		name        string
		now         time.Time
		taskworkers int
		publishing  int
		onEvery     bool
		onAny       bool
		scheduling  int
		earliest    time.Time
		start       time.Time
		tier        string
	}{
		{name: "agreeing, before the start", now: start.Add(-time.Hour), taskworkers: 4, publishing: 4, scheduling: 4, earliest: start, start: start},
		{name: "one without the gate, before the start", now: start.Add(-time.Hour), taskworkers: 4, publishing: 3, scheduling: 3, earliest: start, start: start, tier: tierWarn},
		{name: "one with the switch set, before the start", now: start.Add(-time.Hour), taskworkers: 4, publishing: 4, scheduling: 3, earliest: start, start: start, tier: tierWarn},
		{name: "agreeing, after the start", now: start.Add(time.Hour), taskworkers: 4, publishing: 4, onEvery: true, onAny: true, scheduling: 4, earliest: start, start: start},
		{name: "one deciding it off, after the start", now: start.Add(time.Hour), taskworkers: 4, publishing: 4, onEvery: false, onAny: true, scheduling: 3, earliest: start, start: start, tier: tierPage},
		{name: "one without the gate, after the start", now: start.Add(time.Hour), taskworkers: 4, publishing: 3, onEvery: true, onAny: true, scheduling: 3, earliest: start, start: start, tier: tierPage},
		{name: "one without the gate, at 18:00 on the start's day", now: start.Add(18 * time.Hour), taskworkers: 4, publishing: 3, onEvery: true, onAny: true, scheduling: 3, earliest: start, start: start, tier: tierWarn},
		{name: "one on a start moved later, 73 hours before its start", now: start.Add(-73 * time.Hour), taskworkers: 4, publishing: 4, scheduling: 4, earliest: start, start: moved},
		{name: "one on a start moved later, before its start", now: start.Add(-time.Hour), taskworkers: 4, publishing: 4, scheduling: 4, earliest: start, start: moved, tier: tierWarn},
		{name: "one on a start moved later, after its start", now: start.Add(time.Hour), taskworkers: 4, publishing: 4, onEvery: false, onAny: true, scheduling: 4, earliest: start, start: moved, tier: tierPage},
		{name: "two starts on one day, both passed", now: start.Add(7 * time.Hour), taskworkers: 4, publishing: 4, onEvery: true, onAny: true, scheduling: 4, earliest: start, start: start.Add(6 * time.Hour)},
	} {
		observation := modernGateReadinessTestObservation(c.now, 90, 10, 90, 10)
		observation.start = &c.start
		observation.settled = modernGateReadinessFleet{
			taskworkers: c.taskworkers, publishing: c.publishing, onEvery: c.onEvery, onAny: c.onAny,
			scheduling: c.scheduling, earliestStart: &c.earliest, latestStart: &c.start,
		}
		observation.countsStarted = c.now.Add(-5 * time.Minute)
		observation.bucketsBefore = map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}
		findings, err := evaluateModernGateReadiness(observation, c.now)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		disagrees, ok := modernGateReadinessTestFailing(t, findings)[""]
		if ok != (c.tier != "") || ok && (disagrees.class != "modern-gate-fleet-disagrees" || disagrees.tier != c.tier) {
			t.Errorf("%s: findings %+v, want a fleet finding at %q", c.name, findings, c.tier)
		}
	}

	unknown := modernGateReadinessTestObservation(start.Add(-time.Hour), 90, 10, 90, 10)
	unknown.taskworkers = 0
	if _, err := evaluateModernGateReadiness(unknown, start.Add(-time.Hour)); err == nil {
		t.Error("a fleet without taskworker process series was judged")
	}

	// counts too old to judge the shadow keep the fleet's finding, beside
	// what could not be observed
	now := start.Add(-time.Hour)
	stale := modernGateReadinessTestObservation(now, 90, 10, 90, 10)
	stale.settled.publishing, stale.settled.scheduling = 3, 3
	stale.countsStarted = now.Add(-modernGateReadinessMaxCountsAge - time.Second)
	findings, err := evaluateModernGateReadiness(stale, now)
	classes := []string{}
	for _, f := range findings {
		classes = append(classes, f.class)
	}
	if err != nil || strings.Join(classes, ",") != "modern-gate-fleet-disagrees,cannot-observe" {
		t.Errorf("a disagreeing fleet with stale counts: classes %v err %v, want the fleet and cannot-observe", classes, err)
	}
}

// Outside the windows, and while provider.yml sets the switch on every
// taskworker so that no start is published, a fleet whose settled
// taskworkers decide the gate differently still WARNs: one on another
// switch, or one that does not publish the gate while the gate is on
// anywhere, since it decides the gate off. A fleet that agrees is quiet, and
// so is one without the gate beside a gate off everywhere, as in a rollout
// from code before the gate days ahead of the start: both decide it off.
func TestModernGateReadinessWarnsOnADisagreeingFleetOutsideItsWindows(t *testing.T) {
	start := modernGateReadinessTestStart
	for _, c := range []struct {
		name       string
		now        time.Time
		scheduled  bool
		publishing int
		onEvery    bool
		onAny      bool
		warn       bool
	}{
		{name: "a start published, a month before, one switched on", now: start.AddDate(0, 0, -30), scheduled: true, publishing: 4, onEvery: false, onAny: true, warn: true},
		{name: "a start published, five days before, half the fleet on code before the gate", now: start.AddDate(0, 0, -5), scheduled: true, publishing: 2, onEvery: false, onAny: false},
		{name: "a start published, a week after, one without the gate", now: start.AddDate(0, 0, 7), scheduled: true, publishing: 3, onEvery: true, onAny: true, warn: true},
		{name: "a start published, a week after, agreeing", now: start.AddDate(0, 0, 7), scheduled: true, publishing: 4, onEvery: true, onAny: true},
		{name: "no start, one switched on", now: start, publishing: 4, onEvery: false, onAny: true, warn: true},
		{name: "no start, on everywhere, one without the gate", now: start, publishing: 3, onEvery: true, onAny: true, warn: true},
		{name: "no start, off everywhere, one without the gate", now: start, publishing: 3, onEvery: false, onAny: false},
		{name: "no start, agreeing", now: start, publishing: 4, onEvery: true, onAny: true},
	} {
		observation := modernGateReadinessTestObservation(c.now, 90, 10, 90, 10)
		observation.settled.publishing, observation.settled.onEvery, observation.settled.onAny = c.publishing, c.onEvery, c.onAny
		if !c.scheduled {
			observation.start = nil
			observation.settled.scheduling, observation.settled.earliestStart, observation.settled.latestStart = 0, nil, nil
		}
		findings, err := evaluateModernGateReadiness(observation, c.now)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		disagrees, ok := modernGateReadinessTestFailing(t, findings)[""]
		if ok != c.warn || ok && (disagrees.class != "modern-gate-fleet-disagrees" || disagrees.tier != tierWarn) {
			t.Errorf("%s: findings %+v, want a fleet warning %t", c.name, findings, c.warn)
		}
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
	moved.start, moved.settled.earliestStart, moved.settled.latestStart = &movedStart, &movedStart, &movedStart
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

// One taskworker process as Mimir holds it: when it started, and the gate
// and the start it publishes, nil for none.
type modernGateReadinessTestProcess struct {
	started time.Time
	on      *bool
	start   *time.Time
}

// count taskworker processes that started a day before at, each publishing
// the gate as on and, unless start is nil, that start.
func modernGateReadinessTestProcesses(count int, at time.Time, on bool, start *time.Time) []modernGateReadinessTestProcess {
	processes := []modernGateReadinessTestProcess{}
	for range count {
		processes = append(processes, modernGateReadinessTestProcess{started: at.AddDate(0, 0, -1), on: &on, start: start})
	}
	return processes
}

// One Mimir answer for the readiness query: the parts the processes give at
// at, as the query reads them, every process for the gate's presence and the
// latest start and those that started five minutes before at for the
// agreement, then the counts' start, the shadow and the buckets. An
// aggregate over no series is absent, as in Mimir.
func modernGateReadinessTestPayload(t testing.TB, at time.Time, processes []modernGateReadinessTestProcess, countsStarted time.Time, shadow map[string]map[string]float64, buckets map[string]float64) string {
	t.Helper()
	result := []map[string]any{}
	add := func(labels map[string]string, value float64) {
		result = append(result, map[string]any{
			"metric": labels,
			"value":  []any{float64(at.Unix()), strconv.FormatFloat(value, 'f', -1, 64)},
		})
	}
	part := func(name string, value float64) {
		add(map[string]string{"monitor_modern_gate_part": name}, value)
	}
	// the count, min and max of values under prefix, none for no values
	aggregates := func(prefix string, values []float64) {
		if 0 < len(values) {
			part(prefix+"count", float64(len(values)))
			part(prefix+"min", slices.Min(values))
			part(prefix+"max", slices.Max(values))
		}
	}
	gates, starts := []float64{}, []float64{}
	settled, settledGates, settledStarts := 0, []float64{}, []float64{}
	for _, process := range processes {
		isSettled := process.started.Before(at.Add(-modernGateReadinessSettleAge))
		if isSettled {
			settled += 1
		}
		if process.on != nil {
			gate := 0.0
			if *process.on {
				gate = 1
			}
			gates = append(gates, gate)
			if isSettled {
				settledGates = append(settledGates, gate)
			}
		}
		if process.start != nil {
			starts = append(starts, float64(process.start.Unix()))
			if isSettled {
				settledStarts = append(settledStarts, float64(process.start.Unix()))
			}
		}
	}
	if 0 < len(gates) {
		part("on_count", float64(len(gates)))
	}
	if 0 < len(processes) {
		part("taskworkers", float64(len(processes)))
	}
	if 0 < len(starts) {
		part("start", slices.Max(starts))
	}
	if 0 < settled {
		part("settled_taskworkers", float64(settled))
	}
	aggregates("settled_on_", settledGates)
	aggregates("settled_start_", settledStarts)
	part("counts_started", float64(countsStarted.Unix()))
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

// The signal with the desired provider.yml's failure, "" for none.
func modernGateReadinessTestSignal(failure string) Signal {
	return newModernGateReadinessSignal(func() string { return failure })
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
	payload := modernGateReadinessTestPayload(t, now, modernGateReadinessTestProcesses(4, now, false, &start), now.Add(-4*time.Minute), modernGateReadinessTestGuttedShadow(), map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100})
	alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, map[int64]string{now.Unix(): payload}))
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
		now.Unix():    modernGateReadinessTestPayload(t, now, modernGateReadinessTestProcesses(4, now, true, &start), start.Add(time.Minute), modernGateReadinessTestGuttedShadow(), map[string]float64{model.RankModeQuality: 30, model.RankModeSpeed: 60}),
		before.Unix(): modernGateReadinessTestPayload(t, before, nil, before, nil, map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}),
	}
	alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, payloads))
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

// Through the signal, six hours after the start, with one taskworker still
// on a config that sets the kill switch: its min reads the disagreement,
// which pages, and the flip-drop waits for a fleet that has flipped.
func TestModernGateReadinessSignalPagesADisagreeingFleet(t *testing.T) {
	start := modernGateReadinessTestStart
	now := start.Add(6 * time.Hour)
	before := start.Add(-time.Second)
	off := false
	processes := append(modernGateReadinessTestProcesses(3, now, true, &start), modernGateReadinessTestProcess{started: now.AddDate(0, 0, -1), on: &off})
	payloads := map[int64]string{
		now.Unix():    modernGateReadinessTestPayload(t, now, processes, start.Add(time.Minute), modernGateReadinessTestGuttedShadow(), map[string]float64{model.RankModeQuality: 30, model.RankModeSpeed: 60}),
		before.Unix(): modernGateReadinessTestPayload(t, before, nil, before, nil, map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}),
	}
	alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, payloads))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts %+v, want the fleet alone", alerts)
	}
	alert := requireAlertClass(t, alerts, "modern-gate-fleet-disagrees")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), "gate_on_min=0 gate_on_max=1") {
		t.Fatalf("alert %+v, want a page reading the min and the max", alert)
	}
}

// Through the signal, five days before the start, in the middle of a
// rollout from code before the gate: two settled taskworkers publish the
// gate off and the start, two publish nothing. Both decide the gate off, so
// nothing fires; three days before the start, inside the windows, the two
// that will not flip WARN.
func TestModernGateReadinessSignalIsQuietDuringARolloutFromBeforeTheGate(t *testing.T) {
	start := modernGateReadinessTestStart
	for _, c := range []struct {
		now  time.Time
		warn bool
	}{
		{now: start.AddDate(0, 0, -5)},
		{now: start.Add(-modernGateReadinessWarnLead + time.Hour), warn: true},
	} {
		processes := append(modernGateReadinessTestProcesses(2, c.now, false, &start), modernGateReadinessTestProcess{started: c.now.AddDate(0, 0, -1)}, modernGateReadinessTestProcess{started: c.now.AddDate(0, 0, -1)})
		payload := modernGateReadinessTestPayload(t, c.now, processes, c.now.Add(-4*time.Minute), modernGateReadinessTestGuttedShadow(), map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100})
		alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, c.now, map[int64]string{c.now.Unix(): payload}))
		if err != nil {
			t.Fatalf("at %s: %v", c.now, err)
		}
		fleet := []Alert{}
		for _, alert := range alerts {
			if alert.Class == "modern-gate-fleet-disagrees" {
				fleet = append(fleet, alert)
			}
		}
		if (len(fleet) == 1 && fleet[0].Severity == SeverityWarn) != c.warn || !c.warn && len(fleet) != 0 {
			t.Errorf("at %s: fleet alerts %+v, want a warning %t", c.now, fleet, c.warn)
		}
	}
}

// Through the signal, an hour after the start: a block restarted two minutes
// ago, whose new process has not registered the gate yet while its drained
// one's series are still readable, is not judged and does not page; the
// same process ten minutes after its start, deciding the gate off, pages.
func TestModernGateReadinessSignalJudgesSettledProcesses(t *testing.T) {
	start := modernGateReadinessTestStart
	now := start.Add(time.Hour)
	before := start.Add(-time.Second)
	off := false
	for _, c := range []struct {
		name    string
		process modernGateReadinessTestProcess
		page    bool
	}{
		{name: "restarted two minutes ago, the gate not yet registered", process: modernGateReadinessTestProcess{started: now.Add(-2 * time.Minute)}},
		{name: "restarted two minutes ago, deciding the gate off", process: modernGateReadinessTestProcess{started: now.Add(-2 * time.Minute), on: &off}},
		{name: "started ten minutes ago, deciding the gate off", process: modernGateReadinessTestProcess{started: now.Add(-10 * time.Minute), on: &off}, page: true},
	} {
		// three taskworkers, the drained process of the restarted block, and
		// its new process
		processes := append(modernGateReadinessTestProcesses(4, now, true, &start), c.process)
		payloads := map[int64]string{
			now.Unix():    modernGateReadinessTestPayload(t, now, processes, start.Add(time.Minute), modernGateReadinessTestGuttedShadow(), map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}),
			before.Unix(): modernGateReadinessTestPayload(t, before, nil, before, nil, map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}),
		}
		alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, payloads))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !c.page {
			if len(alerts) != 0 {
				t.Errorf("%s: alerts %+v, want none", c.name, alerts)
			}
			continue
		}
		if len(alerts) != 1 || alerts[0].Class != "modern-gate-fleet-disagrees" || alerts[0].Severity != SeverityPage {
			t.Errorf("%s: alerts %+v, want the fleet's page", c.name, alerts)
		}
	}
}

// A postponement that failed in the desired provider.yml WARNs as an
// unknown state, the schedule that was meant: beside a fleet that holds the
// gate off as it says, and beside a Mimir that cannot be read. Without a
// failure nothing fires.
func TestModernGateReadinessSignalWarnsOnAFailedPostponement(t *testing.T) {
	start := modernGateReadinessTestStart
	now := start.Add(-time.Hour)
	failure := "quality_speed_modern_provider_start is neither an RFC 3339 instant nor a date"
	// the taskworkers on the failed release: the gate off and no start
	off := modernGateReadinessTestPayload(t, now, modernGateReadinessTestProcesses(4, now, false, nil), now.Add(-4*time.Minute), modernGateReadinessTestGuttedShadow(), map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100})

	alerts, err := modernGateReadinessTestSignal(failure).Run(context.Background(), modernGateReadinessTestSettings(t, now, map[int64]string{now.Unix(): off}))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts %+v, want the failure alone", alerts)
	}
	alert := requireAlertClass(t, alerts, "modern-gate-schedule-unreadable")
	if alert.Severity != SeverityWarn || !isVisibilityAlert(alert) || !strings.Contains(alert.Markdown(), "failure="+failure) || !strings.Contains(alert.Markdown(), "quality_speed_modern_provider_start:") {
		t.Fatalf("alert %+v, want a visibility warning naming the failure and the key to correct", alert)
	}

	// no answer for the evaluation time: Mimir cannot be read
	alerts, err = modernGateReadinessTestSignal(failure).Run(context.Background(), modernGateReadinessTestSettings(t, now, map[int64]string{}))
	if err != nil {
		t.Fatal(err)
	}
	classes := []string{}
	for _, alert := range alerts {
		classes = append(classes, alert.Class)
	}
	if strings.Join(classes, ",") != "modern-gate-schedule-unreadable,cannot-observe" {
		t.Fatalf("classes %v, want the failure beside cannot-observe", classes)
	}

	alerts, err = modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, map[int64]string{now.Unix(): off}))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("alerts %+v err %v without a failure, want none", alerts, err)
	}
}

// The query reads every part from the configured environment only.
func TestModernGateReadinessQueryReadsEveryPart(t *testing.T) {
	query := modernGateReadinessQuery("synthetic")
	settled := ` and on(instance) (process_start_time_seconds{env="synthetic",job="taskworker"} < time() - 300))`
	for _, part := range []string{
		`count(urnetwork_stats_provider_modern_gate_on{env="synthetic",job="taskworker"})`,
		`count(process_start_time_seconds{env="synthetic",job="taskworker"})`,
		`max(urnetwork_stats_provider_modern_gate_start_seconds{env="synthetic",job="taskworker"})`,
		`count(process_start_time_seconds{env="synthetic",job="taskworker"} < time() - 300)`,
		`count(urnetwork_stats_provider_modern_gate_on{env="synthetic",job="taskworker"}` + settled,
		`min(urnetwork_stats_provider_modern_gate_on{env="synthetic",job="taskworker"}` + settled,
		`max(urnetwork_stats_provider_modern_gate_on{env="synthetic",job="taskworker"}` + settled,
		`count(urnetwork_stats_provider_modern_gate_start_seconds{env="synthetic",job="taskworker"}` + settled,
		`min(urnetwork_stats_provider_modern_gate_start_seconds{env="synthetic",job="taskworker"}` + settled,
		`max(urnetwork_stats_provider_modern_gate_start_seconds{env="synthetic",job="taskworker"}` + settled,
		`max(urnetwork_stats_provider_egress_source_started_seconds{env="synthetic"})`,
		`max by (bucket, outcome) (urnetwork_stats_provider_modern_gate_shadow{env="synthetic",bucket=~"quality|speed"})`,
		`sum by (bucket) (max by (bucket, index) (urnetwork_stats_provider_egress_index{env="synthetic",bucket=~"quality|speed"}))`,
	} {
		if !strings.Contains(query, part) {
			t.Errorf("query %q does not read %q", query, part)
		}
	}
	if strings.Count(query, "env=") != 19 {
		t.Errorf("query %q reads a part outside the environment", query)
	}
}
