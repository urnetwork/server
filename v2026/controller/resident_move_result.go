package controller

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Result reports of server-directed moves (connect/MOVE.md §5.1). A client
// reports to the control id when it declines a move or its replacement
// transport fails; a move that succeeds shows as an arrival instead. The report
// lands on whichever process runs ConnectControlFrames for the client (a connect
// resident, api `/connect/control`, the proxy's in-process control or
// taskworker egress control), so it is validated against the client's move
// intent in Redis, never process memory: it is accepted only while the intent
// holds the reported move.
//
// An accepted report is counted by outcome and reason, removes the intent (no
// arrival is coming), and a failure counts against the target ip's breaker
// (§5.5) unless the move was a drain's. A report is client input: it can never
// open a breaker alone (the breaker needs a minimum of sends), the counters
// have fixed label sets, and the detail is logged only at V(1).

// the `cause` label values of urnetwork_connect_move_results_dropped_total
const (
	residentMoveResultDropInvalidMoveId = "invalid_move_id"
	residentMoveResultDropInvalidResult = "invalid_result"
	residentMoveResultDropNoIntent      = "no_intent"
	residentMoveResultDropForeignMove   = "foreign_move"
)

var residentMovesDeclinedCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "moves_declined_total",
		Help:      "Accepted client reports that declined a server-directed move, by reason",
	},
	[]string{"reason"},
)

var residentMovesFailedCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "moves_failed_total",
		Help:      "Accepted client reports that a server-directed move's replacement transport failed, by reason and target host",
	},
	[]string{"reason", "target_host"},
)

var residentMoveResultsDroppedCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "move_results_dropped_total",
		Help:      "Move result reports dropped: an invalid move id or result, no move intent for the client, or the intent of another move",
	},
	[]string{"cause"},
)

// the `reason` label of each protocol reason, and of a reason outside the enum
var residentMoveReasonLabels = map[protocol.ResidentMoveReason]string{
	protocol.ResidentMoveReason_ResidentMoveReasonOther:                     "other",
	protocol.ResidentMoveReason_ResidentMoveReasonExtenderCarrier:           "extender_carrier",
	protocol.ResidentMoveReason_ResidentMoveReasonDnsCarrier:                "dns_carrier",
	protocol.ResidentMoveReason_ResidentMoveReasonNoFamilyTarget:            "no_family_target",
	protocol.ResidentMoveReason_ResidentMoveReasonMemoryBudget:              "memory_budget",
	protocol.ResidentMoveReason_ResidentMoveReasonRecentMove:                "recent_move",
	protocol.ResidentMoveReason_ResidentMoveReasonTargetFailedRecently:      "target_failed_recently",
	protocol.ResidentMoveReason_ResidentMoveReasonMobileBackgroundOrMetered: "mobile_background_or_metered",
	protocol.ResidentMoveReason_ResidentMoveReasonMoveInProgress:            "move_in_progress",
	protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout:            "connect_timeout",
	protocol.ResidentMoveReason_ResidentMoveReasonDialError:                 "dial_error",
	protocol.ResidentMoveReason_ResidentMoveReasonTlsError:                  "tls_error",
	protocol.ResidentMoveReason_ResidentMoveReasonAuthError:                 "auth_error",
}

const residentMoveReasonUnknownLabel = "unknown"

func init() {
	prometheus.MustRegister(
		residentMovesDeclinedCounter,
		residentMovesFailedCounter,
		residentMoveResultsDroppedCounter,
	)
	for _, label := range residentMoveReasonLabels {
		residentMovesDeclinedCounter.WithLabelValues(label)
	}
	residentMovesDeclinedCounter.WithLabelValues(residentMoveReasonUnknownLabel)
	for _, cause := range []string{
		residentMoveResultDropInvalidMoveId,
		residentMoveResultDropInvalidResult,
		residentMoveResultDropNoIntent,
		residentMoveResultDropForeignMove,
	} {
		residentMoveResultsDroppedCounter.WithLabelValues(cause)
	}
}

// The bounded label of a client-supplied reason.
func residentMoveReasonLabel(reason protocol.ResidentMoveReason) string {
	if label, ok := residentMoveReasonLabels[reason]; ok {
		return label
	}
	return residentMoveReasonUnknownLabel
}

func dropResidentMoveResult(clientId server.Id, cause string) {
	residentMoveResultsDroppedCounter.WithLabelValues(cause).Inc()
	if glog.V(1) {
		glog.Infof("[move]result dropped client=%s cause=%s\n", clientId, cause)
	}
}

// Applies one move result report from clientId. A report that does not hold
// the client's current move is dropped and counted, not an error; a store
// failure is the frame's error.
func ReportResidentMoveResult(ctx context.Context, clientId server.Id, result *protocol.ResidentMoveResult) error {
	moveId, err := server.IdFromBytes(result.MoveId)
	if err != nil {
		dropResidentMoveResult(clientId, residentMoveResultDropInvalidMoveId)
		return nil
	}
	switch result.Outcome {
	case protocol.ResidentMoveOutcome_ResidentMoveOutcomeDeclined, protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed:
	default:
		dropResidentMoveResult(clientId, residentMoveResultDropInvalidResult)
		return nil
	}
	intent, err := model.GetResidentMoveIntent(ctx, clientId)
	if err != nil {
		return err
	}
	if intent == nil {
		dropResidentMoveResult(clientId, residentMoveResultDropNoIntent)
		return nil
	}
	if intent.MoveId != moveId {
		dropResidentMoveResult(clientId, residentMoveResultDropForeignMove)
		return nil
	}

	reason := residentMoveReasonLabel(result.Reason)
	failed := result.Outcome == protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed
	if failed {
		residentMovesFailedCounter.WithLabelValues(reason, intent.TargetHost).Inc()
	} else {
		residentMovesDeclinedCounter.WithLabelValues(reason).Inc()
	}
	if glog.V(1) {
		glog.Infof(
			"[move]result client=%s move=%s kind=%s failed=%t reason=%s family=%d elapsed=%dms\n",
			clientId,
			moveId,
			intent.Kind,
			failed,
			reason,
			result.IpFamily,
			result.Elapsed,
		)
	}
	// no arrival is coming for this move
	if _, err := model.DeleteResidentMoveIntent(ctx, clientId, moveId); err != nil {
		return err
	}
	// a drain points the client at its own host, which says nothing about the
	// mover's targets
	if failed && intent.Kind != model.ResidentMoveKindDrain {
		if target, ok := intent.Target(result.IpFamily); ok {
			if _, err := model.RecordResidentMoveTargetFailure(ctx, target); err != nil {
				return err
			}
		}
	}
	return nil
}
