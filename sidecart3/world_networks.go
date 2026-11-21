package sidecart3

// A T3 world of networks (the end-to-end tests' clients; §22.4's T3, §3.7's F1): many full sidecars in each of several
// networks, each a T3Sidecar enrolled through T3Api from an auth key its network's owner made, Online on server's
// exchange, and the services a test serves reached at their origins (T3Api.ServeService), so a test drives what
// production gives a device end to end. A T3 Net's AddSidecar still refuses: its helpers stand on T2's fake api.
//
//	stores    one T3 Net (NewNet): its buckets on the local stack's MinIO, server's Redis, its capture and its fakes;
//	          the world's hosts run on a copy of its ServiceHost on real time (Host), since server's api, exchange and
//	          oauth keep real time and judge every token and proof by it
//	api       T3Api over server's routes, with what api's process installs for the log and the token exchange
//	          (controller.InstallApiAuthorization's projection part): server's sequencer over the Net's first metadata
//	          cluster (controller.NewHostedConfigLogSequencer), a gate cache over it, and the exchange's scopes from
//	          that cache (oauth.NewProjectionExchangeScopeSource), so a service token carries exactly what §8's table
//	          grants at the log's newest state
//	oauth     the issuer is the sidecars' api origin and WARP_DOMAIN the world's domain while it runs, so a DPoP proof's
//	          htu and a service's origin are what server checks them against; the svc- key is a throwaway the world
//	          makes, never vault's, and every host the world builds verifies tokens and rev/ under its own verifier of
//	          it, as a block given HostDeps.ServiceTokens does, never the process's
//	networks  each a personal network in preview_networks, funded in the Net's quota and in every gated service's gate
//	          (AddNetwork): its first sidecar bootstraps it, signing the Genesis and base 1 through api (Bootstrap),
//	          and admits the others (Admit), each then following the log through its own token with its own key
//	          client; statements a device files go through POST /network/config/delta with its token (FileStatement)
//	keys      sidecartest's FakeGkm on the Net's real stores, writing each generation's GenRotate where evs's gate reads
//	          it, one client a device, handed each state its follower verifies; at KF1 the world's client also stores
//	          the repo's GroupForm, signed by the device's identity key, where gkm stores it and evs's activate route
//	          reads it (t3WorldGkmClient): gkm's own host and the MLS key client are not this world's (the file's gap
//	          list)
//	config    the config repo's heads are sidecartest's FakeConfigHeads in memory, one per network, as S1's genesis
//	          clause keeps them: the config group's KF1 on gkm is not this world's either
//
// Every wait is on a monitor, a follow or a gate's load, bounded by SyncTimeout in real time only to fail a broken run.
// What the world installs process-wide (the sequencer, the exchange's source, oauth's issuer and keys, WARP_DOMAIN,
// sidecar.yml's preview_networks) is restored by Close, so worlds run one at a time, as every T3 test does. Its
// sidecars run until it closes, whatever context a call that added them carried. Safe for concurrent use once made.
//
// Not here yet: gkm's host and the devices' MLS key clients, so no KeyPackage, Welcome or LeafWrap is made, and a key
// reaches a device through FakeGkm's deliveries; the config repo's heads on evs; the api's other installs (approvals,
// dns scopes, edge logins, the contract gate, rev/'s store), which no flow of this world reaches; and §7.9's append
// limits and §20.4's admission caps, off by default (T3WorldSettings.AdjustConfigLog).

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecarhost"
)

// What a T3 world runs with.
type T3WorldSettings struct {
	// The T3 Net the world's stores, capture and fakes are. nil: DefaultSettings.
	Net *Settings
	// T3Api's and the sidecars'. Their origins must sit under Domain. nil: DefaultT3SidecarSettings.
	Sidecar *T3SidecarSettings
	// Server's exchange the sidecars connect on. nil: DefaultExchangeSettings.
	Exchange *ExchangeSettings
	// The domain the origins sit under, WARP_DOMAIN while the world runs: sidecart3.example, a reserved name nothing
	// resolves. The token exchange issues for https://<service>.<domain> alone, and DPoP's api origin is
	// https://api.<domain>.
	Domain string
	// Each gate cache's: api's and each service's the world serves. nil: sidecar.DefaultAclGateSettings with its poll
	// off and no wait after a failed load, so a load follows a statement at once and the world's syncs, not a timer,
	// move each cache.
	Gate *sidecar.AclGateSettings
	// The services whose gates the world opens to every network of its, on the Net's sidecar.yml (sidecartest's
	// FakeServiceGates): evs and gkm.
	GatedServices []string
	// What the world's sequencer runs with past api's own (model.NewApiConfigLogSettings), nil for nothing. The
	// default takes §7.9's append limits and §20.4's admission caps off, the limiter and both caps: their numbers come
	// from pro.yml's tiers alone, which the test stack's config does not carry, so every append would meet a limit of
	// none (429 at the bootstrap's base 1).
	AdjustConfigLog func(settings *model.ConfigLogSettings)
	// What each network holds in the Net's quota: 1 TiB, past what any test writes.
	NetworkBalanceByteCount int64
	// How long an auth key an owner makes for one enrollment lives: 10 min.
	AuthKeyDuration time.Duration
	// How many sidecars enroll at once: 4.
	EnrollConcurrency int
	// How long a statement a device files lives past now, its header's not_after: 1 h, inside §7.9's 7 days.
	StatementLife time.Duration
	// How long, in real time, a wait for a gate's load, a follower's catch-up or a sidecar's standing may take: 60 s,
	// only a broken run's bound.
	SyncTimeout time.Duration
}

