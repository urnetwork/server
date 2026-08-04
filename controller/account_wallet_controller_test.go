package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestAccountWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		networkIdB := server.NewId()
		clientIdB := server.NewId()

		ownerSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		nonOwnerSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkIdB,
			ClientId:  &clientIdB,
		})

		// invalid chain
		result, err := CreateAccountWalletExternal(&model.CreateAccountWalletExternalArgs{
			Blockchain: "BTC",
		}, ownerSession)
		connect.AssertEqual(t, result, nil)
		connect.AssertEqual(t, err, ErrInvalidBlockchain)

		// invalid address
		result, err = CreateAccountWalletExternal(&model.CreateAccountWalletExternalArgs{
			Blockchain:    "SOL",
			WalletAddress: "1234",
		}, ownerSession)
		connect.AssertEqual(t, result, nil)
		connect.AssertEqual(t, err, ErrInvalidWalletAddress)

		// should have 0 wallets associated with this session
		walletResults, err := GetAccountWallets(ownerSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletResults.Wallets), 0)

		// payout wallet should be nil
		payoutWalletId := model.GetPayoutWalletId(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, payoutWalletId, nil)

		// success
		wallet := &model.CreateAccountWalletExternalArgs{
			Blockchain:    "SOL",
			WalletAddress: "74UNdYRpvakSABaYHSZMQNaXBVtA6eY9Nt8chcqocKe7",
		}

		_, err = CreateAccountWalletExternal(wallet, ownerSession)
		connect.AssertEqual(t, err, nil)

		// should have 1 wallets associated with this session
		walletResults, err = GetAccountWallets(ownerSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletResults.Wallets), 1)

		firstWalletId := walletResults.Wallets[0].WalletId

		// check if a payout wallet has been created too
		payoutWalletId = model.GetPayoutWalletId(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, payoutWalletId, firstWalletId)

		wallet2 := &model.CreateAccountWalletExternalArgs{
			Blockchain:    "SOL",
			WalletAddress: "74UNdYRpvakSABaYHSZMQNaXBVtA6eY9Nt8chcqocKe8",
		}

		_, err = CreateAccountWalletExternal(wallet2, ownerSession)
		connect.AssertEqual(t, err, nil)

		walletResults, err = GetAccountWallets(ownerSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletResults.Wallets), 2)

		// payout wallet should still be the first wallet
		payoutWalletId = model.GetPayoutWalletId(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, payoutWalletId, firstWalletId)

		// fail with invalid wallet id string
		toRemoveArgs := &model.RemoveWalletArgs{
			WalletId: "abc",
		}

		removeResult, err := RemoveWallet(toRemoveArgs, ownerSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removeResult.Success, false)
		connect.AssertNotEqual(t, removeResult.Error, nil)

		walletResults, err = GetAccountWallets(ownerSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletResults.Wallets), 2)

		// fail removing another users wallet
		toRemoveId := walletResults.Wallets[0].WalletId
		toRemoveArgs = &model.RemoveWalletArgs{
			WalletId: toRemoveId.String(),
		}

		removeResult, err = RemoveWallet(toRemoveArgs, nonOwnerSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removeResult.Success, false)

		walletResults, err = GetAccountWallets(ownerSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletResults.Wallets), 2)

		// successfully remove wallet (set active = false)
		toRemoveArgs = &model.RemoveWalletArgs{
			WalletId: toRemoveId.String(),
		}

		removeResult, err = RemoveWallet(toRemoveArgs, ownerSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removeResult.Success, true)
		connect.AssertEqual(t, removeResult.Error, nil)

		walletResults, err = GetAccountWallets(ownerSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletResults.Wallets), 1)

	})
}

// TestSeekerNFTVerification covers the holder predicates against recorded
// Helius responses (testdata/*.json, captured from mainnet.helius-rpc.com).
//
// These assertions used to run against the LIVE Helius RPC with hardcoded
// wallet keys, which made the suite bet on three things at once: the API being
// reachable and un-throttled, its indexer returning complete results on every
// call, and a stranger's wallet never transferring its NFT. It burned 4 of its
// 5 retry attempts in one full-suite run; the recorded response proves the
// wallet still held the NFT, so the failure was an incomplete API result — the
// parser was never wrong. Fixtures keep the predicate coverage (the part this
// package owns) and drop the coin flip. Re-record with a capture harness
// against the same four wallets if the API shape changes.
func TestSeekerNFTVerification(t *testing.T) {
	loadAssets := func(name string) []HeliusAsset {
		assetBytes, err := os.ReadFile(filepath.Join("testdata", name+".json"))
		connect.AssertEqual(t, err, nil)
		var assets []HeliusAsset
		connect.AssertEqual(t, json.Unmarshal(assetBytes, &assets), nil)
		return assets
	}

	// saga: collection grouping identifies the holder
	connect.AssertEqual(t, isSagaNftHolder(loadAssets("saga_holder_saga")), true)
	connect.AssertEqual(t, isSagaNftHolder(loadAssets("non_holder")), false)

	// seeker preorder: the asset id appears among the wallet's assets. The
	// recorded wallet holds thousands; the fixture keeps the match surrounded
	// by non-matching assets so a scan bug cannot pass by position.
	connect.AssertEqual(t, isSeekerNftHolder(loadAssets("seeker_preorder_holder")), true)

	// seeker genesis: token-2022 metadata pointer (authority + metadata
	// address), not an id match
	connect.AssertEqual(t, isSeekerNftHolder(loadAssets("seeker_genesis_holder")), true)

	// neither preorder nor genesis
	connect.AssertEqual(t, isSeekerNftHolder(loadAssets("non_holder")), false)
	// saga holding alone does not make a seeker holder
	connect.AssertEqual(t, isSeekerNftHolder(loadAssets("saga_holder")), false)
}
