// The real CLI terminal branch keeps exit-two panic behavior while emitting
// only the supervisor's finite report. Private causes never become panic text.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/controller"
)

// Before the finite-report branch, this exact call panics with private cause
// bytes and writes no document; no process scheduling is needed to reproduce it.
func TestCompetitionWorkerSupervisedFailureEmitsFiniteReportBeforePanic(t *testing.T) {
	const private = "synthetic-private-supervised-cause"
	var output bytes.Buffer
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		finishCompetitionWorker(errors.New(private), true, &output)
	}()
	report, err := controller.DecodeCompetitionWorkerFailureReport(bytes.TrimSpace(output.Bytes()))
	if err != nil || report.Category != "other" || recovered == nil || strings.Contains(fmt.Sprint(recovered), private) || strings.Contains(output.String(), private) {
		t.Fatal("supervised exit lost its document, changed status behavior or leaked private bytes", recovered, err)
	}
}

// Healthy/canceled shutdown remains successful and does not invent a report.
func TestCompetitionWorkerCanceledShutdownDoesNotEmitFailureReport(t *testing.T) {
	for _, cause := range []error{nil, context.Canceled} {
		var output bytes.Buffer
		finishCompetitionWorker(cause, true, &output)
		if output.Len() != 0 {
			t.Fatal("ordinary shutdown acquired a failure document")
		}
	}
}
