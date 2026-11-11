package sidecart3

// CheckLinearizable on histories built by hand (TEST-M2-02): what §11.8's commit rule accepts passes, a lost update, a
// stale read, a double commit and the rule's other refusals fail, an unknown outcome matches both a commit and none,
// a history with no unknown outcome is refused as no evidence, each ref is checked apart, and a failure is rendered.
// The rows' own histories are linearize_rows_test.go's.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// A history being built by hand on one ref, its times given.
type linearizeFixture struct {
	history *fault.History
	ref     fault.HistoryRef
}

// An empty history named name, on a ref of its own.
func newLinearizeFixture(name string) *linearizeFixture {
	return &linearizeFixture{
		history: fault.NewHistory(name),
		ref:     fault.HistoryRef{RepoId: sidecar.NewId(), RefId: sidecar.NewId()},
	}
}

// A push attempt by actor at instance; ret 0 for an unknown outcome with no return.
func (self *linearizeFixture) push(actor string, instance string, pushId sidecar.Id, call int64, ret int64, outcome fault.Outcome, version uint64) {
	self.history.Record(&fault.Operation{
		Kind:     fault.OperationPush,
		Actor:    actor,
		Instance: instance,
		Ref:      self.ref,
		PushId:   pushId,
		Call:     call,
		Return:   ret,
		Outcome:  outcome,
		Version:  version,
	})
}

// A head read served version, naming pushId's commit there when pushId is not zero.
func (self *linearizeFixture) read(call int64, ret int64, version uint64, pushId sidecar.Id) {
	self.history.Record(&fault.Operation{
		Kind:    fault.OperationRead,
		Actor:   "reader",
		Ref:     self.ref,
		PushId:  pushId,
		Call:    call,
		Return:  ret,
		Outcome: fault.OutcomeCommitted,
		Version: version,
	})
}

// An unknown attempt of a push nothing else names, so a history that fails fails for its own reason and not for want
// of evidence.
func (self *linearizeFixture) unrelatedUnknown(call int64) {
	self.push("writerC", "evs3", sidecar.NewId(), call, 0, fault.OutcomeUnknown, 0)
}

// The defaults, rendering into t's artifact directory.
func linearizeSettings(t *testing.T) *LinearizeSettings {
	settings := DefaultLinearizeSettings()
	settings.RenderDirectory = t.ArtifactDir()
	return settings
}

// Fails t unless the history passes.
func linearizePasses(t *testing.T, history *fault.History) *LinearizeReport {
	t.Helper()
	report, err := CheckLinearizable(history, linearizeSettings(t))
	if err != nil {
		t.Fatalf("%s: %v", report, err)
	}
	return report
}

// Fails t unless the history fails as not linearizable, and for no other reason.
func linearizeFailsAsNotLinearizable(t *testing.T, history *fault.History) *LinearizeReport {
	t.Helper()
	report, err := CheckLinearizable(history, linearizeSettings(t))
	if !errors.Is(err, ErrNotLinearizable) {
		t.Fatalf("%s: %v, want it not linearizable", report, err)
	}
	if errors.Is(err, ErrNoUnknownOutcome) || errors.Is(err, ErrLinearizeUndecided) {
		t.Fatalf("%s failed for another reason too: %v", report, err)
	}
	return report
}

func TestCheckLinearizablePassesALinearizableHistory(t *testing.T) {
	fixture := newLinearizeFixture("linearizable")
	p1, p2, p3, p4, p5 := sidecar.NewId(), sidecar.NewId(), sidecar.NewId(), sidecar.NewId(), sidecar.NewId()
	fixture.push("writerA", "evs1", p1, 1, 2, fault.OutcomeCommitted, 1)
	fixture.push("writerB", "evs3", p2, 3, 4, fault.OutcomeCommitted, 2)
	fixture.read(5, 6, 2, p2)
	// a retry of p1 answers its original version
	fixture.push("writerA", "evs2", p1, 7, 8, fault.OutcomeCommitted, 1)
	// p3's answer is lost, and it may commit at any time from its call on
	fixture.push("writerA", "evs1", p3, 9, 0, fault.OutcomeUnknown, 0)
	fixture.push("writerB", "evs3", p4, 10, 11, fault.OutcomeCommitted, 3)
	fixture.push("writerB", "evs3", p5, 12, 13, fault.OutcomeAborted, 0)
	fixture.read(14, 15, 3, p4)
	// two attempts at once, the lb's re-send among them, commit once
	p6 := sidecar.NewId()
	fixture.push("writerA", "evs1", p6, 16, 19, fault.OutcomeCommitted, 4)
	fixture.push("writerA", "evs2", p6, 17, 18, fault.OutcomeCommitted, 4)
	fixture.read(20, 21, 4, sidecar.Id{})
	report := linearizePasses(t, fixture.history)
	if len(report.Refs) != 1 || report.OperationCount() != 11 || report.UnknownCount() != 1 {
		t.Fatalf("the report: %s", report)
	}
}

