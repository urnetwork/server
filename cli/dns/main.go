// server/cli/dns (DNS-M5-18; §12.6, §19.4): the dns engine's one binary in two roles, --role api for
// the API blocks on dns.bringyour.com and --role edge for dnsedge on the dns edge hosts, each a block
// run by sidecarhost.Run with GET /status on port 80, {vault: no, config: yes, site: no} and user 65532.
// server/cli/dnsedge builds this same main and picks the edge role.
package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/dnshost"
	"github.com/urnetwork/server/sidecarhost"
)

var (
	role          = flag.String("role", "", "api or edge")
	port          = flag.Int("port", 80, "the block's status port")
	dnsPort       = flag.Int("dns_port", 53, "edge: the block's UDP and TCP port for DNS")
	secretDir     = flag.String("secret_dir", sidecarhost.ScopedVaultHome, "where the block's secret_files mount")
	apiUrl        = flag.String("api_url", "", "edge: the dns API block's base URL")
	snapshotDir   = flag.String("snapshot_dir", "/srv/warp/data/dnsedge", "edge: where each root's local snapshot lives")
	credential    = flag.String("credential_file", "", "edge: its bearer's file in secret_dir; empty is internal-<WARP_SERVICE>.yml")
	requireReload = flag.Bool("require_reload", false, "edge: serve nothing until a reload (§19.10's start for a stopped NS)")
)

func main() {
	// before anything can log (server.ScrubCommandLogs)
	logs := server.ScrubCommandLogs()
	defer logs.Flush()

	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			logs.Exit(0)
		}
		logs.Exit(2)
	}
	if *role != dnshost.HostRoleApi && *role != dnshost.HostRoleEdge {
		glog.Errorf("[dns]--role must be api or edge\n")
		logs.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()

	settings, err := loadDnsSettings()
	if err != nil {
		// a dns.yml that cannot be served from never serves (dnsservice.LoadDnsSettings)
		glog.Errorf("[dns]dns.yml: %v\n", err)
		logs.Exit(1)
	}
	hostSettings := &dnshost.HostSettings{
		Dns:                settings,
		SecretDirectory:    *secretDir,
		CredentialFileName: *credential,
		ApiUrl:             *apiUrl,
		SnapshotDirectory:  *snapshotDir,
		RequireReload:      *requireReload,
	}
	if hostSettings.CredentialFileName == "" {
		hostSettings.CredentialFileName = "internal-" + os.Getenv("WARP_SERVICE") + ".yml"
	}
	// no key windows until DNS-M5-19's KeyWindowRelease, so until then no edge this API feeds is ready
	if err := dnshost.RunHost(ctx, *role, hostSettings, nil, &dnshost.HostPorts{Status: *port, Dns: *dnsPort}); err != nil {
		panic(err)
	}
}

// dns.yml from config.
func loadDnsSettings() (*dnsservice.DnsSettings, error) {
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
