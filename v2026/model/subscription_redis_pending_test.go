// The pending reservation protocol: a request reserves before its contract
// transaction, and recovery never reaches a live request's pending token.
package model

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// A live request's tokens reach recovery only after its publication window
// has closed. The window is measured on the request's monotonic clock, the
// marker age by the Redis slot master's TIME: a forward step of that clock, or
// a failover to a replica whose clock leads, shortens the age by as much, so
// the margin is minutes. The window itself spans pooler waits for keyset pages
// after the first reservation and for BEGIN.
func TestRedisPendingRecoveryAgeExceedsPublicationWindow(t *testing.T) {
	if redisPendingPublicationTimeout < time.Minute {
		t.Fatalf("publication window %s is shorter than a minute of pooler waits", redisPendingPublicationTimeout)
	}
	if redisPendingRecoveryAge-redisPendingPublicationTimeout < 5*time.Minute {
		t.Fatalf("pending recovery age %s leaves less than five minutes of clock margin over the %s publication window", redisPendingRecoveryAge, redisPendingPublicationTimeout)
	}
}

func redisPendingRequireMarkers(t testing.TB, ctx context.Context, balance, contract server.Id, pending, recovery bool) {
	t.Helper()
	server.Redis(ctx, func(client server.RedisClient) {
		keys := redisContractReservationKeys(balance)
		for _, check := range []struct {
			key  string
			want bool
		}{{keys[5], pending}, {keys[4], recovery}} {
			_, err := client.ZScore(ctx, check.key, contract.String()).Result()
			if err != nil && !errors.Is(err, redis.Nil) {
				t.Fatal(err)
			}
			if (err == nil) != check.want {
				t.Fatalf("marker %s present=%v want=%v", check.key, err == nil, check.want)
			}
		}
	})
}

// Another request's recovery page, which runs after every public creation on
// the balance, neither enrolls nor releases a pending token before the
// recovery age, even when its request holds no transaction or fence: that
// request may still be about to take its fence and publish. Once the marker
// is older than the recovery age, the next page enrolls it and the fenced SQL
// custody check releases it.
func TestRedisPendingReservationIsInvisibleToRecoveryUntilAged(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		request := withRedisContractAdmission(ctx)
		owner := redisAdmissionFromContext(request)
		_, finish := reserveRedisAdmissionTestBeforeTx(t, request, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23)
		defer finish()
		redisPendingRequireMarkers(t, ctx, f.balanceId, owner.contractId, true, false)

		first := createRedisAdmissionTest(ctx, f, 17)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 40 {
			t.Fatal("recovery released a live pending reservation", got)
		}
		redisPendingRequireMarkers(t, ctx, f.balanceId, owner.contractId, true, false)

		redisRecoveryAgePendingMarker(t, ctx, f.balanceId, owner.contractId)
		last := createRedisAdmissionTest(ctx, f, 29)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 46 {
			t.Fatal("recovery did not take over a pending reservation past its age", got)
		}
		redisPendingRequireMarkers(t, ctx, f.balanceId, owner.contractId, false, false)
		server.Redis(ctx, func(client server.RedisClient) {
			tokens, err := client.HGetAll(ctx, redisContractReservationKeys(f.balanceId)[1]).Result()
			server.Raise(err)
			if len(tokens) != 2 || tokens[first.ContractId.String()] != "17" || tokens[last.ContractId.String()] != "29" {
				t.Fatal("recovery changed a published token", tokens)
			}
		})
	})
}

// A transaction that takes its fence after the publication window publishes
// nothing, and the request's compensation releases the token.
func TestRedisPendingReservationPastPublicationWindowRefuses(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		refusalsBefore := testutil.ToFloat64(redisPendingPublicationRefusals.WithLabelValues("window_expired"))
		escrow, err := runRedisContractAdmission(ctx, func(attempt context.Context) (*TransferEscrow, error) {
			reserved, finish, err := reserveRedisTransferEscrowBeforeTx(attempt, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23)
			if err != nil {
				return nil, err
			}
			defer finish()
			// as if the wait for the transaction outlasted the window
			redisPendingReservationFromContext(reserved).startTime = time.Now().Add(-redisPendingPublicationTimeout)
			var escrow *TransferEscrow
			server.Tx(reserved, func(tx server.PgTx) {
				escrow, _, err = createTransferEscrowInTx(reserved, tx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23, nil)
			}, server.TxReadCommitted, server.OptNoRetry())
			return escrow, err
		})
		if escrow != nil || !errors.Is(err, ErrRedisPendingReservationExpired) {
			t.Fatal("a reservation past its publication window was published", escrow, err)
		}
		drainRedisContractCompensations(t, ctx)
		if refusals := testutil.ToFloat64(redisPendingPublicationRefusals.WithLabelValues("window_expired")) - refusalsBefore; refusals != 1 {
			t.Fatal("the expired publication was not counted", refusals)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatal("the expired attempt kept its reservation", got)
		}
		requireRedisRefusalOnlyContract(t, ctx, f, neighbor.ContractId)
	})
}