func DefaultT3WorldSettings() *T3WorldSettings {
	gate := sidecar.DefaultAclGateSettings()
	gate.PollInterval = 0
	gate.PollJitter = 0
	gate.FailureBackoff = 0
	return &T3WorldSettings{
		Net:           DefaultSettings(),
		Sidecar:       DefaultT3SidecarSettings(),
		Exchange:      DefaultExchangeSettings(),
		Domain:        "sidecart3.example",
		Gate:          gate,
		GatedServices: []string{"evs", "gkm"},
		AdjustConfigLog: func(settings *model.ConfigLogSettings) {
			settings.Limiter = nil
			settings.AdmissionCap = nil
			settings.ActivationCap = nil
		},
		NetworkBalanceByteCount: 1024 * 1024 * 1024 * 1024,
		AuthKeyDuration:         10 * time.Minute,
		EnrollConcurrency:       4,
		StatementLife:           time.Hour,
		SyncTimeout:             60 * time.Second,
	}
}

// The purpose a GroupForm is signed under (signed.proto's registry, §6.11).
const t3WorldGroupFormPurpose = "group-form"

// One gate cache of the world's, api's or a service's, and how many loads it has finished, which a sync waits on.
type t3WorldGate struct {
	name  string
	cache *sidecar.AclGateCache
	loads *sidecar.MonitorValue[uint64]
	close func()
}

// See the file's header.
type T3World struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *T3WorldSettings
	net      *sidecartest.Net
	host     *sidecar.ServiceHost
	// the verifier of the world's svc- key, which every host the world builds verifies tokens and rev/ under
	serviceTokens *oauth.ServiceTokenVerifier
	api           *T3Api
	exchange      *T3SidecarExchange
	gkm           *sidecartest.FakeGkm

	stateLock sync.Mutex
	gates     []*t3WorldGate
	networks  []*T3Network
	// what Close undoes, newest first
	closes    []func()
	popConfig func()
	closed    bool
}

