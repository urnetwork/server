//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world every e2e scenario runs in (e2e-world; the owner's end-to-end tests of 2026-11-21): every server service
// of sidecar's in the test process, each on its real stores, with real sidecar clients enrolled in several networks.
// It builds on sidecart3's T3World and the hosts e2e-hosts-in-process made runnable in one process, rather than beside
// them:
//
//	stores    the T3World's Net: server.DefaultTestEnv's Postgres database and leased Redis, migrations applied, and
//	          file clusters in buckets of the world's own on the local stack's MinIO; every block holds those clusters
//	          and that Redis, as production's blocks share the site's
//	api       T3Api, server's api and oauth in process, with api's installs over its gate cache (the sequencer, the
//	          exchange's scopes, approvals, dns scopes, edge logins, rev/'s store, the issuer's worker, and §8.10's
//	          contract gate when WorldSettings.AclContractGate turns sidecar.yml's switch on), the sequencer and rev/'s
//	          store spending on api's own write budget over the world's Redis, which reports beside each block's
//	          (T3WorldSettings.ApiWriteBudget), and api's internal gate surface on the world's throwaway bearers
//	          (ServeInternalGate), as a block reaches api
//	exchange  server's exchange and its connect handler on loopback, which every sidecar's and edge client's connect
//	          client dials, with sidecar.yml's push switch on, so each sidecar's client hears its hints from the
//	          exchange's resident publisher as in production (WorldSettings.Push)
//	blocks    each new service's block through its own exported Run, on a listener the OS picks, its routes served at
//	          its origin through T3Api.ServeService by the world's lb (worldLb: http over loopback to the block, with
//	          X-UR-Forwarded-For set as warp's nginx sets it), and its direct ports on listeners of their own:
//	          tlsedge's TLS behind its PROXY listener, smtpedge's and mxedge's SMTP, dnsedge's DNS on TCP and UDP
//	quota     every block's Quota reads and writes server's ledgers through api's internal quota surface on its own
//	          bearer (quotahost's api-backed HostQuota), each network funded by a transfer balance of server's
//	edges     tlsedge, smtpedge and mxedge's router hold their K platform clients in urnetwork-edge, a network the world
//	          makes: each run's server.PlatformClientSource asks api's edge client route on the edge bearer for each
//	          client's jwt and sockets token, under the world's edge.yml, and sidecar.EdgeClientPool opens the clients
//	          on the exchange, their Finds and registrations at sockets' origin, so each edge reads ready as in
//	          production
//	clients   sidecart3's T3Network and T3Device: full sidecar.Sidecar cores on memory instances, enrolled through
//	          T3Api from their owners' auth keys, Online on the exchange, their service origins at the blocks
//
// Every block verifies service tokens and rev/ under the T3World's svc- verifier and reads its authorization state from
// a gate cache the world's syncs wait on (T3World.AddGate), so a statement's filer knows the block holds it before a
// device acts. What a block reads from vault in production, the world makes: the svc- key and access-token key
// (T3World), evs's receipt keys, sockets' ticket keys, smtpedge's X-UR-Ref keys, the dns API's bearer digests, the
// edge's bearer and ZSK, the dns root's KSK window, mxedge-platform's certificate, mcp's seal secret, and each internal
// gate caller's bearer with api's digests of them. The blocks run under pro.yml's sidecar limits the T3World sets
// (§20.4's Community column by default) and a sidecar.yml that opens every service's gate to every network.
//
// What stays a fake, each because the test stack cannot give the real part, named where it is built:
//
//   - the clock check (evs): chronyd on warp's services network, which no test host runs; sidecartest's FakeClockCheck
//     answers an offset of zero;
//   - smtpedge's MX world (resolver, PTR, dialer, proof verifier): sidecartest's MxWorld and FakeSmtpProofVerifier,
//     since the test stack resolves no public name and dials no public MX; mxedge's router's PTR and host lookups
//     answer nothing for the same reason;
//   - the dns edge's lb probes: refused, since no lb interface answers in the test stack;
//   - gkm's hint writer's sidecar_id resolver: plan-KEYS-hint-addressing has api answer it on an internal route, POST
//     /sidecar/identity/clients, which is not built, so production's block loses every hint by sidecar id; the world's
//     block resolves them from sidecar_identity_binding instead (worldPushClientResolver, WorldSettings.GkmHintResolver),
//     so a late delivery's wrap-pending and pending hints reach the sidecars they name;
//   - keys: the T3World's FakeGkm, whose deliveries reach each device, and the config repo's heads in memory; gkm's
//     block runs and answers. sidecart3's T3WorldSettings.RealKeys gives each device its real key client at gkm's
//     block and the config heads on evs, as ur daemon composes them (world_keys.go); there a joining device takes the
//     config repo's key through the resolver above, but a repo's creation is refused by gkm's cached 404 of its
//     placement, so the world runs FakeGkm by default (world_keys_test.go has the evidence).
//
// Process-wide state, so one world runs at a time and its Close puts each back: the T3World's (sidecar.yml, pro.yml's
// limits, oauth's issuer and keys, WARP_DOMAIN, api's installs), evs.yml, mcp's seal secret and McpResource, and the
// T3Api's internal gate handler. mcp's /status latch is router's global one, so the world runs one mcp.

