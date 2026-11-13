package dnshost

// server/cli/dns's two roles (DNS-M5-18; §12.6, §19.4, D-DNS-3, R-DNS-8, I-12):
// one binary, a thin host each.
//
//   - api: dnsservice.NewDnsApi over Store, on dns.bringyour.com, with certs'
//     challenge writer, the bearer digests and services.yml's lb inventory
//     from its secret_files (reading 6: services.yml is mounted, since
//     server.Vault has nothing to read under {vault: no}), the router list
//     from the same file's stream services (DNS-M7-01), and each NS's SOA
//     read over TCP at dns.yml's glue. It writes <root>'s own records at start
//     (DNS-M5-11). Ready on §12.14's tables, read by its store probe.
//   - edge: dnsservice.NewDnsEdge, a FeedLoop over the block's HTTP feed and a
//     Prober of the lb interfaces, signing with the ZSK its secret_files hold
//     against dns.yml's trust anchors. It holds no Postgres or Redis handle at
//     all, so I-12 holds by construction, and with the core down it serves its
//     local snapshot. Ready on a current snapshot and a verified key set. UDP 53
//     is served beside the host's TCP listener, since a block's listeners are
//     TCP.
//
// A role that cannot start for want of a file or a key is a not-ready engine,
// so its block serves /status, never answers queries, and a deploy reverts.
// RunHost runs either as a block, for server/cli/dns and for server/local's
// test dns edge, which hands the API its own key windows.

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// The roles.
const (
	HostRoleApi  = "api"
	HostRoleEdge = "edge"
)

// What each role reads from its block's secret_files.
const (
	// The API block's bearer digests (plan-DNS-internal-route-credential).
	HostVerifyFileName = "internal-verify.yml"
	// vault's services.yml, for the lb inventory every feed answer names.
	HostServicesFileName = "services.yml"
	// An edge's current ZSK, in BIND's private key format (DNS-M5-04's LoadZsk).
	HostZskFileName = "dns-zsk.private"
)

// The port an NS answers its SOA on.
const hostDnsPort = 53

// What a role is given.
type HostSettings struct {
	// dns.yml.
	Dns *dnsservice.DnsSettings
	// Where the block's secret_files mount: sidecarhost.ScopedVaultHome in a block.
	SecretDirectory string
	// edge: its bearer's file in SecretDirectory, internal-<service>.yml.
	CredentialFileName string
	// edge: the API block's base URL.
	ApiUrl string
	// edge: where each root's local snapshot lives, which must outlive the process.
	SnapshotDirectory string
	// edge: §19.10's start for a stopped NS, serving nothing until a reload.
	RequireReload bool
}

// The seams a test replaces. nil takes the real one.
type HostDeps struct {
	// edge: the block's feed. nil is HTTP to ApiUrl.
	Feed dnsservice.DnsFeed
	// edge: how probes reach the lb. nil dials TCP.
	ProbeDialer dnsservice.DnsProbeDialer
	// edge: where route probes go. nil posts to ApiUrl.
	RouteProbes dnsservice.DnsRouteProbeSink
	// api: the store. nil is Store over host.Redis. Only a Store has <root>'s records written at start,
	// since they go through model to Postgres, which a fake does not read.
	Store dnsservice.DnsStore
	// api: §8.10's gate on dns:<host> for the record routes (DNS-M5-12). nil is
	// none, so a write under a grant answers 503 and the zone view is empty.
	RecordGate dnsservice.DnsRecordGate
	// api: each root's DNSKEY window released at now (§12.6's Keys). nil is DNS-M5-19's
	// KeyWindowRelease over the window files in secret_files (hostKeyWindowRelease); server/local's test
	// dns edge signs its own.
	KeyWindow func(root string, now time.Time) *dnsservice.DnsKeyWindow
}

// The ports a role's block serves on, as warp maps them to the host's (WARP_PORTS).
type HostPorts struct {
	// GET /status's, which also serves the API's routes: 80 in a block.
	Status int
	// edge: DNS over UDP and TCP: 53 in a block.
	Dns int
}

// A role that could not start: never ready, and why in its log.
type notReadyEngine struct{}

func (notReadyEngine) Ready() bool { return false }
func (notReadyEngine) Drain()      {}
func (notReadyEngine) Close()      {}

