package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urnetwork/server/v2026"
)

type testingCensusCancelAfterBegin struct {
	parent   pgx.QueryTracer
	cancel   context.CancelFunc
	deadline time.Time
	fired    bool
}

func (self *testingCensusCancelAfterBegin) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if self.parent != nil {
		return self.parent.TraceQueryStart(ctx, conn, data)
	}
	return ctx
}

func (self *testingCensusCancelAfterBegin) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if self.parent != nil {
		self.parent.TraceQueryEnd(ctx, conn, data)
	}
	if data.Err == nil && data.CommandTag.String() == "BEGIN" && !self.fired {
		self.fired = true
		self.cancel()
		// The original deadline stays immutable. Cancellation wins first, then
		// the successful BEGIN response is held until its budget is exhausted.
		if delay := time.Until(self.deadline); delay > 0 {
			timer := time.NewTimer(delay)
			<-timer.C
		}
	}
}

// BEGIN has actually succeeded, but cancellation precedes cap calculation.
// Exhausting the remaining budget must not replace context.Canceled with
// DeadlineExceeded. This seam occurs before the target SQL is admitted.
func TestProviderUrlProbeFleetServerDeadlineCancelAfterBeginRetainsCause(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		server.Db(ctx, func(observer server.PgConn) {
			config, err := pgxpool.ParseConfig("")
			server.Raise(err)
			config.ConnConfig = observer.Conn().Config().Copy()
			config.MaxConns = 1
			config.MinConns = 0
			tracer := &testingCensusCancelAfterBegin{parent: config.ConnConfig.Tracer}
			config.ConnConfig.Tracer = tracer
			pool, err := pgxpool.NewWithConfig(ctx, config)
			server.Raise(err)
			defer pool.Close()
			conn, err := pool.Acquire(ctx)
			server.Raise(err)
			defer conn.Release()
			readCtx, readCancel := context.WithTimeout(ctx, time.Second)
			defer readCancel()
			tracer.cancel = readCancel
			tracer.deadline, _ = readCtx.Deadline()
			called := false
			failure := server.HandleError(func() {
				providerUrlProbeFleetRead(readCtx, conn, func(server.PgTx) { called = true })
			})
			err, ok := failure.(error)
			if !tracer.fired || called || !ok || !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("post-BEGIN cancellation lost caller cause or admitted target query: fired=%t called=%t failure=%v", tracer.fired, called, failure)
			}
			if !conn.Conn().IsClosed() && conn.Conn().PgConn().TxStatus() != 'I' {
				t.Fatal("post-BEGIN cancellation returned an unclean live transaction")
			}
		}, server.OptNoRetry())
	})
}
