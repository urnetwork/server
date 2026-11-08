package model

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// a Friday, and the injected clock of every ladder test
var abuseTestNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

const abuseTestDay = 24 * time.Hour

func abuseTestSettings() *AbusePolicySettings {
	settings := DefaultAbusePolicySettings()
	settings.RootNames = []string{"root.example"}
	settings.PlatformNames = []string{"platform.example", "ns1.root.example"}
	return settings
}

func abuseTestHost() AbuseTarget {
	return AbuseTarget{Kind: AbuseTargetKindHost, Name: "shop.net.root.example"}
}

func abuseTestSendingDomain() AbuseTarget {
	return AbuseTarget{Kind: AbuseTargetKindSendingDomain, Name: "net.root.example"}
}

// An action on another host of the network, in a case of its own.
func abuseTestAction(category AbuseCategory, kind AbuseActionKind, age time.Duration) AbuseHistoryAction {
	return AbuseHistoryAction{
		CaseId:      server.NewId(),
		Category:    category,
		Kind:        kind,
		Target:      "other.net.root.example",
		ReviewState: AbuseReviewStatePending,
		CreateTime:  abuseTestNow.Add(-age),
	}
}

func abuseTestActionInState(
	category AbuseCategory,
	kind AbuseActionKind,
	age time.Duration,
	reviewState AbuseReviewState,
) AbuseHistoryAction {
	action := abuseTestAction(category, kind, age)
	action.ReviewState = reviewState
	return action
}

func abuseTestStep(
	t *testing.T,
	category AbuseCategory,
	target AbuseTarget,
	history []AbuseHistoryAction,
) AbuseStep {
	t.Helper()
	step, err := NextAbuseStep(category, target, history, abuseTestNow, abuseTestSettings())
	if err != nil {
		t.Fatalf("%s: %v", category, err)
	}
	return step
}

func abuseTestStepEqual(a AbuseStep, b AbuseStep) bool {
	return slices.Equal(a.ActionKinds, b.ActionKinds) && a.Protective == b.Protective && a.Report == b.Report
}

func abuseTestKinds(kinds ...AbuseActionKind) AbuseStep {
	return AbuseStep{ActionKinds: kinds}
}

func TestAbuseMachineActionTakesEveryRowOfTheSignalTable(t *testing.T) {
	for _, c := range []struct {
		signal AbuseSignal
		kind   AbuseActionKind
	}{
		// §14.8 high or medium join
		{signal: AbuseSignalMailJoinHigh, kind: AbuseActionKindMailLadder},
		{signal: AbuseSignalMailJoinMedium, kind: AbuseActionKindMailLadder},
		// Web Risk or feed hit; trusted-flagger report naming a url
		{signal: AbuseSignalWebRiskHit, kind: AbuseActionKindHostKill},
		{signal: AbuseSignalFeedHit, kind: AbuseActionKindHostKill},
		{signal: AbuseSignalTrustedFlaggerUrl, kind: AbuseActionKindHostKill},
		// notices: a kill only for a registrar's or registry's url
		{signal: AbuseSignalRegistrarUrl, kind: AbuseActionKindHostKill},
		{signal: AbuseSignalRegistryUrl, kind: AbuseActionKindHostKill},
		{signal: AbuseSignalRegistrarNotice, kind: AbuseActionKindNone},
		{signal: AbuseSignalRegistryNotice, kind: AbuseActionKindNone},
		{signal: AbuseSignalLawEnforcementEmergency, kind: AbuseActionKindNone},
		{signal: AbuseSignalEuRemovalOrder, kind: AbuseActionKindNone},
		{signal: AbuseSignalProductionOrder, kind: AbuseActionKindNone},
		// a report of phishing, malware or command-and-control naming a url
		{signal: AbuseSignalThreatReport, kind: AbuseActionKindNone},
		// other
		{signal: AbuseSignalReport, kind: AbuseActionKindNone},
		{signal: AbuseSignalMailJoinLow, kind: AbuseActionKindNone},
		// a signal this release does not know
		{signal: AbuseSignal("unlisted"), kind: AbuseActionKindNone},
		{signal: AbuseSignal(""), kind: AbuseActionKindNone},
	} {
		if kind := AbuseMachineAction(c.signal); kind != c.kind {
			t.Errorf("%q: got %q, want %q", c.signal, kind, c.kind)
		}
	}
}

func TestAbuseSignalOfReportNamesItsRow(t *testing.T) {
	for _, c := range []struct {
		class    AbuseReporterClass
		category AbuseCategory
		namesUrl bool
		signal   AbuseSignal
	}{
		{class: AbuseReporterClassWebRisk, category: AbuseCategoryPhishing, namesUrl: true, signal: AbuseSignalWebRiskHit},
		{class: AbuseReporterClassFeed, category: AbuseCategoryMalware, namesUrl: false, signal: AbuseSignalFeedHit},
		{class: AbuseReporterClassTrustedFlagger, category: AbuseCategoryPhishing, namesUrl: true, signal: AbuseSignalTrustedFlaggerUrl},
		{class: AbuseReporterClassTrustedFlagger, category: AbuseCategoryCopyright, namesUrl: true, signal: AbuseSignalTrustedFlaggerUrl},
		// a flagger's report of anything else is a report
		{class: AbuseReporterClassTrustedFlagger, category: AbuseCategoryPhishing, namesUrl: false, signal: AbuseSignalReport},
		{class: AbuseReporterClassPublic, category: AbuseCategoryPhishing, namesUrl: true, signal: AbuseSignalThreatReport},
		{class: AbuseReporterClassPublic, category: AbuseCategoryMalware, namesUrl: true, signal: AbuseSignalThreatReport},
		{class: AbuseReporterClassPublic, category: AbuseCategoryCommandAndControl, namesUrl: true, signal: AbuseSignalThreatReport},
		{class: AbuseReporterClassPublic, category: AbuseCategoryPhishing, namesUrl: false, signal: AbuseSignalReport},
		{class: AbuseReporterClassPublic, category: AbuseCategorySpam, namesUrl: true, signal: AbuseSignalReport},
		// a ct hit is a report, not a signal for a kill (§21.6)
		{class: AbuseReporterClassCtWatch, category: AbuseCategoryBrand, namesUrl: true, signal: AbuseSignalReport},
		// a search console flag waits for business hours (§21.17)
		{class: AbuseReporterClassSearchConsole, category: AbuseCategoryPhishing, namesUrl: true, signal: AbuseSignalThreatReport},
	} {
		signal := AbuseSignalOfReport(c.class, c.category, c.namesUrl)
		if signal != c.signal {
			t.Errorf("%q %q url=%t: got %q, want %q", c.class, c.category, c.namesUrl, signal, c.signal)
		}
	}
}

