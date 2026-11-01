package controller

// st_ledger_v2_recovery_driver_test.go — the recovery component (VERSION2-FINAL.md
// §16 step 6) through the v2 driver's steps, with the real builder over the
// test database and an in-memory vault2: the alert and retry of a receiving
// root without its source rows, the commit, the finalization, the record
// step's per-epoch amounts and the source epoch's own earnings notification,
// and the root after the receiving root.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urfoundation/sn/ledger"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// ledgerV2RecoveryDriver is pool 1 with first v2 epoch 2, whose source epoch
// 1's v1 funding is recovered: the build fixture's usage and consents for
// epochs 1 to 4, an in-memory vault2, the real builder and captured alerts.
type ledgerV2RecoveryDriver struct {
	*ledgerV2BuildFixture
	p      *ledgerV2BuildProviders
	v2     *fakeLedgerV2Chain
	alerts *[]string
	log    []string
}

func newLedgerV2RecoveryDriver(t testing.TB) *ledgerV2RecoveryDriver {
	t.Helper()
	useLedgerV2RecoverySchedule(t)
	d := &ledgerV2RecoveryDriver{ledgerV2BuildFixture: newLedgerV2BuildFixture(t)}
	d.p = d.providers()
	p := d.p
	d.contract(ledgerV2BuildAuthority(1).StartTime, ledgerV2BuildUsage{p.own, p.networks[0], 100}, ledgerV2BuildUsage{p.network, p.networks[1], 300},
		ledgerV2BuildUsage{p.unmapped, p.networks[3], 50}, ledgerV2BuildUsage{p.head, p.networks[4], 500}, ledgerV2BuildUsage{p.shared, p.networks[4], 400})
	d.contract(ledgerV2BuildAuthority(2).StartTime, ledgerV2BuildUsage{p.own, p.networks[0], 60}, ledgerV2BuildUsage{p.delegated, p.networks[2], 40})
	d.contract(ledgerV2BuildAuthority(3).StartTime, ledgerV2BuildUsage{p.network, p.networks[1], 70}, ledgerV2BuildUsage{p.delegated, p.networks[2], 30})
	d.contract(ledgerV2BuildAuthority(4).StartTime, ledgerV2BuildUsage{p.own, p.networks[0], 25}, ledgerV2BuildUsage{p.network, p.networks[1], 75})
	d.chain.pool = &StPoolState{Status: ledger.V1EpochRootMissed, PoolTotalRao: big.NewInt(ledgerV2RecoveryAmount), ClaimedRao: big.NewInt(0)}
	d.v2 = newFakeLedgerV2Chain(&d.log)
	d.v2.firstEpoch, d.v2.lastFunded, d.v2.lastCaptured = 2, 1, 1
	SetStLedgerV2Client(d.v2)
	SetStLedgerBuilder(stLedgerV2Builder{})
	resetLedgerV2Caches()
	d.alerts = captureLedgerV2Alerts(t)
	t.Cleanup(func() {
		SetStLedgerV2Client(nil)
		SetStLedgerBuilder(nil)
		resetLedgerV2Caches()
	})
	return d
}

// mirror applies what the event mirror would after epoch's close: its funding
// snapshot, a SurplusAssigned for it when surplus is positive, and a
// checkpoint past both.
func (d *ledgerV2RecoveryDriver) mirror(epoch uint64, surplus uint64) {
	d.t.Helper()
	key := d.cfg.DeploymentKey()
	block := d.v2.end(epoch) + 5
	if surplus != 0 {
		data, err := json.Marshal(map[string]string{"no_id": "1", "e": fmt.Sprint(epoch), "amount": fmt.Sprint(surplus)})
		if err != nil {
			d.t.Fatal(err)
		}
		at := d.v2.end(epoch-1) + 500
		model.UpsertStEvents(d.ctx, key, []*model.StChainEvent{{BlockNumber: at, BlockHash: fmt.Sprintf("0x%064x", at), LogIndex: 0, TxHash: fmt.Sprintf("0x%064x", at+1), Kind: model.StLedgerV2SurplusAssignedEventKind, DataJson: string(data)}})
	}
	model.SetStLedgerV2Funding(d.ctx, key, &model.StLedgerV2Funding{NoId: 1, Epoch: epoch, BlockNumber: block})
	model.SetStChainCheckpoint(d.ctx, key, model.StChainCheckpoint{NextBlock: block + 100, BlockHash: fmt.Sprintf("0x%064x", block+99)})
}

