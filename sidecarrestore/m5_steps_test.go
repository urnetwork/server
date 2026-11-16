package sidecarrestore

// §19.10's abuse, dns and labels steps in the frame (OPS-M5-06): their order, the abuse step's plan before its
// apply, each step's outcome listed once however often it runs, the steps' inputs, and a database before MG5. The
// mechanisms are faked here, since each owner tests its own (ABUSE-M5-18, DNS-M5-15, DNS-M5-16); the steps never
// reach a database in these tests.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/taskworker/work"
)

// The sidecar dump's time in testIncidentList's head.
var testSidecarDumpTime = time.Date(2026, 11, 6, 2, 30, 0, 250_000_000, time.UTC)

// A fake of the abuse mechanisms: the exports' records as given, each apply recorded, and every item applied the
// first time and found applied after.
type testAbuseMechanisms struct {
	abuse *controller.AbuseMailboxRecords
	legal *controller.LegalHoldMailboxRecords
	// each apply's flag, and each holds call's
	applies     []bool
	holdApplies []bool
	siteLost    []bool
	applied     map[server.Id]bool
}

func (self *testAbuseMechanisms) deps() *AbuseStepDeps {
	return &AbuseStepDeps{
		TablePresent: func(ctx context.Context, table string) (bool, error) { return true, nil },
		Linked:       func() bool { return true },
		LoadRecords: func(run *StepRun) (*controller.AbuseMailboxRecords, *controller.LegalHoldMailboxRecords, error) {
			return self.abuse, self.legal, nil
		},
		Apply: func(ctx context.Context, plan *controller.AbuseRestorePlan, apply bool, run *StepRun) (*controller.AbuseRestoreResult, error) {
			self.applies = append(self.applies, apply)
			result := &controller.AbuseRestoreResult{Applied: apply}
			if !apply {
				return result, nil
			}
			for _, item := range plan.Items {
				outcome := controller.AbuseRestoreOutcomeApplied
				if self.applied[item.Record.ActionId] {
					outcome = controller.AbuseRestoreOutcomeAlready
				}
				self.applied[item.Record.ActionId] = true
				result.Outcomes = append(result.Outcomes, outcome)
			}
			return result, nil
		},
		ReplaceHolds: func(ctx context.Context, records []*controller.LegalHoldRecord, dumpTime time.Time, now time.Time, siteLost bool, apply bool, run *StepRun) ([]*controller.LegalHoldRestoreLine, error) {
			self.holdApplies = append(self.holdApplies, apply)
			self.siteLost = append(self.siteLost, siteLost)
			lines := []*controller.LegalHoldRestoreLine{}
			for _, record := range records {
				lines = append(lines, &controller.LegalHoldRestoreLine{NetworkId: record.NetworkId, CaseId: record.CaseId, Outcome: controller.LegalHoldRestoreOutcomePlaced, Void: siteLost})
			}
			return lines, nil
		},
		SkipMarker: true,
		Now:        func() time.Time { return testSidecarDumpTime.Add(6 * time.Hour) },
	}
}

// A kill and a lift of it mailed after the dump, an action from before the dump, one that ran out, a message that
// did not verify, and a hold placed after the dump.
func newTestAbuseMechanisms() (*testAbuseMechanisms, server.Id, server.Id) {
	networkId := server.NewId()
	killId := server.NewId()
	liftId := server.NewId()
	expired := testSidecarDumpTime.Add(time.Hour)
	mechanisms := &testAbuseMechanisms{
		abuse: &controller.AbuseMailboxRecords{
			Records: []*controller.AbuseRestoreRecord{
				{Index: 0, Record: &controller.AbuseRecord{ActionId: server.NewId(), CaseId: server.NewId(), NetworkId: &networkId, Kind: model.AbuseActionKindLabelKill, Time: testSidecarDumpTime.Add(-time.Hour)}},
				{Index: 1, Record: &controller.AbuseRecord{ActionId: killId, CaseId: server.NewId(), NetworkId: &networkId, Kind: model.AbuseActionKindLabelKill, Time: testSidecarDumpTime.Add(time.Minute)}},
				{Index: 2, Record: &controller.AbuseRecord{ActionId: server.NewId(), CaseId: server.NewId(), NetworkId: &networkId, Kind: model.AbuseActionKindHostSuspend, Until: &expired, Time: testSidecarDumpTime.Add(2 * time.Minute)}},
				{Index: 4, Record: &controller.AbuseRecord{ActionId: liftId, CaseId: server.NewId(), NetworkId: &networkId, Kind: model.AbuseActionKindLift, Target: killId.String(), Time: testSidecarDumpTime.Add(3 * time.Minute)}},
			},
			Refused: []*controller.AbuseRestoreRefusal{{Index: 3, Err: controller.ErrAbuseRecordMac}},
		},
		legal: &controller.LegalHoldMailboxRecords{
			Records: []*controller.LegalHoldRecord{{NetworkId: networkId, CaseId: server.NewId(), PlaceTime: testSidecarDumpTime.Add(time.Minute)}},
		},
		applied: map[server.Id]bool{},
	}
	return mechanisms, killId, liftId
}

