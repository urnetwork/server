// One finite local postprocessor. It cannot contact a database, change source,
// select another test/marker, or overwrite the original jq/Go evidence.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// A syntactically complete projection does not mean the native test succeeded.
type projectionReceipt struct {
	Schema             int             `json:"schema"`
	Kind               string          `json:"kind"`
	ProjectionComplete bool            `json:"projection_complete"`
	ErrorCode          string          `json:"error_code,omitempty"`
	Stats              projectionStats `json:"stats"`
}

// The opened source must be a private regular single-link file, not an alias.
func openProjectionInput(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("invalid_input_path")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("input_open_failed")
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, errors.New("input_stat_failed")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		file.Close()
		return nil, errors.New("input_custody_rejected")
	}
	return file, nil
}

// Both new outputs are exclusive: a failed attempt remains retained, not reset.
func runProjection(inputPath, eventsPath, receiptPath string) error {
	if inputPath == eventsPath || inputPath == receiptPath || eventsPath == receiptPath ||
		!filepath.IsAbs(eventsPath) || filepath.Clean(eventsPath) != eventsPath ||
		!filepath.IsAbs(receiptPath) || filepath.Clean(receiptPath) != receiptPath {
		return errors.New("invalid_output_paths")
	}
	input, err := openProjectionInput(inputPath)
	if err != nil {
		return err
	}
	defer input.Close()
	before, err := input.Stat()
	if err != nil {
		return errors.New("input_stat_failed")
	}
	receiptFile, err := os.OpenFile(receiptPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return errors.New("receipt_identity_used_or_unavailable")
	}
	defer receiptFile.Close()
	events, err := os.OpenFile(eventsPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	receipt := projectionReceipt{Schema: 1, Kind: "migration-census-session-memory-projection/v1"}
	if err != nil {
		receipt.ErrorCode = "events_identity_used_or_unavailable"
	} else {
		receipt.Stats, err = projectDiagnostic(input, events, defaultProjectionSettings())
		if err != nil {
			receipt.ErrorCode = err.Error()
		}
		if closeErr := errors.Join(events.Sync(), events.Close()); closeErr != nil {
			receipt.ErrorCode = "events_persistence_failed"
		}
		after, statErr := input.Stat()
		pathInfo, pathErr := os.Lstat(inputPath)
		if statErr != nil || pathErr != nil || !os.SameFile(before, after) || !os.SameFile(before, pathInfo) ||
			before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			receipt.ErrorCode = "input_changed_during_projection"
		}
	}
	receipt.ProjectionComplete = receipt.ErrorCode == ""
	if err := json.NewEncoder(receiptFile).Encode(receipt); err != nil || receiptFile.Sync() != nil {
		return errors.New("receipt_persistence_failed")
	}
	if !receipt.ProjectionComplete {
		return errors.New(receipt.ErrorCode)
	}
	return nil
}

// Root runs this only after the original diagnostic owner has closed its log.
// Its status/receipt is independent of the original Go and jq outcomes.
func main() {
	if os.Geteuid() != 0 || len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: projector INPUT_JSONL NEW_EVENTS_JSONL NEW_RECEIPT_JSON")
		os.Exit(2)
	}
	if err := runProjection(os.Args[1], os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, "projection_failed:", err.Error())
		os.Exit(1)
	}
	fmt.Println("projection_complete; original native outcome unchanged")
}
