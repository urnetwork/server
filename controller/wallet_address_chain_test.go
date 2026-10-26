// A wallet's address must be an address on the wallet's chain when it is
// added and when it is selected for payouts. The payout review found a Solana
// key saved as a Polygon (MATIC) payout wallet before addresses were
// validated (refs support inbox 8045). Keys and addresses are synthetic,
// derived from fixed seeds.
package controller

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"
	"unicode"

	"github.com/ethereum/go-ethereum/common"
	"github.com/gagliardetto/solana-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A Solana public key derived from the seed.
func walletAddressTestSolana(seed string) string {
	digest := sha256.Sum256([]byte("synthetic solana wallet/" + seed))
	return solana.PublicKeyFromBytes(digest[:]).String()
}

// An EIP-55 checksummed EVM address derived from the seed.
func walletAddressTestPolygon(seed string) string {
	digest := sha256.Sum256([]byte("synthetic polygon wallet/" + seed))
	return common.BytesToAddress(digest[:20]).Hex()
}

// An EVM address in mixed case carries its EIP-55 checksum, so a letter typed
// in the wrong case is refused. An address in one case throughout carries no
// checksum and is accepted.
func TestWalletValidateAddressRequiresTheChecksumOfAMixedCaseEvmAddress(t *testing.T) {
	checksummed := walletAddressTestPolygon("checksum")
	hexPart := checksummed[2:]
	if strings.ToLower(hexPart) == hexPart || strings.ToUpper(hexPart) == hexPart {
		t.Fatal("the fixture address is not in mixed case", checksummed)
	}
	// the first lowercase letter in upper case, which keeps the address in
	// mixed case
	index := strings.IndexFunc(hexPart, unicode.IsLower)
	mistyped := "0x" + hexPart[:index] + strings.ToUpper(hexPart[index:index+1]) + hexPart[index+1:]
	if strings.ToUpper(mistyped[2:]) == mistyped[2:] {
		t.Fatal("the mistyped fixture lost its mixed case", mistyped)
	}
	for _, c := range []struct {
		chain   string
		address string
		valid   bool
	}{
		{chain: "MATIC", address: checksummed, valid: true},
		{chain: "MATIC", address: "0x" + strings.ToLower(hexPart), valid: true},
		{chain: "MATIC", address: "0x" + strings.ToUpper(hexPart), valid: true},
		{chain: "MATIC", address: mistyped, valid: false},
		{chain: "ETHEREUM", address: mistyped, valid: false},
	} {
		result, err := WalletValidateAddress(&WalletValidateAddressArgs{Address: c.address, Chain: c.chain}, nil)
		if err != nil || result.Valid != c.valid {
			t.Errorf("%s %s: valid %v (%v), want %v", c.chain, c.address, result != nil && result.Valid, err, c.valid)
		}
	}
}

// The refusal of an address that is not on the requested chain says what the
// address is, or what an address on the chain looks like, in a 400 the apps
// show as the reason.
func TestCreateAccountWalletRefusalNamesWhatTheAddressIs(t *testing.T) {
	clientSession := session.Testing_CreateClientSession(context.Background(), &session.ByJwt{NetworkId: server.NewId()})
	defer clientSession.Cancel()
	solanaAddress := walletAddressTestSolana("refusal")
	polygonAddress := walletAddressTestPolygon("refusal")
	for _, c := range []struct {
		chain   string
		address string
		message string
	}{
		{chain: "MATIC", address: solanaAddress, message: "invalid wallet address: this is a Solana address, not a Polygon address"},
		{chain: "SOL", address: polygonAddress, message: "invalid wallet address: this is a Polygon or Ethereum (0x) address, not a Solana address"},
		{chain: "TAO", address: solanaAddress, message: "invalid wallet address: this is a Solana address, not a Bittensor address"},
		{chain: "SOL", address: "1234", message: "invalid wallet address: a Solana address is a base58 public key of 32 bytes"},
		{chain: "MATIC", address: "0x1234", message: "invalid wallet address: a Polygon address is 0x followed by 40 hex characters"},
		{chain: "TAO", address: polygonAddress, message: "invalid wallet address: this is a Polygon or Ethereum (0x) address, not a Bittensor address"},
	} {
		_, err := CreateAccountWalletExternal(&model.CreateAccountWalletExternalArgs{Blockchain: c.chain, WalletAddress: c.address, DefaultTokenType: "USDC"}, clientSession)
		requireRefusalAnswer(t, err, http.StatusBadRequest, c.message)
	}
}

