// Claim statements run on the collector's guard session, which live executions
// also finalize on. If a claim context expired during a statement, pgx would
// close that session and every live finalization on it would fail. A claim
// with a deadline instead keeps a server-side statement limit that ends each
// of its statements before the deadline, so a slow claim fails as an ordinary
// statement timeout and the session stays usable.
package task

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Budget left unused when the limit is set.
const claimStatementMargin = 2 * time.Second

// A statement starting within this long of the last limit still ends at least
// claimStatementMargin-claimStatementSlack before the claim deadline. Later
// statements set a new limit first.
const claimStatementSlack = time.Second

// The claim deadline leaves no room for another statement.
var errClaimStatementBudget = errors.New("task claim statement budget exhausted")

// Sets the statement limit before each statement of the claim transaction.
type claimBudgetTx struct {
	pgx.Tx
	deadline time.Time
	limitAt  time.Time
}

// A claim without a deadline cannot be canceled by expiry, so it keeps the
// transaction unchanged.
func newClaimBudgetTx(ctx context.Context, tx pgx.Tx) pgx.Tx {
	deadline, ok := ctx.Deadline()
	if !ok {
		return tx
	}
	return &claimBudgetTx{Tx: tx, deadline: deadline}
}

// Lowers the limit to the remaining budget once the previous limit could reach
// the deadline.
func (self *claimBudgetTx) limit(ctx context.Context) error {
	now := time.Now()
	if !self.limitAt.IsZero() && now.Sub(self.limitAt) <= claimStatementSlack {
		return nil
	}
	remaining := self.deadline.Sub(now) - claimStatementMargin
	if remaining < time.Millisecond {
		return errClaimStatementBudget
	}
	if _, err := self.Tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, remaining.Milliseconds())); err != nil {
		return err
	}
	self.limitAt = now
	return nil
}

// Limits the statement before running it.
func (self *claimBudgetTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	if err := self.limit(ctx); err != nil {
		return pgconn.CommandTag{}, err
	}
	return self.Tx.Exec(ctx, sql, arguments...)
}

// Limits the query before running it.
func (self *claimBudgetTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := self.limit(ctx); err != nil {
		return nil, err
	}
	return self.Tx.Query(ctx, sql, args...)
}

// Limits the single-row query before running it.
func (self *claimBudgetTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := self.limit(ctx); err != nil {
		return claimBudgetRow{err: err}
	}
	return self.Tx.QueryRow(ctx, sql, args...)
}

// Limits the batch before sending it; each statement keeps that limit.
func (self *claimBudgetTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	if err := self.limit(ctx); err != nil {
		return claimBudgetBatch{err: err}
	}
	return self.Tx.SendBatch(ctx, batch)
}

// Limits the copy before running it.
func (self *claimBudgetTx) CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, source pgx.CopyFromSource) (int64, error) {
	if err := self.limit(ctx); err != nil {
		return 0, err
	}
	return self.Tx.CopyFrom(ctx, table, columns, source)
}

// Limits the commit before sending it.
func (self *claimBudgetTx) Commit(ctx context.Context) error {
	if err := self.limit(ctx); err != nil {
		return err
	}
	return self.Tx.Commit(ctx)
}

// A refused statement's row reports the budget error.
type claimBudgetRow struct{ err error }

// Returns the budget error.
func (self claimBudgetRow) Scan(...any) error {
	return self.err
}

// A refused batch reports the budget error for every result.
type claimBudgetBatch struct{ err error }

// Returns the budget error.
func (self claimBudgetBatch) Exec() (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, self.err
}

// Returns the budget error.
func (self claimBudgetBatch) Query() (pgx.Rows, error) {
	return nil, self.err
}

// Returns a row with the budget error.
func (self claimBudgetBatch) QueryRow() pgx.Row {
	return claimBudgetRow{err: self.err}
}

// Returns the budget error.
func (self claimBudgetBatch) Close() error {
	return self.err
}
