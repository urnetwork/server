// The held-provider fixture observes the dispatched statement across transports.
package model

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// This seam verifies dispatch order only. The 512-contract native fixture still
// requires the exact PostgreSQL blocker edge before proving financial progress.
type providerContentionObserverTestTx struct {
	server.PgTx
	beforeDispatch func()
}

// Preserve the caller's dispatch while inspecting its preceding observation.
func (self *providerContentionObserverTestTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	self.beforeDispatch()
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

// No results are consumed in the observer-only dispatch control.
func (self *providerContentionObserverTestTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	self.beforeDispatch()
	return nil
}

// An Exec-only hook misses the actual pipelined provider submission and strands
// its entered wait until the whole fixture deadline, despite a real SQL failure.
func TestLegacyProviderTotalsContentionObserverCoversWriteTransports(t *testing.T) {
	const provider = "INSERT INTO account_balance(network_id) VALUES($1)"
	const other = "UPDATE pending_task SET claim_time=claim_time"
	for _, shape := range []struct {
		name       string
		statements []string
		batch      bool
		want       int
	}{
		{"exec_unrelated", []string{other}, false, 0},
		{"exec_provider", []string{provider}, false, 1},
		{"batch_unrelated", []string{other}, true, 0},
		{"batch_provider_first", []string{provider, other}, true, 1},
		{"batch_provider_later", []string{other, provider, provider}, true, 1},
	} {
		t.Run(shape.name, func(t *testing.T) {
			observed, dispatched := 0, 0
			underlying := &providerContentionObserverTestTx{beforeDispatch: func() {
				dispatched++
				if observed != shape.want {
					t.Fatalf("provider observation preceded no actual matching dispatch: got=%d want=%d", observed, shape.want)
				}
			}}
			wrapped := &providerTotalContentionTx{PgTx: underlying, beforeProvider: func() { observed++ }}
			// Repeated submissions must not close the one-shot signal twice.
			for range 2 {
				if shape.batch {
					batch := &pgx.Batch{}
					for _, statement := range shape.statements {
						batch.Queue(statement)
					}
					wrapped.SendBatch(t.Context(), batch)
				} else {
					_, err := wrapped.Exec(t.Context(), shape.statements[0])
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if observed != shape.want || dispatched != 2 {
				t.Fatal("observer changed dispatch or repeated its one-shot signal", observed, dispatched)
			}
		})
	}
}