// A Solana key saved as a Polygon wallet stays as it is: adding the key as a
// Solana wallet creates the Solana wallet. It never returns the Polygon row,
// or reactivates it after the network removed it, which kept the key on the
// wrong chain while the request reported success. A mistyped checksum is
// refused before anything is written.
func TestCreateAccountWalletOnItsChainDoesNotReuseAnotherChainsRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		networkId := server.NewId()
		owner := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId})
		defer owner.Cancel()
		solanaAddress := walletAddressTestSolana("legacy")
		// the legacy row, written without validation as before
		legacyId := model.CreateAccountWalletExternal(owner, &model.CreateAccountWalletExternalArgs{NetworkId: networkId, Blockchain: "MATIC", WalletAddress: solanaAddress, DefaultTokenType: "USDC"})
		if legacyId == nil {
			tb.Fatal("legacy wallet fixture was not created")
		}
		if err := model.SetPayoutWallet(ctx, networkId, *legacyId); err != nil {
			tb.Fatal(err)
		}

		args := func() *model.CreateAccountWalletExternalArgs {
			return &model.CreateAccountWalletExternalArgs{Blockchain: "SOL", WalletAddress: solanaAddress, DefaultTokenType: "USDC"}
		}
		created, err := CreateAccountWalletExternal(args(), owner)
		if err != nil {
			tb.Fatal(err)
		}
		if created.WalletId == *legacyId {
			tb.Fatal("adding the key as a Solana wallet returned the Polygon row")
		}
		if wallet := model.GetAccountWallet(ctx, created.WalletId); wallet == nil || wallet.Blockchain != "SOL" || wallet.WalletAddress != solanaAddress || !wallet.Active {
			tb.Fatal("the Solana wallet was not created", wallet)
		}
		if legacy := model.GetAccountWallet(ctx, *legacyId); legacy == nil || legacy.Blockchain != "MATIC" || !legacy.Active {
			tb.Fatal("the legacy row changed", legacy)
		}
		// create selects a payout wallet only when the network has none
		if payoutWalletId := model.GetPayoutWalletId(ctx, networkId); payoutWalletId == nil || *payoutWalletId != *legacyId {
			tb.Fatal("create moved the existing payout wallet", payoutWalletId)
		}
		// adding the key on its chain again returns the same wallet
		again, err := CreateAccountWalletExternal(args(), owner)
		if err != nil || again.WalletId != created.WalletId {
			tb.Fatal("adding the Solana wallet again did not return it", again, err)
		}

		// a removed legacy row is not reactivated by adding the key on its chain
		removedNetworkId := server.NewId()
		removedOwner := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: removedNetworkId})
		defer removedOwner.Cancel()
		removedLegacyId := model.CreateAccountWalletExternal(removedOwner, &model.CreateAccountWalletExternalArgs{NetworkId: removedNetworkId, Blockchain: "MATIC", WalletAddress: solanaAddress, DefaultTokenType: "USDC"})
		if removedLegacyId == nil {
			tb.Fatal("legacy wallet fixture was not created")
		}
		if result := model.RemoveWallet(*removedLegacyId, removedOwner); !result.Success {
			tb.Fatal("legacy wallet fixture was not removed")
		}
		readded, err := CreateAccountWalletExternal(args(), removedOwner)
		if err != nil {
			tb.Fatal(err)
		}
		if readded.WalletId == *removedLegacyId {
			tb.Fatal("adding the key as a Solana wallet reactivated the removed Polygon row")
		}
		if legacy := model.GetAccountWallet(ctx, *removedLegacyId); legacy == nil || legacy.Blockchain != "MATIC" || legacy.Active {
			tb.Fatal("the removed legacy row changed", legacy)
		}
		if wallet := model.GetAccountWallet(ctx, readded.WalletId); wallet == nil || wallet.Blockchain != "SOL" || !wallet.Active {
			tb.Fatal("the Solana wallet was not created", wallet)
		}
		if payoutWalletId := model.GetPayoutWalletId(ctx, removedNetworkId); payoutWalletId == nil || *payoutWalletId != readded.WalletId {
			tb.Fatal("the new Solana wallet did not become the payout wallet of a network without one", payoutWalletId)
		}

		// a mixed-case Polygon address with a mistyped checksum writes nothing
		polygonAddress := walletAddressTestPolygon("mistyped")
		if strings.ToLower(polygonAddress[2:]) == polygonAddress[2:] || strings.ToUpper(polygonAddress[2:]) == polygonAddress[2:] {
			tb.Fatal("the fixture address is not in mixed case", polygonAddress)
		}
		mistyped := "0x" + strings.Map(func(r rune) rune {
			if unicode.IsUpper(r) {
				return unicode.ToLower(r)
			}
			return unicode.ToUpper(r)
		}, polygonAddress[2:])
		before := len(model.GetActiveAccountWallets(owner).Wallets)
		_, err = CreateAccountWalletExternal(&model.CreateAccountWalletExternalArgs{Blockchain: "MATIC", WalletAddress: mistyped, DefaultTokenType: "USDC"}, owner)
		requireRefusalAnswer(tb, err, http.StatusBadRequest, "invalid wallet address: the mixed-case checksum of this Polygon address does not match; check it for a typo")
		if after := len(model.GetActiveAccountWallets(owner).Wallets); after != before {
			tb.Fatal("a refused address was stored", before, after)
		}
	})
}

