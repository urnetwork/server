// T3 for the sockets engine on server (SOCK-M5-17; R-SOCK-6, D-SOCK-7, D-SOCK-10, D-SOCK-15, I-12): SOCK-M5-16's
// cross-component rows on server's host with real Redis (Q-TEST-1). The host is built as a block builds it
// (newSocketsHost), on model.NewSocketStore over the private test Redis that test-env.sh attests, with the production
// gate reading a network's real authorization log (the admission world on a Net, through the direct source standing
// for api's internal route), and the Net's fake clock, authenticator, Quota, Push and gates. On it:
//
//   - §22.5's hot-path gate: Find's whole authenticated request with every PostgreSQL acquisition in the process
//     refused, and every Redis call the store makes counted (sidecart3.InlinePath on budgets.yml's sockets.find): one
//     script, the group's;
//   - §22.4's slot rule over every script the composed engine runs, registration, listing, Find, sweep and removal:
//     each script's keys under one hash tag (the runtime half on three masters waits on T3's cluster, TEST-M1-08);
//   - D-SOCK-3's 3,000 names in three pages on real Redis, and the listing that opens them all.
//
// FS-41's and FS-42's socket step is sockets_redis_loss_test.go's, and the load harness's scaled run and the Find
// script's benchmark sockets_bench_test.go's. Nothing sleeps: the engine's first probe, the gate's first over-limit read
// and its loads are awaited by count, and time moves on the Net's clock.

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/sidecart3"
)

// A device of the network as a sockets caller.
type t3Sidecar struct {
	device   *sidecartest.AdmissionDevice
	clientId connect.Id
	token    string
}

// The scripts the store ran, by name, each checked for one hash tag as it ran.
type t3Scripts struct {
	t testing.TB

	stateLock sync.Mutex
	counts    map[string]int
}

func (self *t3Scripts) observe(name string, keys []string) {
	if !sidecar.RedisKeysShareTag(keys) {
		self.t.Errorf("the %s script names keys under more than one tag: %v", name, keys)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.counts[name] += 1
}

func (self *t3Scripts) seen() map[string]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	counts := map[string]int{}
	for name, count := range self.counts {
		counts[name] = count
	}
	return counts
}

// One sockets world on server's host and the test Redis.
type t3World struct {
	t         testing.TB
	ctx       context.Context
	net       *sidecartest.Net
	admission *sidecartest.AdmissionNet
	world     *hostTestWorld
	store     *model.SocketStore
	scripts   *t3Scripts
	host      *socketsHost
	networkId connect.Id
	keys      *sidecar.SocketKeys
	listener  *t3Sidecar
	dialer    *t3Sidecar
}

