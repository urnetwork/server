package model

import (
	"context"
	"strings"
	"testing"
	"time"

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

// a stale `payout_wallet` row that the payout planner ignores must not read as
// a set payout wallet, otherwise connecting a new wallet does not replace it
// and the network keeps getting missing-wallet notices (support inbox 897)
func TestGetPayoutWalletIdIgnoresStaleRows(t *testing.T) {
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

		setRawPayoutWallet := func(networkId server.Id, walletId server.Id) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO payout_wallet (network_id, wallet_id)
						VALUES ($1, $2)
						ON CONFLICT (network_id) DO UPDATE
						SET wallet_id = $2
					`,
					networkId,
					walletId,
				))
			})
		}

		// corrupt row: network B points at network A's wallet
		setRawPayoutWallet(networkBId, *walletAId)
		connect.AssertEqual(t, GetPayoutWalletId(ctx, networkBId), nil)

		// connecting a wallet can now replace the stale row
		walletBId := CreateAccountWalletExternal(sessionB, &CreateAccountWalletExternalArgs{
			NetworkId:        networkBId,
			Blockchain:       "matic",
			WalletAddress:    "0xbbbb",
			DefaultTokenType: "usdc",
		})
		connect.AssertNotEqual(t, walletBId, nil)
		err := SetPayoutWallet(ctx, networkBId, *walletBId)
		connect.AssertEqual(t, err, nil)
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
		err = SetPayoutWallet(ctx, networkAId, *walletAId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *GetPayoutWalletId(ctx, networkAId), *walletAId)
	})
}

// The promoted wallet can receive payouts (Solana or Polygon, whatever the
// stored spelling), the removed wallet's chain wins over a newer wallet on
// the other chain, and otherwise the newest wallet wins. Candidates are
// newest first, as promotePayoutWalletInTx reads them.
func TestChoosePromotedPayoutWallet(t *testing.T) {
	// a candidate is "name:blockchain"
	for _, c := range []struct {
		name       string
		removed    string
		candidates []string
		want       string
	}{
		{name: "newest solana past a newer bittensor", removed: "SOL", candidates: []string{"tao:TAO", "sol-newer:solana", "sol-older:SOL"}, want: "sol-newer"},
		{name: "same chain over a newer polygon", removed: "SOL", candidates: []string{"matic-newer:MATIC", "sol-older:sol"}, want: "sol-older"},
		{name: "same chain over a newer solana", removed: "polygon", candidates: []string{"sol-newer:SOL", "poly-older:POLY"}, want: "poly-older"},
		{name: "other chain when the removed chain is gone", removed: "SOL", candidates: []string{"eth-newer:ETHEREUM", "matic-older:matic"}, want: "matic-older"},
		{name: "removed chain unknown", removed: "", candidates: []string{"unknown:DOGE", "matic:MATIC", "sol:SOL"}, want: "matic"},
		{name: "only bittensor and ethereum", removed: "SOL", candidates: []string{"tao:bittensor", "eth:ETH"}, want: ""},
		{name: "no candidates", removed: "SOL", candidates: nil, want: ""},
	} {
		nameWalletIds := map[string]server.Id{}
		candidates := []*payoutWalletCandidate{}
		for _, nameBlockchain := range c.candidates {
			name, blockchain, _ := strings.Cut(nameBlockchain, ":")
			nameWalletIds[name] = server.NewId()
			candidates = append(candidates, &payoutWalletCandidate{walletId: nameWalletIds[name], blockchain: blockchain})
		}
		promotedWalletId := choosePromotedPayoutWallet(c.removed, candidates)
		if c.want == "" {
			if promotedWalletId != nil {
				t.Errorf("%s: promoted %s, want none", c.name, *promotedWalletId)
			}
			continue
		}
		if promotedWalletId == nil || *promotedWalletId != nameWalletIds[c.want] {
			t.Errorf("%s: promoted %v, want %s (%s)", c.name, promotedWalletId, c.want, nameWalletIds[c.want])
		}
	}
}

// Removing the payout wallet promotes another active Solana or Polygon wallet
// of the same network, preferring the removed wallet's chain, then the newest
// (support inbox 897). A Bittensor wallet and another network's wallet are
// never chosen, and removing a wallet that is not the payout wallet leaves
// the payout wallet alone.
func TestRemoveWalletPromotesActivePayoutWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		startTime := server.NowUtc().Add(-time.Hour)

		newNetwork := func() (server.Id, *session.ClientSession) {
			networkId := server.NewId()
			clientId := server.NewId()
			return networkId, session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
				NetworkId: networkId,
				ClientId:  &clientId,
			})
		}
		// a wallet created `minute` minutes after startTime, so newest is explicit
		addWallet := func(clientSession *session.ClientSession, blockchain string, address string, minute int) server.Id {
			walletId := CreateAccountWalletExternal(clientSession, &CreateAccountWalletExternalArgs{
				NetworkId:        clientSession.ByJwt.NetworkId,
				Blockchain:       blockchain,
				WalletAddress:    address,
				DefaultTokenType: "usdc",
			})
			if walletId == nil {
				t.Fatalf("wallet %s was not created", address)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE account_wallet SET create_time = $2 WHERE wallet_id = $1`,
					*walletId,
					startTime.Add(time.Duration(minute)*time.Minute),
				))
			})
			return *walletId
		}
		removeWallet := func(clientSession *session.ClientSession, walletId server.Id) *RemoveWalletResult {
			result := RemoveWallet(walletId, clientSession)
			if !result.Success || result.Error != nil {
				t.Fatalf("remove wallet %s: %+v", walletId, result)
			}
			return result
		}
		assertPayoutWallet := func(name string, networkId server.Id, result *RemoveWalletResult, want *server.Id) {
			payoutWalletId := GetPayoutWalletId(ctx, networkId)
			if want == nil {
				if result.PayoutWalletId != nil || payoutWalletId != nil {
					t.Fatalf("%s: promoted %v, payout wallet %v, want none", name, result.PayoutWalletId, payoutWalletId)
				}
				return
			}
			if result.PayoutWalletId == nil || *result.PayoutWalletId != *want {
				t.Fatalf("%s: promoted %v, want %s", name, result.PayoutWalletId, *want)
			}
			if payoutWalletId == nil || *payoutWalletId != *want {
				t.Fatalf("%s: payout wallet %v, want %s", name, payoutWalletId, *want)
			}
		}

		// the newest Solana wallet, past a newer Bittensor wallet
		networkId, owner := newNetwork()
		payoutWalletId := addWallet(owner, "SOL", "synthetic-sol-payout", 1)
		olderWalletId := addWallet(owner, "SOL", "synthetic-sol-older", 2)
		newerWalletId := addWallet(owner, "solana", "synthetic-sol-newer", 3)
		addWallet(owner, "TAO", "synthetic-tao-newest", 4)
		connect.AssertEqual(t, SetPayoutWallet(ctx, networkId, payoutWalletId), nil)
		// not the payout wallet: nothing changes
		result := removeWallet(owner, olderWalletId)
		if result.PayoutWalletId != nil {
			t.Fatalf("removing a non-payout wallet promoted %s", *result.PayoutWalletId)
		}
		connect.AssertEqual(t, *GetPayoutWalletId(ctx, networkId), payoutWalletId)
		result = removeWallet(owner, payoutWalletId)
		assertPayoutWallet("newest solana", networkId, result, &newerWalletId)

		// the removed wallet's chain wins over a newer Polygon wallet
		sameChainNetworkId, sameChainOwner := newNetwork()
		sameChainPayoutWalletId := addWallet(sameChainOwner, "SOL", "synthetic-sol-payout-2", 1)
		sameChainWalletId := addWallet(sameChainOwner, "SOL", "synthetic-sol-same-chain", 2)
		addWallet(sameChainOwner, "MATIC", "synthetic-matic-newer", 3)
		connect.AssertEqual(t, SetPayoutWallet(ctx, sameChainNetworkId, sameChainPayoutWalletId), nil)
		result = removeWallet(sameChainOwner, sameChainPayoutWalletId)
		assertPayoutWallet("same chain", sameChainNetworkId, result, &sameChainWalletId)

		// only a Bittensor wallet is left, and a newer wallet belongs to
		// another network: no payout wallet
		taoNetworkId, taoOwner := newNetwork()
		taoPayoutWalletId := addWallet(taoOwner, "MATIC", "synthetic-matic-payout-3", 1)
		addWallet(taoOwner, "TAO", "synthetic-tao-only", 2)
		_, otherOwner := newNetwork()
		addWallet(otherOwner, "MATIC", "synthetic-matic-other-network", 3)
		connect.AssertEqual(t, SetPayoutWallet(ctx, taoNetworkId, taoPayoutWalletId), nil)
		result = removeWallet(taoOwner, taoPayoutWalletId)
		assertPayoutWallet("only bittensor", taoNetworkId, result, nil)
	})
}
