// The independent consumer and the existing immutable database boundary must
// commit the same earning policy without binding mutable deployment readiness.
package server

import (
	"testing"
	"time"

	"github.com/urfoundation/sn/payoutartifact"
)

// This compares two separately implemented canonical identity encoders. A
// changed asset boundary must change the pin; a readiness update must not.
func TestProviderPayoutEarningIdentityMatchesIndependentConsumer(t *testing.T) {
	raw, _ := payoutTransitionFixture(t)
	policy, err := ParseProviderPayoutTransition(raw)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := payoutartifact.NewWholeWorkEarningSelection(payoutartifact.WholeWorkEarningIdentity{
		Schema: policy.Schema, CutoffUtc: policy.Cutoff.UTC().Format(time.RFC3339Nano), Attribution: policy.Attribution,
		LegacyUsdc: policy.LegacyUsdc, Profile: policy.Mainnet.Profile, ChainId: policy.Mainnet.ChainId,
		GenesisHash: policy.Mainnet.GenesisHash, Netuid: policy.Mainnet.Netuid,
	})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := policy.EarningIdentitySha256()
	if err != nil || selection.PolicyHash != "sha256:"+digest || !selection.StartTime.Equal(policy.Cutoff) {
		t.Fatal("independent earning selection differs from original database identity", err)
	}
	policy.Mainnet.Activation = "blocked"
	policy.Mainnet.ReadinessSha256 = ""
	policy.ConfigSha256 = "changed-full-config-audit"
	if after, err := policy.EarningIdentitySha256(); err != nil || after != digest {
		t.Fatal("readiness-only update changed original earning selection", err)
	}
	policy.Cutoff = policy.Cutoff.Add(time.Nanosecond)
	policy.CutoffUtc = policy.Cutoff.Format(time.RFC3339Nano)
	if after, err := policy.EarningIdentitySha256(); err != nil || after == digest {
		t.Fatal("changed cutoff retained original earning-selection authority", err)
	}
}