// A world whose network has a member with two devices, the listener and the dialer, K_sock in its head and a policy
// giving members listen and dial on app/*, the host serving it once its network has loaded.
func newT3World(t testing.TB) *t3World {
	t.Helper()
	ctx := t.Context()
	net := sidecartest.NewNet(ctx, nil)
	t.Cleanup(net.Close)
	tier, err := net.AdmissionTier()
	if err != nil {
		t.Fatal(err)
	}
	tier.AclCompile = sidecartest.AclAdmissionCompile
	admission := sidecartest.NewAdmissionNet(t, tier)
	t.Cleanup(admission.Close)
	memberId, listenerDevice := admission.ConsentedMember("m")
	dialerDevice := admission.Device("m second device", memberId)
	admission.AdmitDevice(admission.Owner(), dialerDevice, uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_INVITED))
	socketKey := bytes.Repeat([]byte{0x5e}, 32)
	keyFileBytes, err := proto.Marshal(&protocol.SocketKeyFile{CurrentKey: socketKey})
	if err != nil {
		t.Fatal(err)
	}
	admission.SaveHead(func(head *sidecar.ConfigHead) (*sidecar.ConfigHead, error) {
		return head.WithFile(sidecar.ConfigHeadSocketKeyPath, keyFileBytes)
	})
	admission.SaveAclPolicy([]byte(`{"grants": [{ "src": ["autogroup:member"], "dst": ["sock:app/*"], "allow": ["listen", "dial"] }]}`), sidecartest.AclSaveMembers{})
	networkId := connect.Id(admission.NetworkId())
	keys, err := sidecar.NewSocketKeys(admission.NetworkId(), socketKey)
	if err != nil {
		t.Fatal(err)
	}

	clock := net.Clock()
	faults := sidecartest.NewFaultQueue()
	gates := sidecartest.NewFakeServiceGates(nil)
	if err := gates.Set("sidecar:\n  sockets: {enabled: true, percent: 100}\n  urnetwork_edge: " + connect.NewId().String() + "\n"); err != nil {
		t.Fatal(err)
	}
	world := &hostTestWorld{
		clock:         clock,
		faults:        faults,
		quota:         sidecartest.NewFakeQuota(clock, faults, nil),
		authenticator: sidecartest.NewFakeAuthenticator(clock, faults),
		gates:         gates,
		log:           &hostTestLog{},
	}
	scripts := &t3Scripts{t: t, counts: map[string]int{}}
	store := model.NewSocketStore(clock, nil)
	store.Testing_ObserveScripts(scripts.observe)
	settings := defaultHostSettings()
	settings.Sockets.Gate.Cache.PollJitter = 0
	settings.Sockets.Gate.Cache.FailureBackoff = 0
	settings.Sockets.Gate.BehindReloadInterval = time.Hour
	host, err := newSocketsHost(ctx, settings, &hostDeps{
		Quota:         world.quota,
		Source:        sidecar.NewAclGateDirectSource(nil, net.Host().Blobs, nil),
		KeyFile:       hostTestKeyFile(t, clock.Now()),
		Authenticator: world.authenticator,
		Gates:         gates,
		Store:         store,
		Push:          net.Fakes().Push,
		Clock:         clock,
		Log:           world.log,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	self := &t3World{
		t:         t,
		ctx:       ctx,
		net:       net,
		admission: admission,
		world:     world,
		store:     store,
		scripts:   scripts,
		host:      host,
		networkId: networkId,
		keys:      keys,
	}
	self.listener = self.sidecarOf(listenerDevice)
	self.dialer = self.sidecarOf(dialerDevice)
	self.await("the engine's first probe", host.engine.Probes)
	self.await("the gate's first over-limit read", host.gate.OverLimitReads)
	for {
		_, changed := host.gate.Loads()
		if _, err := host.gate.View(networkId); err == nil {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("the network never loaded")
		}
	}
	return self
}

// Waits until counts is past zero.
func (self *t3World) await(what string, counts func() (int, chan struct{})) {
	self.t.Helper()
	for {
		count, changed := counts()
		if 0 < count {
			return
		}
		select {
		case <-changed:
		case <-self.ctx.Done():
			self.t.Fatalf("%s never came", what)
		}
	}
}

// A device's sockets token, as plan-SOCK-client-auth's Client-class token names its sidecar.
func (self *t3World) sidecarOf(device *sidecartest.AdmissionDevice) *t3Sidecar {
	sidecarId := connect.Id(device.SidecarId)
	clientId := connect.NewId()
	now := self.world.clock.Now()
	return &t3Sidecar{
		device:   device,
		clientId: clientId,
		token: self.world.authenticator.Issue(&sidecar.Principal{
			NetworkId: self.networkId,
			ClientId:  clientId,
			SidecarId: &sidecarId,
			Audience:  socketsservice.SocketsAudience,
			IssuedAt:  now,
			ExpiresAt: now.Add(48 * time.Hour),
		}),
	}
}

// A blinded registration of name by caller, its token path, seal and claim as the client side makes them.
func (self *t3World) registration(caller *t3Sidecar, name string) *sidecar.SocketRegistration {
	self.t.Helper()
	nameToken, err := self.keys.TokenPath(name)
	if err != nil {
		self.t.Fatal(err)
	}
	sealedName, err := self.keys.Seal(name)
	if err != nil {
		self.t.Fatal(err)
	}
	listenId := connect.NewId()
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(&protocol.SocketRegistrationClaim{
		NetworkId: self.networkId.Bytes(),
		NameToken: nameToken,
		ListenId:  listenId.Bytes(),
		Protocol:  "tcp",
	})
	if err != nil {
		self.t.Fatal(err)
	}
	claim, err := proto.MarshalOptions{Deterministic: true}.Marshal(&bridge.Signed{
		Body:      body,
		Signature: ed25519.Sign(caller.device.Key, bridge.SignedBytes(sidecar.SocketRegistrationPurpose, bridge.SignedType_SIGNED_SOCKET_REGISTRATION, body)),
		Type:      uint32(bridge.SignedType_SIGNED_SOCKET_REGISTRATION),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	return &sidecar.SocketRegistration{
		NameToken:  nameToken,
		SealedName: sealedName,
		ListenId:   &listenId,
		Protocol:   "tcp",
		Weight:     100,
		MaxStreams: 256,
		Serving:    true,
		Claim:      claim,
	}
}

// One page of caller's set, sorted by name key.
func (self *t3World) register(caller *t3Sidecar, setVersion uint64, keyStart []byte, keyEnd []byte, registrations []*sidecar.SocketRegistration) *sidecar.SocketRegistrationsResult {
	self.t.Helper()
	sort.Slice(registrations, func(i int, j int) bool {
		return bytes.Compare(registrations[i].NameKey(), registrations[j].NameKey()) < 0
	})
	recorder := hostTestServe(self.host, http.MethodPost, "/socket/registrations", caller.token, &sidecar.SocketRegistrationsArgs{
		SetVersion: setVersion, KeyStart: keyStart, KeyEnd: keyEnd, Registrations: registrations,
	}, "")
	result := &sidecar.SocketRegistrationsResult{}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), result) != nil {
		self.t.Fatalf("a page answered %d", recorder.Code)
	}
	return result
}

// A whole-set page every entry of which must answer ok.
func (self *t3World) registerOk(caller *t3Sidecar, setVersion uint64, registrations ...*sidecar.SocketRegistration) {
	self.t.Helper()
	result := self.register(caller, setVersion, nil, nil, registrations)
	if result.Error != nil {
		self.t.Fatalf("a page refused: %+v", result.Error)
	}
	for index, status := range result.Statuses {
		if status != sidecar.SocketRegistrationStatusOk {
			self.t.Fatalf("entry %d answered %s", index, status)
		}
	}
}

// One Find of name by caller through the host: the refusal's code, or "" once its primary ticket verifies.
func (self *t3World) find(caller *t3Sidecar, name string) sidecar.SocketErrorCode {
	self.t.Helper()
	nameToken, err := self.keys.TokenPath(name)
	if err != nil {
		self.t.Fatal(err)
	}
	networkId := self.networkId
	recorder := hostTestServe(self.host, http.MethodPost, "/socket/find", caller.token, &sidecar.SocketFindArgs{NameToken: nameToken, NetworkId: &networkId}, "")
	result := &sidecar.SocketFindResult{}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), result) != nil {
		self.t.Fatalf("a Find answered %d", recorder.Code)
	}
	if result.Error != nil {
		return result.Error.Code
	}
	verifyKeys := &sidecar.SocketKeysResult{}
	if keysRecorder := hostTestServe(self.host, http.MethodGet, "/socket/keys", "", nil, ""); json.Unmarshal(keysRecorder.Body.Bytes(), verifyKeys) != nil {
		self.t.Fatal("GET /socket/keys answered no keys")
	}
	if _, err := sidecar.VerifySocketTicket(result.Primary.Ticket, verifyKeys.Keys, uint64(self.world.clock.Now().UnixMilli())); err != nil {
		self.t.Fatalf("a Find's ticket: %v", err)
	}
	return ""
}