// Reads name from the secret directory, or os.ErrNotExist.
func readHostSecret(settings *HostSettings, name string) ([]byte, error) {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return nil, fmt.Errorf("dns host: secret file %q", name)
	}
	return os.ReadFile(filepath.Join(settings.SecretDirectory, name))
}

// The warp service W4 sends a router interface's TCP 25 to: mxedge's router
// (§19.5), as against mxedge-platform on rpt.bringyour.com's two interfaces.
const hostRouterService = "mxedge"

// What the router list reads of services.yml's active lb version: the
// version's stream services, and each interface's own, which override the
// version's port by port (warp's SetDefaultStreamPortServices).
type hostStreamServicesYaml struct {
	Domain   string `yaml:"domain"`
	Versions []struct {
		Lb struct {
			TcpStreamPortServices map[int]string `yaml:"tcp_stream_port_services"`
			Interfaces            map[string]map[string]struct {
				TcpStreamPortServices map[int]string `yaml:"tcp_stream_port_services"`
			} `yaml:"interfaces"`
		} `yaml:"lb"`
	} `yaml:"versions"`
}

// The lb inventory's interfaces as a feed answer names them: every edge host's
// public interfaces, named host/interface, each a router (DNS-M7-01) when W4
// sends its TCP routerPort to mxedge's router, by its own
// tcp_stream_port_services or else its version's (§19.5). Until W4's M7 half
// names mxedge no interface is a router, and mx answers no address: the closed
// direction.
func hostLbInterfaces(servicesYaml []byte, routerPort int) ([]dnsservice.DnsLbInterface, error) {
	inventory, err := server.ParseServicesLbInventory(servicesYaml)
	if err != nil {
		return nil, err
	}
	var streamServices hostStreamServicesYaml
	if err := yaml.Unmarshal(servicesYaml, &streamServices); err != nil {
		return nil, fmt.Errorf("services.yml: %w", err)
	}
	if len(streamServices.Versions) == 0 {
		return nil, errors.New("services.yml: no active version")
	}
	active := &streamServices.Versions[0].Lb
	routerNames := map[string]bool{}
	for configuredHost, configuredInterfaces := range active.Interfaces {
		host := server.ServicesLbHostName(streamServices.Domain, configuredHost)
		for interfaceName, configured := range configuredInterfaces {
			service, ok := configured.TcpStreamPortServices[routerPort]
			if !ok {
				service = active.TcpStreamPortServices[routerPort]
			}
			routerNames[host+"/"+interfaceName] = service == hostRouterService
		}
	}
	interfaces := []dnsservice.DnsLbInterface{}
	for host, hostInterfaces := range inventory.Interfaces {
		for _, lbInterface := range hostInterfaces {
			entry := dnsservice.DnsLbInterface{Name: host + "/" + lbInterface.Interface}
			entry.Router = routerNames[entry.Name]
			if lbInterface.Ipv4 != "" {
				if entry.Ipv4, err = netip.ParseAddr(lbInterface.Ipv4); err != nil {
					return nil, err
				}
			}
			if lbInterface.Ipv6 != "" {
				if entry.Ipv6, err = netip.ParseAddr(lbInterface.Ipv6); err != nil {
					return nil, err
				}
			}
			interfaces = append(interfaces, entry)
		}
	}
	return interfaces, nil
}

// Each NS's SOA address: its glue in each root's root_records, on port 53.
func hostNsAddresses(settings *dnsservice.DnsSettings) map[string][]netip.AddrPort {
	addresses := map[string][]netip.AddrPort{}
	for _, records := range settings.RootRecords {
		for ns, glue := range records.Glue {
			for _, text := range glue {
				if address, err := netip.ParseAddr(text); err == nil {
					addresses[ns] = append(addresses[ns], netip.AddrPortFrom(address, hostDnsPort))
				}
			}
		}
	}
	return addresses
}

