package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// TestDataCodeIsNotPro is the load-bearing rule of the data-code product: a data
// code is PAID (it carries revenue) but it is DATA ONLY. Redeeming one must add
// balance without granting the Pro entitlement.
//
// This is exactly the bug the old IsPro had -- it treated "has any paid balance"
// as Pro, so buying data silently upgraded you to Pro for free.
func TestDataCodeIsNotPro(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		// no balances at all -> not pro
		connect.AssertEqual(t, IsProNetwork(ctx, networkId), false)

		balanceCode, err := CreateBalanceCode(
			ctx,
			ByteCount(1024*1024*1024),
			Pro().DataCodeDuration,
			UsdToNanoCents(5.00),
			"test-purchase-pro-1",
			"test-purchase-pro-1-receipt",
			"test@bringyour.com",
		)
		connect.AssertEqual(t, err, nil)

		_, err = RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: networkId,
		}, ctx)
		connect.AssertEqual(t, err, nil)

		// the data landed...
		transferBalances := GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(transferBalances), 1)
		transferBalance := transferBalances[0]
		connect.AssertEqual(t, transferBalance.BalanceByteCount, ByteCount(1024*1024*1024))
		// ...and it IS a paid balance (it carries revenue)...
		connect.AssertEqual(t, transferBalance.Paid, true)
		// ...but it does NOT carry the Pro entitlement
		connect.AssertEqual(t, transferBalance.Pro, false)

		// the network is still free. This is the whole point.
		connect.AssertEqual(t, UpdateProNetwork(ctx, networkId), false)
		connect.AssertEqual(t, IsProNetwork(ctx, networkId), false)
	})
}

// TestProEntitlementFollowsProBalance covers the entitlement lifecycle through
// pro_model: a pro balance grants Pro for its window, and the cache reflects an
// upgrade immediately once the writer calls UpdateProNetwork.
func TestProEntitlementFollowsProBalance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		// prime the cache with the "not pro" answer, the way a hot path would
		connect.AssertEqual(t, IsProNetwork(ctx, networkId), false)

		now := server.NowUtc()

		// a free/data-only balance does not confer Pro even though it has bytes
		server.Tx(ctx, func(tx server.PgTx) {
			err := AddBasicTransferBalanceInTx(
				tx, ctx, networkId, ByteCount(30*1024*1024*1024), now, now.Add(24*time.Hour),
			)
			connect.AssertEqual(t, err, nil)
		})
		connect.AssertEqual(t, UpdateProNetwork(ctx, networkId), false)

		// a pro balance does
		server.Tx(ctx, func(tx server.PgTx) {
			err := AddProTransferBalanceInTx(
				tx, ctx, networkId, ByteCount(10*1024*1024*1024*1024), now, now.Add(30*24*time.Hour),
			)
			connect.AssertEqual(t, err, nil)
		})

		// the writer refreshes the cache, so the upgrade is visible immediately
		// rather than after ProCacheTtl
		connect.AssertEqual(t, UpdateProNetwork(ctx, networkId), true)
		connect.AssertEqual(t, IsProNetwork(ctx, networkId), true)

		// IsPro (the *server.Id wrapper the rest of the code uses) agrees
		connect.AssertEqual(t, IsPro(ctx, &networkId), true)
	})
}

// TestProEntitlementIsTimeBasedNotByteBased pins that spending the whole balance
// does not drop Pro: a subscriber is Pro for the period they paid for. (The `active`
// column on transfer_balance is bytes-remaining, so using it for the Pro check would
// silently downgrade anyone who used all their data.)
func TestProEntitlementIsTimeBasedNotByteBased(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		now := server.NowUtc()

		server.Tx(ctx, func(tx server.PgTx) {
			err := AddProTransferBalanceInTx(
				tx, ctx, networkId, ByteCount(1024), now, now.Add(30*24*time.Hour),
			)
			connect.AssertEqual(t, err, nil)
		})
		connect.AssertEqual(t, UpdateProNetwork(ctx, networkId), true)

		// drain the balance to zero -- `active` (0 < balance_byte_count) goes false
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE transfer_balance SET balance_byte_count = 0 WHERE network_id = $1`,
				networkId,
			))
		})

		// no data left...
		connect.AssertEqual(t, len(GetActiveTransferBalances(ctx, networkId)), 0)
		// ...but still Pro, because the period they paid for has not ended
		connect.AssertEqual(t, UpdateProNetwork(ctx, networkId), true)
	})
}

// TestProEntitlementExpires pins the other end: once the pro balance window closes,
// the network drops to free.
func TestProEntitlementExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		now := server.NowUtc()

		// a pro balance whose window has already closed
		server.Tx(ctx, func(tx server.PgTx) {
			err := AddProTransferBalanceInTx(
				tx, ctx, networkId,
				ByteCount(1024*1024),
				now.Add(-48*time.Hour),
				now.Add(-24*time.Hour),
			)
			connect.AssertEqual(t, err, nil)
		})

		connect.AssertEqual(t, UpdateProNetwork(ctx, networkId), false)
		connect.AssertEqual(t, IsProNetwork(ctx, networkId), false)
	})
}

// TestAddTransferBalanceProIsExplicit pins the trap that the `pro` column default is
// TRUE (so the migration keeps existing subscribers Pro). That default means any
// caller of AddTransferBalance that FORGETS to say what it means would silently grant
// Pro -- which is how a data-only purchase (a Play data pack, an x402 data sku) could
// upgrade a network for free.
//
// AddTransferBalanceInTx therefore writes the column explicitly from the struct, so a
// caller that says nothing gets pro = false (data only), not pro = true.
func TestAddTransferBalanceProIsExplicit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		// a data purchase: paid, but says nothing about Pro -> must NOT be Pro
		dataNetworkId := server.NewId()
		AddTransferBalance(ctx, &TransferBalance{
			NetworkId:             dataNetworkId,
			StartTime:             now,
			EndTime:               now.Add(24 * time.Hour),
			StartBalanceByteCount: 1 * Tib,
			BalanceByteCount:      1 * Tib,
			NetRevenue:            UsdToNanoCents(5.00),
			// Pro not set
		})
		balances := GetActiveTransferBalances(ctx, dataNetworkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Paid, true) // it IS paid
		connect.AssertEqual(t, balances[0].Pro, false) // but NOT pro
		connect.AssertEqual(t, UpdateProNetwork(ctx, dataNetworkId), false)

		// a subscription activation: says Pro: true -> is Pro
		proNetworkId := server.NewId()
		AddTransferBalance(ctx, &TransferBalance{
			NetworkId:             proNetworkId,
			StartTime:             now,
			EndTime:               now.Add(30 * 24 * time.Hour),
			StartBalanceByteCount: 600 * Gib,
			BalanceByteCount:      600 * Gib,
			SubsidyNetRevenue:     UsdToNanoCents(10.00),
			Pro:                   true,
		})
		balances = GetActiveTransferBalances(ctx, proNetworkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Pro, true)
		connect.AssertEqual(t, UpdateProNetwork(ctx, proNetworkId), true)
	})
}
