package controller

// certs' install in api (QD-846, question tls-m5-17-certs-host-install; QD-847;
// §13.8, §13.15): what makes §13.15's /tls routes reachable in a running api,
// behind sidecar.yml's certs switch. While the switch is off, absent or
// unreadable on a first read, nothing is built and every /tls route answers
// 503, as a process with nothing installed does (SetTlsCerts). While it is on,
// api holds:
//
//   - the engine, certsservice.CertsService on certs.yml's settings, with no
//     DNS client and no ACME client: a route writes or reads a row, and only
//     taskworker's chains reach a zone or a CA (work.InstallTlsCertChains);
//   - the Postgres store, model.NewCertsStore;
//   - the certs authenticator, the process's service token verifier for aud
//     certs;
//   - the subject, read from the caller's network's newest verified projection
//     through sidecar.AclGateView over the process's gate cache (C-09).
//
// The subject of an order, from the token's network and sidecar and never the
// body:
//
//	identity key   the key the projection admits the sidecar under, by its
//	               SidecarAdmit or the Genesis for a network's first device;
//	               none for a sidecar it does not admit, has revoked or has
//	               parked, which is then granted nothing
//	grant          a §8 smtp/ grant, listen on smtp/25/mx, or a §8 tls/ grant:
//	               AclGateView.PublishGrant answering a grant for one of the
//	               names the network's rules publish (AclState.PublishedNames),
//	               in either app mode. A name the rules write as a glob, as
//	               the implicit document's dns:* does, is asked as one host the
//	               glob names, so the answer is the gate's own for a
//	               registration of that host. A scope host's grant is left out
//	               until TLS-M6-03 gives a scope's devices their own set, so a
//	               device granted only there holds no network set meanwhile
//	identifier set sidecar.TlsIdentifierSet over certs.yml's root, the
//	               network's label and its live aliases
//	               (model.GetNetworkTlsCertNames), the set POST /dns/key-record
//	               attests; a granted network with no label yet takes one,
//	               §12.5's first need at an order
//
// Anything the projection, §21's standing or Postgres cannot answer is an
// error, which the route answers 503: a network whose state the cache has not
// loaded is asked for and waited on for SubjectLoadWait, and a standing not
// loaded yet answers at the caller's retry. A halted network is granted
// nothing.
//
// api hosts no gate cache of its own yet, so the default builds this install's:
// sidecar.AclGateCache over the first metadata cluster of vault's
// file-clusters-api.yml and the host's Redis (sidecar.AclGateDirectSource),
// following {acl_<networkId>}v. A process that comes to host the one cache
// passes it in (TlsCertsInstallDeps.Gate). A missing certs.yml or
// file-clusters-api.yml leaves certs uninstalled, said once in the log by the
// file's name, and is looked for again at each step of the switch's install.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

const (
	// certs.yml under config/<env>.
	TlsCertsConfigFileName = "certs.yml"
	// The block whose cluster credentials api's gate cache reads proj/ and
	// projrows/ with: file-clusters-api.yml (plan-OPS-small-readings #7).
	tlsCertsFileClustersService = "api"
	// The receiver's listen grant, §8's smtp/ grant for a network's own mail.
	tlsCertsMailListenName = "smtp/25/mx"
	// What a glob in a published host is asked as: one host the glob names.
	tlsCertsGlobHostFill = "x"
)

var (
	// The projection, the standing or the names cannot answer for the caller
	// now. The route answers 503 and the device asks again.
	ErrTlsCertSubjectUnavailable = errors.New("the certificate subject cannot be read")
	// What the install is built from is missing or refused. The text names the
	// file or the part, never a value read from one.
	ErrTlsCertsInstall = errors.New("certs is not installed")
)

// What the subject reads of the process's gate cache: sidecar.AclGateCache's
// methods.
type TlsCertsGateStates interface {
	// The network's verified state, with no I/O.
	State(networkId sidecar.Id) (*sidecar.AclGateSnapshot, sidecar.AclGateStatus)
	// Asks for the network's load, without blocking.
	Changed(networkId sidecar.Id)
	// Runs callback after each new snapshot is installed, until the returned call.
	AddInstallCallback(callback sidecar.AclGateInstallFunction) (remove func())
}

var _ TlsCertsGateStates = (*sidecar.AclGateCache)(nil)

// Tunables of api's certs install.
type TlsCertsInstallSettings struct {
	// How the install follows sidecar.yml's certs switch.
	Switch *server.SidecarSwitchInstallSettings
	// How long an order waits for the gate cache to load its network's state
	// before the route answers unavailable. A request waits on it, so it stays
	// well under the lb's 30 s; a load that takes longer lands for the device's
	// retry. Non-positive waits none.
	SubjectLoadWait time.Duration
}

