// A payout wallet is promoted only when its address is an address on its
// chain. The payout review found a Solana key saved as a Polygon (MATIC)
// payout wallet before addresses were validated (refs support inbox 8045).
// Keys and addresses are synthetic, derived from fixed seeds.
package model

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/gagliardetto/solana-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A Solana public key derived from the seed.
func payoutWalletTestSolana(seed string) string {
	digest := sha256.Sum256([]byte("synthetic solana wallet/" + seed))
	return solana.PublicKeyFromBytes(digest[:]).String()
}

// An EIP-55 checksummed EVM address derived from the seed.
func payoutWalletTestPolygon(seed string) string {
	digest := sha256.Sum256([]byte("synthetic polygon wallet/" + seed))
	return common.BytesToAddress(digest[:20]).Hex()
}

// An address on the chain derived from the seed: a Solana key for SOL, an EVM
// address for MATIC and ETHEREUM, and the seed itself for any other chain.
func payoutWalletTestAddress(blockchain string, seed string) string {
	chain, err := ParseBlockchain(blockchain)
	switch {
	case err != nil:
		return seed
	case chain == SOL:
		return payoutWalletTestSolana(seed)
	case chain == MATIC || chain == ETHEREUM:
		return payoutWalletTestPolygon(seed)
	default:
		return seed
	}
}

// Removing the payout wallet never promotes a wallet whose address is not on
// its chain, even when it is the newest wallet on the removed wallet's chain:
// an older wallet on its chain takes over, and with none left the network has
// no payout wallet and its payouts are held.
func TestRemoveWalletDoesNotPromoteAWalletWhoseAddressIsNotOnItsChain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		startTime := server.NowUtc().Add(-time.Hour)

		newNetwork := func() (server.Id, *session.ClientSession) {
			networkId := server.NewId()
			return networkId, session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId})
		}
		// written without validation, the way the legacy rows were; created
		// `minute` minutes after startTime, so newest is explicit
		addWallet := func(clientSession *session.ClientSession, blockchain string, address string, minute int) server.Id {
			walletId := CreateAccountWalletExternal(clientSession, &CreateAccountWalletExternalArgs{
				NetworkId:        clientSession.ByJwt.NetworkId,
				Blockchain:       blockchain,
				WalletAddress:    address,
				DefaultTokenType: "USDC",
			})
			if walletId == nil {
				tb.Fatalf("wallet %s was not created", address)
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

		// the newest Polygon wallet holds a Solana key: the older Solana
		// wallet takes over
		networkId, owner := newNetwork()
		payoutWalletId := addWallet(owner, "MATIC", payoutWalletTestPolygon("payout"), 1)
		solanaWalletId := addWallet(owner, "SOL", payoutWalletTestSolana("older"), 2)
		legacyWalletId := addWallet(owner, "MATIC", payoutWalletTestSolana("legacy"), 3)
		if err := SetPayoutWallet(ctx, networkId, payoutWalletId); err != nil {
			tb.Fatal(err)
		}
		result := RemoveWallet(payoutWalletId, owner)
		if !result.Success {
			tb.Fatal("the payout wallet was not removed", result)
		}
		if result.PayoutWalletId != nil && *result.PayoutWalletId == legacyWalletId {
			tb.Fatal("a Polygon wallet holding a Solana key was promoted")
		}
		if result.PayoutWalletId == nil || *result.PayoutWalletId != solanaWalletId {
			tb.Fatal("the wallet on its chain was not promoted", result.PayoutWalletId)
		}
		if selected := GetPayoutWalletId(ctx, networkId); selected == nil || *selected != solanaWalletId {
			tb.Fatal("the payout wallet is not the promoted wallet", selected)
		}

		// only wallets whose address is not on their chain are left: none
		// is promoted
		onlyLegacyNetworkId, onlyLegacyOwner := newNetwork()
		onlyLegacyPayoutWalletId := addWallet(onlyLegacyOwner, "SOL", payoutWalletTestSolana("only legacy payout"), 1)
		addWallet(onlyLegacyOwner, "SOL", payoutWalletTestPolygon("only legacy"), 2)
		addWallet(onlyLegacyOwner, "MATIC", payoutWalletTestSolana("only legacy"), 3)
		if err := SetPayoutWallet(ctx, onlyLegacyNetworkId, onlyLegacyPayoutWalletId); err != nil {
			tb.Fatal(err)
		}
		result = RemoveWallet(onlyLegacyPayoutWalletId, onlyLegacyOwner)
		if !result.Success {
			tb.Fatal("the payout wallet was not removed", result)
		}
		if result.PayoutWalletId != nil || GetPayoutWalletId(ctx, onlyLegacyNetworkId) != nil {
			tb.Fatal("a wallet whose address is not on its chain was promoted", result.PayoutWalletId)
		}
	})
}
