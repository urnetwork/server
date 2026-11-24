//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// Each block of the world (world_test.go) as one run of its exported Run on the world's stores, credentials and
// sources: what a block's main composes from its vault and config, composed here from the world's. Every run builds
// what a process of the block holds afresh, its gate cache and clients included, on the same stores, so chaos's
// restart reads back what the last run left as a restarted process does.

package sidecare2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/dnshost"
	"github.com/urnetwork/server/evshost"
	"github.com/urnetwork/server/gkm"
	"github.com/urnetwork/server/mcp"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/mxedgehost"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/smtpedgehost"
	"github.com/urnetwork/server/socketshost"
	"github.com/urnetwork/server/tlsedgehost"
)

// The port the dns edge takes DNS on, over TCP and UDP.
const worldDnsPort = 53

// The port mxedge's two listeners take SMTP on.
const worldSmtpPort = 25

// The port mxedge's router takes smtpedge's LAN splices on, mxedge.yml's lan_port (D-SMTP-13): a key of the block's
// direct listeners, which binds the world's fixed loopback address for it.
const worldMxLanPort = 2525

// The world's clusters as a block holds them: the T3World's Net's file clusters on the local stack's MinIO, which
// every block shares as production's share the site's. Ready reads true, since the Net read each cluster's admin
// info when it made the buckets, and Close leaves them to the world, which ends them with the Net.
type worldClusters struct {
	sidecar.BlobStore
}

func (self *worldClusters) Ready() bool {
	return true
}

func (self *worldClusters) Close() {}

// The clusters every block of the world holds.
func (self *World) clusters() *worldClusters {
	return &worldClusters{BlobStore: self.t3.Host().Blobs}
}

// The rev/ reader a block builds on the first cluster, each object verified under the world's svc- key.
func (self *World) revocations() *sidecar.RevocationChecker {
	settings := sidecar.DefaultRevocationSettings()
	settings.Clock = sidecar.RealClock{}
	return sidecar.NewRevocationChecker(self.t3.Host().Blobs, sidecarhost.RevocationVerify(self.t3.ServiceTokens()), settings)
}

// The authorization log's source a block with no Postgres reads through api's internal gate on caller's bearer
// (sidecar.AclGateApiSource, QD-680), at api's origin in process.
func (self *World) projectionSource(caller server.InternalGateCaller) (sidecar.AclGateSource, error) {
	bearer := self.bearer(caller)
	return sidecar.NewAclGateApiSource(self.apiOrigin(), self.apiClient(), func() (string, error) { return bearer, nil }, nil)
}

// The block named name, its run composed for the world.
func (self *World) newBlock(name string) (*worldBlock, error) {
	block := &worldBlock{world: self, name: name}
	switch name {
	case WorldBlockEvs:
		block.service = "evs"
		block.run = self.runEvs
	case WorldBlockGkm:
		block.service = "gkm"
		block.run = self.runGkm
	case WorldBlockSockets:
		block.service = "sockets"
		block.run = self.runSockets
	case WorldBlockTlsedge:
		block.service = "tlsedge"
		block.directPorts = tlsedgehost.HostServicePorts
		block.run = self.runTlsedge
	case WorldBlockSmtpedge:
		block.service = "smtpedge"
		block.run = self.runSmtpedge
	case WorldBlockMxedge:
		block.service = "mxedge"
		block.directPorts = []int{worldSmtpPort, worldMxLanPort}
		block.fixedAddresses = map[int]netip.AddrPort{worldMxLanPort: self.mxLanAddress}
		block.run = self.runMxedgeRouter
	case WorldBlockMxedgePlatform:
		block.directPorts = []int{worldSmtpPort}
		block.run = self.runMxedgePlatform
	case WorldBlockDns:
		block.service = "dns"
		run, err := self.dnsApiRun()
		if err != nil {
			return nil, err
		}
		block.run = run
	case WorldBlockDnsedge:
		block.directPorts = []int{worldDnsPort}
		block.directUdp = true
		run, err := self.dnsEdgeRun()
		if err != nil {
			return nil, err
		}
		block.run = run
	case WorldBlockMcp:
		block.service = "mcp"
		block.run = self.runMcp
	default:
		return nil, fmt.Errorf("no block is named %q", name)
	}
	return block, nil
}

