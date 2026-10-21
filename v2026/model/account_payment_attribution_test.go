// Cross-payment fences use physical destinations, not unrelated wallet rows.
package model

import "testing"

// Aliases share one key while case-sensitive mint-chain destinations do not.
func TestProviderPaymentAttributionCanonicalDestination(t *testing.T) {
	chain, address := providerPaymentDestination(" polygon ", "0x00aB")
	if chain != "MATIC" || address != "0x00ab" {
		t.Fatal("EVM destination was not canonical", chain, address)
	}
	chain, address = providerPaymentDestination("solana", "SyntheticAbC")
	if chain != "SOL" || address != "SyntheticAbC" {
		t.Fatal("Solana destination casing changed", chain, address)
	}
}