// A fake restore step: each root's run ends on its second call, the first answering where it stood with an error.
type testDnsRestoreRunner struct {
	calls     []string
	incidents []sidecar.Id
}

func (self *testDnsRestoreRunner) Run(ctx context.Context, root string, incidentId sidecar.Id) (*dnsservice.DnsRestoreProgress, error) {
	self.calls = append(self.calls, root)
	self.incidents = append(self.incidents, incidentId)
	progress := &dnsservice.DnsRestoreProgress{Root: root, IncidentId: incidentId, RestoreSerial: 2026111700, StoppedNs: []string{"ns2"}}
	count := 0
	for _, called := range self.calls {
		if called == root {
			count += 1
		}
	}
	if count == 1 {
		progress.WaitingNs = []string{"ns1"}
		return progress, errors.New("ns1 has not served the restore serial")
	}
	progress.Done = true
	progress.DiffCount = 1
	return progress, nil
}

func (self *testDnsRestoreRunner) deps(roots ...string) *DnsStepDeps {
	return &DnsStepDeps{
		TablePresent: func(ctx context.Context, table string) (bool, error) { return true, nil },
		Linked:       func() bool { return true },
		Open: func(ctx context.Context, run *StepRun) ([]string, DnsRestoreRunner, func(), error) {
			return roots, self, func() {}, nil
		},
		SkipMarker: true,
	}
}

// A fake scheduler of CT replays.
type testCtReplays struct {
	scheduled []*work.DnsCtReplayArgs
}

func (self *testCtReplays) deps(roots ...string) *LabelsStepDeps {
	return &LabelsStepDeps{
		TablePresent: func(ctx context.Context, table string) (bool, error) { return true, nil },
		Linked:       func() bool { return true },
		Roots:        func() ([]string, error) { return roots, nil },
		Schedule: func(ctx context.Context, args *work.DnsCtReplayArgs) error {
			self.scheduled = append(self.scheduled, args)
			return nil
		},
		SkipMarker: true,
	}
}

// The process's frame holds the three steps' functions.
func TestM5StepsAreRegisteredInTheProcesssFrame(t *testing.T) {
	registered := map[string]bool{}
	for _, step := range DefaultFrame().Steps() {
		registered[step.Name] = step.Function != nil
	}
	for _, name := range []string{StepOps, StepAbuse, StepDns, StepApi, StepLabels} {
		connect.AssertEqual(t, registered[name], true)
	}
}

