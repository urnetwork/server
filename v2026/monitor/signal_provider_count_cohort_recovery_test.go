// A measured provider-count incident waits only on the evidence of the
// cohorts it was reported for. On Main every five-minute evaluation over a day
// (2026-10-30 to 10-31) had 106 to 286 ordinary cohorts below the 20-request
// floor, so a hold from any sparse cohort meant no effective-empty or
// small-list incident could ever clear. Every value below is synthetic.
package monitor

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The incident fixtures report any/location/us/quality; the siblings are other
// ordinary cohorts that never reported a measured class.
func providerCountCohortRecoveryPayload(t testing.TB, now time.Time, mode string) string {
	t.Helper()
	sibling := func(bands map[string]float64) string {
		return providerCountDirectControlFixture(t, now, "group", bands)
	}
	switch mode {
	case "healthy+idle":
		return providerCountJoinedControlFixtures(t, providerCountRecoveryPayload(t, now, "healthy"), sibling(map[string]float64{"0": 0}))
	case "sibling-only":
		return sibling(map[string]float64{"3-9": 100})
	case "own-sparse+sibling":
		return providerCountJoinedControlFixtures(t, providerCountRecoveryPayload(t, now, "sparse"), sibling(map[string]float64{"3-9": 100}))
	case "own-unknown+sibling":
		return providerCountJoinedControlFixtures(t, providerCountRecoveryPayload(t, now, "unknown"), sibling(map[string]float64{"3-9": 100}))
	case "own-idle+sibling":
		return providerCountJoinedControlFixtures(t, providerCountRecoveryPayload(t, now, "idle"), sibling(map[string]float64{"3-9": 100}))
	case "explained-page":
		return providerCountJoinedControlFixtures(t, providerCountRecoveryPayload(t, now, "page"), sibling(map[string]float64{"3-9": 100}))
	case "explained":
		return providerCountJoinedControlFixtures(t,
			providerCountExplainedPayload(t, now, "us", map[string]float64{"0": 20}, providerCountExplanation{"city", "cache_empty", 20}),
			sibling(map[string]float64{"3-9": 100}))
	case "explained-sibling-only":
		return providerCountExplainedPayload(t, now, "au", map[string]float64{"0": 20, "10+": 1}, providerCountExplanation{"city", "cache_empty", 20})
	default:
		return providerCountRecoveryPayload(t, now, mode)
	}
}

// Runs the modes through the real cadence gate and notifier. Returns the
// incident's absence credit after each run but the last (-1 once the incident
// is gone) and whether it ended cleared by exactly one post.
func providerCountRecoverySequence(t *testing.T, class string, modes []string) ([]int, bool) {
	t.Helper()
	now := time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC)
	var run atomic.Int32
	settings := providerCountSyntheticSettings(t, now, providerCountRecoveryPayload(t, now, "healthy"))
	settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return providerCountCohortRecoveryPayload(t, now, modes[int(run.Load())]), nil
	}}
	webhook, url := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(url))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	signal := NewProviderCountSignal()
	key := slackIncidentKey(Alert{SignalID: signal.ID(), Class: class, Target: "api-fleet"})
	credits := []int{}
	postSignalRunsThroughTheGate(t, signal, settings, len(modes), func(next int) {
		if next > 0 {
			notifier.stateLock.Lock()
			credit := -1
			if incident, present := notifier.keyIncidents[key]; present {
				if owned, ok := incident.signalKeyAbsentRuns[signal.Key()]; ok {
					credit = owned
				}
			}
			notifier.stateLock.Unlock()
			credits = append(credits, credit)
			harness.advance(time.Minute)
		}
		run.Store(int32(next))
	}, notifier)
	closeSlackNotifier(t, notifier)
	notifier.stateLock.Lock()
	_, present := notifier.keyIncidents[key]
	notifier.stateLock.Unlock()
	return credits, !present && countSlackPosts(webhook.posted(), "[CLEARED]", class+" · api-fleet") == 1
}

func providerCountInitialMode(class string) string {
	if class == "provider-count-small-list" {
		return "warn"
	}
	return "page"
}

// A sibling below the floor, in the intent-unknown shape or idle, never
// reported the class, so it cannot keep the incident open once its own cohort
// shows two qualified runs.
func TestProviderCountIncidentClearsOnItsOwnCohortDespiteSparseSiblings(t *testing.T) {
	for _, class := range []string{"provider-count-effective-empty", "provider-count-small-list"} {
		for _, sibling := range []string{"healthy+sparse", "healthy+unknown", "healthy+idle"} {
			t.Run(class+"/"+sibling, func(t *testing.T) {
				credits, cleared := providerCountRecoverySequence(t, class, []string{providerCountInitialMode(class), sibling, sibling})
				if len(credits) != 2 || credits[0] != 0 || credits[1] != 1 || !cleared {
					t.Fatalf("credits=%v cleared=%t, want [0 1] and one clear after two qualified runs", credits, cleared)
				}
			})
		}
	}
}

// The incident's own cohort below the floor, in the intent-unknown shape or
// idle is not recovery, whatever its siblings show; two qualified runs of its
// own still clear it.
func TestProviderCountIncidentHoldsWhileItsOwnCohortLacksEvidence(t *testing.T) {
	for _, class := range []string{"provider-count-effective-empty", "provider-count-small-list"} {
		t.Run(class, func(t *testing.T) {
			modes := []string{providerCountInitialMode(class), "own-sparse+sibling", "own-unknown+sibling", "own-idle+sibling", "healthy", "healthy"}
			credits, cleared := providerCountRecoverySequence(t, class, modes)
			want := []int{0, 0, 0, 0, 1}
			if len(credits) != len(want) || !cleared {
				t.Fatalf("credits=%v cleared=%t, want %v and one clear", credits, cleared, want)
			}
			for index := range want {
				if credits[index] != want[index] {
					t.Fatalf("credits=%v, want %v", credits, want)
				}
			}
		})
	}
}

