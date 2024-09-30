package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/jedib0t/go-pretty/v6/progress"
)

func runGoMainProcess(ctx context.Context, name string, pw progress.Writer, mainDir string, args ...string) (err error) {

	tracker := &progress.Tracker{
		Message: fmt.Sprintf("Running %s", name),
		Total:   0,
		// Units:   *units,
	}

	pw.AppendTracker(tracker)
	tracker.Start()

	tracker.Increment(1)

	defer func() {
		if err != nil {
			tracker.UpdateMessage(fmt.Sprintf("%s failed: %v", name, err))
			tracker.MarkAsErrored()
			return
		}
		tracker.UpdateMessage(fmt.Sprintf("%s is done", name))
		tracker.MarkAsDone()
	}()

	cmd := exec.CommandContext(ctx, "go", append([]string{"run", "."}, args...)...)
	cmd.Dir = mainDir

	cmd.Env = os.Environ()

	out, err := cmd.CombinedOutput()

	if err != nil {
		fmt.Println("err:", err, string(out))
		return err
	}

	return nil
}
