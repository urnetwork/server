// Synthetic exact-child headroom controls exercise the real parser, probe,
// ticket lifecycle and rendering without a production metric or identity.
package monitor

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// One complete strict frame keeps the limit and retained head paired to the
// same synthetic child; the active set deliberately differs from the head.
func headroomChildFixture(port int, localLimit, memorySeries, discards int64) string {
	return mimirAdmissionInstanceFixture(mimirAdmissionFixtureOptions{
		port: port, observable: true, processStart: "2000000000.25",
		localLimit: localLimit, globalLimit: localLimit, memorySeries: memorySeries,
		activeSeries: 1, createdTotal: 500, removedTotal: 10,
		ingestionRateLimit: 1234, ingestionBurstLimit: 5678,
		discardDescriptor: true, discardPresent: true, discardTotal: discards,
		rateDiscardPresent: true,
	})
}

// Two services hosts stay in the desired inventory even when a response is
// unknown, so losing a sibling cannot silently shrink the recovery target.
func headroomHostResponses(first string, second string) map[string]mimirAdmissionSyntheticResponse {
	return map[string]mimirAdmissionSyntheticResponse{
		"metrics-a.example": {output: mimirAdmissionHostFixture([]string{first}, true, 0, 0, 0)},
		"metrics-b.example": {output: mimirAdmissionHostFixture([]string{second}, true, 0, 0, 0)},
	}
}

// Runs the actual check so tests can inspect healthy sentinels, which the
// public alert adapter intentionally omits. Every host gets one fixed read.
func headroomCheck(t *testing.T, probe *mimirAdmissionProbe, now time.Time, responses map[string]mimirAdmissionSyntheticResponse) []finding {
	t.Helper()
	var commands atomic.Int64
	var invalidCommand atomic.Bool
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		commands.Add(1)
		if !strings.Contains(command, mimirAdmissionMarker) || !strings.Contains(command, "/metrics") || !strings.Contains(command, "/config") {
			invalidCommand.Store(true)
		}
		response, ok := responses[host.Name]
		if !ok {
			return "", fmt.Errorf("unexpected synthetic host")
		}
		return response.output, response.err
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic-environment"
	settings.LogServices = []string{"api", "connect", "taskworker"}
	settings.LogServiceBlocks = map[string][]string{
		"api": {"synthetic-api"}, "connect": {"synthetic-connect"}, "taskworker": {"synthetic-worker"},
	}
	settings.Now = func() time.Time { return now }
	names := make([]string, 0, len(responses))
	for name := range responses {
		names = append(names, name)
	}
	sort.Strings(names)
	settings.Hosts = nil
	for _, name := range names {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: name, Roles: []string{"services"}})
	}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := probe.check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if commands.Load() != int64(len(responses)) || invalidCommand.Load() {
		t.Fatalf("invalid bounded source use: commands=%d hosts=%d malformed=%t", commands.Load(), len(responses), invalidCommand.Load())
	}
	return findings
}

// Requires one classification rather than treating absence as healthy.
func requireHeadroomFinding(t *testing.T, findings []finding, healthy bool) finding {
	t.Helper()
	var found []finding
	for _, f := range findings {
		if f.class == "mimir-series-headroom" {
			found = append(found, f)
		}
	}
	if len(found) != 1 || found[0].healthy != healthy {
		t.Fatalf("headroom finding count=%d, want one healthy=%t", len(found), healthy)
	}
	f := found[0]
	if f.probeId != "observability/mimir-admission" || f.target != "mimir-fleet" || f.tier != tierWarn {
		t.Fatalf("wrong headroom identity or tier: %+v", f)
	}
	if !healthy && (f.frame != "retained-series-headroom" || f.sustain != 1) {
		t.Fatalf("headroom warning changed its immediate stable identity: %+v", f)
	}
	return f
}

