package model

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The country site judgement (provider_egress_country_site.go), pure.

var countrySiteTestNow = time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC)

// Evidence of one network per difference: loads paired loads each, whose
// providers' general outcomes predict 60% successes, and the site's successes
// that much above or below; the recent days carry the same shares on a
// quarter of the loads.
func countrySiteTestEvidence(differences []float64, loads int, providers int) []ProviderEgressCountrySiteNetworkEvidence {
	evidence := []ProviderEgressCountrySiteNetworkEvidence{}
	for _, difference := range differences {
		recent := loads / 4
		evidence = append(evidence, ProviderEgressCountrySiteNetworkEvidence{
			NetworkId:         server.NewId(),
			PairedLoads:       loads,
			PairedOk:          int(math.Round((0.6 + difference) * float64(loads))),
			ExpectedOk:        0.6 * float64(loads),
			Providers:         providers,
			RecentPairedLoads: recent,
			RecentPairedOk:    int(math.Round((0.6 + difference) * float64(recent))),
			RecentExpectedOk:  0.6 * float64(recent),
		})
	}
	return evidence
}

// differences repeated, for count networks
func countrySiteTestDifferences(count int, differences ...float64) []float64 {
	out := []float64{}
	for i := range count {
		out = append(out, differences[i%len(differences)])
	}
	return out
}

func countrySiteTestState(scored bool, probationSince time.Time) *ProviderEgressCountrySite {
	return &ProviderEgressCountrySite{
		ProviderEgressCountrySiteKey: ProviderEgressCountrySiteKey{Name: "de-site", CountryCode: "de"},
		Url:                          "https://de-site.example/",
		Scored:                       scored,
		ProbationSince:               probationSince,
	}
}

// The t quantile the bounds use is the table's, from the fewest networks a
// judgement takes.
func TestProviderEgressStudentT95MatchesTheTables(t *testing.T) {
	for df, want := range map[int]float64{9: 1.833, 10: 1.812, 15: 1.753, 20: 1.725, 30: 1.697, 60: 1.671, 120: 1.658} {
		if got := ProviderEgressStudentT95(df); math.Abs(got-want) > 0.001 {
			t.Errorf("t(0.95, %d) = %.4f, want %.3f", df, got, want)
		}
	}
}

// A site as good as the general catalog for the same providers graduates once
// it has been on probation for the whole window, not before.
func TestCountrySiteAsGoodAsGeneralGraduatesAfterTheWindow(t *testing.T) {
	evidence := countrySiteTestEvidence(countrySiteTestDifferences(12, 0.01, -0.01, 0.02, -0.02, 0), 100, 3)
	early := JudgeProviderEgressCountrySite(countrySiteTestState(false, countrySiteTestNow.Add(-6*24*time.Hour)), evidence, nil, countrySiteTestNow)
	if early.Change != ProviderEgressCountrySiteUnchanged || early.State.Scored {
		t.Fatalf("graduated after six days on probation: %+v", early)
	}
	judgement := JudgeProviderEgressCountrySite(countrySiteTestState(false, countrySiteTestNow.Add(-ProviderEgressCountrySiteWindow)), evidence, nil, countrySiteTestNow)
	if judgement.Change != ProviderEgressCountrySiteGraduated || judgement.Reason != ProviderEgressCountrySiteReasonPaired || !judgement.State.Scored ||
		judgement.State.GraduatedTime == nil || judgement.State.NetworkCount != 12 || judgement.State.ProviderCount != 36 || judgement.State.PairedLoadCount != 1200 {
		t.Fatalf("an equal site did not graduate: %+v %s", judgement, judgement.State.Judgement)
	}
}

// A worse site never graduates: per network beyond the margin, or
// load-weighted, where the operators carrying most providers cannot use it.
func TestCountrySiteWorseThanGeneralNeverGraduates(t *testing.T) {
	probation := countrySiteTestState(false, countrySiteTestNow.Add(-30*24*time.Hour))
	worse := countrySiteTestEvidence(countrySiteTestDifferences(20, -0.05, -0.04, -0.06), 100, 2)
	if judgement := JudgeProviderEgressCountrySite(probation, worse, nil, countrySiteTestNow); judgement.State.Scored {
		t.Fatalf("a site five points worse per network graduated: %s", judgement.State.Judgement)
	}
	// thirty networks find it as good as general, which passes per network;
	// the biggest, with most of the loads, finds it ten points worse
	farm := countrySiteTestEvidence(countrySiteTestDifferences(30, 0), 100, 2)
	farm = append(farm, countrySiteTestEvidence([]float64{-0.1}, 10000, 300)...)
	summary := SummarizeProviderEgressCountrySite(farm)
	if summary.Lower < -ProviderEgressCountrySiteGraduateMargin || -ProviderEgressCountrySiteGraduateMargin <= summary.LoadDifference {
		t.Fatalf("the fixture does not isolate the load-weighted guard: %+v", summary)
	}
	judgement := JudgeProviderEgressCountrySite(probation, farm, nil, countrySiteTestNow)
	if judgement.State.Scored || !strings.Contains(judgement.State.Judgement, "worse") {
		t.Fatalf("a site the biggest operator cannot use graduated: %s", judgement.State.Judgement)
	}
}

