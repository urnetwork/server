//go:build sidecar_pending

package api

// GET /sidecar/scim-changes end to end against api on the private test stack (QD-829): server's router over api's
// route table in process, on Postgres with MG6, and server's own sequencer, installed as api's, over T2's fake first
// cluster. Real Sidecars, NewSidecar's own, enroll through POST /network/enroll and POST /sidecar/bind on that router
// with auth keys the network's owner makes there, and are admitted in the network's log by their real identity keys
// from an admission world whose appends go through the same sequencer. The indexer ServiceSettings.scim_signer names
// reads SCIM's queue through its own api and client token, and its ConfigScimSigner signs from it: its MemberRevoke of
// the member outside the AdminSet goes through POST /network/config/delta to the sequencer, which takes it on the
// scim_signer rule, and the member leaves the queue the route answers; the AdminSet member's removal is never listed.
// Another sidecar's token and the network's credential are refused 403.
//
// What this stands in: no exchange runs here, so each Sidecar's generation runs Offline on a client that never connects,
// and the daemon's composition, its follower and scim_signer component, is cli's test (daemon_config_scim_test.go);
// here a ConfigScimSigner reads the source the component installs (Sidecar.NewScimChangeSource), over the Sidecar's
// ConfigClient, which reads the log from the sequencer in process with the world's config heads, as a daemon's
// follower would through the route.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// The admission world's api over server's sequencer: its appends and read, a ConfigLogError as the world's refusal.
type scimRouteAdmissionApi struct {
	sequencer *model.ConfigLogSequencer
}

func scimRouteAdmissionError(err error) error {
	configLogError := &model.ConfigLogError{}
	if errors.As(err, &configLogError) {
		return &sidecartest.ConfigLogRefusal{Status: configLogError.Status, Tip: configLogError.Tip, Err: configLogError.Err}
	}
	return err
}

func (self *scimRouteAdmissionApi) AppendStatement(ctx context.Context, networkId sidecar.Id, signed []byte) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendStatement(ctx, server.Id(networkId), signed)
	return pin, scimRouteAdmissionError(err)
}

func (self *scimRouteAdmissionApi) AppendBase(ctx context.Context, networkId sidecar.Id, signedHeader []byte, rows []byte, genesis []byte, configPushes [][]byte) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendBase(ctx, server.Id(networkId), &model.ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
	return pin, scimRouteAdmissionError(err)
}

func (self *scimRouteAdmissionApi) AppendTick(ctx context.Context, networkId sidecar.Id, tick *bridge.ApiTick) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendTick(ctx, server.Id(networkId), tick)
	return pin, scimRouteAdmissionError(err)
}

func (self *scimRouteAdmissionApi) ReadConfigProjection(ctx context.Context, networkId sidecar.Id, after uint64) (*sidecar.ConfigProjection, error) {
	read, err := self.sequencer.Read(ctx, server.Id(networkId), after)
	if err != nil {
		return nil, scimRouteAdmissionError(err)
	}
	return &sidecar.ConfigProjection{SignedGenesis: read.SignedGenesis, Entries: read.Entries, Rows: read.Rows, Tip: read.Tip, More: read.More}, nil
}

func (self *scimRouteAdmissionApi) SequenceAnyway(ctx context.Context, networkId sidecar.Id, kind uint32, body []byte) (*bridge.AuthPin, error) {
	return nil, errors.New("server's sequencer appends nothing its check refuses")
}

// The network's log as one network's ConfigClient reads it: the sequencer's, in process.
type scimRouteProjection struct {
	api       *scimRouteAdmissionApi
	networkId sidecar.Id
}

func (self *scimRouteProjection) ReadConfigProjection(ctx context.Context, after uint64) (*sidecar.ConfigProjection, error) {
	return self.api.ReadConfigProjection(ctx, self.networkId, after)
}

// Every request a Sidecar sends, served by api's router in process from a documentation address, each route counted.
type scimRouteTransport struct {
	handler   http.Handler
	stateLock sync.Mutex
	counts    map[string]int
}

func (self *scimRouteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		var err error
		body, err = io.ReadAll(request.Body)
		request.Body.Close()
		if err != nil {
			return nil, err
		}
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.counts[request.Method+" "+request.URL.Path] += 1
	}()
	served := httptest.NewRequestWithContext(request.Context(), request.Method, request.URL.RequestURI(), bytes.NewReader(body))
	served.Header = request.Header.Clone()
	served.RemoteAddr = "198.51.100.74:4000"
	recorder := httptest.NewRecorder()
	self.handler.ServeHTTP(recorder, served)
	response := recorder.Result()
	response.Request = request
	return response, nil
}

