// `sidecarctl compute adjust` with its env and ledger calls replaced (BAL-M12-14): each of its three forms, a charge's
// refund, a support credit or debit, and a dispute's decision, hands the ledger what its line says in exact nano-USD,
// and a line it cannot take is refused before an env is opened.

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// What the replaced ledger calls were handed.
type computeAdjustCalls struct {
	refunds   []*sidecarcontroller.ComputeRefundChargeRequest
	adjusts   []*sidecarcontroller.ComputeAdjustRequest
	decisions []bool
	opened    int
}

// The command over recording ledger calls, opened only for env local.
func computeAdjustTestCommands(calls *computeAdjustCalls) *sidecarctlCommandSet {
	settings := defaultComputeLedgerCommandSettings()
	settings.openEnv = func(ctx context.Context, env string) (operator.Env, error) {
		calls.opened += 1
		if env != "local" {
			return nil, errors.New("not this process's env")
		}
		return nil, nil
	}
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	settings.refundCharge = func(env operator.Env, ctx context.Context, ledger *model.ComputeLedgerSettings, request *sidecarcontroller.ComputeRefundChargeRequest) (*sidecarcontroller.ComputeRefundChargeAnswer, error) {
		calls.refunds = append(calls.refunds, request)
		amount := request.AmountNanoUsd
		if amount == 0 {
			amount = 4 * model.ComputeCreditNanoUsd
		}
		return &sidecarcontroller.ComputeRefundChargeAnswer{
			Charge: &model.ComputeCharge{ChargeId: request.ChargeId, Kind: model.ComputeChargeKindInference},
			Write:  &sidecarcontroller.ComputeWriteAnswer{Written: true, StartTime: now, Outcome: &sidecarcontroller.ComputeOutcome{AmountNanoUsd: -amount}},
		}, nil
	}
	settings.adjust = func(env operator.Env, ctx context.Context, ledger *model.ComputeLedgerSettings, request *sidecarcontroller.ComputeAdjustRequest) (*sidecarcontroller.ComputeWriteAnswer, error) {
		calls.adjusts = append(calls.adjusts, request)
		return &sidecarcontroller.ComputeWriteAnswer{Written: true, StartTime: now, Outcome: &sidecarcontroller.ComputeOutcome{AmountNanoUsd: request.AmountNanoUsd}}, nil
	}
	settings.decideDispute = func(env operator.Env, ctx context.Context, ledger *model.ComputeLedgerSettings, requestId operator.Id, upheld bool) (*sidecarcontroller.ComputeDecideAnswer, error) {
		calls.decisions = append(calls.decisions, upheld)
		state := model.ComputeDisputeStateRejected
		if upheld {
			state = model.ComputeDisputeStateUpheld
		}
		return &sidecarcontroller.ComputeDecideAnswer{Dispute: &model.ComputeDispute{RequestId: requestId, State: state}, Decided: true}, nil
	}
	commands := newSidecarctlCommandSet()
	registered, _ := sidecarctlCommands.find(strings.Fields("compute adjust"))
	command := *registered
	command.run = func(args []string, out io.Writer) error {
		return runComputeAdjustWithSettings(args, settings, out)
	}
	commands.add(&command)
	return commands
}