// A world on the private stores and the local stack's MinIO, inside a TestEnv that applies the migrations: see the
// file's header. Closed at t's end, or by Close. settings nil takes the defaults.
func NewT3World(t testing.TB, settings *T3WorldSettings) (world *T3World, returnErr error) {
	if settings == nil {
		settings = DefaultT3WorldSettings()
	}
	if !server.SidecarMg1Linked() || !server.SidecarMg2Linked() {
		return nil, errors.New("the world needs the log's tables, MG1 and MG2: build with -tags sidecar_pending")
	}
	sidecarSettings := settings.Sidecar
	if sidecarSettings == nil {
		sidecarSettings = DefaultT3SidecarSettings()
	}
	// every origin under the world's domain, or the exchange and DPoP refuse what the sidecars send
	if sidecarSettings.ApiOrigin != "https://api."+settings.Domain {
		return nil, fmt.Errorf("the api origin %q is not https://api.%s", sidecarSettings.ApiOrigin, settings.Domain)
	}
	for _, service := range sidecarSettings.ServiceNames {
		if origin := fmt.Sprintf(sidecarSettings.ServiceOriginFormat, service); origin != "https://"+service+"."+settings.Domain {
			return nil, fmt.Errorf("the origin of %s, %q, is not https://%s.%s", service, origin, service, settings.Domain)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	self := &T3World{ctx: ctx, cancel: cancel, settings: settings}
	t.Cleanup(self.Close)
	defer func() {
		if returnErr != nil {
			self.Close()
		}
	}()

	previousDomain, hadDomain := os.LookupEnv("WARP_DOMAIN")
	if err := os.Setenv("WARP_DOMAIN", settings.Domain); err != nil {
		return nil, err
	}
	self.closes = append(self.closes, func() {
		if hadDomain {
			os.Setenv("WARP_DOMAIN", previousDomain)
		} else {
			os.Unsetenv("WARP_DOMAIN")
		}
	})

	netSettings := settings.Net
	if netSettings == nil {
		netSettings = DefaultSettings()
	}
	self.net = sidecartest.NewNet(ctx, NetSettings(t, netSettings))
	self.closes = append(self.closes, self.net.Close)
	serviceTokens, pops, err := installT3WorldOauth(sidecarSettings.ApiOrigin)
	if err != nil {
		return nil, err
	}
	self.closes = append(self.closes, pops)
	self.serviceTokens = serviceTokens
	// the Net's stores on real time, since server's api, exchange and oauth judge every token and proof by it, its
	// Authenticator verifying under the world's own svc- key as a block given HostDeps.ServiceTokens does
	host := *self.net.Host()
	host.Clock = sidecar.RealClock{}
	host.Authenticator = sidecarhost.NewAuthenticatorWithVerifier(serviceTokens)
	self.host = &host

	// api's log and exchange, as its process installs them over its first cluster
	apiGate, err := self.addGate("api")
	if err != nil {
		return nil, err
	}
	first, err := host.Blobs.FirstCluster()
	if err != nil {
		return nil, err
	}
	configLogSettings := model.NewApiConfigLogSettings()
	if settings.AdjustConfigLog != nil {
		settings.AdjustConfigLog(configLogSettings)
	}
	model.SetConfigLogSequencer(controller.NewHostedConfigLogSequencer(self.host, first, configLogSettings))
	self.closes = append(self.closes, func() { model.SetConfigLogSequencer(nil) })
	self.closes = append(self.closes, oauth.SetExchangeScopeSource(oauth.NewProjectionExchangeScopeSource(apiGate.cache, &oauth.ExchangeScopeSettings{Clock: host.Clock})))

	self.api = NewT3Api(ctx, self.net, sidecarSettings)
	exchange, err := NewT3SidecarExchange(ctx, t, settings.Exchange)
	if err != nil {
		return nil, err
	}
	self.exchange = exchange
	self.closes = append(self.closes, exchange.Close)
	self.gkm = sidecartest.NewFakeGkm(host.Clock, host.Blobs, nil)
	return self, nil
}

// The world's issuer at origin and its svc- key, a throwaway made here, which api's process signs service tokens and
// rev/ objects with, until the returned pop; and the verifier holding the key, as a block's mounted jwks gives one,
// which the world hands each host it builds (HostDeps.ServiceTokens' way), so no host reads the process's.
func installT3WorldOauth(origin string) (*oauth.ServiceTokenVerifier, func(), error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	thumbprint, err := oauth.SignerKid(&privateKey.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	key := &oauth.SignerKey{
		Kid:        oauth.SignerKeySetService.KidPrefix() + thumbprint,
		Alg:        "ES256",
		Set:        oauth.SignerKeySetService,
		PrivateKey: privateKey,
	}
	pops := []func(){oauth.Testing_SetIssuer(origin), oauth.Testing_SetSignerKeys(key)}
	popAll := func() {
		for index := len(pops) - 1; 0 <= index; index -= 1 {
			pops[index]()
		}
	}
	verifier, err := oauth.NewServiceTokenVerifier(&oauth.PublicJwksDocument{
		Issuer: origin,
		Keys:   oauth.PublicJwks(oauth.SignerKeySetService).Keys,
	})
	if err != nil {
		popAll()
		return nil, nil, err
	}
	return verifier, popAll, nil
}

// A gate cache over the world's host named name, watching the log's {acl_}v events, which every sync waits on until
// its owner removes it or the world closes. Its close is safe to call again, and the world's Close calls it in its
// turn.
func (self *T3World) addGate(name string) (*t3WorldGate, error) {
	gateSettings := *self.settings.Gate
	gate := &t3WorldGate{name: name, loads: sidecar.NewMonitorValue(uint64(0))}
	gateSettings.OnLoad = func(networkId sidecar.Id, pin *bridge.AuthPin, err error) {
		gate.loads.Update(func(count uint64) uint64 { return count + 1 })
	}
	cache, closeCache, err := sidecarhost.WatchAclGate(self.ctx, self.host, nil, &gateSettings)
	if err != nil {
		return nil, fmt.Errorf("%s's gate cache: %w", name, err)
	}
	gate.cache = cache
	gate.close = sync.OnceFunc(closeCache)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		gate.close()
		return nil, errors.New("the world has closed")
	}
	self.gates = append(self.gates, gate)
	self.closes = append(self.closes, gate.close)
	return gate, nil
}

// Stops waiting on gate, which its owner closes.
func (self *T3World) removeGate(gate *t3WorldGate) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.gates = slices.DeleteFunc(self.gates, func(held *t3WorldGate) bool { return held == gate })
}

// The Net whose stores, capture and fakes the world's are.
func (self *T3World) Net() *sidecartest.Net {
	return self.net
}

// The Net's host on real time, which the world's services run on.
func (self *T3World) Host() *sidecar.ServiceHost {
	return self.host
}

// Server's api in process, the service origins' lb.
func (self *T3World) Api() *T3Api {
	return self.api
}

// The world's settings.
func (self *T3World) Settings() *T3WorldSettings {
	return self.settings
}

// The networks added so far, in order.
func (self *T3World) Networks() []*T3Network {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.networks)
}