package sidecare2e

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/evshost"
	"github.com/urnetwork/server/mcp"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecart3"
)

// The blocks a world runs, by name.
const (
	WorldBlockEvs            = "evs"
	WorldBlockGkm            = "gkm"
	WorldBlockSockets        = "sockets"
	WorldBlockTlsedge        = "tlsedge"
	WorldBlockSmtpedge       = "smtpedge"
	WorldBlockMxedge         = "mxedge"
	WorldBlockMxedgePlatform = "mxedgeplatform"
	WorldBlockDns            = "dns"
	WorldBlockDnsedge        = "dnsedge"
	WorldBlockMcp            = "mcp"
)

// Every block, in the order a world starts them: the dns API before the edge that feeds from it.
func WorldBlockNames() []string {
	return []string{
		WorldBlockEvs,
		WorldBlockGkm,
		WorldBlockSockets,
		WorldBlockTlsedge,
		WorldBlockSmtpedge,
		WorldBlockMxedge,
		WorldBlockMxedgePlatform,
		WorldBlockDns,
		WorldBlockDnsedge,
		WorldBlockMcp,
	}
}

// What a world runs with (QD-702): every count, bound and name it uses.
type WorldSettings struct {
	// The T3World the stores, api, oauth, exchange and clients are. nil: its defaults, with every service's gate
	// opened on sidecar.yml (NewWorld sets SidecarYaml whatever this holds).
	T3 *sidecart3.T3WorldSettings
	// The blocks' names, roots, credentials' windows and waits, as e2e-hosts-in-process's harness has them. nil:
	// defaultE2eHostsSettings.
	Hosts *e2eHostsSettings
	// The blocks the world starts, by name: every one.
	Blocks []string
	// What each block must answer on /status before the world is made, by name: ready, or merely up for a block whose
	// readiness waits on a part the world does not give it yet (the smtpedge's platform clients). A block named
	// nowhere must be ready.
	StartStatus map[string]string
	// M, the networks AddNetworks makes, and N, the sidecars each enrolls.
	NetworkCount      int
	ClientsPerNetwork int
	// The storage format evs's block runs as, which evs.yml names (QD-854): format 2, production's from M10's release;
	// format 1 runs a rollback's block, its guard refusing format-2 pushes.
	EvsInstanceFormat uint32
	// sidecar.yml's acl_contract_gate switch (ACL-M4-01): on, connect's CreateContract consults §8.10's contract gate,
	// which the T3World installs over api's gate cache as api's process does when the switch is on at its start. false,
	// as production ships the switch.
	AclContractGate bool
	// The bytes of each internal gate caller's bearer: the format's (server.DefaultInternalGateSettings).
	InternalGateBearerByteCount int
	// How long a block may take to stop, its Run returning, when the world closes or chaos stops it.
	StopTimeout time.Duration
	// The host every edge block's platform clients name, as edge.yml lists it, and the environment api derives their
	// ids under: plain names of the world's own.
	EdgeHost        string
	EdgeEnvironment string
	// How long a helper waits out api's 429 on a base or statement past §7.9's limits, its retry_after, before it
	// gives up: a base a minute, so past one minute.
	RateLimitWaitTimeout time.Duration
	// How many of each sidecar's newest log lines the world keeps for a stalled phase's dump (FailProgress).
	DumpLogLineCount int
	// sidecar.yml's push switch and the exchange's EnableSidecarPush, as connect's process reads the switch at its
	// start: each sidecar's client hears its hints from the exchange's resident publisher (§6.8). true.
	Push bool
	// Whether gkm's hint writer resolves sidecar ids through the world's stand-in for api's unbuilt internal route
	// (worldPushClientResolver), so a hint by sidecar id reaches its sidecar's client: true. false runs gkm's own
	// writer, which resolves none, as production's block does today.
	GkmHintResolver bool
	// Each sidecar's gkm hint resync under RealKeys (T3WorldSettings.KeyResyncInterval): 0, the sidecar's own hourly
	// one, since the hints reach every sidecar.
	KeyResyncInterval time.Duration
}