// The frame refuses dns before abuse, api before dns and labels before api, and runs them in §19.10's order.
func TestM5StepsRunInTheFramesOrder(t *testing.T) {
	mechanisms, _, _ := newTestAbuseMechanisms()
	runner := &testDnsRestoreRunner{}
	replays := &testCtReplays{}
	opsCount, apiCount := 0, 0
	frame := NewFrame()
	frame.Register(StepOps, testCountingStep(&opsCount))
	frame.Register(StepAbuse, func(ctx context.Context, run *StepRun) error { return RunAbuseStep(ctx, mechanisms.deps(), run) })
	frame.Register(StepDns, func(ctx context.Context, run *StepRun) error {
		return RunDnsStep(ctx, runner.deps("ur.example."), run)
	})
	frame.Register(StepApi, testCountingStep(&apiCount))
	frame.Register(StepLabels, func(ctx context.Context, run *StepRun) error {
		return RunLabelsStep(ctx, replays.deps("ur.example."), run)
	})
	incident := testIncident(t, IncidentKindPostgres)
	out := &bytes.Buffer{}
	apply := &StepOptions{Apply: true}

	connect.AssertEqual(t, frame.RunStep(t.Context(), incident, StepOps, out, ""), nil)
	connect.AssertEqual(t, errors.Is(frame.RunStepWithOptions(t.Context(), incident, StepDns, out, apply), ErrStepOutOfOrder), true)
	connect.AssertEqual(t, len(runner.calls), 0)
	connect.AssertEqual(t, frame.RunStepWithOptions(t.Context(), incident, StepAbuse, out, apply), nil)
	connect.AssertEqual(t, errors.Is(frame.RunStep(t.Context(), incident, StepApi, out, ""), ErrStepOutOfOrder), true)
	connect.AssertEqual(t, apiCount, 0)
	// the dns step's first run ends at an NS that has not served yet, and the next goes on
	connect.AssertEqual(t, frame.RunStep(t.Context(), incident, StepDns, out, "") != nil, true)
	connect.AssertEqual(t, incident.StepEnded(StepDns), false)
	connect.AssertEqual(t, errors.Is(frame.RunStep(t.Context(), incident, StepApi, out, ""), ErrStepOutOfOrder), true)
	connect.AssertEqual(t, frame.RunStep(t.Context(), incident, StepDns, out, ""), nil)
	connect.AssertEqual(t, errors.Is(frame.RunStep(t.Context(), incident, StepLabels, out, ""), ErrStepOutOfOrder), true)
	connect.AssertEqual(t, len(replays.scheduled), 0)
	connect.AssertEqual(t, frame.RunStep(t.Context(), incident, StepApi, out, ""), nil)
	connect.AssertEqual(t, frame.RunStep(t.Context(), incident, StepLabels, out, ""), nil)
	connect.AssertEqual(t, len(replays.scheduled), 1)
	for _, step := range []string{StepOps, StepAbuse, StepDns, StepApi, StepLabels} {
		connect.AssertEqual(t, incident.StepEnded(step), true)
	}
}

// The abuse step prints its plan and changes nothing until a run says to apply: then every action and lift mailed
// after the dump, and not past its until, is re-applied in mail order, the refused message listed and never
// applied, and the hold listed for counsel. A second run applies again, which the mechanism finds done, and lists
// nothing twice.
func TestAbuseStepPlansFirstAndAppliesOnlyWhenTold(t *testing.T) {
	mechanisms, killId, liftId := newTestAbuseMechanisms()
	frame := NewFrame()
	frame.Register(StepOps, testCountingStep(new(int)))
	frame.Register(StepAbuse, func(ctx context.Context, run *StepRun) error { return RunAbuseStep(ctx, mechanisms.deps(), run) })
	incident := testIncident(t, IncidentKindPostgres)
	out := &bytes.Buffer{}
	connect.AssertEqual(t, frame.RunStep(t.Context(), incident, StepOps, out, ""), nil)

	err := frame.RunStep(t.Context(), incident, StepAbuse, out, "")
	connect.AssertEqual(t, errors.Is(err, ErrStepPlanOnly), true)
	connect.AssertEqual(t, incident.StepEnded(StepAbuse), false)
	connect.AssertEqual(t, mechanisms.applies, []bool{false})
	connect.AssertEqual(t, mechanisms.holdApplies, []bool{false})
	connect.AssertEqual(t, len(incident.RowsOfKind(ListKindAbuseRestored)), 0)

	for range 2 {
		connect.AssertEqual(t, frame.RunStepWithOptions(t.Context(), incident, StepAbuse, out, &StepOptions{Apply: true}), nil)
	}
	connect.AssertEqual(t, mechanisms.applies, []bool{false, true, true})
	restored := incident.RowsOfKind(ListKindAbuseRestored)
	connect.AssertEqual(t, len(restored), 2)
	// mail order: the kill, then its lift; the action from before the dump and the expired one are not re-applied
	connect.AssertEqual(t, restored[0].RowId, killId.String())
	connect.AssertEqual(t, restored[0].Detail, "label_kill applied")
	connect.AssertEqual(t, restored[1].RowId, liftId.String())
	connect.AssertEqual(t, restored[1].Detail, "lift applied")
	refused := incident.RowsOfKind(ListKindAbuseRefused)
	connect.AssertEqual(t, len(refused), 1)
	connect.AssertEqual(t, refused[0].RowId, "3")
	connect.AssertEqual(t, len(incident.RowsOfKind(ListKindLegalHoldRestored)), 1)
	connect.AssertEqual(t, incident.StepEnded(StepAbuse), true)
	connect.AssertEqual(t, mechanisms.siteLost, []bool{false, false, false})
}

