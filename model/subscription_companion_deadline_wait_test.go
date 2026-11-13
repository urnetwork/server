// Public creators must recheck shard time after acquiring their writer relations.
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
	"github.com/urnetwork/server"
)

type companionDeadlineQueryKey struct{}

// Stop the actual first BEGIN after autocommit discovery and reservations have
// joined. The tracer gates dispatch but leaves every statement and reply real.
// BEGIN's detached bound starts only after caller cancellation, so a live
// caller can hold this explicit setup barrier without expiring that bound.
type companionDeadlineWaitTrace struct {
	*redisPublicationObserver
	stateLock     sync.Mutex
	beginHeld     bool
	originReads   int
	deadlineReads int
	waitingStage  string
	arrived       chan companionDeadlineAcquire
	release       chan struct{}
}

type companionDeadlineAcquire struct {
	originReads int
	owners      int
	txStatus    byte
}

// The marked public request alone reaches the BEGIN barrier; setup uses its
// parent context. Retain statement text only as finite labels and counters.
func (self *companionDeadlineWaitTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx = self.redisPublicationObserver.TraceQueryStart(ctx, conn, data)
	if ctx.Value(self.redisPublicationObserver) != true {
		return ctx
	}
	sql := strings.Join(strings.Fields(data.SQL), " ")
	self.stateLock.Lock()
	hold := !self.beginHeld && strings.HasPrefix(strings.ToUpper(sql), "BEGIN")
	originReads := self.originReads
	if hold {
		self.beginHeld = true
	}
	switch {
	case strings.HasPrefix(sql, "SELECT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1 AND COALESCE(expiration_time"):
		self.waitingStage = "companion_expiry"
	case strings.HasPrefix(strings.ToUpper(sql), "LOCK TABLE"):
		self.waitingStage = "relation_ownership"
	}
	self.stateLock.Unlock()
	if hold {
		self.arrived <- companionDeadlineAcquire{originReads: originReads, owners: self.redisPublicationObserver.activeCount(), txStatus: conn.PgConn().TxStatus()}
		select {
		case <-self.release:
		case <-ctx.Done():
		}
	}
	return context.WithValue(ctx, companionDeadlineQueryKey{}, sql)
}

// An origin read counts only after its real reply, before the creation acquisition.
func (self *companionDeadlineWaitTrace) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	self.redisPublicationObserver.TraceQueryEnd(ctx, conn, data)
	if ctx.Value(self.redisPublicationObserver) != true || data.Err != nil {
		return
	}
	sql, _ := ctx.Value(companionDeadlineQueryKey{}).(string)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.beginHeld && strings.Contains(sql, "transfer_contract") {
		self.originReads++
	}
	if sql == `SELECT clock_timestamp() AT TIME ZONE 'UTC' < $1::timestamp` {
		self.deadlineReads++
	}
}

// A plain zero-byte creation first reaches the contract relation in its real insert batch.
func (self *companionDeadlineWaitTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if ctx.Value(self.redisPublicationObserver) == true {
		for _, query := range data.Batch.QueuedQueries {
			if strings.Contains(query.SQL, "INSERT INTO transfer_contract") {
				self.stateLock.Lock()
				self.waitingStage = "insert_batch"
				self.stateLock.Unlock()
			}
		}
	}
	return ctx
}

// Count returned batch clocks too, so a positive Redis writer exposes the same boundary.
func (self *companionDeadlineWaitTrace) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if ctx.Value(self.redisPublicationObserver) != true || data.Err != nil {
		return
	}
	sql := strings.Join(strings.Fields(data.SQL), " ")
	if sql == `SELECT clock_timestamp() AT TIME ZONE 'UTC'` || strings.HasPrefix(sql, `SELECT count(*)=$2,clock_timestamp() AT TIME ZONE 'UTC'`) {
		self.stateLock.Lock()
		self.deadlineReads++
		self.stateLock.Unlock()
	}
}

// The transaction owner joins the batch normally.
func (self *companionDeadlineWaitTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}

