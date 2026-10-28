package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
)

// salesLedgerTx is a transaction over in-memory tables: just enough postgres
// for the payment gates the sales notices hang off, so each gate runs as
// written, without a database. A gate statement behaves as its SQL does: an
// insert ON CONFLICT DO NOTHING inserts once, a guarded UPDATE updates once,
// a unique insert fails the second time, and the existence reads see what the
// earlier statements wrote. Any other write succeeds and any other read finds
// nothing. Any pgx.Tx method other than Exec, Query and QueryRow panics.
//
// One salesLedgerTx stands for the database across several transactions: a
// test calls a gate again on the same value to model a later delivery.
type salesLedgerTx struct {
	pgx.Tx

	stateLock sync.Mutex
	// networks that exist, for the payment network lock
	networks map[server.Id]bool
	// "table/key" of every gate row written
	rows     map[string]bool
	balances []salesLedgerBalance
	renewals []salesLedgerRenewal
}

type salesLedgerBalance struct {
	networkId     server.Id
	purchaseToken string
	startTime     time.Time
	endTime       time.Time
	pro           bool
}

type salesLedgerRenewal struct {
	networkId     server.Id
	market        string
	transactionId string
}

func newSalesLedgerTx(networkIds ...server.Id) *salesLedgerTx {
	networks := map[server.Id]bool{}
	for _, networkId := range networkIds {
		networks[networkId] = true
	}
	return &salesLedgerTx{
		networks: networks,
		rows:     map[string]bool{},
	}
}

var errSalesLedgerUniqueViolation = &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}

func salesLedgerTag(rowsAffected int) pgconn.CommandTag {
	return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", rowsAffected))
}

// insertOnce is an insert ON CONFLICT DO NOTHING.
func (self *salesLedgerTx) insertOnce(key string) pgconn.CommandTag {
	if self.rows[key] {
		return salesLedgerTag(0)
	}
	self.rows[key] = true
	return salesLedgerTag(1)
}

func (self *salesLedgerTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	switch {
	case strings.Contains(sql, "INSERT INTO stripe_invoice"):
		return self.insertOnce("stripe_invoice/" + args[0].(string)), nil

	case strings.Contains(sql, "INSERT INTO apple_subscription_transaction"):
		return self.insertOnce("apple_subscription_transaction/" + args[0].(string)), nil

	case strings.Contains(sql, "UPDATE solana_payment_intent") && strings.Contains(sql, "tx_signature IS NULL"):
		// SET tx_signature = $1 WHERE payment_reference = $2 AND tx_signature IS NULL
		return self.insertOnce("solana_payment_intent/" + args[1].(string)), nil

	case strings.Contains(sql, "UPDATE crypto_payment_intent") && strings.Contains(sql, "credited_at IS NULL"):
		return self.insertOnce("crypto_payment_intent/" + args[0].(server.Id).String()), nil

	case strings.Contains(sql, "INSERT INTO transfer_balance_code"):
		key := "transfer_balance_code/" + args[7].(string)
		if self.rows[key] {
			// UNIQUE (purchase_event_id)
			return pgconn.CommandTag{}, errSalesLedgerUniqueViolation
		}
		self.rows[key] = true
		return salesLedgerTag(1), nil

	case strings.Contains(sql, "INSERT INTO transfer_balance ("):
		balance := salesLedgerBalance{
			networkId: args[1].(server.Id),
			startTime: args[2].(time.Time),
			endTime:   args[3].(time.Time),
			// AddProTransferBalanceInTx writes six columns, pro always true
			pro: true,
		}
		if len(args) == 10 {
			balance.purchaseToken = args[7].(string)
			balance.pro = args[9].(bool)
		}
		self.balances = append(self.balances, balance)
		return salesLedgerTag(1), nil

	case strings.Contains(sql, "INSERT INTO subscription_renewal"):
		self.renewals = append(self.renewals, salesLedgerRenewal{
			networkId:     args[0].(server.Id),
			market:        args[6].(string),
			transactionId: args[7].(string),
		})
		return salesLedgerTag(1), nil
	}
	return salesLedgerTag(1), nil
}

