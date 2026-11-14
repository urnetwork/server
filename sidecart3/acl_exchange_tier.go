package sidecart3

// The AclExchangeTier of every T3 Net (sidecartest/fault/fs21.go; ACL-M4-04's FS-21): sidecars' clients on server's
// exchange, as a sidecar's enrollment and bind leave them, under server's own contract gate (ACL-M4-01).
//
//   - OpenSidecarClient mints a client through model.AuthNetworkClient in the row's network, made and funded at its
//     first client and listed in sidecar.yml's preview_networks, with the marks a sidecar's enrollment sets (strict_exp,
//     admin_refused, declaring no egress), binds it through model.BindSidecarIdentity with the binding the row signed,
//     which this verifies first as api's bind route does, and opens it on the tier's own exchange (exchange.go), so
//     getProvideRelationshipDetails finds each end's sidecar_id and marks as production's query does.
//   - AdvanceHead is model.AdvanceNetworkAclHead, what api's sequencer runs after each append; the T3 Net's own
//     sequencer runs none (config_log.go).
//   - InstallGate builds controller.NewAclContractGate over the row's state source, its heads from network_acl_head,
//     waits for the first heads load, installs it (controller.SetAclContractGate) and turns sidecar.yml's
//     acl_contract_gate switch on; the process-wide gate and switch are undone at restore, or when the Net closes.
//   - Partition takes a client's transport off the exchange, so server relays nothing to or from it, and brings it
//     back on a new one (exchangeClient.partition): the exchange takes no per-pair conditions, so a partition here
//     cuts one client off every peer.
//
// The exchange is built at the first client and closed with the Net, after every client the tier opened. Safe for
// concurrent use.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// What a Net's AclExchangeTier runs with.
type AclExchangeTierSettings struct {
	// The exchange its clients run on.
	Exchange *ExchangeSettings
	// The gate InstallGate installs: §8.10's, its heads every HeadsInterval in real time.
	Gate *sidecar.AclContractGateSettings
	// How long, in real time, InstallGate waits for the gate's first heads load before it fails.
	HeadsLoadTimeout time.Duration
}

func DefaultAclExchangeTierSettings() *AclExchangeTierSettings {
	return &AclExchangeTierSettings{
		Exchange:         DefaultExchangeSettings(),
		Gate:             sidecar.DefaultAclContractGateSettings(),
		HeadsLoadTimeout: 30 * time.Second,
	}
}

// One network the tier made: the session its clients are minted under.
type aclExchangeNetwork struct {
	userSession *session.ClientSession
}

// One Net's tier.
type aclExchangeTier struct {
	t        testing.TB
	ctx      context.Context
	settings *AclExchangeTierSettings

	stateLock sync.Mutex
	platform  *exchangePlatform
	networks  map[server.Id]*aclExchangeNetwork
	clients   map[connect.Id]*exchangeClient
	// sidecar.yml as the tier pushed it: the networks it previews, whether the gate's switch is on, and the pop
	previewIds []server.Id
	gateOn     bool
	popConfig  func()
	// the installed gate's undo, nil while none is installed
	restoreGate func()
	closed      bool
}

// The tier of one Net, and what closes it: every client it opened, its exchange, its gate and its sidecar.yml.
func newAclExchangeTier(ctx context.Context, t testing.TB, settings *AclExchangeTierSettings) (*fault.AclExchangeTier, func()) {
	if settings == nil {
		settings = DefaultAclExchangeTierSettings()
	}
	self := &aclExchangeTier{
		t:        t,
		ctx:      ctx,
		settings: settings,
		networks: map[server.Id]*aclExchangeNetwork{},
		clients:  map[connect.Id]*exchangeClient{},
	}
	return &fault.AclExchangeTier{
		OpenSidecarClient: self.openSidecarClient,
		AdvanceHead:       self.advanceHead,
		InstallGate:       self.installGate,
		Partition:         self.partition,
	}, self.close
}

