package main

// bringyourctl evidence move-flat / move-flat-verify / flat-census: moves the
// legacy flat evidence content objects on edge-5's MinIO to their sharded
// keys, without listing the flat folder (evidencemove). Postgres is read only:
// the process is read only (server.RequirePgReadOnly), so PostgreSQL refuses
// any write.
//
// From the operator's Mac, with the Main tunnel up (xops/bin/by-patch-main-data)
// and MinIO reached over the management VPN:
//
//	WARP_ENV=main WARP_HOST=owner-local WARP_VERSION=local+<sha> \
//	  WARP_CONFIG_HOME=~/urnetwork/config WARP_VAULT_HOME=~/urnetwork/vault \
//	  BRINGYOUR_POSTGRES_HOSTNAME=127.0.0.1 BRINGYOUR_MINIO_HOSTNAME=172.28.208.176 \
//	  go run ./bringyourctl evidence move-flat --checkpoint=<path>

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/evidencemove"
)

func evidenceLogf(format string, args ...any) {
	fmt.Printf("%s %s\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), fmt.Sprintf(format, args...))
}

// The move's settings from the command line: the defaults, overridden by each
// option given.
func evidenceMoveFlatOptions(opts docopt.Opts) (settings evidencemove.Settings, checkpointPath string, journalPath string, err error) {
	settings = evidencemove.DefaultSettings()
	checkpointPath, _ = opts.String("--checkpoint")
	journalPath, _ = opts.String("--journal")
	if snapshotPath, _ := opts.String("--snapshot"); snapshotPath != "" {
		if _, statErr := os.Stat(snapshotPath); statErr != nil {
			return settings, "", "", fmt.Errorf("--snapshot: %w", statErr)
		}
	}
	settings.DryRun, _ = opts.Bool("--dry-run")
	if value, _ := opts.String("--objects-per-second"); value != "" {
		if settings.ObjectsPerSecond, err = strconv.ParseFloat(value, 64); err != nil {
			return settings, "", "", fmt.Errorf("--objects-per-second: %w", err)
		}
	}
	for _, option := range []struct {
		name  string
		value *int
	}{
		{"--workers", &settings.Workers},
		{"--batch-rows", &settings.BatchRows},
	} {
		if value, _ := opts.String(option.name); value != "" {
			if *option.value, err = strconv.Atoi(value); err != nil {
				return settings, "", "", fmt.Errorf("%s: %w", option.name, err)
			}
		}
	}
	if value, _ := opts.String("--max-moves"); value != "" {
		if settings.MaxMoves, err = strconv.ParseInt(value, 10, 64); err != nil {
			return settings, "", "", fmt.Errorf("--max-moves: %w", err)
		}
	}
	if value, _ := opts.String("--until-created"); value != "" {
		if settings.UntilCreated, err = time.Parse(time.RFC3339, value); err != nil {
			return settings, "", "", fmt.Errorf("--until-created: %w", err)
		}
	}
	if checkpointPath == "" && !settings.DryRun {
		return settings, "", "", fmt.Errorf("a run that removes objects needs --checkpoint")
	}
	return settings, checkpointPath, journalPath, settings.Validate()
}

// The bucket's store, checked read only before anything moves.
func evidenceMoveStore(ctx context.Context, connections int) (*evidencemove.MinioObjectStore, *server.BlobStoreConfig) {
	config, present := server.LoadBlobStoreConfig()
	if !present || config.Local {
		evidenceFail(fmt.Errorf("no MinIO blob store is configured (vault minio.yml)"))
	}
	store, err := evidencemove.NewMinioObjectStore(config, connections)
	if err != nil {
		evidenceFail(err)
	}
	preflightCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := store.Preflight(preflightCtx); err != nil {
		evidenceFail(err)
	}
	evidenceLogf("store %s bucket %s prefix %s", config.Authority, config.Bucket, config.Prefix)
	return store, config
}

func evidenceFail(err error) {
	evidenceLogf("error: %v", err)
	os.Exit(1)
}

func evidenceReadOnlyDb(name string) {
	server.RequirePgReadOnly()
	server.SetPgDirectApplicationName("bringyourctl evidence " + name)
}

func evidencePrintJson(label string, value any) {
	data, _ := json.MarshalIndent(value, "", "  ")
	evidenceLogf("%s:\n%s", label, data)
}

func evidenceMoveFlatCommand(opts docopt.Opts) {
	settings, checkpointPath, journalPath, err := evidenceMoveFlatOptions(opts)
	if err != nil {
		evidenceFail(err)
	}
	evidenceReadOnlyDb("move-flat")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, config := evidenceMoveStore(ctx, settings.Workers)
	latency := evidencemove.NewLatencies()
	// the validator idle, MinIO healthy, and Main healthy too: the move's
	// load reaches Main's clients through MinIO
	gates := evidencemove.NewGateKeeper(evidenceLogf, append(
		evidencemove.DefaultGates(store.HealthURL(), latency, evidencemove.DefaultPromUrl),
		evidencemove.MainHealthGates(evidencemove.DefaultPromUrl)...,
	)...)
	gates.Start(ctx)
	var source evidencemove.Source = &evidencemove.ClientKeyHistorySource{}
	if snapshotPath, _ := opts.String("--snapshot"); snapshotPath != "" {
		snapshot, err := evidencemove.OpenSnapshotSource(snapshotPath)
		if err != nil {
			evidenceFail(err)
		}
		defer snapshot.Close()
		source = snapshot
		evidenceLogf("source: snapshot %s, %d rows", snapshotPath, snapshot.Rows())
	}
	mover := &evidencemove.Mover{
		Store:    store,
		Prefix:   config.Prefix,
		Source:   source,
		Gates:    gates,
		Pacer:    evidencemove.NewPacer(settings.ObjectsPerSecond),
		Settings: settings,
		Logf:     evidenceLogf,
		Latency:  latency,
	}
	if checkpointPath != "" {
		checkpoint, err := evidencemove.OpenCheckpoint(checkpointPath, evidencemove.ClientKeyHistorySourceName, config.Bucket, config.Prefix)
		if err != nil {
			evidenceFail(err)
		}
		defer checkpoint.Close()
		mover.Checkpoint = checkpoint
		refusals, err := evidencemove.OpenJSONL(checkpoint.RefusalsPath())
		if err != nil {
			evidenceFail(err)
		}
		defer refusals.Close()
		mover.Refusals = refusals
		evidenceLogf("checkpoint %s at %s (%+v)", checkpointPath, checkpoint.State.Cursor, checkpoint.State.Counts)
	}
	if journalPath != "" {
		journal, err := evidencemove.OpenJSONL(journalPath)
		if err != nil {
			evidenceFail(err)
		}
		defer journal.Close()
		mover.Journal = journal
	}
	evidenceLogf(
		"move-flat: %.0f objects a second, %d workers, batches of %d, max moves %d, until created %s, dry run %v",
		settings.ObjectsPerSecond, settings.Workers, settings.BatchRows, settings.MaxMoves, settings.UntilCreated.Format(time.RFC3339), settings.DryRun,
	)
	report, err := mover.Run(ctx)
	if report != nil {
		evidencePrintJson("report", report)
	}
	if err != nil {
		evidenceFail(err)
	}
}

func evidenceMoveFlatVerifyCommand(opts docopt.Opts) {
	journalPath, _ := opts.String("--journal")
	checkpointPath, _ := opts.String("--checkpoint")
	sample := 200
	if value, _ := opts.String("--fallback-sample"); value != "" {
		var err error
		if sample, err = strconv.Atoi(value); err != nil || sample < 0 {
			evidenceFail(fmt.Errorf("--fallback-sample must be a count"))
		}
	}
	settings := evidencemove.DefaultSettings()
	evidenceReadOnlyDb("move-flat-verify")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, config := evidenceMoveStore(ctx, settings.Workers)
	reader, err := server.NewBlobStore(config)
	if err != nil {
		evidenceFail(err)
	}
	moved, err := evidencemove.ReadJournalRemovals(journalPath)
	if err != nil {
		evidenceFail(err)
	}
	var unmoved []evidencemove.Item
	if checkpointPath != "" && 0 < sample {
		data, err := os.ReadFile(checkpointPath)
		if err != nil {
			evidenceFail(err)
		}
		var state evidencemove.CheckpointState
		if err := json.Unmarshal(data, &state); err != nil {
			evidenceFail(err)
		}
		// the rows the checkpoint has not reached: not moved by any run of it
		unmoved, err = (&evidencemove.ClientKeyHistorySource{}).Next(ctx, state.Cursor, sample)
		if err != nil {
			evidenceFail(err)
		}
	}
	latency := evidencemove.NewLatencies()
	gates := evidencemove.NewGateKeeper(evidenceLogf, evidencemove.DefaultGates(store.HealthURL(), latency, evidencemove.DefaultPromUrl)...)
	gates.Start(ctx)
	evidenceLogf("verify: %d moved hashes from %s, %d unmoved rows after the checkpoint", len(moved), journalPath, len(unmoved))
	start := time.Now()
	report, err := evidencemove.Verify(ctx, store, reader, config.Prefix, moved, unmoved, gates, evidencemove.NewPacer(settings.ObjectsPerSecond), settings.Workers, settings.MaxObjectBytes)
	evidencePrintJson("verify report", report)
	evidenceLogf("verify took %s; health latency %s", time.Since(start).Round(time.Second), latency.Line())
	if err != nil {
		evidenceFail(err)
	}
	if report.Readable != len(moved) || report.FlatGone != len(moved) || report.FallbackReadable != report.FlatOnly {
		evidenceFail(fmt.Errorf("verification failed: %d failures", len(report.Failures)))
	}
}

func evidenceFlatCensusCommand(opts docopt.Opts) {
	rows := 3000
	if value, _ := opts.String("--sample-rows"); value != "" {
		var err error
		if rows, err = strconv.Atoi(value); err != nil || rows < 1 {
			evidenceFail(fmt.Errorf("--sample-rows must be a positive count"))
		}
	}
	settings := evidencemove.DefaultSettings()
	settings.DryRun = true
	settings.ObjectsPerSecond = 100
	settings.Workers = 32
	evidenceReadOnlyDb("flat-census")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, config := evidenceMoveStore(ctx, settings.Workers)
	estimate, pages, err := evidencemove.ClientKeyHistoryRowEstimate(ctx)
	if err != nil {
		evidenceFail(err)
	}
	sample, err := evidencemove.SampleClientKeyHistory(ctx, rows, pages, time.Now().Unix())
	if err != nil {
		evidenceFail(err)
	}
	evidenceLogf("st_client_key_history: about %d rows on %d pages; %d rows sampled", estimate, pages, len(sample))
	latency := evidencemove.NewLatencies()
	// HEADs only: the census waits on MinIO's health, not on the validator
	gates := evidencemove.NewGateKeeper(evidenceLogf, &evidencemove.MinioHealthGate{Url: store.HealthURL(), MaxLatency: evidencemove.DefaultMinioHealthMaxLatency, Latency: latency})
	gates.Start(ctx)
	mover := &evidencemove.Mover{
		Store:    store,
		Prefix:   config.Prefix,
		Source:   evidencemove.NewSliceSource(sample),
		Gates:    gates,
		Pacer:    evidencemove.NewPacer(settings.ObjectsPerSecond),
		Settings: settings,
		Logf:     evidenceLogf,
		Latency:  latency,
	}
	report, err := mover.Run(ctx)
	if err != nil {
		evidenceFail(err)
	}
	counts := report.Counts
	share := func(count int64) float64 { return float64(count) / float64(max(1, counts.Items)) }
	evidenceLogf(
		"census of %d sampled rows: flat only %d (%.2f%%), both %d (%.2f%%), sharded only %d (%.2f%%), neither %d (%.2f%%), refused %d",
		counts.Items, counts.WouldMove, 100*share(counts.WouldMove), counts.WouldDedupe, 100*share(counts.WouldDedupe),
		counts.Done, 100*share(counts.Done), counts.Absent, 100*share(counts.Absent), counts.Refused,
	)
	evidenceLogf(
		"estimate: %.0f flat objects have a st_client_key_history row (%.0f of them also sharded); %.0f rows to read",
		float64(estimate)*share(counts.WouldMove+counts.WouldDedupe), float64(estimate)*share(counts.WouldDedupe), float64(estimate),
	)
	evidenceLogf("latency %s", latency.Line())
}

// The snapshot's settings from the command line: the defaults, overridden by
// each option given.
func evidenceSnapshotOptions(opts docopt.Opts) (settings evidencemove.SnapshotSettings, outPath string, err error) {
	settings = evidencemove.DefaultSnapshotSettings()
	outPath, _ = opts.String("--out")
	if outPath == "" {
		return settings, "", fmt.Errorf("--out is required")
	}
	if value, _ := opts.String("--pages-per-second"); value != "" {
		if settings.PagesPerSecond, err = strconv.ParseFloat(value, 64); err != nil {
			return settings, "", fmt.Errorf("--pages-per-second: %w", err)
		}
	}
	if value, _ := opts.String("--batch-pages"); value != "" {
		if settings.BatchPages, err = strconv.Atoi(value); err != nil {
			return settings, "", fmt.Errorf("--batch-pages: %w", err)
		}
	}
	return settings, outPath, settings.Validate()
}

// Snapshots every st_client_key_history row's evidence hash: the heap in page
// order into <out>.raw (resuming one an earlier run left), paced and gated on
// Main's health, then sorted into key order at --out. A finished snapshot gets
// its manifest: rows, sha256, and 2,000 random records checked against the
// table; the raw file is removed once the manifest is written.
func evidenceSnapshotHashesCommand(opts docopt.Opts) {
	settings, outPath, err := evidenceSnapshotOptions(opts)
	if err != nil {
		evidenceFail(err)
	}
	evidenceReadOnlyDb("snapshot-hashes")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	evidenceLogf("snapshot %s: %.0f heap pages a second, %d a statement", outPath, settings.PagesPerSecond, settings.BatchPages)
	gates := evidencemove.NewGateKeeper(evidenceLogf, evidencemove.MainHealthGates(evidencemove.DefaultPromUrl)...)
	gates.Start(ctx)
	report, err := evidencemove.ExportSnapshotPages(ctx, outPath, gates, settings, evidenceLogf)
	if report != nil {
		evidencePrintJson("export report", report)
	}
	if err != nil {
		evidenceFail(err)
	}
	if !report.Finished {
		evidenceLogf("snapshot stopped before the table's end; rerun the same command to resume")
		return
	}
	// about 1 GB of records a sorted run
	sorted, err := evidencemove.SortSnapshot(evidencemove.SnapshotRawPath(outPath), outPath, 8*1024*1024)
	if err != nil {
		evidenceFail(err)
	}
	evidencePrintJson("sort report", sorted)
	manifest, err := evidencemove.SummarizeSnapshot(outPath)
	if err != nil {
		evidenceFail(err)
	}
	manifest.Finished = time.Now().UTC()
	manifest.Export = *report
	manifest.Sort = sorted
	sample, err := evidencemove.SampleSnapshot(outPath, 2000, uint64(time.Now().UnixNano()))
	if err != nil {
		evidenceFail(err)
	}
	if manifest.Sample, err = evidencemove.CheckSnapshotSample(ctx, sample); err != nil {
		evidenceFail(err)
	}
	if err := evidencemove.WriteSnapshotManifest(outPath, manifest); err != nil {
		evidenceFail(err)
	}
	evidencePrintJson("manifest "+evidencemove.SnapshotManifestPath(outPath), manifest)
	if manifest.Sample.Differ != 0 {
		evidenceFail(fmt.Errorf("%d sampled records differ from the table", manifest.Sample.Differ))
	}
	if err := evidencemove.RemoveSnapshotRaw(outPath); err != nil {
		evidenceFail(err)
	}
}