func DefaultTlsCertsInstallSettings() *TlsCertsInstallSettings {
	return &TlsCertsInstallSettings{
		Switch:          server.DefaultSidecarSwitchInstallSettings(),
		SubjectLoadWait: 5 * time.Second,
	}
}

// The gate half of the install: the host, the process's gate cache and §21's
// standing, for as long as certs is installed.
type TlsCertsGate struct {
	// Redis for the budget's reads, the service token authenticator and the
	// clock. Required.
	Host *sidecar.ServiceHost
	// The process's gate cache. Required.
	States TlsCertsGateStates
	// §21.7's standing. nil has loaded no network, so every order waits.
	Abuse sidecar.AbuseGate
	// Releases what the gate holds, when certs is removed. May be nil.
	Close func()
}

// What the install is built from. Each nil takes api's own.
type TlsCertsInstallDeps struct {
	// Whether sidecar.yml's certs switch reads on. nil: this process's reader.
	SwitchOn func() bool
	// A file of config/<env> by name. nil: this process's config mount.
	ReadConfigFile func(name string) ([]byte, error)
	// The gate half, built when the switch turns on. nil: a cache of this
	// install's own over file-clusters-api.yml's first metadata cluster.
	Gate func(ctx context.Context) (*TlsCertsGate, error)
	// The rows. nil: model.NewCertsStore on this process's Postgres, in a
	// binary that links MG5.
	Store certsservice.CertsStore
	// A network's label in root and its live aliases; allocate gives a network
	// with no label one. nil: model.GetNetworkTlsCertNames.
	Names func(ctx context.Context, networkId sidecar.Id, root string, allocate bool) (label string, aliases []string, err error)
}

// api's certs install as server.SidecarSwitchInstall drives it: nothing is
// built until a step finds the switch on. deps and settings may be nil.
func NewTlsCertsInstall(deps *TlsCertsInstallDeps, settings *TlsCertsInstallSettings) *server.SidecarSwitchInstall {
	if deps == nil {
		deps = &TlsCertsInstallDeps{}
	}
	if settings == nil {
		settings = DefaultTlsCertsInstallSettings()
	}
	switchOn := deps.SwitchOn
	if switchOn == nil {
		switchOn = func() bool {
			return server.CurrentSidecarConfig().Certs()
		}
	}
	return server.NewSidecarSwitchInstall("certs", switchOn, func(ctx context.Context) (func(), error) {
		return buildTlsCerts(ctx, deps, settings)
	}, settings.Switch)
}

// Starts api's certs install: /tls routes serve while sidecar.yml's certs
// switch is on and what they run on is there, and answer 503 otherwise. It
// follows the switch until ctx ends, then removes what it installed.
func InstallTlsCerts(ctx context.Context) *server.SidecarSwitchInstall {
	install := NewTlsCertsInstall(nil, nil)
	go server.HandleError(func() {
		install.Run(ctx)
	})
	return install
}

// certs.yml's bytes from this process's config mount. An error names the file.
func readTlsCertsConfigFile(name string) ([]byte, error) {
	data, err := server.ReadSidecarInstallFile(server.Config, "config", name)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTlsCertsInstall, err)
	}
	return data, nil
}

// The gate half over vault's file-clusters-api.yml: a host on its clusters, a
// gate cache on the first metadata cluster that follows {acl_}v on the host's
// Redis, and §21's standing over this process's hard deny snapshot. An error
// names the file or the part and nothing the file holds.
func tlsCertsGateFromVault(ctx context.Context) (*TlsCertsGate, error) {
	clusters, err := server.LoadFileClusters(ctx, tlsCertsFileClustersService, nil)
	if err != nil {
		// the cause may quote the file, which holds credentials: it is not passed on
		if errors.Is(err, server.ErrResourceNotFound) && !errors.Is(err, server.ErrResourceUnavailable) {
			return nil, fmt.Errorf("%w: vault holds no file-clusters-%s.yml, the first metadata cluster the gate cache reads", ErrTlsCertsInstall, tlsCertsFileClustersService)
		}
		return nil, fmt.Errorf("%w: vault's file-clusters-%s.yml did not load", ErrTlsCertsInstall, tlsCertsFileClustersService)
	}
	abuse := model.NewAbuseGate(ctx, abuseHardDeny.Load(), nil)
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{
		Blobs: clusters,
		Abuse: abuse,
	})
	gateSettings := sidecar.DefaultAclGateSettings()
	cache := sidecar.NewAclGateCache(ctx, sidecar.NewAclGateDirectSource(host.Redis, host.Blobs, gateSettings), host.Clock, gateSettings)
	removeWatch, err := cache.Watch(host.Redis)
	if err != nil {
		// with no events the cache would learn of no change
		cache.Close()
		abuse.Close()
		clusters.Close()
		return nil, fmt.Errorf("%w: the gate cache cannot watch {acl_}v: %v", ErrTlsCertsInstall, err)
	}
	return &TlsCertsGate{
		Host:   host,
		States: cache,
		Abuse:  abuse,
		Close: func() {
			removeWatch()
			cache.Close()
			abuse.Close()
			clusters.Close()
		},
	}, nil
}

