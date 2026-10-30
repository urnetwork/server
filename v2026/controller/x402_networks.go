package controller

import (
	"fmt"
	"regexp"
)

// The networks x402 can quote, and the token each one settles in.
//
// x402 v1 quotes `asset` as the token's on-chain address, never a symbol: the
// EVM exact scheme signs an EIP-3009 authorization whose EIP-712 domain names
// the token contract, and the SVM exact scheme transfers to the associated
// token account of `payTo` for that mint (coinbase/x402
// specs/x402-specification-v1.md 5.1.2, specs/schemes/exact/scheme_exact_evm.md,
// specs/schemes/exact/scheme_exact_svm.md). x402.yml names the asset by symbol
// ("usdc"); this table is the only place a symbol becomes an address.
//
// A network is here only if all three legs carry it: the CDP facilitator
// settles it for x402 v1 (its X402V1Network enum is base, base-sepolia, solana
// and solana-devnet), Stripe's crypto transaction_verification records USDC on
// it (docs.stripe.com/payments/machine/x402, "Token and network support"),
// and Stripe issues a crypto deposit address on it. Testnets are left out on
// purpose: a production config that listed one would sell real goods for test
// tokens.
//
// A (network, asset) pair that is not in this table makes the whole config
// unusable (x402ConfigProblem), so an unknown pair is never quoted with a
// guessed address.

// x402 v1 network names, as the v1 transport and the CDP facilitator spell them.
const (
	X402NetworkBase   = "base"
	X402NetworkSolana = "solana"
)

// X402AssetUsdc is the one asset symbol x402.yml can name.
const X402AssetUsdc = "usdc"

type x402AddressKind int

const (
	// an EVM 0x address
	x402AddressEvm x402AddressKind = iota
	// a Solana base58 public key
	x402AddressSolana
)

type x402Token struct {
	addressKind x402AddressKind
	// The token contract (EVM) or mint (Solana) address. This is what the
	// terms quote as `asset`.
	address string
	// The EIP-712 domain of the token contract, which the EVM exact scheme
	// quotes as extra.name and extra.version so the client can sign the
	// EIP-3009 authorization against it. Empty for Solana.
	eip712Name    string
	eip712Version string
	// The SVM exact scheme quotes the facilitator's fee payer as
	// extra.feePayer. The client builds the transaction with that account as
	// its fee payer, so a term without it cannot be paid.
	feePayerRequired bool
}

var x402Tokens = map[string]map[string]x402Token{
	X402NetworkBase: {
		// USDC on Base mainnet (chain 8453). Its EIP-712 domain is
		// {name: "USD Coin", version: "2"}; the testnet USDC uses "USDC".
		// (coinbase/x402 go/legacy/pkg/types/types.go SetUSDCInfo and
		// typescript/packages/legacy/x402/src/types/shared/evm/config.ts)
		X402AssetUsdc: {
			addressKind:   x402AddressEvm,
			address:       "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913",
			eip712Name:    "USD Coin",
			eip712Version: "2",
		},
	},
	X402NetworkSolana: {
		// the USDC mint on Solana mainnet
		X402AssetUsdc: {
			addressKind:      x402AddressSolana,
			address:          "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
			feePayerRequired: true,
		},
	},
}

// Networks Stripe supports that x402 must not quote, and why. These get their
// own message because an operator copying Stripe's network list would
// otherwise only learn that the network is "not supported".
var x402ExcludedNetworks = map[string]string{
	"tempo": "Stripe takes Tempo payments through MPP, not x402, and the CDP facilitator does not settle Tempo",
}

// x402TokenFor is the token an asset symbol settles in on a network.
func x402TokenFor(network string, asset string) (x402Token, error) {
	if reason, excluded := x402ExcludedNetworks[network]; excluded {
		return x402Token{}, fmt.Errorf("network %s is not an x402 network: %s", network, reason)
	}
	assets, ok := x402Tokens[network]
	if !ok {
		return x402Token{}, fmt.Errorf("network %s is not an x402 network this server can quote (base, solana)", network)
	}
	token, ok := assets[asset]
	if !ok {
		return x402Token{}, fmt.Errorf("asset %q has no token address on network %s (usdc)", asset, network)
	}
	return token, nil
}

var (
	x402EvmAddressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	// base58, the length of a 32-byte public key
	x402SolanaAddressPattern = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,44}$`)
)

// x402AddressValid reports whether an address has the form its network uses,
// so a deposit address pasted under the wrong network is caught at load
// instead of being quoted to agents.
func x402AddressValid(kind x402AddressKind, address string) bool {
	switch kind {
	case x402AddressEvm:
		return x402EvmAddressPattern.MatchString(address)
	case x402AddressSolana:
		return x402SolanaAddressPattern.MatchString(address)
	default:
		return false
	}
}
