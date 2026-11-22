//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// One device's bridge in the it-bridge case (§17.10): what ur daemon builds for a network when ur tun asks
// (sidecar/cli's bridge_wire.go and bridge_host.go, ported, since cli is package main), on a world sidecar.
//
//	role       a device.Bridge on a file posture store of the device's own, which outlives the wire, so a restart
//	           restores the posture before any generation serves (DP1)
//	component  the root's BridgeComponent, the Sidecar's "bridge" component, over the device's config follower: its
//	           client, head reader, kept rows, the config repo's keys and its acl follower's local policy and engine;
//	           it compiles each state and serves bridge flows on each client generation through a LocalUserNat with
//	           the role's hook in its settings, whose dials go to the device's dialer (it_bridge_targets_test.go)
//	links      device.BridgeHost: each link a RemoteUserNatMultiClient of one window client to its target, its
//	           generator minting each window client a child of the sidecar's own client through the Sidecar's own api
//	           (POST /network/auth-client, DPoP-signed, admin_refused and strict_exp confirmed) and opening it on
//	           server's exchange, its out-of-band control server's controller in process, as the world sidecars'
//	           own clients have it; device.NewBridgeLinkGenerator signs each one's WindowDelegation through
//	           Sidecar.SignWindowDelegation and holds its contracts to the target's binding, which GET
//	           /sidecar/identity answers and the role verifies against the identity key the rules pin
//	stack      a userspace connect.Tun on gVisor whose every packet the role routes (Bridge.Route): a link's, a
//	           reject's or the resolver's client, or the default route, which goes nowhere here and is counted; the
//	           links' replies and the role's rejects are written back into it, an ICMP echo reply to one of the
//	           case's echoes taken off first
//
// A window client the links mint stays a client of the network after its link drops it, as connect's own generator's
// credential removal is not run here: the case reads every contract's close afterwards (§20.6's ledgers). Safe for
// concurrent use.

package sidecare2e

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/device"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

// What each device's bridge runs with (QD-702).
type ItBridgeWireSettings struct {
	// Edits the device role's settings, which start as DefaultBridgeSettings with the Sidecar's signer and api's
	// bindings. Nil leaves them.
	Bridge func(settings *device.BridgeSettings)
	// Edits the component's settings, which start as DefaultBridgeComponentSettings with the NAT's dials on the
	// device's dialer. Nil leaves them.
	Component func(settings *sidecar.BridgeComponentSettings)
	// The packets the stack queues toward the role (connect.TunSettings' channel size): 64.
	TunChannelSize int
	// How long one window client's mint through api may take: 30 s.
	MintTimeout time.Duration
	// How long a window client's open may wait for the controller's ack of its provide modes and for server to hold
	// the key it publishes in band, and how often it looks for the key meanwhile, since nothing acks it: 60 s, 20 ms.
	WindowClientOpenTimeout     time.Duration
	KeyRegistrationPollInterval time.Duration
	// How long a send waits for its link to take a packet: 5 s.
	SendTimeout time.Duration
	// How long closing a window client, the stack or the wire may take: 30 s.
	CloseTimeout time.Duration
	// The app version a window client's auth names.
	AppVersion string
	// The bytes an ICMP echo the case sends carries.
	EchoPayload []byte
}

func DefaultItBridgeWireSettings() *ItBridgeWireSettings {
	return &ItBridgeWireSettings{
		TunChannelSize:              64,
		MintTimeout:                 30 * time.Second,
		WindowClientOpenTimeout:     60 * time.Second,
		KeyRegistrationPollInterval: 20 * time.Millisecond,
		SendTimeout:                 5 * time.Second,
		CloseTimeout:                30 * time.Second,
		AppVersion:                  "0.0.0",
		EchoPayload:                 []byte("it bridge echo"),
	}
}

// One device of the case: its world sidecar, and what its bridge keeps across a restart, its posture store and its
// NAT dialer, beside its current wire.
type itBridgeDevice struct {
	world    *ItBridgeWorld
	t3       *sidecart3.T3Device
	name     string
	settings *ItBridgeWireSettings
	// the posture's directory, which every wire of the device restores from
	postureDirectory string
	dialer           *itBridgeDialer

	stateLock sync.Mutex
	wire      *itBridgeWire
	// how many wires the device has built: 1 until a restart
	wireCount int
	// a filing device's filer, and its gate's stand-in (it_bridge_world_test.go); nil on a client
	filer *sidecar.BridgeFiler
	gate  *itBridgeStandingGate
}

