// The global queue command has one explicit action and a finite result envelope.
package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server/taskworker/work"
)

func TestContractExpiryQueueCommandUsesNormalOwnerKickoff(t *testing.T) {
	opts, err := docopt.ParseArgs(bringyourctlUsage, []string{"contracts", "queue-expiry"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if queued, _ := opts.Bool("queue-expiry"); !queued {
		t.Fatal("global recovery command did not parse")
	}
	var output bytes.Buffer
	called := 0
	code := runContractExpiryRecovery(t.Context(), &output, func(ctx context.Context) (work.ContractExpiryRecoveryResult, error) {
		called++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > contractExpiryRepairCommandBudget || time.Until(deadline) <= 0 {
			t.Fatal("queue publication escaped its bounded command context")
		}
		return work.ContractExpiryRecoveryResult{RequestedAt: time.Now().UTC(), SweepRequests: 1, LegacyDispatcherRequests: 16}, nil
	})
	if code != 0 || called != 1 || !strings.Contains(output.String(), `"status":"queued"`) ||
		!strings.Contains(output.String(), `"sweep_requests":1`) || !strings.Contains(output.String(), `"legacy_dispatcher_requests":16`) {
		t.Fatal("command did not acknowledge exactly the committed queue request")
	}
}

func TestContractExpiryQueueFailureCannotAcknowledgeWork(t *testing.T) {
	for _, panicFailure := range []bool{false, true} {
		var output bytes.Buffer
		code := runContractExpiryRecovery(t.Context(), &output, func(context.Context) (work.ContractExpiryRecoveryResult, error) {
			if panicFailure {
				panic("synthetic-sensitive-database-error")
			}
			return work.ContractExpiryRecoveryResult{SweepRequests: 1}, errors.New("synthetic-sensitive-database-error")
		})
		if code != 1 || strings.Contains(output.String(), "synthetic-sensitive") || !strings.Contains(output.String(), `"request":null`) ||
			!strings.Contains(output.String(), `"status":"queue_failed"`) {
			t.Fatal("failed queue publication escaped as an acknowledgement or raw error")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var output bytes.Buffer
	called := false
	if runContractExpiryRecovery(ctx, &output, func(context.Context) (work.ContractExpiryRecoveryResult, error) {
		called = true
		return work.ContractExpiryRecoveryResult{}, nil
	}) != 1 || called || !strings.Contains(output.String(), `"status":"parent_canceled"`) {
		t.Fatal("already-canceled global kickoff reached the queue")
	}
}
