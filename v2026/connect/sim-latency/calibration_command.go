// Offline calibration holds the same scoring slot through authentication,
// immutable-image replay, cleanup and reduction. It never changes live policy.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/docopt/docopt-go"
)

// Opens only an existing canonical lock: creating a different missing lock
// would not exclude the evaluator that owns the actual host slot.
func withCalibrationScoringSlot(lockPath string, do func() (*calibrationAnalysisReport, error)) (*calibrationAnalysisReport, error) {
	if !filepath.IsAbs(lockPath) || filepath.Clean(lockPath) != lockPath {
		return nil, errors.New("calibration needs the canonical existing scoring lock")
	}
	resolved, err := filepath.EvalSymlinks(lockPath)
	if err != nil || resolved != lockPath {
		return nil, errors.New("calibration scoring lock is unavailable or redirected")
	}
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), lockPath)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("calibration scoring lock is not a regular file")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.New("calibration cannot overlap an occupied scoring slot")
	}
	return do()
}

// The production callback invokes only the frozen scorer, with no test/model
// provenance inferred from synthetic numerical fixtures.
func executeCalibrationCommand(ctx context.Context, manifestPath string, manifestSha256 string, lockPath string,
	settings calibrationAnalysisSettings, replaySettings calibrationReplaySettings, docker sourceBuildDocker) (*calibrationAnalysisReport, error) {
	if runtime.Version() != "go1.27.3" {
		return &calibrationAnalysisReport{
			Kind: "sim-latency-calibration-analysis", InputManifestSha256: manifestSha256,
			Reasons: []string{"wrong_competition_toolchain"},
		}, errors.New("competition calibration requires actual Go 1.27.3")
	}
	observations := []calibrationReplayObservation{}
	replaySettings.Observe = func(observation calibrationReplayObservation) {
		observations = append(observations, observation)
	}
	report, err := withCalibrationScoringSlot(lockPath, func() (*calibrationAnalysisReport, error) {
		return analyzeCalibrationFiles(ctx, manifestPath, manifestSha256, settings,
			func(ctx context.Context, root string, inputs ScoreInputs, image string, roundId string, baseline bool) ([]byte, error) {
				return replayCalibrationBundle(ctx, root, inputs, image, roundId, baseline, settings, replaySettings, docker)
			})
	})
	if report == nil {
		report = &calibrationAnalysisReport{
			Kind: "sim-latency-calibration-analysis", InputManifestSha256: manifestSha256,
			Reasons: []string{"calibration_input_or_replay_rejected"},
		}
	}
	report.ReplayObservations = observations
	return report, err
}

// Writes JSON only to stdout; no option can overwrite original evidence.
func runCalibrationAnalysis(opts docopt.Opts) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	replaySettings := defaultCalibrationReplaySettings()
	replaySettings.CpuSet = optString(opts, "--calibration-cpuset", "")
	report, err := executeCalibrationCommand(ctx,
		optString(opts, "--calibration-inputs", ""), optString(opts, "--calibration-inputs-sha256", ""),
		optString(opts, "--calibration-lock", ""), defaultCalibrationAnalysisSettings(), replaySettings, runSourceBuildDocker)
	content, marshalErr := json.MarshalIndent(report, "", "  ")
	if marshalErr != nil {
		err = errors.Join(err, marshalErr)
		content = []byte(`{"kind":"sim-latency-calibration-analysis","accepted":false,"reasons":["invalid_analysis_output"]}`)
	}
	if _, writeErr := os.Stdout.Write(append(content, '\n')); writeErr != nil {
		err = errors.Join(err, writeErr)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "calibration rejected: %v\n", err)
		os.Exit(1)
	}
}