// The device's id in the network's config.
func (self *itBridgeDevice) SidecarId() sidecar.Id {
	return self.t3.Sidecar().Identity().SidecarId
}

// The device's current wire, nil while it is down.
func (self *itBridgeDevice) currentWire() *itBridgeWire {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.wire
}

// How many wires the device has built.
func (self *itBridgeDevice) WireCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.wireCount
}

// Builds the device's bridge (the file's header) and adds its component to the Sidecar. Refused while one runs.
func (self *itBridgeDevice) startWire() error {
	wire, err := newItBridgeWire(self)
	if err != nil {
		return err
	}
	kept := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.wire != nil {
			return false
		}
		self.wire = wire
		self.wireCount += 1
		return true
	}()
	if !kept {
		wire.close()
		return fmt.Errorf("%s's bridge runs already", self.name)
	}
	return nil
}

// Ends the device's bridge, as a stop of its daemon does: the component first, then the role and its links, then the
// stack. The posture's store stays for the next wire. Nothing while it is down.
func (self *itBridgeDevice) stopWire() {
	wire := func() *itBridgeWire {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		wire := self.wire
		self.wire = nil
		return wire
	}()
	if wire != nil {
		wire.close()
	}
}

// One wire of a device (the file's header).
type itBridgeWire struct {
	device   *itBridgeDevice
	settings *ItBridgeWireSettings
	ctx      context.Context
	cancel   context.CancelFunc

	bridge          *device.Bridge
	component       *sidecar.BridgeComponent
	removeComponent func()
	links           *itBridgeLinkHost
	tun             *connect.Tun
	source          netip.Addr
	workers         sync.WaitGroup
	removeCallbacks []func()
	// the posture's pin at the role's last publish
	adoptedPin *connect.MonitorValue[uint64]
	// moves at each echo reply or ICMP error taken off the stack's path
	icmpChanges *connect.MonitorValue[int]

	stateLock sync.Mutex
	// each echo the case sent, by id<<16|sequence, and when its reply came; zero for none yet
	echoReplies map[uint32]time.Time
	// each echo an ICMP error answered, by id<<16|sequence, with the error's type
	echoErrors map[uint32]byte
	// packets the role sent down the default route, and sends a link refused
	defaultRouteCount int
	refusedSendCount  int
	closed            bool
}