// Attempts of one request share its tokens. A release by the request itself
// while another attempt has reserved but not yet taken its fence advances
// the epoch, so that attempt publishes nothing; the release takes the fenced
// SQL custody path because an attempt is in flight.
func TestRedisPendingReservationSameRequestReleaseStopsPublication(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		request := withRedisContractAdmission(ctx)
		owner := redisAdmissionFromContext(request)
		reserved, finish := reserveRedisAdmissionTestBeforeTx(t, request, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23)
		defer finish()
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 40 {
			t.Fatal("pending reservation fixture differs", got)
		}
		refusalsBefore := testutil.ToFloat64(redisPendingPublicationRefusals.WithLabelValues("released"))
		if err := owner.compensate(ctx); err != nil {
			t.Fatal("the request's own release failed", err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatal("the request's own release kept its token", got)
		}
		var publishErr error
		server.Tx(reserved, func(tx server.PgTx) {
			var escrow *TransferEscrow
			escrow, _, publishErr = createTransferEscrowInTx(reserved, tx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23, nil)
			if escrow != nil {
				t.Fatal("an attempt published a token its own request released", escrow)
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if !errors.Is(publishErr, ErrRedisPendingReservationReleased) {
			t.Fatal("the released attempt was not refused", publishErr)
		}
		if refusals := testutil.ToFloat64(redisPendingPublicationRefusals.WithLabelValues("released")) - refusalsBefore; refusals != 1 {
			t.Fatal("the released publication was not counted", refusals)
		}
		requireRedisRefusalOnlyContract(t, ctx, f, neighbor.ContractId)
	})
}

// A refused request releases its pending token in Redis alone: no PostgreSQL
// transaction is opened to compensate it, and none holds a Redis round trip.
func TestRedisPendingCompensationOpensNoTransaction(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false, deactivate_time=$2 WHERE client_id=$1`, f.destinationId, server.NowUtc()))
		})
		observation, uninstall := server.Testing_ObserveTxIo()
		defer uninstall()
		escrow, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 23)
		// a compensation left to the deferred owner is observed too
		drainRedisContractCompensations(t, ctx)
		uninstall()
		if escrow != nil || !errors.Is(err, ErrContractDestinationInactive) {
			t.Fatal("the refusal fixture did not reach the client fence", escrow, err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatal("the refused request kept its token", got)
		}
		for _, statement := range observation.FirstStatements() {
			if strings.Contains(statement.TxSite, "recoverRedisReservationRequest") {
				t.Fatalf("compensation opened a transaction: %s", statement.TxSite)
			}
		}
		if events := observation.Events(); len(events) != 0 {
			t.Fatalf("a transaction was held open across I/O: %+v", events)
		}
		requireRedisRefusalOnlyContract(t, ctx, f, neighbor.ContractId)
	})
}

// The previous release's scripts run during a rolling deploy. Its recovery
// page reads only the recovery marker, and its owned release requires one, so
// it can neither discover nor release a pending token.
func TestRedisPendingReservationIsUnreachableByPreviousRelease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		request := withRedisContractAdmission(ctx)
		owner := redisAdmissionFromContext(request)
		_, finish := reserveRedisAdmissionTestBeforeTx(t, request, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23)
		defer finish()
		redisRecoveryAgePendingMarker(t, ctx, f.balanceId, owner.contractId)
		previousKeys := redisContractReservationKeys(f.balanceId)[:5]
		server.Redis(ctx, func(client server.RedisClient) {
			candidates, err := client.Eval(ctx, redisReservationRecoveryPageScriptPrevious, previousKeys).StringSlice()
			server.Raise(err)
			if len(candidates) != 0 {
				t.Fatal("the previous recovery page discovered a pending token", candidates)
			}
			_, err = client.Eval(ctx, redisContractReservationScriptPrevious, previousKeys,
				"release-owned", owner.contractId.String(), "0", "23", redisContractReservationLease.Milliseconds()).Result()
			if err == nil || !strings.Contains(err.Error(), "missing owned reservation marker") {
				t.Fatal("the previous owned release accepted a pending token", err)
			}
			// a previous settlement's plain release of a published contract
			// removes the token; the pending marker it leaves is stale
			_, err = client.Eval(ctx, redisContractReservationScriptPrevious, previousKeys,
				"release", owner.contractId.String(), "0", "0", redisContractReservationLease.Milliseconds()).Result()
			server.Raise(err)
		})
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 0 {
			t.Fatal("the previous plain release left the token", got)
		}
		// the current page enrolls the stale pending marker and drops it
		page, cancel := context.WithTimeout(ctx, redisContractAdmissionTimeout)
		defer cancel()
		candidates, err := redisReservationRecoveryPage(page, f.balanceId, "")
		if err != nil || len(candidates) != 0 {
			t.Fatal("a stale pending marker became a recovery candidate", candidates, err)
		}
		redisPendingRequireMarkers(t, ctx, f.balanceId, owner.contractId, false, false)
	})
}

// The reservation script's operations keep one token per request: a retry
// of the request takes over its pending token, restarting its window, and
// refuses a token that is no longer pending.
func TestRedisPendingReservationScriptOwnership(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		balance, contract := server.NewId(), server.NewId()
		reserve := func(operation string, requested ByteCount) (ByteCount, error) {
			return redisContractReservation(ctx, operation, balance, contract, 1000, requested, redisContractReservationLease)
		}
		pendingScore := func() float64 {
			var score float64
			server.Redis(ctx, func(client server.RedisClient) {
				var err error
				score, err = client.ZScore(ctx, redisContractReservationKeys(balance)[5], contract.String()).Result()
				server.Raise(err)
			})
			return score
		}
		if amount, err := reserve("reserve-pending", 23); err != nil || amount != 23 {
			t.Fatal("pending reservation", amount, err)
		}
		redisRecoveryAgePendingMarker(t, ctx, balance, contract)
		aged := pendingScore()
		if amount, err := reserve("reserve-pending", 23); err != nil || amount != 23 || pendingScore() <= aged {
			t.Fatal("a retry did not take over its pending token and restart its window", amount, err)
		}
		if _, err := reserve("reserve-pending", 17); err == nil || !strings.Contains(err.Error(), "retry changed amount") {
			t.Fatal("a smaller retry took over a larger token", err)
		}
		page, cancel := context.WithTimeout(ctx, redisContractAdmissionTimeout)
		defer cancel()
		if candidates, err := redisReservationRecoveryPage(page, balance, contract.String()); err != nil || len(candidates) != 1 || candidates[0].contractId != contract || candidates[0].amount != 23 {
			t.Fatal("the request's own page did not enroll its pending token", candidates, err)
		}
		redisPendingRequireMarkers(t, ctx, balance, contract, false, true)
		if _, err := reserve("reserve-pending", 23); !errors.Is(err, errRedisReservationNotPending) {
			t.Fatal("a retry took over an enrolled token", err)
		}
		if _, err := redisContractReservation(ctx, "release-pending", balance, contract, 0, 23, redisContractReservationLease); !errors.Is(err, errRedisReservationNotPending) {
			t.Fatal("the direct release took an enrolled token", err)
		}
		if amount, err := redisContractReservation(ctx, "release-owned", balance, contract, 0, 23, redisContractReservationLease); err != nil || amount != 23 {
			t.Fatal("the fenced release of an enrolled token", amount, err)
		}
		if amount, err := reserve("reserve-pending-full", 1001); err != nil || amount != 0 {
			t.Fatal("a whole-grant reservation took partial credit", amount, err)
		}
		if amount, err := reserve("reserve-pending", 23); err != nil || amount != 23 {
			t.Fatal("pending reservation after release", amount, err)
		}
		if amount, err := redisContractReservation(ctx, "release-pending", balance, contract, 0, 23, redisContractReservationLease); err != nil || amount != 23 {
			t.Fatal("the direct release of a pending token", amount, err)
		}
		redisPendingRequireMarkers(t, ctx, balance, contract, false, false)
		if got := Testing_NetEscrowByteCount(ctx, balance); got != 0 {
			t.Fatal("released reservations left a total", got)
		}
	})
}

// Compensation may begin while another attempt of the same request is
// publishing: its first, fenced attempt finds the request fence held and
// retries. That attempt then commits and ends. The retry must leave the token
// it published, although no attempt is in flight any more and the token stays
// pending until the publishing attempt's own page acknowledges it.
func TestRedisPendingCompensationRetryKeepsPublishedToken(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		request := withRedisContractAdmission(ctx)
		owner := redisAdmissionFromContext(request)
		reserved, finish := reserveRedisAdmissionTestBeforeTx(t, request, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23)
		defer finish()
		// compensation began before the publication started
		ids := owner.compensationIds()
		if len(ids) != 1 || ids[0] != f.balanceId {
			t.Fatal("compensation fixture differs", ids)
		}
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer held.Rollback(context.Background())
		published, _, err := createTransferEscrowInTx(reserved, held, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23, nil)
		if err != nil || published == nil {
			t.Fatal("publishing attempt fixture failed", err)
		}
		attempts := 0
		cleanup := owner.reservationCleanup()
		release := cleanup.recover
		cleanup.recover = func(owned context.Context, balance, contract server.Id) error {
			attempts++
			return release(owned, balance, contract)
		}
		cleanup.wait = func(context.Context, time.Duration) error {
			// the publishing attempt commits and ends between compensation attempts
			server.Raise(held.Commit(ctx))
			finish()
			return nil
		}
		if err := cleanup.run(ctx, owner.contractId, ids); err != nil {
			t.Fatal("compensation did not complete after the publication", err)
		}
		if attempts != 2 {
			t.Fatal("the publishing attempt's fence did not hold the first compensation attempt", attempts)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 40 {
			t.Fatal("compensation released a token its own request published", got)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			tokens, err := client.HGetAll(ctx, redisContractReservationKeys(f.balanceId)[1]).Result()
			server.Raise(err)
			if len(tokens) != 2 || tokens[neighbor.ContractId.String()] != "17" || tokens[published.ContractId.String()] != "23" {
				t.Fatal("compensation changed a published token", tokens)
			}
		})
		// the fenced custody check acknowledged the committed contract
		redisRecoveryRequireMarker(t, ctx, f.balanceId, published.ContractId, false)
	})
}

// Fails only grant pages that continue after a keyset cursor.
type redisSelectionPageFaultQuery struct {
	query server.PgCanQuery
	fault error
}

func (self *redisSelectionPageFaultQuery) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "(end_time,start_time,balance_id)>(") {
		return nil, self.fault
	}
	return self.query.Query(ctx, sql, args...)
}

// A grant page that fails after earlier pages reserved tokens panics out of
// the selection. The attempt still ends, so the request releases its pending
// tokens in Redis alone: no transaction holds a Redis round trip to
// compensate them, and the request's own page is not kept from its markers.
func TestRedisPendingSelectionPanicEndsAttempt(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		clients := newEscrowSelectionTestClients(t, ctx)
		ids := redisSelectionTestGrants(t, ctx, clients, redisGrantPageRows+1, 1, false)
		fault := errors.New("synthetic second grant page failure")
		request := withRedisContractAdmission(context.WithValue(ctx, redisGrantReadContextKey{}, func(query server.PgCanQuery) server.PgCanQuery {
			return &redisSelectionPageFaultQuery{query: query, fault: fault}
		}))
		owner := redisAdmissionFromContext(request)
		observation, uninstall := server.Testing_ObserveTxIo()
		defer uninstall()
		var failure error
		if value := server.HandleError(func() {
			_, failure = CreateTransferEscrow(request, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, redisGrantPageRows+1)
		}); value != nil {
			failure, _ = value.(error)
		}
		// a compensation left to the deferred owner is observed too
		drainRedisContractCompensations(t, ctx)
		uninstall()
		if !errors.Is(failure, fault) {
			t.Fatal("the second grant page did not fail the request", failure)
		}
		owner.pendingLock.Lock()
		writers := owner.pendingWriters
		owner.pendingLock.Unlock()
		if writers != 0 {
			t.Fatal("the failed attempt never ended", writers)
		}
		for _, id := range ids {
			if got := Testing_NetEscrowByteCount(ctx, id); got != 0 {
				t.Fatal("the failed attempt kept a token", got)
			}
		}
		for _, event := range observation.Events() {
			t.Fatalf("compensation held a transaction across I/O: %s in %s from %s", event.Kind, event.TxSite, event.Site)
		}
	})
}

// Records when each grant read was issued.
type redisSelectionReadTimes struct {
	stateLock sync.Mutex
	issued    []time.Time
}

func (self *redisSelectionReadTimes) observe(query server.PgCanQuery) server.PgCanQuery {
	return &redisSelectionTimedQuery{query: query, times: self}
}

type redisSelectionTimedQuery struct {
	query server.PgCanQuery
	times *redisSelectionReadTimes
}

func (self *redisSelectionTimedQuery) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	func() {
		self.times.stateLock.Lock()
		defer self.times.stateLock.Unlock()
		self.times.issued = append(self.times.issued, time.Now())
	}()
	return self.query.Query(ctx, sql, args...)
}

// The publication window opens immediately before the attempt's first
// reservation request: pooler waits for the grant reads that precede it
// reserve nothing, and must not shorten the window.
func TestRedisPendingWindowStartsAtFirstReservation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		times := &redisSelectionReadTimes{}
		request := withRedisContractAdmission(context.WithValue(ctx, redisGrantReadContextKey{}, times.observe))
		reserved, finish := reserveRedisAdmissionTestBeforeTx(t, request, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23)
		defer finish()
		reservation := redisPendingReservationFromContext(reserved)
		times.stateLock.Lock()
		issued := append([]time.Time(nil), times.issued...)
		times.stateLock.Unlock()
		// the prober window read and the one keyset page precede the reservation
		if len(issued) != 2 || reservation == nil || reservation.startTime.Before(issued[len(issued)-1]) {
			t.Fatal("the publication window opened before the grant reads that precede the first reservation")
		}
	})
}

// The previous release's reservation and recovery page scripts, which run
// beside this release during a rolling deploy.
const redisContractReservationScriptPrevious = `
local function valid(s)
 return s and string.match(s, '^%d+$') and (s == '0' or string.sub(s,1,1) ~= '0') and
  (#s < 19 or (#s == 19 and s <= '9223372036854775807'))
end
local function less(a,b) return #a < #b or (#a == #b and a < b) end
local function sub(a,b)
 if less(a,b) then return '0' end
 local out = {}; local carry=0
 for i=0,#a-1 do
  local x=tonumber(string.sub(a,#a-i,#a-i))-carry
  local j=#b-i; local y=0; if j>0 then y=tonumber(string.sub(b,j,j)) end
  if x<y then x=x+10; carry=1 else carry=0 end
  table.insert(out,1,tostring(x-y))
 end
 local s=string.gsub(table.concat(out),'^0+',''); if s=='' then return '0' end; return s
end
local rawTotal=redis.call('GET',KEYS[1])
local total=rawTotal or '0'
local legacy=redis.call('GET',KEYS[4]) or '0'
for _,entry in ipairs({{2,'hash'},{3,'zset'},{5,'zset'}}) do
 local kind=redis.call('TYPE',KEYS[entry[1]]).ok
 if kind~='none' and kind~=entry[2] then return redis.error_reply('invalid reservation key type') end
end
if not valid(total) or not valid(legacy) or not valid(ARGV[3]) or not valid(ARGV[4]) then
 return redis.error_reply('invalid reservation counter')
end
local lease=tonumber(ARGV[5])
if not lease or lease<1 or lease>86400000 then return redis.error_reply('invalid reservation lease') end
-- Partial key eviction starts a new approximate generation too. Old releases
-- then find no token and cannot subtract a newly admitted neighbor's bytes.
if not rawTotal or (total~='0' and (redis.call('EXISTS',KEYS[2])==0 or redis.call('EXISTS',KEYS[3])==0)) then
 redis.call('DEL',KEYS[2],KEYS[3],KEYS[5]);total='0'
end
local now=redis.call('TIME'); local milliseconds=tonumber(now[1])*1000+math.floor(tonumber(now[2])/1000)
local expired=redis.call('ZRANGEBYSCORE',KEYS[3],'-inf',milliseconds,'LIMIT',0,32)
for _,token in ipairs(expired) do
 local amount=redis.call('HGET',KEYS[2],token)
 if amount then
  if not valid(amount) then return redis.error_reply('invalid reservation token') end
  total=sub(total,amount); redis.call('HDEL',KEYS[2],token)
 end
 redis.call('ZREM',KEYS[3],token); redis.call('ZREM',KEYS[5],token)
end
local existing=redis.call('HGET',KEYS[2],ARGV[2])
if existing and not valid(existing) then return redis.error_reply('invalid reservation token') end
local amount='0'
if ARGV[1]=='release-owned' and existing and not redis.call('ZSCORE',KEYS[5],ARGV[2]) then
 return redis.error_reply('missing owned reservation marker')
end
if (ARGV[1]=='release-owned' or ARGV[1]=='published') and existing and ARGV[4]~='0' and existing~=ARGV[4] then
 return redis.error_reply('reservation recovery amount changed')
end
if ARGV[1]=='release' or ARGV[1]=='release-owned' then
 if existing then
  if ARGV[1]=='release-owned' then amount=existing end
  total=sub(total,existing); redis.call('HDEL',KEYS[2],ARGV[2]); redis.call('ZREM',KEYS[3],ARGV[2])
 end
 redis.call('ZREM',KEYS[5],ARGV[2])
elseif ARGV[1]=='settle' then
 -- Never resurrect a token after the durable batch already released it.
 -- A missing/expired Redis generation retains its documented approximation.
 if existing then
  if less(existing,ARGV[4]) then return redis.error_reply('settled consumption exceeds reservation') end
  total=sub(total,sub(existing,ARGV[4])); amount=ARGV[4]
  redis.call('HSET',KEYS[2],ARGV[2],amount)
 end
elseif ARGV[1]=='reserve' or ARGV[1]=='reserve-owned' or ARGV[1]=='reserve-owned-full' or ARGV[1]=='restore' then
 if existing then
  if (ARGV[1]=='reserve-owned' or ARGV[1]=='reserve-owned-full') and not redis.call('ZSCORE',KEYS[5],ARGV[2]) then
   return redis.error_reply('missing owned reservation marker')
  end
  if less(ARGV[4],existing) then return redis.error_reply('reservation retry changed amount') end
  amount=existing
 else
  local available=sub(sub(ARGV[3],legacy),total)
  if ARGV[1]=='restore' then available=sub('9223372036854775807',total) end
  amount=ARGV[4]; if less(available,amount) then
   if ARGV[1]=='restore' then return redis.error_reply('reservation recovery overflow') end
   amount=available
   if ARGV[1]=='reserve-owned-full' then amount='0' end
  end
  if amount~='0' then
   -- The total remains <= the supplied signed-int64 credit, so INCRBY is exact.
   redis.call('SET',KEYS[1],total)
   redis.call('INCRBY',KEYS[1],amount); total=redis.call('GET',KEYS[1])
   redis.call('HSET',KEYS[2],ARGV[2],amount)
   redis.call('ZADD',KEYS[3],milliseconds+lease,ARGV[2])
   if ARGV[1]=='reserve-owned' or ARGV[1]=='reserve-owned-full' then redis.call('ZADD',KEYS[5],milliseconds,ARGV[2]) end
  end
 end
 if ARGV[1]=='restore' then redis.call('ZREM',KEYS[5],ARGV[2]) end
elseif ARGV[1]=='published' then
 redis.call('ZREM',KEYS[5],ARGV[2])
else return redis.error_reply('invalid reservation operation') end
-- Recovery may restore an older token with only a short lease remaining.
-- Never let that operation shorten the shared keys beneath younger tokens.
redis.call('SET',KEYS[1],total,'PX',90000000)
redis.call('PEXPIRE',KEYS[2],90000000)
redis.call('PEXPIRE',KEYS[3],90000000)
redis.call('PEXPIRE',KEYS[5],90000000)
return amount
`

const redisReservationRecoveryPageScriptPrevious = `
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
