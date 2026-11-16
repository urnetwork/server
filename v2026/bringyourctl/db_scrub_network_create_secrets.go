package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// `bringyourctl db scrub-network-create-secrets` removes the sign-up
// credentials from the network-create audit rows written before the audit
// stored an allowlist of the request (model/audit_network_create_scrub.go).
// Without --apply it is a read-only dry run that counts the rows holding each
// key and prints the digest of the rows to rewrite, the scope it read (the
// newest event id, unless --through names one) and the command that applies
// exactly that. Run it after the release that stores the allowlist is
// deployed everywhere network create runs (api and alt).

// The scope and pace of a run.
type dbScrubNetworkCreateSecretsOptions struct {
	// the scope's lower bound, exclusive; nil from the first row
	after *server.Id
	// the scope's upper bound, inclusive; nil for the dry run's newest row
	through  *server.Id
	settings *model.NetworkCreateAuditScrubSettings
}

// Runs the command line and returns the exit code.
func dbScrubNetworkCreateSecretsCommand(opts docopt.Opts) int {
	afterStr, _ := opts.String("--after")
	throughStr, _ := opts.String("--through")
	batchStr, _ := opts.String("--batch")
	batchPauseStr, _ := opts.String("--batch-pause")
	apply, _ := opts.Bool("--apply")
	expect, _ := opts.String("--expect")
	options, err := parseDbScrubNetworkCreateSecretsOptions(afterStr, throughStr, batchStr, batchPauseStr, apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db scrub-network-create-secrets: %s\n", err)
		return 1
	}
	// The dry run writes nothing to the database: its process is read only, so
	// PostgreSQL refuses any write on its path.
	if !apply {
		server.RequirePgReadOnly()
	}
	return runDbScrubNetworkCreateSecrets(context.Background(), os.Stdout, os.Stderr, options, apply, expect)
}

// The options of the command line; --apply needs --through.
func parseDbScrubNetworkCreateSecretsOptions(
	afterStr string,
	throughStr string,
	batchStr string,
	batchPauseStr string,
	apply bool,
) (*dbScrubNetworkCreateSecretsOptions, error) {
	options := &dbScrubNetworkCreateSecretsOptions{
		settings: model.DefaultNetworkCreateAuditScrubSettings(),
	}
	if afterStr != "" {
		after, err := server.ParseId(afterStr)
		if err != nil {
			return nil, fmt.Errorf("--after: %q is not an event id", afterStr)
		}
		options.after = &after
	}
	if throughStr != "" {
		through, err := server.ParseId(throughStr)
		if err != nil {
			return nil, fmt.Errorf("--through: %q is not an event id", throughStr)
		}
		options.through = &through
	}
	if apply && options.through == nil {
		return nil, errors.New("--apply needs --through, the scope the dry run printed")
	}
	if batchStr != "" {
		// smaller than the default, never larger: big single updates have
		// hurt the primary
		maxBatch := model.DefaultNetworkCreateAuditScrubSettings().BatchSize
		batch, err := strconv.Atoi(batchStr)
		if err != nil || batch <= 0 || maxBatch < batch {
			return nil, fmt.Errorf("--batch: %q is not a row count from 1 to %d", batchStr, maxBatch)
		}
		options.settings.BatchSize = batch
	}
	if batchPauseStr != "" {
		batchPause, err := time.ParseDuration(batchPauseStr)
		if err != nil || batchPause < 0 {
			return nil, fmt.Errorf("--batch-pause: %q is not a duration", batchPauseStr)
		}
		options.settings.BatchPause = batchPause
	}
	return options, nil
}

// The flags of the command that runs the same scope and pace.
func dbScrubNetworkCreateSecretsFlags(options *dbScrubNetworkCreateSecretsOptions) string {
	defaults := model.DefaultNetworkCreateAuditScrubSettings()
	flags := ""
	if options.after != nil {
		flags += " --after=" + options.after.String()
	}
	if options.through != nil {
		flags += " --through=" + options.through.String()
	}
	if options.settings.BatchSize != defaults.BatchSize {
		flags += " --batch=" + strconv.Itoa(options.settings.BatchSize)
	}
	if options.settings.BatchPause != defaults.BatchPause {
		flags += " --batch-pause=" + options.settings.BatchPause.String()
	}
	return flags
}

