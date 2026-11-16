package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// api's certs install (QD-846; tls_certs_install.go) over fakes: with
// sidecar.yml's switch off nothing is built and /tls answers 503; with it on
// and a certs.yml the routes serve, the subject read from each network's
// verified state through AclGateView; a missing file leaves certs uninstalled
// and named in the log.

// The gate cache as a test holds it: each network's installed state, the loads
// asked for, and what an ask installs.
type tlsInstallTestStates struct {
	stateLock sync.Mutex
	snapshots map[sidecar.Id]*sidecar.AclGateSnapshot
	// what an ask for a network installs; a network with none loads nothing
	onAsk     map[sidecar.Id]*sidecar.AclGateSnapshot
	asked     []sidecar.Id
	callbacks map[int]sidecar.AclGateInstallFunction
	nextIndex int
}

var _ TlsCertsGateStates = (*tlsInstallTestStates)(nil)

func newTlsInstallTestStates() *tlsInstallTestStates {
	return &tlsInstallTestStates{
		snapshots: map[sidecar.Id]*sidecar.AclGateSnapshot{},
		onAsk:     map[sidecar.Id]*sidecar.AclGateSnapshot{},
		callbacks: map[int]sidecar.AclGateInstallFunction{},
	}
}

func (self *tlsInstallTestStates) State(networkId sidecar.Id) (*sidecar.AclGateSnapshot, sidecar.AclGateStatus) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	snapshot := self.snapshots[networkId]
	if snapshot == nil {
		return nil, sidecar.AclGateUnknown
	}
	return snapshot, sidecar.AclGateLoaded
}

func (self *tlsInstallTestStates) Changed(networkId sidecar.Id) {
	snapshot := func() *sidecar.AclGateSnapshot {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.asked = append(self.asked, networkId)
		return self.onAsk[networkId]
	}()
	if snapshot != nil {
		self.install(networkId, snapshot)
	}
}

func (self *tlsInstallTestStates) AddInstallCallback(callback sidecar.AclGateInstallFunction) func() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	index := self.nextIndex
	self.nextIndex += 1
	self.callbacks[index] = callback
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.callbacks, index)
	}
}

// Installs a network's state and tells the callbacks, as the cache's load does.
func (self *tlsInstallTestStates) install(networkId sidecar.Id, snapshot *sidecar.AclGateSnapshot) {
	callbacks := func() []sidecar.AclGateInstallFunction {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.snapshots[networkId] = snapshot
		callbacks := []sidecar.AclGateInstallFunction{}
		for _, callback := range self.callbacks {
			callbacks = append(callbacks, callback)
		}
		return callbacks
	}()
	for _, callback := range callbacks {
		callback(networkId, snapshot)
	}
}

func (self *tlsInstallTestStates) askedCount(networkId sidecar.Id) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, asked := range self.asked {
		if asked == networkId {
			count += 1
		}
	}
	return count
}

func (self *tlsInstallTestStates) callbackCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.callbacks)
}

// One network's log as a test writes it: its devices and their rules.
type tlsInstallTestNetwork struct {
	world     *tlsInstallTestWorld
	networkId sidecar.Id
	label     string
	view      *sidecartest.FakeAclView
	memberId  sidecar.Id
	atoms     []*protocol.AclAtom
	rules     []*protocol.AclRule
}

// A device of the network whose rule, when targets are given, allows actions
// on them. Its set is the network's label and aliases.
func (self *tlsInstallTestNetwork) device(targets []string, actions ...protocol.AclAction) *tlsTestDevice {
	t := self.world.t
	t.Helper()
	identityPublicKey, identityKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tlsKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sidecarId := self.view.AddDevice(self.memberId)
	self.view.Sidecar(sidecarId).Row.IdentityPublicKey = identityPublicKey
	atom := sidecartest.FakeAclSidecarAtom(sidecarId)
	self.atoms = append(self.atoms, atom)
	if 0 < len(targets) {
		rule := &protocol.AclRule{
			RuleId:   sidecar.NewId().Bytes(),
			SrcAtoms: [][]byte{atom.Atom},
		}
		for _, target := range targets {
			rule.Targets = append(rule.Targets, &protocol.AclTarget{PublicName: target})
		}
		for _, action := range actions {
			rule.Actions = append(rule.Actions, uint32(action))
		}
		self.rules = append(self.rules, rule)
	}
	identifierSet, err := sidecar.TlsIdentifierSet(tlsTestRoot, self.label, "", self.world.aliases[self.networkId])
	if err != nil {
		t.Fatal(err)
	}
	device := &tlsTestDevice{
		networkId:     self.networkId,
		clientId:      sidecar.NewId(),
		sidecarId:     sidecarId,
		identityKey:   identityKey,
		tlsKey:        tlsKey,
		identifierSet: identifierSet,
	}
	device.token = self.world.token(device, TlsCertsAudience)
	return device
}

