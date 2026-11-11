//go:build sidecar_testhooks

// FS-1's and FS-58's histories under CheckLinearizable (TEST-M2-02, on EVS1-M2-42's model of §11.8's commit rule),
// read from the evs world each row runs (fault.EvsWorld.History), on T2's fakes. A history must hold an unknown
// outcome to count, so each run here holds one: FS-1's registered case loses its CAS answer to a killed instance, and
// again with each of a spread of its writes' answers lost; its unknown clause answers 503 {outcome: unknown}; and
// FS-58's cases, which lose nothing of their own, run with the instance serving A killed at its last head CAS, so the
// lb sends A on and the next instance answers it. A run of a row as written that loses no answer is
// refused as no evidence, which is shown on FS-1's concurrent retries and FS-58's cases. The schedules hold evs
// instances at barriers, so they need the sidecar_testhooks tag; T3's runs are linearize_rows_t3_test.go's.

package sidecart3

import (
	"errors"
	"fmt"
	"testing"

	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// One fixed run of builder's schedule alone, refusal or not, on a Net from netSettings, each plan the world's schedule
// names in plans armed before its first step, and the history its world left. plans nil arms none.
func linearizeRowHistory(t testing.TB, builder fault.Builder, netSettings *sidecartest.NetSettings, label string, plans func(world *fault.EvsWorld) []*fault.FaultPlan) *fault.History {
	t.Helper()
	net := sidecartest.NewNet(t.Context(), netSettings)
	defer net.Close()
	schedule := builder(net)
	schedule.Twin = nil
	schedule.Refuses = ""
	world := fault.EvsWorldOf(net)
	if world == nil {
		t.Fatalf("%s made no evs world", schedule.Id)
	}
	if plans != nil {
		steps := []*fault.Step{}
		for _, plan := range plans(world) {
			steps = append(steps, &fault.Step{Fault: plan})
		}
		schedule.Steps = append(steps, schedule.Steps...)
	}
	if label != "" {
		schedule.Id = fmt.Sprintf("%s with %s", schedule.Id, label)
	}
	if result := fault.Run(t.Context(), schedule, net); result.Err != nil {
		t.Fatalf("%s", result)
	}
	return world.History(schedule.Id)
}

// Fails t unless the history linearizes, holding an unknown outcome.
func linearizeRowPasses(t testing.TB, history *fault.History) {
	t.Helper()
	report, err := CheckLinearizable(history, linearizeRowSettings(t))
	if err != nil {
		t.Fatalf("%s: %v", report, err)
	}
	t.Logf("%s", report)
}

// Fails t unless the history linearizes and is refused for holding no unknown outcome alone.
func linearizeRowHasNoEvidence(t testing.TB, history *fault.History) {
	t.Helper()
	report, err := CheckLinearizable(history, linearizeRowSettings(t))
	if !errors.Is(err, ErrNoUnknownOutcome) || errors.Is(err, ErrNotLinearizable) || errors.Is(err, ErrLinearizeUndecided) {
		t.Fatalf("%s: %v, want it linearizable and refused as no evidence alone", report, err)
	}
}

func linearizeRowSettings(t testing.TB) *LinearizeSettings {
	settings := DefaultLinearizeSettings()
	settings.RenderDirectory = t.ArtifactDir()
	return settings
}

// A put's answer lost at the nth write, as §22.8's unknown rule loses one.
func linearizeLostPut(nth int) func(world *fault.EvsWorld) []*fault.FaultPlan {
	return func(world *fault.EvsWorld) []*fault.FaultPlan {
		return []*fault.FaultPlan{{Op: sidecartest.FaultOpPut, Nth: nth, Result: fault.ApplyThenDrop, Displaces: true, WritesOnly: true}}
	}
}

// The instance killed as its nth head CAS on the world's repo applies, before it answers.
func linearizeCrashAtHeadCas(instance string, nth int) func(world *fault.EvsWorld) []*fault.FaultPlan {
	return func(world *fault.EvsWorld) []*fault.FaultPlan {
		return []*fault.FaultPlan{{Op: sidecartest.FaultOpPut, KeyPrefix: evsservice.HeadRepoPrefix(world.RepoId), Nth: nth, Result: fault.CrashAfterApply, Actor: instance, WritesOnly: true}}
	}
}

// FS-1's histories on netSettings' Nets: the registered case as written and with each of a spread of writes' answers
// lost, every one holding P's killed attempt; the unknown clause; and the concurrent retries, refused as no evidence.
func linearizeFs1Rows(t testing.TB, netSettings func() *sidecartest.NetSettings, spread int) {
	t.Helper()
	linearizeRowPasses(t, linearizeRowHistory(t, fault.Fs1LostAnswerRetriedOnEvs2, netSettings(), "", nil))
	for nth := 1; nth <= 200; nth += spread {
		linearizeRowPasses(t, linearizeRowHistory(t, fault.Fs1LostAnswerRetriedOnEvs2, netSettings(), fmt.Sprintf("put write %d's answer lost", nth), linearizeLostPut(nth)))
	}
	linearizeRowPasses(t, linearizeRowHistory(t, fault.Fs1UnknownDeletesNothing, netSettings(), "", nil))
	linearizeRowHasNoEvidence(t, linearizeRowHistory(t, fault.Fs1ConcurrentRetries, netSettings(), "", nil))
}

// FS-58's histories on netSettings' Nets: the registered case and its twin as written, refused as no evidence, and
// each with evs1 killed at A's last head CAS, after x.txt's and z.txt's creations, so the lb sends A on to evs2,
// which answers it from the head. In the registered case that is A's CAS the fold failed, the third, since A's rerun
// refuses it 409 for good and records nothing: evs1's attempt is unknown and did not commit, and evs2's refuses A
// again. In the twin it is A's commit on its rerun, the fourth: evs1's attempt is unknown and did commit, and evs2's
// answers A's version.
func linearizeFs58Rows(t testing.TB, netSettings func() *sidecartest.NetSettings) {
	t.Helper()
	type fs58Case struct {
		builder  fault.Builder
		casCount int
		label    string
		// what evs2 answers A
		evs2Outcome fault.Outcome
	}
	cases := []fs58Case{
		{builder: fault.Fs58FoldBetweenReadAndRerun, casCount: 3, label: "evs1 killed at A's CAS the fold failed", evs2Outcome: fault.OutcomeAborted},
		{builder: fault.Fs58DisjointRerun, casCount: 4, label: "evs1 killed as A's rerun commits", evs2Outcome: fault.OutcomeCommitted},
	}
	for _, c := range cases {
		linearizeRowHasNoEvidence(t, linearizeRowHistory(t, c.builder, netSettings(), "", nil))
		history := linearizeRowHistory(t, c.builder, netSettings(), c.label, linearizeCrashAtHeadCas("evs1", c.casCount))
		// the run did what its label says: A's one unknown attempt on evs1, and evs2's answer to A
		unknowns := []*fault.Operation{}
		resent := []*fault.Operation{}
		for _, operation := range history.Operations() {
			switch {
			case operation.Outcome == fault.OutcomeUnknown:
				unknowns = append(unknowns, operation)
			case operation.Actor == "writerA" && operation.Instance == "evs2":
				resent = append(resent, operation)
			}
		}
		if len(unknowns) != 1 || unknowns[0].Actor != "writerA" || unknowns[0].Instance != "evs1" || len(resent) != 1 || resent[0].PushId != unknowns[0].PushId || resent[0].Outcome != c.evs2Outcome {
			t.Fatalf("%s: unknown %v and evs2's %v, want A's attempt on evs1 unknown and evs2's %s", history.Name(), unknowns, resent, c.evs2Outcome)
		}
		linearizeRowPasses(t, history)
	}
}

func TestFs1HistoriesLinearizeOnT2(t *testing.T) {
	linearizeFs1Rows(t, func() *sidecartest.NetSettings { return nil }, 50)
}

func TestFs58HistoriesLinearizeOnT2(t *testing.T) {
	linearizeFs58Rows(t, func() *sidecartest.NetSettings { return nil })
}

// FS-1's own history, changed as a broken evs would have answered: P's retry answering a version other than its
// original, and one of writerB's commits answered as a refusal. Each must fail, so a pass on the real history is the
// check reading it and not a check that passes anything.
func TestFs1HistoryMutantsFailOnT2(t *testing.T) {
	history := linearizeRowHistory(t, fault.Fs1LostAnswerRetriedOnEvs2, nil, "", nil)
	type mutant struct {
		name   string
		mutate func(operation *fault.Operation) bool
	}
	mutants := []mutant{
		{name: "P's retry answering a new version", mutate: func(operation *fault.Operation) bool {
			if operation.Actor == "writerA" && operation.Instance == "evs2" && operation.Outcome == fault.OutcomeCommitted {
				operation.Version += 1
				return true
			}
			return false
		}},
		{name: "a commit answered as a refusal", mutate: func(operation *fault.Operation) bool {
			if operation.Actor == "writerB" && operation.Outcome == fault.OutcomeCommitted && operation.Version == 10 {
				operation.Outcome = fault.OutcomeAborted
				operation.Version = 0
				return true
			}
			return false
		}},
	}
	for _, m := range mutants {
		mutated := fault.NewHistory(history.Name() + ", " + m.name)
		changed := 0
		for _, operation := range history.Operations() {
			if m.mutate(operation) {
				changed += 1
			}
			mutated.Record(operation)
		}
		if changed != 1 {
			t.Fatalf("%s changed %d operations, want one", m.name, changed)
		}
		report, err := CheckLinearizable(mutated, linearizeRowSettings(t))
		if !errors.Is(err, ErrNotLinearizable) {
			t.Errorf("%s: %v, want it not linearizable (%s)", m.name, err, report)
		}
	}
}
