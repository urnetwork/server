// Observe acknowledged publication at the real public entry and preserve the
// independent SQL proof for every uncertain, changed or unrelated candidate.
package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

var errRedisPublicationLostCommitReply = errors.New("synthetic acknowledged-publication commit reply loss")

// Recover through the typed handler so sentinel errors retain their identity.
func redisPublicationHandleError(action func()) (returnErr error) {
	server.HandleError(action, func(err error) { returnErr = err })
	return
}

type redisPublicationCommitQueryKey struct{}

// Only marked callers contribute counts. Releases are joined by connection
// identity because pgxpool's release callback does not carry a context.
type redisPublicationObserver struct {
	stateLock     sync.Mutex
	acquisitions  int
	releases      int
	acquireErrors int
	queryStarts   int
	queryEnds     int
	queryErrors   int
	custodyReads  int
	fences        int
	activeConnKVs map[*pgx.Conn]bool
	loseCommit    bool
	lostCommit    bool
	cancelCommit  context.CancelFunc
}

func (self *redisPublicationObserver) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return ctx
}

func (self *redisPublicationObserver) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	if ctx.Value(self) != true {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.acquisitions++
	if data.Err != nil || data.Conn == nil {
		self.acquireErrors++
		return
	}
	if self.activeConnKVs == nil {
		self.activeConnKVs = map[*pgx.Conn]bool{}
	}
	self.activeConnKVs[data.Conn] = true
}

func (self *redisPublicationObserver) TraceRelease(_ *pgxpool.Pool, data pgxpool.TraceReleaseData) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.activeConnKVs[data.Conn] {
		self.releases++
		delete(self.activeConnKVs, data.Conn)
	}
}

func (self *redisPublicationObserver) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if ctx.Value(self) != true {
		return ctx
	}
	sql := strings.Join(strings.Fields(data.SQL), " ")
	self.stateLock.Lock()
	self.queryStarts++
	if sql == "SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))" {
		self.fences++
	} else if strings.Contains(sql, "JOIN transfer_escrow e USING(contract_id)") && strings.Contains(sql, "FROM transfer_debit_journal") && strings.Contains(sql, "NOT applied") {
		self.custodyReads++
	}
	self.stateLock.Unlock()
	return context.WithValue(ctx, redisPublicationCommitQueryKey{}, sql == "commit")
}

func (self *redisPublicationObserver) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(self) != true {
		return
	}
	self.stateLock.Lock()
	self.queryEnds++
	if data.Err != nil {
		self.queryErrors++
	}
	lose := self.loseCommit && !self.lostCommit && data.Err == nil && ctx.Value(redisPublicationCommitQueryKey{}) == true
	if lose {
		self.lostCommit = true
	}
	var cancel context.CancelFunc
	if data.Err == nil && ctx.Value(redisPublicationCommitQueryKey{}) == true {
		cancel = self.cancelCommit
		self.cancelCommit = nil
	}
	self.stateLock.Unlock()
	if cancel != nil {
		cancel()
	}
	if lose {
		// The real backend has replied, but its owner never receives Commit's
		// successful return. This is reply-delivery injection, not a wire fault.
		panic(errRedisPublicationLostCommitReply)
	}
}

func (self *redisPublicationObserver) activeCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.activeConnKVs)
}

func (self *redisPublicationObserver) require(t testing.TB, acquisitions, custodyReads int) {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.acquisitions != acquisitions || self.custodyReads != custodyReads || self.fences != custodyReads {
		t.Fatalf("publication recovery counts acquisitions=%d custody=%d fences=%d; want %d/%d/%d", self.acquisitions, self.custodyReads, self.fences, acquisitions, custodyReads, custodyReads)
	}
	if self.acquireErrors != 0 || self.releases != self.acquisitions || len(self.activeConnKVs) != 0 || self.queryStarts != self.queryEnds || self.queryErrors != 0 {
		t.Fatalf("publication owner did not join: acquire_errors=%d acquisitions=%d releases=%d active=%d starts=%d ends=%d query_errors=%d", self.acquireErrors, self.acquisitions, self.releases, len(self.activeConnKVs), self.queryStarts, self.queryEnds, self.queryErrors)
	}
}