// Closes every listener of listeners, for a run that fails before its block takes them.
func (self *worldBlockListeners) close() {
	self.status.Close()
	for _, listener := range self.direct {
		listener.Close()
	}
	if self.udp != nil {
		self.udp.Close()
	}
}

// evs on the world's clusters and Redis, its receipt keys and svc- verifier, its authorization state and gate views
// over a gate cache the world's syncs wait on, its write budget its own over the clusters, its Quota over api's
// internal quota surface on its bearer, and the clock check faked (the World's header).
func (self *World) runEvs(ctx context.Context, listeners *worldBlockListeners) error {
	cache, closeCache, err := self.t3.AddGate("evs")
	if err != nil {
		listeners.close()
		return err
	}
	defer closeCache()
	quota, endQuota, err := self.blockQuota(server.InternalGateCallerEvs)
	if err != nil {
		listeners.close()
		return err
	}
	defer endQuota()
	settings := evshost.DefaultHostSettings()
	settings.Run = self.HostsSettings().Run
	deps := &evshost.HostDeps{
		AuthState:      evsservice.NewAclEvsAuthState(cache, self.revocations()),
		GateViews:      evsservice.NewAclGateViews(cache),
		ReceiptKeys:    self.credentials.receiptKeys,
		Clusters:       self.clusters(),
		ClockCheck:     sidecartest.NewFakeClockCheck(nil),
		Quota:          quota,
		ServiceTokens:  self.t3.ServiceTokens(),
		StatusListener: listeners.status,
	}
	return evshost.RunHost(ctx, settings, deps, 0)
}

// gkm on the world's clusters and Redis, its svc- verifier, its authorization state over a gate cache the world's
// syncs wait on, its write budget its own, and its Quota over api's internal quota surface on its bearer.
func (self *World) runGkm(ctx context.Context, listeners *worldBlockListeners) error {
	cache, closeCache, err := self.t3.AddGate("gkm")
	if err != nil {
		listeners.close()
		return err
	}
	defer closeCache()
	quota, endQuota, err := self.blockQuota(server.InternalGateCallerGkm)
	if err != nil {
		listeners.close()
		return err
	}
	defer endQuota()
	settings := gkm.DefaultHostSettings()
	settings.Run = self.HostsSettings().Run
	deps := &gkm.HostDeps{
		AuthState:     gkmservice.NewAclGkmAuthState(cache, self.revocations(), sidecar.RealClock{}),
		Clusters:      self.clusters(),
		Quota:         quota,
		ServiceTokens: self.t3.ServiceTokens(),
	}
	if self.settings.GkmHintResolver {
		// the block's own hint writer, its sidecar ids resolved through api's internal route on the block's gkm bearer,
		// as server/cli/gkm builds it (plan-KEYS-hint-addressing, QD-1171)
		resolver, err := server.NewSidecarIdentityClientsResolver(&server.SidecarIdentityClientsResolverArgs{
			ApiUrl:     self.apiOrigin(),
			Bearer:     self.bearer(server.InternalGateCallerGkm),
			HttpClient: self.apiClient(),
		})
		if err != nil {
			listeners.close()
			return err
		}
		deps.PushResolver = resolver
	}
	return gkm.Run(ctx, gkm.RunOptions{StatusListener: listeners.status, Settings: settings, Deps: deps})
}

