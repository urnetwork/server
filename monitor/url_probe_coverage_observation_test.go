// The public signal preserves healthy, broken, partial and unknown coverage records.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// Every execution reads exactly one bounded synthetic response and records it once.
func runUrlProbeObservationFixture(t *testing.T, now time.Time, payload string, receive func(UrlProbeCoverageObservation) error) (Alerts, error) {
	t.Helper()
	contacts := 0
	settings := syntheticSettings(&syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		contacts++
		if !strings.Contains(command, "urnetwork_url_probe_admission_cohort{") {
			t.Fatal("watcher omitted the age cohort from its existing query")
		}
		return payload, nil
	}})
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics.example", Roles: []string{"services"}})
	settings.LogServices = []string{"taskworker"}
	settings.LogServiceHosts = map[string][]string{"taskworker": {"worker-a.example", "worker-b.example"}}
	settings.LogServiceBlocks = map[string][]string{"taskworker": {"worker-group"}}
	settings.UrlProbeCoverageObserver = receive
	signal := NewUrlProbeCoverageSignal().(*signalAdapter)
	signal.probe = urlProbeCoverageProbe{loadDesired: func() (urlProbeCoverageDesired, error) {
		return urlProbeCoverageDesired{enabled: true, shardCount: 2}, nil
	}}
	alerts, err := signal.Run(context.Background(), settings)
	if contacts != 1 {
		t.Fatalf("observation added or skipped source reads: %d", contacts)
	}
	return alerts, err
}

// Healthy mature coverage stays visible while genuinely younger cycles accumulate credit.
func TestUrlProbeObservationHealthyNewcomersKeepMatureHundredPercent(t *testing.T) {
	now := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	setUrlProbeAdmissionCohortFixture(first, [3][3]float64{{100, 100, 0}, {30, 2, 140}, {0, 0, 0}})
	var output bytes.Buffer
	for range 2 {
		alerts, err := runUrlProbeObservationFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second), func(record UrlProbeCoverageObservation) error {
			return record.WriteJsonl(&output)
		})
		if err != nil || len(alerts) != 0 {
			t.Fatalf("healthy coverage manufactured an alert: err=%v alerts=%d", err, len(alerts))
		}
	}
	decoder := json.NewDecoder(&output)
	for range 2 {
		var record UrlProbeCoverageObservation
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("healthy execution lost its continuous record: %v", err)
		}
		if record.SchemaVersion != 1 || record.AgeDomain != "immutable_probe_cycle_started_at" || !record.ObservedAt.Equal(now) ||
			record.AllCurrent == nil || record.AllCurrent.Eligible != 130 || record.AllCurrent.QuotaComplete != 102 ||
			record.Mature == nil || record.Mature.Eligible != 100 || record.Warming == nil || record.Warming.Eligible != 30 ||
			record.AgeUnknown == nil || record.AgeUnknown.Eligible != 0 || record.KnownMatureQuotaPercent == nil || *record.KnownMatureQuotaPercent != 100 ||
			!record.SourceCoverageComplete || !record.WholeFleetAgeKnown || record.SourceObservedSeconds == nil || *record.SourceObservedSeconds != first.values["observed"] ||
			record.SourceSampleSeconds == nil || *record.SourceSampleSeconds != first.values["observed_time"] || record.SecurityPending == nil || *record.SecurityPending != 0 {
			t.Fatalf("healthy record lost a qualified age partition or source clock: %+v", record)
		}
	}
	if err := decoder.Decode(&UrlProbeCoverageObservation{}); err != io.EOF {
		t.Fatalf("unexpected extra observation: %v", err)
	}
}