func (d *ledgerV2RecoveryDriver) run(kind string, epoch uint64, seq uint64) *StPublishOutcome {
	d.t.Helper()
	outcome, err := StLedgerV2RunStep(d.ctx, &StLedgerV2Step{Kind: kind, Epoch: epoch, Seq: seq})
	if err != nil {
		d.t.Fatalf("%s step: %v", kind, err)
	}
	return outcome
}

// header reads a committed root's published header.
func (d *ledgerV2RecoveryDriver) header(ledgerHash [32]byte) *ledger.Header {
	d.t.Helper()
	data, err := GetStLedgerV2Object(d.ctx, ledger.Ref(ledgerHash).String())
	if err != nil {
		d.t.Fatal(err)
	}
	header, err := ledger.DecodeHeader(data)
	if err != nil {
		d.t.Fatal(err)
	}
	return header
}

func (d *ledgerV2RecoveryDriver) recordSource() {
	d.t.Helper()
	usage, err := BuildStLedgerV2RecoveryUsage(d.ctx, 1)
	if err != nil {
		d.t.Fatal(err)
	}
	dir := filepath.Join(d.t.TempDir(), "usage")
	if err := WriteStLedgerV2RecoveryUsage(usage, dir); err != nil {
		d.t.Fatal(err)
	}
	if _, err := StLedgerV2RecordRecoveryRows(d.ctx, 1, usage.Ref, dir, "recorder@example"); err != nil {
		d.t.Fatal(err)
	}
}

func sumLedgerV2Amounts(amounts []*model.StLedgerV2Amount) uint64 {
	var total uint64
	for _, amount := range amounts {
		total += amount.Amount.Uint64()
	}
	return total
}

// The receiving root through the driver. Without epoch 1's recorded rows the
// epoch step alerts and retries and commits nothing; once they are recorded
// it commits the root with the recovery. The root finalizes; its record step
// writes epoch 1's amounts, which sum exactly to the recovery, and epoch 3's
// ordinary amounts, f_3 − R, and claims epoch 1's recovery notification even
// though v1's finalization claimed the epoch's own. The root after it carries
// no recovery and reads the receiving root's header back as its previous.
func TestStLedgerV2DriverCommitsAndRecordsTheRecovery(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		d := newLedgerV2RecoveryDriver(t)
		key := d.cfg.DeploymentKey()
		captureLedgerV2Emails(t)
		d.v2.emission[2] = 1_000_000
		d.v2.head = d.v2.end(2) + 10
		if _, err := d.v2.LedgerV2Close(d.ctx, 2, 1); err != nil {
			t.Fatal(err)
		}
		d.mirror(2, 0)
		d.v2.emission[3] = ledgerV2RecoveryCapture + ledgerV2RecoveryAmount + ledgerV2RecoveryOrdinary
		d.mirror(3, ledgerV2RecoveryAmount+ledgerV2RecoveryOrdinary)
		d.v2.head = d.v2.end(3) + 10

		// No recorded source rows: an alert, a retry, no commit.
		requireLedgerV2Outcome(t, d.run(StLedgerV2StepEpoch, 3, 0), model.StPublishStatusFailed, true, ErrStLedgerV2RecoveryRows.Error())
		if d.v2.pending != nil || !slices.Contains(*d.alerts, StLedgerV2AlertRecoveryRows+" epoch 3") {
			t.Fatalf("pending %+v, alerts %v", d.v2.pending, *d.alerts)
		}

		d.recordSource()
		requireLedgerV2Outcome(t, d.run(StLedgerV2StepEpoch, 3, 0), model.StPublishStatusConfirmed, false, "")
		pending := d.v2.pending
		if pending == nil || pending.Epoch != 3 {
			t.Fatalf("pending %+v", pending)
		}
		header := d.header(pending.LedgerHash)
		if len(header.Epochs) != 2 || len(header.Epochs[0].Recovery) != 0 || len(header.Epochs[1].Recovery) != 1 ||
			header.Epochs[1].Recovery[0].Epoch != 1 || header.Epochs[1].Recovery[0].Amount != ledgerV2RecoveryAmount {
			t.Fatalf("header epochs %+v", header.Epochs)
		}

		d.v2.approvals = 1
		d.v2.head = pending.CommittedBlock + d.v2.challenge
		requireLedgerV2Outcome(t, d.run(StLedgerV2StepFinalize, 3, 0), model.StPublishStatusConfirmed, false, "")
		root := &model.StLedgerV2Root{NoId: 1, Seq: pending.Seq, Epoch: 3, Root: pending.Root, Total: pending.Total, LeafCount: pending.LeafCount, LedgerHash: pending.LedgerHash}
		model.AddStLedgerV2Commit(d.ctx, key, root, pending.CommittedBlock, 0)
		zero := uint64(0)
		root.RootIndex = &zero
		model.SetStLedgerV2RootFinalized(d.ctx, key, root, d.v2.head)
		// v1 finalized root-missed epoch 1 and claimed its own notification.
		if !model.ClaimStEpochNotification(d.ctx, key, 1) {
			t.Fatal("epoch 1's notification was claimed early")
		}
		requireLedgerV2Outcome(t, d.run(StLedgerV2StepRecord, 3, pending.Seq), model.StPublishStatusConfirmed, false, "root 0 recorded")
		if got := sumLedgerV2Amounts(model.GetStLedgerV2Amounts(d.ctx, key, 1, 1)); got != ledgerV2RecoveryAmount {
			t.Fatalf("epoch 1 amounts sum to %d", got)
		}
		if got := sumLedgerV2Amounts(model.GetStLedgerV2Amounts(d.ctx, key, 1, 3)); got != ledgerV2RecoveryCapture+ledgerV2RecoveryOrdinary {
			t.Fatalf("epoch 3 amounts sum to %d", got)
		}
		if got := sumLedgerV2Amounts(model.GetStLedgerV2Amounts(d.ctx, key, 1, 2)); got != 1_000_000 {
			t.Fatalf("epoch 2 amounts sum to %d", got)
		}
		if model.ClaimStRecoveryNotification(d.ctx, key, 1) {
			t.Fatal("the record step did not claim epoch 1's recovery notification")
		}

		// The root after the receiving root.
		d.v2.emission[4] = 900_000
		d.v2.head = d.v2.end(4) + 10
		d.mirror(4, 0)
		requireLedgerV2Outcome(t, d.run(StLedgerV2StepEpoch, 4, 0), model.StPublishStatusConfirmed, false, "")
		next := d.v2.pending
		if next == nil || next.Epoch != 4 {
			t.Fatalf("pending %+v", next)
		}
		if header := d.header(next.LedgerHash); len(header.Epochs) != 1 || len(header.Epochs[0].Recovery) != 0 {
			t.Fatalf("the root after carries %+v", header.Epochs)
		}
		for _, alert := range *d.alerts {
			if strings.HasPrefix(alert, StLedgerV2AlertRecoveryIncomplete) {
				t.Fatalf("alert %s", alert)
			}
		}
	})
}