// sockets on the world's Redis and its store model's, its ticket key file, its svc- verifier and rev/ reader on the
// world's clusters, the log and tls/'s listen facts through api's internal gate on its own bearer, as its main
// sets them (SetGateApiSources), and its Quota over api's internal quota surface on that bearer.
func (self *World) runSockets(ctx context.Context, listeners *worldBlockListeners) error {
	quota, endQuota, err := self.blockQuota(server.InternalGateCallerSockets)
	if err != nil {
		listeners.close()
		return err
	}
	defer endQuota()
	settings := socketshost.DefaultHostSettings()
	settings.Run = self.HostsSettings().Run
	ticketKeyFile := self.credentials.ticketKeyFile
	deps := &socketshost.HostDeps{
		Quota:         quota,
		Blobs:         self.t3.Host().Blobs,
		KeyFile:       func() ([]byte, error) { return ticketKeyFile, nil },
		ServiceTokens: self.t3.ServiceTokens(),
	}
	if err := deps.SetGateApiSources(self.apiOrigin(), self.bearer(server.InternalGateCallerSockets), self.apiClient(), settings.ListenFactsMaxByteCount); err != nil {
		listeners.close()
		return err
	}
	return socketshost.Run(ctx, &socketshost.RunOptions{StatusListener: listeners.status, Settings: settings, Deps: deps})
}

// tlsedge.yml for the hosts' root, the env's one block.
func worldTlsEdgeSettings(hostsSettings *e2eHostsSettings) *tlsedgeservice.TlsEdgeSettings {
	tlsEdge := tlsedgeservice.DefaultTlsEdgeSettings()
	tlsEdge.Root = hostsSettings.TlsEdgeRoot
	tlsEdge.Blocks = 1
	return tlsEdge
}

// One run of an edge block's platform client source, as its main builds it on the edge bearer: api's edge client
// route through T3Api for clientCount clients of block on the world's edge host at slot 0, each client's jwt and
// sockets token minted in urnetwork-edge. It runs until the returned end, which the run calls once its Run has
// returned, so the drain still has its credentials.
func (self *World) edgeClientSource(block string, clientCount int) (*server.PlatformClientSource, func(), error) {
	settings := server.DefaultPlatformClientSourceSettings()
	settings.ClientCount = clientCount
	ctx, cancel := context.WithCancel(context.Background())
	source, err := server.NewPlatformClientSource(ctx, &server.PlatformClientSourceArgs{
		ApiUrl:     self.apiOrigin(),
		Block:      block,
		Host:       self.settings.EdgeHost,
		Slot:       0,
		Bearer:     self.bearer(server.InternalGateCallerEdge),
		HttpClient: self.apiClient(),
		Settings:   settings,
	})
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return source, func() {
		source.Close()
		cancel()
	}, nil
}

// How an edge block's dialer sends its Finds to sockets: at sockets' origin through the world's lb, on the first of
// its clients' sockets tokens.
func (self *World) socketFindArgs(source *server.PlatformClientSource) *sidecar.SocketFindHttpArgs {
	find := source.SocketFindHttpArgs(self.t3.Api().ServiceOrigin("sockets"))
	find.Transport = self.t3.Api().Transport()
	return find
}

// tlsedge on the world's Redis and its store in the test's Postgres (model.NewTlsEdgeStore), its four ports on
// listeners of their own behind its PROXY listener, and its K platform clients in urnetwork-edge on the exchange,
// minted by api's edge client route on the edge bearer, each Find at sockets' origin, and its Quota over api's internal
// quota surface on its bearer.
func (self *World) runTlsedge(ctx context.Context, listeners *worldBlockListeners) error {
	hostsSettings := self.HostsSettings()
	tlsEdge := worldTlsEdgeSettings(hostsSettings)
	settings := tlsedgehost.DefaultHostSettings(tlsEdge)
	settings.Run = hostsSettings.Run
	source, endSource, err := self.edgeClientSource("tlsedge", tlsEdge.Clients)
	if err != nil {
		listeners.close()
		return err
	}
	defer endSource()
	quota, endQuota, err := self.blockQuota(server.InternalGateCallerTlsedge)
	if err != nil {
		listeners.close()
		return err
	}
	defer endQuota()
	deps := &tlsedgehost.HostDeps{
		Quota:  quota,
		Source: source,
		Store:  model.NewTlsEdgeStore(nil),
		NewClients: func(ctx context.Context) (tlsedgehost.HostClients, error) {
			pool, err := sidecar.NewEdgeClientPool(ctx, &sidecar.EdgeClientPoolArgs{
				Source:   source,
				Platform: self.t3.Platform(),
				Settings: settings.Clients,
				Log:      tlsedgehost.HostLogger(),
			})
			if err != nil {
				return nil, err
			}
			return pool, nil
		},
		NewDial: func(clients tlsedgehost.HostClients) (tlsedgehost.HostSocketDial, error) {
			dialer, err := sidecar.NewSocketEdgeDialer(&sidecar.SocketEdgeDialerArgs{
				Clients: clients.Clients(),
				Find:    self.socketFindArgs(source),
				Settings: &sidecar.SocketEdgeDialSettings{
					FindTimeout:   tlsEdge.FindTimeout,
					FindCount:     tlsEdge.FindMaxCount,
					AcceptTimeout: tlsEdge.AcceptTicketTimeout,
				},
			})
			if err != nil {
				return nil, err
			}
			return func(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error) {
				stream, err := dialer.Dial(ctx, request)
				if err != nil {
					// never a typed nil as the engine's stream
					return nil, err
				}
				return stream, nil
			}, nil
		},
	}
	return tlsedgehost.Run(ctx, &tlsedgehost.RunOptions{StatusListener: listeners.status, Listeners: listeners.direct, Settings: settings, Deps: deps})
}