// Ends the world: its networks' sidecars, then the services, gates and installs it made, newest first. Safe to call
// again.
func (self *T3World) Close() {
	var networks []*T3Network
	var closes []func()
	var popConfig func()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return
		}
		self.closed = true
		networks, closes, popConfig = self.networks, self.closes, self.popConfig
		self.networks, self.closes, self.gates, self.popConfig = nil, nil, nil, nil
	}()
	for _, network := range networks {
		network.close()
	}
	if popConfig != nil {
		popConfig()
	}
	for index := len(closes) - 1; 0 <= index; index -= 1 {
		closes[index]()
	}
	self.cancel()
}

// Waits until every gate cache of the world holds networkId at version or past it, asking each to load: what a
// statement's filer waits on before a device acts on it at a gate, as api's {acl_}v event brings a block's gate
// within 5 s (§8.9). SyncTimeout bounds it.
func (self *T3World) SyncGates(ctx context.Context, networkId server.Id, version uint64) error {
	ctx, cancel := context.WithTimeout(ctx, self.settings.SyncTimeout)
	defer cancel()
	self.stateLock.Lock()
	gates := slices.Clone(self.gates)
	self.stateLock.Unlock()
	for _, gate := range gates {
		for {
			// armed before the read, so a load between them wakes the wait
			_, loaded := gate.loads.Get()
			snapshot, status := gate.cache.State(sidecar.Id(networkId))
			if status == sidecar.AclGateLoaded && snapshot != nil && version <= snapshot.Pin.GetVersion() {
				break
			}
			gate.cache.Changed(sidecar.Id(networkId))
			select {
			case <-loaded:
			case <-ctx.Done():
				held := uint64(0)
				if snapshot != nil {
					held = snapshot.Pin.GetVersion()
				}
				return fmt.Errorf("%s's gate holds network %s at %d (%v), not %d: %w", gate.name, networkId, held, status, version, ctx.Err())
			}
		}
	}
	return nil
}

// Puts sidecar.yml's preview_networks at every network of the world, as an operator lists the networks a preview
// admits, and opens each gated service's gate on the Net's sidecar.yml to them.
func (self *T3World) gateNetworksWithLock() {
	networkIds := []string{}
	connectIds := []connect.Id{}
	for _, network := range self.networks {
		networkIds = append(networkIds, strconv.Quote(network.NetworkId.String()))
		connectIds = append(connectIds, connect.Id(network.NetworkId))
	}
	if self.popConfig != nil {
		self.popConfig()
	}
	self.popConfig = server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%s]\n", strings.Join(networkIds, ", ")))
	for _, service := range self.settings.GatedServices {
		self.net.Fakes().Gates.OpenService(service, connectIds...)
	}
}

// A personal network of a new owner, named name: in preview_networks, in every gated service's gate and funded in the
// Net's quota, with no sidecar yet.
func (self *T3World) AddNetwork(ctx context.Context, name string) (*T3Network, error) {
	network := &T3Network{
		world:        self,
		Name:         name,
		NetworkId:    server.NewId(),
		OwnerUserId:  server.NewId(),
		configRepoId: sidecar.NewEvsRepoId(),
	}
	networkName := fmt.Sprintf("t3%s%s", strings.ReplaceAll(network.NetworkId.String(), "-", "")[:12], strings.ToLower(name))
	model.Testing_CreateNetwork(ctx, network.NetworkId, networkName, network.OwnerUserId)
	network.root = "Bearer " + session.NewByJwt(network.NetworkId, network.OwnerUserId, networkName, false, false).Testing_Sign()
	network.heads = sidecartest.NewFakeConfigHeads(network.configRepoId)
	self.net.Fakes().Quota.SetBalance(connect.Id(network.NetworkId), self.settings.NetworkBalanceByteCount)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, errors.New("the world has closed")
	}
	self.networks = append(self.networks, network)
	self.gateNetworksWithLock()
	return network, nil
}

// One network of a world: its owner, its sidecars, its log's config heads and genesis.
type T3Network struct {
	world        *T3World
	Name         string
	NetworkId    server.Id
	OwnerUserId  server.Id
	configRepoId sidecar.Id
	// the owner's own credential, a network jwt as a signed-in owner holds
	root  string
	heads *sidecartest.FakeConfigHeads

	stateLock sync.Mutex
	devices   []*T3Device
	owner     *T3Device
	genesis   *[sha256.Size]byte
}

// The network's owner member, its owner user in a personal network (§7.5).
func (self *T3Network) MemberId() sidecar.Id {
	return sidecar.Id(self.OwnerUserId)
}

// Every sidecar added, in order.
func (self *T3Network) Devices() []*T3Device {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.devices)
}

// The device that bootstrapped the network, nil before.
func (self *T3Network) Owner() *T3Device {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.owner
}

// Closes every device, newest first.
func (self *T3Network) close() {
	devices := self.Devices()
	for index := len(devices) - 1; 0 <= index; index -= 1 {
		devices[index].close()
	}
}

