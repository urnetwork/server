package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// `bringyourctl network trim-proxies` (model/network_client_proxy_trim.go;
// owner, 2026-11-05: "Trim networks over the cap").

// Pure: a dry run unless --commit, and --commit needs the dry run's digest;
// --network_id keeps its single value in the other commands.
func TestNetworkTrimProxiesCommandLine(t *testing.T) {
	parse := func(args []string) (docopt.Opts, error) {
		return (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, args, "test")
	}
	a, b := server.NewId().String(), server.NewId().String()
	for _, args := range [][]string{
		{"network", "trim-proxies"},
		{"network", "trim-proxies", "--network_ids=" + a + "," + b},
		{"network", "trim-proxies", "--keep=5", "--rate=100"},
		{"network", "trim-proxies", "--commit", "--expect=digest"},
		{"network", "trim-proxies", "--network_ids=" + a, "--keep=5", "--rate=50", "--commit", "--expect=digest"},
	} {
		opts, err := parse(args)
		if err != nil {
			t.Fatal(args, err)
		}
		network, _ := opts.Bool("network")
		trimProxies, _ := opts.Bool("trim-proxies")
		commit, _ := opts.Bool("--commit")
		expect, _ := opts.String("--expect")
		if !network || !trimProxies || commit != slices.Contains(args, "--commit") || (expect == "digest") != commit {
			t.Fatal("command line", args, opts)
		}
	}
	for _, args := range [][]string{
		{"network", "trim-proxies", "--commit"},
		{"network", "trim-proxies", "--expect=digest"},
		{"network", "trim-proxies", "--network_id=" + a},
	} {
		if _, err := parse(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	opts, err := parse([]string{"network", "sign-out-all", "--network_id=" + a})
	if err != nil {
		t.Fatal(err)
	}
	if networkId, err := opts.String("--network_id"); err != nil || networkId != a {
		t.Fatal("--network_id is no longer a single value", opts)
	}
}

// Pure: the options, with the free cap and 200 a second by default.
func TestNetworkTrimProxiesOptions(t *testing.T) {
	a, b := server.NewId(), server.NewId()
	options, err := parseNetworkTrimProxiesOptions("", "", "")
	if err != nil || options.Keep != model.DefaultNetworkProxyTrimKeep() || options.RatePerSecond != 200 || len(options.NetworkIds) != 0 {
		t.Fatal("defaults", options, err)
	}
	options, err = parseNetworkTrimProxiesOptions(a.String()+", "+b.String(), "3", "50")
	if err != nil || options.Keep != 3 || options.RatePerSecond != 50 || !slices.Equal(options.NetworkIds, []server.Id{a, b}) {
		t.Fatal("parsed", options, err)
	}
	if flags := networkTrimProxiesFlags(options); flags != " --network_ids="+a.String()+","+b.String()+" --keep=3 --rate=50" {
		t.Fatal("flags", flags)
	}
	for _, bad := range [][3]string{{"not-an-id", "", ""}, {"", "-1", ""}, {"", "x", ""}, {"", "", "0"}, {"", "", "fast"}} {
		if _, err := parseNetworkTrimProxiesOptions(bad[0], bad[1], bad[2]); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}

// Pure: the dry run's process is read only before it opens a database
// session, and main runs the command.
func TestNetworkTrimProxiesDryRunProcessIsReadOnly(t *testing.T) {
	source, err := os.ReadFile("network_trim_proxies.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	readOnly := strings.Index(text, "if !commit {\n\t\tserver.RequirePgReadOnly()\n\t}")
	run := strings.Index(text, "return runNetworkTrimProxies(")
	if readOnly < 0 || run < readOnly {
		t.Fatal("the dry run opens a database session before its process is read only")
	}
	main, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(main), "os.Exit(networkTrimProxiesCommand(opts))") {
		t.Fatal("main does not run the trim-proxies command")
	}
}
