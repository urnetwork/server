// The historical command now runs synchronous, verified contract reconciliation.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// An omitted limit is unbounded; an explicitly empty or invalid bound is an
// error, so an operator's malformed bounded command cannot close everything.
func synchronousContractClosureLimit(opts docopt.Opts) (int, error) {
	if opts["--limit"] == nil {
		return 0, nil
	}
	limit, err := opts.Int("--limit")
	if err != nil || limit < 0 {
		return 0, fmt.Errorf("contract scan limit must be a nonnegative integer")
	}
	return limit, nil
}

// Stream one acknowledged result per contract and retain partial counts on
// cancellation. Failed attempts are distinct from verified terminal outcomes.
func runSynchronousContractClosures(parent context.Context, writer io.Writer, limit int,
	scan func(context.Context, model.ContractClosureScanOptions, func(*model.ContractDeadlineReconciliation, error) error) (*model.ContractClosureScanResult, error),
) (exitCode int) {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	encoder := json.NewEncoder(writer)
	status := "scan_failed"
	var result *model.ContractClosureScanResult
	defer func() {
		if recover() != nil {
			exitCode = 1
		}
		if ctx.Err() != nil {
			status = "canceled"
		}
		if err := encoder.Encode(struct {
			Kind   string                           `json:"kind"`
			Status string                           `json:"status"`
			Result *model.ContractClosureScanResult `json:"result,omitempty"`
		}{Kind: "contract-synchronous-closure-scan-v1", Status: status, Result: result}); err != nil {
			exitCode = 1
		}
	}()
	if ctx.Err() != nil {
		return 1
	}
	var err error
	result, err = scan(ctx, model.ContractClosureScanOptions{At: server.NowUtc(), PageSize: 1024, Limit: limit}, func(closed *model.ContractDeadlineReconciliation, closeErr error) error {
		if closed == nil && closeErr == nil {
			return fmt.Errorf("contract close returned no verified result")
		}
		state := "closed"
		message := ""
		if closeErr != nil {
			state = "failed"
			message = closeErr.Error()
		} else if closed.Missing {
			state = "missing"
		} else if closed.AlreadyClosed {
			state = "already_closed"
		}
		return encoder.Encode(struct {
			Kind   string                                `json:"kind"`
			Status string                                `json:"status"`
			Result *model.ContractDeadlineReconciliation `json:"result"`
			Error  string                                `json:"error,omitempty"`
		}{Kind: "contract-synchronous-closure-v1", Status: state, Result: closed, Error: message})
	})
	if err != nil || ctx.Err() != nil || result == nil || result.Failed > 0 {
		return 1
	}
	status = "scan_completed"
	return 0
}
