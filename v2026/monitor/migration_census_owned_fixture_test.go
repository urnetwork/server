// Ordinary census fixtures mirror the production psql session's physical
// lifetime. Fault fixtures retain their caller-owned transactions unchanged.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// The fixture owns one independent cleanup budget for rollback, close and join.
// A nonpositive budget refuses observation before acquiring a connection.
type migrationCensusFixtureSettings struct {
	CleanupTimeout time.Duration
}

// Match the former rollback budget without extending the census query budget.
func defaultMigrationCensusFixtureSettings() migrationCensusFixtureSettings {
	return migrationCensusFixtureSettings{CleanupTimeout: 5 * time.Second}
}

// Invalid cleanup authority is never interpreted as unbounded cleanup.
func (self migrationCensusFixtureSettings) validate() error {
	if self.CleanupTimeout <= 0 {
		return errors.New("migration census fixture cleanup budget is invalid")
	}
	return nil
}

// These actions belong only to the ordinary adapter's acquired connection.
// The pool still owns release; no connection is hijacked or global pool reset.
type migrationCensusFixtureCleanup struct {
	rollback    func(context.Context) error
	close       func(context.Context) error
	cleanupDone <-chan struct{}
}

// A canceled observation still closes its own connection under a fresh budget.
func (self migrationCensusFixtureCleanup) finish(ctx context.Context, settings migrationCensusFixtureSettings) error {
	if err := settings.validate(); err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settings.CleanupTimeout)
	defer cancel()
	return self.finishWithin(cleanupCtx)
}

// Closure of CleanupDone proves local transport cleanup, not a server-side
// process-exit acknowledgement. An incomplete join remains an explicit error.
func (self migrationCensusFixtureCleanup) finishWithin(ctx context.Context) error {
	if self.rollback == nil || self.close == nil || self.cleanupDone == nil {
		return errors.New("migration census fixture cleanup ownership is incomplete")
	}
	rollbackErr := self.rollback(ctx)
	if rollbackErr == pgx.ErrTxClosed {
		rollbackErr = nil
	}
	closeErr := self.close(ctx)
	cleanupErr := errors.Join(rollbackErr, closeErr)
	// Physical completion wins if the owner resumes after its deadline. A
	// pending cleanup channel cannot turn cancellation into healthy release.
	select {
	case <-self.cleanupDone:
		return cleanupErr
	default:
	}
	select {
	case <-self.cleanupDone:
		return cleanupErr
	case <-ctx.Done():
		select {
		case <-self.cleanupDone:
			return cleanupErr
		default:
		}
		return errors.Join(cleanupErr, fmt.Errorf("migration census fixture physical cleanup did not join: %w", ctx.Err()))
	}
}

// A committed transaction is already closed, but its physical session must
// still be closed and joined before the ordinary adapter can publish success.
func TestMigrationCensusOwnedFixtureCleanupOrdersRollbackCloseAndJoin(t *testing.T) {
	stages := []string{}
	done := make(chan struct{})
	cleanup := migrationCensusFixtureCleanup{
		rollback: func(context.Context) error {
			stages = append(stages, "rollback")
			return pgx.ErrTxClosed
		},
		close: func(context.Context) error {
			stages = append(stages, "close")
			close(done)
			return nil
		},
		cleanupDone: done,
	}
	if err := cleanup.finish(t.Context(), defaultMigrationCensusFixtureSettings()); err != nil ||
		!reflect.DeepEqual(stages, []string{"rollback", "close"}) {
		t.Fatal("owned physical cleanup lost the exact transaction/connection ordering", stages, err)
	}
}

// Cleanup removes cancellation and the expired observation deadline, but keeps
// the owner's context values and one finite independent deadline.
func TestMigrationCensusOwnedFixtureCleanupUsesIndependentBudget(t *testing.T) {
	type fixtureContextKey struct{}
	parent, cancel := context.WithDeadline(context.WithValue(t.Context(), fixtureContextKey{}, "synthetic-owner"), time.Unix(1, 0))
	cancel()
	done := make(chan struct{})
	before := time.Now()
	settings := defaultMigrationCensusFixtureSettings()
	check := func(ctx context.Context) {
		deadline, ok := ctx.Deadline()
		if ctx.Err() != nil || ctx.Value(fixtureContextKey{}) != "synthetic-owner" || !ok ||
			deadline.Before(before.Add(settings.CleanupTimeout)) || time.Now().Add(settings.CleanupTimeout).Before(deadline) {
			t.Fatal("cleanup inherited dead authority or lost its independent bound")
		}
	}
	cleanup := migrationCensusFixtureCleanup{
		rollback: func(ctx context.Context) error { check(ctx); return nil },
		close: func(ctx context.Context) error {
			check(ctx)
			close(done)
			return nil
		},
		cleanupDone: done,
	}
	if err := cleanup.finish(parent, settings); err != nil || !errors.Is(parent.Err(), context.DeadlineExceeded) {
		t.Fatal("canceled observation prevented or renewed its physical cleanup", err, parent.Err())
	}
}

