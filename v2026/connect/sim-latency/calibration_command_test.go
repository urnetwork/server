// Scoring-slot and CLI checks use private synthetic files, never the live lock.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
)

// A second descriptor cannot enter while the callback owns the existing slot;
// closing the first descriptor makes a later operation immediately available.
func TestCalibrationScoringSlotExcludesConcurrentOperation(t *testing.T) {
	root := newCalibrationFileRoot(t)
	lock := filepath.Join(root, "scoring.lock")
	if err := os.WriteFile(lock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err := withCalibrationScoringSlot(lock, func() (*calibrationAnalysisReport, error) {
		calls++
		_, err := withCalibrationScoringSlot(lock, func() (*calibrationAnalysisReport, error) {
			calls++
			return &calibrationAnalysisReport{}, nil
		})
		if err == nil {
			t.Error("occupied scoring slot admitted a second operation")
		}
		return &calibrationAnalysisReport{}, nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("slot isolation failed: calls=%d error=%v", calls, err)
	}
	_, err = withCalibrationScoringSlot(lock, func() (*calibrationAnalysisReport, error) {
		calls++
		return &calibrationAnalysisReport{}, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("slot was not released: calls=%d error=%v", calls, err)
	}
}

// A typo must not create a different lock that bypasses the real evaluator.
func TestCalibrationScoringSlotNeverCreatesMissingLock(t *testing.T) {
	path := filepath.Join(newCalibrationFileRoot(t), "missing.lock")
	called := false
	_, err := withCalibrationScoringSlot(path, func() (*calibrationAnalysisReport, error) {
		called = true
		return &calibrationAnalysisReport{}, nil
	})
	if err == nil || called {
		t.Fatalf("missing slot admitted work: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("missing lock was created: %v", err)
	}
}

// Symlink aliases cannot substitute a different lock identity.
func TestCalibrationScoringSlotRejectsAlias(t *testing.T) {
	root := newCalibrationFileRoot(t)
	lock, alias := filepath.Join(root, "actual.lock"), filepath.Join(root, "alias.lock")
	if err := os.WriteFile(lock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(lock, alias); err != nil {
		t.Fatal(err)
	}
	_, err := withCalibrationScoringSlot(alias, func() (*calibrationAnalysisReport, error) {
		t.Error("aliased lock admitted work")
		return &calibrationAnalysisReport{}, nil
	})
	if err == nil {
		t.Fatal("lock alias accepted")
	}
}

// The complete operator contract parses without weakening an existing route or
// adding a candidate-controlled output/command option.
func TestCalibrationCommandRequiresPinnedInputsAndSlot(t *testing.T) {
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	args := []string{"calibrate", "--calibration-inputs=/synthetic/input.json",
		"--calibration-inputs-sha256=" + strings.Repeat("a", 64), "--calibration-lock=/synthetic/scoring.lock", "--calibration-cpuset=0,1"}
	opts, err := parser.ParseArgs(simulationUsage(), args, "synthetic-version")
	if err != nil || !optBool(opts, "calibrate") || optString(opts, "--calibration-cpuset", "") != "0,1" {
		t.Fatalf("complete calibration CLI rejected: %v", err)
	}
	for i := 1; i < len(args); i++ {
		missing := append([]string(nil), args[:i]...)
		missing = append(missing, args[i+1:]...)
		if _, err := parser.ParseArgs(simulationUsage(), missing, "synthetic-version"); err == nil {
			t.Errorf("missing required argument accepted: %s", args[i])
		}
	}
}
