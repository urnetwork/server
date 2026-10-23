package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Move result reports (connect/MOVE.md §5.1) through ConnectControlFrames, the
// internal path a connect resident uses, and through the HTTP control path.

var residentMoveResultTestTargets = []netip.Addr{
	netip.MustParseAddr("198.51.100.21"),
	netip.MustParseAddr("2001:db8::21"),
}

// Writes the client's intent of a fresh move and opens the targets' breaker
// windows with one send each.
func setResidentMoveResultTestIntent(ctx context.Context, t testing.TB, clientId server.Id, kind model.ResidentMoveKind) *model.ResidentMoveIntent {
	t.Helper()
	intent := &model.ResidentMoveIntent{
		MoveId:         server.NewId(),
		Kind:           kind,
		FromResidentId: server.NewId(),
		FromExchangeId: server.NewId(),
		TargetHost:     "alpha-edge-1",
		Targets:        residentMoveResultTestTargets,
	}
	if err := model.SetResidentMoveIntent(ctx, clientId, intent, time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, target := range intent.Targets {
		if err := model.RecordResidentMoveTargetSend(ctx, target, 10*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	return intent
}

func residentMoveResultTestFrame(t testing.TB, result *protocol.ResidentMoveResult) *protocol.Frame {
	t.Helper()
	frame, err := connect.ToFrame(result, connect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// Applies one report through the internal control path.
func reportResidentMoveResultForTest(ctx context.Context, t testing.TB, clientId server.Id, result *protocol.ResidentMoveResult) {
	t.Helper()
	frames := []*protocol.Frame{residentMoveResultTestFrame(t, result)}
	defer returnConnectControlFrames(frames)
	outFrames, err := ConnectControlFrames(ctx, clientId, frames, connect.DefaultContractManagerSettings())
	defer returnConnectControlFrames(outFrames)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(outFrames) != 0 {
		t.Fatalf("a report produced replies: %d", len(outFrames))
	}
}

// The failures counted in a target's breaker window.
func residentMoveTargetFailuresForTest(ctx context.Context, t testing.TB, target netip.Addr) int64 {
	t.Helper()
	var failures int64
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.HGet(ctx, "{mtb_"+target.String()+"}w", "failures").Int64()
		if err != nil && !errors.Is(err, server.RedisNil) {
			t.Fatal(err)
		}
		failures = value
	})
	return failures
}

func residentMoveResultIntentForTest(ctx context.Context, t testing.TB, clientId server.Id) *model.ResidentMoveIntent {
	t.Helper()
	intent, err := model.GetResidentMoveIntent(ctx, clientId)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

// A report counts only while it holds the client's current move: an invalid
// id or result, a client without an intent, and another move's id are dropped
// and counted, and leave the intent alone.
func TestReportResidentMoveResultDropsReportsWithoutTheIntentMove(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		intent := setResidentMoveResultTestIntent(ctx, t, clientId, model.ResidentMoveKindAdvisory)

		for _, test := range []struct {
			cause    string
			clientId server.Id
			result   *protocol.ResidentMoveResult
		}{
			{
				cause:    residentMoveResultDropForeignMove,
				clientId: clientId,
				result: &protocol.ResidentMoveResult{
					MoveId:  server.NewId().Bytes(),
					Outcome: protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed,
					Reason:  protocol.ResidentMoveReason_ResidentMoveReasonDialError,
				},
			},
			{
				cause:    residentMoveResultDropInvalidMoveId,
				clientId: clientId,
				result: &protocol.ResidentMoveResult{
					MoveId:  []byte{1, 2, 3},
					Outcome: protocol.ResidentMoveOutcome_ResidentMoveOutcomeDeclined,
				},
			},
			{
				cause:    residentMoveResultDropInvalidResult,
				clientId: clientId,
				result: &protocol.ResidentMoveResult{
					MoveId:  intent.MoveId.Bytes(),
					Outcome: protocol.ResidentMoveOutcome(7),
				},
			},
			{
				// the same move id from a client that holds no intent
				cause:    residentMoveResultDropNoIntent,
				clientId: server.NewId(),
				result: &protocol.ResidentMoveResult{
					MoveId:  intent.MoveId.Bytes(),
					Outcome: protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed,
				},
			},
		} {
			droppedBefore := testutil.ToFloat64(residentMoveResultsDroppedCounter.WithLabelValues(test.cause))
			reportResidentMoveResultForTest(ctx, t, test.clientId, test.result)
			if dropped := testutil.ToFloat64(residentMoveResultsDroppedCounter.WithLabelValues(test.cause)); dropped != droppedBefore+1 {
				t.Fatalf("%s: dropped = %v, want %v", test.cause, dropped, droppedBefore+1)
			}
			if kept := residentMoveResultIntentForTest(ctx, t, clientId); kept == nil || kept.MoveId != intent.MoveId {
				t.Fatalf("%s: the intent changed: %+v", test.cause, kept)
			}
		}
		for _, target := range intent.Targets {
			if failures := residentMoveTargetFailuresForTest(ctx, t, target); failures != 0 {
				t.Fatalf("a dropped report counted %d failures for %s", failures, target)
			}
		}
	})
}

// An accepted failure is counted by reason and target host, removes the intent
// and counts against the reported family's target; a repeat finds no intent.
func TestReportResidentMoveResultAcceptsFailureForTheIntentMove(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		intent := setResidentMoveResultTestIntent(ctx, t, clientId, model.ResidentMoveKindAdvisory)

		failedBefore := testutil.ToFloat64(residentMovesFailedCounter.WithLabelValues("dial_error", "alpha-edge-1"))
		result := &protocol.ResidentMoveResult{
			MoveId:   intent.MoveId.Bytes(),
			Outcome:  protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed,
			Reason:   protocol.ResidentMoveReason_ResidentMoveReasonDialError,
			IpFamily: 6,
			Elapsed:  1500,
		}
		reportResidentMoveResultForTest(ctx, t, clientId, result)
		if failed := testutil.ToFloat64(residentMovesFailedCounter.WithLabelValues("dial_error", "alpha-edge-1")); failed != failedBefore+1 {
			t.Fatalf("failed = %v, want %v", failed, failedBefore+1)
		}
		if remaining := residentMoveResultIntentForTest(ctx, t, clientId); remaining != nil {
			t.Fatalf("the accepted report left the intent: %+v", remaining)
		}
		// the v6 target is charged, the v4 target is not
		if failures := residentMoveTargetFailuresForTest(ctx, t, intent.Targets[1]); failures != 1 {
			t.Fatalf("v6 target failures = %d, want 1", failures)
		}
		if failures := residentMoveTargetFailuresForTest(ctx, t, intent.Targets[0]); failures != 0 {
			t.Fatalf("v4 target failures = %d, want 0", failures)
		}

		droppedBefore := testutil.ToFloat64(residentMoveResultsDroppedCounter.WithLabelValues(residentMoveResultDropNoIntent))
		reportResidentMoveResultForTest(ctx, t, clientId, result)
		if dropped := testutil.ToFloat64(residentMoveResultsDroppedCounter.WithLabelValues(residentMoveResultDropNoIntent)); dropped != droppedBefore+1 {
			t.Fatalf("a repeated report was not dropped: %v", dropped)
		}
		if failures := residentMoveTargetFailuresForTest(ctx, t, intent.Targets[1]); failures != 1 {
			t.Fatalf("a repeated report counted another failure: %d", failures)
		}
	})
}