// The device's wire, its component added to its Sidecar. A failure leaves nothing running.
func newItBridgeWire(owner *itBridgeDevice) (returnWire *itBridgeWire, returnErr error) {
	running := owner.t3.Sidecar()
	follower := owner.t3.Follower()
	if follower == nil || follower.Client() == nil {
		return nil, fmt.Errorf("%s runs a bridge by a follower that stands", owner.name)
	}
	identity := running.Identity()
	if identity == nil {
		return nil, fmt.Errorf("%s has no identity", owner.name)
	}
	ctx, cancel := context.WithCancel(owner.world.world.T3().Context())
	self := &itBridgeWire{
		device:      owner,
		settings:    owner.settings,
		ctx:         ctx,
		cancel:      cancel,
		adoptedPin:  connect.NewMonitorValue[uint64](0),
		icmpChanges: connect.NewMonitorValue(0),
		echoReplies: map[uint32]time.Time{},
		echoErrors:  map[uint32]byte{},
	}
	defer func() {
		if returnErr != nil {
			self.close()
		}
	}()

	bridgeSettings := device.DefaultBridgeSettings()
	bridgeSettings.LinkSign = self.sign
	bridgeSettings.LinkTargetKey = self.targetKey
	if self.settings.Bridge != nil {
		self.settings.Bridge(bridgeSettings)
	}
	store, err := device.NewFilePostureStore(owner.postureDirectory)
	if err != nil {
		return nil, err
	}
	self.links = &itBridgeLinkHost{wire: self, settings: bridgeSettings}
	self.bridge, err = device.NewBridge(ctx, store, self.links, bridgeSettings)
	if err != nil {
		return nil, fmt.Errorf("%s's device role: %w", owner.name, err)
	}
	self.removeCallbacks = append(self.removeCallbacks, self.bridge.AddChangeCallback(func() {
		pin := self.bridge.Decision().Pin.GetVersion()
		self.adoptedPin.Update(func(adopted uint64) uint64 { return max(adopted, pin) })
	}))
	// a restart's role restores its posture before any change, which publishes nothing new while the tip has not moved
	restored := self.bridge.Decision().Pin.GetVersion()
	self.adoptedPin.Update(func(adopted uint64) uint64 { return max(adopted, restored) })
	self.bridge.SetReceivePacket(self.receive)

	componentSettings := sidecar.DefaultBridgeComponentSettings()
	natSettings := connect.DefaultLocalUserNatSettings()
	dialSettings := &connect.DialContextSettings{DialContext: owner.dialer.dial}
	natSettings.TcpBufferSettings.DialContextSettings = dialSettings
	natSettings.UdpBufferSettings.DialContextSettings = dialSettings
	componentSettings.Nat = natSettings
	if self.settings.Component != nil {
		self.settings.Component(componentSettings)
	}
	// the engine an approval gate and a preview ask holds the routes in force the role's state was compiled from
	installRoutes := func(servedRoutes *sidecar.AclServedRoutes) {
		if engine := follower.AclEngine(); engine != nil {
			engine.InstallRoutes(servedRoutes)
		}
	}
	self.component, err = sidecar.NewBridgeComponent(ctx, &sidecar.BridgeComponentArgs{
		NetworkId:    identity.NetworkId,
		Identity:     running.Identity,
		Config:       follower.Client(),
		Heads:        follower.Heads(),
		Rows:         follower.Rows(),
		RepoKeyAt:    follower.RepoKeyAt,
		AclLocal:     follower.AclLocal,
		ServedRoutes: installRoutes,
		OnAttach:     self.links.attach,
		Emit:         owner.world.noteAlarm(owner),
		Log:          owner.t3.Settings.Log,
		Settings:     componentSettings,
	})
	if err != nil {
		return nil, fmt.Errorf("%s's bridge component: %w", owner.name, err)
	}
	if err := self.component.SetDeviceRole(&itBridgeSink{bridge: self.bridge}); err != nil {
		return nil, fmt.Errorf("%s's device role: %w", owner.name, err)
	}

	tunSettings := connect.DefaultTunSettingsWithBufferSize(self.settings.TunChannelSize)
	tunSettings.Log = connect.NewNoopLogger()
	self.tun, err = connect.CreateTun(ctx, tunSettings)
	if err != nil {
		return nil, fmt.Errorf("%s's stack: %w", owner.name, err)
	}
	for _, address := range self.tun.LocalAddresses() {
		if address.Is4() {
			self.source = address
		}
	}
	if !self.source.IsValid() {
		return nil, fmt.Errorf("%s's stack has no IPv4 address", owner.name)
	}
	self.workers.Go(self.pump)

	self.removeComponent, err = running.AddComponent(self.component)
	if err != nil {
		return nil, fmt.Errorf("%s's bridge component: %w", owner.name, err)
	}
	return self, nil
}

// The address the stack sends from.
func (self *itBridgeWire) Source() netip.Addr {
	return self.source
}

// The component's status.
func (self *itBridgeWire) Status() sidecar.BridgeComponentStatus {
	return self.component.Status()
}

// device.BridgeSignFunction over the Sidecar's signer (§17.9's sign row), as cli's urBridgeWire.sign: it refuses what
// the Sidecar would not sign.
func (self *itBridgeWire) sign(ctx context.Context, delegation *bridge.WindowDelegation) ([]byte, error) {
	ids := []sidecar.Id{}
	for _, idBytes := range [][]byte{delegation.GetNetworkId(), delegation.GetSubjectSidecarId(), delegation.GetWindowClientId(), delegation.GetTargetSidecarId()} {
		id, err := sidecar.IdFromBytes(idBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: a delegation's id", sidecar.ErrBridgeDelegationRefused)
		}
		ids = append(ids, id)
	}
	if 0 < len(delegation.GetShareId()) {
		return nil, fmt.Errorf("%w: the case runs no share", sidecar.ErrBridgeDelegationRefused)
	}
	return self.device.t3.Sidecar().SignWindowDelegation(&sidecar.BridgeDelegationRequest{
		NetworkId:             ids[0],
		SubjectSidecarId:      ids[1],
		WindowClientId:        ids[2],
		WindowClientPublicKey: delegation.GetWindowClientPublicKey(),
		TargetSidecarId:       ids[3],
		NotAfter:              time.UnixMilli(delegation.GetNotAfterMs()),
	})
}

