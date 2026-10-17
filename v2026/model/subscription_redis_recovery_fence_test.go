package model

// Observe the real request and page entry points in a disposable PG/Redis
// fixture. Query counts cannot substitute for unchanged reservation custody.
import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// The observer retains only finite counts and connection identity, never SQL
// text or argument values. Its instance key excludes setup and other owners.
type redisRecoveryFenceObserver struct {
	stateLock      sync.Mutex
	fences         int
	custodyReads   int
	started        int
	completed      int
	queryErrors    int
	connectionPids map[uint32]bool
}

func (self *redisRecoveryFenceObserver) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if ctx.Value(self) != true {
		return ctx
	}
	sql := strings.Join(strings.Fields(data.SQL), " ")
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.started++
	if self.connectionPids == nil {
		self.connectionPids = map[uint32]bool{}
	}
	self.connectionPids[conn.PgConn().PID()] = true
	if sql == "SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))" {
		self.fences++
	} else if strings.Contains(sql, "JOIN transfer_escrow e USING(contract_id)") && strings.Contains(sql, "FROM transfer_debit_journal") && strings.Contains(sql, "NOT applied") {
		self.custodyReads++
	}
	return ctx
}

func (self *redisRecoveryFenceObserver) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(self) != true {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.completed++
	if data.Err != nil {
		self.queryErrors++
	}
}

// Reset only after all observed callers have returned and joined their queries.
func (self *redisRecoveryFenceObserver) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.fences, self.custodyReads, self.started, self.completed, self.queryErrors = 0, 0, 0, 0, 0
	self.connectionPids = nil
}

func (self *redisRecoveryFenceObserver) require(t testing.TB, fences, custodyReads int) {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.fences != fences || self.custodyReads != custodyReads {
		t.Fatalf("recovery query counts fences=%d custody=%d; want fences=%d custody=%d", self.fences, self.custodyReads, fences, custodyReads)
	}
	if self.started != self.completed || self.queryErrors != 0 || len(self.connectionPids) != 1 {
		t.Fatalf("recovery query ownership incomplete: starts=%d completions=%d errors=%d connections=%d", self.started, self.completed, self.queryErrors, len(self.connectionPids))
	}
}

func observeRedisRecoveryFence(t testing.TB, ctx context.Context) (*redisRecoveryFenceObserver, context.Context, func()) {
	t.Helper()
	observer := &redisRecoveryFenceObserver{}
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

// The old request path completes the same exact compensation but issues two
// fence queries. This assertion is the causal RED; the repair must issue one.
func TestRedisRecoveryRequestReusesTransactionFence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, fixture, 23)
		if Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 40 {
			t.Fatal("abandoned and live reservation fixture differs")
		}
		observer, observed, closeObservation := observeRedisRecoveryFence(t, ctx)
		defer closeObservation()
		if err := recoverRedisReservationRequest(observed, fixture.balanceId, abandoned); err != nil {
			t.Fatal("exact request recovery failed", err)
		}
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, abandoned, false)
		if Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 17 {
			t.Fatal("request compensation changed the live neighbor or retained abandoned debt")
		}
		requireRedisRefusalOnlyContract(t, ctx, fixture, neighbor.ContractId)
		observer.stateLock.Lock()
		fences := observer.fences
		observer.stateLock.Unlock()
		if fences != 1 {
			t.Fatalf("request recovery repeated its already-owned transaction fence: got %d want 1", fences)
		}
		observer.require(t, 1, 1)

		// A retry after a lost release acknowledgement sees no marker. It still
		// fences that exact request once and performs no SQL custody query.
		observer.reset()
		if err := recoverRedisReservationRequest(observed, fixture.balanceId, abandoned); err != nil {
			t.Fatal("idempotent request recovery failed", err)
		}
		observer.require(t, 1, 0)
		if Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 17 {
			t.Fatal("idempotent recovery changed the remaining reservation")
		}

		// A committed publication with a retained marker models a lost commit
		// acknowledgement. Recovery may forget its marker, but not its token.
		request := withRedisContractAdmission(ctx)
		var published *TransferEscrow
		var publicationErr error
		server.Tx(ctx, func(tx server.PgTx) {
			published, _, publicationErr = createTransferEscrowInTx(request, tx, fixture.sourceNetworkId, fixture.sourceId,
				fixture.destinationNetworkId, fixture.destinationId, fixture.sourceNetworkId, 23, nil)
		}, server.TxReadCommitted, server.OptNoRetry())
		if publicationErr != nil || published == nil {
			t.Fatal("committed publication fixture failed", publicationErr)
		}
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, published.ContractId, true)
		observer.reset()
		if err := recoverRedisReservationRequest(observed, fixture.balanceId, published.ContractId); err != nil {
			t.Fatal("committed custody recovery failed", err)
		}
		observer.require(t, 1, 1)
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, published.ContractId, false)
		if Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 40 {
			t.Fatal("lost commit acknowledgement released committed reservation")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var contract, escrow bool
			server.Raise(conn.QueryRow(ctx, `SELECT
			 EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1 AND payer_network_id=$2),
			 EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$3 AND redis_reserved AND balance_byte_count=23)`,
				published.ContractId, fixture.sourceNetworkId, fixture.balanceId).Scan(&contract, &escrow))
			if !contract || !escrow {
				t.Fatal("published recovery changed exact SQL custody")
			}
		})
		server.Redis(ctx, func(client server.RedisClient) {
			values, err := client.HGetAll(ctx, redisContractReservationKeys(fixture.balanceId)[1]).Result()
			server.Raise(err)
			if len(values) != 2 || values[neighbor.ContractId.String()] != "17" || values[published.ContractId.String()] != "23" {
				t.Fatal("published recovery changed either exact token")
			}
		})
	})
}