func (self *scimRouteTransport) count(route string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.counts[route]
}

// The api a Sidecar reaches, and no exchange: a generation's client opens and never connects, so the generation runs,
// its token installed in the Sidecar's api, Offline.
type scimRoutePlatform struct {
	transport *scimRouteTransport
}

func (self *scimRoutePlatform) ApiTransport() http.RoundTripper {
	return self.transport
}

func (self *scimRoutePlatform) OpenClient(ctx context.Context, args *sidecar.SidecarPlatformClientArgs) (*sidecar.SidecarPlatformClient, error) {
	client := connect.NewClient(ctx, args.ClientId, connect.NewNoContractClientOob(), args.ClientSettings)
	never := make(chan struct{})
	return &sidecar.SidecarPlatformClient{
		Client:  client,
		SetAuth: func(auth *connect.ClientAuth) {},
		Connected: func() (bool, <-chan struct{}) {
			return false, never
		},
		CloseAndWait: func(ctx context.Context) error {
			client.Cancel()
			return client.CloseAndWait(ctx)
		},
	}, nil
}

// A Sidecar's host: its instance's store, a memory key store, no token source, and no credential store of this
// machine's, so the build machine's own app state never decides a run.
type scimRouteHost struct {
	store    sidecar.SidecarStore
	keyStore sidecar.SidecarKeyStore
}

func (self *scimRouteHost) Store() sidecar.SidecarStore {
	return self.store
}

func (self *scimRouteHost) KeyStore() sidecar.SidecarKeyStore {
	return self.keyStore
}

func (self *scimRouteHost) TokenSource() sidecar.SidecarTokenSource {
	return nil
}

func (self *scimRouteHost) HostCredentialStores() []sidecar.SidecarHostCredentialStore {
	return nil
}

// The world: the network on server's rows and the sequencer, its owner's credential, the router, and the admission
// world of its log.
type scimRouteWorld struct {
	t         testing.TB
	ctx       context.Context
	clock     *sidecartest.FakeClock
	api       *scimRouteAdmissionApi
	net       *sidecartest.AdmissionNet
	networkId server.Id
	root      string
	transport *scimRouteTransport
}