// smtpedge on the world's Postgres and Redis: its send store (model.NewSmtpEdgeStore), the abuse gate, the stock
// gate over the log through api's internal gate on its bearer and the model's facts, its X-UR-Ref keys; its resolver
// the world's DNSSEC for a network's names and the internet sender's, validated from the world's own anchor
// (world_mail_dns_test.go), and sidecartest's MX world for every other name, the PTR and the dialer, but for the
// internet sender's MX, which the world serves, beside a fake proof verifier (the World's header); mxedge's router at
// the world's lb interface reached over the LAN splice to the
// router's LAN listener (router_interfaces, router_lan; D-SMTP-13); its platform clients in urnetwork-edge, registered
// with sockets on their own sockets tokens, and its Quota over api's internal quota surface on its bearer.
func (self *World) runSmtpedge(ctx context.Context, listeners *worldBlockListeners) error {
	hostsSettings := self.HostsSettings()
	config, err := smtpedgeservice.LoadSmtpEdgeConfig([]byte(fmt.Sprintf(
		"root: %s\nreport_host: %s\nhelo_domain: %s\nsending_addresses:\n  - {address: %s, helo: %s}\nrouter_interfaces: [%s]\nrouter_lan: %s\n",
		hostsSettings.SmtpRoot, hostsSettings.SmtpReportHost, hostsSettings.SmtpHeloDomain, hostsSettings.SmtpAddress, hostsSettings.SmtpHeloName,
		self.settings.LbAddress, self.mxLanAddress,
	)), 0)
	if err != nil {
		listeners.close()
		return err
	}
	config.Settings.XUrRefKeys = self.credentials.xUrRefKeys
	// the lb's interface is a documentation address, which the edge refuses to dial as it refuses the ranges it never
	// sends to; only its prefix is let through, so every other refused range stays refused
	refused := []netip.Prefix{}
	for _, prefix := range config.Settings.MxRefusedPrefixes {
		if !prefix.Contains(self.settings.LbAddress) {
			refused = append(refused, prefix)
		}
	}
	config.Settings.MxRefusedPrefixes = refused
	config.Settings.GateRetryTimeout = self.settings.SmtpGateRetryTimeout
	config.Settings.MxRetryTimeout = self.settings.SmtpMxRetryTimeout
	config.Settings.MxBannerBackoffTimeout = self.settings.SmtpMxBannerBackoffTimeout
	world, err := sidecartest.NewMxWorld(sidecartest.DefaultMxWorldSettings())
	if err != nil {
		listeners.close()
		return err
	}
	if err := world.AddMx(hostsSettings.SmtpHeloName, hostsSettings.SmtpAddress, nil); err != nil {
		listeners.close()
		return err
	}
	world.SetPtr(hostsSettings.SmtpAddress, hostsSettings.SmtpHeloName)
	anchors, err := sidecar.NewSmtpDnssecAnchors(world.TrustAnchorDs())
	if err != nil {
		listeners.close()
		return err
	}
	resolver := sidecar.NewSmtpMxDnsResolver(world, anchors, nil, sidecar.RealClock{})
	mxResolver, err := newWorldSmtpMxResolver(self.dnssec, resolver)
	if err != nil {
		listeners.close()
		return err
	}
	projection, err := self.projectionSource(server.InternalGateCallerSmtpedge)
	if err != nil {
		listeners.close()
		return err
	}
	settings := smtpedgehost.DefaultHostSettings(config)
	settings.Run = hostsSettings.Run
	source, endSource, err := self.edgeClientSource("smtpedge", settings.Clients.ClientCount)
	if err != nil {
		listeners.close()
		return err
	}
	defer endSource()
	registrations := source.SocketPlatformRegistrationsArgs(self.t3.Api().ServiceOrigin("sockets"))
	registrations.Transport = self.t3.Api().Transport()
	quota, endQuota, err := self.blockQuota(server.InternalGateCallerSmtpedge)
	if err != nil {
		listeners.close()
		return err
	}
	defer endQuota()
	deps := &smtpedgehost.HostDeps{
		Quota:      quota,
		Source:     source,
		Abuse:      model.NewAbuseGate(ctx, model.NewAbuseHardDenySnapshot(ctx, nil), nil),
		Store:      model.NewSmtpEdgeStore(nil),
		Projection: projection,
		Facts:      smtpedgehost.NewModelGateSource(config.Settings.Root, quota),
		Resolver:   mxResolver,
		Ptr:        resolver,
		Dialer:     self.smtpMxDialer(world),
		Verifier:   sidecartest.NewFakeSmtpProofVerifier(nil),
		NewClients: func(ctx context.Context) (smtpedgehost.HostClients, error) {
			return smtpedgehost.NewEdgeClients(ctx, sidecar.EdgeClientPoolArgs{
				Source:   source,
				Platform: self.t3.Platform(),
				Settings: settings.Clients,
				Log:      smtpedgehost.HostLogger(),
			})
		},
		Registrations: registrations,
	}
	return smtpedgehost.Run(ctx, &smtpedgehost.RunOptions{StatusListener: listeners.status, Settings: settings, Deps: deps})
}