func (self *salesLedgerTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	switch {
	case strings.Contains(sql, "FROM apple_subscription_transaction ledger"):
		// HasPreLedgerAppleRenewalInTx: no credit from before the ledger
		return &salesLedgerRows{rows: [][]any{{false}}}, nil

	case strings.Contains(sql, "FROM transfer_balance") && strings.Contains(sql, "purchase_token = $1"):
		// the overlap read: a balance of the token covering the expiry
		purchaseToken := args[0].(string)
		expiryTime := args[1].(time.Time)
		for _, balance := range self.balances {
			if balance.purchaseToken == purchaseToken && expiryTime.Before(balance.endTime) && !expiryTime.Before(balance.startTime) {
				return &salesLedgerRows{rows: [][]any{{server.NewId()}}}, nil
			}
		}
		return &salesLedgerRows{}, nil

	case strings.Contains(sql, "FROM subscription_renewal") && strings.Contains(sql, "UNION ALL"):
		// x402AlreadyGrantedForTransaction
		networkId := args[0].(server.Id)
		market := args[1].(string)
		transaction := args[2].(string)
		for _, renewal := range self.renewals {
			if renewal.networkId == networkId && renewal.market == market && renewal.transactionId == transaction {
				return &salesLedgerRows{rows: [][]any{{1}}}, nil
			}
		}
		for _, balance := range self.balances {
			if balance.networkId == networkId && balance.purchaseToken == transaction {
				return &salesLedgerRows{rows: [][]any{{1}}}, nil
			}
		}
		return &salesLedgerRows{}, nil

	case strings.Contains(sql, "FROM transfer_balance_code") && strings.Contains(sql, "purchase_event_id = $1"):
		if self.rows["transfer_balance_code/"+args[0].(string)] {
			return &salesLedgerRows{rows: [][]any{{server.NewId()}}}, nil
		}
		return &salesLedgerRows{}, nil
	}
	return &salesLedgerRows{}, nil
}

func (self *salesLedgerTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if strings.Contains(sql, "payment-network-credit-lock") {
		networkId := args[0].(server.Id)
		if self.networks[networkId] {
			return &salesLedgerRow{values: []any{networkId}}
		}
	}
	return &salesLedgerRow{err: pgx.ErrNoRows}
}

type salesLedgerRow struct {
	values []any
	err    error
}

func (self *salesLedgerRow) Scan(dest ...any) error {
	if self.err != nil {
		return self.err
	}
	return salesLedgerScan(self.values, dest)
}

type salesLedgerRows struct {
	rows  [][]any
	index int
}

func (self *salesLedgerRows) Close()                                       {}
func (self *salesLedgerRows) Err() error                                   { return nil }
func (self *salesLedgerRows) CommandTag() pgconn.CommandTag                { return salesLedgerTag(len(self.rows)) }
func (self *salesLedgerRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (self *salesLedgerRows) RawValues() [][]byte                          { return nil }
func (self *salesLedgerRows) Conn() *pgx.Conn                              { return nil }

func (self *salesLedgerRows) Next() bool {
	if self.index < len(self.rows) {
		self.index += 1
		return true
	}
	return false
}

func (self *salesLedgerRows) Values() ([]any, error) {
	if self.index == 0 || len(self.rows) < self.index {
		return nil, errors.New("no current row")
	}
	return self.rows[self.index-1], nil
}

func (self *salesLedgerRows) Scan(dest ...any) error {
	values, err := self.Values()
	if err != nil {
		return err
	}
	return salesLedgerScan(values, dest)
}

// salesLedgerScan assigns each value to its destination: a value of the
// destination's type, or of the type a pointer destination points to.
func salesLedgerScan(values []any, dest []any) error {
	if len(values) != len(dest) {
		return fmt.Errorf("scan of %d values into %d destinations", len(values), len(dest))
	}
	for i, d := range dest {
		target := reflect.ValueOf(d).Elem()
		if values[i] == nil {
			target.Set(reflect.Zero(target.Type()))
			continue
		}
		value := reflect.ValueOf(values[i])
		switch {
		case value.Type().AssignableTo(target.Type()):
			target.Set(value)
		case target.Kind() == reflect.Pointer && value.Type().AssignableTo(target.Type().Elem()):
			pointer := reflect.New(target.Type().Elem())
			pointer.Elem().Set(value)
			target.Set(pointer)
		case value.Type().ConvertibleTo(target.Type()):
			target.Set(value.Convert(target.Type()))
		default:
			return fmt.Errorf("cannot scan %T into %s", values[i], target.Type())
		}
	}
	return nil
}