// A cohort missing from a successful response has no evidence: a healthy or
// explained sibling cannot certify it, and its qualified streak restarts.
func TestProviderCountIncidentHoldsWhileItsOwnCohortIsMissing(t *testing.T) {
	for _, class := range []string{"provider-count-effective-empty", "provider-count-small-list"} {
		for _, sibling := range []string{"sibling-only", "explained-sibling-only"} {
			t.Run(class+"/"+sibling, func(t *testing.T) {
				modes := []string{providerCountInitialMode(class), "healthy", sibling, sibling, "healthy", "healthy"}
				credits, cleared := providerCountRecoverySequence(t, class, modes)
				want := []int{0, 1, 0, 0, 1}
				if len(credits) != len(want) || !cleared {
					t.Fatalf("credits=%v cleared=%t, want %v and one clear", credits, cleared, want)
				}
				for index := range want {
					if credits[index] != want[index] {
						t.Fatalf("credits=%v, want %v", credits, want)
					}
				}
			})
		}
	}
}

// Twenty requests whose zeros the request or its narrow target explains are
// the cohort's own sufficient evidence that it no longer meets the PAGE.
func TestProviderCountExplainedCohortRecoversItsOwnIncident(t *testing.T) {
	credits, cleared := providerCountRecoverySequence(t, "provider-count-effective-empty", []string{"explained-page", "explained", "explained"})
	if len(credits) != 2 || credits[0] != 0 || credits[1] != 1 || !cleared {
		t.Fatalf("credits=%v cleared=%t, want [0 1] and one clear", credits, cleared)
	}
}

// The probe's memory, read through its holds: a recovered cohort is forgotten,
// so it cannot hold a later incident another cohort reports, and a run that
// never evaluated forgets nothing.
func TestProviderCountProbeHoldsOnlyForAwaitingCohorts(t *testing.T) {
	now := time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC)
	probe := NewProviderCountSignal().(*signalAdapter).probe.(*providerCountProbe)
	check := func(ctx context.Context, payload string) []finding {
		t.Helper()
		env, err := newProbeEnv(providerCountSyntheticSettings(t, now, payload).withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		findings, err := probe.check(ctx, env)
		if err != nil && ctx.Err() == nil {
			t.Fatal(err)
		}
		return findings
	}
	holds := func(findings []finding) map[string]bool {
		held := map[string]bool{}
		for _, f := range findings {
			if f.clearHold && f.probeId == "mimir/provider-count" && f.target == "api-fleet" {
				held[f.class] = true
			}
		}
		return held
	}
	// another cohort reports the class while the first is still sparse
	otherPage := func(own string) string {
		return providerCountJoinedControlFixtures(t, providerCountRecoveryPayload(t, now, own),
			providerCountDirectControlFixture(t, now, "group", map[string]float64{"0": 20}))
	}
	otherHealthy := func(own string) string {
		return providerCountJoinedControlFixtures(t, providerCountRecoveryPayload(t, now, own),
			providerCountDirectControlFixture(t, now, "group", map[string]float64{"3-9": 100}))
	}
	check(context.Background(), providerCountRecoveryPayload(t, now, "page"))
	if held := holds(check(context.Background(), otherHealthy("sparse"))); !held["provider-count-effective-empty"] || held["provider-count-small-list"] {
		t.Fatalf("the reporting cohort below the floor held %v, want only its own class", held)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	check(canceled, providerCountRecoveryPayload(t, now, "healthy"))
	for run := 0; run < 2; run++ {
		if held := holds(check(context.Background(), providerCountRecoveryPayload(t, now, "healthy"))); len(held) != 0 {
			t.Fatalf("qualified run %d held %v", run+1, held)
		}
	}
	if held := holds(check(context.Background(), otherPage("sparse"))); len(held) != 0 {
		t.Fatalf("a forgotten cohort below the floor held %v beside another cohort's report", held)
	}
	if held := holds(check(context.Background(), otherHealthy("sparse"))); len(held) != 0 {
		t.Fatalf("a forgotten cohort below the floor held another cohort's recovery: %v", held)
	}
}

func TestMonitorDocumentationProviderCountCohortRecovery(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(data), "### 2.9a ")
	if !found {
		t.Fatal("SIGNALS.md lost the provider-count section")
	}
	section, _, found = strings.Cut(section, "\n### 2.9b ")
	if !found {
		t.Fatal("SIGNALS.md lost the provider-count section boundary")
	}
	documentation := strings.Join(strings.Fields(section), " ")
	for _, required := range []string{
		"a measured incident waits only on the evidence of the cohorts it was reported for",
		"two consecutive executions in which it returns at least the 20-request floor",
		"below the floor, in the intent-unknown shape, idle, or missing from the response",
		"a healthy sibling cannot certify it",
		"A cohort that never reported a class holds nothing",
		"a response with no ordinary cohort at the floor in a judgeable shape holds both classes",
		"Silence is not recovery",
		"never vouch for another cohort",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("SIGNALS.md lost the cohort recovery contract %q", required)
		}
	}
}