// After a site loss the holds the abuse step re-places are void, and the list says so.
func TestAbuseStepMarksHoldsVoidAfterASiteLoss(t *testing.T) {
	mechanisms, _, _ := newTestAbuseMechanisms()
	incident := testIncident(t, IncidentKindSite)
	run := &StepRun{Incident: incident, Out: &bytes.Buffer{}, Apply: true}
	connect.AssertEqual(t, RunAbuseStep(t.Context(), mechanisms.deps(), run), nil)
	holds := incident.RowsOfKind(ListKindLegalHoldRestored)
	connect.AssertEqual(t, len(holds), 1)
	connect.AssertEqual(t, strings.HasSuffix(holds[0].Detail, "void: the site took what it kept"), true)
}

// The abuse step reads both exports and the seal key from the operator's inputs, and names the one missing.
func TestAbuseStepNamesTheInputItLacks(t *testing.T) {
	incident := testIncident(t, IncidentKindPostgres)
	deps := &AbuseStepDeps{
		TablePresent: func(ctx context.Context, table string) (bool, error) { return true, nil },
		Linked:       func() bool { return true },
		SkipMarker:   true,
	}
	run := &StepRun{Incident: incident, Out: &bytes.Buffer{}, Apply: true, Inputs: map[string]string{StepInputAbuseMailbox: "/nonexistent/abuse.mbox"}}
	err := RunAbuseStep(t.Context(), deps, run)
	connect.AssertEqual(t, errors.Is(err, ErrStepInput), true)
	connect.AssertEqual(t, strings.Contains(err.Error(), StepInputLegalMailbox), true)
	run.Inputs[StepInputLegalMailbox] = "/nonexistent/legal.mbox"
	err = RunAbuseStep(t.Context(), deps, run)
	connect.AssertEqual(t, errors.Is(err, ErrStepInput), true)
	connect.AssertEqual(t, strings.Contains(err.Error(), StepInputMailboxKey), true)
}

