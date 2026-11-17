package main

import (
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// `bringyourctl network trim-proxies --include_pro --keep_described
// --keep_used_since --list` (owner, 2026-11-14, support item 8327).

func networkTrimProxiesParse(args []string) (docopt.Opts, error) {
	return (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, args, "test")
}

// The options the command builds from a parsed command line.
func networkTrimProxiesOptionsFromOpts(opts docopt.Opts) (model.NetworkProxyTrimOptions, error) {
	networkIdsStr, _ := opts.String("--network_ids")
	keepStr, _ := opts.String("--keep")
	rateStr, _ := opts.String("--rate")
	includePro, _ := opts.Bool("--include_pro")
	keepDescribed, _ := opts.Bool("--keep_described")
	keepUsedSinceStr, _ := opts.String("--keep_used_since")
	options, err := parseNetworkTrimProxiesOptions(networkIdsStr, keepStr, rateStr)
	if err == nil {
		err = parseNetworkTrimProxiesKeepOptions(&options, includePro, keepDescribed, keepUsedSinceStr)
	}
	return options, err
}

// Pure: the new flags parse beside --keep (an exact option, not a prefix of
// --keep_described or --keep_used_since), and the commit line a dry run prints
// parses back to the same options.
func TestNetworkTrimProxiesProCommandLine(t *testing.T) {
	a := server.NewId()
	args := []string{
		"network", "trim-proxies",
		"--network_ids=" + a.String(),
		"--include_pro",
		"--keep=5",
		"--keep_described",
		"--keep_used_since=2026-11-07T20:00:00Z",
		"--list",
	}
	opts, err := networkTrimProxiesParse(args)
	if err != nil {
		t.Fatal(args, err)
	}
	includePro, _ := opts.Bool("--include_pro")
	keepDescribed, _ := opts.Bool("--keep_described")
	since, _ := opts.String("--keep_used_since")
	keep, _ := opts.String("--keep")
	list, _ := opts.Bool("--list")
	commit, _ := opts.Bool("--commit")
	if !includePro || !keepDescribed || since != "2026-11-07T20:00:00Z" || keep != "5" || !list || commit {
		t.Fatal("command line", opts)
	}
	options, err := networkTrimProxiesOptionsFromOpts(opts)
	if err != nil {
		t.Fatal(err)
	}

	commitArgs := append([]string{"network", "trim-proxies"}, strings.Fields(networkTrimProxiesFlags(options))...)
	commitArgs = append(commitArgs, "--commit", "--expect=digest")
	commitOpts, err := networkTrimProxiesParse(commitArgs)
	if err != nil {
		t.Fatal(commitArgs, err)
	}
	again, err := networkTrimProxiesOptionsFromOpts(commitOpts)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.NetworkIds) != 1 || again.NetworkIds[0] != a ||
		!again.IncludePro || !again.KeepDescribed || again.Keep != options.Keep ||
		again.KeepUsedSince == nil || !again.KeepUsedSince.Equal(*options.KeepUsedSince) ||
		again.RatePerSecond != options.RatePerSecond {
		t.Fatal("the commit line does not parse back to the dry run's options", commitArgs, again)
	}
}

