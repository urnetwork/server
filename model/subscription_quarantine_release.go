// A quarantine settles a malformed contract with no payout and leaves its
// escrow unsettled: it has no settlement authority, so no debit journal row may
// own its Redis release. Its commit instead queues one durable task per
// contract that releases the reservation, retrying with backoff until Redis
// acknowledges it. The release after the commit stays the fast path; both are
// idempotent. Quarantines are rare, so this adds at most one pending row each.
package model

import (
	"context"
	"errors"
	"math"
	mathrand "math/rand"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The first run waits out the fast path, which usually leaves it nothing to do.
const quarantineReservationReleaseDelay = 30 * time.Second

// A failed release doubles its delay from the base up to the cap, so a
// degraded Redis is never retried hot.
const quarantineReservationReleaseRetryBase = 2 * time.Second
const quarantineReservationReleaseRetryCap = 10 * time.Minute

const quarantineReservationReleaseTimeout = 10 * time.Second

// Redis drops a token when its lease ends. The lease starts at reservation,
// before the contract row is written; the margin covers clock skew between the
// two servers. Past this age there is nothing left to release, so retries end.
const quarantineReservationReleaseHorizon = redisContractReservationLease + 10*time.Minute

type QuarantineReservationReleaseArgs struct {
	ContractId server.Id `json:"contract_id"`
}

type QuarantineReservationReleaseResult struct {
	// The lease had ended, so Redis drops the token without this task.
	LeaseEnded bool `json:"lease_ended,omitempty"`
}

func quarantineReservationReleaseRunOnce(contractId server.Id) *task.RunOnceOption {
	return task.RunOnce("quarantine_reservation_release", contractId)
}

// Both workload profiles register this target. The cap also bounds failures
// that carry no delay of their own, such as a run that exceeds its max time.
func NewQuarantineReservationReleaseTaskTarget() task.Target {
	return task.WithErrorRetryCap(
		task.NewTaskTarget(ReleaseQuarantinedContractReservation),
		quarantineReservationReleaseRetryCap+quarantineReservationReleaseRetryCap/2,
	)
}

// Runs in the quarantine's commit, after its claim. Only a Redis-held
// reservation has a token to release. The contract keys the task, so a repeat
// or a retry never adds a second row.
func queueQuarantineReservationReleaseInTx(ctx context.Context, tx server.PgTx, contractId server.Id) {
	var reserved bool
	server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_escrow
		WHERE contract_id=$1 AND redis_reserved AND balance_byte_count>0)`, contractId).Scan(&reserved))
	if !reserved {
		return
	}
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	task.ScheduleTaskInTxIfAbsent(
		tx,
		ReleaseQuarantinedContractReservation,
		&QuarantineReservationReleaseArgs{ContractId: contractId},
		owner,
		quarantineReservationReleaseRunOnce(contractId),
		task.RunAt(server.NowUtc().Add(quarantineReservationReleaseDelay)),
		task.MaxTime(time.Minute),
	)
}

// Releases the contract's Redis reservation through the same per-contract
// reconciliation as the fast path. It completes, which deletes its pending
// row, once Redis acknowledges every release, or once the lease has ended or
// the contract was reaped. A failed release retries with jittered backoff.
func ReleaseQuarantinedContractReservation(
	args *QuarantineReservationReleaseArgs,
	clientSession *session.ClientSession,
) (*QuarantineReservationReleaseResult, error) {
	ctx, cancel := context.WithTimeout(clientSession.Ctx, quarantineReservationReleaseTimeout)
	defer cancel()
	found := false
	terminal := false
	var createTime time.Time
	server.Db(ctx, func(conn server.PgConn) {
		err := conn.QueryRow(ctx, `SELECT create_time, outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1`,
			args.ContractId).Scan(&createTime, &terminal)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
		found = true
	})
	if !found {
		return &QuarantineReservationReleaseResult{}, nil
	}
	if !server.NowUtc().Before(createTime.Add(quarantineReservationReleaseHorizon)) {
		return &QuarantineReservationReleaseResult{LeaseEnded: true}, nil
	}
	if !terminal {
		// Only the quarantine's commit queues this task, with the outcome.
		// Reconciliation would restore an open contract's token instead.
		return nil, errors.New("quarantined contract has no outcome")
	}
	var releaseErr error
	server.HandleError(func() {
		ReconcileRedisContractReservation(ctx, args.ContractId)
	}, func(err error) {
		releaseErr = err
	})
	if releaseErr == nil {
		return &QuarantineReservationReleaseResult{}, nil
	}
	// The pending row counts this task's earlier failures. If it cannot be
	// read, the target's own capped backoff applies instead.
	var errorCount int
	countErr := server.HandleError(func() {
		if identity, ok := task.ExecutionIdentityFromContext(clientSession.Ctx); ok {
			if claimed := task.GetTasks(clientSession.Ctx, identity.TaskId)[identity.TaskId]; claimed != nil {
				errorCount = claimed.RescheduleErrorCount
			}
		}
	})
	if countErr != nil {
		return nil, releaseErr
	}
	return nil, task.WithRetryDelay(releaseErr, quarantineReservationReleaseRetryDelay(errorCount, mathrand.Float64()))
}

// Below the cap, the delay doubles from the base per earlier failure and adds
// up to one base of jitter, as ordinary task backoff does. At the cap, retries
// spread over half to one and a half times the cap, so releases left by one
// outage do not retry Redis together. randomUnit is in [0,1).
func quarantineReservationReleaseRetryDelay(errorCount int, randomUnit float64) time.Duration {
	randomUnit = max(0, min(randomUnit, math.Nextafter(1, 0)))
	nominal := quarantineReservationReleaseRetryCap
	if errorCount < 16 {
		nominal = min(nominal, quarantineReservationReleaseRetryBase<<max(0, errorCount))
	}
	if nominal < quarantineReservationReleaseRetryCap {
		return nominal + time.Duration(randomUnit*float64(quarantineReservationReleaseRetryBase))
	}
	return nominal/2 + time.Duration(randomUnit*float64(nominal))
}