// The API role's DNSKEY windows (DNS-M5-19): each root's window file from
// secret_files (dnsservice.DnsKeyWindowFileName), verified by dns.yml's anchors.
// A root with no file has no window, so no edge it feeds becomes ready; a file
// that does not read or verify refuses the block, so a deploy reverts rather
// than serve a window no edge accepts.
func hostKeyWindowRelease(settings *HostSettings, host *sidecar.ServiceHost) (*dnsservice.KeyWindowRelease, error) {
	files := []*dnsservice.DnsKeyWindowFile{}
	for _, root := range settings.Dns.Roots {
		data, err := readHostSecret(settings, dnsservice.DnsKeyWindowFileName(root))
		if errors.Is(err, os.ErrNotExist) {
			glog.Warningf("[dns]no DNSKEY windows for %s: its edges serve nothing until a ceremony's file is mounted\n", root)
			continue
		}
		if err != nil {
			return nil, err
		}
		file, err := dnsservice.ParseDnsKeyWindowFile(data)
		if err != nil {
			return nil, fmt.Errorf("dns api: %s: %w", dnsservice.DnsKeyWindowFileName(root), err)
		}
		if file.Root != root {
			return nil, fmt.Errorf("dns api: %s holds %s's windows", dnsservice.DnsKeyWindowFileName(root), file.Root)
		}
		files = append(files, file)
	}
	metrics := host.Metrics
	if metrics == nil {
		return nil, errors.New("dns api: no metrics")
	}
	return dnsservice.NewKeyWindowRelease(settings.Dns, files, metrics, nil)
}

