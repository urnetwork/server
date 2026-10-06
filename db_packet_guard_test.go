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
