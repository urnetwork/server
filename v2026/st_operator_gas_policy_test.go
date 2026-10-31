package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func stGasPolicyTestApproval(t testing.TB) (*StOperatorGasPolicy, *StOperatorGasAuthority) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x63}, ed25519.SeedSize))
	p := &StOperatorGasPolicy{Schema: StOperatorGasPolicySchema, Profile: "testnet", ChainId: 945, GenesisHash: "0x" + strings.Repeat("1", 64), NoId: 1, Coordinator: "0x" + strings.Repeat("2", 40), PolicyHash: "0x" + strings.Repeat("3", 64), Accounts: []StOperatorGasPolicyAccount{{Role: "deposit", Address: "0x" + strings.Repeat("4", 40), InitialHistorySha256: strings.Repeat("5", 64)}, {Role: "root", Address: "0x" + strings.Repeat("6", 40), InitialHistorySha256: strings.Repeat("7", 64)}}, ValidFrom: 100, ValidUntil: 200, MaximumGas: 60_000, MaximumFeePerGasWei: "10", MaximumTipPerGasWei: "2", MaximumIntentLiabilityWei: "600000", MaximumLifetimeLiabilityWei: "1200000", MaximumIntentAttempts: 4, MaximumLifetimeAttempts: 8}
	encoded, err := p.SigningBytes()
	if err != nil {
		t.Fatal(err)
	}
	p.Signature = hex.EncodeToString(ed25519.Sign(key, encoded))
	digest, _ := p.Digest()
	return p, &StOperatorGasAuthority{Schema: StOperatorGasAuthoritySchema, Profile: p.Profile, ChainId: p.ChainId, GenesisHash: p.GenesisHash, NoId: p.NoId, ApproverPublicKey: hex.EncodeToString(key.Public().(ed25519.PublicKey)), PolicySha256: digest}
}

func TestStOperatorGasPolicyRequiresIndependentExactApproval(t *testing.T) {
	p, a := stGasPolicyTestApproval(t)
	if err := p.Verify(a, time.Unix(150, 0)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"authority", "policy", "signature", "chain"} {
		candidate := p.Clone()
		authority := *a
		switch kind {
		case "authority":
			authority.ApproverPublicKey = strings.Repeat("8", 64)
		case "policy":
			candidate.MaximumLifetimeLiabilityWei = "2400000"
		case "signature":
			candidate.Signature = strings.Repeat("00", 64)
		case "chain":
			authority.ChainId = 964
		}
		if err := candidate.Verify(&authority, time.Unix(150, 0)); err == nil {
			t.Fatalf("%s borrowed independent gas approval", kind)
		}
	}
}

func TestStOperatorGasPolicyHasNoDefaultFinancialAuthority(t *testing.T) {
	p, _ := stGasPolicyTestApproval(t)
	for _, value := range []string{"", "0", "-1", "01", "1.0", "1e6", "115792089237316195423570985008687907853269984665640564039457584007913129639936"} {
		candidate := p.Clone()
		candidate.MaximumLifetimeLiabilityWei = value
		if candidate.Validate() == nil {
			t.Fatalf("invalid explicit wei allowance %q accepted", value)
		}
	}
	candidate := p.Clone()
	candidate.MaximumLifetimeAttempts = 0
	if candidate.Validate() == nil {
		t.Fatal("missing finite lifetime count gained a default")
	}
	candidate = p.Clone()
	candidate.Revision = 1
	if candidate.Validate() == nil {
		t.Fatal("successor policy has no original predecessor")
	}
}

func TestStOperatorGasPolicySigningWindowAndHistoricalCensusAreExact(t *testing.T) {
	p, a := stGasPolicyTestApproval(t)
	for _, value := range []int64{100, 199} {
		if err := p.Verify(a, time.Unix(value, 0)); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []int64{99, 200} {
		if p.Verify(a, time.Unix(value, 0)) == nil {
			t.Fatal("signing authority escaped its half-open interval")
		}
	}
	candidate := p.Clone()
	candidate.HistoricalAccounts = []StOperatorGasPolicyAccount{{Role: "retired_deposit", Address: "0x" + strings.Repeat("9", 40), InitialHistorySha256: strings.Repeat("a", 64)}}
	if candidate.Verify(a, time.Unix(150, 0)) == nil {
		t.Fatal("unapproved retired-account census reused original signature")
	}
	candidate = p.Clone()
	candidate.Accounts[0].InitialHistorySha256 = strings.Repeat("b", 64)
	if candidate.Verify(a, time.Unix(150, 0)) == nil {
		t.Fatal("changed original history borrowed approved census")
	}
	if p.Accounts[0].InitialHistorySha256 == candidate.Accounts[0].InitialHistorySha256 {
		t.Fatal("policy clone shares mutable account authority")
	}
}
