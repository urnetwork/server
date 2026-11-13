// A complete measured latency miss is data, while its original goal stays
// failed. These scalar controls make no network or product qualification claim.
package perfvar

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sdk/v2026"
)

// A complete synthetic census reaches the real witness and pressure verdicts.
func newC48ReturnFlightMeasurementControl(t *testing.T, slow bool) (*nativeBrowsingObservation,
	nativeBrowsingColdLinkObservation, c48ReturnFlightOutcome, *c48ReturnFlightBoundary) {
	t.Helper()
	witness := newC48ReturnFlightVerdictControl(t)
	observation := &nativeBrowsingObservation{CompletedNavigations: 100, OverlapProofs: 99,
		Pressure: &nativeBrowsingPressureObservation{}}
	for page := range observation.Pages {
		observation.Pages[page].Page = page
		observation.Pages[page].FirstByteNanoseconds = int64(time.Millisecond)
		observation.Pages[page].CompletionNanoseconds = int64(2 * time.Millisecond)
	}
	if slow {
		limit := int64(sdk.DefaultDeviceLocalSettings().SendTimeout)
		observation.Pages[2].FirstByteNanoseconds = limit
		observation.Pages[2].CompletionNanoseconds = limit + int64(time.Millisecond)
		witness.row.CombinedFirstByteNs[2] = limit
	}
	witnessErr, numeric := witness.verdictWithLatencyForTest()
	latencyErr := nativeBrowsingPressureLatencyError(observation)
	cold, snapshotErr := witness.snapshot()
	if snapshotErr != nil {
		t.Fatal(snapshotErr)
	}
	outcome := c48ReturnFlightOutcome{ClassificationVersion: 2, VerdictEvaluated: true,
		LatencyGateFailed: latencyErr != nil || numeric, ColdChainLatencyGateFailed: numeric,
		WitnessVerdictFailed: witnessErr != nil && !numeric, GoalGatePassed: witnessErr == nil && latencyErr == nil}
	joined := &c48ReturnFlightBoundary{Joined: true,
		Device: &sdk.DeviceLocalMemoryUsage{TargetByteCount: 32 * 1024 * 1024}}
	return observation, cold, outcome, joined
}

// Eligibility does not require the absence of the measured stall. A complete
// positive exceedance stays valid data and keeps its raw goal failed.
func TestC48ReturnFlightCompleteLatencyMissRemainsValidData(t *testing.T) {
	for _, slow := range []bool{false, true} {
		observation, cold, outcome, joined := newC48ReturnFlightMeasurementControl(t, slow)
		if !c48ReturnFlightLatencyDataValid(observation, cold, outcome, joined) {
			t.Fatalf("complete measured observation was excluded: slow=%t outcome=%+v", slow, outcome)
		}
		if outcome.GoalGatePassed == slow || outcome.LatencyGateFailed != slow {
			t.Fatalf("metric validity changed original goal result: slow=%t outcome=%+v", slow, outcome)
		}
	}
}

// Missing rows, partial/corrupt work, deadlines and failed owners cannot donate
// selected fast values to the same fixed100 latency observation.
func TestC48ReturnFlightInvalidMeasurementCannotDonateLatency(t *testing.T) {
	for _, kind := range []string{"missing-time", "missing-html-time", "wrong-page", "incomplete100", "incomplete400", "owner", "cleanup", "deadline", "joined-payload", "marked-owner"} {
		observation, cold, outcome, joined := newC48ReturnFlightMeasurementControl(t, true)
		switch kind {
		case "missing-time":
			cold.CombinedFirstByteNs[99] = 0
		case "missing-html-time":
			observation.Pages[99].FirstByteNanoseconds = 0
		case "wrong-page":
			observation.Pages[99].Page = 98
		case "incomplete100":
			observation.CompletedNavigations = 99
		case "incomplete400":
			cold.Assets = 399
		case "owner":
			joined.Device.TransferRootUsedByteCount = 1
		case "cleanup":
			outcome.CleanupFailed = true
		case "deadline", "joined-payload", "marked-owner":
			var err error = context.DeadlineExceeded
			if kind == "joined-payload" {
				err = errors.Join(err, errNativeBrowsingResponseMismatch)
			}
			if kind == "marked-owner" {
				err = &c48ReturnFlightWorkloadOwnerFailure{cause: err}
			}
			outcome.WorkloadErrorClass = c48ReturnFlightWorkloadErrorClass(err)
			outcome.WorkloadDeadlineOnly = c48ReturnFlightDeadlineOnly(err)
			outcome.WorkloadOwnershipFailed = c48ReturnFlightWorkloadOwnershipFailed(err)
			outcome.WorkloadAdditionalFailure = errors.Is(err, context.DeadlineExceeded) && !outcome.WorkloadDeadlineOnly
			outcome.PayloadMismatch = errors.Is(err, errNativeBrowsingResponseMismatch)
			outcome.VerdictEvaluated = false
		}
		if c48ReturnFlightLatencyDataValid(observation, cold, outcome, joined) {
			t.Fatalf("invalid observation donated latency: kind=%s outcome=%+v", kind, outcome)
		}
	}
}