// A wallet whose address is not on its chain cannot be selected as the payout
// wallet; the selection stays where it was, and a wallet on its chain can
// still be selected.
func TestSetPayoutWalletRefusesAWalletWhoseAddressIsNotOnItsChain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		networkId := server.NewId()
		owner := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId})
		defer owner.Cancel()
		// the legacy row, written without validation as before
		legacyId := model.CreateAccountWalletExternal(owner, &model.CreateAccountWalletExternalArgs{NetworkId: networkId, Blockchain: "MATIC", WalletAddress: walletAddressTestSolana("legacy payout"), DefaultTokenType: "USDC"})
		if legacyId == nil {
			tb.Fatal("legacy wallet fixture was not created")
		}
		solanaWallet, err := CreateAccountWalletExternal(&model.CreateAccountWalletExternalArgs{Blockchain: "SOL", WalletAddress: walletAddressTestSolana("payout"), DefaultTokenType: "USDC"}, owner)
		if err != nil {
			tb.Fatal(err)
		}
		polygonWallet, err := CreateAccountWalletExternal(&model.CreateAccountWalletExternalArgs{Blockchain: "MATIC", WalletAddress: walletAddressTestPolygon("payout"), DefaultTokenType: "USDC"}, owner)
		if err != nil {
			tb.Fatal(err)
		}
		if payoutWalletId := model.GetPayoutWalletId(ctx, networkId); payoutWalletId == nil || *payoutWalletId != solanaWallet.WalletId {
			tb.Fatal("the first valid wallet did not become the payout wallet", payoutWalletId)
		}

		_, err = SetPayoutWallet(model.SetPayoutWalletArgs{WalletId: *legacyId}, owner)
		requireRefusalAnswer(tb, err, http.StatusBadRequest, "The wallet's address is not an address on its blockchain, so it cannot be the payout wallet.")
		if payoutWalletId := model.GetPayoutWalletId(ctx, networkId); payoutWalletId == nil || *payoutWalletId != solanaWallet.WalletId {
			tb.Fatal("a refused selection moved the payout wallet", payoutWalletId)
		}

		if _, err := SetPayoutWallet(model.SetPayoutWalletArgs{WalletId: polygonWallet.WalletId}, owner); err != nil {
			tb.Fatal(err)
		}
		if payoutWalletId := model.GetPayoutWalletId(ctx, networkId); payoutWalletId == nil || *payoutWalletId != polygonWallet.WalletId {
			tb.Fatal("a Polygon wallet on its chain was not selected", payoutWalletId)
		}
	})
}