// Two pushes based on v1 both answered committed at v2, and a head read naming another push at a version one
// answered: either way one update was lost.
func TestCheckLinearizableFailsALostUpdate(t *testing.T) {
	overwritten := newLinearizeFixture("lost update")
	p1, p2, p3 := sidecar.NewId(), sidecar.NewId(), sidecar.NewId()
	overwritten.push("writerA", "evs1", p1, 1, 2, fault.OutcomeCommitted, 1)
	overwritten.push("writerA", "evs1", p2, 3, 4, fault.OutcomeCommitted, 2)
	overwritten.push("writerB", "evs3", p3, 5, 6, fault.OutcomeCommitted, 2)
	overwritten.unrelatedUnknown(7)
	linearizeFailsAsNotLinearizable(t, overwritten.history)

	replaced := newLinearizeFixture("lost update read")
	q1, q2 := sidecar.NewId(), sidecar.NewId()
	replaced.push("writerA", "evs1", q1, 1, 2, fault.OutcomeCommitted, 1)
	replaced.read(3, 4, 1, q2)
	replaced.unrelatedUnknown(5)
	linearizeFailsAsNotLinearizable(t, replaced.history)
}

// A read that began after v2's commit returned, served v1; the same read overlapping that commit may see either.
func TestCheckLinearizableFailsAStaleRead(t *testing.T) {
	stale := newLinearizeFixture("stale read")
	p1, p2 := sidecar.NewId(), sidecar.NewId()
	stale.push("writerA", "evs1", p1, 1, 2, fault.OutcomeCommitted, 1)
	stale.push("writerB", "evs3", p2, 3, 4, fault.OutcomeCommitted, 2)
	stale.read(5, 6, 1, p1)
	stale.unrelatedUnknown(7)
	linearizeFailsAsNotLinearizable(t, stale.history)

	concurrent := newLinearizeFixture("concurrent read")
	concurrent.push("writerA", "evs1", p1, 1, 2, fault.OutcomeCommitted, 1)
	concurrent.push("writerB", "evs3", p2, 4, 5, fault.OutcomeCommitted, 2)
	concurrent.read(3, 6, 1, p1)
	concurrent.unrelatedUnknown(7)
	linearizePasses(t, concurrent.history)
}

// A retry of a committed push that committed again rather than answering its original version (FI-2).
func TestCheckLinearizableFailsADoubleCommit(t *testing.T) {
	fixture := newLinearizeFixture("double commit")
	p1, p2 := sidecar.NewId(), sidecar.NewId()
	fixture.push("writerA", "evs1", p1, 1, 2, fault.OutcomeCommitted, 1)
	fixture.push("writerB", "evs3", p2, 3, 4, fault.OutcomeCommitted, 2)
	fixture.push("writerA", "evs2", p1, 5, 6, fault.OutcomeCommitted, 3)
	fixture.unrelatedUnknown(7)
	linearizeFailsAsNotLinearizable(t, fixture.history)
}