// The defaults: every block, two networks of three sidecars.
func DefaultWorldSettings() *WorldSettings {
	return &WorldSettings{
		Hosts:                       defaultE2eHostsSettings(),
		Blocks:                      WorldBlockNames(),
		StartStatus:                 map[string]string{},
		NetworkCount:                2,
		ClientsPerNetwork:           3,
		EvsInstanceFormat:           sidecar.EvsRepoFormat2,
		AclContractGate:             false,
		InternalGateBearerByteCount: server.DefaultInternalGateSettings().BearerByteCount,
		StopTimeout:                 90 * time.Second,
		EdgeHost:                    "e2e-edge-1",
		EdgeEnvironment:             "e2e",
		RateLimitWaitTimeout:        3 * time.Minute,
		DumpLogLineCount:            40,
		Push:                        true,
		GkmHintResolver:             true,
		KeyResyncInterval:           0,
	}
}

// The two answers StartStatus may ask of a block's /status.
const (
	worldStatusReady = "ready"
	worldStatusUp    = "up"
)

// sidecar.yml as a world puts it: preview_networks, every service's gate open to every network, evs's with the env's
// one block, urnetwork_edge the edges' clients' network, the push switch when push is on, and the contract gate's when
// the world turns it on.
func worldSidecarYaml(urnetworkEdge server.Id, push bool, aclContractGate bool) func(previewNetworkIds []string) string {
	return func(previewNetworkIds []string) string {
		lines := []string{
			"sidecar:",
			"  preview_networks: [" + strings.Join(previewNetworkIds, ", ") + "]",
			"  evs: {enabled: true, percent: 100, blocks: 1}",
			"  gkm: {enabled: true, percent: 100}",
			"  sockets: {enabled: true, percent: 100}",
			"  tlsedge: {enabled: true, percent: 100}",
			"  mxedge: {enabled: true, percent: 100}",
			"  smtpedge: {enabled: true, percent: 100}",
			"  dns: {enabled: true, percent: 100}",
			"  urnetwork_edge: " + urnetworkEdge.String(),
		}
		if push {
			lines = append(lines, "  push: {enabled: true}")
		}
		if aclContractGate {
			lines = append(lines, "  "+server.SidecarSwitchAclContractGate+": {enabled: true}")
		}
		return strings.Join(lines, "\n") + "\n"
	}
}