// A one-use auth key the owner makes through POST /network/auth-keys, as `ur network auth-key` does.
func (self *T3Network) authKey(ctx context.Context) (string, error) {
	status, answer, err := self.world.api.Call(ctx, http.MethodPost, "/network/auth-keys", self.root, &model.CreateAuthKeyArgs{
		DurationSeconds: int64(self.world.settings.AuthKeyDuration / time.Second),
		MaxUses:         1,
	})
	if err != nil {
		return "", err
	}
	result := &model.CreateAuthKeyResult{}
	if status != http.StatusOK || json.Unmarshal(answer, result) != nil || result.AuthKey == "" {
		return "", fmt.Errorf("POST /network/auth-keys: %d", status)
	}
	return result.AuthKey, nil
}

// count sidecars, each enrolled through api from an auth key of its own and Online on server's exchange, at most
// EnrollConcurrency at once, named for the network and their order. settings, called for each, adjusts a sidecar's
// settings past the defaults; nil for none. A failure closes what it enrolled and answers the first error.
func (self *T3Network) AddSidecars(ctx context.Context, count int, settings func(name string, sidecarSettings *sidecar.SidecarSettings)) ([]*T3Device, error) {
	start := len(self.Devices())
	added := make([]*T3Device, count)
	errs := make([]error, count)
	concurrency := max(1, self.world.settings.EnrollConcurrency)
	tickets := make(chan struct{}, concurrency)
	var group sync.WaitGroup
	for index := range count {
		group.Go(func() {
			tickets <- struct{}{}
			defer func() { <-tickets }()
			name := fmt.Sprintf("%s-%d", self.Name, start+index)
			added[index], errs[index] = self.enroll(ctx, name, settings)
		})
	}
	group.Wait()
	if err := errors.Join(errs...); err != nil {
		for _, device := range added {
			if device != nil {
				device.close()
			}
		}
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.devices = append(self.devices, added...)
	return added, nil
}

// One sidecar named name enrolled from a fresh auth key, Online. It runs until the world closes, ctx bounding only its
// enrollment.
func (self *T3Network) enroll(ctx context.Context, name string, settings func(name string, sidecarSettings *sidecar.SidecarSettings)) (*T3Device, error) {
	authKey, err := self.authKey(ctx)
	if err != nil {
		return nil, err
	}
	sidecarSettings := sidecar.DefaultSidecarSettings()
	if settings != nil {
		settings(name, sidecarSettings)
	}
	running, err := StartT3Sidecar(self.world.ctx, self.world.net, name, self.world.api, self.world.exchange, sidecarSettings)
	if err != nil {
		return nil, err
	}
	if err := running.EnrollOnline(ctx, &sidecar.SidecarEnrollCredential{AuthKey: authKey}); err != nil {
		running.Close()
		return nil, err
	}
	identity := running.Sidecar().Identity()
	if identity == nil || server.Id(identity.NetworkId) != self.NetworkId {
		running.Close()
		return nil, fmt.Errorf("%s enrolled in no network of %s's", name, self.Name)
	}
	device := &T3Device{T3Sidecar: running, Network: self}
	device.Gkm = &t3WorldGkmClient{
		GkmClient: self.world.gkm.Client(identity.SidecarId, func(purpose string, signedType bridge.SignedType, body []byte, agentId *sidecar.Id) (*bridge.Signed, error) {
			return running.Sidecar().Sign(purpose, signedType, body, agentId)
		}),
		device: device,
	}
	return device, nil
}

// FakeGkm's client for one device (the file's keys row), which also stores the GroupForm gkm's KF1 stores (gkm-1, §10.6),
// signed by the device's identity key at its follower's pin, at gkm/repo/<repo_id>/<seq>: evs's activate route reads the
// newest of them for the group whose gen/1 it then looks for (evsservice's repo.go), and FakeGkm stores no form.
type t3WorldGkmClient struct {
	sidecar.GkmClient
	device *T3Device
}

// KF1: FakeGkm's group and generation 1, then the repo's first GroupForm naming that group.
func (self *t3WorldGkmClient) CreateGroup(ctx context.Context, args *sidecar.GkmCreateGroupArgs) (*sidecar.EvsRepoKey, error) {
	key, err := self.GkmClient.CreateGroup(ctx, args)
	if err != nil {
		return nil, err
	}
	world := self.device.Network.world
	groupId, _, ok := world.gkm.Newest(args.RepoId)
	if !ok {
		return nil, fmt.Errorf("the fake gkm holds no group of repo %s after its KF1", args.RepoId)
	}
	follower := self.device.Follower()
	if follower == nil || follower.Client() == nil {
		return nil, fmt.Errorf("%s forms a group by a follower that stands", self.device.Name)
	}
	formBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(&protocol.GroupForm{
		RepoId:     args.RepoId.Bytes(),
		GroupId:    groupId.Bytes(),
		Generation: 1,
		Auth:       proto.Clone(follower.Client().Pin()).(*bridge.AuthPin),
	})
	if err != nil {
		return nil, err
	}
	signed, err := self.device.Sidecar().Sign(t3WorldGroupFormPurpose, bridge.SignedType_SIGNED_GROUP_FORM, formBytes, nil)
	if err != nil {
		return nil, err
	}
	signedBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	if err != nil {
		return nil, err
	}
	cluster, err := world.host.Blobs.FirstCluster()
	if err != nil {
		return nil, err
	}
	if _, err := sidecar.PutImmutable(ctx, cluster, gkmservice.GkmFormKey(args.RepoId, 1), signedBytes, nil); err != nil {
		return nil, fmt.Errorf("repo %s's GroupForm: %w", args.RepoId, err)
	}
	return key, nil
}

// The genesis's A_0 once Bootstrap made it.
func (self *T3Network) genesisChain() (*[sha256.Size]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.genesis == nil {
		return nil, fmt.Errorf("%s has no genesis yet", self.Name)
	}
	return self.genesis, nil
}

// owner, an enrolled sidecar of the network, signs the Genesis and base 1 through POST /network/config/projection
// (§7.9's bootstrap), its first head on the network's config heads, then follows the log with its key client: base 1
// held by the owner's follower and by every gate of the world.
func (self *T3Network) Bootstrap(ctx context.Context, owner *T3Device) error {
	if owner.Network != self {
		return fmt.Errorf("%s is no sidecar of %s", owner.Name, self.Name)
	}
	running := owner.Sidecar()
	identity := running.Identity()
	sign := func(purpose string, signedType bridge.SignedType, body []byte) (*bridge.Signed, error) {
		return running.Sign(purpose, signedType, body, nil)
	}
	routes := &t3NetworkRoutes{api: self.world.api, bearer: owner.Bearer()}
	bootstrap, err := sidecar.BootstrapNetwork(ctx, &sidecar.ConfigBootstrapArgs{
		NetworkId:         identity.NetworkId,
		OwnerMemberId:     self.MemberId(),
		SidecarId:         identity.SidecarId,
		IdentityPublicKey: identity.IdentityPublicKey,
		Sign:              sign,
		ConfigRepoId:      self.configRepoId,
		Heads:             self.heads.ForSigner(identity.SidecarId, sign),
		Publisher:         routes,
		Projection:        routes,
		Clock:             sidecar.RealClock{},
	})
	if err != nil {
		return fmt.Errorf("%s's bootstrap through api: %w", self.Name, err)
	}
	if !bootstrap.Won || bootstrap.Pin.GetVersion() != 1 {
		return fmt.Errorf("%s's genesis did not take entry 1: won %t at %d", self.Name, bootstrap.Won, bootstrap.Pin.GetVersion())
	}
	genesis := bootstrap.GenesisChain
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.owner = owner
		self.genesis = &genesis
	}()
	if err := owner.follow(ctx, genesis); err != nil {
		return err
	}
	return self.Sync(ctx, bootstrap.Pin.GetVersion())
}

