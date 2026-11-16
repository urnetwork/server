package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/operator-core/coretest"
	"github.com/urnetwork/server"
)

// operator-core's db suite over server's own helpers, on the private test stack.
func TestDbKeepsTheDbContract(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		coretest.DbSuite(tb, NewDb())
	})
}

// A statement's lock wait past the call's bound ends the call with ErrDbLockTimeout, unrerun, while the
// lock's holder waits at a barrier.
func TestDbWrapsALockTimeout(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		db := NewDb()
		rowId := operator.Id(connect.NewId())
		err := db.Tx(ctx, func(tx operator.DbTx) error {
			if _, err := tx.Exec(ctx, `CREATE TABLE core_lock_test (id uuid PRIMARY KEY)`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO core_lock_test (id) VALUES ($1)`, rowId)
			return err
		})
		if err != nil {
			tb.Fatal(err)
		}

		locked := make(chan struct{})
		release := make(chan struct{})
		holderDone := make(chan error, 1)
		go func() {
			holderDone <- db.Tx(ctx, func(tx operator.DbTx) error {
				rows, err := tx.Query(ctx, `SELECT id FROM core_lock_test WHERE id = $1 FOR UPDATE`, rowId)
				if err != nil {
					return err
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					return err
				}
				close(locked)
				<-release
				return nil
			}, operator.DbRetryNone)
		}()
		select {
		case <-locked:
		case err := <-holderDone:
			tb.Fatalf("the holder ended before it locked: %v", err)
		}

		attemptCount := 0
		err = db.Tx(ctx, func(tx operator.DbTx) error {
			attemptCount += 1
			_, err := tx.Exec(ctx, `UPDATE core_lock_test SET id = id WHERE id = $1`, rowId)
			return err
		}, operator.DbLockTimeout(50*time.Millisecond))
		close(release)
		if holderErr := <-holderDone; holderErr != nil {
			tb.Fatal(holderErr)
		}
		if !errors.Is(err, operator.ErrDbLockTimeout) {
			tb.Fatalf("answered %v, want ErrDbLockTimeout", err)
		}
		if attemptCount != 1 {
			tb.Errorf("%d attempts, want 1: a lock timeout is not rerun", attemptCount)
		}
	})
}
