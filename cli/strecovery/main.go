// Command strecovery collects complete read-only operator signature custody and
// restores exact original bytes locally. It has no signing or chain-send port.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urnetwork/server/strecovery"
)

// Interrupts cancel database reads and file publication before process exit.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, strecovery.PostgresReader{})
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// The injected reader is only a read-only snapshot interface, allowing command
// tests to prove rejected input never reaches a database connection.
func run(ctx context.Context, args []string, stdout io.Writer, reader strecovery.SnapshotReader) error {
	if len(args) == 0 {
		return errors.New("usage: strecovery collect|inspect|restore [flags]")
	}
	command := args[0]
	flags := flag.NewFlagSet("strecovery "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	archivePath := flags.String("archive", "", "absolute private archive path")
	timeout := flags.Duration("timeout", 5*time.Minute, "bounded command deadline, at most 30m")
	var configPath, storePath, acceptedHash string
	switch command {
	case "collect":
		flags.StringVar(&configPath, "config", "", "absolute private census config path")
	case "inspect":
	case "restore":
		flags.StringVar(&storePath, "store", "", "absolute existing owner-private destination directory")
		flags.StringVar(&acceptedHash, "accept-census-hash", "", "independently reviewed census digest")
	default:
		return errors.New("unknown recovery command")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *archivePath == "" || *timeout <= 0 || *timeout > 30*time.Minute || command == "collect" && configPath == "" || command == "restore" && (storePath == "" || acceptedHash == "") {
		return errors.New("recovery command requires exact paths, a bounded deadline and all command-specific inputs")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if command == "collect" {
		config, err := strecovery.LoadConfig(ctx, configPath)
		if err != nil {
			return err
		}
		archive, err := strecovery.Collect(ctx, *config, reader)
		if err != nil {
			return err
		}
		if err := strecovery.WriteArchive(ctx, *archivePath, archive); err != nil {
			return err
		}
		return encoder.Encode(archive.Inspect())
	}
	archive, err := strecovery.LoadArchive(ctx, *archivePath)
	if err != nil {
		return err
	}
	if command == "inspect" {
		return encoder.Encode(archive.Inspect())
	}
	result, err := strecovery.Restore(ctx, archive, storePath, acceptedHash)
	if result != nil {
		err = errors.Join(err, encoder.Encode(result))
	}
	return err
}
