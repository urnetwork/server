// Receipt collection has only owned HTTP read access and create-only evidence
// publication. Offline replay uses no HTTP or operator database connection.
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

// Complete argument validation precedes archive reads, and full commitment
// verification precedes file publication or successful command output.
func runReceiptCollectionCommand(ctx context.Context, args []string, stdout io.Writer) error {
	command := args[0]
	flags := flag.NewFlagSet("strecovery "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	archivePath := flags.String("archive", "", "absolute private archive path")
	collectionPath := flags.String("collection", "", "absolute private collected evidence path")
	timeout := flags.Duration("timeout", 10*time.Minute, "total command deadline, 60s to 15m")
	var configPath, configPin, collectionPin string
	if command == "collect-receipts" {
		flags.StringVar(&configPath, "config", "", "absolute private receipt collection config")
		flags.StringVar(&configPin, "config-sha256", "", "exact config byte pin; does not approve its node or mapping")
	} else {
		flags.StringVar(&collectionPin, "collection-sha256", "", "exact collected evidence file byte pin")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *archivePath == "" || *collectionPath == "" || *timeout < time.Minute || *timeout > 15*time.Minute ||
		command == "collect-receipts" && (configPath == "" || configPin == "") || command == "verify-collection" && collectionPin == "" {
		return errors.New("receipt collection command requires complete pinned inputs, exact paths and a 60s to 15m deadline")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	archive, err := strecovery.LoadArchive(ctx, *archivePath)
	if err != nil {
		return err
	}
	var collection *strecovery.ReceiptCollection
	if command == "collect-receipts" {
		config, err := strecovery.LoadReceiptCollectionConfig(ctx, strecovery.FileReference{Path: configPath, Sha256: configPin})
		if err != nil {
			return err
		}
		collection, err = strecovery.CollectReceiptEvidence(ctx, archive, *config)
		if err != nil {
			return err
		}
		if err := strecovery.WriteReceiptCollection(ctx, *collectionPath, archive, collection); err != nil {
			return err
		}
	} else {
		collection, err = strecovery.LoadReceiptCollection(ctx, strecovery.FileReference{Path: *collectionPath, Sha256: collectionPin})
		if err != nil {
			return err
		}
	}
	result, err := strecovery.VerifyReceiptCollection(ctx, archive, collection)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(result)
}
