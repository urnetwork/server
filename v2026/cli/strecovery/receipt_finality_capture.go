// Native capture owns read-only RPC and a private resumable evidence directory.
// It never opens operator databases, constructs signatures or sends chain writes.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"github.com/urnetwork/server/v2026/strecovery"
)

// Complete pinned inputs precede journal/network access. Failure emits no
// success report; synced partial evidence remains explicitly resumable on disk.
func runReceiptFinalityCaptureCommand(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("strecovery capture-finality", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	archivePath := flags.String("archive", "", "absolute private archive path")
	collectionPath := flags.String("collection", "", "absolute private receipt collection")
	collectionPin := flags.String("collection-sha256", "", "exact collection byte pin")
	checkpointPath := flags.String("checkpoint", "", "absolute private independent checkpoint")
	checkpointPin := flags.String("checkpoint-sha256", "", "exact checkpoint byte pin; grants no approval")
	configPath := flags.String("config", "", "absolute private owned-node capture config")
	configPin := flags.String("config-sha256", "", "exact capture config byte pin; grants no approval")
	directory := flags.String("capture-dir", "", "existing owner-private directory for immutable partial evidence and proof.json")
	timeout := flags.Duration("timeout", 10*time.Minute, "total capture deadline, 60s to 15m")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if ctx == nil || flags.NArg() != 0 || *archivePath == "" || *collectionPath == "" || *collectionPin == "" || *checkpointPath == "" || *checkpointPin == "" || *configPath == "" || *configPin == "" || *directory == "" || *timeout < time.Minute || *timeout > 15*time.Minute {
		return errors.New("capture-finality requires archive, pinned collection/checkpoint/config, private capture directory and a 60s to 15m deadline")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	archive, err := strecovery.LoadArchive(ctx, *archivePath)
	if err != nil {
		return err
	}
	collection, err := strecovery.LoadReceiptCollection(ctx, strecovery.FileReference{Path: *collectionPath, Sha256: *collectionPin})
	if err != nil {
		return err
	}
	checkpoint, err := strecovery.LoadNativeFinalityCheckpoint(ctx, strecovery.FileReference{Path: *checkpointPath, Sha256: *checkpointPin})
	if err != nil {
		return err
	}
	config, err := strecovery.LoadReceiptFinalityCaptureConfig(ctx, strecovery.FileReference{Path: *configPath, Sha256: *configPin})
	if err != nil {
		return err
	}
	result, err := strecovery.CaptureReceiptFinality(ctx, archive, collection, checkpoint, *config, *directory)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(result)
}