func observeRedisPublication(t testing.TB, ctx context.Context) (*redisPublicationObserver, context.Context, func()) {
	t.Helper()
	observer := &redisPublicationObserver{}
	scope, err := server.NewTestPgQueryScope(ctx, observer)
	if err != nil {
		t.Fatal(err)
	}
	return observer, context.WithValue(ctx, observer, true), func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}
}

// The hook records finite ownership counts and can change exactly one reply.
// Optional barriers are joined by the test's independent resource owner.
type redisPublicationReplyHook struct {
	stateLock sync.Mutex
	observer  *redisPublicationObserver
	key       string
	mode      string
	owners    []int
	started   chan server.Id
	release   chan struct{}
}

func (self *redisPublicationReplyHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (self *redisPublicationReplyHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (self *redisPublicationReplyHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		args := command.Args()
		if ctx.Value(self.observer) != true || command.Name() != "eval" || len(args) < 4 || args[3] != self.key {
			return next(ctx, command)
		}
		keys, err := strconv.Atoi(fmt.Sprint(args[2]))
		operation := 3 + keys
		if err != nil || keys < 1 || len(args) <= operation+1 || args[operation] != "published" {
			return next(ctx, command)
		}
		self.stateLock.Lock()
		self.owners = append(self.owners, self.observer.activeCount())
		first := len(self.owners) == 1
		self.stateLock.Unlock()
		if first && self.started != nil {
			id, err := server.ParseId(fmt.Sprint(args[operation+1]))
			if err != nil {
				return err
			}
			self.started <- id
			select {
			case <-self.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if first && self.mode == "before" {
			return io.ErrUnexpectedEOF
		}
		err = next(ctx, command)
		if first && self.mode == "after" && err == nil {
			return io.ErrUnexpectedEOF
		}
		return err
	}
}

func (self *redisPublicationReplyHook) require(t testing.TB, owners ...int) {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.owners) != len(owners) {
		t.Fatalf("publication attempts=%d; want %d", len(self.owners), len(owners))
	}
	for index, owner := range owners {
		if self.owners[index] != owner {
			t.Fatalf("publication attempt %d retained %d PG owners; want %d", index, self.owners[index], owner)
		}
	}
}

func installRedisPublicationHook(t testing.TB, ctx context.Context, hook *redisPublicationReplyHook) {
	t.Helper()
	if err := server.RedisWithDeadline(ctx, func(client server.RedisClient) error { client.AddHook(hook); return nil }); err != nil {
		t.Fatal(err)
	}
}

// All reads use an unobserved caller after publication joins. A before/after
// expiry score checks the per-token lease independently of shared-key TTLs.
func requireRedisPublicationCustody(t testing.TB, ctx context.Context, fixture netEscrowOrderingTestFixture, neighbor, own server.Id, total ByteCount, tokens int) float64 {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var contracts, escrows int
		var allocated ByteCount
		server.Raise(conn.QueryRow(ctx, `SELECT
		 (SELECT count(*) FROM transfer_contract WHERE contract_id IN ($1,$2) AND payer_network_id=$3),
		 (SELECT count(*) FROM transfer_escrow WHERE contract_id IN ($1,$2) AND balance_id=$4 AND redis_reserved),
		 (SELECT COALESCE(sum(balance_byte_count),0) FROM transfer_escrow WHERE contract_id IN ($1,$2) AND balance_id=$4)`,
			neighbor, own, fixture.sourceNetworkId, fixture.balanceId).Scan(&contracts, &escrows, &allocated))
		if contracts != 2 || escrows != 2 || allocated != 40 {
			t.Fatal("acknowledgement changed committed SQL custody")
		}
	})
	var expiry float64
	server.Redis(ctx, func(client server.RedisClient) {
		keys := redisContractReservationKeys(fixture.balanceId)
		values, err := client.HGetAll(ctx, keys[1]).Result()
		server.Raise(err)
		if len(values) != tokens || values[neighbor.String()] != "17" || values[own.String()] != "23" {
			t.Fatal("acknowledgement changed an exact live token")
		}
		expiry, err = client.ZScore(ctx, keys[2], own.String()).Result()
		server.Raise(err)
	})
	if Testing_NetEscrowByteCount(ctx, fixture.balanceId) != total {
		t.Fatal("acknowledgement changed reservation accounting")
	}
	redisRecoveryRequireMarker(t, ctx, fixture.balanceId, own, false)
	return expiry
}