// The owner admits each device, a new device of its member, as `ur approve` does on its approval item (§5.6, §7.9's
// SidecarAdmit by the member's consented device); then each trusts the genesis and follows the log with its key
// client, standing at the admissions. Each admission is one act, in order.
func (self *T3Network) Admit(ctx context.Context, devices []*T3Device) error {
	owner := self.Owner()
	genesis, err := self.genesisChain()
	if err != nil || owner == nil {
		return fmt.Errorf("%s admits nothing before its bootstrap: %w", self.Name, err)
	}
	for _, device := range devices {
		identity := device.Sidecar().Identity()
		approvalId, err := owner.Sidecar().OpenAdmission(&sidecar.SidecarAdmissionRequest{
			SidecarId:         identity.SidecarId,
			MemberId:          self.MemberId(),
			IdentityPublicKey: identity.IdentityPublicKey,
		})
		if err != nil {
			return fmt.Errorf("%s's admission item: %w", device.Name, err)
		}
		if err := owner.Sidecar().DecideApproval(ctx, approvalId, sidecar.SidecarDecisionApprove, sidecar.SidecarActConfirmed, nil); err != nil {
			return fmt.Errorf("%s's admission: %w", device.Name, err)
		}
	}
	client := owner.follower.Client()
	if err := client.CatchUp(ctx); err != nil {
		return err
	}
	tip := client.Pin().GetVersion()
	for _, device := range devices {
		if err := device.follow(ctx, *genesis); err != nil {
			return err
		}
	}
	return self.Sync(ctx, tip)
}

// Waits until every gate of the world and every following device of the network hold its log at version or past
// it, so each key client holds the state there (SidecarConfigFollowerArgs.Gkm).
func (self *T3Network) Sync(ctx context.Context, version uint64) error {
	if err := self.world.SyncGates(ctx, self.NetworkId, version); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, self.world.settings.SyncTimeout)
	defer cancel()
	for _, device := range self.Devices() {
		follower := device.Follower()
		if follower == nil {
			continue
		}
		if _, err := follower.Client().WaitFor(ctx, version); err != nil {
			return fmt.Errorf("%s did not follow %s's log to %d: %w", device.Name, self.Name, version, err)
		}
	}
	return nil
}

