package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server/v2026"
)

type boundaryShortWriter struct{}

func (self boundaryShortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestProviderBoundaryMigrationPreparationRequiresExactDigest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		data := []byte(fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: 2026-10-06T00:00:00Z\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", "0x"+strings.Repeat("11", 32)))
		t.Cleanup(server.Config.PushSimpleResource("sn.yml", data))
		policy, err := server.LoadProviderPayoutTransition(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := preparePayoutBoundaryAfterMigrateTo(ctx, docopt.Opts{}, &output); err != nil || output.Len() != 0 {
			t.Fatal("ordinary migration implicitly prepared authority", err)
		}
		if _, err := server.LoadProviderPayoutEarningPolicy(ctx); !errors.Is(err, server.ErrProviderEarningBoundaryUnprepared) {
			t.Fatal("missing explicit digest became worker authority", err)
		}
		wrong := docopt.Opts{"--sn-schedule-sha256": strings.Repeat("22", 32)}
		if err := preparePayoutBoundaryAfterMigrateTo(ctx, wrong, &output); err == nil || output.Len() != 0 {
			t.Fatal("wrong reviewed digest reported preparation")
		}
		opts := docopt.Opts{"--sn-schedule-sha256": policy.ConfigSha256}
		if err := preparePayoutBoundaryAfterMigrateTo(ctx, opts, &output); err != nil {
			t.Fatal(err)
		}
		var report struct {
			Boundary                 *server.ProviderEarningBoundary `json:"earning_boundary"`
			DeploymentVerified       bool                            `json:"deployment_verified"`
			ChainReadinessAuthorized bool                            `json:"chain_readiness_authorized"`
		}
		if err := json.Unmarshal(output.Bytes(), &report); err != nil || report.Boundary == nil || report.Boundary.InitialConfigSha256 != policy.ConfigSha256 || report.Boundary.CutoffUtc != "2026-10-06T00:00:00Z" || report.DeploymentVerified || report.ChainReadinessAuthorized {
			t.Fatal("preparation output omitted boundary or invented deployment/readiness", err)
		}
		if err := preparePayoutBoundaryAfterMigrateTo(ctx, opts, boundaryShortWriter{}); !errors.Is(err, io.ErrShortWrite) {
			t.Fatal("truncated preparation report acknowledged", err)
		}
		binding, err := server.RequireProviderPayoutBoundary(ctx, policy)
		if err != nil || binding == nil || binding.IdentitySha256 != report.Boundary.IdentitySha256 {
			t.Fatal("lost report altered idempotently prepared boundary", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := preparePayoutBoundaryAfterMigrateTo(canceled, opts, io.Discard); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled preparation admitted filesystem/database work", err)
		}
	})
}