// Runs the dry run or the apply and returns the exit code. Prints ids and
// counts only, never event details.
func runDbScrubNetworkCreateSecrets(
	ctx context.Context,
	stdout io.Writer,
	stderr io.Writer,
	options *dbScrubNetworkCreateSecretsOptions,
	apply bool,
	expect string,
) int {
	lastCensusReport := time.Time{}
	progress := func(progress model.NetworkCreateAuditScrubProgress) {
		switch progress.Stage {
		case "census":
			if time.Since(lastCensusReport) < 10*time.Second {
				return
			}
			lastCensusReport = time.Now()
			fmt.Fprintf(stderr, "db scrub-network-create-secrets: census read through %s, %d candidate rows\n", progress.EventId, progress.Count)
		case "rewrite":
			fmt.Fprintf(stdout, "  %s rewrote %d of %d rows (committed through %s)\n", time.Now().UTC().Format("15:04:05Z"), progress.Count, progress.Total, progress.EventId)
		}
	}

	if !apply {
		if options.through == nil {
			through, found := model.NewestAuditNetworkEventId(ctx)
			if !found {
				fmt.Fprintf(stdout, "db scrub-network-create-secrets DRY RUN: audit_network_event is empty; nothing to scrub\n")
				return 0
			}
			options.through = &through
		}
		census, err := model.CensusNetworkCreateAuditSecrets(ctx, options.after, *options.through, options.settings, progress)
		if err != nil {
			fmt.Fprintf(stderr, "%s\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "db scrub-network-create-secrets DRY RUN: nothing was changed\n")
		writeDbScrubNetworkCreateSecretsCensus(stdout, census)
		fmt.Fprintf(stdout, "to apply exactly this: bringyourctl db scrub-network-create-secrets%s --apply --expect=%s\n", dbScrubNetworkCreateSecretsFlags(options), census.Digest)
		return 0
	}

	start := time.Now()
	result, err := model.ApplyNetworkCreateAuditScrub(ctx, options.after, *options.through, expect, options.settings, progress)
	if err != nil {
		switch {
		case errors.Is(err, model.ErrNetworkCreateAuditScrubDigestMismatch) && result != nil && result.Census != nil:
			fmt.Fprintf(stdout, "db scrub-network-create-secrets APPLY REFUSED: nothing was changed; the rows as they are now:\n")
			writeDbScrubNetworkCreateSecretsCensus(stdout, result.Census)
		case result != nil && result.LastEventId != nil:
			fmt.Fprintf(stdout, "db scrub-network-create-secrets STOPPED after rewriting %d rows, committed through %s\n", result.RewrittenCount, result.LastEventId)
			fmt.Fprintf(stdout, "to resume, dry run again: bringyourctl db scrub-network-create-secrets --after=%s --through=%s\n", result.LastEventId, options.through)
		case result != nil && result.Census != nil:
			fmt.Fprintf(stdout, "db scrub-network-create-secrets STOPPED before rewriting any row\n")
		}
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "db scrub-network-create-secrets APPLY: rewrote %d of %d rows in %s\n", result.RewrittenCount, len(result.Census.EventIds), time.Since(start).Round(time.Second))
	fmt.Fprintf(stdout, "[audit]db scrub-network-create-secrets digest=%s after=%s through=%s rewritten=%d\n", expect, dbScrubNetworkCreateSecretsEventIdString(options.after), options.through, result.RewrittenCount)
	return 0
}

// Prints the scope, a count per key, the rows to rewrite, the first unreadable
// ids and the digest.
func writeDbScrubNetworkCreateSecretsCensus(out io.Writer, census *model.NetworkCreateAuditScrubCensus) {
	fmt.Fprintf(out, "scope: audit_network_event ids after %s through %s\n", dbScrubNetworkCreateSecretsEventIdString(census.After), census.Through)
	fmt.Fprintf(out, "network_created rows that may hold a key: %d\n", census.CandidateCount)
	for _, keyPath := range model.NetworkCreateAuditScrubKeyPaths() {
		fmt.Fprintf(out, "  rows holding %s: %d\n", keyPath, census.KeyCounts[keyPath])
	}
	fmt.Fprintf(out, "rows to rewrite: %d\n", len(census.EventIds))
	if 0 < len(census.UnreadableEventIds) {
		fmt.Fprintf(out, "unreadable rows, left as they are: %d\n", len(census.UnreadableEventIds))
		for i, eventId := range census.UnreadableEventIds {
			if i == 20 {
				fmt.Fprintf(out, "  ... and %d more\n", len(census.UnreadableEventIds)-i)
				break
			}
			fmt.Fprintf(out, "  %s\n", eventId)
		}
	}
	fmt.Fprintf(out, "digest: %s\n", census.Digest)
}

// The id, or "start" for none.
func dbScrubNetworkCreateSecretsEventIdString(eventId *server.Id) string {
	if eventId == nil {
		return "start"
	}
	return eventId.String()
}