// device.BridgeTargetKeyFunction: the binding api answers for the target client, which the link verifies against the
// identity key the rules pin, so api stays untrusted (§17.9's target row).
func (self *itBridgeWire) targetKey(ctx context.Context, targetSidecarId connect.Id, targetClientId connect.Id) ([]byte, error) {
	generation := self.links.currentGeneration()
	if generation == nil {
		return nil, errors.New("no client generation")
	}
	record, err := generation.api.GetIdentityByClient(ctx, sidecar.Id(targetClientId))
	if err != nil {
		return nil, err
	}
	if record.SidecarId != sidecar.Id(targetSidecarId) {
		return nil, errors.New("api names another sidecar for the target client")
	}
	return record.Binding, nil
}

// Routes the stack's packets until the wire closes (the file's header's stack row).
func (self *itBridgeWire) pump() {
	for {
		packet, err := self.tun.Read()
		if err != nil {
			return
		}
		self.route(packet)
	}
}

// Sends one pooled packet the way the role routes it, which takes it.
func (self *itBridgeWire) route(packet []byte) bool {
	client := self.bridge.Route(packet)
	if client == nil {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.defaultRouteCount += 1
		}()
		connect.MessagePoolReturn(packet)
		return false
	}
	source := self.links.sourcePath()
	if !client.SendPacket(source, protocol.ProvideMode_Network, packet, self.settings.SendTimeout) {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.refusedSendCount += 1
		}()
		connect.MessagePoolReturn(packet)
		return false
	}
	return true
}

// connect.ReceivePacketFunction for the links' replies and the role's rejects: borrowed, so the stack's write copies
// it, and an ICMP answer to one of the case's echoes is noted and kept off the stack.
func (self *itBridgeWire) receive(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
	if key, icmpType, ok := itBridgeEchoAnswer(packet); ok {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if icmpType == 0 {
				if self.echoReplies[key].IsZero() {
					self.echoReplies[key] = time.Now()
				}
			} else {
				self.echoErrors[key] = icmpType
			}
		}()
		self.icmpChanges.Update(func(count int) int { return count + 1 })
		return
	}
	self.tun.Write(packet)
}

// The packets the role sent down the default route, and the sends a link refused.
func (self *itBridgeWire) Counts() (defaultRouteCount int, refusedSendCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.defaultRouteCount, self.refusedSendCount
}

// An ICMP echo from the stack's address to destination, id and sequence its own, routed as the stack's packets are.
// Whether a link took it.
func (self *itBridgeWire) SendEcho(destination netip.Addr, id uint16, sequence uint16) bool {
	request := itBridgeEchoRequest(self.source, destination, id, sequence, self.settings.EchoPayload)
	packet := connect.MessagePoolGet(len(request))
	copy(packet, request)
	return self.route(packet)
}

// Waits until the echo of id and sequence is answered, a reply true, an ICMP error or timeout false, with the error's
// type, 0 for none.
func (self *itBridgeWire) WaitEcho(id uint16, sequence uint16, timeout time.Duration) (bool, byte) {
	key := uint32(id)<<16 | uint32(sequence)
	deadline := time.After(timeout)
	for {
		_, changed := self.icmpChanges.Get()
		replied, icmpType := func() (bool, byte) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return !self.echoReplies[key].IsZero(), self.echoErrors[key]
		}()
		if replied || icmpType != 0 {
			return replied, icmpType
		}
		select {
		case <-changed:
		case <-deadline:
			return false, 0
		case <-self.ctx.Done():
			return false, 0
		}
	}
}