// Unknown coverage must neither manufacture a healthy gauge nor discard an
// independently known violation. This helper selects the unknown-only case.
func requireHeadroomUnknown(t *testing.T, findings []finding) {
	t.Helper()
	visible := false
	for _, f := range findings {
		if f.class == "mimir-series-headroom" {
			t.Fatalf("unknown headroom was classified: healthy=%t", f.healthy)
		}
		visible = visible || f.class == "cannot-observe" && !f.healthy
	}
	if !visible {
		t.Fatal("unknown source lost its visibility finding")
	}
}

// A current low gauge is affirmative even before a counter-delta baseline,
// with zero discard counters and a tiny active set.
func TestMimirAdmissionHeadroomInitialLowWarnsWithoutDiscards(t *testing.T) {
	now := time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC)
	responses := headroomHostResponses(headroomChildFixture(19191, 1000, 951, 0), headroomChildFixture(19192, 2000, 500, 0))
	findings := headroomCheck(t, &mimirAdmissionProbe{}, now, responses)
	f := requireHeadroomFinding(t, findings, false)
	for _, wanted := range []string{"low_headroom_instances=1", "headroom_observed_instances=2", "direct_complete=true"} {
		if !strings.Contains(f.observed, wanted) {
			t.Fatalf("headroom evidence omitted %q", wanted)
		}
	}
	for _, f := range findings {
		if !f.healthy && (f.class == "mimir-series-limit" || f.class == "mimir-ingestion-rate-limit") {
			t.Fatal("gauge risk was misclassified as affirmative sample loss")
		}
	}
}

// Clearing the independent two-hour discard hold cannot hide low reserve.
func TestMimirAdmissionHeadroomSurvivesDiscardQuietRecovery(t *testing.T) {
	start := time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC)
	probe := &mimirAdmissionProbe{}
	responses := headroomHostResponses(headroomChildFixture(19191, 1000, 950, 2), headroomChildFixture(19192, 1000, 200, 0))
	headroomCheck(t, probe, start, responses)
	headroomCheck(t, probe, start.Add(time.Minute), responses)
	findings := headroomCheck(t, probe, start.Add(time.Minute+mimirAdmissionQuietWindow), responses)
	seriesHealthy := false
	for _, f := range findings {
		if f.class == "mimir-series-limit" {
			if !f.healthy {
				t.Fatal("fixture did not complete the discard quiet hold")
			}
			seriesHealthy = true
		}
	}
	if !seriesHealthy {
		t.Fatal("discard quiet recovery did not emit its own healthy sentinel")
	}
	requireHeadroomFinding(t, findings, false)
}

// A valid sibling remains actionable when a different host is unavailable.
func TestMimirAdmissionHeadroomLowSiblingSurvivesUnknown(t *testing.T) {
	now := time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC)
	responses := headroomHostResponses(headroomChildFixture(19191, 1000, 950, 0), "")
	responses["metrics-b.example"] = mimirAdmissionSyntheticResponse{err: fmt.Errorf("synthetic-secret.example/token=do-not-render")}
	findings := headroomCheck(t, &mimirAdmissionProbe{}, now, responses)
	f := requireHeadroomFinding(t, findings, false)
	if !strings.Contains(f.observed, "direct_complete=false") || !strings.Contains(f.observed, "headroom_observed_instances=1") {
		t.Fatal("known sibling warning lost partial coverage")
	}
	visible := false
	for _, f := range findings {
		visible = visible || f.class == "cannot-observe" && !f.healthy
		if strings.Contains(f.symptom+f.evidence+f.context+f.observed+f.action, "do-not-render") {
			t.Fatal("remote error escaped the fixed cause boundary")
		}
	}
	if !visible {
		t.Fatal("unknown sibling was hidden by the positive warning")
	}
}

// An unknown child in the same host must not erase its valid sibling.
func TestMimirAdmissionHeadroomLowChildSurvivesUnknownChild(t *testing.T) {
	now := time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC)
	responses := map[string]mimirAdmissionSyntheticResponse{"metrics-a.example": {output: mimirAdmissionHostFixture([]string{
		headroomChildFixture(19191, 1000, 950, 0),
		mimirAdmissionInstanceFixture(mimirAdmissionFixtureOptions{port: 19192}),
	}, true, 0, 0, 0)}}
	f := requireHeadroomFinding(t, headroomCheck(t, &mimirAdmissionProbe{}, now, responses), false)
	if !strings.Contains(f.observed, "observable_hosts=0") || !strings.Contains(f.observed, "headroom_observed_instances=1") {
		t.Fatal("partial same-host coverage was presented as complete")
	}
}