// The network's verified state over its devices and rules as they stand.
func (self *tlsInstallTestNetwork) snapshot() *sidecar.AclGateSnapshot {
	self.world.t.Helper()
	state, err := sidecar.NewAclState(&protocol.AclRows{
		HasDocument: true,
		Atoms:       slices.Clone(self.atoms),
		Rules:       slices.Clone(self.rules),
	}, self.view)
	if err != nil {
		self.world.t.Fatal(err)
	}
	return &sidecar.AclGateSnapshot{Pin: self.view.Pin(), Acl: state}
}

// Loads the network's state into the cache.
func (self *tlsInstallTestNetwork) load() {
	self.world.states.install(self.networkId, self.snapshot())
}

// The install's world: the routes' fakes, a gate cache, §21's standing, the
// config file and the switch, each as a test sets it.
type tlsInstallTestWorld struct {
	*tlsTestWorld
	states *tlsInstallTestStates
	abuse  *sidecartest.FakeAbuseGate
	on     bool
	// certs.yml's text, nil for a config with no such file
	certsConfig []byte
	gateErr     error
	gateBuilds  int
	gateCloses  int
	configReads []string
	// by network: the label and aliases the names read answers
	labels  map[sidecar.Id]string
	aliases map[sidecar.Id][]string
	// the names reads, as "<network>:<allocate>"
	nameReads []string
	namesErr  error
	lines     []string
	install   *server.SidecarSwitchInstall
}

func newTlsInstallTestWorld(t *testing.T) *tlsInstallTestWorld {
	t.Helper()
	world := &tlsInstallTestWorld{
		tlsTestWorld: newTlsTestWorld(t),
		states:       newTlsInstallTestStates(),
		abuse:        sidecartest.NewFakeAbuseGate(),
		certsConfig:  []byte("root: " + tlsTestRoot + "\n"),
		labels:       map[sidecar.Id]string{},
		aliases:      map[sidecar.Id][]string{},
	}
	// the routes start with nothing installed, as a process does
	SetTlsCerts(nil)
	settings := DefaultTlsCertsInstallSettings()
	// a network the cache does not hold answers at once
	settings.SubjectLoadWait = 0
	world.install = NewTlsCertsInstall(&TlsCertsInstallDeps{
		SwitchOn: func() bool {
			return world.on
		},
		ReadConfigFile: func(name string) ([]byte, error) {
			world.configReads = append(world.configReads, name)
			if world.certsConfig == nil {
				return nil, fmt.Errorf("%w: config holds no %s", ErrTlsCertsInstall, name)
			}
			return world.certsConfig, nil
		},
		Gate: func(ctx context.Context) (*TlsCertsGate, error) {
			world.gateBuilds += 1
			if world.gateErr != nil {
				return nil, world.gateErr
			}
			return &TlsCertsGate{
				Host:   world.host.ServiceHost(),
				States: world.states,
				Abuse:  world.abuse,
				Close: func() {
					world.gateCloses += 1
				},
			}, nil
		},
		Store: world.store,
		Names: func(ctx context.Context, networkId sidecar.Id, root string, allocate bool) (string, []string, error) {
			world.nameReads = append(world.nameReads, fmt.Sprintf("%s:%t", networkId, allocate))
			if world.namesErr != nil {
				return "", nil, world.namesErr
			}
			if root != tlsTestRoot {
				return "", nil, fmt.Errorf("names read in %q", root)
			}
			return world.labels[networkId], world.aliases[networkId], nil
		},
	}, settings)
	world.install.Testing_SetLog(func(format string, arguments ...any) {
		world.lines = append(world.lines, fmt.Sprintf(format, arguments...))
	})
	t.Cleanup(world.install.Close)
	return world
}