// A TCP connection through the stack to address.
func (self *itBridgeWire) DialTcp(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
	return self.tun.DialContext(ctx, "tcp4", address.String())
}

// A UDP socket of the stack's connected to address.
func (self *itBridgeWire) DialUdp(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
	return self.tun.DialContext(ctx, "udp4", address.String())
}

// Ends the wire: the component, out of the Sidecar, then the role and its links, the stack and the window clients.
// Idempotent.
func (self *itBridgeWire) close() {
	first := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.closed = true
		return true
	}()
	if !first {
		return
	}
	if self.removeComponent != nil {
		self.removeComponent()
	} else if self.component != nil {
		self.component.Close()
	}
	for _, remove := range self.removeCallbacks {
		remove()
	}
	if self.bridge != nil {
		self.bridge.Close()
	}
	if self.tun != nil {
		self.tun.Close()
	}
	self.cancel()
	self.workers.Wait()
	if self.links != nil {
		self.links.close()
	}
}

// device.Bridge as the component's sink, as cli's urBridgeSink: each call through, a log run converted to the
// posture's, and the decision read off what the role adopted.
type itBridgeSink struct {
	bridge *device.Bridge
}

var _ sidecar.BridgeStateSink = (*itBridgeSink)(nil)

func (self *itBridgeSink) SetPending(pending *bridge.Pending) error {
	return self.bridge.SetPending(pending)
}

func (self *itBridgeSink) SetState(state *bridge.BridgeState, log *sidecar.BridgeStateLog) error {
	return self.bridge.SetState(state, itBridgePostureLog(log))
}

func (self *itBridgeSink) ApplyDeltas(log *sidecar.BridgeStateLog) error {
	return self.bridge.ApplyDeltas(itBridgePostureLog(log))
}

func (self *itBridgeSink) SetFork(fork bool) error {
	return self.bridge.SetFork(fork)
}

func (self *itBridgeSink) Decision() sidecar.BridgeSinkDecision {
	decision := self.bridge.Decision()
	sinkDecision := sidecar.BridgeSinkDecision{Pin: decision.Pin, Fork: decision.Fork}
	switch decision.State {
	case device.PostureLegacy:
		sinkDecision.State = sidecar.BridgeSinkLegacy
	case device.PostureEnforcingDocument, device.PostureEnforcingNoDocument:
		sinkDecision.State = sidecar.BridgeSinkEnforcing
	default:
		sinkDecision.State = sidecar.BridgeSinkPending
	}
	if decision.BridgeState != nil {
		sinkDecision.StatePin = decision.BridgeState.GetAuth()
	}
	return sinkDecision
}

func (self *itBridgeSink) Revoke(statementType bridge.SignedType, id []byte, n uint64) error {
	return self.bridge.Revoke(statementType, id, n)
}

func (self *itBridgeSink) EgressHook() connect.EgressHook {
	return self.bridge.EgressHook()
}

// A log run as the posture reads it.
func itBridgePostureLog(log *sidecar.BridgeStateLog) *device.PostureLog {
	if log == nil {
		return nil
	}
	return &device.PostureLog{Entries: log.Entries, OpenRows: log.OpenRows}
}

// What a link's generator mints under: one client generation of the device's Sidecar.
type itBridgeGeneration struct {
	api      *sidecar.SidecarApi
	clientId connect.Id
}

// The wire's device.BridgeHost (the file's header's links row).
type itBridgeLinkHost struct {
	wire     *itBridgeWire
	settings *device.BridgeSettings

	stateLock  sync.Mutex
	generation *itBridgeGeneration
	// every window client the links opened, which close ends
	windowClients []*itBridgeWindowClient
	// what the links' own local NATs were asked to dial, which a bridged packet never reaches
	localDials []string
	closed     bool
	// the window clients' closes in flight
	closing sync.WaitGroup
}

var _ device.BridgeHost = (*itBridgeLinkHost)(nil)