// Pure: --include_pro needs named networks, --keep_used_since takes an RFC 3339
// time and keeps it in UTC, and the commit line carries the new flags only when
// set, so a trim without them prints the line it printed before.
func TestNetworkTrimProxiesKeepOptions(t *testing.T) {
	a := server.NewId()

	options, err := parseNetworkTrimProxiesOptions("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := parseNetworkTrimProxiesKeepOptions(&options, true, false, ""); err == nil || options.IncludePro {
		t.Fatal("--include_pro without --network_ids", options, err)
	}

	for _, bad := range []string{"7d", "168h", "2026-11-07", "2026-11-07 20:00:00", "not a time"} {
		options, err := parseNetworkTrimProxiesOptions(a.String(), "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := parseNetworkTrimProxiesKeepOptions(&options, true, true, bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}

	options, err = parseNetworkTrimProxiesOptions(a.String(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	err = parseNetworkTrimProxiesKeepOptions(&options, true, true, "2026-11-07T14:00:00-06:00")
	want := time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)
	if err != nil || !options.IncludePro || !options.KeepDescribed || options.KeepUsedSince == nil ||
		!options.KeepUsedSince.Equal(want) || options.KeepUsedSince.Location() != time.UTC {
		t.Fatal("parsed", options, err)
	}
	if flags := networkTrimProxiesFlags(options); flags != " --network_ids="+a.String()+" --include_pro --keep_described --keep_used_since=2026-11-07T20:00:00Z" {
		t.Fatal("flags", flags)
	}

	options, err = parseNetworkTrimProxiesOptions(a.String(), "3", "50")
	if err != nil {
		t.Fatal(err)
	}
	if err := parseNetworkTrimProxiesKeepOptions(&options, false, false, ""); err != nil || options.IncludePro || options.KeepDescribed || options.KeepUsedSince != nil {
		t.Fatal("defaults", options, err)
	}
	if flags := networkTrimProxiesFlags(options); flags != " --network_ids="+a.String()+" --keep=3 --rate=50" {
		t.Fatal("flags", flags)
	}
}

// Pure: for a named Pro network the dry run says why it keeps what it keeps
// and what the removed proxies have in common, and --list prints each proxy.
// A plan without the new options prints exactly what it printed before.
func TestNetworkTrimProxiesPlanOutput(t *testing.T) {
	since := time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)
	old := time.Date(2026, 10, 14, 15, 45, 0, 0, time.UTC)
	oldUse := old.Add(48 * time.Hour)
	recent := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	newest := &model.NetworkProxyTrimProxy{ClientId: server.NewId(), CreateTime: recent, AuthTime: recent, KeepReason: model.NetworkProxyTrimKeepNewest}
	named := &model.NetworkProxyTrimProxy{ClientId: server.NewId(), Description: "home", CreateTime: old, AuthTime: old, KeepReason: model.NetworkProxyTrimKeepDescribed}
	used := &model.NetworkProxyTrimProxy{ClientId: server.NewId(), CreateTime: old, AuthTime: old, ContractTime: &recent, KeepReason: model.NetworkProxyTrimKeepUsed}
	removed := &model.NetworkProxyTrimProxy{ClientId: server.NewId(), CreateTime: old, AuthTime: old, ContractTime: &oldUse}
	networkId := server.NewId()
	plan := &model.NetworkProxyTrimPlan{
		Keep:          1,
		IncludePro:    true,
		KeepDescribed: true,
		KeepUsedSince: &since,
		Networks: []*model.NetworkProxyTrimNetwork{{
			NetworkId:       networkId,
			NetworkName:     "magic",
			ActiveProxies:   4,
			Pro:             true,
			KeepClientIds:   []server.Id{newest.ClientId, named.ClientId, used.ClientId},
			RemoveClientIds: []server.Id{removed.ClientId},
			KeepProxies:     []*model.NetworkProxyTrimProxy{newest, named, used},
			RemoveProxies:   []*model.NetworkProxyTrimProxy{removed},
		}},
		RemoveCount: 1,
		Digest:      "d",
	}
	var out strings.Builder
	writeNetworkTrimProxiesPlan(&out, plan, true)
	text := out.String()
	for _, want := range []string{
		"keep the newest 1 active hosted proxies of each free network and of each Pro network named in --network_ids\n",
		"also keep every hosted proxy with a description\n",
		"also keep every hosted proxy used since 2026-11-07T20:00:00Z (its last contract or auth)\n",
		"  trim " + networkId.String() + ` "magic" (Pro, named with --include_pro): 4 active, keep 3, remove 1` + "\n",
		"    keep 3: 1 newest, 1 described, 1 used\n",
		"    remove 1: created 2026-10-14T15:45:00Z to 2026-10-14T15:45:00Z, last used 2026-10-16T15:45:00Z to 2026-10-16T15:45:00Z, 0 described\n",
		"    keep " + newest.ClientId.String() + " newest created 2026-11-12T18:00:00Z last used 2026-11-12T18:00:00Z description \"\"\n",
		"    keep " + named.ClientId.String() + " described created 2026-10-14T15:45:00Z last used 2026-10-14T15:45:00Z description \"home\"\n",
		"    keep " + used.ClientId.String() + " used created 2026-10-14T15:45:00Z last used 2026-11-12T18:00:00Z description \"\"\n",
		"    remove " + removed.ClientId.String() + " - created 2026-10-14T15:45:00Z last used 2026-10-16T15:45:00Z description \"\"\n",
		"networks to trim: 1; hosted proxies to remove: 1\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the plan lacks %q:\n%s", want, text)
		}
	}

	free := &model.NetworkProxyTrimNetwork{
		NetworkId:       server.NewId(),
		NetworkName:     "free",
		ActiveProxies:   7,
		KeepClientIds:   make([]server.Id, 5),
		RemoveClientIds: make([]server.Id, 2),
	}
	pro := &model.NetworkProxyTrimNetwork{NetworkId: server.NewId(), NetworkName: "pro", ActiveProxies: 1001, Pro: true, Skip: model.NetworkProxyTrimSkipPro}
	out.Reset()
	writeNetworkTrimProxiesPlan(&out, &model.NetworkProxyTrimPlan{Keep: 5, Networks: []*model.NetworkProxyTrimNetwork{free, pro}, RemoveCount: 2, Digest: "d"}, false)
	if want := "keep the newest 5 active hosted proxies of each free network\n" +
		"  trim " + free.NetworkId.String() + ` "free": 7 active, keep 5, remove 2` + "\n" +
		"  skip " + pro.NetworkId.String() + ` "pro": Pro, 1001 active (over the Pro cap; trimmed only when named with --include_pro)` + "\n" +
		"networks to trim: 1; hosted proxies to remove: 2\n" +
		"digest: d\n"; out.String() != want {
		t.Fatalf("the plan without the new options:\n%s\nwant:\n%s", out.String(), want)
	}
}