// A network in good standing with a label, and no state loaded yet.
func (self *tlsInstallTestWorld) network(label string, aliases ...string) *tlsInstallTestNetwork {
	networkId := sidecar.NewId()
	self.labels[networkId] = label
	self.aliases[networkId] = aliases
	self.abuse.Set(connect.Id(networkId), sidecar.AbuseStandingStandard, true)
	view := sidecartest.NewFakeAclView()
	return &tlsInstallTestNetwork{
		world:     self,
		networkId: networkId,
		label:     label,
		view:      view,
		memberId:  view.AddMember(bridge.MemberRole_MEMBER_ROLE_OWNER),
	}
}

// The device's order for its own set.
func (self *tlsInstallTestWorld) order(device *tlsTestDevice) (*sidecar.TlsCertOrderResult, error) {
	return TlsCertOrder(self.ctx, device.token, self.orderArgs(device, device.identifierSet))
}

var tlsInstallTestPublish = []protocol.AclAction{protocol.AclAction_ACL_ACTION_PUBLISH, protocol.AclAction_ACL_ACTION_LISTEN}

// The implicit document's grant: every host, every tls/ name.
var tlsInstallTestImplicitTargets = []string{"*", "tls/*"}

// With the switch off nothing is built or read and every /tls route answers
// 503.
func TestTlsCertsInstallOffInstallsNothing(t *testing.T) {
	world := newTlsInstallTestWorld(t)
	network := world.network("acme")
	device := network.device(tlsInstallTestImplicitTargets, tlsInstallTestPublish...)
	network.load()

	for range 2 {
		if world.install.Sync(world.ctx) || world.install.Installed() {
			t.Fatal("installed with the switch off")
		}
	}
	if world.gateBuilds != 0 || len(world.configReads) != 0 || len(world.lines) != 0 {
		t.Fatalf("the switch off built %d gates, read %v and said %q", world.gateBuilds, world.configReads, world.lines)
	}
	if _, err := world.order(device); !tlsTestStatus(err, "503") {
		t.Fatalf("an order with the switch off answered %v", err)
	}
	if _, err := TlsCertOrderStatus(world.ctx, device.token, server.NewId()); !tlsTestStatus(err, "503") {
		t.Fatalf("an order's status with the switch off answered %v", err)
	}
	clientSession := session.NewLocalClientSession(world.ctx, "", &session.ByJwt{NetworkId: server.Id(device.networkId)})
	defer clientSession.Cancel()
	if _, err := TlsCerts(clientSession); !tlsTestStatus(err, "503") {
		t.Fatalf("the certificates with the switch off answered %v", err)
	}
	if _, err := TlsCertRevoke(&sidecar.TlsCertRevokeArgs{CertId: sidecar.NewId()}, nil, clientSession); !tlsTestStatus(err, "503") {
		t.Fatalf("a revocation with the switch off answered %v", err)
	}
	if len(world.store.Rows()) != 0 {
		t.Fatal("a row was written with the switch off")
	}
}

