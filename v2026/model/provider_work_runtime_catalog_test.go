// Observe the real connection, reservation, stream and terminal owners after
// migrations. Signed custody checks accompany the absence of catalog probes.
package model

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// Captures statement templates without arguments. pg_locks remains permitted:
// current ownership evidence is part of signing, not a schema readiness check.
type providerWorkRuntimeQueryObserver struct {
	stateLock         sync.Mutex
	statements        []string
	catalogStatements []string
}

// Record attempts, including statements whose required objects are absent.
func (self *providerWorkRuntimeQueryObserver) record(sql string) {
	sql = strings.Join(strings.Fields(strings.ToLower(sql)), " ")
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.statements = append(self.statements, sql)
	for _, catalog := range []string{
		"to_regclass", "to_regprocedure", "pg_attribute", "pg_class", "pg_index",
		"pg_indexes", "pg_trigger", "pg_proc", "pg_constraint", "information_schema",
	} {
		if strings.Contains(sql, catalog) {
			self.catalogStatements = append(self.catalogStatements, sql)
			break
		}
	}
}

// Query, QueryRow and Exec all cross the same driver observation boundary.
func (self *providerWorkRuntimeQueryObserver) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.record(data.SQL)
	return ctx
}

// Owner assertions verify committed results; this tripwire counts attempts.
func (self *providerWorkRuntimeQueryObserver) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Batches cannot bypass the tripwire, even when preparation fails first.
func (self *providerWorkRuntimeQueryObserver) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		self.record(query.SQL)
	}
	return ctx
}

// Each batch statement was already recorded before its execution.
func (self *providerWorkRuntimeQueryObserver) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}

// Committed custody is checked outside the driver's callback.
func (self *providerWorkRuntimeQueryObserver) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}

// Positive statement controls make an uninstalled or unused observer fail.
// The caller joins its owners and closes the scope before checking this result.
func (self *providerWorkRuntimeQueryObserver) requireNoCatalog(t testing.TB, required ...string) {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.catalogStatements) != 0 {
		t.Fatalf("ordinary provider-work owners queried schema catalogs: %v", self.catalogStatements)
	}
	for _, fragment := range required {
		found := false
		for _, statement := range self.statements {
			if strings.Contains(statement, fragment) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("provider-work query observer missed required owner work: %s", fragment)
		}
	}
}

// Both application and maintenance pools belong to this disposable fixture.
// Defer the returned close inside the TestEnv callback, before pool restoration.
func providerWorkObserveRuntimeQueries(t testing.TB, ctx context.Context) (*providerWorkRuntimeQueryObserver, func()) {
	t.Helper()
	observer := &providerWorkRuntimeQueryObserver{}
	scope, err := server.NewTestPgQueryScope(ctx, observer)
	if err != nil {
		t.Fatal(err)
	}
	return observer, func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}
}

// An absent signer still takes the real endpoint fence and journals admission
// and retirement. It neither probes schema nor invents signed provenance.
func TestProviderWorkUnsignedConnectionSkipsRuntimeCatalog(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx := WithProviderWorkSessionSource(f.ctx, nil)
		observer, closeObservation := providerWorkObserveRuntimeQueries(t, ctx)
		defer closeObservation()
		connectionId, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, f.intermediaryId, "192.0.2.17:10008", f.handlerId, 4)
		server.Raise(err)
		server.Raise(DisconnectNetworkClient(ctx, connectionId))
		closeObservation()
		observer.requireNoCatalog(t, "pg_advisory_xact_lock", "insert into network_client_connection", "update network_client_connection")
		server.Db(ctx, func(conn server.PgConn) {
			var sequence, events, receipts, active int
			server.Raise(conn.QueryRow(ctx, `SELECT sequence,
 (SELECT count(*) FROM provider_work_session_event WHERE client_id=$1),
 (SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$1),
 (SELECT count(*) FROM network_client_connection WHERE client_id=$1 AND connected)
 FROM provider_work_session_head WHERE client_id=$1`, f.intermediaryId).Scan(&sequence, &events, &receipts, &active))
			if sequence != 2 || events != 2 || receipts != 0 || active != 0 {
				t.Fatal("unsigned connection changed durable history or minted originals", sequence, events, receipts, active)
			}
		})
	})
}