// Machines act only on §21.8's signals, so no report reaches a kill or a page by
// another path, a class claiming to be a registrar included.
func TestAbuseReportNeverReachesAKillOrAPage(t *testing.T) {
	reportClasses := []AbuseReporterClass{
		AbuseReporterClassPublic,
		AbuseReporterClassRoleMail,
		AbuseReporterClassMailReport,
		AbuseReporterClassCtWatch,
		AbuseReporterClassSearchConsole,
		AbuseReporterClassRegistrar,
		AbuseReporterClassRegistry,
		AbuseReporterClassLawEnforcement,
		AbuseReporterClassStaff,
		AbuseReporterClass("unknown"),
	}
	categories := []AbuseCategory{
		AbuseCategorySpam,
		AbuseCategoryPhishing,
		AbuseCategoryMalware,
		AbuseCategoryCommandAndControl,
		AbuseCategoryCopyright,
		AbuseCategoryExposedPanel,
		AbuseCategoryCompromise,
		AbuseCategoryCsam,
		AbuseCategoryThreatToLife,
		AbuseCategoryBrand,
		AbuseCategoryOther,
		AbuseCategory("unknown"),
	}
	for _, class := range reportClasses {
		for _, category := range categories {
			for _, namesUrl := range []bool{false, true} {
				signal := AbuseSignalOfReport(class, category, namesUrl)
				if signal != AbuseSignalReport && signal != AbuseSignalThreatReport {
					t.Errorf("%q %q url=%t: landed on %q", class, category, namesUrl, signal)
				}
				if kind := AbuseMachineAction(signal); kind != AbuseActionKindNone {
					t.Errorf("%q %q url=%t: machine action %q", class, category, namesUrl, kind)
				}
			}
		}
	}
}

func TestNextAbuseStepTakesEveryRowOfTheCategoryTable(t *testing.T) {
	strike := func(category AbuseCategory, count int) []AbuseHistoryAction {
		history := []AbuseHistoryAction{}
		for range count {
			history = append(history, abuseTestAction(category, AbuseActionKindHostKill, abuseTestDay))
		}
		return history
	}
	halts := func(count int) []AbuseHistoryAction {
		history := []AbuseHistoryAction{}
		for range count {
			history = append(history, abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, abuseTestDay))
		}
		return history
	}
	upheld := func(count int) []AbuseHistoryAction {
		history := []AbuseHistoryAction{}
		for range count {
			history = append(history, abuseTestActionInState(
				AbuseCategoryCopyright, AbuseActionKindNoticeForward, 60*abuseTestDay, AbuseReviewStateUpheld,
			))
		}
		return history
	}
	cases := []struct {
		category AbuseCategory
		target   AbuseTarget
		history  []AbuseHistoryAction
		step     AbuseStep
	}{
		// spam: §14.8's ladder; 3 halts in 30 d: suspend
		{category: AbuseCategorySpam, target: abuseTestSendingDomain(), step: abuseTestKinds(AbuseActionKindMailLadder)},
		{category: AbuseCategorySpam, target: abuseTestSendingDomain(), history: halts(2), step: abuseTestKinds(AbuseActionKindMailLadder)},
		{category: AbuseCategorySpam, target: abuseTestSendingDomain(), history: halts(3), step: abuseTestKinds(AbuseActionKindNetworkSuspend)},
		// copyright: notice forwarded; a 2nd valid one in 30 d: host kill; 3 upheld in 12 months: suspend
		{category: AbuseCategoryCopyright, target: abuseTestHost(), step: abuseTestKinds(AbuseActionKindNoticeForward)},
		{
			category: AbuseCategoryCopyright,
			target:   abuseTestHost(),
			history:  []AbuseHistoryAction{abuseTestAction(AbuseCategoryCopyright, AbuseActionKindNoticeForward, abuseTestDay)},
			step:     abuseTestKinds(AbuseActionKindHostKill),
		},
		{category: AbuseCategoryCopyright, target: abuseTestHost(), history: upheld(3), step: abuseTestKinds(AbuseActionKindNetworkSuspend)},
		// exposed panel: a protective host kill, no strike
		{
			category: AbuseCategoryExposedPanel,
			target:   abuseTestHost(),
			step:     AbuseStep{ActionKinds: []AbuseActionKind{AbuseActionKindHostKill}, Protective: true},
		},
		// compromise: a halt or kill, no strike
		{
			category: AbuseCategoryCompromise,
			target:   abuseTestHost(),
			step:     AbuseStep{ActionKinds: []AbuseActionKind{AbuseActionKindHostKill}, Protective: true},
		},
		{
			category: AbuseCategoryCompromise,
			target:   abuseTestSendingDomain(),
			step:     AbuseStep{ActionKinds: []AbuseActionKind{AbuseActionKindMailHalt}, Protective: true},
		},
		// csam and threat to life: suspend, report
		{
			category: AbuseCategoryCsam,
			target:   abuseTestHost(),
			step:     AbuseStep{ActionKinds: []AbuseActionKind{AbuseActionKindNetworkSuspend}, Report: true},
		},
		{
			category: AbuseCategoryThreatToLife,
			target:   AbuseTarget{Kind: AbuseTargetKindNetwork, Name: "net"},
			step:     AbuseStep{ActionKinds: []AbuseActionKind{AbuseActionKindNetworkSuspend}, Report: true},
		},
		// no ladder
		{category: AbuseCategoryBrand, target: abuseTestHost(), step: AbuseStep{}},
		{category: AbuseCategoryOther, target: abuseTestHost(), step: AbuseStep{}},
	}
	// phishing, malware, command-and-control: host kill; by mail, a network halt;
	// 2nd in 30 d: label kill, mail halt; 3rd: suspend
	for _, category := range []AbuseCategory{
		AbuseCategoryPhishing,
		AbuseCategoryMalware,
		AbuseCategoryCommandAndControl,
	} {
		cases = append(cases, []struct {
			category AbuseCategory
			target   AbuseTarget
			history  []AbuseHistoryAction
			step     AbuseStep
		}{
			{category: category, target: abuseTestHost(), step: abuseTestKinds(AbuseActionKindHostKill)},
			{
				category: category,
				target:   AbuseTarget{Kind: AbuseTargetKindLabel, Name: "net.root.example"},
				step:     abuseTestKinds(AbuseActionKindHostKill),
			},
			{category: category, target: abuseTestSendingDomain(), step: abuseTestKinds(AbuseActionKindMailHalt)},
			{
				category: category,
				target:   abuseTestHost(),
				history:  strike(category, 1),
				step:     abuseTestKinds(AbuseActionKindLabelKill, AbuseActionKindMailHalt),
			},
			{category: category, target: abuseTestHost(), history: strike(category, 2), step: abuseTestKinds(AbuseActionKindNetworkSuspend)},
		}...)
	}
	for _, c := range cases {
		step := abuseTestStep(t, c.category, c.target, c.history)
		if !abuseTestStepEqual(step, c.step) {
			t.Errorf("%q on %q with %d actions: got %+v, want %+v", c.category, c.target.Kind, len(c.history), step, c.step)
		}
	}
}

