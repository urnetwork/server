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
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
	"gopkg.in/yaml.v3"
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
// eight connections per node that every reservation shares, and room for a
// bounded number of balances. A worker is held only while a compensation's
// release calls run; between passes the compensation waits on a timer.
const redisDeferredCompensationWorkers = 4
const redisDeferredCompensationBalances = 1024

// How long a test environment reset waits for the deferred owner's workers.
const redisDeferredCompensationQuiesceTimeout = 5 * time.Second

// After one of the deferred owner's release or handoff calls ends in a
// timeout, the owner hands no given-up token to recovery for this long. A
// timeout means a slow Redis node or PostgreSQL pool, and the page that
// releases a handed-off token holds one of the process's PostgreSQL
// connections through its Redis release: in a storm the handoff would move
// every given-up token into such a page at once.
const redisDeferredCompensationHandoffQuiet = 30 * time.Second

// The operator switch for the deferred owner's give-up handoff, read on each
// give-up, so that a config push applies to the next one without a restart:
//
//	handoff: false  # leave given-up tokens pending for the 10-minute age gate
//
// Only an explicit false turns the handoff off. An absent file, one without
// the key, or one that does not decode keeps it on.
const redisContractCompensationResourceName = "redis_contract_compensation.yml"

type redisContractCompensationConfig struct {
	Handoff *bool `yaml:"handoff"`
}

// The switch decoded once per config version.
var redisCompensationHandoffSwitch server.ResourceValueCache[server.ResourceVersion, bool]

// Whether the operator switch leaves the give-up handoff on.
func redisCompensationHandoffEnabled() bool {
	resource, err := server.Config.SimpleResource(redisContractCompensationResourceName)
	if err != nil {
		return true
	}
	return redisCompensationHandoffSwitch.Get(resource.Version(), nil, func() (bool, bool) {
		data, err := resource.BytesE()
		if err != nil {
			// a failed read is retried by the next give-up
			return true, false
		}
		var config redisContractCompensationConfig
		if err := yaml.Unmarshal(data, &config); err != nil {
			return true, true
		}
		return config.Handoff == nil || *config.Handoff, true
	})
}

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
			admission.noteRecoveryRerun()
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

// Moves one request's pending marker on a balance to the recovery marker,
// keeping its score, as a page enrolls its own request's pending markers. It
// changes no token amount or lease. A replay finds no pending marker and
// changes nothing.
const redisReservationHandoffScript = `
local score=redis.call('ZSCORE',KEYS[6],ARGV[1])
if not score then return 0 end
redis.call('ZADD',KEYS[5],'NX',score,ARGV[1]); redis.call('ZREM',KEYS[6],ARGV[1]); redis.call('PEXPIRE',KEYS[5],90000000)
return 1
`

// Reports whether the request's pending marker on the balance moved to the
// recovery marker. Only the request's own live admission may call this, with
// its attempts read under its pending lock (handOffReservation): Redis cannot
// tell an abandoned pending token from one a live attempt may still publish.
// It runs outside any PostgreSQL transaction, so as redisContractReservation
// does there, it sends no preflight PING and waits out a late reply on the
// late-reply pool rather than closing the connection.
func redisReservationHandoff(ctx context.Context, balanceId, contractId server.Id) (bool, error) {
	bounded, cancel := context.WithTimeout(ctx, redisContractAdmissionTimeout)
	defer cancel()
	var moved bool
	err := server.RedisWithDeadline(bounded, func(client server.RedisClient) error {
		value, err := client.Eval(bounded, redisReservationHandoffScript, redisContractReservationKeys(balanceId), contractId.String()).Int64()
		if err != nil {
			return err
		}
		moved = value == 1
		return nil
	}, server.OptRedisNoPreflight(), server.OptRedisKeepLateReplies())
	return moved, err
}

