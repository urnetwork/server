// Retires one payer network's due contracts now, through the payer drain that
// the payer's refused admission or stranded Pending requests on its own, and
// that the stranded-grant pass runs for payers of grants that ended with escrow
// unsettled. The command is a read-only dry run unless --commit: the dry run
// reports the contracts due by its bound and their digest, and --commit
// retires exactly the contracts due by the same bound when the digest is still
// the one the dry run printed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The drain's reads and runs; tests replace them.
type payerDueContractCloseOps struct {
	summarize func(ctx context.Context, payerNetworkId server.Id, dueBefore time.Time) (*model.PayerDueContractSummary, error)
	digest    func(ctx context.Context, payerNetworkId server.Id, dueBefore time.Time) (int, string, error)
	close     func(ctx context.Context, payerNetworkId server.Id, now time.Time, more func() bool,
		settings *model.PayerDueContractCloseSettings) (*model.PayerDueContractClose, error)
}

func defaultPayerDueContractCloseOps() payerDueContractCloseOps {
	return payerDueContractCloseOps{
		summarize: model.SummarizePayerDueContracts,
		digest:    model.DigestPayerDueContracts,
		close:     model.ClosePayerDueContracts,
	}
}

// The dry run's bound when --before is omitted: the drain's own, in whole
// seconds so that the commit line repeats it exactly.
func defaultPayerDueContractCloseBefore(now time.Time) time.Time {
	return now.Add(-model.DefaultPayerDueContractCloseSettings().DueMargin).UTC().Truncate(time.Second)
}

// The parsed command line of contracts close-due: the bound, and whether and
// what it commits. A commit needs the bound and the digest of the dry run it
// commits; --dry-run, kept for earlier command lines, is the default.
func parsePayerDueContractCloseFlags(opts docopt.Opts, now time.Time) (before time.Time, commit bool, expect string, err error) {
	commit, _ = opts.Bool("--commit")
	expect, _ = opts.String("--expect")
	dryRun, _ := opts.Bool("--dry-run")
	if commit && dryRun {
		return time.Time{}, false, "", errors.New("--dry-run and --commit exclude each other")
	}
	beforeValue, _ := opts.String("--before")
	if beforeValue == "" {
		if commit {
			return time.Time{}, false, "", errors.New("--commit needs the --before its dry run printed")
		}
		return defaultPayerDueContractCloseBefore(now), false, "", nil
	}
	before, err = time.Parse(time.RFC3339Nano, beforeValue)
	if err != nil {
		return time.Time{}, false, "", fmt.Errorf("--before: %w", err)
	}
	if commit && expect == "" {
		return time.Time{}, false, "", errors.New("--commit needs the --expect digest its dry run printed")
	}
	return before.UTC(), commit, expect, nil
}

func contractsCloseDueCommand(opts docopt.Opts, payerNetworkId server.Id) int {
	before, commit, expect, err := parsePayerDueContractCloseFlags(opts, server.NowUtc())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		return 1
	}
	if !commit {
		// The dry run writes nothing to the database: its process is read
		// only, so PostgreSQL refuses any write on its path.
		server.RequirePgReadOnly()
	}
	return runPayerDueContractClose(context.Background(), os.Stdout, payerNetworkId, before, commit, expect, defaultPayerDueContractCloseOps())
}

// Writes one JSON line for the contracts due by before, with their digest. A
// dry run then prints the command that commits exactly them. A commit whose
// digest differs from expect changes nothing and exits 1; otherwise it writes
// one line per drain run and one for the contracts left due by before. Runs
// continue while one reports more and closed something; a run that only met
// held owners waits the drain's busy delay once before the next. A failed run
// or an interrupt ends the command with exit code 1; committed runs stay
// committed.
func runPayerDueContractClose(parent context.Context, writer io.Writer, payerNetworkId server.Id, before time.Time,
	commit bool, expect string, ops payerDueContractCloseOps) (exitCode int) {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	settings := model.DefaultPayerDueContractCloseSettings()
	encoder := json.NewEncoder(writer)
	type summaryLine struct {
		Kind           string          `json:"kind"`
		NetworkId      server.Id       `json:"network_id"`
		DryRun         bool            `json:"dry_run"`
		Before         time.Time       `json:"before"`
		Due            int             `json:"due"`
		ByteCount      model.ByteCount `json:"byte_count"`
		OldestDeadline *time.Time      `json:"oldest_deadline,omitempty"`
		NewestDeadline *time.Time      `json:"newest_deadline,omitempty"`
		Digest         string          `json:"digest,omitempty"`
		Error          string          `json:"error,omitempty"`
	}
	summarize := func(ctx context.Context, kind string, withDigest bool) (line summaryLine, ok bool) {
		line = summaryLine{Kind: kind, NetworkId: payerNetworkId, DryRun: !commit, Before: before}
		summary, err := ops.summarize(ctx, payerNetworkId, before)
		if err == nil {
			line.Due, line.ByteCount, line.OldestDeadline, line.NewestDeadline = summary.Count, summary.ByteCount, summary.OldestDeadline, summary.NewestDeadline
			if withDigest {
				_, line.Digest, err = ops.digest(ctx, payerNetworkId, before)
			}
		}
		if err != nil {
			line.Error = err.Error()
		}
		return line, err == nil
	}

	line, ok := summarize(ctx, "payer-due-contract-summary-v2", true)
	if ok && commit && line.Digest != expect {
		line.Error = fmt.Sprintf("the contracts due by %s are no longer the ones the dry run reported (digest %s, expected %s); run the dry run again",
			before.Format(time.RFC3339), line.Digest, expect)
		ok = false
	}
	if encoder.Encode(line) != nil || !ok {
		return 1
	}
	if !commit {
		if 0 < line.Due {
			fmt.Fprintf(writer, "to retire exactly these: bringyourctl contracts close-due --network_id=%s --before=%s --commit --expect=%s\n",
				payerNetworkId, before.Format(time.RFC3339), line.Digest)
		}
		return 0
	}

	// The drain retires the contracts due DueMargin before its now, so this
	// now bounds it at before.
	drainNow := before.Add(settings.DueMargin)
	waitedBusy := false
	for ctx.Err() == nil {
		started := time.Now()
		result, err := ops.close(ctx, payerNetworkId, drainNow, func() bool {
			return ctx.Err() == nil && time.Since(started) < settings.RunBudget
		}, settings)
		runLine := struct {
			Kind   string                       `json:"kind"`
			Result *model.PayerDueContractClose `json:"result,omitempty"`
			Error  string                       `json:"error,omitempty"`
		}{Kind: "payer-due-contract-close-run-v1", Result: result}
		if err != nil {
			runLine.Error = err.Error()
		}
		if encoder.Encode(runLine) != nil || err != nil || result == nil {
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
	left, ok := summarize(leftCtx, "payer-due-contract-left-v1", false)
	if encoder.Encode(left) != nil || !ok {
		exitCode = 1
	}
	return
}
