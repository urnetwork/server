package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// `bringyourctl network client-limit` (EMBED1.md): ops sets, clears and shows
// a network's Embed plan client allowance.

var networkClientLimitTestParser = &docopt.Parser{HelpHandler: docopt.NoHelpHandler}

func parseNetworkClientLimitArgs(t testing.TB, args ...string) docopt.Opts {
	t.Helper()
	opts, err := networkClientLimitTestParser.ParseArgs(bringyourctlUsage, append([]string{"network", "client-limit"}, args...), "test")
	if err != nil {
		t.Fatalf("parse %v: %s", args, err)
	}
	return opts
}

// Pure: the usage parses each form, and --set and --clear are exclusive.
func TestNetworkClientLimitUsageParses(t *testing.T) {
	networkId := server.NewId().String()

	opts := parseNetworkClientLimitArgs(t, "--network_id="+networkId)
	for _, command := range []string{"network", "client-limit"} {
		value, _ := opts.Bool(command)
		connect.AssertEqual(t, value, true)
	}
	networkIdArg, _ := opts.String("--network_id")
	connect.AssertEqual(t, networkIdArg, networkId)
	setArg, _ := opts.String("--set")
	connect.AssertEqual(t, setArg, "")
	clearArg, _ := opts.Bool("--clear")
	connect.AssertEqual(t, clearArg, false)

	opts = parseNetworkClientLimitArgs(t, "--network_id="+networkId, "--set=5000")
	setArg, _ = opts.String("--set")
	connect.AssertEqual(t, setArg, "5000")

	opts = parseNetworkClientLimitArgs(t, "--network_id="+networkId, "--clear")
	clearArg, _ = opts.Bool("--clear")
	connect.AssertEqual(t, clearArg, true)

	if _, err := networkClientLimitTestParser.ParseArgs(bringyourctlUsage, []string{"network", "client-limit", "--network_id=" + networkId, "--set=1", "--clear"}, "test"); err == nil {
		t.Fatal("--set and --clear must be exclusive")
	}
	if _, err := networkClientLimitTestParser.ParseArgs(bringyourctlUsage, []string{"network", "client-limit"}, "test"); err == nil {
		t.Fatal("--network_id must be required")
	}
}

// Pure: the help states what an Embed plan sets.
func TestNetworkClientLimitHelpText(t *testing.T) {
	for _, line := range []string{
		"bringyourctl network client-limit --network_id=<network_id> [--set=<limit> | --clear]",
		"--set=<limit>  Set the network's Embed plan client allowance: the top-level",
		"client limit and the concurrent connection limit (the defaults",
		"are 100 and the tier's concurrent_clients).",
		"--clear        Return the network to the default limits.",
	} {
		if !strings.Contains(bringyourctlUsage, line) {
			t.Fatalf("usage is missing %q", line)
		}
	}
}

// captureStdout runs fn and returns what it printed.
func captureStdout(t testing.TB, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = writer
	done := make(chan string)
	go func() {
		out, _ := io.ReadAll(reader)
		done <- string(out)
	}()
	func() {
		defer func() { os.Stdout = stdout }()
		fn()
	}()
	writer.Close()
	return <-done
}

// DB-backed (postgres): under the owner rule this runs only after the branch,
// with its migrations, is merged to main.
func TestNetworkClientLimitCommand(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "embed-ctl", server.NewId())
		defer model.Testing_ClearNetworkClientLimitCache()

		// show: the defaults
		out := captureStdout(t, func() {
			networkClientLimit(parseNetworkClientLimitArgs(t, "--network_id="+networkId.String()))
		})
		connect.AssertEqual(t, strings.TrimSpace(out), "network "+networkId.String()+" top-level client limit 100 (default; concurrent connections follow the tier)")

		// set: an Embed plan allowance for both limits
		out = captureStdout(t, func() {
			networkClientLimit(parseNetworkClientLimitArgs(t, "--network_id="+networkId.String(), "--set=5000"))
		})
		connect.AssertEqual(t, strings.TrimSpace(out), "network "+networkId.String()+" client allowance 5000 (Embed plan: top-level client limit and concurrent connection limit)")
		limit := model.GetNetworkTopLevelClientLimit(ctx, networkId)
		connect.AssertEqual(t, limit.Limit, 5000)
		connect.AssertEqual(t, limit.Override, true)

		// clear: back to the defaults
		out = captureStdout(t, func() {
			networkClientLimit(parseNetworkClientLimitArgs(t, "--network_id="+networkId.String(), "--clear"))
		})
		connect.AssertEqual(t, strings.TrimSpace(out), "network "+networkId.String()+" top-level client limit 100 (default; concurrent connections follow the tier)")
		connect.AssertEqual(t, model.GetNetworkTopLevelClientLimit(ctx, networkId).Override, false)
	})
}