// Pushes sidecar.yml as the tier's state now says, popping the push before it. Runs under stateLock.
func (self *aclExchangeTier) pushConfigWithLock() {
	quoted := make([]string, 0, len(self.previewIds))
	for _, networkId := range self.previewIds {
		quoted = append(quoted, fmt.Sprintf("%q", networkId.String()))
	}
	yaml := fmt.Sprintf("sidecar:\n  preview_networks: [%s]\n", strings.Join(quoted, ", "))
	if self.gateOn {
		yaml += "  acl_contract_gate: {enabled: true}\n"
	}
	pop := server.Testing_PushSidecarConfig(yaml)
	if self.popConfig != nil {
		self.popConfig()
	}
	self.popConfig = pop
}

// The exchange, built at the first call.
func (self *aclExchangeTier) exchange() (*exchangePlatform, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, errors.New("the tier's Net has closed")
	}
	if self.platform == nil {
		platform, err := newExchangePlatform(self.ctx, self.t, self.settings.Exchange)
		if err != nil {
			return nil, err
		}
		self.platform = platform
	}
	return self.platform, nil
}

// The network's session, the network made, funded and previewed at the first call.
func (self *aclExchangeTier) network(ctx context.Context, networkId server.Id) (returnNetwork *aclExchangeNetwork, returnErr error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if network := self.networks[networkId]; network != nil {
		return network, nil
	}
	defer func() {
		// the model raises a database failure
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("server made no network %s: %v", networkId, recovered)
		}
	}()
	userId := server.NewId()
	networkName := fmt.Sprintf("sidecart3-acl-%s", strings.ReplaceAll(networkId.String(), "-", "")[:16])
	model.Testing_CreateNetwork(self.ctx, networkId, networkName, userId)
	now := server.NowUtc()
	exchangeSettings := self.settings.Exchange
	if err := model.AddBasicTransferBalance(self.ctx, networkId, exchangeSettings.NetworkBalanceByteCount, now, now.Add(exchangeSettings.NetworkBalanceDuration)); err != nil {
		return nil, err
	}
	self.previewIds = append(self.previewIds, networkId)
	self.pushConfigWithLock()
	network := &aclExchangeNetwork{
		// minted through NewByJwt, so each by-client jwt derived from it carries every claim the transport's auth checks
		userSession: session.Testing_CreateClientSession(self.ctx, session.NewByJwt(networkId, userId, networkName, false, false)),
	}
	self.networks[networkId] = network
	return network, nil
}

// fault.AclExchangeTier.OpenSidecarClient. See the file header.
func (self *aclExchangeTier) openSidecarClient(ctx context.Context, networkId sidecar.Id, name string, settings *connect.ClientSettings, bind func(clientId sidecar.Id) ([]byte, error)) (platformClient *sidecartest.NetPlatformClient, returnErr error) {
	platform, err := self.exchange()
	if err != nil {
		return nil, err
	}
	network, err := self.network(ctx, server.Id(networkId))
	if err != nil {
		return nil, err
	}
	defer func() {
		// the model raises a database failure
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("server refused sidecar client %q: %v", name, recovered)
		}
	}()
	result, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{
		Description:      name,
		DeviceSpec:       "sidecart3",
		AdminRefused:     true,
		StrictExp:        true,
		DeclaresNoEgress: true,
	}, network.userSession)
	if err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("server refused sidecar client %q: %+v", name, *result.Error)
	}
	clientId := *result.ClientId
	signedBinding, err := bind(sidecar.Id(clientId))
	if err != nil {
		return nil, err
	}
	// what api's bind route checks before it appends: both signatures, and that the bearer is the binding's client in
	// the binding's network
	binding, err := sidecar.VerifySidecarIdentityBinding(signedBinding)
	if err != nil {
		return nil, err
	}
	if binding.ClientId != sidecar.Id(clientId) || binding.NetworkId != networkId {
		return nil, fmt.Errorf("sidecar client %q's binding names another client or network", name)
	}
	outcome, _ := model.BindSidecarIdentity(ctx, &model.SidecarBindingRecord{
		NetworkId:         server.Id(binding.NetworkId),
		SidecarId:         server.Id(binding.SidecarId),
		ClientId:          clientId,
		Generation:        int64(binding.Generation),
		ClientPublicKey:   binding.ClientPublicKey,
		IdentityPublicKey: binding.IdentityPublicKey,
		SignedBinding:     signedBinding,
	}, server.NowUtc())
	if outcome != model.SidecarBindAppended {
		return nil, fmt.Errorf("server did not bind sidecar client %q: %s", name, outcome)
	}
	// the client lives until the tier closes it; ctx bounds only the open's waits
	opened, err := platform.openClientAs(self.ctx, ctx, name, settings, clientId, *result.ByClientJwt)
	if err != nil {
		return nil, err
	}
	closed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return true
		}
		self.clients[connect.Id(clientId)] = opened
		return false
	}()
	if closed {
		opened.close()
		return nil, errors.New("the tier's Net has closed")
	}
	return &sidecartest.NetPlatformClient{
		ClientId: connect.Id(clientId),
		Client:   opened.client,
		Close:    opened.close,
	}, nil
}

