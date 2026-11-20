// Historical storage commands borrow pinned local proof inputs and never open
// an operator database. Only capture owns read-only RPC and private journaling.
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

func runReceiptHistoricalNativeStateCommand(ctx context.Context, args []string, stdout io.Writer) error {
	command := args[0]
	capture := command == "capture-historical-state"
	flags := flag.NewFlagSet("strecovery "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	archivePath := flags.String("archive", "", "absolute private archive path")
	collectionPath := flags.String("collection", "", "absolute private receipt collection")
	collectionPin := flags.String("collection-sha256", "", "exact collection byte pin")
	checkpointPath := flags.String("checkpoint", "", "absolute private independent checkpoint")
	checkpointPin := flags.String("checkpoint-sha256", "", "exact checkpoint byte pin; grants no approval")
	proofPath := flags.String("proof", "", "absolute private native finality proof")
	proofPin := flags.String("proof-sha256", "", "exact finality proof byte pin")
	timeout := flags.Duration("timeout", 10*time.Minute, "total command deadline, 60s to 15m")
	var configPath, configPin, directory, witnessPath, witnessPin string
	if capture {
		flags.StringVar(&configPath, "config", "", "absolute private capture config")
		flags.StringVar(&configPin, "config-sha256", "", "exact capture config byte pin")
		flags.StringVar(&directory, "capture-dir", "", "existing private directory for immutable partial evidence and witness.json")
	} else {
		flags.StringVar(&witnessPath, "witness", "", "absolute private historical storage witness")
		flags.StringVar(&witnessPin, "witness-sha256", "", "exact storage witness byte pin")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if ctx == nil || flags.NArg() != 0 || *archivePath == "" || *collectionPath == "" || *collectionPin == "" || *checkpointPath == "" || *checkpointPin == "" || *proofPath == "" || *proofPin == "" || *timeout < time.Minute || *timeout > 15*time.Minute ||
		capture && (configPath == "" || configPin == "" || directory == "") || !capture && (witnessPath == "" || witnessPin == "") {
		return errors.New("historical state command requires archive, all pinned inputs and a 60s to 15m deadline")
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
	var result any
	if capture {
		config, loadErr := strecovery.LoadReceiptHistoricalNativeCaptureConfig(ctx, strecovery.FileReference{Path: configPath, Sha256: configPin})
		if loadErr != nil {
			return loadErr
		}
		result, err = strecovery.CaptureReceiptHistoricalNativeState(ctx, archive, collection, checkpoint, proof, *config, directory)
	} else {
		witness, loadErr := strecovery.LoadReceiptHistoricalNativeStateWitness(ctx, strecovery.FileReference{Path: witnessPath, Sha256: witnessPin})
		if loadErr != nil {
			return loadErr
		}
		result, err = strecovery.VerifyReceiptHistoricalNativeState(ctx, archive, collection, checkpoint, proof, witness)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(result)
}