// The names one listing page opens under the world's K_sock, and the page's next cursor.
func t3ListedPage(t testing.TB, world *t3World, body []byte) (map[string]bool, string) {
	t.Helper()
	result := &sidecar.SocketListResult{}
	if err := json.Unmarshal(body, result); err != nil || result.Error != nil {
		t.Fatalf("a listing page: %v, %+v", err, result.Error)
	}
	listed := map[string]bool{}
	for _, name := range result.Names {
		if opened, ok := sidecar.OpenSocketName(world.keys, nil, name.NameToken, name.SealedName); ok {
			listed[opened] = true
		}
	}
	return listed, result.NextCursor
}

// The names one listing page opens.
func t3ListedNames(t testing.TB, world *t3World, body []byte) map[string]bool {
	t.Helper()
	listed, _ := t3ListedPage(t, world, body)
	return listed
}

// §22.5's hot-path gate (budgets.yml's sockets.find): a Find whose listener passes, its whole authenticated request
// through the host, reaches no PostgreSQL and makes one Redis call, the group's pick script, on the test Redis.
func TestT3FindIsOneScriptWithPostgresRefused(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3World(t)
		world.registerOk(world.listener, 1, world.registration(world.listener, "app/db"))
		if code := world.find(world.dialer, "app/db"); code != "" {
			t.Fatalf("the first Find answered %s", code)
		}
		nameToken, _ := world.keys.TokenPath("app/db")
		networkId := world.networkId
		var code int
		var answer []byte
		result := sidecart3.InlinePath(t, "sockets.find", sidecarhost.NewRedis(nil), func(counted sidecar.Redis) {
			world.store.Testing_RunScriptsThrough(counted)
			defer world.store.Testing_RunScriptsThrough(nil)
			recorder := hostTestServe(world.host, http.MethodPost, "/socket/find", world.dialer.token, &sidecar.SocketFindArgs{NameToken: nameToken, NetworkId: &networkId}, "")
			code = recorder.Code
			answer = recorder.Body.Bytes()
		})
		findResult := &sidecar.SocketFindResult{}
		if code != http.StatusOK || json.Unmarshal(answer, findResult) != nil || findResult.Error != nil || findResult.Primary == nil {
			t.Fatalf("the counted Find answered %d, %s", code, answer)
		}
		if result.RedisCallCount != 1 || result.RedisCalls["script:sockets.pick"] != 1 {
			t.Fatalf("the counted Find made %v", result.RedisCalls)
		}
	})
}

