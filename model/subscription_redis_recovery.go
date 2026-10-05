// Redis v2 reservation markers survive a failed caller. Recovery checks exact
// SQL custody under the writer's per-request fence, never a global census.
package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

const redisReservationRecoveryBatch = 32
const redisReservationCleanupBudget = 300 * time.Second

var errRedisReservationCleanupPending = errors.New("Redis reservation compensation remains pending in retained v2 markers")
var errRedisReservationRecoveryIdentity = errors.New("Redis reservation marker or SQL custody does not match")
var errRedisReservationRequestActive = errors.New("Redis reservation request still owns its SQL publication fence")
var errRedisReservationDebitPending = errors.New("Redis reservation retains unapplied asynchronous consumption")

func redisContractAdmissionLock(contractId server.Id) string {
	return "redis-contract-v2/" + contractId.String()
}

// A bounded page rotates after observation, so busy or unknown entries cannot
// monopolize the next page. Rotation changes no token amount or lease. A marker
// with no token is stale and removable; an unmarked token is never enrolled.
const redisReservationRecoveryPageScript = `
local candidates=redis.call('ZRANGE',KEYS[5],0,31)
local tail=redis.call('ZRANGE',KEYS[5],-1,-1,'WITHSCORES')
local score=0; if #tail==2 then score=tonumber(tail[2]) end
local result={}
for _,token in ipairs(candidates) do
 local amount=redis.call('HGET',KEYS[2],token)
 if amount then
  -- Bound the reply even if an unknown producer corrupted a member. Empty
  -- fields report that refusal while its original marker remains retained.
  if #token<=36 and #amount<=19 then
   table.insert(result,token); table.insert(result,amount)
  else table.insert(result,''); table.insert(result,'') end
  score=score+1; redis.call('ZADD',KEYS[5],score,token)
 else redis.call('ZREM',KEYS[5],token) end
end
return result
`

type redisReservationRecoveryCandidate struct {
	contractId server.Id
	amount     ByteCount
}

func redisReservationRecoveryPage(ctx context.Context, balanceId server.Id) ([]redisReservationRecoveryCandidate, error) {
	bounded, cancel := context.WithTimeout(ctx, redisContractAdmissionTimeout)
	defer cancel()
	var result []redisReservationRecoveryCandidate
	err := server.RedisWithDeadline(bounded, func(client server.RedisClient) error {
		values, err := client.Eval(bounded, redisReservationRecoveryPageScript, redisContractReservationKeys(balanceId)).StringSlice()
		if err != nil {
			return err
		}
		if len(values)%2 != 0 || len(values) > 2*redisReservationRecoveryBatch {
			return errRedisReservationRecoveryIdentity
		}
		for index := 0; index < len(values); index += 2 {
			id, err := server.ParseId(values[index])
			amount, amountErr := strconv.ParseInt(values[index+1], 10, 64)
			if err != nil || id.String() != values[index] || amountErr != nil || amount <= 0 || strconv.FormatInt(amount, 10) != values[index+1] {
				redisContractReservationResults.WithLabelValues("recovery", "invalid-marker").Inc()
				continue
			}
			result = append(result, redisReservationRecoveryCandidate{contractId: id, amount: amount})
		}
		return nil
	})
	return result, err
}