// BridgeComponentArgs.OnAttach: takes a client generation for the links the next builds mint under.
func (self *itBridgeLinkHost) attach(env *sidecar.SidecarEnv) {
	var generation *itBridgeGeneration
	if env.Identity != nil && env.SidecarApi != nil {
		generation = &itBridgeGeneration{api: env.SidecarApi, clientId: connect.Id(env.Identity.ClientId)}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.generation = generation
}

// The current generation, nil before one.
func (self *itBridgeLinkHost) currentGeneration() *itBridgeGeneration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.generation
}

// The source a send into a link names: the sidecar's own client, whose children the window clients are.
func (self *itBridgeLinkHost) sourcePath() connect.TransferPath {
	if generation := self.currentGeneration(); generation != nil {
		return connect.SourceId(generation.clientId)
	}
	return connect.TransferPath{}
}

// device.BridgeHost: one link to target, a fixed window of one window client whose delegation sign returns. The case
// runs no share, so a share link is refused.
func (self *itBridgeLinkHost) NewBridgeLink(ctx context.Context, target connect.Id, subject connect.Id, shareAliases []netip.Prefix,
	sign func(windowClientId connect.Id, windowClientPublicKey []byte, notAfter time.Time) ([]byte, error),
) (connect.UserNatClient, error) {
	if shareAliases != nil {
		return nil, errors.New("the case runs no share link")
	}
	generation := self.currentGeneration()
	if generation == nil {
		return nil, errors.New("no client generation")
	}
	multiClientSettings := device.BridgeLinkMultiClientSettings(self.settings)
	if err := device.CheckBridgeLinkMultiClientSettings(multiClientSettings, self.settings.LinkAllowDirect); err != nil {
		return nil, err
	}
	// the multi client's own local NAT, which a bridged packet must never reach (TCP/25's rule)
	multiClientSettings.LocalDialContextSettings = &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			self.stateLock.Lock()
			self.localDials = append(self.localDials, network+" "+address)
			self.stateLock.Unlock()
			return nil, errors.New("a link dials nothing locally")
		},
	}
	generator := &itBridgeLinkGenerator{host: self, generation: generation, target: target, clients: map[*connect.Client]*itBridgeWindowClient{}}
	return connect.NewRemoteUserNatMultiClient(ctx, device.NewBridgeLinkGenerator(ctx, generator, sign), self.wire.receive,
		protocol.ProvideMode_Network, multiClientSettings), nil
}

// What the links' own local NATs were asked to dial.
func (self *itBridgeLinkHost) LocalDials() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.localDials)
}

// How many window clients the links opened.
func (self *itBridgeLinkHost) WindowClientCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.windowClients)
}

// Ends every window client the links opened and waits for them. Idempotent.
func (self *itBridgeLinkHost) close() {
	windowClients := func() []*itBridgeWindowClient {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed = true
		return slices.Clone(self.windowClients)
	}()
	for _, windowClient := range windowClients {
		windowClient.close()
	}
	self.closing.Wait()
}

// One window client a link opened: its client, the auth its transport presents, and its transport to server's
// exchange.
type itBridgeWindowClient struct {
	settings  *ItBridgeWireSettings
	client    *connect.Client
	transport *connect.PlatformTransport
	closeOnce sync.Once
}

// Ends the transport and the client, waiting for each within CloseTimeout. Idempotent.
func (self *itBridgeWindowClient) close() {
	self.closeOnce.Do(func() {
		self.client.Cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), self.settings.CloseTimeout)
		defer closeCancel()
		self.transport.CloseAndWait(closeCtx)
		self.client.CloseAndWait(closeCtx)
	})
}

// One link's generator: its target alone, each window client a child of the sidecar's own client minted through the
// generation's api and opened on server's exchange.
type itBridgeLinkGenerator struct {
	host       *itBridgeLinkHost
	generation *itBridgeGeneration
	target     connect.Id

	stateLock sync.Mutex
	clients   map[*connect.Client]*itBridgeWindowClient
}

var _ connect.MultiClientGenerator = (*itBridgeLinkGenerator)(nil)

func (self *itBridgeLinkGenerator) NextDestinations(count int, excludeDestinations []connect.MultiHopId, rankMode string) (map[connect.MultiHopId]connect.DestinationStats, error) {
	destination, err := connect.NewMultiHopId(self.target)
	if err != nil {
		return nil, err
	}
	if slices.Contains(excludeDestinations, destination) {
		return map[connect.MultiHopId]connect.DestinationStats{}, nil
	}
	return map[connect.MultiHopId]connect.DestinationStats{destination: {}}, nil
}

