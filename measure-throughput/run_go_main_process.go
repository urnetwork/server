package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/jedib0t/go-pretty/v6/progress"
)

func runGoMainProcess(ctx context.Context, name string, pw progress.Writer, mainDir, tempDir string, args ...string) (err error) {

	tracker := &progress.Tracker{
		Message: fmt.Sprintf("Starting %s", name),
		Total:   2,
	}

	pw.AppendTracker(tracker)
	tracker.Start()

	defer func() {
		if ctx.Err() != nil {
			err = nil
		}

		if err != nil {
			tracker.UpdateMessage(fmt.Sprintf("%s failed: %v", name, err))
			tracker.MarkAsErrored()
			return
		}
		tracker.UpdateMessage(fmt.Sprintf("%s terminated", name))
		tracker.MarkAsDone()
	}()

	tracker.UpdateMessage(fmt.Sprintf("Building %s", name))

	binaryPath := filepath.Join(tempDir, name)

	buildCmd := exec.CommandContext(ctx, "go", "build", "-o", binaryPath, ".")
	buildCmd.Dir = mainDir
	buildCmd.Env = os.Environ()

	out, err := buildCmd.CombinedOutput()
	if err != nil {
		pw.Log(fmt.Sprintf("failed to build %s: %v\n%s", name, err, string(out)))
		return fmt.Errorf("failed to build %s: %w\n%s", name, err, string(out))
	}

	tracker.Increment(1)

	tracker.UpdateMessage(fmt.Sprintf("Running %s", name))

	cmd := exec.CommandContext(ctx, binaryPath, args...)
	cmd.Dir = mainDir

	cmd.Env = os.Environ()

	out, err = cmd.CombinedOutput()

	if err != nil {
		pw.Log(fmt.Sprintf("failed to run %s: %v\n%s", name, err, string(out)))
		return fmt.Errorf("failed to run %s: %w\n%s", name, err, string(out))
	}

	return nil
}