// The rule's other refusals, each beside an unrelated unknown outcome.
func TestCheckLinearizableFailsWhatTheRuleForbids(t *testing.T) {
	type forbidden struct {
		name  string
		build func(fixture *linearizeFixture)
	}
	p1 := sidecar.NewId()
	cases := []forbidden{
		{name: "a refused push id committing later (I-EVS-3)", build: func(fixture *linearizeFixture) {
			fixture.push("writerA", "evs1", p1, 1, 2, fault.OutcomeAborted, 0)
			fixture.push("writerA", "evs2", p1, 3, 4, fault.OutcomeCommitted, 1)
		}},
		{name: "a committed push id refused later", build: func(fixture *linearizeFixture) {
			fixture.push("writerA", "evs1", p1, 1, 2, fault.OutcomeCommitted, 1)
			fixture.push("writerA", "evs2", p1, 3, 4, fault.OutcomeAborted, 0)
		}},
		{name: "a skipped version", build: func(fixture *linearizeFixture) {
			fixture.push("writerA", "evs1", p1, 1, 2, fault.OutcomeCommitted, 2)
		}},
		{name: "an unknown attempt of a refused push id committing", build: func(fixture *linearizeFixture) {
			fixture.push("writerA", "evs1", p1, 1, 2, fault.OutcomeAborted, 0)
			fixture.push("writerA", "evs2", p1, 3, 0, fault.OutcomeUnknown, 0)
			fixture.read(5, 6, 1, p1)
		}},
		{name: "a read above every commit", build: func(fixture *linearizeFixture) {
			fixture.push("writerA", "evs1", p1, 1, 2, fault.OutcomeCommitted, 1)
			fixture.read(3, 4, 2, sidecar.Id{})
		}},
		{name: "a read of an absent ref after its first commit", build: func(fixture *linearizeFixture) {
			fixture.push("writerA", "evs1", p1, 1, 2, fault.OutcomeCommitted, 1)
			fixture.read(3, 4, 0, sidecar.Id{})
		}},
	}
	for _, c := range cases {
		fixture := newLinearizeFixture(c.name)
		c.build(fixture)
		fixture.unrelatedUnknown(10)
		report, err := CheckLinearizable(fixture.history, linearizeSettings(t))
		if !errors.Is(err, ErrNotLinearizable) {
			t.Errorf("%s: %v, want it not linearizable (%s)", c.name, err, report)
		}
	}
}

// An attempt validated before its repo's shred began commits after another attempt of its push id was refused
// SHREDDING, which FI-2 allows since the shred destroys it; the same after any other refusal fails.
func TestCheckLinearizableLetsAShredRefusalBindNothing(t *testing.T) {
	type refusal struct {
		code   protocol.EvsRefusalCode
		passes bool
	}
	for _, r := range []refusal{
		{code: protocol.EvsRefusalCode_EVS_REFUSAL_SHREDDING, passes: true},
		{code: protocol.EvsRefusalCode_EVS_REFUSAL_GATE_CLOSED, passes: false},
		{code: protocol.EvsRefusalCode(0), passes: false},
	} {
		fixture := newLinearizeFixture("refused " + r.code.String())
		p1, p2 := sidecar.NewId(), sidecar.NewId()
		fixture.push("writerA", "evs1", p1, 1, 2, fault.OutcomeCommitted, 1)
		fixture.history.Record(&fault.Operation{Kind: fault.OperationPush, Actor: "writerB", Instance: "evs3", Ref: fixture.ref, PushId: p2, Call: 3, Return: 4, Outcome: fault.OutcomeAborted, Code: r.code})
		fixture.push("writerB", "evs2", p2, 2, 6, fault.OutcomeCommitted, 2)
		fixture.read(7, 8, 2, p2)
		fixture.unrelatedUnknown(9)
		report, err := CheckLinearizable(fixture.history, linearizeSettings(t))
		if r.passes && err != nil {
			t.Errorf("after a %s refusal: %v (%s)", r.code, err, report)
		}
		if !r.passes && !errors.Is(err, ErrNotLinearizable) {
			t.Errorf("after a %s refusal: %v, want it not linearizable (%s)", r.code, err, report)
		}
	}
}

// A lost answer whose push did commit: a later read names it, and its retry answers its version.
func TestCheckLinearizablePassesAnUnknownOutcomeThatCommitted(t *testing.T) {
	fixture := newLinearizeFixture("unknown that committed")
	p1, p2 := sidecar.NewId(), sidecar.NewId()
	fixture.push("writerA", "evs1", p1, 1, 0, fault.OutcomeUnknown, 0)
	fixture.read(3, 4, 1, p1)
	fixture.push("writerB", "evs3", p2, 5, 6, fault.OutcomeCommitted, 2)
	fixture.push("writerA", "evs2", p1, 7, 8, fault.OutcomeCommitted, 1)
	linearizePasses(t, fixture.history)
}

