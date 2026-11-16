// Early authority refusals must return without waiting for endpoint or grant checks.
package model

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Observe actual driver sends; a held endpoint owner supplies the independent
// lock boundary, so a queued check cannot masquerade as a prompt refusal.
type redisAdmissionRefusalTrace struct {
	*redisPublicationObserver
	stateLock       sync.Mutex
	endpointChecks  int
	grantChecks     int
	registryChecks  int
	endpointStarted chan struct{}
	startOnce       sync.Once
}

// Record only this public request's later admission checks, not grant discovery.
func (self *redisAdmissionRefusalTrace) observe(ctx context.Context, sql string) {
	if ctx.Value(self.redisPublicationObserver) != true {
		return
	}
	sql = strings.Join(strings.Fields(sql), " ")
	self.stateLock.Lock()
	endpoint := strings.Contains(sql, "contract_lifecycle_write_boundary")
	if endpoint {
		self.endpointChecks++
	}
	if strings.HasPrefix(sql, "SELECT count(*)=$2") && strings.Contains(sql, "FROM transfer_balance") {
		self.grantChecks++
	}
	if strings.Contains(sql, "FROM prober_shard_run") && strings.Contains(sql, "FOR SHARE") {
		self.registryChecks++
	}
	self.stateLock.Unlock()
	if endpoint {
		self.startOnce.Do(func() { close(self.endpointStarted) })
	}
}

// The sequential path is observed at its real query entry.
func (self *redisAdmissionRefusalTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx = self.redisPublicationObserver.TraceQueryStart(ctx, conn, data)
	self.observe(ctx, data.SQL)
	return ctx
}

// Sending a batch already commits its later checks to this connection owner.
func (self *redisAdmissionRefusalTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		self.observe(ctx, query.SQL)
	}
	return ctx
}

// Real results remain unchanged.
func (self *redisAdmissionRefusalTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}

// The operation owns and joins its ordinary batch.
func (self *redisAdmissionRefusalTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}

// A conclusive custody or registry refusal must finish while a distinct
// endpoint owner remains held. If a query is sent, prove its actual wait and
// release that owner before checking the unchanged financial state.
func exerciseRedisAdmissionEarlyRefusal(t testing.TB, published bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ctx = WithProviderWorkSessionSource(ctx, nil)
	shard := shardTestOwner(t, ctx, shardTestKey(0))
	peer := newEscrowSelectionTestClients(t, ctx)
	var prior *TransferEscrow
	wantReason, wantReserved := "outside its probe shard payer", ByteCount(0)
	if published {
		var err error
		prior, err = CreateTransferEscrow(ctx, shard.NetworkId, shard.ClientId, peer.providerNetworkId, peer.providerId, 23)
		if err != nil || prior == nil {
			t.Fatal("published custody fixture failed", err)
		}
		// A missing Redis generation permits this request's original id to
		// reserve again; its already committed SQL contract must still refuse.
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Del(ctx, redisContractReservationKeys(shard.BalanceId)...).Err())
		})
		wantReason = "already has SQL custody"
	} else {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE prober_shard_run SET state='draining' WHERE network_id=$1`, shard.NetworkId))
		}, server.TxReadCommitted, server.OptNoRetry())
	}
	type financialState struct{ contracts, escrows, amount, revision int64 }
	readState := func() financialState {
		state := financialState{}
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_contract WHERE payer_network_id=$1),
				(SELECT count(*) FROM transfer_escrow WHERE balance_id=$2),
				(SELECT COALESCE(sum(balance_byte_count),0) FROM transfer_escrow WHERE balance_id=$2),
				COALESCE((SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$2),0)`, shard.NetworkId, shard.BalanceId).Scan(&state.contracts, &state.escrows, &state.amount, &state.revision))
		})
		return state
	}
	before := readState()
	trace := &redisAdmissionRefusalTrace{redisPublicationObserver: &redisPublicationObserver{}, endpointStarted: make(chan struct{})}
	scope, err := server.NewTestPgQueryScope(ctx, trace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}()
	held, finish := beginRareGrantTestTx(t, ctx)
	defer finish(false)
	server.RaisePgResult(held.Exec(ctx, `SELECT client_id FROM network_client WHERE client_id=$1 FOR UPDATE`, shard.ClientId))
	blockerPid := contractLifecycleTestBackendPid(t, ctx, held)
	observed := context.WithValue(ctx, trace.redisPublicationObserver, true)
	if published {
		observed = withRedisContractAdmission(observed)
		redisAdmissionFromContext(observed).contractId = prior.ContractId
	}
	type result struct {
		escrow *TransferEscrow
		err    error
	}
	results, done := make(chan result, 1), make(chan struct{})
	go func() {
		defer close(done)
		got := result{}
		panicErr := redisPublicationHandleError(func() {
			got.escrow, got.err = CreateTransferEscrow(observed, shard.NetworkId, shard.ClientId, peer.providerNetworkId, peer.providerId, 23)
		})
		got.err = errors.Join(got.err, panicErr)
		results <- got
	}()
	defer func() { cancel(); _ = finish(false); <-done }()
	var got result
	select {
	case got = <-results:
	case <-trace.endpointStarted:
		requireContractLifecycleBlockedBy(t, ctx, held, blockerPid)
		server.Raise(finish(true))
		got = <-results
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	<-done
	if got.escrow != nil || got.err == nil || !strings.Contains(got.err.Error(), wantReason) {
		t.Fatal("early authority refusal changed its result", got.escrow, got.err)
	}
	if after := readState(); after != before || Testing_NetEscrowByteCount(ctx, shard.BalanceId) != wantReserved {
		t.Fatal("early refusal changed exact SQL or Redis custody", before, after)
	}
	trace.stateLock.Lock()
	endpoints, grants, registries := trace.endpointChecks, trace.grantChecks, trace.registryChecks
	trace.stateLock.Unlock()
	wantRegistries := 1
	if published {
		wantRegistries = 0
	}
	if endpoints != 0 || grants != 0 || registries != wantRegistries {
		t.Fatalf("conclusive refusal sent later admission work: endpoint_checks=%d grant_checks=%d registry_checks=%d; want0/0/%d", endpoints, grants, registries, wantRegistries)
	}
	if trace.redisPublicationObserver.activeCount() != 0 {
		t.Fatal("early refusal retained a PostgreSQL owner")
	}
}

// An invalid shard must not queue behind endpoint work that cannot authorize it.
func TestRedisAdmissionRetiredRegistrySkipsEndpointAndGrantWork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseRedisAdmissionEarlyRefusal(t, false) })
}

// Already committed custody refuses before taking even the shard or endpoint authority.
func TestRedisAdmissionPublishedCustodySkipsEndpointAndGrantWork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseRedisAdmissionEarlyRefusal(t, true) })
}