// Each form hands the ledger what its line says: a charge's refund, whole or in part with its cost back; a credit
// negative and a debit positive, with the case and its number; a dispute upheld or rejected.
func TestSidecarctlAdjustParsesEachForm(t *testing.T) {
	calls := &computeAdjustCalls{}
	commands := computeAdjustTestCommands(calls)
	chargeId, networkId, caseId, requestId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	run := func(args ...string) string {
		t.Helper()
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		if code := commands.run(append([]string{"compute", "adjust", "local"}, args...), stdout, stderr); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, stderr.String())
		}
		return stdout.String()
	}
	out := run("--refund="+chargeId.String(), "--amount=1.50", "--cost-back=0.25", "--by=operator.example", "--reason=upheld by hand")
	if len(calls.refunds) != 1 || calls.refunds[0].ChargeId != chargeId || calls.refunds[0].AmountNanoUsd != 1_500_000_000 ||
		calls.refunds[0].CostBackNanoUsd != 250_000_000 || calls.refunds[0].NetworkId != nil || !strings.Contains(out, "refunded $1.50") {
		t.Fatalf("a charge's refund: %+v %s", calls.refunds, out)
	}
	run("--refund="+chargeId.String(), "--network="+networkId.String(), "--by=operator.example", "--reason=all of it")
	if refund := calls.refunds[1]; refund.AmountNanoUsd != 0 || refund.NetworkId == nil || *refund.NetworkId != networkId {
		t.Fatalf("a whole refund: %+v", refund)
	}
	out = run("--credit=5", "--network="+networkId.String(), "--case="+caseId.String(), "--by=operator.example", "--reason=outage credit")
	if adjust := calls.adjusts[0]; adjust.AmountNanoUsd != -5*model.ComputeCreditNanoUsd || adjust.CaseId != caseId || adjust.Number != 1 ||
		adjust.NetworkId != networkId || !strings.Contains(out, "credited $5.00") {
		t.Fatalf("a credit: %+v %s", adjust, out)
	}
	run("--debit=2.10", "--network="+networkId.String(), "--case="+caseId.String(), "--number=2", "--by=operator.example", "--reason=reversed credit")
	if adjust := calls.adjusts[1]; adjust.AmountNanoUsd != 2_100_000_000 || adjust.Number != 2 {
		t.Fatalf("a debit: %+v", adjust)
	}
	out = run("--dispute="+requestId.String(), "--uphold", "--by=operator.example", "--reason=usage overcounted")
	if len(calls.decisions) != 1 || !calls.decisions[0] || !strings.Contains(out, "upheld") {
		t.Fatalf("a dispute: %v %s", calls.decisions, out)
	}
	run("--dispute="+requestId.String(), "--reject", "--by=operator.example", "--reason=usage matched")
	if len(calls.decisions) != 2 || calls.decisions[1] {
		t.Fatalf("a rejection: %v", calls.decisions)
	}
}

// A line the command cannot take is refused before an env is opened.
func TestSidecarctlAdjustRefusesBeforeTheEnv(t *testing.T) {
	calls := &computeAdjustCalls{}
	commands := computeAdjustTestCommands(calls)
	id := server.NewId().String()
	for _, args := range [][]string{
		{"local", "--by=x", "--reason=y"},
		{"local", "--refund=" + id, "--credit=5", "--by=x", "--reason=y"},
		{"local", "--refund=" + id, "--by=x"},
		{"local", "--refund=not-an-id", "--by=x", "--reason=y"},
		{"local", "--refund=" + id, "--amount=-1", "--by=x", "--reason=y"},
		{"local", "--credit=5", "--case=" + id, "--by=x", "--reason=y"},
		{"local", "--credit=5", "--network=" + id, "--by=x", "--reason=y"},
		{"local", "--debit=0", "--network=" + id, "--case=" + id, "--by=x", "--reason=y"},
		{"local", "--credit=5", "--network=" + id, "--case=" + id, "--number=0", "--by=x", "--reason=y"},
		{"local", "--dispute=" + id, "--by=x", "--reason=y"},
		{"local", "--dispute=" + id, "--uphold", "--reject", "--by=x", "--reason=y"},
		{"not/an/env", "--refund=" + id, "--by=x", "--reason=y"},
	} {
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		if code := commands.run(append([]string{"compute", "adjust"}, args...), stdout, stderr); code == 0 {
			t.Errorf("%v: accepted", args)
		}
	}
	if calls.opened != 0 || len(calls.refunds)+len(calls.adjusts)+len(calls.decisions) != 0 {
		t.Fatalf("a refused line opened %d envs", calls.opened)
	}
}