func TestNextAbuseStepCountsTheThreatCategoriesAsOneLadder(t *testing.T) {
	history := []AbuseHistoryAction{abuseTestAction(AbuseCategoryPhishing, AbuseActionKindHostKill, abuseTestDay)}
	step := abuseTestStep(t, AbuseCategoryMalware, abuseTestHost(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindLabelKill, AbuseActionKindMailHalt)) {
		t.Fatalf("a malware case after a phishing strike: %+v", step)
	}
	history = append(history, abuseTestAction(AbuseCategoryMalware, AbuseActionKindLabelKill, abuseTestDay))
	step = abuseTestStep(t, AbuseCategoryCommandAndControl, abuseTestHost(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNetworkSuspend)) {
		t.Fatalf("a third threat case: %+v", step)
	}
}

// A kill lifted as wrong leaves the count, and one lifted because the network fixed
// the host stays in it.
func TestNextAbuseStepLiftedKillIsNotCountedTowardTheSecond(t *testing.T) {
	wrong := []AbuseHistoryAction{abuseTestActionInState(
		AbuseCategoryPhishing, AbuseActionKindHostKill, abuseTestDay, AbuseReviewStateLiftedWrong,
	)}
	step := abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), wrong)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindHostKill)) {
		t.Fatalf("after a kill lifted as wrong: %+v", step)
	}

	fixed := []AbuseHistoryAction{abuseTestActionInState(
		AbuseCategoryPhishing, AbuseActionKindHostKill, abuseTestDay, AbuseReviewStateLifted,
	)}
	step = abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), fixed)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindLabelKill, AbuseActionKindMailHalt)) {
		t.Fatalf("after a kill lifted as fixed: %+v", step)
	}
}

// A lift's own row is no action to count, whatever its state.
func TestNextAbuseStepCountsNoLiftRow(t *testing.T) {
	history := []AbuseHistoryAction{
		abuseTestAction(AbuseCategoryPhishing, AbuseActionKindLift, abuseTestDay),
		abuseTestAction(AbuseCategoryCopyright, AbuseActionKindLift, abuseTestDay),
		abuseTestAction(AbuseCategorySpam, AbuseActionKindLift, abuseTestDay),
	}
	step := abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindHostKill)) {
		t.Fatalf("phishing: %+v", step)
	}
	step = abuseTestStep(t, AbuseCategoryCopyright, abuseTestHost(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNoticeForward)) {
		t.Fatalf("copyright: %+v", step)
	}
}

// The 2nd rung takes two rows, and a strike is one case however many rows it took, so
// a network with one such case is at its 2nd strike, not its 3rd.
func TestNextAbuseStepCountsOneStrikePerCase(t *testing.T) {
	label := abuseTestAction(AbuseCategoryPhishing, AbuseActionKindLabelKill, abuseTestDay)
	halt := abuseTestAction(AbuseCategoryPhishing, AbuseActionKindMailHalt, abuseTestDay)
	halt.CaseId = label.CaseId
	step := abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), []AbuseHistoryAction{label, halt})
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindLabelKill, AbuseActionKindMailHalt)) {
		t.Fatalf("after one case of two rows: %+v", step)
	}

	first := abuseTestAction(AbuseCategoryPhishing, AbuseActionKindHostKill, 2*abuseTestDay)
	step = abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), []AbuseHistoryAction{first, label, halt})
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNetworkSuspend)) {
		t.Fatalf("after two cases: %+v", step)
	}
}

func TestNextAbuseStepCountsOnlyStrikingRows(t *testing.T) {
	for _, action := range []AbuseHistoryAction{
		// a revocation follows a kill and is no rung of its own
		abuseTestAction(AbuseCategoryPhishing, AbuseActionKindCertRevoke, abuseTestDay),
		abuseTestAction(AbuseCategoryPhishing, AbuseActionKindMailLabelHalt, abuseTestDay),
		abuseTestAction(AbuseCategoryPhishing, AbuseActionKindNoticeForward, abuseTestDay),
		abuseTestAction(AbuseCategoryCopyright, AbuseActionKindHostKill, abuseTestDay),
		abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, abuseTestDay),
		abuseTestAction(AbuseCategoryExposedPanel, AbuseActionKindHostKill, abuseTestDay),
		abuseTestAction(AbuseCategoryCompromise, AbuseActionKindHostKill, abuseTestDay),
	} {
		step := abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), []AbuseHistoryAction{action})
		if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindHostKill)) {
			t.Errorf("%q %q counted as a strike: %+v", action.Category, action.Kind, step)
		}
	}
	for _, kind := range []AbuseActionKind{
		AbuseActionKindHostKill,
		AbuseActionKindHostSuspend,
		AbuseActionKindLabelKill,
		AbuseActionKindZoneSuspend,
		AbuseActionKindMailHalt,
	} {
		action := abuseTestAction(AbuseCategoryPhishing, kind, abuseTestDay)
		step := abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), []AbuseHistoryAction{action})
		if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindLabelKill, AbuseActionKindMailHalt)) {
			t.Errorf("%q not counted as a strike: %+v", kind, step)
		}
	}
}