// Builds what the routes run on and installs it; the returned call removes
// it. See the file header.
func buildTlsCerts(ctx context.Context, deps *TlsCertsInstallDeps, settings *TlsCertsInstallSettings) (uninstall func(), err error) {
	store := deps.Store
	names := deps.Names
	if store == nil || names == nil {
		if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
			return nil, fmt.Errorf("%w: this binary links no dns or tls tables (MG2, MG5)", ErrTlsCertsInstall)
		}
	}
	if store == nil {
		store = model.NewCertsStore(nil)
	}
	if names == nil {
		names = func(ctx context.Context, networkId sidecar.Id, root string, allocate bool) (string, []string, error) {
			return model.GetNetworkTlsCertNames(ctx, server.Id(networkId), root, allocate)
		}
	}
	readConfigFile := deps.ReadConfigFile
	if readConfigFile == nil {
		readConfigFile = readTlsCertsConfigFile
	}
	data, err := readConfigFile(TlsCertsConfigFileName)
	if err != nil {
		return nil, err
	}
	config, err := server.ParseCertsConfig(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTlsCertsInstall, err)
	}
	buildGate := deps.Gate
	if buildGate == nil {
		buildGate = tlsCertsGateFromVault
	}
	gate, err := buildGate(ctx)
	if err != nil {
		return nil, err
	}
	closeGate := func() {
		if gate != nil && gate.Close != nil {
			gate.Close()
		}
	}
	if gate == nil || gate.Host == nil || gate.Host.Authenticator == nil || gate.States == nil {
		closeGate()
		return nil, fmt.Errorf("%w: no host, authenticator or gate cache", ErrTlsCertsInstall)
	}
	// no DNS client and no ACME client: api's routes reach no zone and no CA
	service, err := certsservice.NewCertsService(ctx, gate.Host, store, nil, nil, config.Settings)
	if err != nil {
		closeGate()
		return nil, fmt.Errorf("%w: the engine: %w", ErrTlsCertsInstall, err)
	}
	source := &tlsCertSubjectSource{
		root:     config.Settings.RootDomain,
		states:   gate.States,
		view:     sidecar.NewAclGateView(gate.States, gate.Abuse, nil),
		abuse:    gate.Abuse,
		names:    names,
		settings: settings,
	}
	installed := &TlsCertsDeps{
		Service:       service,
		Store:         store,
		Authenticator: gate.Host.Authenticator,
		Subject:       source.subject,
	}
	SetTlsCerts(installed)
	// the custom-domain routes on the same gate (TLS-M6-05); QD-175's challenge writes need certs' DNS client,
	// which api does not hold, so its two routes answer 503 here
	var installedCustom *TlsCustomDeps
	if customStore, err := model.NewTlsCustomStore(&model.TlsCustomStoreOptions{Roots: []string{config.Settings.RootDomain}}); err == nil {
		installedCustom = &TlsCustomDeps{
			Store:           customStore,
			Caller:          source.customCaller,
			Roots:           []string{config.Settings.RootDomain},
			OrderRetryAfter: config.Settings.OrderRetryAfter,
		}
		SetTlsCustom(installedCustom)
	}
	return func() {
		// only this install's deps: a later install's stay
		tlsCertsDeps.CompareAndSwap(installed, nil)
		if installedCustom != nil {
			tlsCustomDeps.CompareAndSwap(installedCustom, nil)
		}
		closeGate()
	}, nil
}

