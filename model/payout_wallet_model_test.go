package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// The payout wallet a network selected while USDC payouts ran reads as it
// was, and removing that wallet removes the selection.
func TestPayoutWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		wallet := &CreateAccountWalletExternalArgs{
			NetworkId:        networkId,
			Blockchain:       "matic",
			WalletAddress:    "0x0",
			DefaultTokenType: "usdc",
		}
		walletId := CreateAccountWalletExternal(clientSession, wallet)
		connect.AssertNotEqual(t, walletId, nil)
		connect.AssertEqual(t, GetPayoutWalletId(ctx, networkId), nil)

		testingSelectPayoutWallet(ctx, networkId, *walletId)
		payoutWalletId := GetPayoutWalletId(ctx, networkId)
		payoutAccountWallet := GetAccountWallet(ctx, *payoutWalletId)
		connect.AssertEqual(t, payoutAccountWallet.WalletAddress, wallet.WalletAddress)

		result := RemoveWallet(*walletId, clientSession)
		connect.AssertEqual(t, result.Success, true)
		connect.AssertEqual(t, GetPayoutWalletId(ctx, networkId), nil)
	})
}

// A stale or corrupt `payout_wallet` row reads as no payout wallet: one that
// names another network's wallet, or a wallet that was deactivated.
func TestGetPayoutWalletIdIgnoresStaleRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkAId := server.NewId()
		clientAId := server.NewId()
		networkBId := server.NewId()
		clientBId := server.NewId()

		sessionA := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: networkAId,
			ClientId:  &clientAId,
		})
		sessionB := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: networkBId,
			ClientId:  &clientBId,
		})

		walletAId := CreateAccountWalletExternal(sessionA, &CreateAccountWalletExternalArgs{
			NetworkId:        networkAId,
			Blockchain:       "matic",
			WalletAddress:    "0xaaaa",
			DefaultTokenType: "usdc",
		})
		connect.AssertNotEqual(t, walletAId, nil)

		// corrupt row: network B points at network A's wallet
		testingSelectPayoutWallet(ctx, networkBId, *walletAId)
		connect.AssertEqual(t, GetPayoutWalletId(ctx, networkBId), nil)

		walletBId := CreateAccountWalletExternal(sessionB, &CreateAccountWalletExternalArgs{
			NetworkId:        networkBId,
			Blockchain:       "matic",
			WalletAddress:    "0xbbbb",
			DefaultTokenType: "usdc",
		})
		connect.AssertNotEqual(t, walletBId, nil)
		testingSelectPayoutWallet(ctx, networkBId, *walletBId)
		connect.AssertEqual(t, *GetPayoutWalletId(ctx, networkBId), *walletBId)

		// stale row: the payout wallet was deactivated
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE account_wallet SET active = false WHERE wallet_id = $1`,
				*walletBId,
			))
		})
		connect.AssertEqual(t, GetPayoutWalletId(ctx, networkBId), nil)

		// the owner's own active payout wallet is still returned
		connect.AssertEqual(t, GetPayoutWalletId(ctx, networkAId), nil)
		testingSelectPayoutWallet(ctx, networkAId, *walletAId)
		connect.AssertEqual(t, *GetPayoutWalletId(ctx, networkAId), *walletAId)
	})
}

// USDC payouts ended (owner decisions 2026-11-12 and 2026-11-13), so removing
// the payout wallet leaves the network without one: no other wallet takes
// over, though an active Solana wallet is left. Before, the newest Solana or
// Polygon wallet was promoted. Removing a wallet that is not the payout wallet
// leaves the selection alone.
func TestRemoveWalletPromotesNoPayoutWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()
		owner := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})
		addWallet := func(blockchain string, address string) server.Id {
			walletId := CreateAccountWalletExternal(owner, &CreateAccountWalletExternalArgs{
				NetworkId:        networkId,
				Blockchain:       blockchain,
				WalletAddress:    address,
				DefaultTokenType: "usdc",
			})
			if walletId == nil {
				t.Fatalf("wallet %s was not created", address)
			}
			return *walletId
		}
		payoutWalletId := addWallet("SOL", "11111111111111111111111111111112")
		otherWalletId := addWallet("SOL", "11111111111111111111111111111113")
		newerWalletId := addWallet("SOL", "11111111111111111111111111111114")
		testingSelectPayoutWallet(ctx, networkId, payoutWalletId)

		if result := RemoveWallet(otherWalletId, owner); !result.Success {
			t.Fatalf("remove a wallet: %+v", result)
		}
		if payout := GetPayoutWalletId(ctx, networkId); payout == nil || *payout != payoutWalletId {
			t.Fatalf("removing another wallet changed the payout wallet to %v", payout)
		}
		if result := RemoveWallet(payoutWalletId, owner); !result.Success {
			t.Fatalf("remove the payout wallet: %+v", result)
		}
		if payout := GetPayoutWalletId(ctx, networkId); payout != nil {
			t.Fatalf("removing the payout wallet promoted %s, with %s left", *payout, newerWalletId)
		}
	})
}
