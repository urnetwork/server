// Signed synthetic policies exercise denomination authority without real keys.
package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/urfoundation/sn/v2026/nativefee"
)

// Synthetic reviewed units exercise admission; the factor is test authority,
// not a production runtime conversion or a replacement for original replay.
func stNativeFeeDenominationFixture(tb testing.TB) (*StNativeFeeDenominationPolicy, *StNativeFeeDenominationAuthority, ed25519.PrivateKey) {
	tb.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x68}, ed25519.SeedSize))
	hash := "sha256:" + strings.Repeat("a", 64)
	policy := &StNativeFeeDenominationPolicy{Schema: StNativeFeeDenominationSchema, Profile: "testnet", ChainId: 945, GenesisHash: "0x" + strings.Repeat("b", 64), NoId: 7,
		NativeAuthority:   nativefee.Authority{Verifier: nativefee.Reference{Path: "/synthetic/native-fee-verifier", Sha256: hash}, NativePolicy: nativefee.NativePolicy{ApprovalPublicKey: "0x" + strings.Repeat("c", 64), Genesis: "0x" + strings.Repeat("b", 64), EvmChainId: 945, EngineSha256: hash, CheckpointSha256: hash, ReviewSha256: hash, ProfileSha256: hash}},
		RuntimeCodeSha256: hash, FirstNativeBlock: 100, LastNativeBlock: 200, NativeUnit: "tao-rao", BudgetUnit: "evm-wei", WeiPerRaoNumerator: "10", WeiPerRaoDenominator: "1"}
	authority := &StNativeFeeDenominationAuthority{Schema: StNativeFeeDenominationAuthoritySchema, Profile: policy.Profile, ChainId: policy.ChainId, GenesisHash: policy.GenesisHash, NoId: policy.NoId, ApproverPublicKey: hex.EncodeToString(key.Public().(ed25519.PublicKey))}
	stNativeFeeDenominationSeal(tb, policy, authority, key)
	return policy, authority, key
}

// All modified authorities are freshly signed only where the test says so.
func stNativeFeeDenominationSeal(tb testing.TB, policy *StNativeFeeDenominationPolicy, authority *StNativeFeeDenominationAuthority, key ed25519.PrivateKey) {
	tb.Helper()
	message, err := policy.SigningBytes()
	if err != nil {
		tb.Fatal(err)
	}
	policy.Signature = hex.EncodeToString(ed25519.Sign(key, message))
	authority.PolicySha256, err = policy.Digest()
	if err != nil {
		tb.Fatal(err)
	}
}

func TestStNativeFeeDenominationPinsOriginalVerifierAndRatio(t *testing.T) {
	policy, authority, key := stNativeFeeDenominationFixture(t)
	if err := policy.Verify(authority); err != nil {
		t.Fatal(err)
	}
	invalidSignature := *policy
	invalidSignature.Signature = strings.Repeat("1", 128)
	if err := invalidSignature.Verify(authority); err == nil {
		t.Fatal("invalid denomination signature gained authority through a matching digest")
	}
	changes := []func(*StNativeFeeDenominationPolicy){
		func(value *StNativeFeeDenominationPolicy) { value.WeiPerRaoNumerator = "11" },
		func(value *StNativeFeeDenominationPolicy) {
			value.NativeAuthority.Verifier.Sha256 = "sha256:" + strings.Repeat("d", 64)
		},
		func(value *StNativeFeeDenominationPolicy) {
			value.NativeAuthority.NativePolicy.EngineSha256 = "sha256:" + strings.Repeat("d", 64)
		},
		func(value *StNativeFeeDenominationPolicy) {
			value.NativeAuthority.NativePolicy.CheckpointSha256 = "sha256:" + strings.Repeat("d", 64)
		},
		func(value *StNativeFeeDenominationPolicy) {
			value.RuntimeCodeSha256 = "sha256:" + strings.Repeat("d", 64)
		},
		func(value *StNativeFeeDenominationPolicy) { value.LastNativeBlock++ },
	}
	for index, change := range changes {
		changed := *policy
		change(&changed)
		if err := changed.Verify(authority); err == nil {
			t.Fatalf("unsigned denomination change %d gained authority", index)
		}
		newAuthority := *authority
		stNativeFeeDenominationSeal(t, &changed, &newAuthority, key)
		if err := changed.Verify(authority); err == nil {
			t.Fatalf("unselected signed denomination change %d gained authority", index)
		}
		if err := changed.Verify(&newAuthority); err != nil {
			t.Fatalf("explicitly selected denomination change %d refused: %v", index, err)
		}
	}
}