// The original path preserves the same custody but reacquires PG for its own
// marker. That extra acquisition is asserted only after accounting and lease.
func TestRedisConfirmedPublicationAvoidsRetainedTransaction(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		observer, observed, closeObservation := observeRedisPublication(t, ctx)
		defer closeObservation()
		hook := &redisPublicationReplyHook{observer: observer, key: redisContractReservationKeys(fixture.balanceId)[0], started: make(chan server.Id, 1), release: make(chan struct{})}
		installRedisPublicationHook(t, ctx, hook)
		type result struct {
			escrow *TransferEscrow
			err    error
		}
		joined := make(chan result, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			var got result
			panicErr := redisPublicationHandleError(func() {
				got.escrow, got.err = CreateTransferEscrow(observed, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 23)
			})
			got.err = errors.Join(got.err, panicErr)
			joined <- got
		}()
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(hook.release) }) }
		defer func() {
			release()
			<-done
		}()
		var own server.Id
		select {
		case own = <-hook.started:
		case got := <-joined:
			t.Fatal("public creation did not reach publication", got.err)
		case <-ctx.Done():
			release()
			<-joined
			t.Fatal(ctx.Err())
		}
		var beforeExpiry float64
		server.Redis(ctx, func(client server.RedisClient) {
			var err error
			beforeExpiry, err = client.ZScore(ctx, redisContractReservationKeys(fixture.balanceId)[2], own.String()).Result()
			server.Raise(err)
		})
		release()
		got := <-joined
		if got.err != nil || got.escrow == nil || got.escrow.ContractId != own {
			t.Fatal("public creation failed", got.err)
		}
		if afterExpiry := requireRedisPublicationCustody(t, ctx, fixture, neighbor.ContractId, own, 40, 2); afterExpiry != beforeExpiry {
			t.Fatal("acknowledgement changed the token lease")
		}
		observer.stateLock.Lock()
		acquisitions := observer.acquisitions
		observer.stateLock.Unlock()
		if acquisitions != 1 {
			t.Fatalf("acknowledged request reacquired PostgreSQL for its own retained marker: got %d want 1", acquisitions)
		}
		observer.require(t, 1, 0)
		hook.require(t, 0)
	})
}

// A before-EVAL failure leaves the marker; an after-EVAL reply loss may have
// removed it. Both uncertain replies retain the unchanged SQL fallback owner.
func testRedisPublicationReplyBoundary(t *testing.T, mode string) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		observer, observed, closeObservation := observeRedisPublication(t, ctx)
		defer closeObservation()
		hook := &redisPublicationReplyHook{observer: observer, key: redisContractReservationKeys(fixture.balanceId)[0], mode: mode}
		installRedisPublicationHook(t, ctx, hook)
		escrow, err := CreateTransferEscrow(observed, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 23)
		if err != nil || escrow == nil {
			t.Fatal("optional acknowledgement changed a committed success", err)
		}
		requireRedisPublicationCustody(t, ctx, fixture, neighbor.ContractId, escrow.ContractId, 40, 2)
		observer.require(t, 2, 1)
		hook.require(t, 0, 1)
	})
}

func TestRedisConfirmedPublicationFailureFallsBackToCustody(t *testing.T) {
	testRedisPublicationReplyBoundary(t, "before")
}