// See the file's header. Safe for concurrent use once made.
type World struct {
	t           testing.TB
	settings    *WorldSettings
	t3          *sidecart3.T3World
	credentials *e2eCredentials
	// each internal gate caller's bearer, which api's verifier holds the digest of
	bearers map[server.InternalGateCaller]string
	// urnetwork-edge, the edges' clients' network
	urnetworkEdge server.Id
	// the case's bound from the world's start
	caseDeadline time.Time
	// GET /status with no connection kept
	statusClient *http.Client
	// the lb's connections to the blocks
	lbTransport *http.Transport

	blocks []*worldBlock

	// each sidecar's newest log lines, by its name, at most DumpLogLineCount
	logLock         sync.Mutex
	sidecarLogLines map[string][]string

	stateLock sync.Mutex
	networks  []*WorldNetwork
	// what Close undoes, newest first
	closes []func()
	closed bool
}

// A world on the private stores and the local stack's MinIO, inside a TestEnv that applies the migrations: every
// block of settings.Blocks started and answering as StartStatus asks. Closed at t's end, or by Close. settings nil
// takes the defaults.
func NewWorld(t testing.TB, settings *WorldSettings) *World {
	t.Helper()
	if settings == nil {
		settings = DefaultWorldSettings()
	}
	hostsSettings := settings.Hosts
	if hostsSettings == nil {
		hostsSettings = defaultE2eHostsSettings()
	}
	self := &World{
		t:             t,
		settings:      settings,
		urnetworkEdge: server.NewId(),
		caseDeadline:  time.Now().Add(hostsSettings.CaseTimeout),
		statusClient:  &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
		lbTransport:   &http.Transport{DisableCompression: true},
	}
	t.Cleanup(self.Close)

	t3Settings := settings.T3
	if t3Settings == nil {
		t3Settings = sidecart3.DefaultT3WorldSettings()
	} else {
		copied := *t3Settings
		t3Settings = &copied
	}
	t3Settings.SidecarYaml = worldSidecarYaml(self.urnetworkEdge, settings.Push, settings.AclContractGate)
	t3Settings.KeyResyncInterval = settings.KeyResyncInterval
	// api spends on a budget of its own over the world's Redis, as its process does, never the Net's fake
	t3Settings.ApiWriteBudget = true
	exchangeSettings := sidecart3.DefaultExchangeSettings()
	if t3Settings.Exchange != nil {
		copied := *t3Settings.Exchange
		exchangeSettings = &copied
	}
	if exchangeSettings.Exchange != nil {
		copied := *exchangeSettings.Exchange
		copied.EnableSidecarPush = settings.Push
		exchangeSettings.Exchange = &copied
	}
	t3Settings.Exchange = exchangeSettings
	sidecarSettings := sidecart3.DefaultT3SidecarSettings()
	if t3Settings.Sidecar != nil {
		copied := *t3Settings.Sidecar
		sidecarSettings = &copied
	}
	sidecarSettings.LogTee = self.keepSidecarLogLine
	t3Settings.Sidecar = sidecarSettings
	t3, err := sidecart3.NewT3World(t, t3Settings)
	if err != nil {
		t.Fatalf("the T3 world: %v", err)
	}
	self.t3 = t3
	self.credentials = newE2eCredentials(t, hostsSettings, time.Now())

	// what every block shares process-wide
	self.closes = append(self.closes, mcp.Testing_SetSealSecret(self.credentials.sealSecret))
	previousMcpResource := mcp.McpResource
	mcp.McpResource = t3.Api().ServiceOrigin(WorldBlockMcp)
	self.closes = append(self.closes, func() { mcp.McpResource = previousMcpResource })
	self.closes = append(self.closes, server.Config.PushSimpleResource(evshost.BlockConfigFileName, []byte(fmt.Sprintf("# the e2e world's evs\ninstance_format: %d\n", settings.EvsInstanceFormat))))
	// urnetwork-edge's row, which api's edge client route mints the edges' clients in
	worldCreateEdgeNetwork(t.Context(), self.urnetworkEdge)

	if err := self.installInternalGate(); err != nil {
		t.Fatalf("api's internal gate: %v", err)
	}

	for _, name := range settings.Blocks {
		block, err := self.newBlock(name)
		if err != nil {
			t.Fatalf("the %s block: %v", name, err)
		}
		self.blocks = append(self.blocks, block)
	}
	for _, block := range self.blocks {
		if err := block.start(); err != nil {
			t.Fatalf("the %s block: %v", block.name, err)
		}
		// the dns edge feeds from the API, so each block is up as asked before the next starts
		if err := self.awaitStartStatus(block); err != nil {
			t.Fatal(err)
		}
	}
	return self
}

