// A policy refusal separates unstarted windows from live request owners.
package perfvar

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/sdk"
)

func TestSyntheticFastPreflightRefusalLeavesNoRequestOwner(t *testing.T) {
	control := syntheticFastMeanControl([5]float64{210, 210, 210, 210, 210})
	beforeErr := errors.New("synthetic Fast default window policy was not live before measurement")
	afterErr := errors.New("synthetic Fast default window policy was not live after measurement")
	origin := &syntheticFastOrigin{}
	ready := true
	executed, quiesced, handled := 0, 0, 0
	var joined [5]bool
	sequence, err := measureSyntheticFastSequence(t.Context(), control.Runs[0].Workload.Settings, 5, "owned-window-preflight",
		func(ctx context.Context, index int) (syntheticFastReport, error) {
			executed++
			owner, err := syntheticFastCarrierBeginOriginRun(origin, ctx, index, false, nil)
			if err != nil {
				return syntheticFastReport{}, err
			}
			// The baseline checked readiness after publishing the origin.
			// Preserve that branch in the test so RED reaches the actual
			// empty workload path from the failed five-window run.
			if !ready {
				return syntheticFastReport{}, beforeErr
			}
			release, admitted := owner.enter()
			if !admitted {
				t.Fatal("a ready original failed to admit its owned handler")
			}
			handled++
			release()
			workload := control.Runs[index].Workload
			if index == 3 {
				ready = false
				return workload, afterErr
			}
			return workload, nil
		},
		func(ctx context.Context, index int) error {
			quiesced++
			err := origin.runs[index].Load().sealAndWait(ctx)
			joined[index] = err == nil && index < handled
			return err
		},
		func() (syntheticFastCumulative, any) {
			return syntheticFastCumulative{Offered: uint64(handled)}, ready
		},
		func() error {
			if !ready {
				return beforeErr
			}
			return nil
		})
	if !errors.Is(err, beforeErr) || !errors.Is(err, afterErr) || sequence.Valid {
		t.Fatalf("the actual window policy failures were lost: sequence=%s err=%v", sequence.Stop, err)
	}
	if executed != 4 || quiesced != 4 || handled != 4 || origin.runs[4].Load() != nil ||
		sequence.Stop != "pre_measurement_policy_failure" || strings.Contains(err.Error(), "request ownership did not quiesce") {
		t.Fatalf("an unstarted Fast window was counted as an active owner: executed=%d quiesced=%d handled=%d stop=%s err=%v",
			executed, quiesced, handled, sequence.Stop, err)
	}
	measured, refused := sequence.Runs[3], sequence.Runs[4]
	if measured.State != "invalid" || measured.Error != afterErr.Error() || !measured.RequestsQuiescent || !measured.Workload.Joined ||
		refused.State != "NOTRUN" || refused.Error != beforeErr.Error() || refused.NotRunReason != sequence.Stop ||
		refused.BeforeDetails != false || refused.AfterDetails != nil || refused.Workload.Joined || refused.RequestsQuiescent {
		t.Fatal("policy failure changed measured or unstarted ownership evidence")
	}
	if _, err := syntheticFastFiveWindowMean(sequence); err == nil {
		t.Fatal("a policy failure qualified for the five-window throughput gate")
	}

	native := syntheticFastNativeIterationReport{
		ConsumerTarget: 64 * 1024 * 1024, ProviderTarget: 128 * 1024 * 1024, ProviderCount: 12,
		Sequence: sequence, WorkerFinalJoined: joined,
		OriginHandlersStarted: int64(handled), OriginHandlersFinished: int64(handled),
		ClientAfterJoin: &sdk.DeviceLocalMemoryUsage{}, ProviderAfterJoin: make([]*sdk.DeviceLocalMemoryUsage, 12),
	}
	for index := range native.ProviderAfterJoin {
		native.ProviderAfterJoin[index] = &sdk.DeviceLocalMemoryUsage{}
	}
	if err := syntheticFastNativeIterationJoinError(native); err != nil {
		t.Fatalf("the four real joined windows lost their cleanup receipt: %v", err)
	}
	native.WorkerFinalJoined[3] = false
	if err := syntheticFastNativeIterationJoinError(native); err == nil || !strings.Contains(err.Error(), "workload owner did not join") {
		t.Fatalf("a started owner lost its strict final join requirement: %v", err)
	}
	native.WorkerFinalJoined[3] = true
	native.WorkerFinalJoined[4] = true
	if err := syntheticFastNativeIterationJoinError(native); err == nil || !strings.Contains(err.Error(), "unstarted workload claimed a worker join") {
		t.Fatalf("an impossible fifth worker join was accepted: %v", err)
	}
}
