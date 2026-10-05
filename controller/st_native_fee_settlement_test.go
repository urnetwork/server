package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urfoundation/sn/nativefee"
	"github.com/urnetwork/server"
)

func nativeFeeDenominationFixture(t *testing.T) (*StConfig, map[string][]byte) {
	t.Helper()
	var genesis [32]byte
	copy(genesis[:], bytes.Repeat([]byte{0x34}, 32))
	cfg := &StConfig{Profile: "mainnet", ChainId: 964, GenesisHash: genesis, NoId: 7}
	hash := "sha256:" + strings.Repeat("35", 32)
	policy := server.StNativeFeeDenominationPolicy{Schema: server.StNativeFeeDenominationSchema, Profile: cfg.Profile, ChainId: cfg.ChainId, GenesisHash: fmt.Sprintf("0x%x", cfg.GenesisHash), NoId: cfg.NoId,
		NativeAuthority:   nativefee.Authority{Verifier: nativefee.Reference{Path: "/approved/sn-mainnet", Sha256: hash}, NativePolicy: nativefee.NativePolicy{ApprovalPublicKey: "0x" + strings.Repeat("36", 32), Genesis: fmt.Sprintf("0x%x", cfg.GenesisHash), EvmChainId: cfg.ChainId, EngineSha256: hash, CheckpointSha256: hash, ReviewSha256: hash, ProfileSha256: hash}},
		RuntimeCodeSha256: hash, FirstNativeBlock: 1, LastNativeBlock: 100, NativeUnit: "tao-rao", BudgetUnit: "evm-wei", WeiPerRaoNumerator: "1", WeiPerRaoDenominator: "1"}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x37}, 32))
	message, err := policy.SigningBytes()
	if err != nil {
		t.Fatal(err)
	}
	policy.Signature = hex.EncodeToString(ed25519.Sign(key, message))
	digest, err := policy.Digest()
	if err != nil {
		t.Fatal(err)
	}
	authority := server.StNativeFeeDenominationAuthority{Schema: server.StNativeFeeDenominationAuthoritySchema, Profile: cfg.Profile, ChainId: cfg.ChainId, GenesisHash: policy.GenesisHash, NoId: cfg.NoId, ApproverPublicKey: hex.EncodeToString(key.Public().(ed25519.PublicKey)), PolicySha256: digest}
	policyBytes, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	authorityBytes, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, map[string][]byte{"native-fee-denomination-policy.yml": policyBytes, "native-fee-denomination-authority.yml": authorityBytes}
}

func TestNativeFeeControllerLoadsIndependentPolicyAndDeployment(t *testing.T) {
	cfg, resources := nativeFeeDenominationFixture(t)
	var reads []string
	policy, authority, err := loadNativeFeeDenomination(t.Context(), cfg, func(_ context.Context, name string) ([]byte, error) {
		reads = append(reads, name)
		return resources[name], nil
	})
	if err != nil || policy == nil || authority == nil || len(reads) != 2 || reads[0] != "native-fee-denomination-authority.yml" {
		t.Fatal("independent policy admission failed", err, reads)
	}
	if err := policy.Verify(authority); err != nil {
		t.Fatal(err)
	}
}

func TestNativeFeeControllerMissingAuthorityCannotChoosePolicy(t *testing.T) {
	cfg, _ := nativeFeeDenominationFixture(t)
	readCount := 0
	absent := errors.New("independent authority absent")
	policy, authority, err := loadNativeFeeDenomination(t.Context(), cfg, func(_ context.Context, name string) ([]byte, error) { readCount++; return nil, absent })
	if !errors.Is(err, absent) || policy != nil || authority != nil || readCount != 1 {
		t.Fatal("missing independent key selected a policy", err)
	}
}

func TestNativeFeeControllerPublicAuthoritySelectsScopeWithoutSignerConfig(t *testing.T) {
	_, resources := nativeFeeDenominationFixture(t)
	policy, authority, err := loadNativeFeeDenomination(t.Context(), nil, func(_ context.Context, name string) ([]byte, error) { return resources[name], nil })
	if err != nil || policy == nil || authority == nil || policy.NoId != 7 || policy.GenesisHash != authority.GenesisHash {
		t.Fatal("protected public scope required signer configuration", err)
	}
	if err := policy.Verify(authority); err != nil {
		t.Fatal(err)
	}
}

func TestNativeFeeControllerRejectsDifferentDeployment(t *testing.T) {
	cfg, resources := nativeFeeDenominationFixture(t)
	cfg.NoId++
	policy, authority, err := loadNativeFeeDenomination(t.Context(), cfg, func(_ context.Context, name string) ([]byte, error) { return resources[name], nil })
	if err == nil || policy != nil || authority != nil {
		t.Fatal("different operator consumed fee denomination authority")
	}
}

func TestNativeFeeControllerCancellationAfterConfigCannotInvoke(t *testing.T) {
	cfg, resources := nativeFeeDenominationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	policy, authority, err := loadNativeFeeDenomination(ctx, cfg, func(_ context.Context, name string) ([]byte, error) {
		if name == "native-fee-denomination-policy.yml" {
			cancel()
		}
		return resources[name], nil
	})
	if !errors.Is(err, context.Canceled) || policy != nil || authority != nil {
		t.Fatal("canceled owner admitted execution authority", err)
	}
}
