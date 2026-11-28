package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// `bringyourctl network drift-test` (lag.dev/DRIFTTEST.md P3): a network's
// drift test settings. --origins sets the parent origins its tokens may be
// framed from, comma separated, each an https origin or, for development, a
// local http one; --clear removes the list, which allows any https origin, a
// local http one and an unknown one again (Q5, W5). --disable switches the
// network's drift tests off: it allocates none, and its tokens start no more
// runs; --enable switches them on again. Either may come with either. It then
// prints the settings, as it does with no option.
func networkDriftTest(opts docopt.Opts) {
	ctx := context.Background()

	networkIdStr, _ := opts.String("--network_id")
	networkId, err := server.ParseId(networkIdStr)
	if err != nil {
		panic(err)
	}

	fail := func(err error) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if originsText, _ := opts.String("--origins"); originsText != "" {
		origins, err := controller.ParseLagDriftOrigins(strings.Split(originsText, ","))
		if err != nil {
			fail(fmt.Errorf("invalid --origins: %w", err))
		}
		if err := model.SetLagDriftNetworkOrigins(ctx, networkId, origins); err != nil {
			fail(err)
		}
	} else if clear, _ := opts.Bool("--clear"); clear {
		if err := model.SetLagDriftNetworkOrigins(ctx, networkId, nil); err != nil {
			fail(err)
		}
	}
	if enable, _ := opts.Bool("--enable"); enable {
		if err := model.SetLagDriftNetworkDisabled(ctx, networkId, false); err != nil {
			fail(err)
		}
	} else if disable, _ := opts.Bool("--disable"); disable {
		if err := model.SetLagDriftNetworkDisabled(ctx, networkId, true); err != nil {
			fail(err)
		}
	}

	fmt.Println(networkDriftTestStatusLine(networkId, model.GetLagDriftNetwork(ctx, networkId), model.GetNetworkEmbed(ctx, networkId).Enabled))
}

// The line `network drift-test` prints. Drift tests are an Embed API, so a
// network without Embed has none whatever its switch says.
func networkDriftTestStatusLine(networkId server.Id, network *model.LagDriftNetwork, embedEnabled bool) string {
	state := "on"
	switch {
	case network.Disabled:
		state = "off (switched off)"
	case !embedEnabled:
		state = "off (Embed is not enabled)"
	}
	origins := "any https origin, a local http one, or an unknown one"
	if network.ParentOrigins != nil {
		origins = strings.Join(network.ParentOrigins, ",")
	}
	return fmt.Sprintf("network %s drift tests %s, origins %s", networkId, state, origins)
}

// `bringyourctl lag drift-test` (lag.dev/DRIFTTEST.md P, K5): shows one drift
// test, and with --revoke revokes it: it keeps its runs and its verdict and
// starts no more runs, and its client_ip hash is deleted at once.
func lagDriftTestCommand(opts docopt.Opts) {
	ctx := context.Background()

	driftTestIdStr, _ := opts.String("--drift_test_id")
	driftTestId, err := server.ParseId(driftTestIdStr)
	if err != nil {
		panic(err)
	}
	if revoke, _ := opts.Bool("--revoke"); revoke {
		if !model.RevokeLagDriftTest(ctx, driftTestId, server.NowUtc()) {
			fmt.Fprintf(os.Stderr, "no drift test %s\n", driftTestId)
			os.Exit(1)
		}
	}
	test := model.GetLagDriftTest(ctx, driftTestId)
	if test == nil {
		fmt.Fprintf(os.Stderr, "no drift test %s\n", driftTestId)
		os.Exit(1)
	}
	fmt.Println(lagDriftTestStatusLine(test))
}

// The line `lag drift-test` prints: the test's network, its runs and its
// times, never its token or address.
func lagDriftTestStatusLine(test *model.LagDriftTest) string {
	timeText := func(t *time.Time) string {
		if t == nil {
			return "-"
		}
		return t.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf(
		"drift test %s network %s, %d runs, created %s, expires %s, first started %s, retest until %s, revoked %s, failed %s",
		test.DriftTestId,
		test.NetworkId,
		test.RunCount,
		test.CreatedAt.UTC().Format(time.RFC3339),
		test.ExpiresAt.UTC().Format(time.RFC3339),
		timeText(test.FirstStartedAt),
		timeText(test.RetestUntil),
		timeText(test.RevokeTime),
		timeText(test.FailedAt),
	)
}
