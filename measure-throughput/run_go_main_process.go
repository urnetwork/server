package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/pterm/pterm"
)

func runGoMainProcess(ctx context.Context, name string, progressWriter io.Writer, mainDir string, args ...string) (err error) {

	spinner, err := pterm.DefaultSpinner.
		WithWriter(progressWriter).
		Start("Running " + name)

	if err != nil {
		return fmt.Errorf("failed to create spinner: %w", err)
	}

	defer func() {
		if err != nil {
			spinner.Fail(fmt.Sprintf("%s failed: %v", name, err))
		}
		spinner.Stop()
	}()

	cmd := exec.CommandContext(ctx, "go", append([]string{"run", "."}, args...)...)
	cmd.Dir = mainDir

	cmd.Env = os.Environ()

	out := bytes.NewBuffer(nil)
	cmd.Stdout = out
	cmd.Stderr = out

	err = cmd.Run()

	if err != nil {
		fmt.Println("err:", err, out.String())
		return err
	}

	return nil
}