// A decline is counted by reason, removes the intent and charges no target; a
// reason outside the enum takes the unknown label; a drain move's failure
// charges no target of the mover.
func TestReportResidentMoveResultDeclinesAndDrainFailures(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientId := server.NewId()
		intent := setResidentMoveResultTestIntent(ctx, t, clientId, model.ResidentMoveKindAdvisory)
		declinedBefore := testutil.ToFloat64(residentMovesDeclinedCounter.WithLabelValues("memory_budget"))
		reportResidentMoveResultForTest(ctx, t, clientId, &protocol.ResidentMoveResult{
			MoveId:  intent.MoveId.Bytes(),
			Outcome: protocol.ResidentMoveOutcome_ResidentMoveOutcomeDeclined,
			Reason:  protocol.ResidentMoveReason_ResidentMoveReasonMemoryBudget,
		})
		if declined := testutil.ToFloat64(residentMovesDeclinedCounter.WithLabelValues("memory_budget")); declined != declinedBefore+1 {
			t.Fatalf("declined = %v, want %v", declined, declinedBefore+1)
		}
		if remaining := residentMoveResultIntentForTest(ctx, t, clientId); remaining != nil {
			t.Fatalf("the decline left the intent: %+v", remaining)
		}
		for _, target := range intent.Targets {
			if failures := residentMoveTargetFailuresForTest(ctx, t, target); failures != 0 {
				t.Fatalf("a decline charged %s", target)
			}
		}

		unknownClientId := server.NewId()
		unknownIntent := setResidentMoveResultTestIntent(ctx, t, unknownClientId, model.ResidentMoveKindAdvisory)
		unknownBefore := testutil.ToFloat64(residentMovesDeclinedCounter.WithLabelValues(residentMoveReasonUnknownLabel))
		reportResidentMoveResultForTest(ctx, t, unknownClientId, &protocol.ResidentMoveResult{
			MoveId:  unknownIntent.MoveId.Bytes(),
			Outcome: protocol.ResidentMoveOutcome_ResidentMoveOutcomeDeclined,
			Reason:  protocol.ResidentMoveReason(99),
		})
		if unknown := testutil.ToFloat64(residentMovesDeclinedCounter.WithLabelValues(residentMoveReasonUnknownLabel)); unknown != unknownBefore+1 {
			t.Fatalf("unknown reason = %v, want %v", unknown, unknownBefore+1)
		}

		drainClientId := server.NewId()
		drainIntent := setResidentMoveResultTestIntent(ctx, t, drainClientId, model.ResidentMoveKindDrain)
		reportResidentMoveResultForTest(ctx, t, drainClientId, &protocol.ResidentMoveResult{
			MoveId:   drainIntent.MoveId.Bytes(),
			Outcome:  protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed,
			Reason:   protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout,
			IpFamily: 4,
		})
		if failures := residentMoveTargetFailuresForTest(ctx, t, drainIntent.Targets[0]); failures != 0 {
			t.Fatalf("a drain failure charged the target %d", failures)
		}
		if remaining := residentMoveResultIntentForTest(ctx, t, drainClientId); remaining != nil {
			t.Fatalf("the drain report left the intent: %+v", remaining)
		}
	})
}