// §22.4's slot rule over the composed engine: every script registration, listing, Find, a revocation's sweep and a
// client's removal run names its keys under one tag, and each of those paths ran.
func TestT3EveryScriptOfTheEngineKeepsToOneSlot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3World(t)
		names := []string{"app/db", "app/cache", "app/queue"}
		registrations := []*sidecar.SocketRegistration{}
		for _, name := range names {
			registrations = append(registrations, world.registration(world.listener, name))
		}
		world.registerOk(world.listener, 1, registrations...)
		if recorder := hostTestServe(world.host, http.MethodGet, "/socket/list", world.listener.token, nil, ""); recorder.Code != http.StatusOK {
			t.Fatalf("the listing answered %d", recorder.Code)
		}
		for _, name := range names {
			if code := world.find(world.dialer, name); code != "" {
				t.Fatalf("a Find answered %s", code)
			}
		}
		// a shorter page unlistens, a revocation sweeps, and a removed client's store work runs
		world.registerOk(world.listener, 2, registrations[1:]...)
		sweeps, _ := world.host.engine.Sweeps()
		loads, _ := world.host.gate.Loads()
		world.admission.MustLand(world.admission.Sign(world.admission.Owner(), bridge.SignedType_SIGNED_SIDECAR_REVOKE,
			world.admission.Revocation(world.listener.device, bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_LOST, 0)))
		world.host.engine.ProjectionChanged(world.networkId)
		for {
			count, changed := world.host.gate.Loads()
			if loads < count {
				break
			}
			select {
			case <-changed:
			case <-world.ctx.Done():
				t.Fatal("the revocation never loaded")
			}
		}
		for {
			count, changed := world.host.engine.Sweeps()
			if sweeps < count {
				break
			}
			select {
			case <-changed:
			case <-world.ctx.Done():
				t.Fatal("the sweep never ran")
			}
		}
		removals, _ := world.host.engine.Removals()
		world.host.engine.ClientRemoved(world.dialer.clientId)
		for {
			count, changed := world.host.engine.Removals()
			if removals < count {
				break
			}
			select {
			case <-changed:
			case <-world.ctx.Done():
				t.Fatal("the removal never ran")
			}
		}
		seen := world.scripts.seen()
		for _, name := range []string{"group", "pick", "set_begin", "set_commit", "names", "read", "claim"} {
			if seen[name] == 0 {
				t.Errorf("the composed run never ran the %s script, so the slot check never read it (%v)", name, seen)
			}
		}
	})
}