// Each window counts an action exactly its length old and none older.
func TestNextAbuseStepWindowsEndAtTheirLength(t *testing.T) {
	window := 30 * abuseTestDay
	for _, c := range []struct {
		age  time.Duration
		step AbuseStep
	}{
		{age: window, step: abuseTestKinds(AbuseActionKindLabelKill, AbuseActionKindMailHalt)},
		{age: window + time.Second, step: abuseTestKinds(AbuseActionKindHostKill)},
	} {
		history := []AbuseHistoryAction{abuseTestAction(AbuseCategoryPhishing, AbuseActionKindHostKill, c.age)}
		step := abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), history)
		if !abuseTestStepEqual(step, c.step) {
			t.Errorf("a strike %s old: got %+v, want %+v", c.age, step, c.step)
		}
	}

	for _, c := range []struct {
		age  time.Duration
		step AbuseStep
	}{
		{age: window, step: abuseTestKinds(AbuseActionKindNetworkSuspend)},
		{age: window + time.Second, step: abuseTestKinds(AbuseActionKindMailLadder)},
	} {
		history := []AbuseHistoryAction{
			abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, abuseTestDay),
			abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, abuseTestDay),
			abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, c.age),
		}
		step := abuseTestStep(t, AbuseCategorySpam, abuseTestSendingDomain(), history)
		if !abuseTestStepEqual(step, c.step) {
			t.Errorf("a third halt %s old: got %+v, want %+v", c.age, step, c.step)
		}
	}

	for _, c := range []struct {
		age  time.Duration
		step AbuseStep
	}{
		{age: window, step: abuseTestKinds(AbuseActionKindHostKill)},
		{age: window + time.Second, step: abuseTestKinds(AbuseActionKindNoticeForward)},
	} {
		history := []AbuseHistoryAction{abuseTestAction(AbuseCategoryCopyright, AbuseActionKindNoticeForward, c.age)}
		step := abuseTestStep(t, AbuseCategoryCopyright, abuseTestHost(), history)
		if !abuseTestStepEqual(step, c.step) {
			t.Errorf("a notice %s old: got %+v, want %+v", c.age, step, c.step)
		}
	}
}

// A halt lifted as wrong leaves the count, and a burner label's halt and a compromise's
// protective one never entered it. Halts of any other category count, as §14.8's rule
// names none.
func TestNextAbuseStepCountsNetworkHalts(t *testing.T) {
	halt := func(category AbuseCategory) AbuseHistoryAction {
		return abuseTestAction(category, AbuseActionKindMailHalt, abuseTestDay)
	}
	for _, c := range []struct {
		name    string
		history []AbuseHistoryAction
		step    AbuseStep
	}{
		{
			name: "one lifted as wrong",
			history: []AbuseHistoryAction{
				halt(AbuseCategorySpam),
				halt(AbuseCategorySpam),
				abuseTestActionInState(AbuseCategorySpam, AbuseActionKindMailHalt, abuseTestDay, AbuseReviewStateLiftedWrong),
			},
			step: abuseTestKinds(AbuseActionKindMailLadder),
		},
		{
			name: "one a burner label's",
			history: []AbuseHistoryAction{
				halt(AbuseCategorySpam),
				halt(AbuseCategorySpam),
				abuseTestAction(AbuseCategorySpam, AbuseActionKindMailLabelHalt, abuseTestDay),
			},
			step: abuseTestKinds(AbuseActionKindMailLadder),
		},
		{
			name: "one a compromise's",
			history: []AbuseHistoryAction{
				halt(AbuseCategorySpam),
				halt(AbuseCategorySpam),
				halt(AbuseCategoryCompromise),
			},
			step: abuseTestKinds(AbuseActionKindMailLadder),
		},
		{
			name:    "one verified phishing",
			history: []AbuseHistoryAction{halt(AbuseCategorySpam), halt(AbuseCategorySpam), halt(AbuseCategoryPhishing)},
			step:    abuseTestKinds(AbuseActionKindNetworkSuspend),
		},
	} {
		step := abuseTestStep(t, AbuseCategorySpam, abuseTestSendingDomain(), c.history)
		if !abuseTestStepEqual(step, c.step) {
			t.Errorf("%s: got %+v, want %+v", c.name, step, c.step)
		}
	}
}

// One open case can take several halts, and each is a halt.
func TestNextAbuseStepCountsEveryHaltOfOneCase(t *testing.T) {
	history := []AbuseHistoryAction{
		abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, 3*abuseTestDay),
		abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, 2*abuseTestDay),
		abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, abuseTestDay),
	}
	history[1].CaseId = history[0].CaseId
	history[2].CaseId = history[0].CaseId
	step := abuseTestStep(t, AbuseCategorySpam, abuseTestSendingDomain(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNetworkSuspend)) {
		t.Fatalf("three halts of one case: %+v", step)
	}
}

// 3 halts in 30 days suspend once. A suspension that stands is not taken again, and
// review ends one for the halts that led to it, so a complaint with no new halt does
// not undo a person's lift. The old halts still count, so one new halt suspends again.
func TestNextAbuseStepSuspendsOnce(t *testing.T) {
	halts := []AbuseHistoryAction{
		abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, abuseTestDay),
		abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, abuseTestDay),
		abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, abuseTestDay),
	}
	standing := append(slices.Clone(halts), abuseTestAction(AbuseCategorySpam, AbuseActionKindNetworkSuspend, time.Hour))
	step := abuseTestStep(t, AbuseCategorySpam, abuseTestSendingDomain(), standing)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindMailLadder)) {
		t.Fatalf("under a standing suspension: %+v", step)
	}
	standing = append(standing, abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, time.Minute))
	step = abuseTestStep(t, AbuseCategorySpam, abuseTestSendingDomain(), standing)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindMailLadder)) {
		t.Fatalf("with a new halt under a standing suspension: %+v", step)
	}

	for _, reviewState := range []AbuseReviewState{AbuseReviewStateLifted, AbuseReviewStateLiftedWrong} {
		lifted := append(slices.Clone(halts), abuseTestActionInState(
			AbuseCategorySpam, AbuseActionKindNetworkSuspend, time.Hour, reviewState,
		))
		step = abuseTestStep(t, AbuseCategorySpam, abuseTestSendingDomain(), lifted)
		if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindMailLadder)) {
			t.Errorf("%q with no new halt: %+v", reviewState, step)
		}

		lifted = append(lifted, abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, time.Minute))
		step = abuseTestStep(t, AbuseCategorySpam, abuseTestSendingDomain(), lifted)
		if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNetworkSuspend)) {
			t.Errorf("%q with a new halt: %+v", reviewState, step)
		}
	}
}