// Integer arithmetic includes exact ten-percent equality and excess head;
// it must not overflow by multiplying a large limit or a negative reserve.
func TestMimirAdmissionHeadroomThresholdIsInclusiveAndOverflowSafe(t *testing.T) {
	for _, test := range []struct {
		limit  int64
		memory int64
	}{
		{limit: 1000, memory: 900}, {limit: 1001, memory: 901},
		{limit: 1000, memory: 1001}, {limit: math.MaxInt64, memory: math.MaxInt64 - math.MaxInt64/10},
	} {
		responses := headroomHostResponses(headroomChildFixture(19191, test.limit, test.memory, 0), headroomChildFixture(19192, 1000, 100, 0))
		requireHeadroomFinding(t, headroomCheck(t, &mimirAdmissionProbe{}, time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC), responses), false)
	}
}

// Healthy recovery starts only after current complete comparable coverage.
func TestMimirAdmissionHeadroomAboveThresholdRecovers(t *testing.T) {
	start := time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC)
	probe := &mimirAdmissionProbe{}
	responses := headroomHostResponses(headroomChildFixture(19191, 1000, 899, 0), headroomChildFixture(19192, 1001, 900, 0))
	for _, f := range headroomCheck(t, probe, start, responses) {
		if f.class == "mimir-series-headroom" {
			t.Fatal("first counter generation manufactured a healthy recovery")
		}
	}
	requireHeadroomFinding(t, headroomCheck(t, probe, start.Add(time.Minute), responses), true)
}

// Fleet extrema cannot be combined: one child's small absolute reserve is
// not measured against a different child's much larger limit.
func TestMimirAdmissionHeadroomPairsEachChildLimit(t *testing.T) {
	start := time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC)
	probe := &mimirAdmissionProbe{}
	responses := headroomHostResponses(headroomChildFixture(19191, 1000, 800, 0), headroomChildFixture(19192, 1000000, 800000, 0))
	for _, f := range headroomCheck(t, probe, start, responses) {
		if f.class == "mimir-series-headroom" {
			t.Fatal("mixed-child extrema manufactured a low-headroom warning")
		}
	}
	requireHeadroomFinding(t, headroomCheck(t, probe, start.Add(time.Minute), responses), true)
}

// Missing high-headroom siblings stay unknown rather than authorizing fleet
// recovery from the surviving healthy subset.
func TestMimirAdmissionHeadroomPartialHighIsUnknown(t *testing.T) {
	responses := headroomHostResponses(headroomChildFixture(19191, 1000, 100, 0), "")
	responses["metrics-b.example"] = mimirAdmissionSyntheticResponse{err: fmt.Errorf("synthetic unavailable")}
	requireHeadroomUnknown(t, headroomCheck(t, &mimirAdmissionProbe{}, time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC), responses))
}

// Numeric corruption, duplicate fields and trailing private content are not
// authoritative zero, and cannot escape through a rendered visibility error.
func TestMimirAdmissionHeadroomMalformedIsUnknownAndRedacted(t *testing.T) {
	valid := headroomChildFixture(19191, 1000, 950, 0)
	for _, frame := range []string{
		strings.Replace(valid, "memory_series 950", "memory_series NaN", 1),
		strings.Replace(valid, "local_limit 1000", "local_limit 0", 1),
		strings.Replace(valid, "local_limit 1000", "local_limit 1000\nlocal_limit 1000", 1),
		valid + "synthetic-secret.example/token=do-not-render\n",
	} {
		findings := headroomCheck(t, &mimirAdmissionProbe{}, time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC), map[string]mimirAdmissionSyntheticResponse{
			"metrics-a.example": {output: mimirAdmissionHostFixture([]string{frame}, true, 0, 0, 0)},
		})
		requireHeadroomUnknown(t, findings)
		for _, f := range findings {
			if strings.Contains(f.symptom+f.evidence+f.context+f.observed+f.action, "do-not-render") {
				t.Fatal("malformed private content escaped")
			}
		}
	}
}