// The caller's error/timeout is not cleanup's lifetime. One finite budget owns
// every balance and every retry after PG has joined; tests supply an owned
// clock/transport to prove long transient intervals without wall-clock sleeps,
// and an owned deferred owner.
type redisReservationCleanup struct {
	now  func() time.Time
	wait func(context.Context, time.Duration) error
	// Calls fire once delay has passed, holding no goroutine meanwhile, and
	// returns what cancels that call. The deferred owner times each retry with
	// it, so a compensation waiting out its backoff holds no worker.
	after   func(time.Duration, func()) func() bool
	recover func(context.Context, server.Id, server.Id) error
	// Moves a token the deferred owner gave up on to the recovery marker; nil
	// leaves it pending for aged recovery.
	handoff  func(context.Context, server.Id, server.Id) error
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
// A token the deferred owner gives up on passes to the recovery marker.
func (self *redisContractAdmission) reservationCleanup() redisReservationCleanup {
	cleanup := defaultRedisReservationCleanup()
	cleanup.recover = func(ctx context.Context, balanceId, _ server.Id) error {
		return self.releaseReservation(ctx, balanceId)
	}
	cleanup.handoff = func(ctx context.Context, balanceId, _ server.Id) error {
		return self.handOffReservation(ctx, balanceId)
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

// Moves this request's pending token on one balance to the recovery marker
// once the deferred owner has given up releasing it, so the next page on the
// balance takes it at once, as the request's own page would have, and releases
// it through the fenced SQL custody check rather than after
// redisPendingRecoveryAge. As releaseReservation does, it reads the request's
// attempts when it runs, under the pending lock: a token an attempt in flight
// may hold, or one a publication may have taken, keeps its pending marker. One
// call, which a replay repeats as a no-op.
func (self *redisContractAdmission) handOffReservation(ctx context.Context, balanceId server.Id) error {
	return self.retirePending(func(inFlight bool) error {
		if inFlight || self.publicationHasStarted() {
			redisContractReservationResults.WithLabelValues("handoff", "skipped").Inc()
			return nil
		}
		moved, err := redisReservationHandoff(ctx, balanceId, self.contractId)
		result := "absent"
		if err != nil {
			result = "error"
		} else if moved {
			result = "moved"
		}
		redisContractReservationResults.WithLabelValues("handoff", result).Inc()
		return err
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
		},
		after:    redisReservationAfter,
		deferred: deferredRedisCompensations}
}

// Times a deferred retry on the runtime's timers, which hold no goroutine.
func redisReservationAfter(delay time.Duration, fire func()) func() bool {
	return time.AfterFunc(delay, fire).Stop
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
	// passes the deferred owner has made
	deferredPasses int
	// no deferred pass fits in the budget
	exhausted bool
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

// The deferred owner's turn of the compensation, on a worker: a pass while the
// budget allows one, then the delay before the next pass, or false once the
// compensation has ended, counted by its outcome. Every deferred pass follows
// a backoff after the one before, the request's own pass first. A
// compensation whose budget left the owner no pass, because no retry fit in it
// or it ended while the compensation waited for a worker, is counted expired.
func (self *redisReservationCompensation) step(owner context.Context) (time.Duration, bool) {
	select {
	case <-owner.Done():
		// A quiesce leaves the tokens pending, as process exit does.
		self.failures = append(self.failures, owner.Err())
		self.finish()
		return 0, false
	default:
	}
	if !self.exhausted && self.cleanup.now().Before(self.end) {
		self.deferredPasses++
		self.attempt(owner)
		if len(self.pending) == 0 {
			self.finish()
			return 0, false
		}
		if delay, ok := self.NextDeferredRetry(); ok {
			return delay, true
		}
	}
	// The budget leaves no further pass: the owner gives up.
	self.handOff(owner)
	if self.deferredPasses == 0 {
		redisContractReservationResults.WithLabelValues("compensation", "expired").Inc()
		return 0, false
	}
	self.failures = append(self.failures, context.DeadlineExceeded)
	self.finish()
	return 0, false
}

// Passes each balance still pending to the recovery marker once the owner
// gives up on it, so the next page on the balance takes it at once: one call
// per balance, each begun within one admission round trip's bound (a late
// reply can end the last up to a socket timeout after it). A balance the
// handoff misses, or one a quiesce reaches first, keeps its pending marker for
// aged recovery.
func (self *redisReservationCompensation) handOff(owner context.Context) {
	if self.cleanup.handoff == nil || len(self.pending) == 0 {
		return
	}
	if refusal := self.cleanup.deferred.handoffRefusal(); refusal != "" {
		// counted per balance, as each handoff is
		redisContractReservationResults.WithLabelValues("handoff", refusal).Add(float64(len(self.pending)))
		return
	}
	bounded, cancel := context.WithTimeout(owner, redisContractAdmissionTimeout)
	defer cancel()
	for _, id := range self.pending {
		select {
		case <-bounded.Done():
			return
		default:
		}
		// the handoff counts its own outcome
		_ = self.cleanup.handoff(bounded, id, self.contractId)
	}
}

// One pass within the request's budget: each pending balance the pass did not
// reach before its owner or the budget ended stays pending.
func (self *redisReservationCompensation) attempt(owner context.Context) {
	self.attemptUntil(owner, func() bool { return self.ended(owner) })
}

// The request's own pass ends with its own deadline alone, not with the shared
// budget: an insufficient-balance rerun may have spent that budget, and the
// compensation after it still makes this pass.
func (self *redisReservationCompensation) attemptOwn(pass context.Context) {
	self.attemptUntil(pass, func() bool {
		select {
		case <-pass.Done():
			return true
		default:
			return false
		}
	})
}

// One pass tries each pending balance once. A transient failure stays pending,
// as does each balance the pass did not reach before over reported its end.
// Any other failure leaves its balance to retained recovery.
func (self *redisReservationCompensation) attemptUntil(owner context.Context, over func() bool) {
	next := make([]server.Id, 0, len(self.pending))
	for index, id := range self.pending {
		if over() {
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

// Each call advances the retry: the backoff before the next deferred pass, and
// whether that pass falls within the budget. The deferred owner does not wait
// for a pass the budget cannot reach.
func (self *redisReservationCompensation) NextDeferredRetry() (time.Duration, bool) {
	delay := redisReservationCleanupRetryDelay(self.retries, mathrand.Float64())
	self.retries++
	return delay, self.cleanup.now().Add(delay).Before(self.end)
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
	self.attemptOwn(pass)
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
// left pending. Its workers are its slots: a worker holds a compensation only
// for a pass, while its release calls run, and between passes the compensation
// waits on a timer for its backoff, holding no worker or goroutine. So however
// many requests fail, a failing Redis sees at most one compensation call per
// worker beyond each request's own pass, on the same deadline pool, and the
// owner's throughput is bounded by those concurrent calls, never by backoff.
// The owner holds a bounded number of balances, since one request can hold one
// per selected grant. A compensation the owner takes counts deferred, then
// completed, pending, or expired when its budget left the owner no pass.
//
// When a compensation's budget leaves no further pass, the owner gives up and
// moves each token still pending to the recovery marker in one call, unless an
// attempt of its request is in flight or has started publication. The next
// page on the balance then releases the token through the fenced SQL custody
// check at once, as the request's own page would have.
//
// A full owner drops a compensation, counted dropped, without blocking or
// starting a goroutine, and a quiesce ends those it holds, as process exit
// does. That only defers the release, as a handoff that fails does: each token
// stays reserved, which can under-admit its payer but never over-admit, and
// keeps its pending marker, the state recovery reclaims for a request that
// died. Any page on the balance takes a pending marker older than
// redisPendingRecoveryAge, and the fenced SQL custody check then releases each
// token no contract holds. A returned request starts no attempt that could
// publish the token, and its lease ends it at last.
type redisDeferredCompensations struct {
	workers  int
	capacity int
	start    sync.Once
	// release and handoff calls its workers are making
	calls atomic.Int64
	// the clock of the handoff's quiet period after a timeout
	now func() time.Time
	// when one of its calls last ended in a timeout, in Unix nanoseconds
	lastTimeout atomic.Int64

	stateLock sync.Mutex
	// signaled when a compensation falls due or the workers stop
	wake *sync.Cond
	// compensations due for a pass, in the order they fell due
	due     []*redisDeferredCompensation
	stopped bool
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
	// the retry the compensation waits for, guarded by the owner's state lock
	retry *redisDeferredRetry
}

// One timed retry of a deferred compensation. Its timer makes the compensation
// due, unless a quiesce ends the retry first; whichever comes first ends it.
type redisDeferredRetry struct {
	// guarded by the owner's state lock
	ended bool
	stop  func() bool
}

var deferredRedisCompensations = newRedisDeferredCompensations(redisDeferredCompensationWorkers, redisDeferredCompensationBalances)

var redisDeferredCompensationBalancesHeld = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
	Name: "urnetwork_redis_contract_compensation_balances_held",
	Help: "Balances the process-wide deferred compensation owner holds, waiting for a retry, due, or in a pass. It drops new compensations past 1024.",
}, func() float64 {
	return float64(deferredRedisCompensations.heldBalances())
})

var redisDeferredCompensationCallsInflight = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
	Name: "urnetwork_redis_contract_compensation_calls_inflight",
	Help: "Release and handoff calls the process-wide deferred compensation owner's workers are making, at most one per worker (4).",
}, func() float64 {
	return float64(deferredRedisCompensations.calls.Load())
})

// A test environment reset ends the deferred compensations before its Redis and
// PostgreSQL go away, as process exit would, so none runs in the next one. A
// worker still blocked past the bound is abandoned, as at exit.
func init() {
	prometheus.MustRegister(redisDeferredCompensationBalancesHeld, redisDeferredCompensationCallsInflight)
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
	deferred := &redisDeferredCompensations{
		workers:    workers,
		capacity:   capacity,
		now:        time.Now,
		idle:       idle,
		generation: generation,
		cancel:     cancel,
	}
	deferred.wake = sync.NewCond(&deferred.stateLock)
	return deferred
}

// Reports whether the owner will run the compensation, within its budget. The
// first offer starts the workers, which run for the life of the process. The
// first deferred pass follows a backoff after the request's own pass.
func (self *redisDeferredCompensations) offer(ctx context.Context, compensation *redisReservationCompensation) bool {
	self.start.Do(func() {
		for range self.workers {
			go self.work()
		}
	})
	deferred := self.hold(ctx, compensation)
	if deferred == nil {
		return false
	}
	if delay, ok := compensation.NextDeferredRetry(); !ok {
		// No deferred pass fits in the budget: a worker ends it at once.
		compensation.exhausted = true
		self.fall(deferred)
	} else if !self.schedule(deferred, delay) {
		// a quiesce came first
		self.quiesced(deferred)
	}
	return true
}

// Takes the compensation's balances if they fit; nil when the owner is full.
func (self *redisDeferredCompensations) hold(ctx context.Context, compensation *redisReservationCompensation) *redisDeferredCompensation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	balances := len(compensation.pending)
	if self.capacity < self.held+balances {
		return nil
	}
	// The request's values stay; its cancellation and synchronous stage
	// timing end when its caller returns. The budget ends its passes, and a
	// quiesce ends it early: at once while it waits for a retry, otherwise
	// when its worker finds its owner ended.
	owner, cancel := context.WithCancel(server.WithoutContractCreationTiming(context.WithoutCancel(ctx)))
	deferred := &redisDeferredCompensation{owner: owner, balances: balances, compensation: compensation}
	compensation.cleanup.recover = self.counted(compensation.cleanup.recover)
	compensation.cleanup.handoff = self.counted(compensation.cleanup.handoff)
	compensation.cleanup.deferred = self
	unlink := context.AfterFunc(self.generation, func() {
		cancel()
		self.unschedule(deferred)
	})
	deferred.stop = func() { unlink(); cancel() }
	if self.held == 0 {
		self.idle = make(chan struct{})
	}
	self.held += balances
	return deferred
}

// Counts one of the owner's release or handoff calls while it runs, and notes
// when one ends in a timeout.
func (self *redisDeferredCompensations) counted(call func(context.Context, server.Id, server.Id) error) func(context.Context, server.Id, server.Id) error {
	if call == nil {
		return nil
	}
	return func(ctx context.Context, balanceId, contractId server.Id) error {
		self.calls.Add(1)
		defer self.calls.Add(-1)
		err := call(ctx, balanceId, contractId)
		if redisReservationTimedOut(err) {
			self.lastTimeout.Store(self.now().UnixNano())
		}
		return err
	}
}

// Why the owner hands no given-up token to recovery now, or "" when it may:
// the operator switch is off, or one of its calls timed out within
// redisDeferredCompensationHandoffQuiet. Without an owner, only the switch
// applies.
func (self *redisDeferredCompensations) handoffRefusal() string {
	if !redisCompensationHandoffEnabled() {
		return "disabled"
	}
	if self == nil {
		return ""
	}
	if last := self.lastTimeout.Load(); last != 0 && self.now().Before(time.Unix(0, last).Add(redisDeferredCompensationHandoffQuiet)) {
		return "slow"
	}
	return ""
}

// Times the compensation's next pass after delay, and reports whether it did:
// not once a quiesce has come. Until the retry falls due the compensation holds
// no worker, and a quiesce ends it.
func (self *redisDeferredCompensations) schedule(deferred *redisDeferredCompensation, delay time.Duration) bool {
	retry := func() *redisDeferredRetry {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		select {
		case <-deferred.owner.Done():
			return nil
		default:
		}
		deferred.retry = &redisDeferredRetry{}
		return deferred.retry
	}()
	if retry == nil {
		return false
	}
	after := deferred.compensation.cleanup.after
	if after == nil {
		// A cleanup made without a timer must not raise here, outside the
		// worker's recovery, which would end the process.
		after = redisReservationAfter
	}
	stop := after(delay, func() { self.fire(deferred, retry) })
	kept := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if retry.ended {
			return false
		}
		retry.stop = stop
		return true
	}()
	if !kept {
		// the retry fell due, or a quiesce ended it, before its timer was kept
		stop()
	}
	return true
}

// The retry's timer fired: the compensation waits for the next free worker.
func (self *redisDeferredCompensations) fire(deferred *redisDeferredCompensation, retry *redisDeferredRetry) {
	due := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if retry.ended {
			return false
		}
		retry.ended = true
		deferred.retry = nil
		return true
	}()
	if due {
		self.fall(deferred)
	}
}

// Makes the compensation due for the next free worker.
func (self *redisDeferredCompensations) fall(deferred *redisDeferredCompensation) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.due = append(self.due, deferred)
	self.wake.Signal()
}

