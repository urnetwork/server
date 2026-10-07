// Savepoint cleanup must not replace the canceled endpoint operation's cause.
package model

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Restrict the test seam to the savepoint protocol; any other call panics.
type providerWorkCanceledSavepointTx struct {
	server.PgTx
	rollbackErr error
	rolledBack  bool
}

// A successfully created optional savepoint precedes the canceled query.
func (self *providerWorkCanceledSavepointTx) Begin(context.Context) (server.PgTx, error) {
	return self, nil
}

// Match pgx's wrapped closed-connection cleanup after its watcher cancels I/O.
func (self *providerWorkCanceledSavepointTx) Rollback(context.Context) error {
	self.rolledBack = true
	return self.rollbackErr
}

// The real endpoint wait control covers transaction rollback and no journal;
// this forced cleanup outcome makes cause preservation independent of timing.
func TestProviderWorkOptionalSchemaCanceledCleanupPreservesCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queryErr := errors.New("endpoint query canceled")
	tx := &providerWorkCanceledSavepointTx{
		rollbackErr: fmt.Errorf("failed to deallocate cached statement(s): %w", pgconn.ErrConnClosed),
	}
	var raised error
	func() {
		defer func() { raised, _ = recover().(error) }()
		providerWorkOptionalSchemaInTx(ctx, tx, func(server.PgTx) error {
			cancel()
			return queryErr
		})
		t.Fatal("canceled optional operation returned to the transaction")
	}()
	if !tx.rolledBack || !errors.Is(raised, context.Canceled) ||
		!errors.Is(raised, queryErr) || !errors.Is(raised, pgconn.ErrConnClosed) {
		t.Fatal("cleanup replaced the operation or cancellation cause", raised)
	}
}
