package main

import (
	"context"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// `bringyourctl network embed` (EMBED1.md): ops enables Embed for a network
// once its sales contract is signed, optionally with its client allowance,
// disables it, and shows its state.

func parseNetworkEmbedArgs(t testing.TB, args ...string) docopt.Opts {
	t.Helper()
	opts, err := networkClientLimitTestParser.ParseArgs(bringyourctlUsage, append([]string{"network", "embed"}, args...), "test")
	if err != nil {
		t.Fatalf("parse %v: %s", args, err)
	}
	return opts
}

// Pure: the usage parses each form; --enable and --disable are exclusive, and
// --client-limit only goes with --enable.
func TestNetworkEmbedUsageParses(t *testing.T) {
	networkId := server.NewId().String()

	opts := parseNetworkEmbedArgs(t, "--network_id="+networkId)
	for _, command := range []string{"network", "embed"} {
		value, _ := opts.Bool(command)
		connect.AssertEqual(t, value, true)
	}
	networkIdArg, _ := opts.String("--network_id")
	connect.AssertEqual(t, networkIdArg, networkId)
	enableArg, _ := opts.Bool("--enable")
	connect.AssertEqual(t, enableArg, false)
	disableArg, _ := opts.Bool("--disable")
	connect.AssertEqual(t, disableArg, false)
	clientLimitArg, _ := opts.String("--client-limit")
	connect.AssertEqual(t, clientLimitArg, "")

	opts = parseNetworkEmbedArgs(t, "--network_id="+networkId, "--enable")
	enableArg, _ = opts.Bool("--enable")
	connect.AssertEqual(t, enableArg, true)
	clientLimitArg, _ = opts.String("--client-limit")
	connect.AssertEqual(t, clientLimitArg, "")

	opts = parseNetworkEmbedArgs(t, "--network_id="+networkId, "--enable", "--client-limit=5000")
	enableArg, _ = opts.Bool("--enable")
	connect.AssertEqual(t, enableArg, true)
	clientLimitArg, _ = opts.String("--client-limit")
	connect.AssertEqual(t, clientLimitArg, "5000")
	// the option is not the `client-limit` command, which main dispatches first
	clientLimitCommand, _ := opts.Bool("client-limit")
	connect.AssertEqual(t, clientLimitCommand, false)

	opts = parseNetworkEmbedArgs(t, "--network_id="+networkId, "--disable")
	disableArg, _ = opts.Bool("--disable")
	connect.AssertEqual(t, disableArg, true)

	// `network client-limit` is unchanged beside it
	opts = parseNetworkClientLimitArgs(t, "--network_id="+networkId, "--set=5000")
	embedArg, _ := opts.Bool("embed")
	connect.AssertEqual(t, embedArg, false)

	for _, args := range [][]string{
		{"--network_id=" + networkId, "--enable", "--disable"},
		{"--network_id=" + networkId, "--client-limit=5000"},
		{"--network_id=" + networkId, "--disable", "--client-limit=5000"},
		{},
	} {
		if _, err := networkClientLimitTestParser.ParseArgs(bringyourctlUsage, append([]string{"network", "embed"}, args...), "test"); err == nil {
			t.Fatalf("network embed %v must not parse", args)
		}
	}
}

// Pure: the help states what enabling opens and what disabling keeps: the
// stored caps and groups, and the refusal of the network's client tokens.
func TestNetworkEmbedHelpText(t *testing.T) {
	for _, line := range []string{
		"bringyourctl network embed --network_id=<network_id> [--enable [--client-limit=<limit>] | --disable]",
		"--enable       Enable Embed for the network, once its sales contract is",
		"signed: its data-cap and ACL-group APIs open.",
		"--client-limit=<limit>  With --enable, also set the network's client",
		"allowance: the top-level client limit and the concurrent",
		"--disable      Disable Embed for the network and return it to the default",
		"limits. Caps and ACL groups already set stay enforced, and",
		"the network's client tokens stay refused on admin routes.",
	} {
		if !strings.Contains(bringyourctlUsage, line) {
			t.Fatalf("usage is missing %q", line)
		}
	}
}

// Pure: the printed state names the flag, the effective allowance and the
// active client count. A disabled network that was enabled says so, apart from
// one that never was.
func TestNetworkEmbedStatusLine(t *testing.T) {
	networkId := server.NewId()
	connect.AssertEqual(
		t,
		networkEmbedStatusLine(networkId, &model.NetworkEmbed{Enabled: true, EverEnabled: true, ClientLimit: 5000, ActiveClientCount: 12}),
		"network "+networkId.String()+" embed enabled, client limit 5000, 12 active clients",
	)
	connect.AssertEqual(
		t,
		networkEmbedStatusLine(networkId, &model.NetworkEmbed{Enabled: false, EverEnabled: true, ClientLimit: 100, ActiveClientCount: 3}),
		"network "+networkId.String()+" embed disabled (was enabled), client limit 100, 3 active clients",
	)
	connect.AssertEqual(
		t,
		networkEmbedStatusLine(networkId, &model.NetworkEmbed{Enabled: false, ClientLimit: 100, ActiveClientCount: 0}),
		"network "+networkId.String()+" embed not enabled, client limit 100, 0 active clients",
	)
}

// DB-backed (postgres): under the owner rule this runs only after the branch,
// with its migrations, is merged to main.
func TestNetworkEmbedCommand(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "embed-ctl", userId)
		defer model.Testing_ClearNetworkClientLimitCache()
		defer model.Testing_ClearNetworkEmbedCache()
		networkIdArg := "--network_id=" + networkId.String()
		prefix := "network " + networkId.String() + " embed "

		// show: not enabled, the default allowance, no clients
		out := captureStdout(t, func() {
			networkEmbed(parseNetworkEmbedArgs(t, networkIdArg))
		})
		connect.AssertEqual(t, strings.TrimSpace(out), prefix+"not enabled, client limit 100, 0 active clients")

		// enable with an allowance
		out = captureStdout(t, func() {
			networkEmbed(parseNetworkEmbedArgs(t, networkIdArg, "--enable", "--client-limit=5000"))
		})
		connect.AssertEqual(t, strings.TrimSpace(out), prefix+"enabled, client limit 5000, 0 active clients")
		connect.AssertEqual(t, model.NetworkEmbedEnabled(ctx, networkId), true)
		limit := model.GetNetworkTopLevelClientLimit(ctx, networkId)
		connect.AssertEqual(t, limit.Limit, 5000)
		connect.AssertEqual(t, limit.Override, true)

		// show counts the active top-level clients
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), server.NewId(), "install", "test")
		out = captureStdout(t, func() {
			networkEmbed(parseNetworkEmbedArgs(t, networkIdArg))
		})
		connect.AssertEqual(t, strings.TrimSpace(out), prefix+"enabled, client limit 5000, 1 active clients")

		// enable alone keeps the allowance already set
		out = captureStdout(t, func() {
			networkEmbed(parseNetworkEmbedArgs(t, networkIdArg, "--enable"))
		})
		connect.AssertEqual(t, strings.TrimSpace(out), prefix+"enabled, client limit 5000, 1 active clients")

		// disable: the flag and the allowance are cleared, and the network shows
		// that it was enabled, its client tokens still refused on the admin
		// routes
		out = captureStdout(t, func() {
			networkEmbed(parseNetworkEmbedArgs(t, networkIdArg, "--disable"))
		})
		connect.AssertEqual(t, strings.TrimSpace(out), prefix+"disabled (was enabled), client limit 100, 1 active clients")
		connect.AssertEqual(t, model.NetworkEmbedEnabled(ctx, networkId), false)
		connect.AssertEqual(t, model.NetworkEmbedEverEnabled(ctx, networkId), true)
		connect.AssertEqual(t, model.NetworkRefusesClientAdmin(ctx, networkId), true)
		connect.AssertEqual(t, model.GetNetworkTopLevelClientLimit(ctx, networkId).Override, false)

		// show and a second disable keep it
		for _, args := range [][]string{{networkIdArg}, {networkIdArg, "--disable"}} {
			out = captureStdout(t, func() {
				networkEmbed(parseNetworkEmbedArgs(t, args...))
			})
			connect.AssertEqual(t, strings.TrimSpace(out), prefix+"disabled (was enabled), client limit 100, 1 active clients")
		}

		// enable again, then disable again
		out = captureStdout(t, func() {
			networkEmbed(parseNetworkEmbedArgs(t, networkIdArg, "--enable"))
		})
		connect.AssertEqual(t, strings.TrimSpace(out), prefix+"enabled, client limit 100, 1 active clients")
		connect.AssertEqual(t, model.NetworkEmbedEnabled(ctx, networkId), true)
		out = captureStdout(t, func() {
			networkEmbed(parseNetworkEmbedArgs(t, networkIdArg, "--disable"))
		})
		connect.AssertEqual(t, strings.TrimSpace(out), prefix+"disabled (was enabled), client limit 100, 1 active clients")
		connect.AssertEqual(t, model.NetworkEmbedEnabled(ctx, networkId), false)
		connect.AssertEqual(t, model.NetworkEmbedEverEnabled(ctx, networkId), true)

		// a disable of a network that was never enabled leaves it not enabled
		otherNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, otherNetworkId, "embed-ctl-other", server.NewId())
		out = captureStdout(t, func() {
			networkEmbed(parseNetworkEmbedArgs(t, "--network_id="+otherNetworkId.String(), "--disable"))
		})
		connect.AssertEqual(t, strings.TrimSpace(out), "network "+otherNetworkId.String()+" embed not enabled, client limit 100, 0 active clients")
		connect.AssertEqual(t, model.NetworkEmbedEverEnabled(ctx, otherNetworkId), false)
	})
}