func TestRedisConfirmedPublicationLostReplyKeepsSafeFallback(t *testing.T) {
	testRedisPublicationReplyBoundary(t, "after")
}

// The same real successful write has an uncertain response at its transaction
// owner. Its marker still requires SQL proof, even though the row committed.
func TestRedisConfirmedPublicationPreservesLostCommitReply(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		observer, observed, closeObservation := observeRedisPublication(t, ctx)
		defer closeObservation()
		observer.loseCommit = true
		hook := &redisPublicationReplyHook{observer: observer, key: redisContractReservationKeys(fixture.balanceId)[0]}
		installRedisPublicationHook(t, ctx, hook)
		owned := withRedisContractAdmission(observed)
		owner := redisAdmissionFromContext(owned)
		var escrow *TransferEscrow
		var returnErr error
		panicErr := redisPublicationHandleError(func() {
			escrow, returnErr = CreateTransferEscrow(owned, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 23)
		})
		if !errors.Is(panicErr, errRedisPublicationLostCommitReply) || returnErr != nil || escrow != nil || !observer.lostCommit {
			t.Fatal("fixture did not withhold the actual successful commit reply", panicErr, returnErr)
		}
		requireRedisPublicationCustody(t, ctx, fixture, neighbor.ContractId, owner.contractId, 40, 2)
		observer.require(t, 2, 1)
		hook.require(t, 1)
	})
}

// PublicationStarted protects uncertain writes, but is never permission to
// acknowledge a rolled-back write without the existing absence proof.
func TestRedisConfirmedPublicationPreservesRollbackRecovery(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		observer, observed, closeObservation := observeRedisPublication(t, ctx)
		defer closeObservation()
		hook := &redisPublicationReplyHook{observer: observer, key: redisContractReservationKeys(fixture.balanceId)[0]}
		installRedisPublicationHook(t, ctx, hook)
		owned := withRedisContractAdmission(observed)
		owner := redisAdmissionFromContext(owned)
		failure := errors.New("synthetic publication rollback")
		panicErr := redisPublicationHandleError(func() {
			_, _ = runRedisContractAdmission(owned, func(attempt context.Context) (*TransferEscrow, error) {
				server.Tx(attempt, func(tx server.PgTx) {
					created, _, err := createTransferEscrowInTx(attempt, tx, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, fixture.sourceNetworkId, 23, nil)
					server.Raise(err)
					if created == nil {
						panic(errors.New("rollback fixture did not write its escrow"))
					}
					panic(failure)
				}, server.TxReadCommitted, server.OptNoRetry())
				return nil, nil
			})
		})
		if !errors.Is(panicErr, failure) {
			t.Fatal("rollback lost its owning failure", panicErr)
		}
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, owner.contractId, false)
		if Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 17 {
			t.Fatal("rollback recovery changed the live neighbor")
		}
		requireRedisRefusalOnlyContract(t, ctx, fixture, neighbor.ContractId)
		observer.require(t, 2, 1)
		hook.require(t)
	})
}

// A successful owner may elide only its own proof. Another retained marker is
// still discovered, fenced and released through SQL in the same bounded page.
func TestRedisConfirmedPublicationStillRecoversUnrelatedMarker(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, fixture, 7)
		observer, observed, closeObservation := observeRedisPublication(t, ctx)
		defer closeObservation()
		hook := &redisPublicationReplyHook{observer: observer, key: redisContractReservationKeys(fixture.balanceId)[0]}
		installRedisPublicationHook(t, ctx, hook)
		escrow, err := CreateTransferEscrow(observed, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 23)
		if err != nil || escrow == nil {
			t.Fatal("public creation with retained peer failed", err)
		}
		requireRedisPublicationCustody(t, ctx, fixture, neighbor.ContractId, escrow.ContractId, 40, 2)
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, abandoned, false)
		observer.require(t, 2, 1)
		hook.require(t, 0)
	})
}