// A quiesce ends a compensation that waits for its retry at once. One that is
// due or in a pass ends when its worker finds its owner ended.
func (self *redisDeferredCompensations) unschedule(deferred *redisDeferredCompensation) {
	stop, waiting := func() (func() bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		retry := deferred.retry
		if retry == nil || retry.ended {
			return nil, false
		}
		retry.ended = true
		deferred.retry = nil
		return retry.stop, true
	}()
	if !waiting {
		return
	}
	if stop != nil {
		stop()
	}
	self.quiesced(deferred)
}

// Ends a compensation a quiesce reached, once. Its tokens stay pending, as at
// process exit.
func (self *redisDeferredCompensations) quiesced(deferred *redisDeferredCompensation) {
	defer self.end(deferred)
	server.HandleError(func() {
		deferred.compensation.failures = append(deferred.compensation.failures, context.Canceled)
		deferred.compensation.finish()
	})
}

func (self *redisDeferredCompensations) work() {
	for {
		deferred := self.next()
		if deferred == nil {
			return
		}
		self.run(deferred)
	}
}

// Waits for the next compensation due for a pass; nil once the workers stop.
func (self *redisDeferredCompensations) next() *redisDeferredCompensation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for len(self.due) == 0 && !self.stopped {
		self.wake.Wait()
	}
	if len(self.due) == 0 {
		return nil
	}
	deferred := self.due[0]
	self.due[0] = nil
	self.due = self.due[1:]
	return deferred
}

