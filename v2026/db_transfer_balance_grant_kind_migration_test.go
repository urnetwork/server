package server

// The grant_kind migration on the hot transfer_balance table.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

// transfer_balance is a hot table. The ALTER that adds grant_kind must give up after
// its lock timeout instead of waiting behind a long lock holder (the nightly pg_dump
// holds a share lock on it for hours) with every balance query queued behind the
// ALTER. A migrate that gave up applies on a later run, and the column is a plain
// nullable varchar.
func TestTransferBalanceGrantKindMigrationGivesUpBehindALongLock(t *testing.T) {
	index := slices.IndexFunc(migrations, func(migration any) bool {
		sqlMigration, ok := migration.(*SqlMigration)
		return ok && strings.Contains(sqlMigration.sql, "ADD COLUMN grant_kind")
	})
	if index < 0 {
		t.Fatal("grant_kind migration is missing")
	}

	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		ApplyDbMigrationsUpTo(ctx, index)

		// a share lock held the way pg_dump holds it. It is released after a bound,
		// so a migration that waits for it finishes instead of hanging the test.
		locked := make(chan struct{})
		release := make(chan struct{})
		released := make(chan struct{})
		go func() {
			defer close(released)
			MaintenanceTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `LOCK TABLE transfer_balance IN ACCESS SHARE MODE`))
				close(locked)
				select {
				case <-release:
				case <-time.After(30 * time.Second):
				}
			}, OptNoRetry())
		}()
		<-locked

		startTime := time.Now()
		var migrateErr any
		func() {
			defer func() {
				migrateErr = recover()
			}()
			ApplyDbMigrationsUpTo(ctx, index+1)
		}()
		elapsed := time.Since(startTime)
		close(release)
		<-released

		var pgErr *pgconn.PgError
		err, _ := migrateErr.(error)
		if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.LockNotAvailable {
			t.Fatalf("migrate behind the lock returned %v after %s, want a lock timeout", migrateErr, elapsed)
		}
		if 20*time.Second < elapsed {
			t.Fatalf("migrate waited %s behind the lock", elapsed)
		}
		if version := DbVersion(ctx); version != index {
			t.Fatalf("db version = %d after the migrate gave up, want %d", version, index)
		}

		ApplyDbMigrationsUpTo(ctx, index+1)
		if version := DbVersion(ctx); version != index+1 {
			t.Fatalf("db version = %d after the re-run, want %d", version, index+1)
		}
		dataType, notNull, defaultExpression, err := loadMigrationColumnShape(ctx, "transfer_balance", "grant_kind")
		if err != nil || dataType != "character varying(32)" || notNull || defaultExpression != "" {
			t.Fatalf("grant_kind column = %s not_null=%t default=%q (%v)", dataType, notNull, defaultExpression, err)
		}
	})
}