// Too few samples do not graduate a site however good it looks: networks,
// the loads that let a network count, providers, each below its minimum.
func TestCountrySiteWithTooFewSamplesDoesNotGraduate(t *testing.T) {
	probation := countrySiteTestState(false, countrySiteTestNow.Add(-30*24*time.Hour))
	for _, c := range []struct {
		name     string
		evidence []ProviderEgressCountrySiteNetworkEvidence
		scored   bool
	}{
		{"every minimum met", countrySiteTestEvidence(countrySiteTestDifferences(10, 0), 50, 2), true},
		{"nine networks", countrySiteTestEvidence(countrySiteTestDifferences(9, 0), 100, 3), false},
		{"one network short of loads", append(countrySiteTestEvidence(countrySiteTestDifferences(9, 0), 100, 3), countrySiteTestEvidence([]float64{0}, 49, 3)...), false},
		{"nineteen providers", append(countrySiteTestEvidence(countrySiteTestDifferences(9, 0), 100, 2), countrySiteTestEvidence([]float64{0}, 100, 1)...), false},
		{"no evidence", nil, false},
	} {
		judgement := JudgeProviderEgressCountrySite(probation, c.evidence, nil, countrySiteTestNow)
		if judgement.State.Scored != c.scored {
			t.Errorf("%s: scored %t, want %t (%s)", c.name, judgement.State.Scored, c.scored, judgement.State.Judgement)
		}
	}
}

// A site with a TLS failure since its probation began that the judge has not
// called the provider's own does not graduate: pending, a site fault or a
// place block. One judged the provider's own, or one from before the
// probation, does not hold it.
func TestCountrySiteFailingTlsDoesNotGraduate(t *testing.T) {
	probationSince := countrySiteTestNow.Add(-10 * 24 * time.Hour)
	probation := countrySiteTestState(false, probationSince)
	evidence := countrySiteTestEvidence(countrySiteTestDifferences(12, 0), 100, 2)
	for _, c := range []struct {
		name    string
		finding ProviderEgressCountrySiteTlsFinding
		scored  bool
	}{
		{"pending", ProviderEgressCountrySiteTlsFinding{ProviderEgressTlsVerdictPending, countrySiteTestNow.Add(-time.Hour)}, false},
		{"site fault", ProviderEgressCountrySiteTlsFinding{ProviderEgressTlsVerdictSite, countrySiteTestNow.Add(-3 * 24 * time.Hour)}, false},
		{"place block", ProviderEgressCountrySiteTlsFinding{ProviderEgressTlsVerdictPlace, countrySiteTestNow.Add(-24 * time.Hour)}, false},
		{"the provider's own", ProviderEgressCountrySiteTlsFinding{ProviderEgressTlsVerdictProvider, countrySiteTestNow.Add(-time.Hour)}, true},
		{"before the probation", ProviderEgressCountrySiteTlsFinding{ProviderEgressTlsVerdictSite, probationSince.Add(-time.Hour)}, true},
	} {
		judgement := JudgeProviderEgressCountrySite(probation, evidence, []ProviderEgressCountrySiteTlsFinding{c.finding}, countrySiteTestNow)
		if judgement.State.Scored != c.scored {
			t.Errorf("%s: scored %t, want %t (%s)", c.name, judgement.State.Scored, c.scored, judgement.State.Judgement)
		}
	}
}