// D-SOCK-3 on real Redis: 3,000 names as three pages of one set_version, none refused for size, and the listing then
// opens all 3,000; a shorter middle page unlistens only its own range.
func TestT3ThreeThousandNamesInThreePages(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3World(t)
		// the bucket charges a page a token, and three pages and some listings fit its burst
		registrations := []*sidecar.SocketRegistration{}
		for index := range 3000 {
			registrations = append(registrations, world.registration(world.listener, fmt.Sprintf("app/n%04d", index)))
		}
		sort.Slice(registrations, func(i int, j int) bool {
			return bytes.Compare(registrations[i].NameKey(), registrations[j].NameKey()) < 0
		})
		pages := [][]*sidecar.SocketRegistration{registrations[:1024], registrations[1024:2048], registrations[2048:]}
		bounds := [][]byte{nil, registrations[1024].NameKey(), registrations[2048].NameKey(), nil}
		for index, page := range pages {
			result := world.register(world.listener, 1, bounds[index], bounds[index+1], page)
			if result.Error != nil || len(result.Statuses) != len(page) {
				t.Fatalf("page %d: %+v", index, result.Error)
			}
		}
		listed := map[string]bool{}
		cursor := ""
		for range 32 {
			path := "/socket/list"
			if cursor != "" {
				path += "?cursor=" + url.QueryEscape(cursor)
			}
			recorder := hostTestServe(world.host, http.MethodGet, path, world.listener.token, nil, "")
			if recorder.Code != http.StatusOK {
				t.Fatalf("a listing page answered %d", recorder.Code)
			}
			page, next := t3ListedPage(t, world, recorder.Body.Bytes())
			for name := range page {
				listed[name] = true
			}
			if cursor = next; cursor == "" {
				break
			}
		}
		if len(listed) != 3000 {
			t.Fatalf("the listing opens %d names", len(listed))
		}
		dropped := pages[1][0]
		if result := world.register(world.listener, 2, bounds[1], bounds[2], pages[1][1:]); result.Error != nil {
			t.Fatalf("the shorter page: %+v", result.Error)
		}
		if listeners, err := world.store.ListListeners(world.ctx, world.networkId, dropped.NameToken); err != nil || len(listeners) != 0 {
			t.Fatalf("a name the page lacked stands: %d, %v", len(listeners), err)
		}
		if listeners, err := world.store.ListListeners(world.ctx, world.networkId, pages[0][0].NameToken); err != nil || len(listeners) != 1 {
			t.Fatalf("a name outside the page's range moved: %d, %v", len(listeners), err)
		}
	})
}