// Only the latest suspension answers: a halt between two lifted ones was answered by
// the second.
func TestNextAbuseStepSecondSuspensionAnswersTheHaltsBeforeIt(t *testing.T) {
	history := []AbuseHistoryAction{
		abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, 10*abuseTestDay),
		abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, 10*abuseTestDay),
		abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, 10*abuseTestDay),
		abuseTestActionInState(AbuseCategorySpam, AbuseActionKindNetworkSuspend, 9*abuseTestDay, AbuseReviewStateLifted),
		abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, 8*abuseTestDay),
		abuseTestActionInState(AbuseCategorySpam, AbuseActionKindNetworkSuspend, 7*abuseTestDay, AbuseReviewStateLifted),
	}
	step := abuseTestStep(t, AbuseCategorySpam, abuseTestSendingDomain(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindMailLadder)) {
		t.Fatalf("with no halt after the second suspension: %+v", step)
	}
	// and history comes in any order
	slices.Reverse(history)
	step = abuseTestStep(t, AbuseCategorySpam, abuseTestSendingDomain(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindMailLadder)) {
		t.Fatalf("in reverse order: %+v", step)
	}
}

// A suspension that ended by its own until answered the same halts as a lifted one.
func TestNextAbuseStepSuspensionThatEndedDoesNotReturn(t *testing.T) {
	ended := abuseTestAction(AbuseCategorySpam, AbuseActionKindNetworkSuspend, time.Hour)
	ended.UntilTime = abuseTestNow.Add(-time.Minute)
	history := []AbuseHistoryAction{
		abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, abuseTestDay),
		abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, abuseTestDay),
		abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, abuseTestDay),
		ended,
	}
	step := abuseTestStep(t, AbuseCategorySpam, abuseTestSendingDomain(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindMailLadder)) {
		t.Fatalf("after a suspension's until: %+v", step)
	}
}

// A new threat case is a new incident, so strikes that a lift left in the window take
// it straight back to a suspension, unlike a complaint with no new halt.
func TestNextAbuseStepThreatCaseSuspendsAfterALift(t *testing.T) {
	history := []AbuseHistoryAction{
		abuseTestAction(AbuseCategoryPhishing, AbuseActionKindHostKill, 3*abuseTestDay),
		abuseTestAction(AbuseCategoryPhishing, AbuseActionKindLabelKill, 2*abuseTestDay),
		abuseTestActionInState(AbuseCategoryPhishing, AbuseActionKindNetworkSuspend, abuseTestDay, AbuseReviewStateLifted),
	}
	step := abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNetworkSuspend)) {
		t.Fatalf("a threat case after a lifted suspension: %+v", step)
	}
}

// A suspension holds until reviewed, so one older than every window still stands.
func TestNextAbuseStepSuspensionStandsPastTheWindows(t *testing.T) {
	history := []AbuseHistoryAction{
		abuseTestAction(AbuseCategoryPhishing, AbuseActionKindNetworkSuspend, 400*abuseTestDay),
	}
	step := abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), history)
	if !abuseTestStepEqual(step, AbuseStep{}) {
		t.Fatalf("a threat case under a suspension: %+v", step)
	}
	step = abuseTestStep(t, AbuseCategoryCsam, abuseTestHost(), history)
	if !abuseTestStepEqual(step, AbuseStep{Report: true}) {
		t.Fatalf("a csam case under a suspension: %+v", step)
	}
}

// A kill that stands holds its host, so another hit on it escalates nothing, whichever
// case it opens. The twins show each way a kill stops standing.
func TestNextAbuseStepHitOnAKilledHostAddsNoStrike(t *testing.T) {
	kill := abuseTestAction(AbuseCategoryPhishing, AbuseActionKindHostKill, abuseTestDay)
	kill.Target = "SHOP.net.root.example."
	step := abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), []AbuseHistoryAction{kill})
	if !abuseTestStepEqual(step, AbuseStep{}) {
		t.Fatalf("a hit on a killed host: %+v", step)
	}

	upheld := kill
	upheld.ReviewState = AbuseReviewStateUpheld
	step = abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), []AbuseHistoryAction{upheld})
	if !abuseTestStepEqual(step, AbuseStep{}) {
		t.Fatalf("a hit on a host whose kill was upheld: %+v", step)
	}

	lifted := kill
	lifted.ReviewState = AbuseReviewStateLifted
	step = abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), []AbuseHistoryAction{lifted})
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindLabelKill, AbuseActionKindMailHalt)) {
		t.Fatalf("a hit after the kill was lifted: %+v", step)
	}

	wrong := kill
	wrong.ReviewState = AbuseReviewStateLiftedWrong
	step = abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), []AbuseHistoryAction{wrong})
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindHostKill)) {
		t.Fatalf("a hit after the kill was lifted as wrong: %+v", step)
	}

	suspended := kill
	suspended.Kind = AbuseActionKindHostSuspend
	step = abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), []AbuseHistoryAction{suspended})
	if !abuseTestStepEqual(step, AbuseStep{}) {
		t.Fatalf("a hit on a suspended host: %+v", step)
	}

	ended := kill
	ended.UntilTime = abuseTestNow.Add(-time.Second)
	step = abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), []AbuseHistoryAction{ended})
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindLabelKill, AbuseActionKindMailHalt)) {
		t.Fatalf("a hit after the kill's until: %+v", step)
	}

	running := kill
	running.UntilTime = abuseTestNow.Add(time.Second)
	step = abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), []AbuseHistoryAction{running})
	if !abuseTestStepEqual(step, AbuseStep{}) {
		t.Fatalf("a hit before the kill's until: %+v", step)
	}

	other := kill
	other.Target = "other.net.root.example"
	step = abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), []AbuseHistoryAction{other})
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindLabelKill, AbuseActionKindMailHalt)) {
		t.Fatalf("a hit on another host: %+v", step)
	}

	protective := kill
	protective.Category = AbuseCategoryExposedPanel
	step = abuseTestStep(t, AbuseCategoryExposedPanel, abuseTestHost(), []AbuseHistoryAction{protective})
	if !abuseTestStepEqual(step, AbuseStep{}) {
		t.Fatalf("a panel on a killed host: %+v", step)
	}
}

