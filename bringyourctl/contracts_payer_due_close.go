// Retires one payer network's due contracts now, through the payer drain that
// the payer's refused admission or stranded Pending requests on its own. A dry
// run reports what the drain would retire and changes nothing.
package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The drain's reads and runs; tests replace them.
type payerDueContractCloseOps struct {
	summarize func(ctx context.Context, payerNetworkId server.Id, dueBefore time.Time) (*model.PayerDueContractSummary, error)
	close     func(ctx context.Context, payerNetworkId server.Id, now time.Time, more func() bool,
		settings *model.PayerDueContractCloseSettings) (*model.PayerDueContractClose, error)
}

func defaultPayerDueContractCloseOps() payerDueContractCloseOps {
	return payerDueContractCloseOps{summarize: model.SummarizePayerDueContracts, close: model.ClosePayerDueContracts}
}

// Writes one JSON line for the due contracts before, one per drain run, and,
// unless a dry run, one for the due contracts left. Runs continue while a run
// reports more and closed something; a run that only met held owners waits
// the drain's busy delay once before the next. A failed run or an interrupt
// ends the command with exit code 1; committed runs stay committed.
func runPayerDueContractClose(parent context.Context, writer io.Writer, payerNetworkId server.Id, dryRun bool,
	ops payerDueContractCloseOps) (exitCode int) {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	settings := model.DefaultPayerDueContractCloseSettings()
	encoder := json.NewEncoder(writer)
	type summaryLine struct {
		Kind           string          `json:"kind"`
		NetworkId      server.Id       `json:"network_id"`
		DryRun         bool            `json:"dry_run"`
		Due            int             `json:"due"`
		ByteCount      model.ByteCount `json:"byte_count"`
		OldestDeadline *time.Time      `json:"oldest_deadline,omitempty"`
		NewestDeadline *time.Time      `json:"newest_deadline,omitempty"`
		Error          string          `json:"error,omitempty"`
	}
	summarize := func(ctx context.Context, kind string) bool {
		summary, err := ops.summarize(ctx, payerNetworkId, server.NowUtc().Add(-settings.DueMargin))
		line := summaryLine{Kind: kind, NetworkId: payerNetworkId, DryRun: dryRun}
		if err != nil {
			line.Error = err.Error()
		} else {
			line.Due, line.ByteCount, line.OldestDeadline, line.NewestDeadline = summary.Count, summary.ByteCount, summary.OldestDeadline, summary.NewestDeadline
		}
		return encoder.Encode(line) == nil && err == nil
	}
	if !summarize(ctx, "payer-due-contract-summary-v1") {
		return 1
	}
	if dryRun {
		return 0
	}
	waitedBusy := false
	for ctx.Err() == nil {
		started := time.Now()
		result, err := ops.close(ctx, payerNetworkId, server.NowUtc(), func() bool {
			return ctx.Err() == nil && time.Since(started) < settings.RunBudget
		}, settings)
		line := struct {
			Kind   string                       `json:"kind"`
			Result *model.PayerDueContractClose `json:"result,omitempty"`
			Error  string                       `json:"error,omitempty"`
		}{Kind: "payer-due-contract-close-run-v1", Result: result}
		if err != nil {
			line.Error = err.Error()
		}
		if encoder.Encode(line) != nil || err != nil || result == nil {
			exitCode = 1
			break
		}
		if !result.More {
			break
		}
		if result.Closed == 0 {
			if result.Busy == 0 || waitedBusy {
				break
			}
			waitedBusy = true
			select {
			case <-ctx.Done():
			case <-time.After(settings.BusyRetryDelay):
			}
			continue
		}
		waitedBusy = false
	}
	if ctx.Err() != nil {
		exitCode = 1
	}
	// An interrupt still reports what is left, within one read's bound.
	leftCtx, leftCancel := context.WithTimeout(context.WithoutCancel(ctx), settings.ReadTimeout)
	defer leftCancel()
	if !summarize(leftCtx, "payer-due-contract-left-v1") {
		exitCode = 1
	}
	return
}