func newScimRouteWorld(t testing.TB) *scimRouteWorld {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// api's time and every statement's, the Sidecars' included, read one clock near server's own, since the router
	// judges what the Sidecars send on server's
	clock := sidecartest.NewFakeClock(server.NowUtc().Truncate(time.Millisecond))
	self := &scimRouteWorld{t: t, ctx: ctx, clock: clock, api: &scimRouteAdmissionApi{}}
	blobs := sidecartest.NewFakeBlobs(clock, sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	suiteSettings := sidecartest.DefaultAdmissionSuiteSettings()
	suiteSettings.ForgedAppends = false
	self.net = sidecartest.NewAdmissionNet(t, &sidecartest.AdmissionTier{
		Cluster:  cluster,
		Clock:    clock,
		Settings: suiteSettings,
		NewApi: func(clock sidecar.Clock, cluster sidecar.BlobCluster) sidecartest.AdmissionApi {
			settings := model.DefaultConfigLogSettings()
			settings.Clock = clock
			self.api.sequencer = model.NewConfigLogSequencer(cluster, settings, nil)
			model.SetConfigLogSequencer(self.api.sequencer)
			return self.api
		},
	})
	t.Cleanup(self.net.Close)
	t.Cleanup(func() { model.SetConfigLogSequencer(nil) })
	self.networkId = server.Id(self.net.NetworkId())
	userId := server.NewId()
	networkName := fmt.Sprintf("scim%s", strings.ReplaceAll(self.networkId.String(), "-", "")[:12])
	model.Testing_CreateNetwork(ctx, self.networkId, networkName, userId)
	t.Cleanup(server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", self.networkId)))
	self.root = "Bearer " + session.NewByJwt(self.networkId, userId, networkName, false, false).Testing_Sign()
	self.transport = &scimRouteTransport{handler: router.NewRouter(ctx, Routes()), counts: map[string]int{}}
	return self
}

// One request through the router.
func (self *scimRouteWorld) serve(method string, path string, authorization string, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.RemoteAddr = "198.51.100.74:4000"
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	self.transport.handler.ServeHTTP(recorder, request)
	return recorder
}

// A Sidecar enrolled through the router from an auth key the owner makes there, on a memory instance of its own,
// running until the test ends.
func (self *scimRouteWorld) enroll(name string) *sidecar.Sidecar {
	self.t.Helper()
	made := self.serve(http.MethodPost, "/network/auth-keys", self.root, `{"duration_seconds":600,"max_uses":1}`)
	authKey := &model.CreateAuthKeyResult{}
	if made.Code != http.StatusOK || json.Unmarshal(made.Body.Bytes(), authKey) != nil || authKey.AuthKey == "" {
		self.t.Fatalf("an auth key for %s: %d %s", name, made.Code, made.Body.String())
	}
	settings := sidecar.DefaultSidecarSettings()
	settings.ApiUrl = "https://api.scim-route.example"
	settings.AuthUrl = "https://api.scim-route.example"
	settings.PlatformUrl = "wss://connect.scim-route.example"
	settings.EvsUrl = "https://evs.scim-route.example"
	settings.GkmUrl = "https://gkm.scim-route.example"
	settings.SocketsUrl = "https://sockets.scim-route.example"
	settings.DnsUrl = "https://dns.scim-route.example"
	settings.Clock = self.clock
	settings.Log = sidecar.NewNoopLogger()
	settings.KeychainSettings.AutoLockTimeout = 0
	settings.Connect.Platform = &scimRoutePlatform{transport: self.transport}
	runCtx, cancel := context.WithCancel(self.ctx)
	keyStore := sidecar.NewSidecarMemoryKeyStore()
	instance, err := sidecar.NewSidecarInstance(runCtx, &sidecar.SidecarInstanceArgs{
		Backend:  sidecar.NewSidecarMemoryBackend(),
		KeyStore: keyStore,
		Settings: settings,
	})
	if err != nil {
		cancel()
		self.t.Fatal(err)
	}
	if err := instance.Unlock(nil); err != nil {
		cancel()
		self.t.Fatal(err)
	}
	running, err := sidecar.NewSidecar(runCtx, &scimRouteHost{store: instance.Store(nil), keyStore: keyStore}, nil, settings)
	if err != nil {
		cancel()
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer closeCancel()
		running.CloseAndWait(closeCtx)
		instance.Close()
		cancel()
	})
	self.await(running, "Unenrolled", func() bool { return running.Status().State == sidecar.SidecarStateUnenrolled })
	if err := running.Enroll(self.ctx, &sidecar.SidecarEnrollCredential{AuthKey: authKey.AuthKey}); err != nil {
		self.t.Fatalf("%s did not enroll through the router: %v", name, err)
	}
	// the run loop adopts the identity enrollment kept and starts its generation, whose token the Sidecar's api holds
	self.await(running, "its generation", func() bool {
		return running.Identity() != nil && running.Status().State == sidecar.SidecarStateOffline
	})
	return running
}

// Waits until done holds of running, looking again at each of its events, bounded by a real-time guard only a broken
// wiring reaches.
func (self *scimRouteWorld) await(running *sidecar.Sidecar, what string, done func() bool) {
	self.t.Helper()
	changed := make(chan struct{}, 1)
	// subscribed before the look, so a change between them wakes the wait
	remove := running.AddEventCallback(func(event sidecar.SidecarEvent) {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	defer remove()
	guard := time.After(60 * time.Second)
	for !done() {
		select {
		case <-changed:
		case <-guard:
			status := running.Status()
			self.t.Fatalf("the sidecar, %s (%s), never came to %s", status.State, status.Reason, what)
		}
	}
}

// running's ConfigClient on the network's log and the world's config heads, installed as its authority.
func (self *scimRouteWorld) follow(running *sidecar.Sidecar) *sidecar.ConfigClient {
	self.t.Helper()
	settings := sidecar.DefaultConfigClientSettings()
	settings.Log = sidecar.NewNoopLogger()
	client, err := sidecar.NewConfigClient(self.ctx, &sidecar.ConfigClientArgs{
		NetworkId:    self.net.NetworkId(),
		GenesisChain: self.net.GenesisChain(),
		Projection:   &scimRouteProjection{api: self.api, networkId: self.net.NetworkId()},
		Heads:        self.net.Heads(),
		Clock:        self.clock,
		Settings:     settings,
	})
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(client.Close)
	self.t.Cleanup(running.InstallAuthority(client.Authority()))
	return client
}

// The indexer scim_signer names, a real Sidecar enrolled through api, reads the queued removal of the member outside
// the AdminSet through GET /sidecar/scim-changes with its own token, and its ConfigScimSigner signs it: the MemberRevoke
// lands at api on the scim_signer rule, every reader of the log applies it, and the queue the route answers is empty;
// the Admin's queued removal is never listed. Before the designation a pass reads nothing; another sidecar's token and
// the network's credential are refused 403 by the route.
func TestScimSignerSignsFromTheRouteEndToEnd(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		world := newScimRouteWorld(t)
		ctx := world.ctx
		owner := world.net.Owner()
		leaver, leaverDevice := world.net.ConsentedMember("route leaver")
		adminMember := world.net.AdmitMember(owner, "route admin", bridge.MemberRole_MEMBER_ROLE_MEMBER)
		world.net.MustLand(world.net.Sign(owner, bridge.SignedType_SIGNED_ADMIN_SET, world.net.AdminSet([]sidecar.Id{owner.MemberId}, []sidecar.Id{adminMember}, 0)))

		// each the invited first device of a member outside the AdminSet, admitted by its real identity key
		host := func(name string) *sidecar.Sidecar {
			running := world.enroll(name)
			memberId := world.net.AdmitMember(owner, name+" member", bridge.MemberRole_MEMBER_ROLE_MEMBER)
			world.net.AdmitSidecar(owner, running.Identity(), memberId, uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_INVITED))
			return running
		}
		indexer := host("route indexer")
		other := host("route other")
		indexerClient := world.follow(indexer)

		server.Tx(ctx, func(tx server.PgTx) {
			now := server.NowUtc()
			for _, memberId := range []sidecar.Id{adminMember, leaver} {
				queued := server.Id(memberId)
				_, err := model.AddNetworkScimChangeInTx(ctx, tx, world.networkId, &queued, model.NetworkScimChangeKindMemberRevoke, &model.NetworkScimMemberRevokeDetail{}, now)
				server.Raise(err)
			}
		})
		settings := sidecar.DefaultConfigScimSignerSettings()
		// the test drives each pass
		settings.PollInterval = 0
		signer := sidecar.NewConfigScimSigner(ctx, indexer, indexer.NewScimChangeSource(settings), settings)
		defer signer.Close()

		// before any designation: a pass reads no queue
		if err := indexerClient.CatchUp(ctx); err != nil {
			t.Fatal(err)
		}
		pass, err := signer.Pass(ctx)
		if err != nil || pass.Designated || world.transport.count("GET /sidecar/scim-changes") != 0 {
			t.Fatalf("a pass before the designation: %+v %v", pass, err)
		}

		world.net.SetServiceSettings(&protocol.ServiceSettings{ScimSigner: indexer.Identity().SidecarId.Bytes()})
		before := world.net.Tip().GetVersion()

		// what the route answers the indexer, through its own api: the leaver's removal alone
		page, err := indexer.NewScimChangeSource(settings).ScimMemberRevokes(ctx)
		if err != nil || !slices.Equal(page, []sidecar.Id{leaver}) {
			t.Fatalf("the indexer's read: %v %v", page, err)
		}

		if err := indexerClient.CatchUp(ctx); err != nil {
			t.Fatal(err)
		}
		pass, err = signer.Pass(ctx)
		if err != nil || !pass.Designated || !slices.Equal(pass.Signed, []sidecar.Id{leaver}) || len(pass.AdminSet) != 0 || pass.PresenceNeeded {
			t.Fatalf("the indexer's pass: %+v %v", pass, err)
		}
		if world.transport.count("POST /network/config/delta") != 1 {
			t.Fatalf("%d statements posted", world.transport.count("POST /network/config/delta"))
		}
		world.net.Sync()
		state := world.net.State()
		if state.Pin().GetVersion() != before+1 || state.Member(leaver) != nil || state.SidecarRow(leaverDevice.SidecarId).GetRevokedAt() != before+1 ||
			state.Member(adminMember) == nil {
			t.Fatalf("after the indexer's pass the log is at %d: the leaver a member %t", state.Pin().GetVersion(), state.Member(leaver) != nil)
		}

		// signed: the route no longer answers it, while the Admin's removal still waits for k of n
		page, err = indexer.NewScimChangeSource(settings).ScimMemberRevokes(ctx)
		if err != nil || len(page) != 0 {
			t.Fatalf("the indexer's read after its pass: %v %v", page, err)
		}

		// another sidecar's token, and the network's credential
		_, err = other.NewScimChangeSource(settings).ScimMemberRevokes(ctx)
		apiErr := &sidecar.SidecarApiError{}
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
			t.Errorf("another sidecar's read: %v", err)
		}
		if w := world.serve(http.MethodGet, "/sidecar/scim-changes", world.root, ""); w.Code != http.StatusForbidden {
			t.Errorf("the network's credential: %d %s", w.Code, w.Body.String())
		}
	})
}