// Whole-family absence is usable only under the existing exact source
// contract; the headroom addition must not broaden that allowlist.
func TestMimirAdmissionHeadroomUnknownSourceAbsenceStaysUnknown(t *testing.T) {
	frame := headroomChildFixture(19191, 1000, 950, 0)
	frame = strings.Replace(frame, "discard_descriptor 1", "discard_descriptor 0", 1)
	frame = strings.Replace(frame, "discard_family_absent 0", "discard_family_absent 1", 1)
	frame = strings.Replace(frame, "discard_present 1", "discard_present 0", 1)
	frame = strings.Replace(frame, "rate_discard_present 1", "rate_discard_present 0", 1)
	requireHeadroomUnknown(t, headroomCheck(t, &mimirAdmissionProbe{}, time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC), map[string]mimirAdmissionSyntheticResponse{
		"metrics-a.example": {output: mimirAdmissionHostFixture([]string{frame}, true, 0, 0, 0)},
	}))
}

// A genuine counter reset is not a complete recovery even when the current
// headroom itself is high.
func TestMimirAdmissionHeadroomCounterResetCannotClear(t *testing.T) {
	start := time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC)
	probe := &mimirAdmissionProbe{}
	headroomCheck(t, probe, start, headroomHostResponses(headroomChildFixture(19191, 1000, 950, 4), headroomChildFixture(19192, 1000, 100, 0)))
	requireHeadroomUnknown(t, headroomCheck(t, probe, start.Add(time.Minute), headroomHostResponses(headroomChildFixture(19191, 1000, 100, 0), headroomChildFixture(19192, 1000, 100, 0))))
}

// Gauge recovery does not shorten the independently owned discard hold.
func TestMimirAdmissionHeadroomRecoveryPreservesDiscardPage(t *testing.T) {
	start := time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC)
	probe := &mimirAdmissionProbe{}
	first := headroomHostResponses(headroomChildFixture(19191, 1000, 950, 4), headroomChildFixture(19192, 1000, 100, 0))
	requireHeadroomFinding(t, headroomCheck(t, probe, start, first), false)
	findings := headroomCheck(t, probe, start.Add(time.Minute), headroomHostResponses(headroomChildFixture(19191, 1000, 100, 4), headroomChildFixture(19192, 1000, 100, 0)))
	requireHeadroomFinding(t, findings, true)
	page := false
	for _, f := range findings {
		page = page || f.class == "mimir-series-limit" && !f.healthy && f.tier == tierPage
	}
	if !page {
		t.Fatal("healthy headroom suppressed an active discard quiet hold")
	}
}

// Actual ticket reconciliation needs complete evidence, not an omitted
// alert, before it can resolve the independent headroom identity.
func TestMimirAdmissionHeadroomTicketRequiresCompleteRecovery(t *testing.T) {
	start := time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC)
	probe := &mimirAdmissionProbe{}
	manager := newTicketManager("synthetic", &ticketEscalationEmitter{})
	manager.resolveTicks = 2
	low := headroomHostResponses(headroomChildFixture(19191, 1000, 950, 0), headroomChildFixture(19192, 1000, 100, 0))
	manager.ingest(context.Background(), headroomCheck(t, probe, start, low))
	if manager.openCount() != 1 {
		t.Fatal("low headroom did not open its ticket")
	}
	high := headroomHostResponses(headroomChildFixture(19191, 1000, 100, 0), headroomChildFixture(19192, 1000, 100, 0))
	unknown := headroomHostResponses(headroomChildFixture(19191, 1000, 100, 0), "")
	unknown["metrics-b.example"] = mimirAdmissionSyntheticResponse{err: fmt.Errorf("synthetic unavailable")}
	for i := range manager.resolveTicks {
		manager.ingest(context.Background(), headroomCheck(t, probe, start.Add(time.Duration(i+1)*time.Minute), unknown))
	}
	open := false
	for _, ticket := range manager.tickets {
		open = open || ticket.open && ticket.class == "mimir-series-headroom"
	}
	if !open {
		t.Fatal("incomplete evidence resolved the headroom ticket")
	}
	for i := range manager.resolveTicks {
		manager.ingest(context.Background(), headroomCheck(t, probe, start.Add(time.Duration(i+10)*time.Minute), high))
	}
	for _, ticket := range manager.tickets {
		if ticket.open && ticket.class == "mimir-series-headroom" {
			t.Fatal("complete healthy evidence could not resolve the headroom ticket")
		}
	}
}