// A bare network names no host to kill and no mail to halt, so the first rung waits
// for a person, and the higher rungs, which act on the network, still act.
func TestNextAbuseStepBareNetworkTakesNoFirstRung(t *testing.T) {
	target := AbuseTarget{Kind: AbuseTargetKindNetwork, Name: "net"}
	step := abuseTestStep(t, AbuseCategoryPhishing, target, nil)
	if !abuseTestStepEqual(step, AbuseStep{}) {
		t.Fatalf("first rung: %+v", step)
	}
	step = abuseTestStep(t, AbuseCategoryExposedPanel, target, nil)
	if !abuseTestStepEqual(step, AbuseStep{}) {
		t.Fatalf("a panel: %+v", step)
	}
	step = abuseTestStep(t, AbuseCategoryCompromise, target, nil)
	if !abuseTestStepEqual(step, AbuseStep{}) {
		t.Fatalf("a compromise: %+v", step)
	}
	history := []AbuseHistoryAction{abuseTestAction(AbuseCategoryPhishing, AbuseActionKindHostKill, abuseTestDay)}
	step = abuseTestStep(t, AbuseCategoryPhishing, target, history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindLabelKill, AbuseActionKindMailHalt)) {
		t.Fatalf("second rung: %+v", step)
	}
}

// Copyright's third counts only when upheld: 2 upheld and a third waiting on review
// suspend nothing, and the same third upheld does.
func TestNextAbuseStepCopyrightThirdCountsOnlyWhenUpheld(t *testing.T) {
	history := []AbuseHistoryAction{
		abuseTestActionInState(AbuseCategoryCopyright, AbuseActionKindHostKill, 200*abuseTestDay, AbuseReviewStateUpheld),
		abuseTestActionInState(AbuseCategoryCopyright, AbuseActionKindHostKill, 100*abuseTestDay, AbuseReviewStateUpheld),
		abuseTestActionInState(AbuseCategoryCopyright, AbuseActionKindHostKill, abuseTestDay, AbuseReviewStatePending),
	}
	step := abuseTestStep(t, AbuseCategoryCopyright, abuseTestHost(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindHostKill)) {
		t.Fatalf("with the third pending: %+v", step)
	}

	history[2].ReviewState = AbuseReviewStateUpheld
	step = abuseTestStep(t, AbuseCategoryCopyright, abuseTestHost(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNetworkSuspend)) {
		t.Fatalf("with the third upheld: %+v", step)
	}

	history[2].ReviewState = AbuseReviewStateLiftedWrong
	step = abuseTestStep(t, AbuseCategoryCopyright, abuseTestHost(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNoticeForward)) {
		t.Fatalf("with the third lifted as wrong: %+v", step)
	}
}

// Copyright's ladder reads copyright's rows: a network's upheld phishing cases are no
// repeat infringement, and its kills no earlier notice.
func TestNextAbuseStepCopyrightCountsOnlyCopyright(t *testing.T) {
	history := []AbuseHistoryAction{
		abuseTestActionInState(AbuseCategoryPhishing, AbuseActionKindHostKill, abuseTestDay, AbuseReviewStateUpheld),
		abuseTestActionInState(AbuseCategoryPhishing, AbuseActionKindHostKill, abuseTestDay, AbuseReviewStateUpheld),
		abuseTestActionInState(AbuseCategoryPhishing, AbuseActionKindHostKill, abuseTestDay, AbuseReviewStateUpheld),
	}
	step := abuseTestStep(t, AbuseCategoryCopyright, abuseTestHost(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNoticeForward)) {
		t.Fatalf("after three upheld phishing cases: %+v", step)
	}
}

// 3 upheld cases suspend once, and a notice under that suspension is still forwarded.
func TestNextAbuseStepCopyrightSuspendsOnce(t *testing.T) {
	history := []AbuseHistoryAction{
		abuseTestActionInState(AbuseCategoryCopyright, AbuseActionKindNoticeForward, 200*abuseTestDay, AbuseReviewStateUpheld),
		abuseTestActionInState(AbuseCategoryCopyright, AbuseActionKindNoticeForward, 150*abuseTestDay, AbuseReviewStateUpheld),
		abuseTestActionInState(AbuseCategoryCopyright, AbuseActionKindNoticeForward, 100*abuseTestDay, AbuseReviewStateUpheld),
		abuseTestAction(AbuseCategoryCopyright, AbuseActionKindNetworkSuspend, 99*abuseTestDay),
	}
	step := abuseTestStep(t, AbuseCategoryCopyright, abuseTestHost(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNoticeForward)) {
		t.Fatalf("under a standing suspension: %+v", step)
	}
	fourth := abuseTestActionInState(
		AbuseCategoryCopyright, AbuseActionKindNoticeForward, 50*abuseTestDay, AbuseReviewStateUpheld,
	)
	step = abuseTestStep(t, AbuseCategoryCopyright, abuseTestHost(), append(slices.Clone(history), fourth))
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNoticeForward)) {
		t.Fatalf("with a new upheld case under a standing suspension: %+v", step)
	}

	// a lift answers those three, so a further notice alone is only forwarded
	history[3].ReviewState = AbuseReviewStateLifted
	step = abuseTestStep(t, AbuseCategoryCopyright, abuseTestHost(), history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNoticeForward)) {
		t.Fatalf("after a lift with no new upheld case: %+v", step)
	}

	// and a fourth upheld case suspends again
	step = abuseTestStep(t, AbuseCategoryCopyright, abuseTestHost(), append(slices.Clone(history), fourth))
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNetworkSuspend)) {
		t.Fatalf("after a lift with a new upheld case: %+v", step)
	}
}

// Two rows of one upheld case are one upheld case.
func TestNextAbuseStepCopyrightCountsUpheldCases(t *testing.T) {
	first := abuseTestActionInState(AbuseCategoryCopyright, AbuseActionKindNoticeForward, 60*abuseTestDay, AbuseReviewStateUpheld)
	second := abuseTestActionInState(AbuseCategoryCopyright, AbuseActionKindHostKill, 60*abuseTestDay, AbuseReviewStateUpheld)
	second.CaseId = first.CaseId
	third := abuseTestActionInState(AbuseCategoryCopyright, AbuseActionKindHostKill, 50*abuseTestDay, AbuseReviewStateUpheld)
	step := abuseTestStep(t, AbuseCategoryCopyright, abuseTestHost(), []AbuseHistoryAction{first, second, third})
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNoticeForward)) {
		t.Fatalf("two upheld cases: %+v", step)
	}
}