// The api `/connect/control` path accepts a report only for the client's own
// intent, with the credential's client as the source.
func TestConnectControlAcceptsMoveResultOverHttp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic move results", userId)
		model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "synthetic mover", "mover")
		clientSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, "synthetic move results", false, false).Client(deviceId, clientId))
		t.Cleanup(clientSession.Cancel)

		report := func(result *protocol.ResidentMoveResult) {
			frame := residentMoveResultTestFrame(t, result)
			defer returnConnectControlFrames([]*protocol.Frame{frame})
			wire, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{frame}})
			if err != nil {
				t.Fatal(err)
			}
			controlResult, err := ConnectControl(&ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(wire)}, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			if controlResult.Error != nil {
				t.Fatalf("report error: %s", controlResult.Error.Message)
			}
		}

		// another client's intent is not this client's
		otherClientId := server.NewId()
		otherIntent := setResidentMoveResultTestIntent(ctx, t, otherClientId, model.ResidentMoveKindAdvisory)
		report(&protocol.ResidentMoveResult{
			MoveId:  otherIntent.MoveId.Bytes(),
			Outcome: protocol.ResidentMoveOutcome_ResidentMoveOutcomeDeclined,
		})
		if kept := residentMoveResultIntentForTest(ctx, t, otherClientId); kept == nil {
			t.Fatal("a report removed another client's intent")
		}

		intent := setResidentMoveResultTestIntent(ctx, t, clientId, model.ResidentMoveKindCompulsory)
		report(&protocol.ResidentMoveResult{
			MoveId:   intent.MoveId.Bytes(),
			Outcome:  protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed,
			Reason:   protocol.ResidentMoveReason_ResidentMoveReasonTlsError,
			IpFamily: 4,
		})
		if remaining := residentMoveResultIntentForTest(ctx, t, clientId); remaining != nil {
			t.Fatalf("the accepted report left the intent: %+v", remaining)
		}
		if failures := residentMoveTargetFailuresForTest(ctx, t, intent.Targets[0]); failures != 1 {
			t.Fatalf("v4 target failures = %d, want 1", failures)
		}
	})
}