// Keeps line as name's newest, dropping its oldest past DumpLogLineCount. Never blocks on anything but its own lock.
func (self *World) keepSidecarLogLine(name string, line string) {
	self.logLock.Lock()
	defer self.logLock.Unlock()
	if self.sidecarLogLines == nil {
		self.sidecarLogLines = map[string][]string{}
	}
	lines := append(self.sidecarLogLines[name], line)
	if excess := len(lines) - self.settings.DumpLogLineCount; 0 < excess {
		lines = slices.Clone(lines[excess:])
	}
	self.sidecarLogLines[name] = lines
}

// name's newest log lines, oldest first.
func (self *World) SidecarLogLines(name string) []string {
	self.logLock.Lock()
	defer self.logLock.Unlock()
	return slices.Clone(self.sidecarLogLines[name])
}

// The T3World under the world: its api, exchange, networks and syncs.
func (self *World) T3() *sidecart3.T3World {
	return self.t3
}

// The world's settings.
func (self *World) Settings() *WorldSettings {
	return self.settings
}

// The blocks' settings: their names, roots and waits.
func (self *World) HostsSettings() *e2eHostsSettings {
	if self.settings.Hosts != nil {
		return self.settings.Hosts
	}
	return defaultE2eHostsSettings()
}

// Fires once timeout has passed or the case's deadline has, whichever is first.
func (self *World) stepDeadline(timeout time.Duration) <-chan time.Time {
	return time.After(min(timeout, time.Until(self.caseDeadline)))
}

// A context that ends at timeout or the case's deadline, whichever is first, or with parent.
func (self *World) StepContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(timeout)
	if self.caseDeadline.Before(deadline) {
		deadline = self.caseDeadline
	}
	return context.WithDeadline(parent, deadline)
}

// Each internal gate caller's bearer, 256 random bits, and api's digests of them: the verify file api's internal gate
// surface checks callers against, served in place of api's own (T3Api.ServeInternalGate) until Close.
func (self *World) installInternalGate() error {
	self.bearers = map[server.InternalGateCaller]string{}
	verifyLines := []string{}
	for _, caller := range server.InternalGateCallers() {
		raw := make([]byte, self.settings.InternalGateBearerByteCount)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		bearer := base64.RawURLEncoding.EncodeToString(raw)
		self.bearers[caller] = bearer
		digest := sha256.Sum256([]byte(bearer))
		verifyLines = append(verifyLines, fmt.Sprintf("%s: {class: %s, sha256: [%s]}", caller, caller, hex.EncodeToString(digest[:])))
	}
	verifier, err := server.NewInternalGateVerifier([]byte(strings.Join(verifyLines, "\n") + "\n"))
	if err != nil {
		return err
	}
	urnetworkEdge := self.urnetworkEdge
	edgeConfig, err := server.ParseEdgeClientsConfig([]byte(self.edgeClientsYaml()), nil)
	if err != nil {
		return err
	}
	environment := self.settings.EdgeEnvironment
	routes := slices.Concat(
		handlers.NewQuotaServiceHandlers(nil, &handlers.QuotaServiceDeps{Verifier: verifier}).Routes(),
		handlers.NewInternalGateProjectionHandlers(&handlers.InternalGateProjectionDeps{Verifier: verifier}).Routes(),
		handlers.NewInternalGateEdgeClientHandlers(&handlers.InternalGateEdgeClientDeps{
			Verifier:    verifier,
			Config:      func() (*server.EdgeClientsConfig, error) { return edgeConfig, nil },
			EdgeNetwork: func() (server.Id, bool) { return urnetworkEdge, true },
			Environment: func() (string, error) { return environment, nil },
		}).Routes(),
		handlers.NewInternalGateAbuseNoticeHandlers(&handlers.InternalGateAbuseNoticeDeps{Verifier: verifier}).Routes(),
	)
	self.t3.Api().ServeInternalGate(router.NewRouter(self.t3.Context(), routes))
	self.closes = append(self.closes, func() { self.t3.Api().ServeInternalGate(nil) })
	return nil
}