// The compensation's turn on a worker: a pass, then its next retry or its end,
// which happens exactly once. A raise ends the compensation pending, and the
// worker goes on.
func (self *redisDeferredCompensations) run(deferred *redisDeferredCompensation) {
	var delay time.Duration
	retry := false
	server.HandleError(func() {
		delay, retry = deferred.compensation.step(deferred.owner)
	}, func() {
		redisContractReservationResults.WithLabelValues("compensation", "pending").Inc()
	})
	switch {
	case !retry:
		self.end(deferred)
	case !self.schedule(deferred, delay):
		// a quiesce came first
		self.quiesced(deferred)
	}
}

// Lets the compensation go.
func (self *redisDeferredCompensations) end(deferred *redisDeferredCompensation) {
	deferred.stop()
	self.release(deferred.balances)
}

func (self *redisDeferredCompensations) release(balances int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.held -= balances
	if self.held == 0 {
		close(self.idle)
	}
}

// The balances offered and not yet ended.
func (self *redisDeferredCompensations) heldBalances() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.held
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

// Stops the workers once no compensation is due. Only a test stops its own
// owner; the process-wide owner's workers run for the life of the process.
func (self *redisDeferredCompensations) stopWorkers() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.stopped = true
	self.wake.Broadcast()
}

// Whether a release or handoff call ended in a timeout: a slow Redis node, or a
// deadline-pool or PostgreSQL wait that outlived the call. Any observed leaf
// cause decides, in a bounded inspection.
func redisReservationTimedOut(err error) bool {
	if err == nil {
		return false
	}
	for _, node := range server.InspectErrorCauses(err).Nodes {
		if !node.Leaf {
			continue
		}
		cause := node.Err
		if cause == context.DeadlineExceeded || cause == os.ErrDeadlineExceeded || cause == syscall.ETIMEDOUT {
			return true
		}
		if timeout, ok := cause.(net.Error); ok && timeout.Timeout() {
			return true
		}
	}
	return false
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
