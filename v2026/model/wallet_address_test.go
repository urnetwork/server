// The address rule of each chain and the reason each refusal gives, without a
// database. Keys and addresses are synthetic, derived from fixed seeds, or
// well known non-wallet values (the zero key, the USDC mint and the substrate
// development key).
package model

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateWalletAddressByChain(t *testing.T) {
	solanaAddress := payoutWalletTestSolana("rule")
	polygonAddress := payoutWalletTestPolygon("rule")
	// the substrate development key at the Bittensor prefix 42 and at the
	// Polkadot prefix 0
	bittensorAddress := "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"
	polkadotAddress := "15oF4uVJwmo4TdGW7VfQxNLavjCXviqxT9S1MgbjMNHr6Sp5"
	// every letter of the hex part in the other case: mixed case, not the
	// checksum
	mistyped := "0x" + strings.Map(func(r rune) rune {
		switch {
		case 'a' <= r && r <= 'f':
			return r - 'a' + 'A'
		case 'A' <= r && r <= 'F':
			return r - 'A' + 'a'
		default:
			return r
		}
	}, polygonAddress[2:])
	for _, c := range []struct {
		chain   Blockchain
		address string
		// empty when valid
		reason string
	}{
		{chain: SOL, address: solanaAddress},
		{chain: SOL, address: polygonAddress, reason: "this is a Polygon or Ethereum (0x) address, not a Solana address"},
		{chain: SOL, address: bittensorAddress, reason: "this is a Bittensor address, not a Solana address"},
		{chain: SOL, address: polkadotAddress, reason: "a Solana address is a base58 public key of 32 bytes"},
		{chain: SOL, address: "11111111111111111111111111111111", reason: "the all-zero key is not a Solana wallet"},
		{chain: SOL, address: SolanaUsdcMint, reason: "this is the USDC token mint, not a Solana wallet"},
		// base58 that decodes to 33 bytes
		{chain: SOL, address: strings.Repeat("z", 45), reason: "a Solana address is a base58 public key of 32 bytes"},
		{chain: SOL, address: "", reason: "the address is empty"},
		{chain: SOL, address: " " + solanaAddress, reason: "the address has spaces around it"},

		{chain: MATIC, address: polygonAddress},
		{chain: MATIC, address: strings.ToLower(polygonAddress)},
		{chain: MATIC, address: "0x" + strings.ToUpper(polygonAddress[2:])},
		{chain: MATIC, address: "0X" + polygonAddress[2:]},
		{chain: MATIC, address: mistyped, reason: "the mixed-case checksum of this Polygon address does not match; check it for a typo"},
		{chain: MATIC, address: solanaAddress, reason: "this is a Solana address, not a Polygon address"},
		{chain: MATIC, address: bittensorAddress, reason: "this is a Bittensor address, not a Polygon address"},
		{chain: MATIC, address: polygonAddress[2:], reason: "a Polygon address is 0x followed by 40 hex characters"},
		{chain: MATIC, address: polygonAddress + "0", reason: "a Polygon address is 0x followed by 40 hex characters"},
		{chain: MATIC, address: "0x" + strings.Repeat("0", 40), reason: "the zero address is not a Polygon wallet"},
		{chain: ETHEREUM, address: polygonAddress},
		{chain: ETHEREUM, address: mistyped, reason: "the mixed-case checksum of this Ethereum address does not match; check it for a typo"},
		{chain: ETHEREUM, address: solanaAddress, reason: "this is a Solana address, not an Ethereum address"},

		{chain: TAO, address: bittensorAddress},
		{chain: TAO, address: polkadotAddress, reason: "a Bittensor address is an ss58 address with prefix 42, starting with 5"},
		{chain: TAO, address: solanaAddress, reason: "this is a Solana address, not a Bittensor address"},
		{chain: TAO, address: polygonAddress, reason: "this is a Polygon or Ethereum (0x) address, not a Bittensor address"},
	} {
		err := ValidateWalletAddress(c.chain, c.address)
		if c.reason == "" {
			if err != nil {
				t.Errorf("%s %q: %v, want valid", c.chain, c.address, err)
			}
			continue
		}
		want := "400 invalid wallet address: " + c.reason
		if err == nil || err.Error() != want || !errors.Is(err, ErrInvalidWalletAddress) {
			t.Errorf("%s %q: %v, want %q", c.chain, c.address, err, want)
		}
	}
}

// Only a payout chain is judged by its address; the chain rule refuses a
// Bittensor payout wallet, and a chain that does not parse is not judged.
func TestPayoutWalletAddressValid(t *testing.T) {
	for _, c := range []struct {
		blockchain string
		address    string
		valid      bool
	}{
		{blockchain: "SOL", address: payoutWalletTestSolana("payout"), valid: true},
		{blockchain: "solana", address: payoutWalletTestPolygon("payout"), valid: false},
		{blockchain: "MATIC", address: payoutWalletTestPolygon("payout"), valid: true},
		{blockchain: "polygon", address: payoutWalletTestSolana("payout"), valid: false},
		{blockchain: "ETH", address: payoutWalletTestSolana("payout"), valid: false},
		{blockchain: "TAO", address: "synthetic-tao-address", valid: true},
		{blockchain: "DOGE", address: "synthetic-address", valid: true},
	} {
		if valid := PayoutWalletAddressValid(c.blockchain, c.address); valid != c.valid {
			t.Errorf("%s %q: %v, want %v", c.blockchain, c.address, valid, c.valid)
		}
	}
}

func TestSameBlockchainParsesStoredNames(t *testing.T) {
	for _, c := range []struct {
		a    string
		b    string
		same bool
	}{
		{a: "SOL", b: "solana", same: true},
		{a: "MATIC", b: "poly", same: true},
		{a: "MATIC", b: "SOL", same: false},
		{a: "ETH", b: "MATIC", same: false},
		{a: "DOGE", b: "DOGE", same: true},
		{a: "DOGE", b: "doge", same: false},
		{a: "DOGE", b: "SOL", same: false},
	} {
		if same := sameBlockchain(c.a, c.b); same != c.same {
			t.Errorf("%q %q: %v, want %v", c.a, c.b, same, c.same)
		}
	}
}