// edge.yml as api reads it for the world: the exchange's url, and each edge block's K clients on the world's one edge
// host.
func (self *World) edgeClientsYaml() string {
	hostsSettings := self.HostsSettings()
	host := self.settings.EdgeHost
	lines := []string{
		"platform_url: " + self.t3.Exchange().PlatformUrl(),
		"blocks:",
		fmt.Sprintf("  tlsedge: {clients: %d, hosts: [%s]}", worldTlsEdgeSettings(hostsSettings).Clients, host),
		fmt.Sprintf("  smtpedge: {clients: %d, hosts: [%s]}", sidecar.DefaultEdgeClientPoolSettings().ClientCount, host),
		fmt.Sprintf("  mxedge: {clients: %d, hosts: [%s]}", e2eMxEdgeSettings(hostsSettings).MxEdge.Clients, host),
	}
	return strings.Join(lines, "\n") + "\n"
}

// The bearer caller sends to api's internal gate.
func (self *World) bearer(caller server.InternalGateCaller) string {
	return self.bearers[caller]
}

// A client of api's internal gate as a block's is: T3Api's transport, so the request reaches api in process at its
// origin, never following a redirect off it.
func (self *World) apiClient() *http.Client {
	return &http.Client{
		Transport: self.t3.Api().Transport(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// The origin api serves at.
func (self *World) apiOrigin() string {
	return self.t3.Api().Settings().ApiOrigin
}

// The block named name, nil for one the world does not run.
func (self *World) Block(name string) *worldBlock {
	for _, block := range self.blocks {
		if block.name == name {
			return block
		}
	}
	return nil
}

// Waits until block's /status answers as StartStatus asks.
func (self *World) awaitStartStatus(block *worldBlock) error {
	switch self.settings.StartStatus[block.name] {
	case worldStatusUp:
		_, err := block.AwaitStatus("up", e2eStatusAnswers)
		return err
	default:
		_, err := block.AwaitStatus("ok", e2eStatusReady)
		return err
	}
}

// Ends the world: its blocks, newest first, then what it installed, then the T3World with its networks and stores.
// Safe to call again.
func (self *World) Close() {
	var closes []func()
	closed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.closed = true
		closes = self.closes
		self.closes = nil
		return true
	}()
	if !closed {
		return
	}
	// the sidecars first, so no block drains under a client's retries
	if self.t3 != nil {
		self.t3.CloseNetworks()
	}
	for index := len(self.blocks) - 1; 0 <= index; index -= 1 {
		if err := self.blocks[index].stop(); err != nil {
			self.t.Errorf("%v", err)
		}
	}
	for index := len(closes) - 1; 0 <= index; index -= 1 {
		closes[index]()
	}
	if self.t3 != nil {
		self.t3.Close()
	}
	self.lbTransport.CloseIdleConnections()
	self.statusClient.CloseIdleConnections()
}

// One block of the world: how to run it, and its current run, which chaos stops and starts again on the same stores.
type worldBlock struct {
	world *World
	name  string
	// the origin the lb serves it at (T3Api.ServeService), "" for a block no client reaches over http
	service string
	// the service ports it takes direct connections on, each on a listener of its own, and whether its DNS port takes
	// UDP too
	directPorts []int
	directUdp   bool
	// one run of the block until ctx ends, on its listeners, which it takes
	run func(ctx context.Context, listeners *worldBlockListeners) error

	stateLock sync.Mutex
	current   *worldBlockRun
	// how many runs have started
	runCount int
}

// The listeners one run of a block is given, each on a port the OS picked.
type worldBlockListeners struct {
	status net.Listener
	// by service port
	direct map[int]net.Listener
	udp    net.PacketConn
}

// One run of a block.
type worldBlockRun struct {
	statusAddress   string
	directAddresses map[int]string
	udpAddress      string
	cancel          context.CancelFunc
	// closed once Run has returned, err then holding what it returned
	done chan struct{}
	err  error
}

// The block's name.
func (self *worldBlock) Name() string {
	return self.name
}

// How many runs of the block have started: 1 until chaos restarts it.
func (self *worldBlock) RunCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.runCount
}

// The current run, nil while the block is stopped.
func (self *worldBlock) currentRun() *worldBlockRun {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.current
}

// The address the current run takes port's direct connections on, "" while the block is stopped.
func (self *worldBlock) DirectAddress(port int) string {
	if run := self.currentRun(); run != nil {
		return run.directAddresses[port]
	}
	return ""
}

// The address the current run takes DNS over UDP on, "" while the block is stopped.
func (self *worldBlock) UdpAddress() string {
	if run := self.currentRun(); run != nil {
		return run.udpAddress
	}
	return ""
}

// Starts a run on fresh listeners, its origin served through the world's lb from the first run on.
func (self *worldBlock) start() error {
	listeners := &worldBlockListeners{direct: map[int]net.Listener{}}
	closeListeners := func() {
		if listeners.status != nil {
			listeners.status.Close()
		}
		for _, listener := range listeners.direct {
			listener.Close()
		}
		if listeners.udp != nil {
			listeners.udp.Close()
		}
	}
	var err error
	if listeners.status, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		return err
	}
	run := &worldBlockRun{
		statusAddress:   listeners.status.Addr().String(),
		directAddresses: map[int]string{},
		done:            make(chan struct{}),
	}
	for _, port := range self.directPorts {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			closeListeners()
			return err
		}
		listeners.direct[port] = listener
		run.directAddresses[port] = listener.Addr().String()
	}
	if self.directUdp {
		if listeners.udp, err = net.ListenPacket("udp", "127.0.0.1:0"); err != nil {
			closeListeners()
			return err
		}
		run.udpAddress = listeners.udp.LocalAddr().String()
	}
	started := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.current != nil {
			return false
		}
		self.current = run
		self.runCount += 1
		return true
	}()
	if !started {
		closeListeners()
		return fmt.Errorf("%s runs already", self.name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	go func() {
		defer close(run.done)
		run.err = self.run(ctx, listeners)
	}()
	if self.service != "" && self.RunCount() == 1 {
		if err := self.world.t3.Api().ServeService(self.service, &worldLb{block: self}); err != nil {
			self.stop()
			return err
		}
	}
	return nil
}

// Ends the current run, the block's SIGTERM, and waits for its Run to drain, close and return nil. Nothing while the
// block is stopped.
func (self *worldBlock) stop() error {
	run := func() *worldBlockRun {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		run := self.current
		self.current = nil
		return run
	}()
	if run == nil {
		return nil
	}
	run.cancel()
	select {
	case <-run.done:
		if run.err != nil {
			return fmt.Errorf("%s's Run returned %v", self.name, run.err)
		}
		return nil
	case <-time.After(self.world.settings.StopTimeout):
		return fmt.Errorf("%s's Run did not return within %s of its end", self.name, self.world.settings.StopTimeout)
	}
}

// GET /status's status on the current run, false when it answers none or the block is stopped.
func (self *worldBlock) Status() (string, bool) {
	run := self.currentRun()
	if run == nil {
		return "", false
	}
	response, err := self.world.statusClient.Get("http://" + run.statusAddress + "/status")
	if err != nil {
		return "", false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", false
	}
	result := &router.WarpStatusResult{}
	if err := decodeJson(response.Body, result); err != nil {
		return "", false
	}
	return result.Status, true
}

// Waits until the current run's /status answers a status want takes, and returns it; an error with the last status
// past the hosts' StatusTimeout or the case's deadline, or once the run ended. /status is the block's own latch, with
// no hook to wait on, so it is asked every StatusPollInterval.
func (self *worldBlock) AwaitStatus(what string, want func(status string) bool) (string, error) {
	settings := self.world.HostsSettings()
	deadline := self.world.stepDeadline(settings.StatusTimeout)
	last := "no answer"
	for {
		run := self.currentRun()
		if run == nil {
			return "", fmt.Errorf("%s is stopped, so its /status is never %s", self.name, what)
		}
		if status, ok := self.Status(); ok {
			if want(status) {
				return status, nil
			}
			last = status
		}
		select {
		case <-run.done:
			return "", fmt.Errorf("%s ended before its /status was %s: %v", self.name, what, run.err)
		case <-deadline:
			return "", fmt.Errorf("%s's /status was never %s within its step's or the case's bound; last %q", self.name, what, last)
		case <-time.After(settings.StatusPollInterval):
		}
	}
}

// The chaos hook: stops the named block mid-run, as a crash or a deploy's SIGTERM does, and starts it again on the same
// stores and new listeners, then waits until it answers as the world's start asked. Its origin answers 503 between the
// two, as the lb does with no upstream, and a client's retries carry it over.
func (self *World) Restart(name string) error {
	block := self.Block(name)
	if block == nil {
		return fmt.Errorf("the world runs no %s", name)
	}
	if err := block.stop(); err != nil {
		return err
	}
	if err := block.start(); err != nil {
		return err
	}
	return self.awaitStartStatus(block)
}

// Stops the named block until Start: chaos's first half, for a scenario that acts while it is down.
func (self *World) Stop(name string) error {
	block := self.Block(name)
	if block == nil {
		return fmt.Errorf("the world runs no %s", name)
	}
	return block.stop()
}

// Starts the named block again after Stop, on the same stores and new listeners, and waits until it answers as the
// world's start asked.
func (self *World) Start(name string) error {
	block := self.Block(name)
	if block == nil {
		return fmt.Errorf("the world runs no %s", name)
	}
	if err := block.start(); err != nil {
		return err
	}
	return self.awaitStartStatus(block)
}

// The lb in front of one block's routes, as warp's nginx is: each request to the block's origin goes over loopback
// http to the block's current run, its host the origin's, X-UR-Forwarded-For the address the request came from and the
// forwarding headers a client could send cleared (router/ingress.go's contract), its answer streamed back. 503 while
// the block is stopped.
type worldLb struct {
	block *worldBlock
}

func (self *worldLb) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	run := self.block.currentRun()
	if run == nil {
		http.Error(w, "no upstream", http.StatusServiceUnavailable)
		return
	}
	target := &url.URL{Scheme: "http", Host: run.statusAddress}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			proxyRequest.SetURL(target)
			proxyRequest.Out.Host = proxyRequest.In.Host
			proxyRequest.Out.Header.Del("X-Forwarded-For")
			proxyRequest.Out.Header.Del("X-Forwarded-Source-Port")
			proxyRequest.Out.Header.Del("X-UR-Request-Buffering")
			proxyRequest.Out.Header.Set("X-UR-Forwarded-For", proxyRequest.In.RemoteAddr)
		},
		Transport:     self.block.world.lbTransport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "upstream failed", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

var errWorldClosed = errors.New("the world has closed")