// Errors from both boundaries survive; an error merely containing ErrTxClosed
// is not proof of an otherwise successful rollback.
func TestMigrationCensusOwnedFixtureCleanupPreservesEveryFailure(t *testing.T) {
	primaryErr := errors.New("synthetic primary query failure")
	rollbackErr := errors.New("synthetic rollback failure")
	closeErr := errors.New("synthetic close failure")
	done := make(chan struct{})
	closeCalls := 0
	cleanup := migrationCensusFixtureCleanup{
		rollback: func(context.Context) error { return errors.Join(pgx.ErrTxClosed, rollbackErr) },
		close: func(context.Context) error {
			closeCalls++
			close(done)
			return closeErr
		},
		cleanupDone: done,
	}
	err := errors.Join(primaryErr, cleanup.finish(t.Context(), defaultMigrationCensusFixtureSettings()))
	if closeCalls != 1 || !errors.Is(err, primaryErr) || !errors.Is(err, rollbackErr) || !errors.Is(err, closeErr) {
		t.Fatal("cleanup masked a primary failure or skipped exact physical close", closeCalls, err)
	}
}

// A real close return is not enough if pgx is still cleaning an earlier
// canceled operation. Explicit cancellation proves the missing join fails.
func TestMigrationCensusOwnedFixtureCleanupRefusesUnjoinedClose(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	cleanup := migrationCensusFixtureCleanup{
		rollback: func(context.Context) error { return pgx.ErrTxClosed },
		close: func(context.Context) error {
			cancel()
			return nil
		},
		cleanupDone: done,
	}
	if err := cleanup.finishWithin(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("an unjoined physical cleanup became healthy release", err)
	}
}

// Once physical cleanup is positively complete, a later owner cancellation
// cannot undo that completion or manufacture a spurious failed join.
func TestMigrationCensusOwnedFixtureCleanupAcceptsCompletedClose(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	cleanup := migrationCensusFixtureCleanup{
		rollback: func(context.Context) error { return pgx.ErrTxClosed },
		close: func(context.Context) error {
			close(done)
			cancel()
			return nil
		},
		cleanupDone: done,
	}
	if err := cleanup.finishWithin(ctx); err != nil {
		t.Fatal("a physically completed cleanup was lost to a later cancellation", err)
	}
}

// Unknown handles cannot claim a join, even if another supplied channel is
// already closed. No action executes after the ownership check fails.
func TestMigrationCensusOwnedFixtureCleanupRefusesUnknownOwnership(t *testing.T) {
	done := make(chan struct{})
	close(done)
	calls := 0
	callback := func(context.Context) error { calls++; return nil }
	for _, cleanup := range []migrationCensusFixtureCleanup{
		{close: callback, cleanupDone: done},
		{rollback: callback, cleanupDone: done},
		{rollback: callback, close: callback},
	} {
		if err := cleanup.finish(t.Context(), defaultMigrationCensusFixtureSettings()); err == nil {
			t.Fatal("incomplete cleanup ownership was accepted")
		}
	}
	if calls != 0 {
		t.Fatal("a refused cleanup touched another owner's connection", calls)
	}
}

// Invalid owner timing refuses before opening a pool; the storage tripwire is
// live so this cannot pass because acquisition was already canceled.
func TestMigrationCensusOwnedFixtureInvalidBudgetRefusesBeforeDatabase(t *testing.T) {
	for _, budget := range []time.Duration{0, -time.Second} {
		ctx := server.WithoutPostgres(t.Context())
		observation := &migrationCensusObservation{settings: DefaultMigrationCensusSettings()}
		ctx = context.WithValue(ctx, migrationCensusContextKey{}, observation)
		source := &migrationPingDatabaseSource{censusFixtureSettings: &migrationCensusFixtureSettings{CleanupTimeout: budget}}
		rows, err := source.PostgreSQL(ctx, "WITH version AS (SELECT 1) SELECT * FROM version;")
		if err == nil || rows != nil || server.PacketPostgresAttempts(ctx) != 0 || ctx.Err() != nil {
			t.Fatal("invalid cleanup timing entered storage or fabricated an observation", err)
		}
	}
}

