// Redis v2 reservation markers survive a failed caller. Recovery checks exact
// SQL custody under the writer's per-request fence, never a global census.
package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

const redisReservationRecoveryBatch = 32

// One short budget bounds a request's compensation: its own pass inside the
// failed CreateContract call, then the deferred owner's retries. A release
// Redis can't take within it stays pending until a later request on the same
// grant moves it to recovery (10 min) or its lease ends. A 300 s budget, spent
// inside the call, kept about 170k failed proxy calls retrying every second on
// 2026-10-26, and their pool waits failed the next creations.
const redisReservationCleanupBudget = 5 * time.Second

// A failed compensation pass waits before the next one. The delay doubles from
// the base up to the cap, and jitter spreads it over its upper half, so
// requests that failed together retry apart and no request retries hot.
const redisReservationCleanupRetryBase = time.Second
const redisReservationCleanupRetryCap = 30 * time.Second

// A request's own compensation is one pass, bounded like any admission round
// trip. What it leaves pending passes to one process-wide deferred owner
// (redisDeferredCompensations): a few workers, beside the deadline pool's
// eight connections per node that every reservation shares, and a queue of a
// bounded number of balances.
const redisDeferredCompensationWorkers = 4
const redisDeferredCompensationBalances = 1024

// How long a test environment reset waits for the deferred owner's workers.
const redisDeferredCompensationQuiesceTimeout = 5 * time.Second

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
// Before discovery the page enrolls the calling request's own pending markers,
// ARGV[2], and up to 32 pending markers older than ARGV[1] milliseconds by
// Redis time: their requests died or are past their publication window.
const redisReservationRecoveryPageScript = `
local now=redis.call('TIME')
local milliseconds=tonumber(now[1])*1000+math.floor(tonumber(now[2])/1000)
local age=tonumber(ARGV[1])
if not age or age<0 then return redis.error_reply('invalid pending recovery age') end
local enrolled=false
local function enroll(token,score)
 redis.call('ZADD',KEYS[5],'NX',score,token); redis.call('ZREM',KEYS[6],token); enrolled=true
end
if ARGV[2]~='' then
 local score=redis.call('ZSCORE',KEYS[6],ARGV[2])
 if score then enroll(ARGV[2],score) end
end
local aged=redis.call('ZRANGEBYSCORE',KEYS[6],'-inf',milliseconds-age,'WITHSCORES','LIMIT',0,32)
for index=1,#aged,2 do enroll(aged[index],aged[index+1]) end
if enrolled then redis.call('PEXPIRE',KEYS[5],90000000) end
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

// own names the calling request, whose pending markers this page enrolls; it
// is empty for a page that enrolls only aged markers.
func redisReservationRecoveryPage(ctx context.Context, balanceId server.Id, own string) ([]redisReservationRecoveryCandidate, error) {
	bounded, cancel := context.WithTimeout(ctx, redisContractAdmissionTimeout)
	defer cancel()
	var result []redisReservationRecoveryCandidate
	// no preflight PING, as for redisContractReservation, and outside a transaction
	err := server.RedisWithDeadline(bounded, func(client server.RedisClient) error {
		values, err := client.Eval(bounded, redisReservationRecoveryPageScript, redisContractReservationKeys(balanceId),
			redisPendingRecoveryAge.Milliseconds(), own).StringSlice()
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
	}, server.OptRedisNoPreflight(), server.OptRedisKeepLateReplies())
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
	return recoverRedisReservationWithFenceInTx(ctx, tx, balanceId, candidate)
}

// Caller already holds this contract's v2 advisory fence in the supplied
// transaction, and retains it through Redis release or marker acknowledgement.
func recoverRedisReservationWithFenceInTx(ctx context.Context, tx server.PgTx, balanceId server.Id, candidate redisReservationRecoveryCandidate) (bool, error) {
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

// A test seam may replace a request's compensation clock, transport and
// deferred owner; production uses the default cleanup.
type redisReservationCleanupContextKey struct{}

// One public operation joins every foreground transaction before cleanup. The
// action returns a successful escrow only after its commit is acknowledged.
// Only a known insufficient-balance refusal can retry after actual recovery;
// success, ambiguous SQL, and a canceled caller are never replayed. The caller
// waits for one bounded compensation pass, never for the cleanup budget.
func runRedisContractAdmission(ctx context.Context, action func(context.Context) (*TransferEscrow, error)) (escrow *TransferEscrow, returnErr error) {
	ctx = withRedisContractAdmission(ctx)
	admission := redisAdmissionFromContext(ctx)
	recoveryAttempted := false
	compensatedCurrent := false
	cleanup := admission.reservationCleanup()
	if observe, ok := ctx.Value(redisReservationCleanupContextKey{}).(func(redisReservationCleanup) redisReservationCleanup); ok {
		cleanup = observe(cleanup)
	}
	// Every compensation of the request shares one budget.
	var cleanupEnd time.Time
	compensationPending := false
	compensate := func() (bool, error) {
		ids := admission.compensationIds()
		if len(ids) == 0 {
			return true, nil
		}
		if cleanupEnd.IsZero() {
			cleanupEnd = cleanup.now().Add(redisReservationCleanupBudget)
		}
		pending, err := cleanup.compensation(admission.contractId, ids, cleanupEnd).attemptOrDefer(ctx)
		compensationPending = compensationPending || pending
		return !pending && err == nil, err
	}
	defer func() {
		if !compensatedCurrent {
			if _, err := compensate(); err != nil {
				returnErr = errors.Join(returnErr, err)
			}
		}
		// Tokens a compensation left pending belong to the deferred owner, or
		// to retained recovery once dropped. The page would move them to the
		// recovery marker and release them in a transaction, or wait on the
		// pending lock behind the owner, so it does not run for this request.
		if !recoveryAttempted && !compensationPending {
			var publication *TransferEscrow
			if returnErr == nil && escrow != nil && escrow.ContractId == admission.contractId {
				publication = escrow
			}
			recoveryCtx := ctx
			if publication == nil && ctx.Err() != nil {
				// The foreground owner has joined. Cancellation cannot skip the
				// bounded SQL custody check for its possibly rolled-back token.
				recoveryCtx = context.WithoutCancel(ctx)
			}
			admission.recoverRetainedWithPublication(recoveryCtx, publication)
		}
	}()
	escrow, returnErr = action(ctx)
	if errors.Is(returnErr, errRedisReservationInsufficient) && escrow == nil && ctx.Err() == nil {
		compensatedCurrent = true
		// The rerun needs this request's own tokens released first. A pass
		// that leaves any of them returns the refusal without a rerun; the
		// deferred owner retries what is still pending.
		if complete, err := compensate(); !complete {
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
	return self.recoverRetainedWithPublication(ctx, nil)
}

// A matching returned escrow carries acknowledged commit authority only from
// the joined public operation above. All other candidates retain SQL proof.
func (self *redisContractAdmission) recoverRetainedWithPublication(ctx context.Context, publication *TransferEscrow) bool {
	page, cancel := context.WithTimeout(ctx, redisContractAdmissionTimeout)
	defer cancel()
	return self.recoverRetainedPageWithPublication(page, publication)
}

// The page owner supplies the one finite deadline. Separating its construction
// lets deterministic SQL-lock tests cancel the same actual page after a barrier.
func (self *redisContractAdmission) recoverRetainedPage(page context.Context) bool {
	return self.recoverRetainedPageWithPublication(page, nil)
}

// Discovery retains its original 32-candidate cap before any acknowledged own
// token is removed from the SQL work list. No caller holds PG during this page.
func (self *redisContractAdmission) recoverRetainedPageWithPublication(page context.Context, publication *TransferEscrow) (progress bool) {
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
	// Discovery changes no token amount. Remaining candidates keep their SQL
	// fence around every custody check and exact release/publication below.
	type retainedCandidate struct {
		balanceId server.Id
		candidate redisReservationRecoveryCandidate
	}
	retained := make([]retainedCandidate, 0, redisReservationRecoveryBatch)
	// This request's attempts have joined their transactions, so its own
	// pending tokens pass to recovery with this page, as tokens reserved
	// inside the transaction always did. An attempt of the same request still
	// in flight keeps them pending.
	self.retirePending(func(inFlight bool) error {
		own := self.contractId.String()
		if inFlight {
			own = ""
		}
		for _, balanceId := range ids {
			if len(retained) == redisReservationRecoveryBatch || page.Err() != nil {
				break
			}
			candidates, err := redisReservationRecoveryPage(page, balanceId, own)
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
		return nil
	})
	if page.Err() != nil {
		redisContractReservationResults.WithLabelValues("recovery", "pending").Inc()
		return false
	}
	// The successful transaction already proved only its exact selected tokens.
	// Keep failed acknowledgements, changed amounts and every peer on the old
	// custody path. Filtering must not refill or reorder the collected page.
	pending := retained[:0]
	for index, item := range retained {
		if page.Err() != nil {
			pending = append(pending, retained[index:]...)
			break
		}
		acknowledged := false
		if publication != nil && publication.ContractId == self.contractId && item.candidate.contractId == self.contractId {
			for _, balance := range publication.Balances {
				if balance != nil && balance.BalanceId == item.balanceId && balance.BalanceByteCount == item.candidate.amount {
					_, err := redisContractReservation(page, "published", item.balanceId, item.candidate.contractId, 0, item.candidate.amount, redisContractReservationLease,
						server.OptRedisKeepLateReplies())
					acknowledged = err == nil
					break
				}
			}
		}
		if acknowledged {
			redisContractReservationResults.WithLabelValues("recovery", "completed").Inc()
		} else {
			pending = append(pending, item)
		}
	}
	retained = pending
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
		// no preflight PING, as for redisContractReservation, inside the transaction
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
		}, server.OptRedisNoPreflight())
		if returnErr == nil && amount != 0 {
			_, returnErr = recoverRedisReservationWithFenceInTx(attempt, tx, balanceId, redisReservationRecoveryCandidate{contractId: contractId, amount: amount})
		}
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}

// The caller's error/timeout is not cleanup's lifetime. One finite budget owns
// every balance and every retry after PG has joined; tests supply an owned
// clock/transport to prove long transient intervals without wall-clock sleeps,
// and an owned deferred owner.
type redisReservationCleanup struct {
	now      func() time.Time
	wait     func(context.Context, time.Duration) error
	recover  func(context.Context, server.Id, server.Id) error
	deferred *redisDeferredCompensations
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
	return self.reservationCleanup().run(ctx, self.contractId, ids)
}

// Compensation releases this request's own tokens, pending ones in Redis alone.
func (self *redisContractAdmission) reservationCleanup() redisReservationCleanup {
	cleanup := defaultRedisReservationCleanup()
	cleanup.recover = func(ctx context.Context, balanceId, _ server.Id) error {
		return self.releaseReservation(ctx, balanceId)
	}
	return cleanup
}

// Releases this request's possibly reserved token on one balance once its
// attempts' transactions have joined. No transaction can publish a pending
// token that no attempt in flight holds, so it is released in Redis alone,
// with no SQL custody check or transaction. A token that is no longer
// pending, one an attempt in flight may hold, and one an attempt may have
// published since compensation began keep the fenced SQL custody check that
// recovery applies to any request. A published token stays pending until its
// attempt's own page acknowledges it.
func (self *redisContractAdmission) releaseReservation(ctx context.Context, balanceId server.Id) error {
	return self.retirePending(func(inFlight bool) error {
		if !inFlight && !self.publicationHasStarted() {
			_, err := redisContractReservation(ctx, "release-pending", balanceId, self.contractId, 0, math.MaxInt64, redisContractReservationLease,
				server.OptRedisKeepLateReplies())
			if !errors.Is(err, errRedisReservationNotPending) {
				return err
			}
		}
		return recoverRedisReservationRequest(ctx, balanceId, self.contractId)
	})
}

// Read again on each compensation attempt: compensationIds checks it only once.
func (self *redisContractAdmission) publicationHasStarted() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.publicationStarted
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
		}, deferred: deferredRedisCompensations}
}

func (self redisReservationCleanup) run(ctx context.Context, contractId server.Id, ids []server.Id) error {
	owner, cancel := context.WithTimeout(context.WithoutCancel(ctx), redisReservationCleanupBudget)
	defer cancel()
	end := self.now().Add(redisReservationCleanupBudget)
	return self.runOwned(owner, end, contractId, ids)
}

func (self redisReservationCleanup) runOwned(owner context.Context, end time.Time, contractId server.Id, ids []server.Id) error {
	return self.compensation(contractId, ids, end).run(owner)
}

// One compensation of a request's own tokens: the balances still pending and
// every refusal, across its passes within the request's one budget.
type redisReservationCompensation struct {
	cleanup        redisReservationCleanup
	contractId     server.Id
	attempted      int
	end            time.Time
	pending        []server.Id
	failures       []error
	lastAttemptErr error
	retries        int
}

func (self redisReservationCleanup) compensation(contractId server.Id, ids []server.Id, end time.Time) *redisReservationCompensation {
	return &redisReservationCompensation{cleanup: self, contractId: contractId, attempted: len(ids), end: end,
		pending: append([]server.Id(nil), ids...)}
}

// Reports whether the owner or the budget has ended.
func (self *redisReservationCompensation) ended(owner context.Context) bool {
	select {
	case <-owner.Done():
		return true
	default:
		return !self.cleanup.now().Before(self.end)
	}
}

// Retries the pending balances, one pass after each backoff, until none
// remains or the budget ends.
func (self *redisReservationCompensation) run(owner context.Context) error {
	for len(self.pending) != 0 {
		if self.ended(owner) {
			self.failures = append(self.failures, context.DeadlineExceeded)
			break
		}
		self.attempt(owner)
		if len(self.pending) != 0 && !self.backOff(owner) {
			break
		}
	}
	return self.finish()
}

// Continues after the request's own pass: every later pass follows a backoff.
// A compensation whose budget ended while it was queued makes no pass.
func (self *redisReservationCompensation) resume(owner context.Context) error {
	if !self.cleanup.now().Before(self.end) {
		redisContractReservationResults.WithLabelValues("compensation", "expired").Inc()
		return self.err()
	}
	if !self.backOff(owner) {
		return self.finish()
	}
	return self.run(owner)
}

// One pass tries each pending balance once. A transient failure stays pending,
// as does each balance the pass did not reach before its owner or the budget
// ended. Any other failure leaves its balance to retained recovery.
func (self *redisReservationCompensation) attempt(owner context.Context) {
	next := make([]server.Id, 0, len(self.pending))
	for index, id := range self.pending {
		if self.ended(owner) {
			next = append(next, self.pending[index:]...)
			break
		}
		err := self.cleanup.recover(owner, id, self.contractId)
		if err == nil {
			continue
		}
		if redisReservationRecoveryRetryable(err) {
			self.lastAttemptErr = err
			next = append(next, id)
		} else {
			self.failures = append(self.failures, err)
		}
	}
	self.pending = next
}

// Waits out the next retry's backoff. Reports whether another pass may follow.
func (self *redisReservationCompensation) backOff(owner context.Context) bool {
	if err := self.cleanup.wait(owner, self.NextRetryTimeout()); err != nil {
		self.failures = append(self.failures, err)
		return false
	}
	return true
}

// Each call advances the retry: its backoff, within the remaining budget.
func (self *redisReservationCompensation) NextRetryTimeout() time.Duration {
	delay := redisReservationCleanupRetryDelay(self.retries, mathrand.Float64())
	self.retries++
	return min(delay, self.end.Sub(self.cleanup.now()))
}

// The delay before retry number retry, from zero. The nominal delay doubles
// from the base up to the cap, and the delay falls in its upper half, so the
// budget bounds the number of passes. randomUnit is in [0,1).
func redisReservationCleanupRetryDelay(retry int, randomUnit float64) time.Duration {
	randomUnit = max(0, min(randomUnit, math.Nextafter(1, 0)))
	nominal := redisReservationCleanupRetryCap
	if retry < 16 {
		nominal = min(nominal, redisReservationCleanupRetryBase<<max(0, retry))
	}
	return nominal/2 + time.Duration(randomUnit*float64(nominal/2))
}

// Names the balances left to retained recovery; nil when none is.
func (self *redisReservationCompensation) err() error {
	if len(self.failures) == 0 && len(self.pending) == 0 {
		return nil
	}
	causes := append([]error{fmt.Errorf("%w: request=%s attempted_balances=%d", errRedisReservationCleanupPending, self.contractId, self.attempted)}, self.failures...)
	if self.lastAttemptErr != nil {
		causes = append(causes, self.lastAttemptErr)
	}
	return errors.Join(causes...)
}

// Counts the compensation's one final outcome.
func (self *redisReservationCompensation) finish() error {
	err := self.err()
	result := "completed"
	if err != nil {
		result = "pending"
	}
	redisContractReservationResults.WithLabelValues("compensation", result).Inc()
	return err
}

// The request's own pass, bounded like one admission round trip, after which
// its caller returns. Balances still pending pass to the deferred owner, with
// the rest of the budget, or are dropped to retained recovery when its queue is
// full. Reports whether balances were left pending; the error names balances
// left to recovery.
func (self *redisReservationCompensation) attemptOrDefer(ctx context.Context) (bool, error) {
	pass, cancel := context.WithTimeout(context.WithoutCancel(ctx), redisContractAdmissionTimeout)
	self.attempt(pass)
	cancel()
	if len(self.pending) == 0 {
		return false, self.finish()
	}
	// Nothing of the compensation is read once the deferred owner has it.
	var refused error
	if len(self.failures) != 0 {
		refused = self.err()
	}
	if self.cleanup.deferred != nil && self.cleanup.deferred.offer(ctx, self) {
		redisContractReservationResults.WithLabelValues("compensation", "deferred").Inc()
		return true, refused
	}
	redisContractReservationResults.WithLabelValues("compensation", "dropped").Inc()
	return true, self.err()
}

// The process-wide owner of the compensations that their requests' own passes
// left pending. Each worker retries one compensation at a time, with its
// backoff and within its request's budget, so however many requests fail, a
// failing Redis sees at most one compensation call per worker beyond each
// request's own pass, on the same deadline pool. The queue holds a bounded
// number of balances, since one request can hold one per selected grant. A
// compensation the owner takes counts deferred, then completed, pending, or
// expired when its budget ended in the queue.
//
// A full queue drops a compensation, counted dropped, without blocking or
// starting a goroutine, and a quiesce ends those it holds, as process exit
// does. That only defers the release, as a compensation that ends pending
// does: each token stays reserved, which can under-admit its payer but never
// over-admit, and keeps its pending marker, the state recovery reclaims for a
// request that died. Any page on the balance takes a pending marker older than
// redisPendingRecoveryAge, and the fenced SQL custody check then releases each
// token no contract holds. A returned request starts no attempt that could
// publish the token, and its lease ends it at last.
type redisDeferredCompensations struct {
	workers  int
	capacity int
	queue    chan redisDeferredCompensation
	start    sync.Once

	stateLock sync.Mutex
	// balances offered and not yet ended
	held int
	// closed while held is zero
	idle chan struct{}
	// canceled by a quiesce, which ends every compensation offered before it
	generation context.Context
	cancel     context.CancelFunc
}

type redisDeferredCompensation struct {
	owner        context.Context
	stop         func()
	balances     int
	compensation *redisReservationCompensation
}

var deferredRedisCompensations = newRedisDeferredCompensations(redisDeferredCompensationWorkers, redisDeferredCompensationBalances)

// A test environment reset ends the deferred compensations before its Redis and
// PostgreSQL go away, as process exit would, so none runs in the next one. A
// worker still blocked past the bound is abandoned, as at exit.
func init() {
	server.OnReset(func() {
		ctx, cancel := context.WithTimeout(context.Background(), redisDeferredCompensationQuiesceTimeout)
		defer cancel()
		_ = deferredRedisCompensations.quiesce(ctx)
	})
}

// Ends the deferred compensations before process exit, leaving their tokens to
// retained recovery, and waits within ctx for the workers to let them go.
func QuiesceRedisContractCompensations(ctx context.Context) {
	_ = deferredRedisCompensations.quiesce(ctx)
}

func newRedisDeferredCompensations(workers, capacity int) *redisDeferredCompensations {
	idle := make(chan struct{})
	close(idle)
	generation, cancel := context.WithCancel(context.Background())
	return &redisDeferredCompensations{
		workers:    workers,
		capacity:   capacity,
		queue:      make(chan redisDeferredCompensation, capacity),
		idle:       idle,
		generation: generation,
		cancel:     cancel,
	}
}

// Reports whether a worker will run the compensation, within its budget. The
// first offer starts the workers, which run for the life of the process.
func (self *redisDeferredCompensations) offer(ctx context.Context, compensation *redisReservationCompensation) bool {
	self.start.Do(func() {
		for range self.workers {
			go self.work()
		}
	})
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	balances := len(compensation.pending)
	if self.capacity < self.held+balances {
		return false
	}
	// The request's values stay; its cancellation and synchronous stage
	// timing end when its caller returns. The budget ends its passes and
	// waits, and a quiesce ends it early.
	owner, cancel := context.WithCancel(server.WithoutContractCreationTiming(context.WithoutCancel(ctx)))
	unlink := context.AfterFunc(self.generation, cancel)
	deferred := redisDeferredCompensation{
		owner:        owner,
		stop:         func() { unlink(); cancel() },
		balances:     balances,
		compensation: compensation,
	}
	// Every queued compensation holds a balance, so the queue has room.
	select {
	case self.queue <- deferred:
	default:
		deferred.stop()
		return false
	}
	if self.held == 0 {
		self.idle = make(chan struct{})
	}
	self.held += balances
	return true
}

func (self *redisDeferredCompensations) work() {
	for deferred := range self.queue {
		self.run(deferred)
	}
}

// A raise ends the compensation pending, and the worker goes on.
func (self *redisDeferredCompensations) run(deferred redisDeferredCompensation) {
	defer self.end(deferred.balances)
	defer deferred.stop()
	server.HandleError(func() {
		deferred.compensation.resume(deferred.owner)
	}, func() {
		redisContractReservationResults.WithLabelValues("compensation", "pending").Inc()
	})
}

func (self *redisDeferredCompensations) end(balances int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.held -= balances
	if self.held == 0 {
		close(self.idle)
	}
}

// Waits within ctx until no compensation is held.
func (self *redisDeferredCompensations) drain(ctx context.Context) error {
	idle := func() chan struct{} {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.idle
	}()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Ends every compensation held now, leaving its tokens to retained recovery,
// then drains within ctx. Compensations offered later run as before.
func (self *redisDeferredCompensations) quiesce(ctx context.Context) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.cancel()
		self.generation, self.cancel = context.WithCancel(context.Background())
	}()
	return self.drain(ctx)
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