// mxedge's router on the world's Postgres and Redis and its store (model.NewMxEdgeStore), TCP 25 on a listener of its
// own and its LAN listener on the world's fixed loopback address, which smtpedge's LAN splices reach from the
// world's lan_sources (D-SMTP-13), and its K platform clients in urnetwork-edge on the exchange, minted by api's edge
// client route on the edge bearer, each Find at sockets' origin, and its Quota over api's internal quota surface on
// its bearer; its PTR and host lookups faked (the World's header).
func (self *World) runMxedgeRouter(ctx context.Context, listeners *worldBlockListeners) error {
	settings := e2eMxEdgeSettings(self.HostsSettings())
	copied := *settings.MxEdge
	copied.LanPort = worldMxLanPort
	copied.LanSources = self.settings.MxLanSources
	settings.MxEdge = &copied
	mxEdge := settings.MxEdge
	source, endSource, err := self.edgeClientSource("mxedge", mxEdge.Clients)
	if err != nil {
		listeners.close()
		return err
	}
	defer endSource()
	poolSettings := mxedgehost.DefaultRouterSettings(mxEdge).Clients
	quota, endQuota, err := self.blockQuota(server.InternalGateCallerMxedge)
	if err != nil {
		listeners.close()
		return err
	}
	defer endQuota()
	deps := &mxedgehost.RouterDeps{
		Quota:       quota,
		Source:      source,
		Store:       model.NewMxEdgeStore(nil),
		StoreGroups: mxedgehost.RouterStoreGroups(),
		NewClients: func(ctx context.Context) (mxedgehost.RouterClients, error) {
			pool, err := sidecar.NewEdgeClientPool(ctx, &sidecar.EdgeClientPoolArgs{
				Source:   source,
				Platform: self.t3.Platform(),
				Settings: poolSettings,
				Log:      mxedgehost.HostLogger(),
			})
			if err != nil {
				return nil, err
			}
			return pool, nil
		},
		NewDial: func(clients mxedgehost.RouterClients) (mxedgehost.RouterSocketDial, error) {
			dialer, err := sidecar.NewSocketEdgeDialer(&sidecar.SocketEdgeDialerArgs{
				Clients: clients.Clients(),
				Find:    self.socketFindArgs(source),
				Settings: &sidecar.SocketEdgeDialSettings{
					FindTimeout:   mxEdge.FindTimeout,
					FindCount:     mxEdge.FindCount,
					AcceptTimeout: mxEdge.AcceptTicketTimeout,
				},
			})
			if err != nil {
				return nil, err
			}
			return dialer.Dial, nil
		},
		LookupPtr: func(ctx context.Context, sender netip.Addr) ([]string, error) {
			return nil, errors.New("the test stack resolves no PTR")
		},
		LookupHost: func(ctx context.Context, name string) ([]netip.Addr, error) {
			return nil, errors.New("the test stack resolves no name")
		},
		StatusListener: listeners.status,
		SmtpListener:   listeners.direct[worldSmtpPort],
		LanListener:    listeners.direct[worldMxLanPort],
	}
	return mxedgehost.RunHost(ctx, mxedgehost.HostRoleRouter, settings, nil, deps, &mxedgehost.HostPorts{Smtp: worldSmtpPort, Lan: worldMxLanPort})
}