// A lost answer whose push did not commit: another push takes the next version, a read names it, and the lost push's
// retry meets its refusal; and one that never resolves at all.
func TestCheckLinearizablePassesAnUnknownOutcomeThatDidNot(t *testing.T) {
	refused := newLinearizeFixture("unknown that did not commit")
	p1, p2 := sidecar.NewId(), sidecar.NewId()
	refused.push("writerA", "evs1", p1, 1, 2, fault.OutcomeUnknown, 0)
	refused.push("writerB", "evs3", p2, 3, 4, fault.OutcomeCommitted, 1)
	refused.read(5, 6, 1, p2)
	refused.push("writerA", "evs2", p1, 7, 8, fault.OutcomeAborted, 0)
	refused.read(9, 10, 1, p2)
	linearizePasses(t, refused.history)

	open := newLinearizeFixture("unknown never resolved")
	q1, q2 := sidecar.NewId(), sidecar.NewId()
	open.push("writerA", "evs1", q1, 1, 0, fault.OutcomeUnknown, 0)
	open.push("writerB", "evs3", q2, 3, 4, fault.OutcomeCommitted, 1)
	open.read(5, 6, 1, q2)
	linearizePasses(t, open.history)
}

// A history every answer of which arrived is refused, linearizable or not, and an empty one too.
func TestCheckLinearizableRefusesAHistoryWithNoUnknownOutcome(t *testing.T) {
	fixture := newLinearizeFixture("no unknown")
	p1, p2 := sidecar.NewId(), sidecar.NewId()
	fixture.push("writerA", "evs1", p1, 1, 2, fault.OutcomeCommitted, 1)
	fixture.push("writerB", "evs3", p2, 3, 4, fault.OutcomeAborted, 0)
	fixture.read(5, 6, 1, p1)
	report, err := CheckLinearizable(fixture.history, linearizeSettings(t))
	if !errors.Is(err, ErrNoUnknownOutcome) || errors.Is(err, ErrNotLinearizable) {
		t.Fatalf("%s: %v, want it refused as no evidence alone", report, err)
	}
	if len(report.Refs) != 1 || report.Refs[0].Result != "Ok" {
		t.Fatalf("the ref did not linearize: %s", report)
	}
	if _, err := CheckLinearizable(fault.NewHistory("empty"), linearizeSettings(t)); !errors.Is(err, ErrNoUnknownOutcome) {
		t.Fatalf("an empty history: %v, want it refused as no evidence", err)
	}
}

// A malformed operation fails the check, whatever the rest holds.
func TestCheckLinearizableFailsAMalformedOperation(t *testing.T) {
	fixture := newLinearizeFixture("malformed")
	fixture.push("writerA", "evs1", sidecar.NewId(), 1, 2, fault.OutcomeCommitted, 0)
	fixture.unrelatedUnknown(3)
	report, err := CheckLinearizable(fixture.history, linearizeSettings(t))
	if err == nil || !strings.Contains(err.Error(), "no check can read") || !strings.Contains(err.Error(), "version 0") {
		t.Fatalf("%s: %v, want the malformed operation named", report, err)
	}
}

// Two refs each commit their own v1, which one ref could not; then a stale read on the second fails it alone.
func TestCheckLinearizableChecksEachRefApart(t *testing.T) {
	first := newLinearizeFixture("two refs")
	second := &linearizeFixture{history: first.history, ref: fault.HistoryRef{RepoId: first.ref.RepoId, RefId: sidecar.NewId()}}
	p1, p2, p3 := sidecar.NewId(), sidecar.NewId(), sidecar.NewId()
	first.push("writerA", "evs1", p1, 1, 2, fault.OutcomeCommitted, 1)
	second.push("writerB", "evs3", p2, 3, 4, fault.OutcomeCommitted, 1)
	second.push("writerB", "evs3", p3, 5, 0, fault.OutcomeUnknown, 0)
	first.read(6, 7, 1, p1)
	report := linearizePasses(t, first.history)
	if len(report.Refs) != 2 {
		t.Fatalf("the report: %s", report)
	}

	second.push("writerB", "evs3", sidecar.NewId(), 8, 9, fault.OutcomeCommitted, 2)
	second.read(10, 11, 1, p2)
	report, err := CheckLinearizable(first.history, linearizeSettings(t))
	if !errors.Is(err, ErrNotLinearizable) {
		t.Fatalf("%s: %v, want the second ref not linearizable", report, err)
	}
	if report.Refs[0].Result != "Ok" || report.Refs[1].Result != "Illegal" || !strings.Contains(err.Error(), second.ref.String()) || strings.Contains(err.Error(), first.ref.String()) {
		t.Fatalf("%s: %v, want the second ref failing alone", report, err)
	}
}