// Run one public creator with a held relation acquired only after its origin
// selection, if any. The deadline update commits before its registry lock, and
// pg_locks plus the primary clock prove both sides of the late-wait boundary.
func exercisePublicContractRelationDeadline(t testing.TB, companion, zeroEscrow bool, requested ByteCount, relation, heldMode string, rawLegacy bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ctx = WithProviderWorkSessionSource(ctx, nil)
	shard := shardTestOwner(t, ctx, shardTestKey(0))
	peer := newEscrowSelectionTestClients(t, ctx)
	if rawLegacy {
		predecessor, err := createTransferEscrow(ctx, shard.NetworkId, shard.ClientId, peer.providerNetworkId, peer.providerId, 17)
		if err != nil || predecessor == nil || predecessor.TransferByteCount != 17 {
			t.Fatal("legacy cache predecessor failed", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var warm bool
			server.Raise(conn.QueryRow(ctx, `SELECT snapshot.reserved_byte_count=17 AND snapshot.revision=revision.revision FROM transfer_balance_net_escrow_snapshot snapshot JOIN transfer_balance_net_escrow_revision revision USING(balance_id) WHERE snapshot.balance_id=$1`, shard.BalanceId).Scan(&warm))
			if !warm {
				t.Fatal("legacy cache was not exact before held relation")
			}
		})
	}
	if companion {
		origin, err := CreateTransferEscrow(ctx, shard.NetworkId, shard.ClientId, peer.providerNetworkId, peer.providerId, 0)
		if err != nil || origin == nil || origin.TransferByteCount != 0 {
			t.Fatal("zero-byte reverse origin failed", err)
		}
	}
	type financialState struct{ contracts, escrows, journals, revision int64 }
	readState := func() financialState {
		state := financialState{}
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_contract WHERE source_network_id=$1 OR destination_network_id=$1),
				(SELECT count(*) FROM transfer_escrow WHERE balance_id=$2),
				(SELECT count(*) FROM transfer_debit_journal WHERE balance_id=$2),
				COALESCE((SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$2),0)`,
				shard.NetworkId, shard.BalanceId).Scan(&state.contracts, &state.escrows, &state.journals, &state.revision))
		})
		return state
	}
	before := readState()
	beforeReserved := Testing_NetEscrowByteCount(ctx, shard.BalanceId)
	beforeCounters := readContractLifecycleCounterTestSnapshot(t)
	zeroKind := zeroEscrowPlainContractKind(shard.NetworkId, peer.providerNetworkId)
	if companion {
		zeroKind = "companion"
	}
	beforeZeroCreated := testutil.ToFloat64(zeroEscrowContractsCreated.WithLabelValues(zeroKind))
	originReads := 0
	if companion && !zeroEscrow {
		originReads = 2
	}
	trace := &companionDeadlineWaitTrace{redisPublicationObserver: &redisPublicationObserver{}, arrived: make(chan companionDeadlineAcquire, 1), release: make(chan struct{})}
	scope, err := server.NewTestPgQueryScope(ctx, trace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}()
	observed, cancelCreation := context.WithCancel(context.WithValue(ctx, trace.redisPublicationObserver, true))
	type creationResult struct {
		escrow *TransferEscrow
		err    error
	}
	create := func(caller context.Context) creationResult {
		result := creationResult{}
		panicErr := redisPublicationHandleError(func() {
			switch {
			case rawLegacy:
				result.escrow, result.err = createTransferEscrow(caller, shard.NetworkId, shard.ClientId, peer.providerNetworkId, peer.providerId, requested)
			case companion && zeroEscrow:
				result.escrow, result.err = CreateZeroEscrowCompanionContract(caller, peer.providerNetworkId, peer.providerId, shard.NetworkId, shard.ClientId, requested, time.Hour)
			case zeroEscrow:
				result.escrow, result.err = CreateZeroEscrowContract(caller, shard.NetworkId, shard.ClientId, peer.providerNetworkId, peer.providerId, requested)
			case companion:
				result.escrow, result.err = CreateCompanionTransferEscrow(caller, peer.providerNetworkId, peer.providerId, shard.NetworkId, shard.ClientId, requested, time.Hour)
			default:
				result.escrow, result.err = CreateTransferEscrow(caller, shard.NetworkId, shard.ClientId, peer.providerNetworkId, peer.providerId, requested)
			}
		})
		result.err = errors.Join(result.err, panicErr)
		return result
	}
	results, done := make(chan creationResult, 1), make(chan struct{})
	go func() {
		defer close(done)
		results <- create(observed)
	}()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(trace.release) }) }
	defer func() { cancelCreation(); release(); <-done }()
	select {
	case arrived := <-trace.arrived:
		if arrived.originReads != originReads || arrived.owners != 1 || arrived.txStatus != 'I' {
			t.Fatalf("creation did not follow joined autocommit origins: reads=%d owners=%d transaction=%q", arrived.originReads, arrived.owners, arrived.txStatus)
		}
	case result := <-results:
		t.Fatal("public creator missed its pre-BEGIN boundary", result.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	reservedRequest := requested
	if zeroEscrow || rawLegacy {
		reservedRequest = 0
	}
	if reserved := Testing_NetEscrowByteCount(ctx, shard.BalanceId); reserved != beforeReserved+reservedRequest {
		t.Fatalf("transaction barrier did not follow its exact reservation: got=%d before=%d requested=%d zero_escrow=%t", reserved, beforeReserved, requested, zeroEscrow)
	}
	// The held transaction has its own goroutine owner. The separate deadline
	// setup commits while creation is still outside BEGIN and registry locks.
	held, finish := beginRareGrantTestTx(t, ctx)
	defer finish(false)
	server.RaisePgResult(held.Exec(ctx, "LOCK TABLE "+relation+" IN "+heldMode+" MODE"))
	blockerPid := contractLifecycleTestBackendPid(t, ctx, held)
	var deadline time.Time
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(tx.QueryRow(ctx, `UPDATE prober_shard_run SET deadline=(clock_timestamp() AT TIME ZONE 'UTC')+make_interval(secs=>$2) WHERE network_id=$1 RETURNING deadline`, shard.NetworkId, proberShardDeadlineBarrierWindow.Seconds()).Scan(&deadline))
	}, server.TxReadCommitted, server.OptNoRetry())
	release()
	blockedPid := requireContractLifecycleBlockedBy(t, ctx, held, blockerPid)
	var relationWait, beforeDeadline bool
	var lockMode string
	server.Raise(held.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND relation=$3::regclass AND NOT granted),
		(clock_timestamp() AT TIME ZONE 'UTC')<$2::timestamp,
		COALESCE((SELECT mode FROM pg_locks WHERE pid=$1 AND relation=$3::regclass AND NOT granted LIMIT 1),'')`,
		blockedPid, deadline, relation).Scan(&relationWait, &beforeDeadline, &lockMode))
	if !relationWait || !beforeDeadline {
		t.Fatal("contract relation wait was not established before shard deadline", relationWait, beforeDeadline)
	}
	trace.stateLock.Lock()
	stage, clocks := trace.waitingStage, trace.deadlineReads
	trace.stateLock.Unlock()
	if rawLegacy && (stage != "insert_batch" || clocks != 2 || lockMode != "RowExclusiveLock") {
		t.Fatalf("legacy cache wait did not follow both shard clocks and financial writes: stage=%s clocks=%d lock=%s", stage, clocks, lockMode)
	}
	t.Logf("companion=%t zero_escrow=%t requested=%d relation=%s actual_relation_wait=%s stage=%s prior_shard_clock_reads=%d", companion, zeroEscrow, requested, relation, lockMode, stage, clocks)
	server.RaisePgResult(held.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamp-(clock_timestamp() AT TIME ZONE 'UTC')))))`, deadline))
	var expired bool
	server.Raise(held.QueryRow(ctx, `SELECT $1::timestamp<=clock_timestamp() AT TIME ZONE 'UTC'`, deadline).Scan(&expired))
	if !expired {
		t.Fatal("primary clock did not cross the held shard deadline")
	}
	server.Raise(finish(true))
	result := <-results
	<-done
	after := readState()
	afterReserved := Testing_NetEscrowByteCount(ctx, shard.BalanceId)
	if result.escrow != nil || !errors.Is(result.err, ErrProberShardRetired) || after != before || afterReserved != beforeReserved {
		t.Fatalf("public creation crossed shard deadline: companion=%t zero_escrow=%t requested=%d escrow=%v error=%v state=%+v/%+v reserved=%d/%d", companion, zeroEscrow, requested, result.escrow, result.err, before, after, beforeReserved, afterReserved)
	}
	requireContractLifecycleCounterTestDelta(t, beforeCounters, 0, 0)
	if got := testutil.ToFloat64(zeroEscrowContractsCreated.WithLabelValues(zeroKind)); got != beforeZeroCreated {
		t.Fatal("refused publication incremented the zero-escrow success counter", beforeZeroCreated, got)
	}
	if trace.redisPublicationObserver.activeCount() != 0 {
		t.Fatal("refused public creator retained its PostgreSQL owner")
	}
	// The failed child is finished. A fresh request uses the same parent,
	// endpoints and grant after explicit test-owned restoration of its lifetime.
	cancelCreation()
	if ctx.Err() != nil {
		t.Fatal("failed child retired its shared parent", ctx.Err())
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE prober_shard_run SET deadline=(clock_timestamp() AT TIME ZONE 'UTC')+interval '1 hour' WHERE network_id=$1`, shard.NetworkId))
	}, server.TxReadCommitted, server.OptNoRetry())
	healthy, stopHealthy := context.WithCancel(context.WithValue(ctx, trace.redisPublicationObserver, true))
	defer stopHealthy()
	successor := create(healthy)
	if successor.err != nil || successor.escrow == nil || successor.escrow.TransferByteCount != requested || successor.escrow.ExpirationTime.IsZero() {
		t.Fatal("healthy successor failed after the refused owner joined", successor.escrow, successor.err)
	}
	want := before
	want.contracts++
	if !zeroEscrow {
		want.escrows++
	}
	if rawLegacy {
		want.revision += 2
	}
	if got := readState(); got != want {
		t.Fatal("healthy successor changed unrelated financial state", want, got)
	}
	successfulReservation := requested
	if zeroEscrow {
		successfulReservation = 0
	}
	if got := Testing_NetEscrowByteCount(ctx, shard.BalanceId); got != beforeReserved+successfulReservation {
		t.Fatal("healthy successor did not own its exact reservation", got, beforeReserved+successfulReservation)
	}
	var persisted bool
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT transfer_byte_count=$2 AND expiration_time=$3 FROM transfer_contract WHERE contract_id=$1`,
			successor.escrow.ContractId, requested, successor.escrow.ExpirationTime).Scan(&persisted))
	})
	if !persisted {
		t.Fatal("healthy successor changed its immutable returned size or deadline")
	}
	requireContractLifecycleCounterTestDelta(t, beforeCounters, 1, 0)
	wantZeroCreated := beforeZeroCreated
	if zeroEscrow {
		wantZeroCreated++
	}
	if got := testutil.ToFloat64(zeroEscrowContractsCreated.WithLabelValues(zeroKind)); got != wantZeroCreated {
		t.Fatal("healthy successor did not count exactly its zero-escrow success", got, wantZeroCreated)
	}
	if trace.redisPublicationObserver.activeCount() != 0 {
		t.Fatal("healthy successor retained its PostgreSQL owner")
	}
}

// A companion's autocommit origin read cannot cover its later transaction's relation wait.
func TestPublicZeroCompanionRechecksShardDeadlineAfterContractRelationWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		exercisePublicContractRelationDeadline(t, true, false, 0, "transfer_contract", "ACCESS EXCLUSIVE", false)
	})
}

// A plain zero-byte creator may first need the relation at its financial insert batch.
func TestPublicZeroContractRechecksShardDeadlineAfterContractRelationWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		exercisePublicContractRelationDeadline(t, false, false, 0, "transfer_contract", "ACCESS EXCLUSIVE", false)
	})
}

// Grant and lifecycle reads can proceed under SHARE while the financial insert
// still needs ROW EXCLUSIVE; expiry must refuse and compensate the pending token.
func TestPublicRedisContractRechecksShardDeadlineAfterEscrowRelationWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		exercisePublicContractRelationDeadline(t, false, false, 23, "transfer_escrow", "SHARE", false)
	})
}

// Extender membership is an unconditional writer in the same publication batch.
func TestPublicRedisContractRechecksShardDeadlineAfterExtenderRelationWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		exercisePublicContractRelationDeadline(t, false, false, 23, "contract_extender", "SHARE", false)
	})
}

// A free fallback still obeys the shard's lifetime after its actual insert waits.
func TestPublicZeroEscrowRechecksShardDeadlineAfterContractRelationWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		exercisePublicContractRelationDeadline(t, false, true, 23, "transfer_contract", "SHARE", false)
	})
}

// The origin's AccessShare lock allows a concurrent SHARE owner that blocks the
// companion's later RowExclusive insert, even within the same transaction.
func TestPublicZeroEscrowCompanionRechecksShardDeadlineAfterContractRelationWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		exercisePublicContractRelationDeadline(t, true, true, 23, "transfer_contract", "SHARE", false)
	})
}

// The already-current cache permits the early read under SHARE. Only the final
// required publication write needs RowExclusive after the financial inserts.
func TestLegacyPositiveRechecksShardDeadlineAfterFinalCacheRelationWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		exercisePublicContractRelationDeadline(t, false, false, 23, "transfer_balance_net_escrow_snapshot", "SHARE", true)
	})
}
