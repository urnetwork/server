// Unqualified first-committed archive preparation, local verification, and
// separately authorized retained upload. Never invoke during timed scoring.
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
)

// Mode parsing has no filesystem, environment, or transport side effects.
func parseArguments(arguments []string) (archiveRequest, *prepareRequest, error) {
	flags := flag.NewFlagSet("retained-archive", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	planPath := flags.String("plan", "", "protected archive-plan.json path")
	planSha256 := flags.String("plan-sha256", "", "exact reviewed plan digest")
	upload := flags.Bool("upload", false, "explicit one-use retained upload")
	authorization := flags.String("authorize-upload-plan-sha256", "", "separate exact-plan upload authorization")
	prepare := flags.Bool("prepare", false, "explicit private local preparation; never uploads")
	source := flags.String("source", "", "one compiled first-committed source root")
	nonce := flags.String("nonce", "", "new 48-character lowercase storage nonce")
	stagingBytes := flags.Int64("staging-reserve-bytes", 0, "additional required free staging bytes")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return archiveRequest{}, nil, errors.New("invalid archive arguments")
	}
	request := archiveRequest{PlanPin: filePin{Path: *planPath, Sha256: *planSha256},
		Upload: *upload, UploadAuthorizationSha256: *authorization}
	if *prepare {
		if *upload || *authorization != "" || *planPath != "" || *planSha256 != "" {
			return archiveRequest{}, nil, errors.New("prepare, verify, and upload modes cannot be mixed")
		}
		return request, &prepareRequest{SourceRoot: *source, StorageNonce: *nonce, StagingReserveBytes: *stagingBytes}, nil
	}
	if *source != "" || *nonce != "" || *stagingBytes != 0 {
		return archiveRequest{}, nil, errors.New("source and staging flags require explicit preparation")
	}
	if *planPath == "" || !validDigest(*planSha256) {
		return archiveRequest{}, nil, errors.New("protected plan and exact digest required")
	}
	return request, nil, nil
}

// Runtime authority stays root-only; no command loads private input structures.
func runCommand(ctx context.Context, arguments []string, output io.Writer) error {
	request, preparation, err := parseArguments(arguments)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("root archive principal required")
	}
	hooks := archiveHooks{Now: time.Now, AvailableBytes: diskAvailableBytes,
		AcquireSlot: acquireNativeSlot, NewTransport: newNativeTransport,
		AfterStep: func(string) error { return nil }}
	report, err := executeArchive(ctx, request, preparation, defaultArchivePolicy(), defaultArchiveSettings(), hooks)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}

// Errors are stage descriptions, not raw client errors, tokens, or source data.
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := runCommand(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "retained archive refused:", err)
		os.Exit(1)
	}
}