func TestStNativeFeeDenominationRequiresExactExplicitUnits(t *testing.T) {
	policy, _, _ := stNativeFeeDenominationFixture(t)
	for _, raw := range []string{"", "-1", "+1", "01", "1.0", "1e2", "18446744073709551616"} {
		if _, err := policy.DebitWei(raw); err == nil {
			t.Fatalf("invalid original native amount %q gained a budget value", raw)
		}
	}
	for _, raw := range []string{"0", "1", "18446744073709551615"} {
		actual, err := policy.DebitWei(raw)
		value, _ := new(big.Int).SetString(raw, 10)
		if err != nil || actual.Cmp(value.Mul(value, big.NewInt(10))) != 0 {
			t.Fatalf("exact native amount %q changed: %v %v", raw, actual, err)
		}
	}
	policy.WeiPerRaoNumerator, policy.WeiPerRaoDenominator = "1", "2"
	if _, err := policy.DebitWei("1"); err == nil {
		t.Fatal("fractional wei silently released native fee liability")
	}
	if exact, err := policy.DebitWei("2"); err != nil || exact.String() != "1" {
		t.Fatal("exact approved fractional ratio failed", exact, err)
	}
}

func TestStNativeFeeDenominationRejectsMissingRatioAndOverflow(t *testing.T) {
	policy, _, _ := stNativeFeeDenominationFixture(t)
	for _, pair := range [][2]string{{"", "1"}, {"1", ""}, {"0", "1"}, {"1", "0"}, {"2", "2"}, {"01", "1"}} {
		changed := *policy
		changed.WeiPerRaoNumerator, changed.WeiPerRaoDenominator = pair[0], pair[1]
		if err := changed.Validate(); err == nil {
			t.Fatalf("absent, noncanonical or unreduced denomination admitted: %v", pair)
		}
	}
	policy.WeiPerRaoNumerator = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)).String()
	if _, err := policy.DebitWei("2"); err == nil {
		t.Fatal("native denomination overflow truncated actual fee")
	}
	policy.NativeUnit = "alpha"
	if err := policy.Validate(); err == nil {
		t.Fatal("principal alpha was admitted as a native gas fee")
	}
}

func TestStNativeFeeDenominationAuthorityRejectsExtraDocument(t *testing.T) {
	policy, authority, _ := stNativeFeeDenominationFixture(t)
	encoded := "schema: " + authority.Schema + "\nprofile: testnet\nchain_id: 945\ngenesis_hash: " + policy.GenesisHash + "\nno_id: 7\napprover_public_key: \"" + authority.ApproverPublicKey + "\"\npolicy_sha256: \"" + authority.PolicySha256 + "\"\n"
	if parsed, err := ParseStNativeFeeDenominationAuthority([]byte(encoded)); err != nil || policy.Verify(parsed) != nil {
		t.Fatal("original protected authority could not be read", err)
	}
	for _, suffix := range []string{"\nunknown_credit: true\n", "\n---\n{}\n"} {
		if _, err := ParseStNativeFeeDenominationAuthority([]byte(encoded + suffix)); err == nil {
			t.Fatal("extra denomination authority content was ignored")
		}
	}
}

func TestStNativeFeeDenominationPolicyRejectsIgnoredCreditFields(t *testing.T) {
	policy, authority, _ := stNativeFeeDenominationFixture(t)
	encoded, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := ParseStNativeFeeDenominationPolicy(encoded); err != nil || parsed.Verify(authority) != nil {
		t.Fatal("original signed denomination policy could not be read", err)
	}
	foreign := append([]byte(`{"unknown_credit":true,`), encoded[1:]...)
	if _, err := ParseStNativeFeeDenominationPolicy(foreign); err == nil {
		t.Fatal("unknown signed-policy credit field was ignored")
	}
	if _, err := ParseStNativeFeeDenominationPolicy(append(bytes.Clone(encoded), []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("second signed-policy document was ignored")
	}
}