// A failed durable save cannot turn current high reserve into a recovery.
func TestMimirAdmissionHeadroomSaveFailureCannotClear(t *testing.T) {
	start := time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC)
	probe := &mimirAdmissionProbe{}
	low := headroomHostResponses(headroomChildFixture(19191, 1000, 950, 0), headroomChildFixture(19192, 1000, 100, 0))
	headroomCheck(t, probe, start, low)
	probe.saveProviderState = func(string, string, int, any) error { return fmt.Errorf("synthetic unavailable") }
	high := headroomHostResponses(headroomChildFixture(19191, 1000, 100, 0), headroomChildFixture(19192, 1000, 100, 0))
	requireHeadroomUnknown(t, headroomCheck(t, probe, start.Add(time.Minute), high))
}

// The public adapter must carry precise count-only risk rather than assert
// sample loss, a publisher cause, runtime enforcement, or rollout capacity.
func TestMimirAdmissionHeadroomMarkdownQualifiesRisk(t *testing.T) {
	responses := headroomHostResponses(headroomChildFixture(19191, 1000, 950, 0), headroomChildFixture(19192, 1000, 100, 0))
	alerts := runMimirAdmissionSynthetic(t, NewMimirAdmissionSignal(), time.Date(2034, 1, 2, 3, 4, 0, 0, time.UTC), responses)
	alert := requireAlertClass(t, alerts, "mimir-series-headroom")
	for _, phrase := range []string{"same child", "retained", "not proof of current sample loss", "rejected candidates", "not a rollout-capacity guarantee", "single-tenant", "10%", "Do not restart"} {
		if !strings.Contains(alert.Markdown(), phrase) {
			t.Errorf("headroom warning omitted qualifier %q", phrase)
		}
	}
	for _, private := range []string{"metrics-a.example", "metrics-b.example", "2000000000.25", strconv.Itoa(19191), strconv.Itoa(19192)} {
		if strings.Contains(alert.Markdown(), private) {
			t.Fatalf("per-child identity escaped aggregate warning: %q", private)
		}
	}
}

// The catalog is part of the acceptance contract, including the distinct
// alert identity and the conservative unknown/partial recovery boundary.
func TestMimirAdmissionHeadroomCatalogContract(t *testing.T) {
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(data)
	start := strings.Index(catalog, "### 11.20a Mimir series and sample-rate admission")
	end := strings.Index(catalog, "### 11.20b Mimir distributor ingestion balance")
	if start < 0 || end <= start {
		t.Fatal("missing exact admission catalog section")
	}
	section := strings.Join(strings.Fields(catalog[start:end]), " ")
	for _, phrase := range []string{"mimir-series-headroom", "retained-series-headroom", "10%", "same child", "independent of both discard quiet holds", "not proof of current sample loss", "complete comparable current", "single-tenant", "not a rollout-capacity guarantee"} {
		if !strings.Contains(section, phrase) {
			t.Errorf("headroom catalog omitted %q", phrase)
		}
	}
	if !strings.Contains(catalog, "| mimir-series-headroom | exact child Mimir metrics |") {
		t.Error("headroom warning is absent from the emission table")
	}
}