// mxedge's platform listener on the world's Postgres and Redis with its certificate and its report intake as mxedge's
// main composes it, each run its own (world_report_intake_test.go), ready once the groups the intake reads are linked,
// TCP 25 on a listener of its own, which the world's lb hands the platform's interface (world_lb_test.go).
func (self *World) runMxedgePlatform(ctx context.Context, listeners *worldBlockListeners) error {
	settings := e2eMxEdgeSettings(self.HostsSettings())
	intake, intakeGroups, err := self.platformReportIntake(settings.MxEdge)
	if err != nil {
		listeners.close()
		return err
	}
	deps := &mxedgehost.HostDeps{
		Intake:         intake,
		IntakeGroups:   intakeGroups,
		Certificate:    self.credentials.platformCertificate,
		StatusListener: listeners.status,
		SmtpListener:   listeners.direct[worldSmtpPort],
	}
	return mxedgehost.RunHost(ctx, mxedgehost.HostRolePlatform, settings, deps, nil, &mxedgehost.HostPorts{Smtp: worldSmtpPort})
}

// The world's dns.yml: the hosts' root, anchored at the credentials' KSK.
func (self *World) dnsSettings() (*dnsservice.DnsSettings, error) {
	return e2eDnsSettings(self.HostsSettings(), self.credentials, nil)
}

// The dns API's run on the world's Postgres and Redis, releasing the credentials' key window, with the edge's bearer
// digests, the world's lb interfaces as its inventory (world_lb_test.go), and the log through api's internal gate on
// its own bearer (HostDeps.Projection).
func (self *World) dnsApiRun() (func(ctx context.Context, listeners *worldBlockListeners) error, error) {
	dnsSettings, err := self.dnsSettings()
	if err != nil {
		return nil, err
	}
	secretDirectory, err := os.MkdirTemp(self.t.TempDir(), "dns-api-")
	if err != nil {
		return nil, err
	}
	window := self.credentials.dnsWindow
	return func(ctx context.Context, listeners *worldBlockListeners) error {
		projection, err := self.projectionSource(server.InternalGateCallerDns)
		if err != nil {
			listeners.close()
			return err
		}
		settings := &dnshost.HostSettings{
			Dns:             dnsSettings,
			SecretDirectory: secretDirectory,
			Run:             self.HostsSettings().Run,
		}
		deps := &dnshost.HostDeps{
			KeyWindow: func(root string, now time.Time) *dnsservice.DnsKeyWindow {
				if root != window.Root {
					return nil
				}
				return window
			},
			Auth:           self.credentials.dnsAuth,
			Interfaces:     self.lbInterfaces(),
			SoaReader:      &worldSoaReader{world: self},
			Projection:     projection,
			ServiceTokens:  self.t3.ServiceTokens(),
			StatusListener: listeners.status,
		}
		return dnshost.RunHost(ctx, dnshost.HostRoleApi, settings, deps, nil)
	}, nil
}