// Unknown age, unavailable extensions, and partial shards cannot erase an independently valid census.
func TestUrlProbeObservationSeparatesDeficitUnknownPartialAndEmpty(t *testing.T) {
	now := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name         string
		counts       [3][3]float64
		change       func(*urlProbeCoverageProcess, *urlProbeCoverageProcess)
		wantAlert    string
		wantMature   bool
		wantPercent  bool
		wantComplete bool
	}{
		{name: "mature deficit", counts: [3][3]float64{{100, 99, 1}, {30, 2, 140}, {0, 0, 0}}, wantAlert: "url-probe-coverage-deficit", wantMature: true, wantPercent: true, wantComplete: true},
		{name: "unknown age", counts: [3][3]float64{{100, 100, 0}, {0, 0, 0}, {30, 2, 140}}, wantAlert: "url-probe-coverage-unobservable", wantMature: true, wantPercent: true},
		{name: "missing extension", counts: [3][3]float64{{100, 100, 0}, {0, 0, 0}, {0, 0, 0}}, change: func(a, _ *urlProbeCoverageProcess) { delete(a.values, "cohort_contract") }, wantAlert: "url-probe-coverage-unobservable"},
		{name: "partial shards", counts: [3][3]float64{{100, 100, 0}, {0, 0, 0}, {0, 0, 0}}, change: func(_, b *urlProbeCoverageProcess) { delete(b.values, "heartbeat:1") }, wantAlert: "url-probe-coverage-unobservable", wantMature: true, wantPercent: true},
		{name: "empty mature", counts: [3][3]float64{{0, 0, 0}, {30, 2, 140}, {0, 0, 0}}, wantMature: true, wantComplete: true},
	} {
		first := urlProbeCoverageFixture(now, "worker-a.example", 0)
		second := urlProbeCoverageFixture(now, "worker-b.example", 1)
		setUrlProbeAdmissionCohortFixture(first, test.counts)
		if test.change != nil {
			test.change(first, second)
		}
		records := []UrlProbeCoverageObservation{}
		alerts, err := runUrlProbeObservationFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second), func(record UrlProbeCoverageObservation) error {
			records = append(records, record)
			return nil
		})
		if err != nil || len(records) != 1 {
			t.Fatalf("%s: missing observation: %v", test.name, err)
		}
		if test.wantAlert != "" {
			requireAlertClass(t, alerts, test.wantAlert)
		} else if len(alerts) != 0 {
			t.Fatalf("%s: false alert", test.name)
		}
		record := records[0]
		if record.CensusReason != "ok" || record.AllCurrent == nil || (record.Mature != nil) != test.wantMature ||
			(record.KnownMatureQuotaPercent != nil) != test.wantPercent || record.SourceCoverageComplete != test.wantComplete {
			t.Fatalf("%s: evidence boundaries changed: %+v", test.name, record)
		}
		if record.AgeUnknown != nil && record.AgeUnknown.Eligible > 0 && record.WholeFleetAgeKnown {
			t.Fatalf("%s: unknown age certified whole fleet", test.name)
		}
	}
}

// Invalid, stale, and ownerless samples replace prior numeric observations with explicit nulls.
func TestUrlProbeObservationUnavailableDoesNotRepeatLastCounts(t *testing.T) {
	now := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)
	for _, mode := range []string{"malformed", "stale", "ownerless"} {
		first := urlProbeCoverageFixture(now, "worker-a.example", 0)
		second := urlProbeCoverageFixture(now, "worker-b.example", 1)
		if mode == "stale" {
			first.values["observed"] = float64(now.Add(-4 * time.Minute).Unix())
		}
		if mode == "ownerless" {
			delete(first.values, "heartbeat:0")
		}
		payload := urlProbeCoverageFixtureJson(t, now, first, second)
		if mode == "malformed" {
			payload = "{"
		}
		calls := 0
		alerts, err := runUrlProbeObservationFixture(t, now, payload, func(record UrlProbeCoverageObservation) error {
			calls++
			if record.AllCurrent != nil || record.Mature != nil || record.KnownMatureQuotaPercent != nil || record.SourceCoverageComplete || record.CensusReason == "ok" {
				t.Fatalf("%s: unavailable sample acquired numeric coverage: %+v", mode, record)
			}
			return nil
		})
		if err != nil || calls != 1 {
			t.Fatalf("%s: unavailable record dropped: %v", mode, err)
		}
		requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
	}
}

// A local output failure must retain real deficits and avoid exposing arbitrary writer errors.
func TestUrlProbeObservationOutputFailureRetainsAlerts(t *testing.T) {
	now := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	setUrlProbeAdmissionCohortFixture(first, [3][3]float64{{100, 99, 1}, {0, 0, 0}, {0, 0, 0}})
	alerts, err := runUrlProbeObservationFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second), func(UrlProbeCoverageObservation) error {
		return errors.New("synthetic-private-writer-error")
	})
	if err == nil || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatal("output error was hidden or leaked")
	}
	if len(alerts) != 1 {
		t.Fatalf("output failure discarded real findings: %d", len(alerts))
	}
	requireAlertClass(t, alerts, "url-probe-coverage-deficit")
	if err := (UrlProbeCoverageObservation{}).WriteJsonl(shortNilWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("partial record passed: %v", err)
	}
}
