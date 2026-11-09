// server/local's test dns edge (DNS-M5-18; §19.11's local row): the dns API block and one dns edge for
// the local stack, which run-local-dns.sh runs on the launcher's loopback alias, with a key set of its
// own so the edge serves signed answers before DNS-M5-19's ceremony releases real windows. Run from a
// server checkout:
//
//	go run ./local/testdns keys -state <dir>
//	go run ./local/testdns api -state <dir>
//	go run ./local/testdns edge -state <dir> -api_url http://<alias>:<port>
//
// keys makes the key set once and keeps it. api and edge read it and config's dns.yml, every root of
// which takes the set's KSK as its trust anchor, and run their role through dnshost.RunHost as
// server/cli/dns does: the API with each window the KSK signs, the edge with the ZSK and the bearer. It
// is a local tool, and no release builds it: its software KSK never ships, where the real one stays on
// an offline HSM (§12.6's Keys).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/dnshost"
)

// The commands.
const (
	testDnsCommandKeys = "keys"
	testDnsCommandApi  = "api"
	testDnsCommandEdge = "edge"
)

// The block ports either role serves on, which run-local-dns.sh maps to the loopback alias through
// WARP_PORTS, as warp maps a block's.
const (
	testDnsStatusPort = 80
	testDnsDnsPort    = 53
)

func main() {
	// before anything can log (server.ScrubCommandLogs)
	logs := server.ScrubCommandLogs()
	defer logs.Flush()

	usage := func() {
		fmt.Fprintf(os.Stderr, "usage: testdns keys|api|edge -state <dir> [-api_url <url>]\n")
	}
	if len(os.Args) < 2 {
		usage()
		logs.Exit(2)
	}
	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	state := flags.String("state", "", "the key set's directory: absolute, owner-only")
	apiUrl := flags.String("api_url", "", "edge: the test API's base URL")
	if err := flags.Parse(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			logs.Exit(0)
		}
		logs.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()

	settings := DefaultTestDnsSettings()
	var err error
	switch command {
	case testDnsCommandKeys:
		err = makeTestDnsKeys(*state, settings)
	case testDnsCommandApi:
		err = runTestDnsApi(ctx, *state, settings)
	case testDnsCommandEdge:
		err = runTestDnsEdge(ctx, *state, *apiUrl)
	default:
		usage()
		logs.Exit(2)
	}
	if err != nil {
		glog.Errorf("[testdns]%s: %v\n", command, err)
		logs.Exit(1)
	}
}

// config's dns.yml.
func loadTestDnsConfig() (*dnsservice.DnsSettings, error) {
	resource, err := server.Config.SimpleResource("dns.yml")
	if err != nil {
		return nil, err
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, err
	}
	return dnsservice.LoadDnsSettings(data)
}

// The API block, over the local stack's tables, releasing the KSK's windows.
func runTestDnsApi(ctx context.Context, state string, settings *TestDnsSettings) error {
	if err := checkTestDnsState(state); err != nil {
		return err
	}
	keys, err := loadTestDnsKeys(state, settings)
	if err != nil {
		return err
	}
	config, err := loadTestDnsConfig()
	if err != nil {
		return err
	}
	dnsSettings, err := keys.anchored(config)
	if err != nil {
		return err
	}
	return dnshost.RunHost(ctx, dnshost.HostRoleApi, &dnshost.HostSettings{
		Dns:             dnsSettings,
		SecretDirectory: filepath.Join(state, testDnsApiDirectoryName),
	}, &dnshost.HostDeps{KeyWindow: keys.Window}, &dnshost.HostPorts{Status: testDnsStatusPort})
}

// The edge, following the API at apiUrl. It reads only the public half of the KSK.
func runTestDnsEdge(ctx context.Context, state string, apiUrl string) error {
	if err := checkTestDnsState(state); err != nil {
		return err
	}
	parsed, err := url.Parse(apiUrl)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return fmt.Errorf("-api_url %q is not the test API's http base URL", apiUrl)
	}
	public, err := loadTestDnsPublicKeys(state)
	if err != nil {
		return err
	}
	config, err := loadTestDnsConfig()
	if err != nil {
		return err
	}
	dnsSettings, err := public.anchored(config)
	if err != nil {
		return err
	}
	return dnshost.RunHost(ctx, dnshost.HostRoleEdge, &dnshost.HostSettings{
		Dns:                dnsSettings,
		SecretDirectory:    filepath.Join(state, testDnsEdgeDirectoryName),
		CredentialFileName: testDnsEdgeCredentialFileName(),
		ApiUrl:             apiUrl,
		SnapshotDirectory:  filepath.Join(state, testDnsSnapshotDirectoryName),
	}, nil, &dnshost.HostPorts{Status: testDnsStatusPort, Dns: testDnsDnsPort})
}