// A repeat of the record command with its usage returns the recorded rows:
// it checks the census against the record and selects no wallets and reads
// no heads again, so it answers while the chain cannot. An epoch declared
// unrecoverable refuses it.
func TestStLedgerV2RecoveryRecordRepeatsSafely(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		s := newLedgerV2RecoveryScenario(t, true)
		// rows built now would fail at the head read
		s.chain.bindingsErr = errors.New("bindingAt unavailable")
		again, err := StLedgerV2RecordRecoveryRows(s.ctx, 1, s.usage.Ref, s.dir, "recorder@example")
		if err != nil {
			t.Fatal(err)
		}
		if !again.AlreadyRecorded || !slices.Equal(again.ProviderChunks, s.record.ProviderChunks) || again.Rows != s.record.Rows || again.EligibleRows != s.record.EligibleRows || again.Coldkeys != s.record.Coldkeys {
			t.Fatalf("repeat %+v, first %+v", again, s.record)
		}
		if record := s.sourceRecord(); !slices.Equal(record.ProviderChunks, s.record.ProviderChunks) {
			t.Fatalf("the repeat changed the record: %+v", record)
		}
		// the repeat still reads its artifact
		if _, err := StLedgerV2RecordRecoveryRows(s.ctx, 1, "sha256:"+strings.Repeat("0", 64), filepath.Join(t.TempDir(), "missing"), "recorder@example"); err == nil {
			t.Fatal("a repeat without its artifact was answered")
		}
		s.chain.bindingsErr = nil
		if err := model.DeclareStLedgerV2EpochUnrecoverable(s.ctx, s.cfg.DeploymentKey(), 1, 9, "operator@example"); err != nil {
			t.Fatal(err)
		}
		stLedgerV2RecoverySchedule = func(vault [20]byte, noID uint64) *ledger.RecoverySchedule {
			return &ledger.RecoverySchedule{Vault: vault, NoID: noID, Sources: []ledger.Recovery{{Epoch: 1, Amount: ledgerV2RecoveryAmount}, {Epoch: 9, Amount: 1}}, FromBlock: 1, ThroughEpoch: 20}
		}
		if _, err := StLedgerV2RecordRecoveryRows(s.ctx, 9, s.usage.Ref, s.dir, "recorder@example"); !errors.Is(err, ErrStLedgerV2RecoveryRefused) || !strings.Contains(err.Error(), "declared unrecoverable") {
			t.Fatalf("a declared epoch: %v", err)
		}
	})
}