// Filtering one own candidate must not admit a 33rd candidate, even when
// acknowledged publication leaves room in the pending SQL slice.
func TestRedisConfirmedPublicationPreservesCollectedPageBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		peers := make([]server.Id, redisReservationRecoveryBatch)
		for index := range peers {
			peers[index] = server.NewId()
			amount, err := redisContractReservation(ctx, "reserve-owned", fixture.balanceId, peers[index], 1000, 1, redisContractReservationLease)
			if err != nil || amount != 1 {
				t.Fatal("bounded page peer fixture failed", err)
			}
		}
		observer, observed, closeObservation := observeRedisPublication(t, ctx)
		defer closeObservation()
		completedBefore := testutil.ToFloat64(redisContractReservationResults.WithLabelValues("recovery", "completed"))
		escrow, err := runRedisContractAdmission(observed, func(owned context.Context) (*TransferEscrow, error) {
			created, err := createTransferEscrow(owned, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 23)
			if err != nil || created == nil {
				return created, err
			}
			// The real creator has joined its confirmed transaction here. Set
			// marker rotation order, leaving token amounts and leases untouched.
			server.Redis(ctx, func(client server.RedisClient) {
				ordered := []redis.Z{{Score: -1, Member: created.ContractId.String()}}
				for index, peer := range peers {
					ordered = append(ordered, redis.Z{Score: float64(index), Member: peer.String()})
				}
				server.Raise(client.ZAdd(ctx, redisContractReservationKeys(fixture.balanceId)[4], ordered...).Err())
			})
			return created, nil
		})
		if err != nil || escrow == nil {
			t.Fatal("bounded page creation failed", err)
		}
		requireRedisPublicationCustody(t, ctx, fixture, neighbor.ContractId, escrow.ContractId, 41, 3)
		for index, peer := range peers {
			redisRecoveryRequireMarker(t, ctx, fixture.balanceId, peer, index == len(peers)-1)
		}
		if completed := testutil.ToFloat64(redisContractReservationResults.WithLabelValues("recovery", "completed")) - completedBefore; completed != redisReservationRecoveryBatch {
			t.Fatal("acknowledgement changed the collected page outcome count", completed)
		}
		observer.require(t, 2, redisReservationRecoveryBatch-1)
	})
}

// An earlier insufficient-credit recovery already consumed this operation's
// optional page. The acknowledged second action must not add another page.
func TestRedisConfirmedPublicationPreservesInsufficientRetryPageCount(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, fixture, 983)
		observer, observed, closeObservation := observeRedisPublication(t, ctx)
		defer closeObservation()
		hook := &redisPublicationReplyHook{observer: observer, key: redisContractReservationKeys(fixture.balanceId)[0]}
		installRedisPublicationHook(t, ctx, hook)
		escrow, err := CreateTransferEscrow(observed, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 23)
		if err != nil || escrow == nil {
			t.Fatal("known insufficient credit did not recover and retry", err)
		}
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, abandoned, false)
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, escrow.ContractId, true)
		observer.require(t, 3, 1)
		hook.require(t)
		if err := recoverRedisReservationRequest(ctx, fixture.balanceId, escrow.ContractId); err != nil {
			t.Fatal("retained retry marker lost ordinary recovery", err)
		}
		requireRedisPublicationCustody(t, ctx, fixture, neighbor.ContractId, escrow.ContractId, 40, 2)
	})
}

// A confirmed result from another identity cannot authorize this request's
// token, even if it happens to contain the same returned balance allocation.
func TestRedisConfirmedPublicationForeignResultKeepsSqlProof(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		observer, observed, closeObservation := observeRedisPublication(t, ctx)
		defer closeObservation()
		hook := &redisPublicationReplyHook{observer: observer, key: redisContractReservationKeys(fixture.balanceId)[0]}
		installRedisPublicationHook(t, ctx, hook)
		var own *TransferEscrow
		returned, err := runRedisContractAdmission(observed, func(attempt context.Context) (*TransferEscrow, error) {
			var err error
			own, err = createTransferEscrow(attempt, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 23)
			if err != nil || own == nil {
				return own, err
			}
			foreign := *own
			foreign.ContractId = neighbor.ContractId
			return &foreign, nil
		})
		if err != nil || own == nil || returned == nil || returned.ContractId != neighbor.ContractId {
			t.Fatal("foreign result boundary did not execute", err)
		}
		requireRedisPublicationCustody(t, ctx, fixture, neighbor.ContractId, own.ContractId, 40, 2)
		observer.require(t, 2, 1)
		hook.require(t, 1)
	})
}