// The API's read of each NS's SOA, over TCP from the world's dns edge, which answers for every NS of the root: what
// the glue's addresses reach in production. An error while the edge is stopped.
type worldSoaReader struct {
	world *World
}

func (self *worldSoaReader) ReadSoa(ctx context.Context, root string, ns string) (uint32, error) {
	edge := self.world.Block(WorldBlockDnsedge)
	if edge == nil {
		return 0, errors.New("the world runs no dns edge")
	}
	address, err := netip.ParseAddrPort(edge.DirectAddress(worldDnsPort))
	if err != nil {
		return 0, fmt.Errorf("the dns edge is stopped: %w", err)
	}
	return dnsservice.NewDnsTcpSoaReader(map[string][]netip.AddrPort{ns: {address}}).ReadSoa(ctx, root, ns)
}

// The dns edge's run, feeding from the dns API at its origin through the world's lb, as an edge reaches
// dns.bringyour.com, with its ZSK and bearer, its probes through the world's lb interface to its tlsedge and mxedge
// (world_lb_test.go), and DNS on TCP and UDP on listeners of its own; its local snapshot in a directory every run of
// the world's shares, which a restart serves from.
func (self *World) dnsEdgeRun() (func(ctx context.Context, listeners *worldBlockListeners) error, error) {
	dnsSettings, err := self.dnsSettings()
	if err != nil {
		return nil, err
	}
	directory := self.t.TempDir()
	secretDirectory := filepath.Join(directory, "secret")
	snapshotDirectory := filepath.Join(directory, "snapshot")
	for _, path := range []string{secretDirectory, snapshotDirectory} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, err
		}
	}
	dnsOrigin := self.t3.Api().ServiceOrigin("dns")
	bearer := self.credentials.dnsBearer
	return func(ctx context.Context, listeners *worldBlockListeners) error {
		client := &http.Client{Transport: self.t3.Api().Transport()}
		feed, err := dnsservice.NewDnsFeedHttp(dnsOrigin, bearer, client, dnsSettings)
		if err != nil {
			listeners.close()
			return err
		}
		routeProbes, err := dnsservice.NewDnsRouteProbeSinkHttp(dnsOrigin, bearer, client, dnsSettings)
		if err != nil {
			listeners.close()
			return err
		}
		settings := &dnshost.HostSettings{
			Dns:               dnsSettings,
			SecretDirectory:   secretDirectory,
			ApiUrl:            dnsOrigin,
			SnapshotDirectory: snapshotDirectory,
			Run:               self.HostsSettings().Run,
		}
		deps := &dnshost.HostDeps{
			Zsk:            self.credentials.dnsZsk,
			Feed:           feed,
			RouteProbes:    routeProbes,
			ProbeDialer:    &worldLbProbeDialer{world: self},
			StatusListener: listeners.status,
			DnsListener:    listeners.direct[worldDnsPort],
			DnsPacketConn:  listeners.udp,
		}
		return dnshost.RunHost(ctx, dnshost.HostRoleEdge, settings, deps, nil)
	}, nil
}

// mcp on the world's Postgres and Redis with the world's seal key and McpResource its origin, its readiness the
// migration-aware check alone: the tools' warmup loads process-wide state, the IP databases and the location searches,
// that would outlive the world.
func (self *World) runMcp(ctx context.Context, listeners *worldBlockListeners) error {
	httpServer := self.HostsSettings().Run.HttpServer
	return mcp.Run(ctx, mcp.RunOptions{Listener: listeners.status, HttpServer: &httpServer, Startup: router.StartupReadiness})
}