// 12 months is a calendar span, and an upheld case that old still counts.
func TestNextAbuseStepCopyrightUpheldWindowIsTwelveMonths(t *testing.T) {
	yearAgo := abuseTestNow.AddDate(-1, 0, 0)
	for _, c := range []struct {
		time time.Time
		step AbuseStep
	}{
		{time: yearAgo, step: abuseTestKinds(AbuseActionKindNetworkSuspend)},
		{time: yearAgo.Add(-time.Second), step: abuseTestKinds(AbuseActionKindNoticeForward)},
	} {
		history := []AbuseHistoryAction{
			abuseTestActionInState(AbuseCategoryCopyright, AbuseActionKindHostKill, 200*abuseTestDay, AbuseReviewStateUpheld),
			abuseTestActionInState(AbuseCategoryCopyright, AbuseActionKindHostKill, 100*abuseTestDay, AbuseReviewStateUpheld),
			abuseTestActionInState(AbuseCategoryCopyright, AbuseActionKindHostKill, 0, AbuseReviewStateUpheld),
		}
		history[2].CreateTime = c.time
		step := abuseTestStep(t, AbuseCategoryCopyright, abuseTestHost(), history)
		if !abuseTestStepEqual(step, c.step) {
			t.Errorf("an upheld case from %s: got %+v, want %+v", c.time, step, c.step)
		}
	}
}

// A notice with no host, such as one about evs content, goes to the network with no
// action (§21.9), however many came before.
func TestNextAbuseStepCopyrightWithNoHostOnlyForwards(t *testing.T) {
	target := AbuseTarget{Kind: AbuseTargetKindNetwork, Name: "net"}
	history := []AbuseHistoryAction{abuseTestAction(AbuseCategoryCopyright, AbuseActionKindNoticeForward, abuseTestDay)}
	step := abuseTestStep(t, AbuseCategoryCopyright, target, history)
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNoticeForward)) {
		t.Fatalf("a second notice with no host: %+v", step)
	}
}

// A further notice on a killed host is still forwarded and takes no second kill.
func TestNextAbuseStepCopyrightForwardsOnAKilledHost(t *testing.T) {
	kill := abuseTestAction(AbuseCategoryCopyright, AbuseActionKindHostKill, abuseTestDay)
	kill.Target = abuseTestHost().Name
	step := abuseTestStep(t, AbuseCategoryCopyright, abuseTestHost(), []AbuseHistoryAction{kill})
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindNoticeForward)) {
		t.Fatalf("a notice on a killed host: %+v", step)
	}
}

// Protective kills strike nothing, so any number of them leaves the next one the same
// and the first threat case still the first.
func TestNextAbuseStepProtectiveKillsNeverEscalate(t *testing.T) {
	protective := AbuseStep{ActionKinds: []AbuseActionKind{AbuseActionKindHostKill}, Protective: true}
	for _, category := range []AbuseCategory{AbuseCategoryExposedPanel, AbuseCategoryCompromise} {
		history := []AbuseHistoryAction{}
		for range 5 {
			history = append(history, abuseTestAction(category, AbuseActionKindHostKill, abuseTestDay))
		}
		step := abuseTestStep(t, category, abuseTestHost(), history)
		if !abuseTestStepEqual(step, protective) {
			t.Errorf("%q after five: %+v", category, step)
		}
		step = abuseTestStep(t, AbuseCategoryPhishing, abuseTestHost(), history)
		if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindHostKill)) {
			t.Errorf("phishing after five of %q: %+v", category, step)
		}
	}
}

// An exposed panel is a host's problem, so a mail target gives it nothing to act on.
func TestNextAbuseStepExposedPanelHaltsNoMail(t *testing.T) {
	step := abuseTestStep(t, AbuseCategoryExposedPanel, abuseTestSendingDomain(), nil)
	if !abuseTestStepEqual(step, AbuseStep{}) {
		t.Fatalf("a panel on a sending domain: %+v", step)
	}
}

// Act on the smallest unit, never the root, in every category and at every rung.
func TestNextAbuseStepRefusesTheRootAndPlatformNames(t *testing.T) {
	names := []string{
		"root.example",
		"ROOT.example",
		"root.example.",
		" root.example ",
		"*.root.example",
		"*",
		"ns1.root.example",
		"platform.example",
		"Platform.Example.",
		"api.platform.example",
		"a.b.platform.example",
		"",
		".",
	}
	categories := []AbuseCategory{
		AbuseCategorySpam,
		AbuseCategoryPhishing,
		AbuseCategoryMalware,
		AbuseCategoryCommandAndControl,
		AbuseCategoryCopyright,
		AbuseCategoryExposedPanel,
		AbuseCategoryCompromise,
		AbuseCategoryCsam,
		AbuseCategoryThreatToLife,
		AbuseCategoryBrand,
		AbuseCategoryOther,
	}
	kinds := []AbuseTargetKind{
		AbuseTargetKindHost,
		AbuseTargetKindSendingDomain,
		AbuseTargetKindLabel,
		AbuseTargetKindNetwork,
	}
	history := []AbuseHistoryAction{
		abuseTestAction(AbuseCategoryPhishing, AbuseActionKindHostKill, abuseTestDay),
		abuseTestAction(AbuseCategoryPhishing, AbuseActionKindHostKill, abuseTestDay),
	}
	for _, name := range names {
		for _, category := range categories {
			for _, kind := range kinds {
				target := AbuseTarget{Kind: kind, Name: name}
				step, err := NextAbuseStep(category, target, history, abuseTestNow, abuseTestSettings())
				if !errors.Is(err, ErrAbuseTargetRefused) {
					t.Errorf("%q %q %q: err %v", name, category, kind, err)
				}
				if !abuseTestStepEqual(step, AbuseStep{}) {
					t.Errorf("%q %q %q: step %+v", name, category, kind, step)
				}
			}
		}
	}
}

// A root's own subdomains are users' names, unlike a platform domain's, and a name
// that only ends like a listed one is neither.
func TestNextAbuseStepActsOnNamesUnderTheRoot(t *testing.T) {
	for _, name := range []string{
		"net.root.example",
		"shop.net.root.example",
		"notroot.example",
		"notplatform.example",
		"platform.example.net.root.example",
	} {
		target := AbuseTarget{Kind: AbuseTargetKindHost, Name: name}
		step := abuseTestStep(t, AbuseCategoryPhishing, target, nil)
		if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindHostKill)) {
			t.Errorf("%q: %+v", name, step)
		}
	}
}

// With either list missing a ladder cannot tell the root from a host, so it acts on
// neither.
func TestNextAbuseStepRefusesEveryTargetWithoutItsNameLists(t *testing.T) {
	for _, c := range []struct {
		rootNames     []string
		platformNames []string
	}{
		{rootNames: nil, platformNames: nil},
		{rootNames: []string{"root.example"}, platformNames: nil},
		{rootNames: nil, platformNames: []string{"platform.example"}},
	} {
		settings := DefaultAbusePolicySettings()
		settings.RootNames = c.rootNames
		settings.PlatformNames = c.platformNames
		step, err := NextAbuseStep(AbuseCategoryCsam, abuseTestHost(), nil, abuseTestNow, settings)
		if !errors.Is(err, ErrAbuseTargetRefused) {
			t.Errorf("%v %v: err %v", c.rootNames, c.platformNames, err)
		}
		if !abuseTestStepEqual(step, AbuseStep{}) {
			t.Errorf("%v %v: step %+v", c.rootNames, c.platformNames, step)
		}
	}
}