// A window client minted through POST /network/auth-client as a child of the sidecar's own client, as cli's
// urBridgeCredentials mints one: refused unless api confirms admin_refused and strict_exp.
func (self *itBridgeLinkGenerator) NewClientArgs() (*connect.MultiClientGeneratorClientArgs, error) {
	settings := self.host.wire.settings
	ctx, cancel := context.WithTimeout(self.host.wire.ctx, settings.MintTimeout)
	defer cancel()
	sourceClientId := sidecar.Id(self.generation.clientId)
	result, err := self.generation.api.AuthClient(ctx, &sidecar.SidecarAuthClientArgs{
		SourceClientId:   &sourceClientId,
		Description:      "it bridge link",
		DeviceSpec:       "ur-bridge",
		DeclaresNoEgress: true,
	})
	if err != nil {
		return nil, err
	}
	if !result.AdminRefused || !result.StrictExp {
		return nil, errors.New("api minted a window client that is not admin_refused and strict_exp")
	}
	byJwt, err := connect.ParseByJwtUnverified(result.ByClientJwt)
	if err != nil {
		return nil, err
	}
	return &connect.MultiClientGeneratorClientArgs{
		ClientId:   byJwt.ClientId,
		ClientAuth: &connect.ClientAuth{ByJwt: result.ByClientJwt, InstanceId: connect.NewId(), AppVersion: settings.AppVersion},
	}, nil
}

func (self *itBridgeLinkGenerator) RemoveClientArgs(args *connect.MultiClientGeneratorClientArgs) {}

// Ends the window client the window dropped, off the window's goroutine.
func (self *itBridgeLinkGenerator) RemoveClientWithArgs(client *connect.Client, args *connect.MultiClientGeneratorClientArgs) {
	windowClient := func() *itBridgeWindowClient {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		windowClient := self.clients[client]
		delete(self.clients, client)
		return windowClient
	}()
	if windowClient == nil {
		client.Cancel()
		return
	}
	self.host.closing.Go(windowClient.close)
}

// A Required client with loopback ICE alone, as every world client runs: the generator wrapper gates its receive on a
// signed identity.
func (self *itBridgeLinkGenerator) NewClientSettings() *connect.ClientSettings {
	settings := connect.DefaultClientSettings()
	settings.EncryptionSettings.Mode = connect.EncryptionModeRequired
	settings.EncryptionSettings.ExposeVerifiedPeerClientKey = true
	webRtcSettings := *settings.WebRtcSettings
	webRtcSettings.IceServerUrls = nil
	webRtcSettings.IceServerPoolUrls = nil
	webRtcSettings.UseLoopbackOnlyIceInterfaces = true
	settings.WebRtcSettings = &webRtcSettings
	return settings
}

