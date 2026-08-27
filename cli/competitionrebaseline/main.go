package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competition"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "competitionrebaseline: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("competitionrebaseline", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	roundValue := flags.String("round_id", "", "generated competition round UUID")
	patchPath := flags.String("patch", "", "trusted structurally valid patch")
	patchSha256 := flags.String("patch_sha256", "", "pinned canonical patch SHA-256")
	outputPath := flags.String("output", "", "exclusive JSON evidence path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 ||
		*roundValue == "" || *patchPath == "" || *patchSha256 == "" || *outputPath == "" {
		return errors.New("usage: competitionrebaseline --round_id UUID --patch FILE --patch_sha256 HEX --output FILE")
	}
	roundId, err := server.ParseId(*roundValue)
	if err != nil {
		return errors.New("round_id is malformed")
	}
	settings, err := competition.LoadSettings()
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	patch, err := readPatch(*patchPath, int64(settings.PatchPolicy.MaxPatchBytes))
	if err != nil {
		return err
	}
	defer clear(patch)
	result, err := competition.RunRebaseline(
		ctx,
		settings,
		competition.PostgresStore{},
		competition.CommandEvaluator{},
		roundId,
		string(patch),
		*patchSha256,
	)
	if err != nil {
		return err
	}
	if err := writeExclusiveJson(*outputPath, result); err != nil {
		return err
	}
	fmt.Printf("rebaseline evaluation authenticated: %s\n", *outputPath)
	return nil
}

func readPatch(path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("patch path must be absolute and canonical")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || limit < info.Size() {
		return nil, errors.New("patch is missing, unsafe, empty, or oversized")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read patch: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() ||
		!os.SameFile(info, opened) || opened.Size() <= 0 || limit < opened.Size() {
		return nil, errors.New("patch changed while opening")
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read patch: %w", err)
	}
	if int64(len(value)) != opened.Size() || limit < int64(len(value)) {
		clear(value)
		return nil, errors.New("patch changed while reading")
	}
	return value, nil
}

func writeExclusiveJson(path string, value any) (err error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("output path must be absolute and canonical")
	}
	parentInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return errors.New("output parent is missing")
	}
	if !parentInfo.IsDir() {
		return errors.New("output parent is not a directory")
	}
	if parentInfo.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("output parent is group/world writable: %04o", parentInfo.Mode().Perm())
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(value); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	keep = true
	return nil
}