func TestRedisConfirmedPublicationZeroEscrowAddsNoAcknowledgement(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		observer, observed, closeObservation := observeRedisPublication(t, ctx)
		defer closeObservation()
		hook := &redisPublicationReplyHook{observer: observer, key: redisContractReservationKeys(fixture.balanceId)[0]}
		installRedisPublicationHook(t, ctx, hook)
		escrow, err := CreateTransferEscrow(observed, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 0)
		if err != nil || escrow == nil {
			t.Fatal("zero escrow was not created", err)
		}
		// Zero-byte contracts keep their earliest-grant anchor without a Redis
		// reservation. Verify both the returned allocation and committed rows.
		for _, value := range []*TransferEscrow{escrow, GetTransferEscrow(ctx, escrow.ContractId)} {
			if value == nil || value.TransferByteCount != 0 || len(value.Balances) != 1 || value.Balances[0] == nil || value.Balances[0].BalanceId != fixture.balanceId || value.Balances[0].BalanceByteCount != 0 {
				t.Fatal("zero escrow lost its exact zero-byte grant anchor")
			}
		}
		server.Redis(ctx, func(client server.RedisClient) {
			values, err := client.HGetAll(ctx, redisContractReservationKeys(fixture.balanceId)[1]).Result()
			server.Raise(err)
			if len(values) != 1 || values[neighbor.ContractId.String()] != "17" {
				t.Fatal("zero escrow added a token or changed its neighbor")
			}
		})
		if Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 17 {
			t.Fatal("zero escrow changed the neighbor reservation")
		}
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, escrow.ContractId, false)
		observer.require(t, 1, 0)
		hook.require(t)
	})
}

// The commit reply remains successful even if the request is canceled at that
// boundary. Optional recovery keeps the marker for a later bounded owner.
func TestRedisConfirmedPublicationCanceledAfterCommitRetainsRecovery(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		observer, observed, closeObservation := observeRedisPublication(t, ctx)
		defer closeObservation()
		caller, stop := context.WithCancel(observed)
		defer stop()
		observer.cancelCommit = stop
		hook := &redisPublicationReplyHook{observer: observer, key: redisContractReservationKeys(fixture.balanceId)[0]}
		installRedisPublicationHook(t, ctx, hook)
		escrow, err := CreateTransferEscrow(caller, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 23)
		if err != nil || escrow == nil || caller.Err() != context.Canceled {
			t.Fatal("acknowledged commit lost its canceled success boundary", err)
		}
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, escrow.ContractId, true)
		observer.require(t, 1, 0)
		hook.require(t)
		if err := recoverRedisReservationRequest(ctx, fixture.balanceId, escrow.ContractId); err != nil {
			t.Fatal("later owner could not recover canceled publication", err)
		}
		requireRedisPublicationCustody(t, ctx, fixture, neighbor.ContractId, escrow.ContractId, 40, 2)
	})
}