// Opens the window client on server's exchange: server's controller as its out-of-band control and an h1 platform
// transport to the exchange's handler with its token, answering once the controller took its provide modes and server
// holds the key it publishes in band, so the host's return contracts to it carry that key.
func (self *itBridgeLinkGenerator) NewClient(ctx context.Context, args *connect.MultiClientGeneratorClientArgs, clientSettings *connect.ClientSettings) (*connect.Client, error) {
	host := self.host
	world := host.wire.device.world
	settings := host.wire.settings
	client := connect.NewClient(ctx, args.ClientId, &itBridgeControl{
		ctx:                     world.world.T3().Context(),
		clientId:                server.Id(args.ClientId),
		contractManagerSettings: clientSettings.ContractManagerSettings,
	}, clientSettings)
	windowClient := &itBridgeWindowClient{
		settings: settings,
		client:   client,
		transport: connect.NewPlatformTransportWithTargetMode(ctx, world.clientStrategy, client.RouteManager(),
			world.world.T3().Exchange().PlatformUrl(), args.ClientAuth, connect.TransportModeH1, world.transportSettings),
	}
	kept := func() bool {
		host.stateLock.Lock()
		defer host.stateLock.Unlock()
		if host.closed {
			return false
		}
		host.windowClients = append(host.windowClients, windowClient)
		return true
	}()
	if !kept {
		windowClient.close()
		return nil, errors.New("the wire has closed")
	}
	fail := func(err error) (*connect.Client, error) {
		host.closing.Go(windowClient.close)
		return nil, err
	}
	openCtx, cancel := context.WithTimeout(ctx, settings.WindowClientOpenTimeout)
	defer cancel()
	provideAck := make(chan error, 1)
	client.ContractManager().SetProvideModesWithReturnTrafficWithOobAckCallback(
		map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true},
		func(err error) {
			select {
			case provideAck <- err:
			default:
			}
		},
	)
	select {
	case err := <-provideAck:
		if err != nil {
			return fail(fmt.Errorf("the controller refused a window client's provide modes: %w", err))
		}
	case <-openCtx.Done():
		return fail(fmt.Errorf("no ack of a window client's provide modes: %w", openCtx.Err()))
	}
	publicKey := client.ClientKeyManager().PublicKey()
	for !itBridgeClientKeyHeld(openCtx, server.Id(args.ClientId), publicKey) {
		select {
		case <-openCtx.Done():
			return fail(fmt.Errorf("server held no key for a window client: %w", openCtx.Err()))
		case <-time.After(settings.KeyRegistrationPollInterval):
		}
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.clients[client] = windowClient
	}()
	return client, nil
}

func (self *itBridgeLinkGenerator) FixedDestinationSize() (int, bool) {
	return 1, true
}

// An IPv4 ICMP echo request from source to destination with its checksums.
func itBridgeEchoRequest(source netip.Addr, destination netip.Addr, id uint16, sequence uint16, payload []byte) []byte {
	icmp := make([]byte, 8+len(payload))
	icmp[0] = 8
	binary.BigEndian.PutUint16(icmp[4:6], id)
	binary.BigEndian.PutUint16(icmp[6:8], sequence)
	copy(icmp[8:], payload)
	binary.BigEndian.PutUint16(icmp[2:4], itBridgeChecksum(icmp))
	packet := make([]byte, 20+len(icmp))
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	packet[8] = 64
	packet[9] = 1
	sourceOctets := source.As4()
	copy(packet[12:16], sourceOctets[:])
	destinationOctets := destination.As4()
	copy(packet[16:20], destinationOctets[:])
	binary.BigEndian.PutUint16(packet[10:12], itBridgeChecksum(packet[:20]))
	copy(packet[20:], icmp)
	return packet
}

// Whether packet is an IPv4 ICMP answer to an echo: a reply's id<<16|sequence and type 0, or an error's (type 3 or
// 11) about the echo it quotes, with its type.
func itBridgeEchoAnswer(packet []byte) (key uint32, icmpType byte, ok bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 || packet[9] != 1 {
		return 0, 0, false
	}
	headerLength := int(packet[0]&0x0f) * 4
	if len(packet) < headerLength+8 {
		return 0, 0, false
	}
	icmp := packet[headerLength:]
	switch icmp[0] {
	case 0:
		return uint32(binary.BigEndian.Uint16(icmp[4:6]))<<16 | uint32(binary.BigEndian.Uint16(icmp[6:8])), 0, true
	case 3, 11:
		quoted := icmp[8:]
		if len(quoted) < 20 || quoted[0]>>4 != 4 || quoted[9] != 1 {
			return 0, 0, false
		}
		quotedHeaderLength := int(quoted[0]&0x0f) * 4
		if len(quoted) < quotedHeaderLength+8 || quoted[quotedHeaderLength] != 8 {
			return 0, 0, false
		}
		echo := quoted[quotedHeaderLength:]
		return uint32(binary.BigEndian.Uint16(echo[4:6]))<<16 | uint32(binary.BigEndian.Uint16(echo[6:8])), icmp[0], true
	}
	return 0, 0, false
}

// The internet checksum of data.
func itBridgeChecksum(data []byte) uint16 {
	sum := uint32(0)
	for index := 0; index+1 < len(data); index += 2 {
		sum += uint32(binary.BigEndian.Uint16(data[index:]))
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for 0xffff < sum {
		sum = sum>>16 + sum&0xffff
	}
	return ^uint16(sum)
}
