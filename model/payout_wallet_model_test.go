package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestPayoutWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		wallet1 := &CreateAccountWalletExternalArgs{
			NetworkId:        networkId,
			Blockchain:       "matic",
			WalletAddress:    "0x0",
			DefaultTokenType: "usdc",
		}

		wallet2 := &CreateAccountWalletExternalArgs{
			NetworkId:        networkId,
			Blockchain:       "matic",
			WalletAddress:    "0x1",
			DefaultTokenType: "usdc",
		}

		walletId1 := CreateAccountWalletExternal(session, wallet1)
		walletId2 := CreateAccountWalletExternal(session, wallet2)
		connect.AssertNotEqual(t, walletId1, nil)
		connect.AssertNotEqual(t, walletId2, nil)

		err := SetPayoutWallet(ctx, networkId, *walletId1)
		connect.AssertEqual(t, err, nil)

		payoutWalletId := GetPayoutWalletId(ctx, networkId)
		payoutAccountWallet := GetAccountWallet(ctx, *payoutWalletId)

		connect.AssertEqual(t, payoutAccountWallet.WalletAddress, wallet1.WalletAddress)

		err = SetPayoutWallet(ctx, networkId, *walletId2)
		connect.AssertEqual(t, err, nil)

		payoutWalletId = GetPayoutWalletId(ctx, networkId)
		payoutAccountWallet = GetAccountWallet(ctx, *payoutWalletId)

		connect.AssertEqual(t, payoutAccountWallet.WalletAddress, wallet2.WalletAddress)

		deletePayoutWallet(*payoutWalletId, session)
		payoutWalletId = GetPayoutWalletId(ctx, networkId)
		connect.AssertEqual(t, payoutWalletId, nil)

	})
}

func TestSetPayoutWalletValidatesOwnership(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkAId := server.NewId()
		clientAId := server.NewId()
		networkBId := server.NewId()
		clientBId := server.NewId()

		sessionA := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkAId,
			ClientId:  &clientAId,
		})
		sessionB := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
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

		walletBId := CreateAccountWalletExternal(sessionB, &CreateAccountWalletExternalArgs{
			NetworkId:        networkBId,
			Blockchain:       "matic",
			WalletAddress:    "0xbbbb",
			DefaultTokenType: "usdc",
		})
		connect.AssertNotEqual(t, walletBId, nil)

		// a network cannot set another network's wallet as its payout wallet
		err := SetPayoutWallet(ctx, networkBId, *walletAId)
		connect.AssertNotEqual(t, err, nil)
		connect.AssertEqual(t, GetPayoutWalletId(ctx, networkBId), nil)

		// a network cannot set a wallet that does not exist
		err = SetPayoutWallet(ctx, networkBId, server.NewId())
		connect.AssertNotEqual(t, err, nil)
		connect.AssertEqual(t, GetPayoutWalletId(ctx, networkBId), nil)

		// a network can set its own wallet
		err = SetPayoutWallet(ctx, networkBId, *walletBId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *GetPayoutWalletId(ctx, networkBId), *walletBId)

		// a failed set does not overwrite the existing payout wallet
		err = SetPayoutWallet(ctx, networkBId, *walletAId)
		connect.AssertNotEqual(t, err, nil)
		connect.AssertEqual(t, *GetPayoutWalletId(ctx, networkBId), *walletBId)

		// a network cannot set a deactivated wallet
		removeResult := RemoveWallet(*walletBId, sessionB)
		connect.AssertEqual(t, removeResult.Success, true)
		// removing the payout wallet clears the payout wallet selection
		connect.AssertEqual(t, GetPayoutWalletId(ctx, networkBId), nil)
		err = SetPayoutWallet(ctx, networkBId, *walletBId)
		connect.AssertNotEqual(t, err, nil)
		connect.AssertEqual(t, GetPayoutWalletId(ctx, networkBId), nil)

	})
}