// A database whose dumps predate MG5 holds no abuse or dns table: each step lists that and ends. One that holds
// them under a binary that does not link MG5 is refused, changing nothing.
func TestM5StepsSkipADatabaseBeforeMg5AndRefuseAnUnlinkedBinary(t *testing.T) {
	absent := func(ctx context.Context, table string) (bool, error) { return false, nil }
	present := func(ctx context.Context, table string) (bool, error) { return true, nil }
	unlinked := func() bool { return false }

	incident := testIncident(t, IncidentKindPostgres)
	run := &StepRun{Incident: incident, Out: &bytes.Buffer{}, Apply: true}
	connect.AssertEqual(t, RunAbuseStep(t.Context(), &AbuseStepDeps{TablePresent: absent, SkipMarker: true}, run), nil)
	connect.AssertEqual(t, RunDnsStep(t.Context(), &DnsStepDeps{TablePresent: absent, SkipMarker: true}, run), nil)
	connect.AssertEqual(t, RunLabelsStep(t.Context(), &LabelsStepDeps{TablePresent: absent, SkipMarker: true}, run), nil)
	skipped := incident.RowsOfKind(ListKindM5StepSkipped)
	connect.AssertEqual(t, len(skipped), 3)

	runner := &testDnsRestoreRunner{}
	replays := &testCtReplays{}
	mechanisms, _, _ := newTestAbuseMechanisms()
	abuseDeps := mechanisms.deps()
	abuseDeps.TablePresent, abuseDeps.Linked = present, unlinked
	dnsDeps := runner.deps("ur.example.")
	dnsDeps.Linked = unlinked
	labelsDeps := replays.deps("ur.example.")
	labelsDeps.Linked = unlinked
	connect.AssertEqual(t, errors.Is(RunAbuseStep(t.Context(), abuseDeps, run), ErrM5Unlinked), true)
	connect.AssertEqual(t, errors.Is(RunDnsStep(t.Context(), dnsDeps, run), ErrM5Unlinked), true)
	connect.AssertEqual(t, errors.Is(RunLabelsStep(t.Context(), labelsDeps, run), ErrM5Unlinked), true)
	connect.AssertEqual(t, len(mechanisms.applies), 0)
	connect.AssertEqual(t, len(runner.calls), 0)
	connect.AssertEqual(t, len(replays.scheduled), 0)
}

// The dns step runs §12.6's step for every root dns.yml serves under the incident's id, ends only once each has,
// and lists each root once, however often it runs.
func TestDnsStepRunsEachRootToItsEndUnderTheIncident(t *testing.T) {
	runner := &testDnsRestoreRunner{}
	incident := testIncident(t, IncidentKindPostgres)
	run := &StepRun{Incident: incident, Out: &bytes.Buffer{}}
	deps := runner.deps("ur.example.", "ur.example.net.")

	connect.AssertEqual(t, RunDnsStep(t.Context(), deps, run) != nil, true)
	connect.AssertEqual(t, len(incident.RowsOfKind(ListKindDnsRestored)), 0)
	connect.AssertEqual(t, RunDnsStep(t.Context(), deps, run) != nil, true)
	connect.AssertEqual(t, len(incident.RowsOfKind(ListKindDnsRestored)), 1)
	connect.AssertEqual(t, RunDnsStep(t.Context(), deps, run), nil)
	connect.AssertEqual(t, RunDnsStep(t.Context(), deps, run), nil)
	rows := incident.RowsOfKind(ListKindDnsRestored)
	connect.AssertEqual(t, len(rows), 2)
	connect.AssertEqual(t, rows[0].RowId, "ur.example.")
	connect.AssertEqual(t, rows[1].RowId, "ur.example.net.")
	connect.AssertEqual(t, rows[0].Detail, "restore serial 2026111700, 1 NS stopped, 1 diffs")
	for _, incidentId := range runner.incidents {
		connect.AssertEqual(t, incidentId, sidecar.Id(incident.IncidentId()))
	}
}

// The labels step schedules one CT replay a root, under the incident, reading from the sidecar dump's time, which
// the replay reads back 7 d from; a rerun schedules again, which the task's run-once key absorbs, and lists nothing
// twice.
func TestLabelsStepSchedulesEachRootsReplayFromTheSidecarDump(t *testing.T) {
	replays := &testCtReplays{}
	incident := testIncident(t, IncidentKindPostgres)
	run := &StepRun{Incident: incident, Out: &bytes.Buffer{}}
	deps := replays.deps("ur.example.")
	connect.AssertEqual(t, RunLabelsStep(t.Context(), deps, run), nil)
	connect.AssertEqual(t, RunLabelsStep(t.Context(), deps, run), nil)
	connect.AssertEqual(t, len(replays.scheduled), 2)
	args := replays.scheduled[0]
	connect.AssertEqual(t, args.Root, "ur.example.")
	connect.AssertEqual(t, args.IncidentId, incident.IncidentId())
	connect.AssertEqual(t, args.DumpTime.Equal(testSidecarDumpTime), true)
	rows := incident.RowsOfKind(ListKindCtReplayScheduled)
	connect.AssertEqual(t, len(rows), 1)
	connect.AssertEqual(t, rows[0].RowId, "ur.example.")
}
