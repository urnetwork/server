// Provider diagnostics name a fixed operation while retaining its original cause.
package model

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

type legacyProviderTotalsPhase uint8

const (
	legacyProviderTotalsTransactionStart legacyProviderTotalsPhase = iota
	legacyProviderTotalsConfigure
	legacyProviderTotalsBody
	legacyProviderTotalsPendingRead
	legacyProviderTotalsAllocation
	legacyProviderTotalsAccountWrite
	legacyProviderTotalsAppliedMarker
	legacyProviderTotalsCommit
	legacyProviderTotalsBatchWait
)

func (self legacyProviderTotalsPhase) String() string {
	switch self {
	case legacyProviderTotalsTransactionStart:
		return "transaction_start"
	case legacyProviderTotalsConfigure:
		return "configure"
	case legacyProviderTotalsBody:
		return "body"
	case legacyProviderTotalsPendingRead:
		return "pending_read"
	case legacyProviderTotalsAllocation:
		return "allocation"
	case legacyProviderTotalsAccountWrite:
		return "account_write"
	case legacyProviderTotalsAppliedMarker:
		return "applied_marker"
	case legacyProviderTotalsCommit:
		return "commit"
	case legacyProviderTotalsBatchWait:
		return "batch_wait"
	default:
		return "unknown"
	}
}

type legacyProviderTotalsPhaseError struct {
	phase legacyProviderTotalsPhase
	cause error
}

// Never format the cause's text, SQL, arguments or identifiers. Typed inspection
// still sees the exact original error; classification here changes no retry rule.
func (self *legacyProviderTotalsPhaseError) Error() string {
	message := "legacy provider totals phase=" + self.phase.String() + " failed"
	code := ""
	canceled, deadline := false, false
	for _, node := range server.InspectErrorCauses(self.cause).Nodes {
		if pgErr, ok := node.Err.(*pgconn.PgError); ok && pgErr != nil {
			valid := len(pgErr.Code) == 5
			if valid {
				for _, value := range pgErr.Code {
					valid = valid && ('0' <= value && value <= '9' || 'A' <= value && value <= 'Z')
				}
			}
			if valid {
				if code == "" {
					code = pgErr.Code
				} else if code != pgErr.Code {
					code = "mixed"
				}
			}
		}
		canceled = canceled || node.Err == context.Canceled
		deadline = deadline || node.Err == context.DeadlineExceeded
	}
	if code != "" {
		message += " (SQLSTATE " + code + ")"
	}
	if deadline {
		message += "; context deadline exceeded"
	}
	if canceled {
		message += "; context canceled"
	}
	return message
}

func (self *legacyProviderTotalsPhaseError) Unwrap() error { return self.cause }

func withLegacyProviderTotalsPhase(phase legacyProviderTotalsPhase, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*legacyProviderTotalsPhaseError); ok {
		return err
	}
	// The DB owner can join a statement error with its context-stop cause.
	// Preserve both causes and the one precise phase without discarding that
	// outer wrapper. Conflicting or incomplete observations keep this boundary.
	inspection := server.InspectErrorCauses(err)
	var observed legacyProviderTotalsPhase
	found, conflict := false, false
	for _, node := range inspection.Nodes {
		if prior, ok := node.Err.(*legacyProviderTotalsPhaseError); ok && prior != nil {
			if found && observed != prior.phase {
				conflict = true
			}
			observed, found = prior.phase, true
		}
	}
	if inspection.Complete && found && !conflict {
		phase = observed
	}
	return &legacyProviderTotalsPhaseError{phase: phase, cause: err}
}

func runLegacyProviderTotalsTx(ctx context.Context, apply func(server.PgTx) error) error {
	return runLegacyProviderTotalsTxWithOwner(ctx, apply, server.Tx)
}

// The ordinary transaction owner and its no-retry policy remain unchanged.
// An explicit owner seam lets a test commit for real, then lose only its reply.
func runLegacyProviderTotalsTxWithOwner(ctx context.Context, apply func(server.PgTx) error,
	owner func(context.Context, func(server.PgTx), ...any)) (returnErr error) {
	phase := legacyProviderTotalsTransactionStart
	server.HandleError(func() {
		// Tag before HandleError logs the recovery. A commit failure stays an
		// error even when the server may already have committed the marker.
		defer func() {
			if recovered := recover(); recovered != nil {
				cause, ok := recovered.(error)
				if !ok {
					cause = errors.New("non-error provider totals panic")
				}
				panic(withLegacyProviderTotalsPhase(phase, cause))
			}
		}()
		owner(ctx, func(tx server.PgTx) {
			phase = legacyProviderTotalsConfigure
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='250ms'`))
			phase = legacyProviderTotalsBody
			server.Raise(apply(tx))
			phase = legacyProviderTotalsCommit
		}, server.TxReadCommitted, server.OptNoRetry())
	}, func(err error) { returnErr = err })
	return
}
