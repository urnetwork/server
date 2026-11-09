package dnshost

// server/cli/dns's two roles (DNS-M5-18; §12.6, §19.4, D-DNS-3, R-DNS-8, I-12):
// one binary, a thin host each.
//
//   - api: dnsservice.NewDnsApi over Store, on dns.bringyour.com, with certs'
//     challenge writer, the bearer digests and services.yml's lb inventory
//     from its secret_files (reading 6: services.yml is mounted, since
//     server.Vault has nothing to read under {vault: no}), and each NS's SOA
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
	"strings"

	"github.com/miekg/dns"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
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
	// api: the store. nil is Store over host.Redis.
	Store dnsservice.DnsStore
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

// The lb inventory's interfaces as a feed answer names them: every edge host's
// public interfaces, named host/interface. The router list is §19.5's and M7's,
// so none is a router here.
func hostLbInterfaces(servicesYaml []byte) ([]dnsservice.DnsLbInterface, error) {
	inventory, err := server.ParseServicesLbInventory(servicesYaml)
	if err != nil {
		return nil, err
	}
	interfaces := []dnsservice.DnsLbInterface{}
	for host, hostInterfaces := range inventory.Interfaces {
		for _, lbInterface := range hostInterfaces {
			entry := dnsservice.DnsLbInterface{Name: host + "/" + lbInterface.Interface}
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
		interfaces, err := hostLbInterfaces(servicesYaml)
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
	if server.SidecarMg5Linked() {
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