// A rebuild request for a v1 epoch, whose rows are a recovery's fixed inputs,
// is refused and leaves its record as it is.
func TestStLedgerV2RebuildRefusesARecoverySource(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		s := newLedgerV2RecoveryScenario(t, true)
		chain := newFakeLedgerV2Chain(&[]string{})
		chain.firstEpoch, chain.lastFunded, chain.lastCaptured = 2, 1, 1
		SetStLedgerV2Client(chain)
		t.Cleanup(func() { SetStLedgerV2Client(nil) })
		if err := StLedgerV2RebuildEpoch(s.ctx, 1, "rebuilder@example"); !errors.Is(err, ErrStLedgerV2RebuildRefused) || !strings.Contains(err.Error(), "before the pool's first v2 epoch 2") {
			t.Fatalf("rebuild of the source epoch: %v", err)
		}
		if record := s.sourceRecord(); record.RebuildRequestedBy != "" || !slices.Equal(record.ProviderChunks, s.record.ProviderChunks) {
			t.Fatalf("record %+v", record)
		}
	})
}

// The plan raises an incomplete recovery: a covered epoch received surplus
// that left part of it unplaced, or a root passes the schedule's last epoch
// with part unplaced. A whole recovery, or no surplus yet before the last
// epoch, raises nothing.
func TestStLedgerV2RecoveryPlanAlertsAnIncompleteRecovery(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		d := newLedgerV2RecoveryDriver(t)
		// Epoch 2 received no surplus: nothing to raise.
		d.mirror(2, 0)
		if _, err := stLedgerV2RecoveryPlan(d.ctx, d.cfg, 2, 1, 2); err != nil || len(*d.alerts) != 0 {
			t.Fatalf("no surplus: %v %v", err, *d.alerts)
		}
		// Epoch 3 received less than the recovery: placed as far as it goes,
		// and raised.
		d.mirror(3, 3_000_000_000_000)
		plan, err := stLedgerV2RecoveryPlan(d.ctx, d.cfg, 2, 1, 3)
		if err != nil || plan[3][0].Amount != 3_000_000_000_000 || !slices.Contains(*d.alerts, StLedgerV2AlertRecoveryIncomplete+" epoch 3") {
			t.Fatalf("short surplus: %v %v %v", plan, err, *d.alerts)
		}
		// Epoch 4 received the rest: the root over it completes the recovery.
		d.mirror(4, ledgerV2RecoveryAmount)
		plan, err = stLedgerV2RecoveryPlan(d.ctx, d.cfg, 2, 3, 4)
		if err != nil || plan[4][0].Amount != ledgerV2RecoveryAmount-3_000_000_000_000 || slices.Contains(*d.alerts, StLedgerV2AlertRecoveryIncomplete+" epoch 4") {
			t.Fatalf("completing surplus: %v %v %v", plan, err, *d.alerts)
		}
		// A schedule whose last epoch has passed with nothing assigned.
		stLedgerV2RecoverySchedule = func(vault [20]byte, noID uint64) *ledger.RecoverySchedule {
			return &ledger.RecoverySchedule{Vault: vault, NoID: noID, Sources: []ledger.Recovery{{Epoch: 1, Amount: ledgerV2RecoveryAmount}}, FromBlock: d.v2.end(9), ThroughEpoch: 4}
		}
		if _, err := stLedgerV2RecoveryPlan(d.ctx, d.cfg, 2, 3, 4); err != nil || !slices.Contains(*d.alerts, StLedgerV2AlertRecoveryIncomplete+" epoch 4") {
			t.Fatalf("passed bound: %v %v", err, *d.alerts)
		}
	})
}

// The usage artifact builds in a read-only process: with every session
// read-only, as `st v2-recovery-usage` runs, it builds the same artifact,
// while a write in the same process fails.
func TestStLedgerV2RecoveryUsageBuildsReadOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		s := newLedgerV2RecoveryScenario(t, true)
		server.RequirePgReadOnly()
		defer server.Testing_ReleasePgReadOnly()
		usage, err := BuildStLedgerV2RecoveryUsage(s.ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if usage.Ref != s.usage.Ref {
			t.Fatalf("read-only artifact %s, earlier %s", usage.Ref, s.usage.Ref)
		}
		func() {
			defer func() { recover() }()
			model.DeclareStLedgerV2EpochUnrecoverable(s.ctx, s.cfg.DeploymentKey(), 1, 9, "operator@example")
		}()
		if model.GetStLedgerV2EpochRows(s.ctx, s.cfg.DeploymentKey(), 1, 9, 9)[9] != nil {
			t.Fatal("a write succeeded in the read-only process")
		}
	})
}
