// Terminal command controls use owned empty collectors and synthetic errors;
// no runtime, database, child process or production request is constructed.
package main

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/server/controller"
)

// Installing the optional observer cannot turn a healthy or canceled worker
// into a panic or emit a new healthy-path diagnostic document.
func TestCompetitionWorkerFenceObservationFinisherKeepsHealthyAndCanceledQuiet(t *testing.T) {
	for _, err := range []error{nil, context.Canceled, fmt.Errorf("synthetic canceled wrapper: %w", context.Canceled)} {
		_, observation := controller.ObserveCompetitionWorkerFailure(t.Context())
		var output bytes.Buffer
		finishCompetitionWorkerObserved(err, true, &output, observation)
		if output.Len() != 0 {
			t.Fatal("healthy or canceled supervised call emitted failure diagnostics")
		}
	}
}

// A collector without a selected fence failure stays byte-compatible with the
// legacy document and retains the exact existing panic that yields exit two.
func TestCompetitionWorkerFenceObservationFinisherRetainsLegacyFailure(t *testing.T) {
	_, observation := controller.ObserveCompetitionWorkerFailure(t.Context())
	var output bytes.Buffer
	defer func() {
		if recovered := recover(); recovered != "competition worker failed; bounded diagnostics emitted" {
			t.Fatal("observed finisher changed the existing panic", recovered)
		}
		legacy, err := controller.MarshalCompetitionWorkerFailureReport(context.DeadlineExceeded)
		if err != nil || !bytes.Equal(output.Bytes(), append(legacy, '\n')) {
			t.Fatal("an empty collector changed the legacy failure document", err)
		}
	}()
	finishCompetitionWorkerObserved(context.DeadlineExceeded, true, &output, observation)
}