// Caller owns an active SQL transaction. try-lock never waits for a live
// writer. Absence is safe only under that same v2 transaction fence.
func recoverRedisReservationInTx(ctx context.Context, tx server.PgTx, balanceId server.Id, candidate redisReservationRecoveryCandidate) (bool, error) {
	var locked bool
	server.Raise(tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`, redisContractAdmissionLock(candidate.contractId)).Scan(&locked))
	if !locked {
		return false, errRedisReservationRequestActive
	}
	var contract, escrow, matching, terminal, debitPending bool
	server.Raise(tx.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1),
		EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1),
		EXISTS(SELECT 1 FROM transfer_contract c JOIN transfer_escrow e USING(contract_id)
			JOIN transfer_balance b USING(balance_id) WHERE c.contract_id=$1 AND e.balance_id=$2
			AND e.redis_reserved AND c.payer_network_id=b.network_id
			AND (e.balance_byte_count=$3 OR (c.outcome IS NOT NULL AND EXISTS(
				SELECT 1 FROM transfer_debit_journal j WHERE j.contract_id=$1 AND j.balance_id=$2
				AND j.applied AND j.debit_byte_count=$3 AND j.debit_byte_count<=e.balance_byte_count)))),
		EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1 AND outcome IS NOT NULL),
		EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1 AND balance_id=$2 AND NOT applied)`,
		candidate.contractId, balanceId, candidate.amount).Scan(&contract, &escrow, &matching, &terminal, &debitPending))
	if debitPending {
		// The same SQL snapshot must cover outcome and consumption. Terminal
		// history may be reaped while its durable debit still awaits writeback.
		// Keep both original and already-settled tokens; the bounded debit owner
		// releases them after commit. Other recovery candidates can advance.
		return false, errRedisReservationDebitPending
	}
	operation := "release-owned"
	if contract || escrow {
		if !contract || !matching {
			return false, errRedisReservationRecoveryIdentity
		}
		if !terminal {
			// A lost commit acknowledgement is an existing live reservation,
			// not abandoned debt. Forget only its recovery marker.
			operation = "published"
		}
	}
	released, err := redisContractReservation(ctx, operation, balanceId, candidate.contractId, 0, candidate.amount, redisContractReservationLease)
	return operation == "release-owned" && released > 0 && err == nil, err
}

// One public operation joins every foreground transaction before cleanup.
// Only a known insufficient-balance refusal can retry after actual recovery;
// success, ambiguous SQL, and a canceled caller are never replayed.
func runRedisContractAdmission(ctx context.Context, action func(context.Context) (*TransferEscrow, error)) (escrow *TransferEscrow, returnErr error) {
	ctx = withRedisContractAdmission(ctx)
	admission := redisAdmissionFromContext(ctx)
	recoveryAttempted := false
	compensatedCurrent := false
	cleanup := defaultRedisReservationCleanup()
	var cleanupCtx context.Context
	var cleanupCancel context.CancelFunc
	var cleanupEnd time.Time
	compensate := func() error {
		ids := admission.compensationIds()
		if len(ids) == 0 {
			return nil
		}
		if cleanupCtx == nil {
			cleanupCtx, cleanupCancel = context.WithTimeout(context.WithoutCancel(ctx), redisReservationCleanupBudget)
			cleanupEnd = cleanup.now().Add(redisReservationCleanupBudget)
		}
		return cleanup.runOwned(cleanupCtx, cleanupEnd, admission.contractId, ids)
	}
	defer func() {
		defer func() {
			if cleanupCancel != nil {
				cleanupCancel()
			}
		}()
		if !compensatedCurrent {
			if err := compensate(); err != nil {
				returnErr = errors.Join(returnErr, err)
			}
		}
		if !recoveryAttempted {
			admission.recoverRetained(ctx)
		}
	}()
	escrow, returnErr = action(ctx)
	if errors.Is(returnErr, errRedisReservationInsufficient) && escrow == nil && ctx.Err() == nil {
		compensatedCurrent = true
		if err := compensate(); err != nil {
			return nil, errors.Join(returnErr, err)
		}
		recoveryAttempted = true
		if admission.recoverRetained(ctx) && ctx.Err() == nil {
			compensatedCurrent = false
			return action(ctx)
		}
	}
	return
}

// This optional page has its own transaction and one actual deadline across
// Redis and PG. A timed-out query aborts only this transaction, never the
// already-joined foreground owner. Markers rotate and retain unknown outcomes.
func (self *redisContractAdmission) recoverRetained(ctx context.Context) bool {
	page, cancel := context.WithTimeout(ctx, redisContractAdmissionTimeout)
	defer cancel()
	return self.recoverRetainedPage(page)
}