func TestNextAbuseStepRefusesUnknownTargetKind(t *testing.T) {
	for _, kind := range []AbuseTargetKind{"", "zone", "root"} {
		target := AbuseTarget{Kind: kind, Name: "shop.net.root.example"}
		_, err := NextAbuseStep(AbuseCategoryPhishing, target, nil, abuseTestNow, abuseTestSettings())
		if !errors.Is(err, ErrAbuseTargetRefused) {
			t.Errorf("%q: err %v", kind, err)
		}
	}
}

func TestNextAbuseStepRefusesUnknownCategory(t *testing.T) {
	for _, category := range []AbuseCategory{"", "unlisted"} {
		step, err := NextAbuseStep(category, abuseTestHost(), nil, abuseTestNow, abuseTestSettings())
		if !errors.Is(err, ErrAbuseCategoryUnknown) {
			t.Errorf("%q: err %v", category, err)
		}
		if !abuseTestStepEqual(step, AbuseStep{}) {
			t.Errorf("%q: step %+v", category, step)
		}
	}
}

// A zero rung disables itself, so a settings struct left empty cannot escalate a first
// report straight to a suspension.
func TestNextAbuseStepZeroRungsNeverEscalate(t *testing.T) {
	settings := &AbusePolicySettings{
		RootNames:     []string{"root.example"},
		PlatformNames: []string{"platform.example"},
	}
	for _, c := range []struct {
		category AbuseCategory
		target   AbuseTarget
		history  []AbuseHistoryAction
		step     AbuseStep
	}{
		{category: AbuseCategorySpam, target: abuseTestSendingDomain(), step: abuseTestKinds(AbuseActionKindMailLadder)},
		{category: AbuseCategoryPhishing, target: abuseTestHost(), step: abuseTestKinds(AbuseActionKindHostKill)},
		{category: AbuseCategoryCopyright, target: abuseTestHost(), step: abuseTestKinds(AbuseActionKindNoticeForward)},
		{
			category: AbuseCategorySpam,
			target:   abuseTestSendingDomain(),
			history:  []AbuseHistoryAction{abuseTestAction(AbuseCategorySpam, AbuseActionKindMailHalt, 0)},
			step:     abuseTestKinds(AbuseActionKindMailLadder),
		},
		{
			category: AbuseCategoryCopyright,
			target:   abuseTestHost(),
			history: []AbuseHistoryAction{
				abuseTestActionInState(AbuseCategoryCopyright, AbuseActionKindNoticeForward, 0, AbuseReviewStateUpheld),
			},
			step: abuseTestKinds(AbuseActionKindNoticeForward),
		},
	} {
		step, err := NextAbuseStep(c.category, c.target, c.history, abuseTestNow, settings)
		if err != nil {
			t.Fatalf("%q: %v", c.category, err)
		}
		if !abuseTestStepEqual(step, c.step) {
			t.Errorf("%q: got %+v, want %+v", c.category, step, c.step)
		}
	}
}

// The windows and rungs read the settings instance, not §21.4's numbers.
func TestNextAbuseStepReadsItsSettings(t *testing.T) {
	settings := abuseTestSettings()
	settings.StrikeWindow = abuseTestDay
	history := []AbuseHistoryAction{abuseTestAction(AbuseCategoryPhishing, AbuseActionKindHostKill, 2*abuseTestDay)}
	step, err := NextAbuseStep(AbuseCategoryPhishing, abuseTestHost(), history, abuseTestNow, settings)
	if err != nil {
		t.Fatal(err)
	}
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindHostKill)) {
		t.Fatalf("a strike past a one-day window: %+v", step)
	}

	settings = abuseTestSettings()
	settings.LabelKillStrike = 3
	settings.SuspendStrike = 4
	history = []AbuseHistoryAction{
		abuseTestAction(AbuseCategoryPhishing, AbuseActionKindHostKill, time.Hour),
		abuseTestAction(AbuseCategoryPhishing, AbuseActionKindHostKill, time.Hour),
	}
	step, err = NextAbuseStep(AbuseCategoryPhishing, abuseTestHost(), history, abuseTestNow, settings)
	if err != nil {
		t.Fatal(err)
	}
	if !abuseTestStepEqual(step, abuseTestKinds(AbuseActionKindLabelKill, AbuseActionKindMailHalt)) {
		t.Fatalf("a third strike with the label rung at three: %+v", step)
	}
}

func TestAbusePolicyDefaultsMatchTheDesign(t *testing.T) {
	settings := DefaultAbusePolicySettings()
	for _, c := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{name: "strike window", got: settings.StrikeWindow, want: 30 * abuseTestDay},
		{name: "halt window", got: settings.HaltWindow, want: 30 * abuseTestDay},
		{name: "copyright kill window", got: settings.CopyrightKillWindow, want: 30 * abuseTestDay},
		{name: "notice review", got: settings.NoticeReview, want: time.Hour},
		{name: "machine review", got: settings.MachineReview, want: 24 * time.Hour},
		{name: "report open review", got: settings.ReportOpenReview, want: 4 * time.Hour},
		{name: "paid review", got: settings.PaidReview, want: 72 * time.Hour},
		{name: "community review", got: settings.CommunityReview, want: 7 * abuseTestDay},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, c.got, c.want)
		}
	}
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{name: "label kill strike", got: settings.LabelKillStrike, want: 2},
		{name: "suspend strike", got: settings.SuspendStrike, want: 3},
		{name: "suspend halts", got: settings.SuspendHalts, want: 3},
		{name: "copyright kill notice", got: settings.CopyrightKillNotice, want: 2},
		{name: "copyright upheld months", got: settings.CopyrightUpheldMonths, want: 12},
		{name: "copyright suspend upheld", got: settings.CopyrightSuspendUpheld, want: 3},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, c.got, c.want)
		}
	}
	if len(settings.RootNames) != 0 || len(settings.PlatformNames) != 0 {
		t.Errorf("a default names a production host: %v %v", settings.RootNames, settings.PlatformNames)
	}
}