// A double commit rendered: the text names the failing prefix, the state it leaves and the attempt no order places,
// and the visualization is written beside it, into the settings' directory or, with none, a directory of its own.
func TestCheckLinearizableRendersTheFailingPrefix(t *testing.T) {
	fixture := newLinearizeFixture("FS-0 rendered")
	p1, p2 := sidecar.NewId(), sidecar.NewId()
	fixture.push("writerA", "evs1", p1, 1, 2, fault.OutcomeCommitted, 1)
	fixture.push("writerB", "evs3", p2, 3, 4, fault.OutcomeCommitted, 2)
	fixture.push("writerA", "evs2", p1, 5, 6, fault.OutcomeCommitted, 3)
	fixture.unrelatedUnknown(7)
	settings := linearizeSettings(t)
	report, err := CheckLinearizable(fixture.history, settings)
	if !errors.Is(err, ErrNotLinearizable) {
		t.Fatalf("%s: %v", report, err)
	}
	renderingPaths := report.Refs[0].RenderingPaths
	if len(renderingPaths) != 2 || filepath.Dir(renderingPaths[0]) != settings.RenderDirectory || !strings.HasSuffix(renderingPaths[0], ".txt") || !strings.HasSuffix(renderingPaths[1], ".html") {
		t.Fatalf("renderings %v in %s", renderingPaths, settings.RenderDirectory)
	}
	if !strings.HasPrefix(filepath.Base(renderingPaths[0]), "FS-0-rendered-") {
		t.Fatalf("a rendering named %s", renderingPaths[0])
	}
	for _, renderingPath := range renderingPaths {
		if !strings.Contains(err.Error(), renderingPath) {
			t.Errorf("the error does not name %s: %v", renderingPath, err)
		}
	}
	textBytes, err := os.ReadFile(renderingPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(textBytes)
	prefix, rest, ok := strings.Cut(text, "what no order places after it")
	if !ok {
		t.Fatalf("the rendering has no unplaced part:\n%s", text)
	}
	unplaced, _, _ := strings.Cut(rest, "every operation of the ref")
	for _, want := range []string{"the failing prefix", "2 of 4", "committed v1", "committed v2", "the state it leaves: {v2}"} {
		if !strings.Contains(prefix, want) {
			t.Errorf("the failing prefix lacks %q:\n%s", want, text)
		}
	}
	if !strings.Contains(unplaced, p1.String()+" at evs2: committed v3") {
		t.Errorf("the double commit is not among what no order places:\n%s", text)
	}
	if info, err := os.Stat(renderingPaths[1]); err != nil || info.Size() == 0 {
		t.Fatalf("the visualization: %v, %v", info, err)
	}

	settings.RenderDirectory = ""
	report, err = CheckLinearizable(fixture.history, settings)
	if !errors.Is(err, ErrNotLinearizable) || len(report.Refs[0].RenderingPaths) != 2 {
		t.Fatalf("%s: %v", report, err)
	}
	directory := filepath.Dir(report.Refs[0].RenderingPaths[0])
	t.Cleanup(func() { os.RemoveAll(directory) })
	if !strings.HasPrefix(filepath.Base(directory), "sidecart3-linearize-") || !strings.Contains(err.Error(), directory) {
		t.Fatalf("rendered into %s: %v", directory, err)
	}
	for _, renderingPath := range report.Refs[0].RenderingPaths {
		if _, err := os.Stat(renderingPath); err != nil {
			t.Fatal(err)
		}
	}
}