// The page owner supplies the one finite deadline. Separating its construction
// lets deterministic SQL-lock tests cancel the same actual page after a barrier.
func (self *redisContractAdmission) recoverRetainedPage(page context.Context) (progress bool) {
	if _, bounded := page.Deadline(); !bounded || page.Err() != nil {
		redisContractReservationResults.WithLabelValues("recovery", "pending").Inc()
		return false
	}
	var ids []server.Id
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		ids = append(ids, self.attemptedBalanceIds...)
	}()
	if len(ids) == 0 {
		return false
	}
	defer func() {
		if value := recover(); value != nil {
			if _, programming := value.(runtime.Error); programming {
				panic(value)
			}
			if _, observation := value.(error); !observation {
				panic(value)
			}
			redisContractReservationResults.WithLabelValues("recovery", "pending").Inc()
		}
	}()
	// Discover bounded candidates before acquiring PG. An empty page is normal
	// after successful publication and needs no SQL transaction; cold Redis
	// routing and candidate reads must not hold a PostgreSQL snapshot or slot.
	// Discovery changes no token amount. The SQL fence still covers every
	// custody check and exact Redis release/publication below.
	type retainedCandidate struct {
		balanceId server.Id
		candidate redisReservationRecoveryCandidate
	}
	retained := make([]retainedCandidate, 0, redisReservationRecoveryBatch)
	for _, balanceId := range ids {
		if len(retained) == redisReservationRecoveryBatch || page.Err() != nil {
			break
		}
		candidates, err := redisReservationRecoveryPage(page, balanceId)
		if err != nil {
			redisContractReservationResults.WithLabelValues("recovery", "pending").Inc()
			continue
		}
		for _, candidate := range candidates {
			if len(retained) == redisReservationRecoveryBatch || page.Err() != nil {
				break
			}
			retained = append(retained, retainedCandidate{balanceId: balanceId, candidate: candidate})
		}
	}
	if page.Err() != nil {
		redisContractReservationResults.WithLabelValues("recovery", "pending").Inc()
		return false
	}
	if len(retained) == 0 {
		return false
	}
	server.Tx(page, func(tx server.PgTx) {
		for _, item := range retained {
			if page.Err() != nil {
				break
			}
			released, err := recoverRedisReservationInTx(page, tx, item.balanceId, item.candidate)
			progress = progress || released
			result := "completed"
			if err != nil {
				result = "pending"
			}
			redisContractReservationResults.WithLabelValues("recovery", result).Inc()
		}
		server.Raise(page.Err())
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}

// One exact failed request can recover after its original transaction has
// joined. No new SQL row is inserted and no missing marker is reconstructed.
func recoverRedisReservationRequest(ctx context.Context, balanceId, contractId server.Id) (returnErr error) {
	attempt, cancel := context.WithTimeout(ctx, redisContractAdmissionTimeout)
	defer cancel()
	defer func() {
		if value := recover(); value != nil {
			if _, programming := value.(runtime.Error); programming {
				panic(value)
			}
			cause, ok := value.(error)
			if !ok {
				panic(value)
			}
			returnErr = cause
		}
		if attempt.Err() != nil {
			returnErr = errors.Join(returnErr, attempt.Err())
		}
	}()
	server.Tx(attempt, func(tx server.PgTx) {
		// Read the token only while its original writer is joined. A retry of
		// the same stable request cannot publish between this read and release.
		var locked bool
		server.Raise(tx.QueryRow(attempt, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`, redisContractAdmissionLock(contractId)).Scan(&locked))
		if !locked {
			returnErr = errRedisReservationRequestActive
			return
		}
		var amount ByteCount
		returnErr = server.RedisWithDeadline(attempt, func(client server.RedisClient) error {
			value, err := client.Eval(attempt, `return redis.call('HGET',KEYS[2],ARGV[1])`, redisContractReservationKeys(balanceId), contractId.String()).Text()
			if errors.Is(err, redis.Nil) {
				return nil
			}
			if err != nil {
				return err
			}
			amount, err = strconv.ParseInt(value, 10, 64)
			if err != nil || amount <= 0 || strconv.FormatInt(amount, 10) != value {
				return errRedisReservationRecoveryIdentity
			}
			return nil
		})
		if returnErr == nil && amount != 0 {
			_, returnErr = recoverRedisReservationInTx(attempt, tx, balanceId, redisReservationRecoveryCandidate{contractId: contractId, amount: amount})
		}
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}

// The caller's error/timeout is not cleanup's lifetime. One finite budget owns
// every balance and every retry after PG has joined; tests supply an owned
// clock/transport to prove long transient intervals without wall-clock sleeps.
type redisReservationCleanup struct {
	now     func() time.Time
	wait    func(context.Context, time.Duration) error
	recover func(context.Context, server.Id, server.Id) error
}

func (self *redisContractAdmission) compensationIds() []server.Id {
	var ids []server.Id
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.publicationStarted {
			for _, id := range self.attemptedBalanceIds {
				if self.compensationAttempts == nil || self.compensationAttempts[id] > 0 {
					ids = append(ids, id)
				}
			}
		}
	}()
	return ids
}

func (self *redisContractAdmission) compensate(ctx context.Context) error {
	ids := self.compensationIds()
	if len(ids) == 0 {
		return nil
	}
	return defaultRedisReservationCleanup().run(ctx, self.contractId, ids)
}

func defaultRedisReservationCleanup() redisReservationCleanup {
	return redisReservationCleanup{now: time.Now, recover: recoverRedisReservationRequest,
		wait: func(ctx context.Context, delay time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				return nil
			}
		}}
}

func (self redisReservationCleanup) run(ctx context.Context, contractId server.Id, ids []server.Id) error {
	owner, cancel := context.WithTimeout(context.WithoutCancel(ctx), redisReservationCleanupBudget)
	defer cancel()
	end := self.now().Add(redisReservationCleanupBudget)
	return self.runOwned(owner, end, contractId, ids)
}

func (self redisReservationCleanup) runOwned(owner context.Context, end time.Time, contractId server.Id, ids []server.Id) error {
	pending := append([]server.Id(nil), ids...)
	var failures []error
	var lastAttemptErr error
	for len(pending) != 0 {
		if owner.Err() != nil || !self.now().Before(end) {
			failures = append(failures, context.DeadlineExceeded)
			break
		}
		next := make([]server.Id, 0, len(pending))
		for _, id := range pending {
			if owner.Err() != nil || !self.now().Before(end) {
				failures = append(failures, context.DeadlineExceeded)
				break
			}
			err := self.recover(owner, id, contractId)
			if err == nil {
				continue
			}
			if redisReservationRecoveryRetryable(err) {
				lastAttemptErr = err
				next = append(next, id)
			} else {
				failures = append(failures, err)
			}
		}
		pending = next
		if len(pending) != 0 {
			if err := self.wait(owner, min(time.Second, end.Sub(self.now()))); err != nil {
				failures = append(failures, err)
				break
			}
		}
	}
	if len(failures) != 0 || len(pending) != 0 {
		if lastAttemptErr != nil {
			failures = append(failures, lastAttemptErr)
		}
		redisContractReservationResults.WithLabelValues("compensation", "pending").Inc()
		return errors.Join(append([]error{fmt.Errorf("%w: request=%s attempted_balances=%d", errRedisReservationCleanupPending, contractId, len(ids))}, failures...)...)
	}
	redisContractReservationResults.WithLabelValues("compensation", "completed").Inc()
	return nil
}

// All joined leaf causes must be transient. A schema, ownership, corrupt-token
// or authentication refusal cannot be hidden behind a timeout sibling.
func redisReservationRecoveryRetryable(err error) bool {
	causes := server.InspectErrorCauses(err)
	if !causes.Complete {
		return false
	}
	for _, node := range causes.Nodes {
		if !node.Leaf {
			continue
		}
		cause := node.Err
		if cause == errRedisReservationRequestActive || cause == context.DeadlineExceeded || cause == context.Canceled || cause == io.EOF || cause == net.ErrClosed || cause == redis.ErrClosed || cause == syscall.ECONNRESET || cause == syscall.ECONNREFUSED || cause == syscall.ETIMEDOUT || cause == syscall.EADDRNOTAVAIL {
			continue
		}
		if database, ok := cause.(*pgconn.PgError); ok && database != nil {
			if strings.HasPrefix(database.Code, "08") || strings.HasPrefix(database.Code, "53") || database.Code == "40001" || database.Code == "40P01" || database.Code == "55P03" || database.Code == "57014" {
				continue
			}
			return false
		}
		if reply, ok := cause.(redis.Error); ok {
			kind, _, _ := strings.Cut(reply.Error(), " ")
			switch kind {
			case "LOADING", "TRYAGAIN", "CLUSTERDOWN", "READONLY", "MASTERDOWN":
				continue
			}
			return false
		}
		if timeout, ok := cause.(net.Error); ok && timeout.Timeout() {
			continue
		}
		return false
	}
	return true
}