// The API role (see the file header). Returns the block, or a not-ready engine
// and why when it cannot start.
func NewApiHost(ctx context.Context, host *sidecar.ServiceHost, settings *HostSettings, deps *HostDeps) (sidecar.Engine, error) {
	if deps == nil {
		deps = &HostDeps{}
	}
	store := deps.Store
	if store == nil {
		if host.Redis == nil {
			return notReadyEngine{}, errors.New("dns api: no redis")
		}
		var err error
		if store, err = NewStore(host.Redis); err != nil {
			return notReadyEngine{}, err
		}
	}
	apiDeps := &dnsservice.DnsApiDeps{
		SoaReader:  dnsservice.NewDnsTcpSoaReader(hostNsAddresses(settings.Dns)),
		Challenges: NewChallenges(),
		Canaries:   NewCanaries(),
		KeyWindow:  deps.KeyWindow,
		Records:    NewRecords(),
		// no gate cache is wired into this block yet (plan-SOCK-gate-data's
		// source for it), so a write under a grant answers 503 and GET
		// /dns/zone shows no RRset: the closed direction
		RecordGate: deps.RecordGate,
	}
	// the record writes read §21.7's standing and network_verified from the
	// host's memory (ServiceHost.Abuse); a host with none answers them 503
	if host.Abuse != nil {
		model.SetDnsRecordsAbuseGate(host.Abuse)
	}
	if apiDeps.KeyWindow == nil {
		release, err := hostKeyWindowRelease(settings, host)
		if err != nil {
			return notReadyEngine{}, err
		}
		apiDeps.KeyWindow = release.Window
	}
	if verifyYaml, err := readHostSecret(settings, HostVerifyFileName); err == nil {
		auth, err := dnsservice.NewDnsInternalVerifier(verifyYaml)
		if err != nil {
			return notReadyEngine{}, err
		}
		apiDeps.Auth = auth
	} else if !errors.Is(err, os.ErrNotExist) {
		return notReadyEngine{}, err
	}
	if servicesYaml, err := readHostSecret(settings, HostServicesFileName); err == nil {
		interfaces, err := hostLbInterfaces(servicesYaml, settings.Dns.Probe.SmtpPort)
		if err != nil {
			return notReadyEngine{}, err
		}
		apiDeps.Interfaces = func() []dnsservice.DnsLbInterface {
			return interfaces
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return notReadyEngine{}, err
	}
	api, err := dnsservice.NewDnsApi(ctx, host, settings.Dns, store, apiDeps)
	if err != nil {
		return notReadyEngine{}, err
	}
	// <root>'s own records, which wait for the next start if they cannot be
	// written now (a restore, or no tables)
	if _, postgres := store.(*Store); postgres && server.SidecarMg5Linked() {
		if err := model.DnsReconcileRootRecords(ctx, settings.Dns); err != nil {
			glog.Warningf("[dns]root records wait for the next start: %v\n", err)
		}
	}
	return api, nil
}

// The edge role: an edge, its feed loop and its prober as one engine.
type EdgeHost struct {
	edge   *dnsservice.DnsEdge
	loop   *dnsservice.FeedLoop
	prober *dnsservice.Prober
}

var _ sidecar.StreamEngine = (*EdgeHost)(nil)

// The edge role (see the file header). Returns the engine, or a not-ready one
// and why when a file, a key or an anchor is missing or refused. host's Redis
// is never read.
func NewEdgeHost(ctx context.Context, host *sidecar.ServiceHost, settings *HostSettings, deps *HostDeps) (sidecar.StreamEngine, error) {
	if deps == nil {
		deps = &HostDeps{}
	}
	zskData, err := readHostSecret(settings, HostZskFileName)
	if err != nil {
		return notReadyStreamEngine{}, fmt.Errorf("dnsedge: zsk: %w", err)
	}
	var zsk crypto.Signer
	if zsk, err = dnsservice.LoadZsk(zskData); err != nil {
		return notReadyStreamEngine{}, fmt.Errorf("dnsedge: zsk: %w", err)
	}
	anchors := map[string][]*dns.DNSKEY{}
	for _, root := range settings.Dns.Roots {
		rootAnchors, err := dnsservice.ParseDnsTrustAnchors(root, settings.Dns.TrustAnchors[root])
		if err != nil {
			return notReadyStreamEngine{}, fmt.Errorf("dnsedge: %s's trust anchors: %w", root, err)
		}
		anchors[root] = rootAnchors
	}
	feed := deps.Feed
	routeProbes := deps.RouteProbes
	if feed == nil || routeProbes == nil {
		credentialData, err := readHostSecret(settings, settings.CredentialFileName)
		if err != nil {
			return notReadyStreamEngine{}, fmt.Errorf("dnsedge: credential: %w", err)
		}
		bearer, err := dnsservice.LoadDnsInternalCredential(credentialData)
		if err != nil {
			return notReadyStreamEngine{}, err
		}
		client := &http.Client{}
		if feed == nil {
			if feed, err = dnsservice.NewDnsFeedHttp(settings.ApiUrl, bearer, client, settings.Dns); err != nil {
				return notReadyStreamEngine{}, err
			}
		}
		if routeProbes == nil {
			if routeProbes, err = dnsservice.NewDnsRouteProbeSinkHttp(settings.ApiUrl, bearer, client, settings.Dns); err != nil {
				return notReadyStreamEngine{}, err
			}
		}
	}
	dialer := deps.ProbeDialer
	if dialer == nil {
		dialer = dnsservice.NewDnsNetProbeDialer()
	}
	edge, err := dnsservice.NewDnsEdge(ctx, host, settings.Dns, nil)
	if err != nil {
		return notReadyStreamEngine{}, err
	}
	prober, err := dnsservice.NewProber(ctx, host, settings.Dns, &dnsservice.ProberDeps{
		Dialer:      dialer,
		RouteProbes: routeProbes,
	}, edge.SetHealthySet)
	if err != nil {
		edge.Close()
		return notReadyStreamEngine{}, err
	}
	loop, err := dnsservice.NewFeedLoop(ctx, host, settings.Dns, &dnsservice.FeedLoopDeps{
		Feed:              feed,
		Edge:              edge,
		Zsk:               zsk,
		Anchors:           anchors,
		SnapshotDirectory: settings.SnapshotDirectory,
		RequireReload:     settings.RequireReload,
		OnInterfaces:      prober.SetInterfaces,
	})
	if err != nil {
		prober.Close()
		edge.Close()
		return notReadyStreamEngine{}, err
	}
	return &EdgeHost{edge: edge, loop: loop, prober: prober}, nil
}

// Whether the edge serves every root from a checked view.
func (self *EdgeHost) Ready() bool {
	return self.edge.Ready()
}

// The serial root is served at, zero before any view, and a channel that closes when it changes, so a
// caller waits for a serial with no sleep.
func (self *EdgeHost) ServedSerial(root string) (uint64, chan struct{}) {
	return self.edge.ServedSerial(root)
}

// Stops new TCP work and lets queries in flight finish.
func (self *EdgeHost) Drain() {
	self.edge.Drain()
}

// Stops the feed first, which writes the local snapshot, then the prober and
// the edge, which closes every socket.
func (self *EdgeHost) Close() {
	self.loop.Close()
	self.prober.Close()
	self.edge.Close()
}

// TCP 53, which the host binds.
func (self *EdgeHost) Serve(listener net.Listener) error {
	return self.edge.Serve(listener)
}

// UDP 53, bound by the role's main beside the host's TCP listener. Returns
// when the edge or conn closes.
func (self *EdgeHost) ServeUdp(conn net.PacketConn) error {
	return self.edge.ServePacket(conn)
}

// A stream role that could not start: never ready, and serving nothing.
type notReadyStreamEngine struct {
	notReadyEngine
}

func (notReadyStreamEngine) Serve(listener net.Listener) error {
	return listener.Close()
}

// The engine for role, api or edge: NewApiHost's or NewEdgeHost's, or a
// not-ready one and why for a role that is neither or cannot start.
func NewHost(ctx context.Context, role string, host *sidecar.ServiceHost, settings *HostSettings, deps *HostDeps) (sidecar.Engine, error) {
	switch role {
	case HostRoleApi:
		return NewApiHost(ctx, host, settings, deps)
	case HostRoleEdge:
		return NewEdgeHost(ctx, host, settings, deps)
	}
	return notReadyEngine{}, fmt.Errorf("dns host: role %q is neither %s nor %s", role, HostRoleApi, HostRoleEdge)
}

// Runs role as a block until ctx ends, through sidecarhost.Run (§19.4): the
// API on a host with Postgres, ready once MG2's and MG5's tables read, or the
// edge on a host with no Redis or Postgres handle at all (I-12), its UDP port
// bound beside the host's TCP listener. A role that cannot start logs why and
// serves /status not ready. Returns early only for a role that is neither, or
// options no block could run.
func RunHost(ctx context.Context, role string, settings *HostSettings, deps *HostDeps, ports *HostPorts) error {
	switch role {
	case HostRoleApi:
		host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{Postgres: true})
		engine, err := NewApiHost(ctx, host, settings, deps)
		if err != nil {
			glog.Errorf("[dns]the api role is not ready: %v\n", err)
		}
		options := &sidecarhost.RunOptions{
			Service:    "dns",
			StatusPort: ports.Status,
			Engine:     engine,
			Postgres:   true,
			Groups: []*sidecarhost.RunGroup{
				{Name: "MG2", Linked: server.SidecarMg2Linked},
				{Name: "MG5", Linked: server.SidecarMg5Linked},
			},
		}
		if httpEngine, ok := engine.(sidecar.HttpEngine); ok {
			options.Http = httpEngine
		}
		return sidecarhost.Run(ctx, options)
	case HostRoleEdge:
		host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{})
		// an edge holds no Redis or Postgres handle at all (I-12)
		host.Redis = nil
		engine, err := NewEdgeHost(ctx, host, settings, deps)
		if err != nil {
			glog.Errorf("[dns]the edge role is not ready: %v\n", err)
		}
		if edgeHost, ok := engine.(*EdgeHost); ok {
			go serveHostUdp(ctx, edgeHost, ports.Dns)
		}
		return sidecarhost.Run(ctx, &sidecarhost.RunOptions{
			Service:    "dnsedge",
			StatusPort: ports.Status,
			Engine:     engine,
			Stream:     engine,
			Listeners:  []*sidecarhost.RunListener{{Port: ports.Dns}},
		})
	}
	return fmt.Errorf("dns host: role %q is neither %s nor %s", role, HostRoleApi, HostRoleEdge)
}

// Binds the block's UDP port as warp maps it and serves it until ctx ends.
func serveHostUdp(ctx context.Context, edgeHost *EdgeHost, port int) {
	ipv4, _, hostPort := server.RequireListenIpPort(port)
	listenConfig := net.ListenConfig{}
	conn, err := listenConfig.ListenPacket(ctx, "udp", net.JoinHostPort(ipv4, strconv.Itoa(hostPort)))
	if err != nil {
		glog.Errorf("[dns]udp %d: %v\n", port, hostErrorWithoutAddress(err))
		return
	}
	context.AfterFunc(ctx, func() {
		conn.Close()
	})
	if err := edgeHost.ServeUdp(conn); err != nil && ctx.Err() == nil {
		glog.Errorf("[dns]udp %d ended: %v\n", port, hostErrorWithoutAddress(err))
	}
}

// The port alone names a socket in the log, which carries no address (I-13), and a net.OpError prints
// the addresses it failed on.
func hostErrorWithoutAddress(err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Err
	}
	return err
}
