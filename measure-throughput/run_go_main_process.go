package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/jedib0t/go-pretty/v6/progress"
)

func runGoMainProcess(ctx context.Context, name string, pw progress.Writer, mainDir string, args ...string) (err error) {

	tracker := &progress.Tracker{
		Message: fmt.Sprintf("%s is running", name),
		Total:   0,
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
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Pgid:    0,
	}

	cmd.Env = os.Environ()
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err != nil {
			tracker.UpdateMessage(fmt.Sprintf("%s failed to kill group: %v", name, err))
			return err
		}
		tracker.UpdateMessage(fmt.Sprintf("%s killing process group %d", name, -cmd.Process.Pid))
		return nil
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err = cmd.Run()

	if err != nil {
		if strings.Contains(err.Error(), "signal: killed") {
			return nil
		}
		// fmt.Println("err:", err)
		return err
	}

	return nil
}
