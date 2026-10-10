// Terminal execution diagnostics count observed typed causes, independently
// of later finalization, retry writes and financial transaction outcomes.
package task

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

var taskExecutionErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "execution_errors_total",
	Help:      "Errored task function executions by finite registered task, caller attribution and typed cause; includes drained and missing targets, not finalization or financial commits.",
}, []string{"task", "attribution", "cause"})

// Never parse error text or invoke custom Is/As methods. Mixed leaves and
// incomplete graphs stay explicit; the historical DB marker adds no cause
// when its preserved physical cancellation or SQL error is available.
func taskExecutionErrorCause(err error) (cause string) {
	cause = "unknown"
	defer func() {
		if recover() != nil {
			cause = "unknown"
		}
	}()
	if err == nil {
		return "none"
	}
	inspection := server.InspectErrorCauseBatch(err)
	if !inspection.Complete || inspection.NilBranches != 0 {
		return "unknown"
	}
	observed := ""
	dbMarker := false
	for _, node := range inspection.Nodes {
		if !node.Leaf {
			continue
		}
		current := "other"
		switch node.Err {
		case context.Canceled:
			current = "canceled"
		case context.DeadlineExceeded:
			current = "deadline"
		case server.DbContextDoneError:
			dbMarker = true
			continue
		case ErrDrained:
			current = "drained"
		case ErrTargetNotFound:
			current = "target_not_found"
		default:
			if pgErr, ok := node.Err.(*pgconn.PgError); ok {
				switch pgErr.Code {
				case "55P03":
					current = "postgres_lock"
				case "57014":
					current = "postgres_canceled"
				case "40001":
					current = "postgres_serialization"
				case "40P01":
					current = "postgres_deadlock"
				case "53300":
					current = "postgres_capacity"
				case "08000", "08001", "08003", "08004", "08006", "08007", "08P01":
					current = "postgres_connection"
				default:
					current = "postgres_other"
				}
			}
		}
		if observed != "" && current != observed {
			return "mixed"
		}
		observed = current
	}
	if observed != "" {
		return observed
	}
	if dbMarker {
		return "db_context_done"
	}
	return "unknown"
}