// fault.AclExchangeTier.AdvanceHead: model.AdvanceNetworkAclHead.
func (self *aclExchangeTier) advanceHead(ctx context.Context, networkId sidecar.Id, pin *bridge.AuthPin, hasDocument bool) (returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("network_acl_head: %v", recovered)
		}
	}()
	_, err := model.AdvanceNetworkAclHead(ctx, server.Id(networkId), pin.GetVersion(), pin.GetChain(), hasDocument)
	return err
}

// fault.AclExchangeTier.InstallGate. See the file header.
func (self *aclExchangeTier) installGate(ctx context.Context, states sidecar.AclContractStateSource) (func(), error) {
	gateSettings := *self.settings.Gate
	heads := make(chan error, 1)
	onHeads := gateSettings.OnHeads
	gateSettings.OnHeads = func(networkCount int, err error) {
		if onHeads != nil {
			onHeads(networkCount, err)
		}
		select {
		case heads <- err:
		default:
		}
	}
	gate := controller.NewAclContractGate(self.ctx, states, &gateSettings)
	select {
	case err := <-heads:
		if err != nil {
			gate.Close()
			return nil, fmt.Errorf("the gate's first heads load: %w", err)
		}
	case <-ctx.Done():
		gate.Close()
		return nil, ctx.Err()
	case <-time.After(self.settings.HeadsLoadTimeout):
		gate.Close()
		return nil, fmt.Errorf("no heads load within %s", self.settings.HeadsLoadTimeout)
	}
	restoreController := controller.SetAclContractGate(gate)
	var once sync.Once
	restore := func() {
		once.Do(func() {
			// what the gate decided while installed, for the run's log
			self.t.Logf("server's contract gate decided %+v", gate.Stats())
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			restoreController()
			gate.Close()
			self.gateOn = false
			self.restoreGate = nil
			if !self.closed {
				self.pushConfigWithLock()
			}
		})
	}
	err := func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || self.restoreGate != nil {
			return errors.New("the tier's Net has closed, or a gate is installed already")
		}
		self.gateOn = true
		self.restoreGate = restore
		self.pushConfigWithLock()
		return nil
	}()
	if err != nil {
		restoreController()
		gate.Close()
		return nil, err
	}
	return restore, nil
}

// fault.AclExchangeTier.Partition. See the file header.
func (self *aclExchangeTier) partition(ctx context.Context, clientId connect.Id, partitioned bool) error {
	self.stateLock.Lock()
	opened := self.clients[clientId]
	self.stateLock.Unlock()
	if opened == nil {
		return fmt.Errorf("the tier opened no client %s", clientId)
	}
	return opened.partition(ctx, partitioned)
}

// Undoes the gate and sidecar.yml, then closes every client the tier opened and its exchange.
func (self *aclExchangeTier) close() {
	var restoreGate func()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		restoreGate = self.restoreGate
	}()
	if restoreGate != nil {
		restoreGate()
	}
	var clients []*exchangeClient
	var platform *exchangePlatform
	var networks []*aclExchangeNetwork
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed = true
		for _, opened := range self.clients {
			clients = append(clients, opened)
		}
		self.clients = map[connect.Id]*exchangeClient{}
		for _, network := range self.networks {
			networks = append(networks, network)
		}
		platform = self.platform
		self.platform = nil
		if self.popConfig != nil {
			self.popConfig()
			self.popConfig = nil
		}
	}()
	for _, opened := range clients {
		opened.close()
	}
	if platform != nil {
		platform.Close()
	}
	for _, network := range networks {
		network.userSession.Cancel()
	}
}