// A custom route's caller from the projection (TLS-M6-05; 17.2.6's orderer): the identity key the projection admits
// the token's sidecar under, its §8 publish grants on any port of an owner label, and whether its user is an Owner
// or Admin of the network (model.ConfigReseedAdminSidecar's reading). A sidecar the projection does not admit holds
// no grant, so every route refuses it.
func (self *tlsCertSubjectSource) customCaller(ctx context.Context, principal *sidecar.Principal, now time.Time) (*model.TlsCustomCaller, error) {
	if principal == nil || principal.SidecarId == nil {
		return nil, fmt.Errorf("%w: no sidecar", ErrTlsCertSubjectUnavailable)
	}
	networkId := sidecar.Id(principal.NetworkId)
	sidecarId := sidecar.Id(*principal.SidecarId)
	snapshot, err := self.state(ctx, networkId)
	if err != nil {
		return nil, err
	}
	caller := &model.TlsCustomCaller{
		NetworkId: server.Id(principal.NetworkId),
		ClientId:  server.Id(principal.ClientId),
		SidecarId: server.Id(*principal.SidecarId),
		PublishGranted: func(ownerLabel string) bool {
			return false
		},
	}
	identityPublicKey := tlsCertIdentityKey(snapshot, sidecarId)
	if identityPublicKey == nil {
		return caller, nil
	}
	caller.IdentityPublicKey = identityPublicKey
	ports := []int{}
	for _, name := range snapshot.Acl.PublishedNames(now) {
		if !slices.Contains(ports, name.Port) {
			ports = append(ports, name.Port)
		}
	}
	subject := sidecar.AclSidecarSubject(sidecarId)
	caller.PublishGranted = func(ownerLabel string) bool {
		for _, port := range ports {
			socketName := "tls/" + strconv.Itoa(port) + "/" + ownerLabel
			for _, appMode := range []protocol.AclAppMode{protocol.AclAppMode_ACL_APP_MODE_HTTP, protocol.AclAppMode_ACL_APP_MODE_RAW} {
				grant, decision := self.view.PublishGrant(networkId, subject, socketName, appMode, now)
				if decision.Allowed && grant != nil {
					return true
				}
			}
		}
		return false
	}
	_, admin, err := model.ConfigReseedAdminSidecar(ctx, server.Id(principal.NetworkId), server.Id(*principal.SidecarId))
	if err != nil {
		return nil, fmt.Errorf("%w: the caller's standing: %w", ErrTlsCertSubjectUnavailable, err)
	}
	caller.Admin = admin
	return caller, nil
}

// An order's subject from the projection. See the file header. Safe for
// concurrent use.
type tlsCertSubjectSource struct {
	// certs.yml's root, canonical, with no final dot
	root   string
	states TlsCertsGateStates
	view   *sidecar.AclGateView
	// asked at an order's start so a standing not loaded is on its way; may be nil
	abuse    sidecar.AbuseGate
	names    func(ctx context.Context, networkId sidecar.Id, root string, allocate bool) (label string, aliases []string, err error)
	settings *TlsCertsInstallSettings
}

// TlsCertsDeps.Subject.
func (self *tlsCertSubjectSource) subject(ctx context.Context, principal *sidecar.Principal, now time.Time) (*TlsCertSubject, error) {
	if principal == nil || principal.SidecarId == nil {
		return &TlsCertSubject{}, nil
	}
	networkId := sidecar.Id(principal.NetworkId)
	sidecarId := sidecar.Id(*principal.SidecarId)
	if self.abuse != nil {
		// a standing not loaded starts loading while the state is read
		self.abuse.Standing(principal.NetworkId)
	}
	snapshot, err := self.state(ctx, networkId)
	if err != nil {
		return nil, err
	}
	identityPublicKey := tlsCertIdentityKey(snapshot, sidecarId)
	if identityPublicKey == nil {
		// no sidecar of the network the projection admits now: granted nothing
		return &TlsCertSubject{}, nil
	}
	granted, err := self.granted(snapshot, networkId, sidecarId, now)
	if err != nil {
		return nil, err
	}
	if !granted {
		return &TlsCertSubject{IdentityPublicKey: identityPublicKey}, nil
	}
	label, aliases, err := self.names(ctx, networkId, self.root, true)
	if err != nil {
		return nil, fmt.Errorf("%w: the network's names: %w", ErrTlsCertSubjectUnavailable, err)
	}
	if label == "" {
		return nil, fmt.Errorf("%w: the network has no label", ErrTlsCertSubjectUnavailable)
	}
	identifierSet, err := sidecar.TlsIdentifierSet(self.root, label, "", aliases)
	if err != nil {
		return nil, fmt.Errorf("%w: the network's names give no identifier set", ErrTlsCertSubjectUnavailable)
	}
	return &TlsCertSubject{
		IdentifierSet:     identifierSet,
		IdentityPublicKey: identityPublicKey,
		Granted:           true,
	}, nil
}

