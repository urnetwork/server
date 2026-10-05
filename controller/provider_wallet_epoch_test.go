// Exercise the production payout entry point and its admitted-roster adapter,
// including the absence case that previously fell through to SQL projections.
package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A configured mainnet earning producer cannot mint an artifact before its
// independent provider/wallet authority has arrived. Startup is unaffected.
func TestProviderWalletEpochProductionHoldsMissingRoster(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkWindowFixture(t)
		client := newStubStClient(&StEpochState{})
		root, count, err := stComputeReleasePayout(t.Context(), f.cfg, client, f.epoch.Epoch, f.epoch.StartTime, f.epoch.EndTime, f.epoch.Start.Block, f.epoch.End.Block, f.epoch)
		if root != ([32]byte{}) || count != 0 || !errors.Is(err, protocol.ErrWalletMappingUnavailable) {
			t.Fatal("payout without approved wallet roster escaped pending", root, count, err)
		}
		if artifact := model.GetStPayoutArtifact(t.Context(), f.cfg.DeploymentKey(), f.epoch.Epoch, f.cfg.NoId); artifact != nil {
			t.Fatal("missing original wallet authority published an artifact")
		}
	})
}

// The adapter uses configuration and authenticated boundaries for scope,
// while the retained original supplies the independent provider and head set.
func TestProviderWalletEpochAdapterBindsConfiguredScope(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkWindowFixture(t)
		raw := f.retainAuthority(t)
		_, _, expected, err := LoadProviderWorkAuthorityPolicy()
		if err != nil {
			t.Fatal(err)
		}
		approved := &stProviderWorkAuthority{Raw: raw, Authority: f.authority, Expectation: expected}
		wallets, err := stProviderWalletsForEpoch(t.Context(), f.cfg, approved, f.epoch)
		if err != nil || wallets == nil || len(wallets) != 0 {
			t.Fatal("independently approved known-empty roster was lost", wallets, err)
		}
		for index, change := range []func(*StConfig){func(c *StConfig) { c.NoId++ }, func(c *StConfig) { c.Netuid++ }, func(c *StConfig) { c.DeploymentId += "-another" }, func(c *StConfig) { c.PolicyHash[0] ^= 1 }} {
			cfg := *f.cfg
			change(&cfg)
			if wallets, err := stProviderWalletsForEpoch(t.Context(), &cfg, approved, f.epoch); wallets != nil || !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
				t.Fatal("another configured payout scope borrowed wallet roster", index, wallets, err)
			}
		}
		if wallets, err := stProviderWalletsForEpoch(t.Context(), f.cfg, nil, f.epoch); wallets != nil || !errors.Is(err, protocol.ErrWalletMappingUnavailable) {
			t.Fatal("missing retained authority acquired account projection", wallets, err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if wallets, err := stProviderWalletsForEpoch(ctx, f.cfg, approved, f.epoch); wallets != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("canceled production owner returned wallet authority", wallets, err)
		}
	})
}
