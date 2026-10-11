// Scheduled closes carry each released legacy reservation's cached snapshot
// through their revision advances, as settlement does, so neither the refresh
// post nor the next locked admission rereads the balance's escrow history.
package model

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Counts reservation censuses sent by any real connection in the scope.
type deadlineCensusCounter struct {
	count atomic.Int64
}

func (self *deadlineCensusCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == netEscrowReservationPageSQL {
		self.count.Add(1)
	}
	return ctx
}

func (self *deadlineCensusCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Runs one callback with every pooled statement observed.
func deadlineCensusCount(t testing.TB, ctx context.Context, f func()) int64 {
	t.Helper()
	counter := &deadlineCensusCounter{}
	scope, err := server.NewTestPgQueryScope(ctx, counter)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}()
	f()
	return counter.count.Load()
}

// Requires the cache, the mirror and the next locked admission to agree on an
// exact reservation without another census.
func requireDeadlineCarriedSnapshot(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, reserved ByteCount) {
	t.Helper()
	cached, ok := settlementCacheSnapshot(ctx, []server.Id{f.balanceId})[f.balanceId]
	if !ok || cached.reserved != reserved {
		t.Fatalf("cached reservation=%+v current=%t, want %d at the current revision", cached, ok, reserved)
	}
	if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != reserved {
		t.Fatalf("mirror=%d want %d", got, reserved)
	}
	if got, reads := lockedAdmissionCacheTestRead(ctx, f); got.reserved != reserved || reads != 0 {
		t.Fatalf("next admission reserved=%d censuses=%d, want %d without a census", got.reserved, reads, reserved)
	}
}

// Before, the close advanced the balance revision twice without carrying the
// cached snapshot, so its refresh post censused the balance's whole live legacy
// escrow history, and so did the next locked admission when no post ran first.
func TestDeadlineCloseCarriesReservationSnapshot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := seedAdmissionCacheHistory(t, ctx, 3, 100)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 17)
		server.RunPosts(ctx, posts...)
		if cached := settlementCacheSnapshot(ctx, []server.Id{f.balanceId})[f.balanceId]; cached.reserved != 20 {
			t.Fatalf("creation left cached reservation %+v, want 20", cached)
		}
		server.Raise(CloseContract(ctx, contract.ContractId, f.sourceId, 5, true))

		deadline := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
		censuses := deadlineCensusCount(t, ctx, func() {
			result, err := ReconcileContractAtDeadline(ctx, contract.ContractId, deadline)
			if err != nil || result == nil || result.AlreadyClosed || result.Missing {
				t.Fatal("deadline did not close the open contract", result, err)
			}
		})
		if censuses != 0 {
			t.Fatalf("deadline close and its refresh sent %d reservation censuses, want 0", censuses)
		}
		requireDeadlineCarriedSnapshot(t, ctx, f, 3)
	})
}

// Owner batches close several members of one balance in a single transaction;
// each member reads the snapshot the previous member carried.
func TestDeadlineOwnerBatchCarriesReservationSnapshot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := seedAdmissionCacheHistory(t, ctx, 3, 100)
		primary, posts := createNetEscrowOrderingTestContract(ctx, f, 17)
		server.RunPosts(ctx, posts...)
		siblingIds := map[server.Id]bool{}
		for _, bytes := range []ByteCount{11, 7} {
			sibling, posts := createNetEscrowOrderingTestContract(ctx, f, bytes)
			server.RunPosts(ctx, posts...)
			siblingIds[sibling.ContractId] = true
		}
		if cached := settlementCacheSnapshot(ctx, []server.Id{f.balanceId})[f.balanceId]; cached.reserved != 38 {
			t.Fatalf("creations left cached reservation %+v, want 38", cached)
		}

		deadline := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
		due := func(ctx context.Context, query server.PgCanQuery, contractIds []server.Id) []ContractDeadline {
			deadlines := []ContractDeadline{}
			for _, contractId := range contractIds {
				if siblingIds[contractId] {
					deadlines = append(deadlines, ContractDeadline{ContractId: contractId, Deadline: deadline})
				}
			}
			return deadlines
		}
		censuses := deadlineCensusCount(t, ctx, func() {
			result, err := ReconcileContractOwnerAtDeadline(ctx, []server.Id{f.balanceId},
				ContractDeadline{ContractId: primary.ContractId, Deadline: deadline}, due)
			if err != nil || result == nil || result.Busy || result.Reconciliation == nil || result.SiblingsClosed != 2 {
				t.Fatal("owner batch did not close the primary and both siblings", result, err)
			}
		})
		if censuses != 0 {
			t.Fatalf("owner batch and its refresh sent %d reservation censuses, want 0", censuses)
		}
		requireDeadlineCarriedSnapshot(t, ctx, f, 3)
	})
}

// A cached amount below the reservation the close releases cannot be carried.
// The close still commits, and its refresh replaces the bad snapshot exactly.
func TestDeadlineCloseDropsInconsistentReservationSnapshot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := seedAdmissionCacheHistory(t, ctx, 3, 100)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 17)
		server.RunPosts(ctx, posts...)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance_net_escrow_snapshot SET reserved_byte_count=5 WHERE balance_id=$1`, f.balanceId))
		})

		deadline := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
		censuses := deadlineCensusCount(t, ctx, func() {
			result, err := ReconcileContractAtDeadline(ctx, contract.ContractId, deadline)
			if err != nil || result == nil || result.AlreadyClosed || result.Missing {
				t.Fatal("an inconsistent optional snapshot blocked the deadline close", result, err)
			}
		})
		if censuses != 1 {
			t.Fatalf("refresh sent %d reservation censuses for a dropped snapshot, want 1", censuses)
		}
		requireDeadlineCarriedSnapshot(t, ctx, f, 3)
	})
}
