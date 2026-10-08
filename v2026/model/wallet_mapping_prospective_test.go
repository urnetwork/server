// Actual immutable SQL custody checks the live prospective boundary before
// first acceptance while exact accepted retries preserve their original bytes.
package model

import (
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

// A concrete test-owned signing key represents the independently configured
// operator; the production controller obtains its boundary from actual RPC.
func walletMappingProspectiveFixture(t testing.TB) *walletMappingFixture {
	t.Helper()
	f := newWalletMappingFixture(t)
	key, err := crypto.HexToECDSA(strings.Repeat("42", 32))
	if err != nil {
		t.Fatal(err)
	}
	f.owner.Prospective = &WalletMappingProspectiveOwner{Boundary: protocol.ClientKeyEffectiveBoundary{Epoch: 50, Block: 500, Hash: [32]byte{51}}, RootKey: key}
	return f
}

// Backdated issuance and acceptance after the earning epoch begins leave no
// financial projection. A future original still commits both histories once.
func TestWalletMappingProspectiveSqlRefusesBackdatedAndDelayedConsent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := walletMappingProspectiveFixture(t)
		if message, err := CreateWalletMappingChallenge(t.Context(), f.owner, f.key.Public().Encode(), 1, 100); message != "" || !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("caller-selected historical epoch was issued", err)
		}
		original := f.challenge(t, 51)
		f.owner.Prospective.Boundary = protocol.ClientKeyEffectiveBoundary{Epoch: 51, Block: 510, Hash: [32]byte{52}}
		if accepted, err := AcceptWalletMappingConsent(t.Context(), f.owner, original, f.address); accepted != nil || !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("delayed signature acquired already-earned epoch", accepted, err)
		}
		f.requireCounts(t, 0)
		future := f.challenge(t, 52)
		if accepted, err := AcceptWalletMappingConsent(t.Context(), f.owner, future, f.address); err != nil || accepted == nil || !accepted.Applied {
			t.Fatal("fresh future mapping failed after refused predecessor", accepted, err)
		}
		f.requireCounts(t, 1)
	})
}

// Reopening a lost acknowledgement after the effective epoch has begun must
// return the exact admitted original, not re-apply a historical projection.
func TestWalletMappingProspectiveSqlRetryKeepsOriginalAcrossEpochAdvance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := walletMappingProspectiveFixture(t)
		original := f.challenge(t, 51)
		first, err := AcceptWalletMappingConsent(t.Context(), f.owner, original, f.address)
		if err != nil || first == nil || !first.Applied {
			t.Fatal(first, err)
		}
		f.owner.Prospective.Boundary = protocol.ClientKeyEffectiveBoundary{Epoch: 60, Block: 600, Hash: [32]byte{61}}
		for range 2 {
			retained, err := AcceptWalletMappingConsent(t.Context(), f.owner, original, f.address)
			if err != nil || retained == nil || retained.Applied || retained.OriginalHash != first.OriginalHash {
				t.Fatal("exact acknowledged original changed after epoch advance", retained, err)
			}
		}
		f.requireCounts(t, 1)
	})
}
