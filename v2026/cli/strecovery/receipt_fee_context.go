// Fee-context replay has only pinned local reads. It joins exact receipt/native
// commitments without a node, database, runtime authority or signing interface.
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

// Print a complete relative-proof report only after all input proofs pass.
// Missing block context is explicit in the report; invalid proof bytes abort.
func runReceiptFeeContextsCommand(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("strecovery verify-fee-contexts", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	archivePath := flags.String("archive", "", "absolute private archive path")
	collectionPath := flags.String("collection", "", "absolute private receipt collection")
	collectionPin := flags.String("collection-sha256", "", "exact collection byte pin")
	checkpointPath := flags.String("checkpoint", "", "absolute private independent checkpoint")
	checkpointPin := flags.String("checkpoint-sha256", "", "exact checkpoint byte pin; grants no approval")
	proofPath := flags.String("proof", "", "absolute private native finality proof")
	proofPin := flags.String("proof-sha256", "", "exact finality proof byte pin")
	timeout := flags.Duration("timeout", 5*time.Minute, "total offline deadline, at most 15m")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if ctx == nil || flags.NArg() != 0 || *archivePath == "" || *collectionPath == "" || *collectionPin == "" || *checkpointPath == "" || *checkpointPin == "" || *proofPath == "" || *proofPin == "" || *timeout <= 0 || *timeout > 15*time.Minute {
		return errors.New("verify-fee-contexts requires archive, pinned collection/checkpoint/proof and a bounded deadline")
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
	proof, err := strecovery.LoadReceiptFinalityProof(ctx, strecovery.FileReference{Path: *proofPath, Sha256: *proofPin})
	if err != nil {
		return err
	}
	result, err := strecovery.VerifyReceiptFeeContexts(ctx, archive, collection, checkpoint, proof)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(result)
}