// Files one of §7.9's or §8.8's statements as device: body, whose header is set here at device's verified pin with a
// fresh nonce and a not_after StatementLife from now, signed by device's identity key and posted to POST
// /network/config/delta with its token, as its act runner posts one (CORE-M2-02); then synced (Sync) at the entry's
// pin, which it answers. A refusal answers api's status and message.
func (self *T3Network) FileStatement(ctx context.Context, device *T3Device, signedType bridge.SignedType, body proto.Message) (*bridge.AuthPin, error) {
	follower := device.Follower()
	if follower == nil || follower.Client() == nil {
		return nil, fmt.Errorf("%s files by a follower that stands", device.Name)
	}
	purpose, ok := sidecar.ConfigStatementPurpose(signedType)
	if !ok {
		return nil, fmt.Errorf("%s is no statement of the log", signedType)
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	header := &bridge.StatementHeader{
		NetworkId:  sidecar.Id(self.NetworkId).Bytes(),
		At:         proto.Clone(follower.Client().Pin()).(*bridge.AuthPin),
		Nonce:      nonce,
		NotAfterMs: self.world.host.ClockOrReal().Now().Add(self.world.settings.StatementLife).UnixMilli(),
	}
	statement := proto.Clone(body)
	if err := setStatementHeader(statement, header); err != nil {
		return nil, err
	}
	statementBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(statement)
	if err != nil {
		return nil, err
	}
	signed, err := device.Sidecar().Sign(purpose, signedType, statementBytes, nil)
	if err != nil {
		return nil, err
	}
	signedBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	if err != nil {
		return nil, err
	}
	status, answer, err := self.world.api.Call(ctx, http.MethodPost, "/network/config/delta", device.Bearer(), &controller.NetworkConfigDeltaArgs{Signed: signedBytes})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s's %s: POST /network/config/delta: %d %s", device.Name, signedType, status, strings.TrimSpace(string(answer)))
	}
	result := &controller.NetworkConfigPinResult{}
	if err := json.Unmarshal(answer, result); err != nil || result.N == 0 {
		return nil, fmt.Errorf("%s's %s: POST /network/config/delta answered no pin", device.Name, signedType)
	}
	pin := &bridge.AuthPin{Version: result.N, Chain: result.Chain}
	return pin, self.Sync(ctx, pin.GetVersion())
}

// Sets a statement's StatementHeader, its field named header, which every statement of the log carries.
func setStatementHeader(statement proto.Message, header *bridge.StatementHeader) error {
	message := statement.ProtoReflect()
	field := message.Descriptor().Fields().ByName("header")
	if field == nil || field.Message() == nil || field.Message().FullName() != header.ProtoReflect().Descriptor().FullName() {
		return fmt.Errorf("%s carries no StatementHeader", message.Descriptor().FullName())
	}
	message.Set(field, protoreflect.ValueOfMessage(header.ProtoReflect()))
	return nil
}

// creator, a device whose follower stands, creates a repo of kind through evs as `ur evs create` does (§11.12): its
// RepoCreate filed through api (FileStatement), placement, KF1 by its key client and activation at the evs the world
// serves. The repo, on the creator's EvsClient.
func (self *T3Network) CreateRepo(ctx context.Context, creator *T3Device, kind protocol.RepoKind) (*sidecar.EvsRepo, error) {
	client, err := creator.EvsClient()
	if err != nil {
		return nil, err
	}
	repo, err := client.CreateRepo(ctx, &sidecar.EvsCreateRepoArgs{Kind: kind, Confirmed: true})
	if err != nil {
		return nil, fmt.Errorf("%s's repo of %s: %w", creator.Name, kind, err)
	}
	return repo, nil
}

// admin grants subject, a sidecar or a member, role on repoId by a RepoGrant it files (§8.8), synced at its entry.
func (self *T3Network) Grant(ctx context.Context, admin *T3Device, repoId sidecar.Id, subject sidecar.Id, role protocol.RepoGrantRole) (*bridge.AuthPin, error) {
	return self.FileStatement(ctx, admin, bridge.SignedType_SIGNED_REPO_GRANT, &protocol.RepoGrant{
		RepoId:  repoId.Bytes(),
		Subject: subject.Bytes(),
		Role:    role,
	})
}

// One sidecar of a world's network: the T3Sidecar, its key client and, once it follows, its follower and EvsClient.
type T3Device struct {
	*T3Sidecar
	Network *T3Network
	// Its key client: FakeGkm's client for its identity, signing by its identity key (the file's keys row).
	Gkm sidecar.GkmClient

	stateLock sync.Mutex
	follower  *sidecar.SidecarConfigFollower
	evs       *sidecar.EvsClient
}

// The token api minted the device, as a bearer: what it presents to api itself.
func (self *T3Device) Bearer() string {
	return "Bearer " + self.Network.world.api.ClientToken(self.Sidecar().Identity().ClientId)
}