// A graduated site that collapses in the recent days is demoted at once:
// failing for most loads, or in most networks. One that only dips is not.
func TestGraduatedCountrySiteThatCollapsesIsDemoted(t *testing.T) {
	scored := countrySiteTestState(true, countrySiteTestNow.Add(-30*24*time.Hour))
	collapsed := func(evidence []ProviderEgressCountrySiteNetworkEvidence, recentShare float64) []ProviderEgressCountrySiteNetworkEvidence {
		for i := range evidence {
			evidence[i].RecentPairedOk = int(math.Round(recentShare * evidence[i].RecentExpectedOk))
		}
		return evidence
	}
	for _, c := range []struct {
		name     string
		evidence []ProviderEgressCountrySiteNetworkEvidence
		demoted  bool
	}{
		{"blocked: no recent success", collapsed(countrySiteTestEvidence(countrySiteTestDifferences(12, 0), 100, 2), 0), true},
		{"broken for most loads", collapsed(countrySiteTestEvidence(countrySiteTestDifferences(12, 0), 100, 2), 0.4), true},
		{"a dip", collapsed(countrySiteTestEvidence(countrySiteTestDifferences(12, 0), 100, 2), 0.7), false},
		// three small networks fail it while the biggest still loads it
		{"failing in most networks", append(
			collapsed(countrySiteTestEvidence(countrySiteTestDifferences(3, 0), 60, 1), 0),
			countrySiteTestEvidence([]float64{0}, 4000, 100)...), true},
	} {
		judgement := JudgeProviderEgressCountrySite(scored, c.evidence, nil, countrySiteTestNow)
		demoted := judgement.Change == ProviderEgressCountrySiteDemoted
		if demoted != c.demoted || judgement.State.Scored == c.demoted {
			t.Errorf("%s: demoted %t, want %t (%s)", c.name, demoted, c.demoted, judgement.State.Judgement)
			continue
		}
		if demoted && (judgement.Reason != ProviderEgressCountrySiteReasonCollapsed || judgement.State.DemoteCount != 1 ||
			judgement.State.DemotedTime == nil || !judgement.State.ProbationSince.Equal(countrySiteTestNow)) {
			t.Errorf("%s: demotion %+v", c.name, judgement.State)
		}
	}
}

// A graduated site with a recent TLS failure judged a site or place fault is
// demoted; one judged the provider's own is the provider's.
func TestGraduatedCountrySiteWithATlsSiteFaultIsDemoted(t *testing.T) {
	scored := countrySiteTestState(true, countrySiteTestNow.Add(-30*24*time.Hour))
	evidence := countrySiteTestEvidence(countrySiteTestDifferences(12, 0), 100, 2)
	for verdict, demoted := range map[ProviderEgressTlsVerdict]bool{
		ProviderEgressTlsVerdictSite:     true,
		ProviderEgressTlsVerdictPlace:    true,
		ProviderEgressTlsVerdictProvider: false,
		ProviderEgressTlsVerdictPending:  false,
	} {
		judgement := JudgeProviderEgressCountrySite(scored, evidence,
			[]ProviderEgressCountrySiteTlsFinding{{verdict, countrySiteTestNow.Add(-time.Hour)}}, countrySiteTestNow)
		if got := judgement.Change == ProviderEgressCountrySiteDemoted; got != demoted || (demoted && judgement.Reason != ProviderEgressCountrySiteReasonTls) {
			t.Errorf("%s: demoted %t (%s), want %t", verdict, got, judgement.Reason, demoted)
		}
	}
}

// No flapping at the boundary. Day by day, a site's per-network difference
// moves from as good as general, down past the graduation margin and back,
// then below the demotion margin, then up again:
//   - between the margins it keeps whatever state it has, scored or not;
//   - clearly worse, it is demoted once;
//   - back between the margins it stays on probation;
//   - as good as general again, it graduates only once a whole window has
//     passed since its demotion, however good the evidence before that.
func TestCountrySiteDoesNotFlapAtTheBoundary(t *testing.T) {
	state := countrySiteTestState(false, countrySiteTestNow.Add(-ProviderEgressCountrySiteWindow))
	changes := []string{}
	for day, difference := range []float64{
		0, -0.04, -0.05, -0.04, // graduates on day 0, then stays scored between the margins
		-0.09,                // clearly worse: demoted once, on day 4
		-0.05, -0.04, -0.045, // between the margins: stays on probation
		0, 0, 0, // as good as general again, but within a window of the demotion
		0, 0, // a whole window after the demotion: graduates on day 11, and stays
	} {
		now := countrySiteTestNow.Add(time.Duration(day) * 24 * time.Hour)
		evidence := countrySiteTestEvidence(countrySiteTestDifferences(16, difference+0.005, difference-0.005), 100, 2)
		judgement := JudgeProviderEgressCountrySite(state, evidence, nil, now)
		state = judgement.State
		if judgement.Change != ProviderEgressCountrySiteUnchanged {
			changes = append(changes, string(judgement.Change))
		}
		wantScored := day < 4 || 11 <= day
		if state.Scored != wantScored {
			t.Fatalf("day %d at %+.3f: scored %t, want %t (%s)", day, difference, state.Scored, wantScored, state.Judgement)
		}
	}
	if strings.Join(changes, ",") != "graduated,demoted,graduated" {
		t.Fatalf("changes %v, want one graduation, one demotion, one graduation", changes)
	}
	if state.DemoteCount != 1 || state.State() != "scored" {
		t.Fatalf("final state %+v", state)
	}
}