// The network's loaded state, asking the cache to load it when it holds none
// and waiting for the install for SubjectLoadWait.
func (self *tlsCertSubjectSource) state(ctx context.Context, networkId sidecar.Id) (*sidecar.AclGateSnapshot, error) {
	loaded := func() *sidecar.AclGateSnapshot {
		snapshot, status := self.states.State(networkId)
		if status != sidecar.AclGateLoaded || snapshot == nil || snapshot.Acl == nil {
			return nil
		}
		return snapshot
	}
	if snapshot := loaded(); snapshot != nil {
		return snapshot, nil
	}
	installed := make(chan struct{})
	var installOnce sync.Once
	// registered before the ask, so an install the ask starts is never missed
	remove := self.states.AddInstallCallback(func(installedNetworkId sidecar.Id, snapshot *sidecar.AclGateSnapshot) {
		if installedNetworkId == networkId {
			installOnce.Do(func() {
				close(installed)
			})
		}
	})
	defer remove()
	self.states.Changed(networkId)
	// an install between the first read and the registration is in the state now
	if snapshot := loaded(); snapshot != nil {
		return snapshot, nil
	}
	if 0 < self.settings.SubjectLoadWait {
		select {
		case <-installed:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(self.settings.SubjectLoadWait):
		}
	}
	if snapshot := loaded(); snapshot != nil {
		return snapshot, nil
	}
	return nil, fmt.Errorf("%w: the gate cache holds no verified state of the network", ErrTlsCertSubjectUnavailable)
}

// The identity key the state admits the sidecar under now, nil for a sidecar
// it never admitted, has revoked or has parked: its SidecarAdmit's, or the
// Genesis's for the network's first device.
func tlsCertIdentityKey(snapshot *sidecar.AclGateSnapshot, sidecarId sidecar.Id) []byte {
	if !snapshot.Acl.Admitted(sidecarId) {
		return nil
	}
	view := snapshot.Acl.View()
	if view == nil || view.Parked(sidecarId) {
		return nil
	}
	if admission := view.Sidecar(sidecarId); admission != nil && admission.Row != nil {
		if identityPublicKey := admission.Row.GetIdentityPublicKey(); len(identityPublicKey) != 0 {
			return bytes.Clone(identityPublicKey)
		}
	}
	if genesis := view.Genesis(); genesis != nil && bytes.Equal(genesis.GetOwnerSidecarId(), sidecarId.Bytes()) {
		if identityPublicKey := genesis.GetOwnerIdentityPublicKey(); len(identityPublicKey) != 0 {
			return bytes.Clone(identityPublicKey)
		}
	}
	return nil
}

// Whether the gate holds an active smtp/ or tls/ grant of the network for the
// sidecar at now (the file header's grant row). An error is a refusal that
// says nothing of the grant: a state or a standing the view has not loaded.
func (self *tlsCertSubjectSource) granted(snapshot *sidecar.AclGateSnapshot, networkId sidecar.Id, sidecarId sidecar.Id, now time.Time) (bool, error) {
	subject := sidecar.AclSidecarSubject(sidecarId)
	unloaded := func(decision sidecar.AclDecision) error {
		if !decision.Allowed && decision.Reason == sidecar.AclReasonNoState {
			return fmt.Errorf("%w: the gate's view of the network is not loaded", ErrTlsCertSubjectUnavailable)
		}
		return nil
	}
	decision := self.view.MayListen(networkId, subject, sidecar.AclSocketName{PublicName: tlsCertsMailListenName}, now)
	if err := unloaded(decision); err != nil {
		return false, err
	}
	if decision.Allowed {
		return true, nil
	}
	for _, name := range snapshot.Acl.PublishedNames(now) {
		if strings.Contains(name.Host, ".") {
			// a scope host: TLS-M6-03's
			continue
		}
		host := strings.ReplaceAll(name.Host, "*", tlsCertsGlobHostFill)
		socketName := "tls/" + strconv.Itoa(name.Port) + "/" + host
		for _, appMode := range []protocol.AclAppMode{protocol.AclAppMode_ACL_APP_MODE_HTTP, protocol.AclAppMode_ACL_APP_MODE_RAW} {
			grant, decision := self.view.PublishGrant(networkId, subject, socketName, appMode, now)
			if err := unloaded(decision); err != nil {
				return false, err
			}
			if decision.Allowed && grant != nil {
				return true, nil
			}
		}
	}
	return false, nil
}
