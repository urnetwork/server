package server

import (
	"context"
	"testing"
)

// Each public acquisition family must trip before resolving credentials or
// opening a pool. The fixture needs no PostgreSQL or Redis service.
func TestPacketPostgresGuardCoversAcquisitionFamilies(t *testing.T) {
	ctx := WithoutPostgres(context.Background())
	operations := []func(){
		func() { Db(ctx, func(PgConn) { t.Error("db callback ran") }) },
		func() { ReplicaDb(ctx, func(PgConn) { t.Error("replica callback ran") }) },
		func() { MaintenanceDb(ctx, func(PgConn) { t.Error("maintenance callback ran") }) },
		func() { Tx(ctx, func(PgTx) { t.Error("transaction callback ran") }) },
		func() { MaintenanceTx(ctx, func(PgTx) { t.Error("maintenance transaction callback ran") }) },
		func() { OwnedTx(ctx, nil, func(PgTx) { t.Error("owned transaction callback ran") }) },
		func() { TryOwnedTx(ctx, nil, func(PgTx) { t.Error("try-owned transaction callback ran") }) },
		func() { _, _ = AcquireMaintenanceDbConn(ctx) },
	}
	for index, operation := range operations {
		func() {
			defer func() {
				if recover() != ErrPacketPostgres {
					t.Errorf("acquisition %d did not refuse at the packet boundary", index)
				}
			}()
			operation()
		}()
	}
	if got := PacketPostgresAttempts(ctx); got != uint64(len(operations)) {
		t.Fatalf("recorded forbidden attempts=%d", got)
	}
}

// Cancellation detachment and repeated marking cannot discard the boundary.
func TestPacketPostgresGuardSurvivesContextDerivation(t *testing.T) {
	ctx := WithoutPostgres(context.Background())
	child, cancel := context.WithCancel(ctx)
	cancel()
	derived := WithoutPostgres(context.WithoutCancel(child))
	func() {
		defer func() {
			if recover() != ErrPacketPostgres {
				t.Error("derived context lost the packet boundary")
			}
		}()
		Db(derived, func(PgConn) { t.Error("derived callback ran") })
	}()
	if PacketPostgresAttempts(ctx) != 1 || PacketPostgresAttempts(derived) != 1 || PacketPostgresAttempts(context.Background()) != 0 {
		t.Fatal("context derivation lost or duplicated the attempt counter")
	}
}

// The process oracle catches a model that discards the marked caller context;
// every acquisition family must refuse before pool or credential resolution.
func TestPacketPostgresGuardProcessScopeCatchesFreshContexts(t *testing.T) {
	attempts, stop := DenyPostgresForTest(t)
	defer stop()
	operations := []func(){
		func() { Db(context.Background(), func(PgConn) { t.Error("db callback ran") }) },
		func() { ReplicaDb(context.Background(), func(PgConn) { t.Error("replica callback ran") }) },
		func() {
			MaintenanceDb(context.WithoutCancel(t.Context()), func(PgConn) { t.Error("maintenance callback ran") })
		},
		func() { Tx(context.Background(), func(PgTx) { t.Error("transaction callback ran") }) },
		func() {
			MaintenanceTx(context.Background(), func(PgTx) { t.Error("maintenance transaction callback ran") })
		},
		func() { _, _ = AcquireMaintenanceDbConn(context.Background()) },
		func() { OwnedTx(context.Background(), nil, func(PgTx) { t.Error("owned transaction callback ran") }) },
		func() {
			TryOwnedTx(context.Background(), nil, func(PgTx) { t.Error("try-owned transaction callback ran") })
		},
	}
	for index, operation := range operations {
		func() {
			defer func() {
				if recover() != ErrPacketPostgres {
					t.Errorf("fresh-context acquisition %d did not refuse", index)
				}
			}()
			operation()
		}()
	}
	if attempts() != uint64(len(operations)) {
		t.Fatalf("process oracle recorded %d attempts", attempts())
	}
	stop()
	nextAttempts, nextStop := DenyPostgresForTest(t)
	defer nextStop()
	stop() // A stale cleanup cannot disarm the successor.
	HandleError(func() { Db(context.Background(), func(PgConn) { t.Error("recovered callback ran") }) })
	if nextAttempts() != 1 || attempts() != uint64(len(operations)) {
		t.Fatal("stale cleanup cleared successor or recovered panic lost its attempt")
	}
}

// A permit belongs to one exact oracle and context. Fresh contexts, a second
// acquisition, explicit revocation and marked packet contexts must still fail.
func TestPacketPostgresGuardSingleAcquisitionPermitHasExactScope(t *testing.T) {
	attempts, stop := DenyPostgresForTest(t)
	defer stop()
	permitted, revoke := PermitPostgresAcquisitionForTest(t, t.Context())
	defer revoke()
	checkPostgresAllowed(permitted)
	if attempts() != 1 {
		t.Fatal("healthy permitted acquisition was not counted")
	}
	revoked, revokeBeforeUse := PermitPostgresAcquisitionForTest(t, t.Context())
	revokeBeforeUse()
	marked := WithoutPostgres(t.Context())
	stillMarked, revokeMarked := PermitPostgresAcquisitionForTest(t, marked)
	defer revokeMarked()
	for index, ctx := range []context.Context{context.Background(), permitted, revoked, stillMarked} {
		func() {
			defer func() {
				if recover() != ErrPacketPostgres {
					t.Errorf("refusal case %d escaped the scoped permit", index)
				}
			}()
			checkPostgresAllowed(ctx)
		}()
	}
	if attempts() != 5 || PacketPostgresAttempts(marked) != 1 {
		t.Fatal("permitted scope lost a refused attempt or bypassed a context guard")
	}
	stale, revokeStale := PermitPostgresAcquisitionForTest(t, t.Context())
	defer revokeStale()
	stop()
	nextAttempts, nextStop := DenyPostgresForTest(t)
	defer nextStop()
	recovered := HandleError(func() { checkPostgresAllowed(stale) })
	if recovered != ErrPacketPostgres || nextAttempts() != 1 {
		t.Fatal("old permit escaped or failed to count in a successor oracle")
	}
}
