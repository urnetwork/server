//go:build linux

// Signal and diagnostic ownership encloses one command's entire cleanup path.
// The private operation seam is bound only to run by the production main.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// Diagnostics are attempted only after the operation has returned from its
// independent native joins. Reporter failure cannot replace that operation's
// original error or terminate the process before these deferred owners close.
func runSubevaluationCommand(args []string, input io.Reader, output io.Writer, diagnostic *os.File,
	operation func(context.Context, []string, io.Reader, io.Writer, io.Writer) error, settings subevaluationDiagnosticSettings) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer cancel()
	writer := newSubevaluationDiagnostic(diagnostic, settings)
	defer writer.Close()
	if operation == nil {
		return errors.New("subevaluation command has no owner")
	}
	// Management placement and native clients require the original physical
	// descriptor; the reporter must never replace that inherited transport.
	err := operation(ctx, args, input, output, diagnostic)
	if err != nil {
		// No request, hidden seed, key or evaluator output reaches this message.
		_, _ = fmt.Fprintln(writer, "subevaluation operation failed")
	}
	return err
}
