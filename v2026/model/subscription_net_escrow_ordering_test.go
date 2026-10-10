// These tests retain real post callbacks and run them after reconciliation.
// They reproduce the database/cache ordering failures without scheduler luck.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Each fixture owns two synthetic clients and exactly one payer balance.
type netEscrowOrderingTestFixture struct {
	sourceNetworkId      server.Id
	sourceId             server.Id
	destinationNetworkId server.Id
	destinationId        server.Id
	balanceId            server.Id
}

// Use ordinary model constructors so ownership, client locks and balance
// eligibility match the production reservation path.
func newNetEscrowOrderingTestFixture(t testing.TB, ctx context.Context) netEscrowOrderingTestFixture {
	t.Helper()
	f := netEscrowOrderingTestFixture{
		sourceNetworkId: server.NewId(), sourceId: server.NewId(),
		destinationNetworkId: server.NewId(), destinationId: server.NewId(),
	}
	testingCreatePaymentClient(ctx, f.sourceNetworkId, f.sourceId)
	testingCreatePaymentClient(ctx, f.destinationNetworkId, f.destinationId)
	AddBasicTransferBalance(ctx, f.sourceNetworkId, 1000, server.NowUtc(), server.NowUtc().Add(time.Hour))
	balances := GetActiveTransferBalances(ctx, f.sourceNetworkId)
	if len(balances) != 1 {
		t.Fatalf("payer balances = %d, want one", len(balances))
	}
	f.balanceId = balances[0].BalanceId
	return f
}

// Commit the same transaction as CreateTransferEscrow but let the test own
// post scheduling. No test hook or asynchronous timing controls production.
func createNetEscrowOrderingTestContract(ctx context.Context, f netEscrowOrderingTestFixture, bytes ByteCount) (*TransferEscrow, []func() any) {
	var escrow *TransferEscrow
	var posts []func() any
	server.Tx(ctx, func(tx server.PgTx) {
		var err error
		escrow, posts, err = createTransferEscrowInTx(ctx, tx,
			f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
			f.sourceNetworkId, bytes, nil)
		server.Raise(err)
	}, server.TxReadCommitted, server.OptNoRetry())
	return escrow, posts
}

// Bilateral zero-use closes retain the real settlement transition while
// avoiding unrelated payout increments when a post is deliberately replayed.
func settleNetEscrowOrderingTestContract(ctx context.Context, contractId server.Id) []func() any {
	var posts []func() any
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO contract_close (contract_id, party, used_transfer_byte_count, close_time, checkpoint)
			VALUES ($1, 'source', 0, $2, false), ($1, 'destination', 0, $2, false)`,
			contractId, server.NowUtc()))
		var err error
		var closed bool
		posts, closed, err = settleEscrowInTx(ctx, tx, contractId, ContractOutcomeSettled)
		server.Raise(err)
		if !closed {
			panic("test settlement did not claim the open contract")
		}
	}, server.TxReadCommitted, server.OptNoRetry())
	return posts
}

// A committed reservation repaired before its create post must not be counted
// twice. Replay tests the ambiguous Redis-success/response-loss boundary too.
func TestNetEscrowDelayedCreatePostAfterReconcile(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		_, posts := createNetEscrowOrderingTestContract(ctx, f, 17)
		ReconcileNetEscrowForNetwork(ctx, f.sourceNetworkId, true)
		server.RunPosts(ctx, posts...)
		server.RunPosts(ctx, posts...)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatalf("reconciled reservation after delayed/retried create = %d, want 17", got)
		}
	})
}

// A delayed settlement post must preserve another live contract's reservation
// after reconciliation already removed the settled contract's contribution.
func TestNetEscrowDelayedReleasePreservesNeighbor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		first, firstPosts := createNetEscrowOrderingTestContract(ctx, f, 17)
		server.RunPosts(ctx, firstPosts...)
		_, neighborPosts := createNetEscrowOrderingTestContract(ctx, f, 23)
		server.RunPosts(ctx, neighborPosts...)
		settlePosts := settleNetEscrowOrderingTestContract(ctx, first.ContractId)
		ReconcileNetEscrowForNetwork(ctx, f.sourceNetworkId, true)
		server.RunPosts(ctx, settlePosts...)
		server.RunPosts(ctx, settlePosts...)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 23 {
			t.Fatalf("neighbor reservation after delayed/retried settlement = %d, want 23", got)
		}
	})
}

// Settlement can finish before a delayed creation callback ever runs. The
// callback must read current durable state instead of resurrecting its delta.
func TestNetEscrowDelayedCreateAfterSettlement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, createPosts := createNetEscrowOrderingTestContract(ctx, f, 17)
		settlePosts := settleNetEscrowOrderingTestContract(ctx, contract.ContractId)
		server.RunPosts(ctx, settlePosts...)
		server.RunPosts(ctx, createPosts...)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 0 {
			t.Fatalf("completed contract resurrected reservation %d", got)
		}
	})
}
