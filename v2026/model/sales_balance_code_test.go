package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// Pure test: no database. A paid data code's creation runs what follows from
// the new purchase record (its sales notice, owner request 2026-10-28) in the
// creating transaction, and only when the code's row is new: a webhook retry
// that finds the code made does not.

// balanceCodeTableTx is transfer_balance_code in memory: the existence read by
// purchase event and the insert. Any other pgx.Tx method panics.
type balanceCodeTableTx struct {
	pgx.Tx
	purchaseEventIds map[string]bool
}

func (self *balanceCodeTableTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if !strings.Contains(sql, "INSERT INTO transfer_balance_code") {
		panic("unexpected statement: " + sql)
	}
	purchaseEventId := args[7].(string)
	if self.purchaseEventIds[purchaseEventId] {
		return pgconn.CommandTag{}, &pgconn.PgError{Code: "23505"}
	}
	self.purchaseEventIds[purchaseEventId] = true
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (self *balanceCodeTableTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if !strings.Contains(sql, "FROM transfer_balance_code") || !strings.Contains(sql, "purchase_event_id = $1") {
		panic("unexpected query: " + sql)
	}
	return &balanceCodeTableRows{found: self.purchaseEventIds[args[0].(string)]}, nil
}

type balanceCodeTableRows struct {
	pgx.Rows
	found bool
	read  bool
}

func (self *balanceCodeTableRows) Next() bool {
	if self.found && !self.read {
		self.read = true
		return true
	}
	return false
}

func (self *balanceCodeTableRows) Err() error { return nil }
func (self *balanceCodeTableRows) Close()     {}

func TestCreateBalanceCodeForSaleRunsOnlyForANewCode(t *testing.T) {
	tx := &balanceCodeTableTx{purchaseEventIds: map[string]bool{}}
	created := []string{}
	createdInTx := func(tx server.PgTx) {
		created = append(created, "cs_1")
	}

	balanceCode, err := createBalanceCodeInTx(tx, context.Background(), Tib, 365*24*time.Hour, UsdToNanoCents(95), "cs_1", "{}", "", createdInTx)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, balanceCode.PurchaseEventId, "cs_1")
	connect.AssertEqual(t, created, []string{"cs_1"})

	// Stripe redelivers the checkout: the code exists, nothing new is recorded
	balanceCode, err = createBalanceCodeInTx(tx, context.Background(), Tib, 365*24*time.Hour, UsdToNanoCents(95), "cs_1", "{}", "", createdInTx)
	connect.AssertNotEqual(t, err, nil)
	connect.AssertEqual(t, balanceCode == nil, true)
	connect.AssertEqual(t, created, []string{"cs_1"})

	// an administrative code (no sale) has nothing to run
	_, err = createBalanceCodeInTx(tx, context.Background(), Tib, 365*24*time.Hour, 0, "admin-1", "", "", nil)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, created, []string{"cs_1"})
}