// The entry point used by retained pages has no pre-owned fence. It must keep
// its acquisition and recover exactly the same abandoned token under that owner.
func TestRedisRecoveryUnownedEntryAcquiresTransactionFence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, fixture, 23)
		observer, observed, closeObservation := observeRedisRecoveryFence(t, ctx)
		defer closeObservation()
		var released bool
		var recoveryErr error
		server.Tx(observed, func(tx server.PgTx) {
			released, recoveryErr = recoverRedisReservationInTx(observed, tx, fixture.balanceId, redisReservationRecoveryCandidate{contractId: abandoned, amount: 23})
		}, server.TxReadCommitted, server.OptNoRetry())
		if !released || recoveryErr != nil {
			t.Fatal("unowned recovery entry failed", recoveryErr)
		}
		observer.require(t, 1, 1)
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, abandoned, false)
		if Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 17 {
			t.Fatal("unowned recovery changed another reservation")
		}
		requireRedisRefusalOnlyContract(t, ctx, fixture, neighbor.ContractId)
	})
}

// Start an independent owner before invoking either contender. This holder
// never acquires another PG connection or calls a recovery entry point.
func holdRedisRecoveryFence(t testing.TB, ctx context.Context, contractId server.Id) (func(), func()) {
	t.Helper()
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var holderErr error
	go func() {
		defer close(done)
		panicErr := server.HandleError(func() {
			server.Db(ctx, func(conn server.PgConn) {
				tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				server.Raise(err)
				defer func() {
					cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
					defer stop()
					_ = tx.Rollback(cleanup)
				}()
				server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, redisContractAdmissionLock(contractId)))
				close(held)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		})
		if panicErr != nil {
			holderErr = fmt.Errorf("recovery fence holder: %v", panicErr)
		}
	}()
	releaseHolder := func() { releaseOnce.Do(func() { close(release) }) }
	joinHolder := func() {
		releaseHolder()
		select {
		case <-done:
			if holderErr != nil {
				t.Error(holderErr)
			}
		case <-ctx.Done():
			t.Error("recovery fence holder did not join", ctx.Err())
		}
	}
	select {
	case <-held:
	case <-done:
		t.Fatal("recovery fence holder did not acquire", holderErr)
	case <-ctx.Done():
		releaseHolder()
		<-done
		t.Fatal(ctx.Err())
	}
	return releaseHolder, joinHolder
}

func requireRedisRecoveryBusyCustody(t testing.TB, ctx context.Context, fixture netEscrowOrderingTestFixture, neighbor, abandoned server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var contracts, escrows int
		server.Raise(conn.QueryRow(ctx, `SELECT
		 (SELECT count(*) FROM transfer_contract WHERE payer_network_id=$1 AND contract_id<>$2),
		 (SELECT count(*) FROM transfer_escrow WHERE balance_id=$3 AND contract_id<>$2)`, fixture.sourceNetworkId, neighbor, fixture.balanceId).Scan(&contracts, &escrows))
		if contracts != 0 || escrows != 0 {
			t.Fatal("lock refusal published SQL custody")
		}
	})
	server.Redis(ctx, func(client server.RedisClient) {
		values, err := client.HGetAll(ctx, redisContractReservationKeys(fixture.balanceId)[1]).Result()
		server.Raise(err)
		if len(values) != 2 || values[neighbor.String()] != "17" || values[abandoned.String()] != "23" {
			t.Fatal("lock refusal changed either exact reservation token")
		}
	})
}

// Both entry points refuse a different live owner before reading SQL custody or
// releasing Redis authority. Joining that owner then permits ordinary recovery.
func TestRedisRecoveryFenceRefusalPreservesCustody(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, fixture, 23)
		observer, observed, closeObservation := observeRedisRecoveryFence(t, ctx)
		defer closeObservation()
		releaseHolder, joinHolder := holdRedisRecoveryFence(t, ctx, abandoned)
		defer joinHolder()
		for _, requestEntry := range []bool{false, true} {
			observer.reset()
			var released bool
			var recoveryErr error
			if requestEntry {
				recoveryErr = recoverRedisReservationRequest(observed, fixture.balanceId, abandoned)
			} else {
				server.Tx(observed, func(tx server.PgTx) {
					released, recoveryErr = recoverRedisReservationInTx(observed, tx, fixture.balanceId, redisReservationRecoveryCandidate{contractId: abandoned, amount: 23})
				}, server.TxReadCommitted, server.OptNoRetry())
			}
			if released || !errors.Is(recoveryErr, errRedisReservationRequestActive) {
				t.Fatal("live request fence did not refuse recovery", recoveryErr)
			}
			observer.require(t, 1, 0)
			redisRecoveryRequireMarker(t, ctx, fixture.balanceId, abandoned, true)
			if Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 40 {
				t.Fatal("lock refusal changed reserved amounts")
			}
			requireRedisRecoveryBusyCustody(t, ctx, fixture, neighbor.ContractId, abandoned)
		}
		releaseHolder()
		joinHolder()
		observer.reset()
		if err := recoverRedisReservationRequest(observed, fixture.balanceId, abandoned); err != nil {
			t.Fatal("joined writer still blocked exact request recovery", err)
		}
		observer.require(t, 1, 1)
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, abandoned, false)
		if Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 17 {
			t.Fatal("post-refusal recovery changed the live neighbor")
		}
		requireRedisRefusalOnlyContract(t, ctx, fixture, neighbor.ContractId)
	})
}
