package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Fake drain runs in order, and the due contracts each summary reports.
type fakePayerDueContractClose struct {
	summaries []int
	runs      []*model.PayerDueContractClose
	errs      []error
	closes    int
}

func (self *fakePayerDueContractClose) ops() payerDueContractCloseOps {
	return payerDueContractCloseOps{
		summarize: func(ctx context.Context, payerNetworkId server.Id, dueBefore time.Time) (*model.PayerDueContractSummary, error) {
			count := self.summaries[0]
			self.summaries = self.summaries[1:]
			return &model.PayerDueContractSummary{Count: count, ByteCount: model.ByteCount(count) * model.Mib}, nil
		},
		close: func(ctx context.Context, payerNetworkId server.Id, now time.Time, more func() bool,
			settings *model.PayerDueContractCloseSettings) (*model.PayerDueContractClose, error) {
			index := self.closes
			self.closes++
			return self.runs[index], self.errs[index]
		},
	}
}

func payerDueContractCloseLines(t *testing.T, output string) []map[string]any {
	t.Helper()
	lines := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatal("output line is not JSON", line, err)
		}
		lines = append(lines, value)
	}
	return lines
}

// A dry run reports the payer's due contracts and runs no drain.
func TestPayerDueContractCloseDryRunClosesNothing(t *testing.T) {
	fake := &fakePayerDueContractClose{summaries: []int{27}}
	var output bytes.Buffer
	if exitCode := runPayerDueContractClose(t.Context(), &output, server.NewId(), true, fake.ops()); exitCode != 0 {
		t.Fatal("dry run failed", exitCode, output.String())
	}
	lines := payerDueContractCloseLines(t, output.String())
	if fake.closes != 0 || len(lines) != 1 || lines[0]["kind"] != "payer-due-contract-summary-v1" || lines[0]["due"] != float64(27) || lines[0]["dry_run"] != true {
		t.Fatal("dry run did not only report", fake.closes, output.String())
	}
}

// Runs continue while one closes something and reports more, then the command
// reports what is left. A failed run ends the command with exit code 1.
func TestPayerDueContractCloseRunsWhileTheDrainReportsMore(t *testing.T) {
	fake := &fakePayerDueContractClose{
		summaries: []int{5000, 0},
		runs: []*model.PayerDueContractClose{
			{Due: 4096, Closed: 4096, More: true},
			{Due: 904, Closed: 904},
		},
		errs: []error{nil, nil},
	}
	var output bytes.Buffer
	if exitCode := runPayerDueContractClose(t.Context(), &output, server.NewId(), false, fake.ops()); exitCode != 0 {
		t.Fatal("drain failed", exitCode, output.String())
	}
	lines := payerDueContractCloseLines(t, output.String())
	if fake.closes != 2 || len(lines) != 4 || lines[3]["kind"] != "payer-due-contract-left-v1" || lines[3]["due"] != float64(0) {
		t.Fatal("drain did not run until no more were due", fake.closes, output.String())
	}

	failing := &fakePayerDueContractClose{
		summaries: []int{3, 3},
		runs:      []*model.PayerDueContractClose{{Due: 3, Failed: 3}},
		errs:      []error{errors.New("synthetic owner failure")},
	}
	output.Reset()
	if exitCode := runPayerDueContractClose(t.Context(), &output, server.NewId(), false, failing.ops()); exitCode != 1 {
		t.Fatal("a failed run did not fail the command", exitCode, output.String())
	}
	if lines := payerDueContractCloseLines(t, output.String()); len(lines) != 3 || lines[1]["error"] != "synthetic owner failure" {
		t.Fatal("a failed run was not reported", output.String())
	}
}