// Its config follower, nil before it follows.
func (self *T3Device) Follower() *sidecar.SidecarConfigFollower {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.follower
}

// Trusts genesis and follows the log on the network's config heads with the device's key client, which the follower
// hands each verified state, until it stands.
func (self *T3Device) follow(ctx context.Context, genesis [sha256.Size]byte) error {
	running := self.Sidecar()
	if err := running.TrustFirstUse(genesis[:]); err != nil {
		return fmt.Errorf("%s's anchor: %w", self.Name, err)
	}
	follower, err := running.FollowConfig(&sidecar.SidecarConfigFollowerArgs{Heads: self.Network.heads, Gkm: self.Gkm})
	if err != nil {
		return fmt.Errorf("%s's follower: %w", self.Name, err)
	}
	ctx, cancel := context.WithTimeout(ctx, self.Network.world.settings.SyncTimeout)
	defer cancel()
	if _, err := follower.WaitStanding(ctx); err != nil {
		follower.Close()
		return fmt.Errorf("%s's follower did not stand: %w", self.Name, err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.follower = follower
	return nil
}

// The device's EvsClient, made at the first call as a host makes it (Sidecar.EvsClientArgs): SidecarApi's transport,
// so every request reaches the evs the world serves at its origin, the device's service tokens from server's
// exchange, its identity key, its key client, its follower's verified state, and its statements filed as
// FileStatement files them. Its follower must stand.
func (self *T3Device) EvsClient() (*sidecar.EvsClient, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.evs != nil {
		return self.evs, nil
	}
	follower := self.follower
	if follower == nil {
		return nil, fmt.Errorf("%s reads evs by a follower that stands", self.Name)
	}
	config := func() sidecar.GkmAuthView {
		// never a typed nil: no state verifies before the first
		if state := follower.VerifiedState(); state != nil {
			return state
		}
		return nil
	}
	fileStatement := func(ctx context.Context, signedType bridge.SignedType, body proto.Message) error {
		_, err := self.Network.FileStatement(ctx, self, signedType, body)
		return err
	}
	args, err := self.Sidecar().EvsClientArgs(self.Gkm, config, fileStatement)
	if err != nil {
		return nil, err
	}
	client, err := sidecar.NewEvsClient(args)
	if err != nil {
		return nil, err
	}
	self.evs = client
	return client, nil
}

// Ends the device's EvsClient and follower, then the sidecar.
func (self *T3Device) close() {
	var evs *sidecar.EvsClient
	var follower *sidecar.SidecarConfigFollower
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		evs, follower = self.evs, self.follower
		self.evs, self.follower = nil, nil
	}()
	if evs != nil {
		evs.Close()
	}
	if follower != nil {
		follower.Close()
	}
	self.T3Sidecar.Close()
}

// GET and POST /network/config/projection with one device's token, as SidecarApi sends them
// (sidecar.ConfigProjectionReader and ConfigBasePublisher), decoding what server's controller answers.
type t3NetworkRoutes struct {
	api    *T3Api
	bearer string
}

func (self *t3NetworkRoutes) ReadConfigProjection(ctx context.Context, after uint64) (*sidecar.ConfigProjection, error) {
	path := "/network/config/projection?" + url.Values{"after": []string{strconv.FormatUint(after, 10)}}.Encode()
	status, answer, err := self.api.Call(ctx, http.MethodGet, path, self.bearer, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET /network/config/projection: %d", status)
	}
	result := &controller.NetworkConfigProjectionResult{}
	if err := json.Unmarshal(answer, result); err != nil {
		return nil, err
	}
	projection := &sidecar.ConfigProjection{SignedGenesis: result.Genesis, Entries: result.Entries, Rows: result.Rows, More: result.More}
	if result.Tip != nil {
		projection.Tip = &bridge.AuthPin{Version: result.Tip.N, Chain: result.Tip.Chain}
	}
	return projection, nil
}

func (self *t3NetworkRoutes) PublishConfigBase(ctx context.Context, base *sidecar.ConfigBasePublish) (*bridge.AuthPin, error) {
	status, answer, err := self.api.Call(ctx, http.MethodPost, "/network/config/projection", self.bearer, &controller.NetworkConfigProjectionPublishArgs{
		SignedHeader: base.SignedHeader,
		Rows:         base.Rows,
		Genesis:      base.Genesis,
		ConfigPushes: base.ConfigPushes,
	})
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusConflict:
		return nil, fmt.Errorf("%w: %s", sidecar.ErrConfigTipMoved, answer)
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s", sidecar.ErrConfigRefused, answer)
	default:
		return nil, fmt.Errorf("POST /network/config/projection: %d %s", status, answer)
	}
	result := &controller.NetworkConfigPinResult{}
	if err := json.Unmarshal(answer, result); err != nil {
		return nil, err
	}
	return &bridge.AuthPin{Version: result.N, Chain: result.Chain}, nil
}
