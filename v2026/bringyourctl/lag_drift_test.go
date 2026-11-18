package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// `bringyourctl network drift-test` and `bringyourctl lag drift-test`
// (lag.dev/DRIFTTEST.md P3, K5): a network's drift test origins and switch,
// and revoking one drift test.

func parseDriftTestArgs(t testing.TB, command []string, args ...string) docopt.Opts {
	t.Helper()
	opts, err := networkClientLimitTestParser.ParseArgs(bringyourctlUsage, append(command, args...), "test")
	if err != nil {
		t.Fatalf("parse %v %v: %s", command, args, err)
	}
	return opts
}

// Pure: the usage parses each form; --origins and --clear are exclusive, and
// so are --enable and --disable, which may come with either. `network
// embed` keeps its own meaning of --enable and --disable.
func TestDriftTestUsageParses(t *testing.T) {
	networkIdArg := "--network_id=" + server.NewId().String()
	command := []string{"network", "drift-test"}

	opts := parseDriftTestArgs(t, command, networkIdArg)
	driftTest, _ := opts.Bool("drift-test")
	connect.AssertEqual(t, driftTest, true)
	embed, _ := opts.Bool("embed")
	connect.AssertEqual(t, embed, false)
	origins, _ := opts.String("--origins")
	connect.AssertEqual(t, origins, "")

	opts = parseDriftTestArgs(t, command, networkIdArg, "--origins=https://a.example,https://b.example", "--disable")
	origins, _ = opts.String("--origins")
	connect.AssertEqual(t, origins, "https://a.example,https://b.example")
	disable, _ := opts.Bool("--disable")
	connect.AssertEqual(t, disable, true)

	opts = parseDriftTestArgs(t, command, networkIdArg, "--clear", "--enable")
	clear, _ := opts.Bool("--clear")
	connect.AssertEqual(t, clear, true)
	enable, _ := opts.Bool("--enable")
	connect.AssertEqual(t, enable, true)

	for _, args := range [][]string{
		{networkIdArg, "--origins=https://a.example", "--clear"},
		{networkIdArg, "--enable", "--disable"},
		{"--origins=https://a.example"},
	} {
		if _, err := networkClientLimitTestParser.ParseArgs(bringyourctlUsage, append(command, args...), "test"); err == nil {
			t.Fatalf("network drift-test %v must not parse", args)
		}
	}

	opts = parseDriftTestArgs(t, []string{"lag", "drift-test"}, "--drift_test_id="+server.NewId().String(), "--revoke")
	lag, _ := opts.Bool("lag")
	connect.AssertEqual(t, lag, true)
	revoke, _ := opts.Bool("--revoke")
	connect.AssertEqual(t, revoke, true)
	network, _ := opts.Bool("network")
	connect.AssertEqual(t, network, false)
}

// DB-backed: origins set, cleared and kept apart from the switch, the
// switch's state with and without Embed, and a revoked drift test.
func TestDriftTestCommands(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "drift-ctl", server.NewId())
		defer model.Testing_ClearNetworkEmbedCache()
		networkIdArg := "--network_id=" + networkId.String()
		command := []string{"network", "drift-test"}
		prefix := "network " + networkId.String() + " drift tests "
		run := func(args ...string) string {
			return strings.TrimSpace(captureStdout(t, func() {
				networkDriftTest(parseDriftTestArgs(t, command, append([]string{networkIdArg}, args...)...))
			}))
		}

		connect.AssertEqual(t, run(), prefix+"off (Embed is not enabled), origins any https origin, a local http one, or an unknown one")
		model.Testing_EnableNetworkEmbed(ctx, networkId)
		connect.AssertEqual(t, run(), prefix+"on, origins any https origin, a local http one, or an unknown one")

		connect.AssertEqual(t, run("--origins=https://Shop.example.com/, http://localhost:3000"), prefix+"on, origins https://shop.example.com,http://localhost:3000")
		connect.AssertEqual(t, model.GetLagDriftNetworkOrigins(ctx, networkId), []string{"https://shop.example.com", "http://localhost:3000"})
		connect.AssertEqual(t, run("--disable"), prefix+"off (switched off), origins https://shop.example.com,http://localhost:3000")
		connect.AssertEqual(t, run("--clear"), prefix+"off (switched off), origins any https origin, a local http one, or an unknown one")
		connect.AssertEqual(t, run("--enable"), prefix+"on, origins any https origin, a local http one, or an unknown one")

		// revoking one drift test
		now := server.NowUtc()
		test := &model.LagDriftTest{
			DriftTestId:    server.NewId(),
			NetworkId:      networkId,
			TokenHash:      server.NewId().Bytes(),
			ClientIpHash:   server.NewId().Bytes(),
			ClientIpFamily: 4,
			Confidence:     0.99,
			CreatedAt:      now,
			ExpiresAt:      now.Add(5 * time.Minute),
		}
		model.CreateLagDriftTest(ctx, test)
		out := strings.TrimSpace(captureStdout(t, func() {
			lagDriftTestCommand(parseDriftTestArgs(t, []string{"lag", "drift-test"}, "--drift_test_id="+test.DriftTestId.String(), "--revoke"))
		}))
		if !strings.HasPrefix(out, "drift test "+test.DriftTestId.String()+" network "+networkId.String()+", 0 runs") || strings.Contains(out, "revoked -") {
			t.Fatalf("revoked as %q", out)
		}
		revoked := model.GetLagDriftTest(ctx, test.DriftTestId)
		connect.AssertNotEqual(t, revoked.RevokeTime, (*time.Time)(nil))
		connect.AssertEqual(t, revoked.ClientIpHash, []byte(nil))
	})
}
