//go:build linux

// Records one independently approved epoch-three runtime correction. It
// cannot obtain a lease, edit a frozen record, or enable grading.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// Hold the opened inode, rejecting links, special files and non-root writers.
func readProtectedDocument(path string, maximum int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private operator document is not a protected regular file")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm()&0077 != 0 {
		return nil, errors.New("operator document changed while opening")
	}
	stat, ok := after.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || after.Size() <= 0 || after.Size() > maximum {
		return nil, errors.New("operator document ownership or size is invalid")
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) != after.Size() {
		return nil, errors.New("operator document could not be read exactly")
	}
	return content, nil
}

// Reserialization prevents duplicate/unknown JSON keys and formatting from
// being interpreted differently by the operator, application and journal.
func decodeApprovedCorrection(content []byte, approved string) (value model.CompetitionEpochThreeRuntimeCorrection, err error) {
	content = bytes.TrimSpace(content)
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != approved || json.Unmarshal(content, &value) != nil {
		return value, errors.New("correction bytes do not match the independent approval")
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(content, canonical) {
		return value, errors.New("correction is not canonical")
	}
	return value, nil
}

// The caller must already hold the lease. Controller and SQL check it again
// inside the same transaction as the append-only insert.
func run(args []string) error {
	flags := flag.NewFlagSet("competitionruntimecorrection", flag.ContinueOnError)
	correctionPath := flags.String("correction", "", "root-only canonical correction document")
	leasePath := flags.String("lease", "", "root-only existing supervisor lease document")
	approved := flags.String("approved-sha256", "", "independently approved canonical SHA-256")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *correctionPath == "" || *leasePath == "" || os.Geteuid() != 0 || os.Getenv("WARP_ENV") != "main" {
		return errors.New("exact correction, live lease and root main environment are required")
	}
	content, err := readProtectedDocument(*correctionPath, 8192)
	if err != nil {
		return err
	}
	value, err := decodeApprovedCorrection(content, *approved)
	if err != nil {
		return err
	}
	content, err = readProtectedDocument(*leasePath, 4096)
	if err != nil {
		return err
	}
	var lease model.CompetitionSupervisorLease
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lease); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("lease contains trailing data")
	}
	settings, err := controller.LoadSettings()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return (controller.PostgresStore{}).RecordEpochThreeRuntimeCorrection(ctx, settings, lease, value, *approved)
}

// Reports only whether the bounded registration succeeded.
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "epoch-three runtime correction was not recorded")
		os.Exit(1)
	}
	fmt.Println("epoch-three runtime correction recorded")
}
