package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type paymentReconcileConnectionKey struct{}
type paymentReconcileConnectionQuery struct{ acquire, release bool }
type paymentReconcileConnectionState struct {
	mutex                      sync.Mutex
	owner                      *pgx.Conn
	acquisitions, transactions int
	cancel                     context.CancelCauseFunc
}

// Refuse a second acquisition at its entry, before a singleton pool can wait
// on itself. Each run carries its own state, so concurrent webhook fixtures
// remain independent. This guard also covers the existing store/race tests.
type paymentReconcileConnectionTracer struct{ t testing.TB }

func (self *paymentReconcileConnectionTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if state, ok := ctx.Value(paymentReconcileConnectionKey{}).(*paymentReconcileConnectionState); ok {
		state.mutex.Lock()
		defer state.mutex.Unlock()
		state.acquisitions++
		if state.owner != nil {
			self.t.Error("payment reconciliation reacquired PostgreSQL while retaining its run lock")
			state.cancel(errors.New("synthetic nested payment reconciliation acquisition"))
		}
	}
	return ctx
}

func (*paymentReconcileConnectionTracer) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
}

func (self *paymentReconcileConnectionTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	state, ok := ctx.Value(paymentReconcileConnectionKey{}).(*paymentReconcileConnectionState)
	if !ok {
		return ctx
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()
	if state.owner != nil && state.owner != conn {
		self.t.Error("payment reconciliation changed its PostgreSQL session while retaining its run lock")
	}
	if state.owner != nil && strings.HasPrefix(strings.ToLower(data.SQL), "begin") {
		state.transactions++
	}
	if len(data.Args) == 1 && data.Args[0] == paymentReconcileRunLockKey {
		query := paymentReconcileConnectionQuery{
			acquire: strings.Contains(data.SQL, "pg_try_advisory_lock("),
			release: strings.Contains(data.SQL, "pg_advisory_unlock("),
		}
		return context.WithValue(ctx, paymentReconcileConnectionQuery{}, query)
	}
	return ctx
}

func (*paymentReconcileConnectionTracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	state, ok := ctx.Value(paymentReconcileConnectionKey{}).(*paymentReconcileConnectionState)
	if !ok || data.Err != nil {
		return
	}
	query, _ := ctx.Value(paymentReconcileConnectionQuery{}).(paymentReconcileConnectionQuery)
	state.mutex.Lock()
	defer state.mutex.Unlock()
	if query.acquire {
		state.owner = conn
	}
	if query.release {
		state.owner = nil
	}
}

func guardPaymentReconciliationConnections(t testing.TB) func() {
	t.Helper()
	scope, err := server.NewTestPgQueryScope(t.Context(), &paymentReconcileConnectionTracer{t: t})
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}
}

// Even a no-credential run writes per-store disposition and a heartbeat. The
// old run held the only connection and tried to acquire another for that first
// audit write. The tracer makes that failure immediate, without a timeout race.
func TestPaymentReconciliationUsesOneSessionWithSingletonPools(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		pop := server.Config.PushSimpleResource(server.DefaultPgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
		popMaintenance := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() { popMaintenance(); pop(); server.PgReset() }()
		defer disableAllReconcileStores(t)()
		owner := reconcileTestSession(t, t.Context())
		result, err := RunPaymentReconciliation(owner)
		if err != nil || result == nil || result.Errors != 0 || len(result.SkippedStores) != 5 {
			t.Fatal("single-session reconciliation failed", result, err)
		}
		state := owner.Ctx.Value(paymentReconcileConnectionKey{}).(*paymentReconcileConnectionState)
		state.mutex.Lock()
		acquisitions, transactions, retained := state.acquisitions, state.transactions, state.owner != nil
		state.mutex.Unlock()
		if acquisitions != 1 || transactions != 6 || retained {
			t.Fatal("run did not reuse and release its single session", acquisitions, transactions, retained)
		}
		if events := model.GetPaymentReconciliationEvents(t.Context(), result.RunId); len(events) != 6 {
			t.Fatal("run acknowledged missing audit commits", len(events))
		}
	})
}