// The composed real signal, not a synthetic completion flag, closes the exact
// acquired pgx connection. A second complete observation owns another one.
func TestMigrationCensusOwnedFixturePgEveryObservationClosesPhysicalConnection(t *testing.T) {
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := snMainnetMigrationFixtureContext(t)
		defer cancel()
		physicals := []*pgx.Conn{}
		source := &migrationPingDatabaseSource{ownedCensusObserved: func(physical *pgx.Conn) { physicals = append(physicals, physical) }}
		var first Row
		for index := 0; index < 2; index++ {
			alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source))
			if err != nil || len(alerts) != 0 || len(physicals) != index+1 || len(source.artifactRow) == 0 {
				t.Fatal("the real complete signal lost its ordinary owned physical session", index, alerts, err)
			}
			physical := physicals[index]
			if !physical.IsClosed() {
				t.Fatal("ordinary census returned a reusable physical connection to the pool", index)
			}
			select {
			case <-physical.PgConn().CleanupDone():
			default:
				t.Fatal("ordinary census published before local physical cleanup joined", index)
			}
			if index == 0 {
				first = append(Row(nil), source.artifactRow...)
			} else if physical == physicals[0] || !reflect.DeepEqual(first, source.artifactRow) {
				t.Fatal("a later census reused an old physical session or changed the artifact row")
			}
		}
	})
}

// Cancellation after actual acquisition but before Begin cannot strand the
// connection. It also cannot replace the canceled query's primary failure.
func TestMigrationCensusOwnedFixturePgCanceledBeginStillClosesPhysicalConnection(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		observation := &migrationCensusObservation{settings: DefaultMigrationCensusSettings(), catalogRows: []pgRow{{"stale"}}, catalogComplete: true}
		ctx = context.WithValue(ctx, migrationCensusContextKey{}, observation)
		var physical *pgx.Conn
		source := &migrationPingDatabaseSource{artifactRow: Row{"stale"}, ownedCensusObserved: func(value *pgx.Conn) { physical = value; cancel() }}
		rows, err := source.PostgreSQL(ctx, "WITH version AS (SELECT 1) SELECT * FROM version;")
		if !errors.Is(err, context.Canceled) || rows != nil || physical == nil || !physical.IsClosed() ||
			observation.catalogRows != nil || observation.catalogComplete || source.artifactRow != nil {
			t.Fatal("canceled Begin lost error provenance, physical custody or stale-observation refusal", err)
		}
		select {
		case <-physical.PgConn().CleanupDone():
		default:
			t.Fatal("canceled Begin did not join its own physical cleanup")
		}
	})
}

// A plan refusal after Begin rolls back and closes the actual session without
// turning a prior completed catalog or artifact row into fresh evidence.
func TestMigrationCensusOwnedFixturePgPlanFailureClearsPriorObservation(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		observation := &migrationCensusObservation{settings: DefaultMigrationCensusSettings(), catalogRows: []pgRow{{"stale"}}, catalogComplete: true}
		ctx := context.WithValue(t.Context(), migrationCensusContextKey{}, observation)
		var physical *pgx.Conn
		source := &migrationPingDatabaseSource{artifactRow: Row{"stale"}, ownedCensusObserved: func(value *pgx.Conn) { physical = value }}
		rows, err := source.PostgreSQL(ctx, "SELECT 1;")
		if err == nil || !strings.Contains(err.Error(), "migration census") || rows != nil || physical == nil || !physical.IsClosed() ||
			observation.catalogRows != nil || observation.catalogComplete || source.artifactRow != nil {
			t.Fatal("a refused source plan retained usable session or stale observation", err)
		}
		select {
		case <-physical.PgConn().CleanupDone():
		default:
			t.Fatal("plan failure left its own physical cleanup unjoined")
		}
	})
}

// The sibling fault adapter borrows an existing transaction. Its uncommitted
// physical fault must be observed without committing or closing its owner.
func TestMigrationCensusOwnedFixturePgFaultTransactionRemainsCallerOwned(t *testing.T) {
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := snMainnetMigrationFixtureContext(t)
		defer cancel()
		server.Db(ctx, func(conn server.PgConn) {
			physical := conn.Conn()
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultMigrationCensusFixtureSettings().CleanupTimeout)
				defer cancel()
				if err := tx.Rollback(cleanupCtx); err != nil && err != pgx.ErrTxClosed {
					t.Error("fault owner could not roll back its own transaction", err)
				}
			}()
			if _, err := tx.Exec(ctx, "ALTER TABLE network_ping ALTER COLUMN hop_count SET DEFAULT 2"); err != nil {
				t.Fatal(err)
			}
			source := &snMainnetMigrationTxSource{tx: tx}
			alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source))
			if err != nil || physical.IsClosed() {
				t.Fatal("the borrowed fault census consumed its caller's physical connection", err)
			}
			found := false
			for _, alert := range alerts {
				if alert.Class == "migration-schema-drift" && strings.Contains(alert.Markdown(), "network_ping.hop_count@v701") {
					found = true
				}
			}
			var retained int
			if err := tx.QueryRow(ctx, "SELECT 1").Scan(&retained); err != nil || retained != 1 || !found {
				t.Fatal("fault observation committed, hid its mutation or retired the caller's transaction", found, err)
			}
		})
		if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
			t.Fatal("the fault owner's rollback did not restore the ordinary census", drift)
		}
	})
}