// A terminal close can win after the real creator joins and before the
// optional page runs. The durable debit owner remains responsible for credit.
func testRedisPublicationPendingDebit(t *testing.T, consumed ByteCount, reduceToken bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		observer, observed, closeObservation := observeRedisPublication(t, ctx)
		defer closeObservation()
		hook := &redisPublicationReplyHook{observer: observer, key: redisContractReservationKeys(fixture.balanceId)[0]}
		installRedisPublicationHook(t, ctx, hook)
		var posts []func() any
		escrow, err := runRedisContractAdmission(observed, func(attempt context.Context) (*TransferEscrow, error) {
			created, err := createTransferEscrow(attempt, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 23)
			if err != nil || created == nil {
				return created, err
			}
			// This independent owner starts only after the creator's PG release.
			posts = asyncDebitTestSettle(ctx, created.ContractId, consumed)
			if reduceToken {
				// Explicit reconciliation is still a supported repair boundary.
				// Current settlement posts leave release to the durable worker.
				ReconcileRedisContractReservation(ctx, created.ContractId)
				server.RunPosts(ctx, posts...)
			}
			return created, nil
		})
		if err != nil || escrow == nil {
			t.Fatal("concurrent debit changed creation success", err)
		}
		credit, pending, applied := asyncDebitTestState(t, ctx, fixture.balanceId)
		if credit != 1000 || pending != 1 || applied != 0 {
			t.Fatal("acknowledgement consumed or discarded unapplied debit")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var amount ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT debit_byte_count FROM transfer_debit_journal WHERE contract_id=$1 AND balance_id=$2 AND NOT applied`, escrow.ContractId, fixture.balanceId).Scan(&amount))
			if amount != consumed {
				t.Fatal("publication changed the durable consumption amount")
			}
		})
		if reduceToken {
			redisRecoveryRequireMarker(t, ctx, fixture.balanceId, escrow.ContractId, true)
			if Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 17+consumed {
				t.Fatal("changed token lost its pending consumption hold")
			}
			observer.require(t, 2, 1)
			hook.require(t)
		} else {
			redisRecoveryRequireMarker(t, ctx, fixture.balanceId, escrow.ContractId, false)
			if Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 40 {
				t.Fatal("acknowledgement released unapplied consumption")
			}
			observer.require(t, 1, 0)
			hook.require(t, 0)
		}
		n, released, busy, flushErr := flushTransferDebitBalance(ctx, fixture.balanceId)
		if flushErr != nil || n != 1 || released != 1 || busy {
			t.Fatal("durable debit owner could not finish", flushErr)
		}
		server.RunPosts(ctx, posts...)
		credit, pending, applied = asyncDebitTestState(t, ctx, fixture.balanceId)
		if credit != 1000-consumed || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 17 {
			t.Fatal("debit completion or late post changed confirmed accounting")
		}
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, escrow.ContractId, false)
		server.Redis(ctx, func(client server.RedisClient) {
			values, err := client.HGetAll(ctx, redisContractReservationKeys(fixture.balanceId)[1]).Result()
			server.Raise(err)
			if len(values) != 1 || values[neighbor.ContractId.String()] != "17" {
				t.Fatal("debit completion resurrected a token or changed its neighbor")
			}
		})
	})
}

func TestRedisConfirmedPublicationKeepsUnchangedPendingDebit(t *testing.T) {
	testRedisPublicationPendingDebit(t, 11, false)
}

func TestRedisConfirmedPublicationChangedTokenKeepsSqlProof(t *testing.T) {
	testRedisPublicationPendingDebit(t, 11, true)
}

func TestRedisConfirmedPublicationPreservesZeroUseReleaseOwner(t *testing.T) {
	testRedisPublicationPendingDebit(t, 0, false)
}

// The companion creator uses the origin's payer but owns a distinct exact
// reservation. Its acknowledged result must have the same release boundary.
func TestRedisConfirmedCompanionPublicationAvoidsRetainedTransaction(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		origin := createRedisAdmissionTest(ctx, fixture, 17)
		observer, observed, closeObservation := observeRedisPublication(t, ctx)
		defer closeObservation()
		hook := &redisPublicationReplyHook{observer: observer, key: redisContractReservationKeys(fixture.balanceId)[0]}
		installRedisPublicationHook(t, ctx, hook)
		escrow, err := CreateCompanionTransferEscrow(observed, fixture.destinationNetworkId, fixture.destinationId, fixture.sourceNetworkId, fixture.sourceId, 23, time.Hour)
		if err != nil || escrow == nil || escrow.CompanionContractId == nil || *escrow.CompanionContractId != origin.ContractId {
			t.Fatal("companion publication lost its origin authority", err)
		}
		requireRedisPublicationCustody(t, ctx, fixture, origin.ContractId, escrow.ContractId, 40, 2)
		observer.require(t, 1, 0)
		hook.require(t, 0)
	})
}

// Pause the actual page after collection. An independently owned terminal
// debit changes or releases the token before acknowledgement resumes. The
// successful creator's stale allocation cannot release debt or restore a token.
func testRedisPublicationAfterCollection(t *testing.T, releaseDebit bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		observer, observed, closeObservation := observeRedisPublication(t, ctx)
		defer closeObservation()
		hook := &redisPublicationReplyHook{observer: observer, key: redisContractReservationKeys(fixture.balanceId)[0], started: make(chan server.Id, 1), release: make(chan struct{})}
		installRedisPublicationHook(t, ctx, hook)
		type result struct {
			escrow *TransferEscrow
			err    error
		}
		joined := make(chan result, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			var got result
			panicErr := redisPublicationHandleError(func() {
				got.escrow, got.err = CreateTransferEscrow(observed, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 23)
			})
			got.err = errors.Join(got.err, panicErr)
			joined <- got
		}()
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(hook.release) }) }
		defer func() {
			release()
			<-done
		}()
		var own server.Id
		select {
		case own = <-hook.started:
		case got := <-joined:
			t.Fatal("page did not reach publication", got.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, own, true)
		posts := asyncDebitTestSettle(ctx, own, 11)
		if releaseDebit {
			applied, released, busy, flushErr := flushTransferDebitBalance(ctx, fixture.balanceId)
			if flushErr != nil || applied != 1 || released != 1 || busy {
				t.Fatal("independent debit did not finish before acknowledgement", flushErr)
			}
		} else {
			ReconcileRedisContractReservation(ctx, own)
		}
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, own, !releaseDebit)
		release()
		got := <-joined
		if got.err != nil || got.escrow == nil || got.escrow.ContractId != own {
			t.Fatal("late acknowledgement changed committed creation", got.err)
		}
		if releaseDebit {
			observer.require(t, 1, 0)
		} else {
			redisRecoveryRequireMarker(t, ctx, fixture.balanceId, own, true)
			credit, pending, applied := asyncDebitTestState(t, ctx, fixture.balanceId)
			if credit != 1000 || pending != 1 || applied != 0 || Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 28 {
				t.Fatal("stale acknowledgement released a changed pending debit")
			}
			observer.require(t, 2, 1)
			applied, released, busy, flushErr := flushTransferDebitBalance(ctx, fixture.balanceId)
			if flushErr != nil || applied != 1 || released != 1 || busy {
				t.Fatal("changed pending debit lost its release owner", flushErr)
			}
		}
		server.RunPosts(ctx, posts...)
		credit, pending, completed := asyncDebitTestState(t, ctx, fixture.balanceId)
		if credit != 989 || pending+completed != 0 || Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 17 {
			t.Fatal("late acknowledgement changed completed consumption")
		}
		server.Redis(ctx, func(client server.RedisClient) {
			values, err := client.HGetAll(ctx, redisContractReservationKeys(fixture.balanceId)[1]).Result()
			server.Raise(err)
			if len(values) != 1 || values[neighbor.ContractId.String()] != "17" {
				t.Fatal("late acknowledgement resurrected a token or changed its neighbor")
			}
		})
		hook.require(t, 0)
	})
}

func TestRedisConfirmedPublicationAfterDebitReleaseCannotResurrect(t *testing.T) {
	testRedisPublicationAfterCollection(t, true)
}

func TestRedisConfirmedPublicationChangedAfterCollectionKeepsSqlProof(t *testing.T) {
	testRedisPublicationAfterCollection(t, false)
}