// With the switch on and a certs.yml the routes serve: an order queues for the
// token's subject with the set the network's label and alias give, reads back,
// and the network's certificates list. Off again, the routes answer 503 and
// the gate is released; on again, they serve anew.
func TestTlsCertsInstallOnServesTheRoutes(t *testing.T) {
	world := newTlsInstallTestWorld(t)
	network := world.network("acme", "shop")
	device := network.device(tlsInstallTestImplicitTargets, tlsInstallTestPublish...)
	network.load()
	if !slices.Contains(device.identifierSet, "*.shop."+tlsTestRoot) || !slices.Contains(device.identifierSet, "acme."+tlsTestRoot) {
		t.Fatalf("the device's set is %v", device.identifierSet)
	}

	world.on = true
	if !world.install.Sync(world.ctx) || !world.install.Installed() {
		t.Fatalf("not installed with the switch on: %q", world.lines)
	}
	if world.gateBuilds != 1 || !slices.Equal(world.configReads, []string{TlsCertsConfigFileName}) {
		t.Fatalf("%d gates built, read %v", world.gateBuilds, world.configReads)
	}

	result, err := world.order(device)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != sidecar.TlsCertStateQueued || result.CertId == (sidecar.Id{}) {
		t.Fatalf("the order answered %+v", result)
	}
	rows := world.store.Rows()
	if len(rows) != 1 || rows[0].NetworkId != device.networkId || rows[0].ClientId != device.clientId || rows[0].SidecarId != device.sidecarId {
		t.Fatalf("the row is not the token's subject: %+v", rows)
	}
	wantSetSha256, err := sidecar.TlsIdentifierSetHash(device.identifierSet)
	if err != nil {
		t.Fatal(err)
	}
	if string(rows[0].IdentifierSetSha256) != string(wantSetSha256[:]) {
		t.Fatal("the row's set is not the label's and the alias's")
	}
	// a granted order may give the network its label
	if !slices.Equal(world.nameReads, []string{fmt.Sprintf("%s:true", network.networkId)}) {
		t.Fatalf("the names were read as %v", world.nameReads)
	}
	status, err := TlsCertOrderStatus(world.ctx, device.token, server.Id(result.CertId))
	if err != nil || status.State != sidecar.TlsCertStateQueued {
		t.Fatalf("the order's status: %+v, %v", status, err)
	}
	clientSession := session.NewLocalClientSession(world.ctx, "", &session.ByJwt{NetworkId: server.Id(device.networkId)})
	defer clientSession.Cancel()
	if _, err := TlsCerts(clientSession); err != nil {
		t.Fatalf("the network's certificates answered %v", err)
	}
	// a CSR for a set that is not the network's is refused whatever it attests
	other, err := sidecar.TlsIdentifierSet(tlsTestRoot, "other", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TlsCertOrder(world.ctx, device.token, world.orderArgs(device, other)); !tlsTestStatus(err, "400") {
		t.Fatalf("a CSR for another network's set answered %v", err)
	}

	world.on = false
	if world.install.Sync(world.ctx) {
		t.Fatal("still installed with the switch off again")
	}
	if world.gateCloses != 1 {
		t.Fatalf("the gate was released %d times", world.gateCloses)
	}
	if _, err := world.order(device); !tlsTestStatus(err, "503") {
		t.Fatalf("an order with the switch off again answered %v", err)
	}

	world.on = true
	if !world.install.Sync(world.ctx) || world.gateBuilds != 2 {
		t.Fatalf("the second on: %d gates built", world.gateBuilds)
	}
	again, err := world.order(device)
	if err != nil || again.CertId != result.CertId {
		t.Fatalf("the same order after the second on: %+v, %v", again, err)
	}
}

// The subject is the projection's: a grant by a host's rule, by the implicit
// document's globs or by the receiver's smtp/ listen orders; no rule, a scope
// host alone, a revoked or a parked sidecar and a halted network are refused
// with nothing allocated; a state or a standing not loaded answers 503.
func TestTlsCertsInstallSubjectIsTheProjections(t *testing.T) {
	world := newTlsInstallTestWorld(t)
	network := world.network("acme")
	implicit := network.device(tlsInstallTestImplicitTargets, tlsInstallTestPublish...)
	host := network.device([]string{"blog", "tls/443/blog"}, tlsInstallTestPublish...)
	receiver := network.device([]string{"smtp/25/mx"}, protocol.AclAction_ACL_ACTION_LISTEN)
	none := network.device(nil)
	scopeOnly := network.device([]string{"app.team", "tls/443/app.team"}, tlsInstallTestPublish...)
	publishNoListen := network.device([]string{"wiki", "tls/443/wiki"}, protocol.AclAction_ACL_ACTION_PUBLISH)
	revoked := network.device(tlsInstallTestImplicitTargets, tlsInstallTestPublish...)
	parked := network.device(tlsInstallTestImplicitTargets, tlsInstallTestPublish...)
	network.view.Revoke(revoked.sidecarId)
	network.view.Park(parked.sidecarId)
	network.load()

	world.on = true
	if !world.install.Sync(world.ctx) {
		t.Fatalf("not installed: %q", world.lines)
	}

	for name, device := range map[string]*tlsTestDevice{"the implicit document's": implicit, "a host's": host, "the receiver's": receiver} {
		result, err := world.order(device)
		if err != nil || result.State != sidecar.TlsCertStateQueued {
			t.Fatalf("an order on %s grant: %+v, %v", name, result, err)
		}
	}
	if len(world.store.Rows()) != 3 {
		t.Fatalf("%d rows for three granted orders", len(world.store.Rows()))
	}

	world.nameReads = nil
	for name, device := range map[string]*tlsTestDevice{
		"no rule":                none,
		"a scope host alone":     scopeOnly,
		"publish with no listen": publishNoListen,
		"a revoked sidecar":      revoked,
		"a parked sidecar":       parked,
	} {
		if _, err := world.order(device); !tlsTestStatus(err, "403") {
			t.Fatalf("an order with %s answered %v", name, err)
		}
	}
	if len(world.nameReads) != 0 || len(world.store.Rows()) != 3 {
		t.Fatalf("a refused order read names %v or wrote a row", world.nameReads)
	}

	// §21: a suspended network is granted nothing; a standing not loaded is not a refusal
	world.abuse.SetStanding(connect.Id(network.networkId), sidecar.AbuseStandingSuspended)
	if _, err := world.order(implicit); !tlsTestStatus(err, "403") {
		t.Fatalf("an order of a suspended network answered %v", err)
	}
	world.abuse.Unload(connect.Id(network.networkId))
	if _, err := world.order(implicit); !tlsTestStatus(err, "503") {
		t.Fatalf("an order with no standing loaded answered %v", err)
	}
	world.abuse.Set(connect.Id(network.networkId), sidecar.AbuseStandingStandard, true)

	// the names: an error or no label is unavailable, never an order on a guess
	world.namesErr = errors.New("postgres is away")
	if _, err := world.order(implicit); !tlsTestStatus(err, "503") {
		t.Fatalf("an order whose names cannot be read answered %v", err)
	}
	world.namesErr = nil
	world.labels[network.networkId] = ""
	if _, err := world.order(implicit); !tlsTestStatus(err, "503") {
		t.Fatalf("an order of a network with no label answered %v", err)
	}
	world.labels[network.networkId] = "acme"

	// a network the cache has not loaded is asked for and answers 503; once the load installs it, it orders
	cold := world.network("cold")
	coldDevice := cold.device(tlsInstallTestImplicitTargets, tlsInstallTestPublish...)
	if _, err := world.order(coldDevice); !tlsTestStatus(err, "503") {
		t.Fatalf("an order of a network not loaded answered %v", err)
	}
	if world.states.askedCount(cold.networkId) != 1 || world.states.callbackCount() != 0 {
		t.Fatalf("the cold network was asked for %d times, %d callbacks left", world.states.askedCount(cold.networkId), world.states.callbackCount())
	}
	world.states.onAsk[cold.networkId] = cold.snapshot()
	result, err := world.order(coldDevice)
	if err != nil || result.State != sidecar.TlsCertStateQueued {
		t.Fatalf("the cold network's order once its load installs: %+v, %v", result, err)
	}
	if world.states.callbackCount() != 0 {
		t.Fatal("an install callback was left registered")
	}
}

// A missing or refused certs.yml, and a gate that cannot be built, leave certs
// uninstalled and the routes at 503, each said once by name; when the file is
// there the next step installs.
func TestTlsCertsInstallMissingFilesLeaveItUninstalled(t *testing.T) {
	world := newTlsInstallTestWorld(t)
	network := world.network("acme")
	device := network.device(tlsInstallTestImplicitTargets, tlsInstallTestPublish...)
	network.load()
	world.on = true

	world.certsConfig = nil
	for range 3 {
		if world.install.Sync(world.ctx) {
			t.Fatal("installed with no certs.yml")
		}
	}
	if world.gateBuilds != 0 {
		t.Fatal("a gate was built with no certs.yml")
	}
	if len(world.lines) != 1 || !strings.Contains(world.lines[0], TlsCertsConfigFileName) {
		t.Fatalf("no certs.yml said %q", world.lines)
	}
	if _, err := world.order(device); !tlsTestStatus(err, "503") {
		t.Fatalf("an order with no certs.yml answered %v", err)
	}

	world.certsConfig = []byte("root: " + tlsTestRoot + "\nbudget_cap_typo: 5\n")
	if world.install.Sync(world.ctx) || world.gateBuilds != 0 {
		t.Fatal("installed on a certs.yml with an unknown key")
	}
	if len(world.lines) != 2 || !strings.Contains(world.lines[1], "certs.yml refused") {
		t.Fatalf("a refused certs.yml said %q", world.lines)
	}

	world.certsConfig = []byte("root: " + tlsTestRoot + "\n")
	world.gateErr = fmt.Errorf("%w: vault holds no file-clusters-api.yml", ErrTlsCertsInstall)
	if world.install.Sync(world.ctx) {
		t.Fatal("installed with no gate")
	}
	if len(world.lines) != 3 || !strings.Contains(world.lines[2], "file-clusters-api.yml") {
		t.Fatalf("no gate said %q", world.lines)
	}
	if _, err := world.order(device); !tlsTestStatus(err, "503") {
		t.Fatalf("an order with no gate answered %v", err)
	}

	world.gateErr = nil
	if !world.install.Sync(world.ctx) {
		t.Fatalf("not installed once the files are there: %q", world.lines)
	}
	if result, err := world.order(device); err != nil || result.State != sidecar.TlsCertStateQueued {
		t.Fatalf("an order once installed: %+v, %v", result, err)
	}
}

// The install as api starts it reads the process's own sidecar.yml and config
// mount: off in sidecar.yml the routes answer 503, and on, with certs.yml in
// the mount, an order queues.
func TestTlsCertsInstallFollowsSidecarYmlAndTheConfigMount(t *testing.T) {
	world := newTlsInstallTestWorld(t)
	network := world.network("acme")
	device := network.device(tlsInstallTestImplicitTargets, tlsInstallTestPublish...)
	network.load()
	defer server.Config.PushSimpleResource(TlsCertsConfigFileName, world.certsConfig)()
	settings := DefaultTlsCertsInstallSettings()
	settings.SubjectLoadWait = 0
	install := NewTlsCertsInstall(&TlsCertsInstallDeps{
		Gate: func(ctx context.Context) (*TlsCertsGate, error) {
			return &TlsCertsGate{Host: world.host.ServiceHost(), States: world.states, Abuse: world.abuse}, nil
		},
		Store: world.store,
		Names: func(ctx context.Context, networkId sidecar.Id, root string, allocate bool) (string, []string, error) {
			return world.labels[networkId], world.aliases[networkId], nil
		},
	}, settings)
	install.Testing_SetLog(func(format string, arguments ...any) {
		world.lines = append(world.lines, fmt.Sprintf(format, arguments...))
	})
	defer install.Close()

	func() {
		defer server.Testing_PushSidecarConfig("sidecar:\n  certs: {enabled: false}\n  tlsedge: {enabled: true, percent: 100}\n")()
		if install.Sync(world.ctx) {
			t.Fatal("installed with sidecar.yml's certs off")
		}
		if _, err := world.order(device); !tlsTestStatus(err, "503") {
			t.Fatalf("an order with sidecar.yml's certs off answered %v", err)
		}
	}()
	func() {
		defer server.Testing_PushSidecarConfig("sidecar:\n  certs: {enabled: true}\n")()
		if !install.Sync(world.ctx) {
			t.Fatalf("not installed with sidecar.yml's certs on: %q", world.lines)
		}
		if result, err := world.order(device); err != nil || result.State != sidecar.TlsCertStateQueued {
			t.Fatalf("an order with sidecar.yml's certs on: %+v, %v", result, err)
		}
	}()
	func() {
		defer server.Testing_PushSidecarConfig("sidecar: {}\n")()
		if install.Sync(world.ctx) {
			t.Fatal("still installed once sidecar.yml names no certs switch")
		}
		if _, err := world.order(device); !tlsTestStatus(err, "503") {
			t.Fatalf("an order once sidecar.yml names no certs switch answered %v", err)
		}
	}()
}
