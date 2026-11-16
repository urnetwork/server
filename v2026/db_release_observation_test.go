// Release proof tests use private pgx wire fixtures. They neither configure a
// host pool nor wait for a database, and never fabricate a cleanup join.
package server

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The actual optional interface is what pgxpool invokes before Destroy.
type dbReleaseTestTracer struct{}

func (self *dbReleaseTestTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (self *dbReleaseTestTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (self *dbReleaseTestTracer) TraceRelease(*pgxpool.Pool, pgxpool.TraceReleaseData)            {}

// Optional hooks are not assumed bounded merely because pgx bounds Close.
func TestDbReleasePolicyRequiresActualNoHookConfiguration(t *testing.T) {
	config := &pgxpool.Config{ConnConfig: &pgx.ConnConfig{}, MaxConns: 2}
	if !dbPoolReleasePolicyKnown(config) {
		t.Fatal("ordinary no-hook pool has no concrete release contract")
	}
	for _, change := range []func(*pgxpool.Config){
		func(value *pgxpool.Config) { value.BeforeClose = func(*pgx.Conn) {} },
		func(value *pgxpool.Config) { value.AfterRelease = func(*pgx.Conn) bool { return true } },
		func(value *pgxpool.Config) { value.ConnConfig.Tracer = &dbReleaseTestTracer{} },
		func(value *pgxpool.Config) { value.MaxConns = 0 },
		func(value *pgxpool.Config) { value.ConnConfig = nil },
	} {
		copy := config.Copy()
		change(copy)
		if dbPoolReleasePolicyKnown(copy) {
			t.Fatal("unknown release/destructor hook borrowed the no-hook contract")
		}
	}
	if dbPoolReleasePolicyKnown(nil) {
		t.Fatal("absent configuration authenticated a transfer")
	}
}

// Entry alone cannot grant permission. Pending cleanup remains pending even
// after a completed transfer, and the copied receipt never updates itself.
func TestDbReleaseObservationDistinguishesTransferFromPhysicalCompletion(t *testing.T) {
	observation := NewDbTxObservation()
	pending := make(chan struct{})
	observation.releaseEntered()
	if snapshot := observation.Snapshot(); snapshot.ReleaseAttempts != 1 || snapshot.ReleaseReturned != 0 || snapshot.LastRelease != (DbTxReleaseObservation{}) {
		t.Fatal("entered release invented a completed transfer", snapshot)
	}
	observation.releaseReturned(DbTxReleaseDestroyTransferred, pending)
	before := observation.Snapshot()
	if before.ReleaseReturned != 1 || before.LastRelease.Disposition != DbTxReleaseDestroyTransferred || before.LastRelease.CleanupComplete {
		t.Fatal("pending physical disposal was mislabeled joined", before)
	}
	close(pending)
	observation.releaseEntered()
	observation.releaseReturned(DbTxReleaseDestroyTransferred, pending)
	after := observation.Snapshot()
	if before.LastRelease.CleanupComplete || !after.LastRelease.CleanupComplete || after.ReleaseAttempts != 2 || after.ReleaseReturned != 2 {
		t.Fatal("release snapshot changed or cumulative custody was erased", before, after)
	}
}

// The real callback panic unwinds rollback and the existing pool release.
// Unknown hooks invalidate disposition without changing error precedence.
func TestDbReleaseObservationCapturesActualReturnedOwnerAndRejectsHooks(t *testing.T) {
	for _, install := range []func(*pgxpool.Config){
		nil,
		func(value *pgxpool.Config) { value.BeforeClose = func(*pgx.Conn) {} },
		func(value *pgxpool.Config) { value.AfterRelease = func(*pgx.Conn) bool { return true } },
		func(value *pgxpool.Config) { value.ConnConfig.Tracer = &dbReleaseTestTracer{} },
	} {
		_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(_ *pgPoolWireFixture, config *pgxpool.Config) {
			if install != nil {
				install(config)
			}
		})
		observation := NewDbTxObservation()
		cause := errors.New("synthetic callback refusal")
		recovered := captureDbErrorPanic(func() {
			txWithPool(t.Context(), pool, func(PgTx) { panic(cause) }, OptNoRetry(), observation)
		})
		snapshot := observation.Snapshot()
		if recovered != cause || snapshot.AcquireSucceeded != 1 || snapshot.RollbackAcknowledged != 1 || snapshot.ReleaseAttempts != 1 || snapshot.ReleaseReturned != 1 {
			t.Fatal("release receipt changed callback/rollback ownership", recovered, snapshot)
		}
		want := DbTxReleaseIdle
		if install != nil {
			want = DbTxReleaseUnknown
		}
		if snapshot.LastRelease.Disposition != want {
			t.Fatal("actual hook configuration borrowed another disposition", snapshot.LastRelease, want)
		}
	}
}
