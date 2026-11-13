package controller

// §6.10.1's Service contracts for ur/ streams on the private stack (QD-728; C9, D-SOCK-11, D-SOCK-12, I-12). A sockets
// engine on server's Redis store opens ur/ as a sockets process does (RegisterPlatformNamespace), an edge's platform
// client lists ur/smtpedge and a sidecar Finds it, so the pair CreateContract reads is the one SOCK-M9-01's Find
// writes: field <edge listener> of {svc_<sidecar dialer>}. A ur/ stream then gets Service both ways on one read, as a
// tls/ or smtp/ stream still does on its own pair; a ur/ pair lives its service's grant, whatever the edge's
// registrations do, and goes with the sidecar's removal. The engine's clock is a fake one started at the real time
// CreateContract judges a grant by, and no test waits on time: a grant's end is a time passed to the decision.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The platform name the tests' edges list, and whose grant their Finds carry.
const servicePlatformName = "ur/smtpedge"

// The one key the test engine signs tickets with.
type servicePlatformTicketKeys struct {
	privateKey       ed25519.PrivateKey
	expireTimeMillis uint64
}

// See socketsservice.SocketTicketKeys.
func (self *servicePlatformTicketKeys) SigningKey() (string, ed25519.PrivateKey, bool) {
	return "k1", self.privateKey, true
}

// See socketsservice.SocketTicketKeys.
func (self *servicePlatformTicketKeys) VerifyKeys() []*sidecar.SocketTicketKey {
	return []*sidecar.SocketTicketKey{{
		KeyId:            "k1",
		PublicKey:        self.privateKey.Public().(ed25519.PublicKey),
		ExpireTimeMillis: self.expireTimeMillis,
	}}
}

// One sockets process on server's Redis store with ur/ open: an engine on a fake host whose sidecar.yml names nets'
// edge network, behind a fake gate holding the edge's and the sidecar's networks loaded.
type servicePlatformSockets struct {
	t             testing.TB
	nets          *serviceTestNets
	clock         *sidecartest.FakeClock
	authenticator *sidecartest.FakeAuthenticator
	ticketKeys    *servicePlatformTicketKeys
	engine        *socketsservice.SocketsService
	// ur/smtpedge's grant, past each ticket's issue
	grantLifetime time.Duration
	// each client's token, issued once
	clientTokens map[server.Id]string
}

func newServicePlatformSockets(t testing.TB, ctx context.Context, nets *serviceTestNets) *servicePlatformSockets {
	t.Helper()
	clock := sidecartest.NewFakeClock(server.NowUtc())
	host := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	if err := host.Gates.Set("sidecar:\n  sockets: {enabled: true, percent: 100}\n  urnetwork_edge: " + nets.edgeNetworkId.String() + "\n"); err != nil {
		t.Fatal(err)
	}
	edgeNetworkId := connect.Id(nets.edgeNetworkId)
	gate := sidecartest.NewFakeSocketGate(clock, &edgeNetworkId)
	gate.Load(edgeNetworkId)
	gate.Load(connect.Id(nets.sidecarNetworkId))
	settings := socketsservice.DefaultSocketsSettings()
	engine, err := socketsservice.NewSocketsService(ctx, host.ServiceHost(), model.NewSocketStore(clock, nil), gate, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	if err := engine.RegisterPlatformNamespace(); err != nil {
		t.Fatal(err)
	}
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	ticketKeys := &servicePlatformTicketKeys{
		privateKey:       privateKey,
		expireTimeMillis: uint64(clock.Now().Add(24 * time.Hour).UnixMilli()),
	}
	engine.SetTicketKeys(ticketKeys)
	return &servicePlatformSockets{
		t:             t,
		nets:          nets,
		clock:         clock,
		authenticator: host.Authenticator,
		ticketKeys:    ticketKeys,
		engine:        engine,
		grantLifetime: settings.Platform.Services["smtpedge"].GrantLifetime,
		clientTokens:  map[server.Id]string{},
	}
}

// clientId's token for the engine's audience: an edge's platform client's in the edge network, a sidecar's naming a
// sidecar id in the sidecar network.
func (self *servicePlatformSockets) token(networkId server.Id, clientId server.Id) string {
	if token, found := self.clientTokens[clientId]; found {
		return token
	}
	now := self.clock.Now()
	principal := &sidecar.Principal{
		NetworkId: connect.Id(networkId),
		ClientId:  connect.Id(clientId),
		Audience:  socketsservice.SocketsAudience,
		IssuedAt:  now,
		ExpiresAt: now.Add(24 * time.Hour),
	}
	if networkId != self.nets.edgeNetworkId {
		sidecarId := connect.NewId()
		principal.SidecarId = &sidecarId
	}
	token := self.authenticator.Issue(principal)
	self.clientTokens[clientId] = token
	return token
}

// One POST to the engine's handler as token's holder: the status, with the body decoded into result when it is 200.
func (self *servicePlatformSockets) post(path string, token string, body any, result any) int {
	self.t.Helper()
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		self.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(bodyBytes))
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	self.engine.Handler().ServeHTTP(recorder, request)
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), result); err != nil {
			self.t.Fatal(err)
		}
	}
	return recorder.Code
}

// The edge's whole set as page setVersion: ur/smtpedge in the clear with no claim, as a platform client lists it, or
// nothing, which unlists it.
func (self *servicePlatformSockets) listEdge(edgeClientId server.Id, setVersion uint64, listen bool) {
	self.t.Helper()
	registrations := []*sidecar.SocketRegistration{}
	if listen {
		listenId := connect.NewId()
		registrations = append(registrations, &sidecar.SocketRegistration{
			Name:       servicePlatformName,
			ListenId:   &listenId,
			Protocol:   "tcp",
			Weight:     100,
			MaxStreams: 256,
			Serving:    true,
		})
	}
	result := &sidecar.SocketRegistrationsResult{}
	code := self.post("/socket/registrations", self.token(self.nets.edgeNetworkId, edgeClientId), &sidecar.SocketRegistrationsArgs{
		SetVersion:    setVersion,
		Registrations: registrations,
	}, result)
	if code != http.StatusOK || result.Error != nil || len(result.Statuses) != len(registrations) {
		self.t.Fatalf("the edge's page answered %d, %+v", code, result)
	}
	for index, status := range result.Statuses {
		if status != sidecar.SocketRegistrationStatusOk {
			self.t.Fatalf("the edge's entry %d answered %s", index, status)
		}
	}
}

// The sidecar client's Find of ur/smtpedge in the edge network: the status and the answer.
func (self *servicePlatformSockets) findAnswer(clientId server.Id) (int, *sidecar.SocketFindResult) {
	self.t.Helper()
	edgeNetworkId := connect.Id(self.nets.edgeNetworkId)
	result := &sidecar.SocketFindResult{}
	code := self.post("/socket/find", self.token(self.nets.sidecarNetworkId, clientId), &sidecar.SocketFindArgs{
		Name:      servicePlatformName,
		NetworkId: &edgeNetworkId,
	}, result)
	return code, result
}

// The sidecar client's Find of ur/smtpedge, whose primary ticket must verify and name edgeClientId as its listener and
// clientId as its dialer: the ticket's grant expiry.
func (self *servicePlatformSockets) find(clientId server.Id, edgeClientId server.Id) time.Time {
	self.t.Helper()
	code, result := self.findAnswer(clientId)
	if code != http.StatusOK || result.Error != nil || result.Primary == nil {
		self.t.Fatalf("the Find answered %d, %+v", code, result.Error)
	}
	ticket, err := sidecar.VerifySocketTicket(result.Primary.Ticket, self.ticketKeys.VerifyKeys(), uint64(self.clock.Now().UnixMilli()))
	if err != nil {
		self.t.Fatal(err)
	}
	if !bytes.Equal(ticket.GetListenerClientId(), edgeClientId.Bytes()) || !bytes.Equal(ticket.GetDialerClientId(), clientId.Bytes()) {
		self.t.Fatal("the ticket names other clients")
	}
	return time.UnixMilli(int64(ticket.GetGrantExpireTimeMillis())).UTC()
}

// Whether field edgeClientId of {svc_<pairClientId>} stands, as CreateContract reads it, and the grant expiry it holds.
func servicePlatformPair(t testing.TB, ctx context.Context, pairClientId server.Id, edgeClientId server.Id) (time.Time, bool) {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	grantExpireMillis, found, err := model.GetSocketServicePair(readCtx, connect.Id(pairClientId), connect.Id(edgeClientId))
	if err != nil {
		t.Fatal(err)
	}
	return time.UnixMilli(int64(grantExpireMillis)).UTC(), found
}

// Counts the decision's pair reads until the test ends.
func servicePlatformCountReads(t testing.TB) *int {
	reads := 0
	read := getSocketServicePair
	getSocketServicePair = func(ctx context.Context, pairClientId connect.Id, edgeClientId connect.Id) (uint64, bool, error) {
		reads += 1
		return read(ctx, pairClientId, edgeClientId)
	}
	t.Cleanup(func() {
		getSocketServicePair = read
	})
	return &reads
}

// QD-728 through a ur/ Find on Redis: the pair stands under the sidecar dialer's key, field the edge listener, holding
// the ticket's grant, and nothing stands under the edge's key. CreateContract issues Service both ways on it: the
// sidecar's forward contract and the edge's replies, companion or not, each ending at the contract's own hour, since
// smtpedge's grant runs past it. Each direction's decision is one Redis read and no PostgreSQL. A dialer that advertises
// no Service gets its forward contract, and the edge's reply to it is refused for want of its key (§6.10.1 item 2;
// question ur-pair-key-dialer-service).
func TestCreateContractServicePlatformPairBothWays(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		nets := newServiceTestNets(t, ctx)
		sockets := newServicePlatformSockets(t, ctx, nets)
		edge := nets.client(nets.edgeNetworkId, model.ProvideModeService)
		// a sidecar that also receives mail holds an smtp/ lease, so it advertises Service (§13.6)
		dialer := nets.client(nets.sidecarNetworkId, model.ProvideModeNetwork, model.ProvideModeService)
		sockets.listEdge(edge, 1, true)

		before := rejectionCount(t, "internal", "service_no_pair", "false")
		for _, pair := range [][2]server.Id{{dialer, edge}, {edge, dialer}} {
			if result := serviceTestCreate(t, ctx, pair[0], pair[1], false); !serviceTestRefused(result) {
				t.Fatalf("%s->%s before a Find: %+v", pair[0], pair[1], result)
			}
		}
		connect.AssertEqual(t, rejectionCount(t, "internal", "service_no_pair", "false"), before+2)

		grantExpire := sockets.find(dialer, edge)
		if want := sockets.clock.Now().Add(sockets.grantLifetime).UnixMilli(); grantExpire.UnixMilli() != want {
			t.Fatalf("the grant ends at %v, want %v", grantExpire, time.UnixMilli(want))
		}
		pairGrantExpire, found := servicePlatformPair(t, ctx, dialer, edge)
		if !found || !pairGrantExpire.Equal(grantExpire) {
			t.Fatalf("the pair under the dialer: %t, %v", found, pairGrantExpire)
		}
		if _, found := servicePlatformPair(t, ctx, edge, dialer); found {
			t.Fatal("a pair stands under the edge's key")
		}

		forward := serviceTestIssued(t, "the sidecar to the edge", serviceTestCreate(t, ctx, dialer, edge, false))
		connect.AssertEqual(t, forward.SourceNetworkId, nets.sidecarNetworkId.Bytes())
		// the edge's reply to the sidecar's contract takes the forward contract's seals
		reply := serviceTestIssued(t, "the edge's reply", serviceTestCreate(t, ctx, edge, dialer, true))
		connect.AssertEqual(t, reply.SourceNetworkId, nets.sidecarNetworkId.Bytes())
		edgeForward := serviceTestIssued(t, "the edge to the sidecar", serviceTestCreate(t, ctx, edge, dialer, false))
		connect.AssertEqual(t, edgeForward.SourceNetworkId, nets.edgeNetworkId.Bytes())
		for name, storedContract := range map[string]*protocol.StoredContract{"forward": forward, "reply": reply, "edge forward": edgeForward} {
			life := time.UnixMilli(*storedContract.ExpirationTimeUnixMilli).Sub(server.NowUtc())
			if life <= model.DefaultContractExpiration-time.Minute || model.DefaultContractExpiration < life {
				t.Errorf("%s: ends in %v, want the hour", name, life)
			}
		}

		// one read of the one field each way, with the relationship's facts already read
		reads := servicePlatformCountReads(t)
		forwardDetails := model.GetProvideRelationshipDetails(ctx, dialer, edge)
		replyDetails := model.GetProvideRelationshipDetails(ctx, edge, dialer)
		config := server.CurrentSidecarConfig()
		attempts, stop := server.DenyPostgresForTest(t)
		forwardDecision := decideServiceContract(ctx, dialer, edge, &forwardDetails, config, server.NowUtc())
		forwardReads := *reads
		replyDecision := decideServiceContract(ctx, edge, dialer, &replyDetails, config, server.NowUtc())
		stop()
		connect.AssertEqual(t, attempts(), uint64(0))
		connect.AssertEqual(t, forwardDecision.outcome, contractServiceIssue)
		connect.AssertEqual(t, replyDecision.outcome, contractServiceIssue)
		connect.AssertEqual(t, forwardReads, 1)
		connect.AssertEqual(t, *reads, 2)

		// a dialer that holds no tls/ or smtp/ lease advertises no Service: the edge has nothing to sign its reply with
		bare := nets.client(nets.sidecarNetworkId, model.ProvideModeNetwork, model.ProvideModeStream)
		sockets.find(bare, edge)
		serviceTestIssued(t, "a bare dialer to the edge", serviceTestCreate(t, ctx, bare, edge, false))
		for _, companion := range []bool{false, true} {
			label := fmt.Sprintf("%t", companion)
			secretBefore := rejectionCount(t, "internal", "provide_secret_unavailable", label)
			if result := serviceTestCreate(t, ctx, edge, bare, companion); !serviceTestRefused(result) {
				t.Errorf("the edge's reply to a bare dialer, companion %t: %+v", companion, result)
			}
			connect.AssertEqual(t, rejectionCount(t, "internal", "provide_secret_unavailable", label), secretBefore+1)
		}
	})
}

// tls/ and smtp/ keep their pairs' keying under QD-728: an edge's Find of a sidecar's listener writes field
// <edge dialer> of {svc_<sidecar listener>} (nets.pair, as find.go's edge pair does), which gives Service both ways on
// one read as before. One sidecar's key holds that pair beside its own ur/ pair, and each gives Service with its own
// edge alone. A pair under an edge's key, where a ur/ pair stood before QD-728, gives nothing either way.
func TestCreateContractServiceEdgePairsKeepTheirKey(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		nets := newServiceTestNets(t, ctx)
		sockets := newServicePlatformSockets(t, ctx, nets)
		// a tlsedge or mxedge client dialing the sidecar's listener, and smtpedge's listener the sidecar dials
		dialingEdge := nets.client(nets.edgeNetworkId, model.ProvideModeService)
		listeningEdge := nets.client(nets.edgeNetworkId, model.ProvideModeService)
		strayEdge := nets.client(nets.edgeNetworkId, model.ProvideModeService)
		sidecarClient := nets.client(nets.sidecarNetworkId, model.ProvideModeNetwork, model.ProvideModeService)
		nets.pair(sidecarClient, dialingEdge, server.NowUtc().Add(10*time.Minute))
		sockets.listEdge(listeningEdge, 1, true)
		sockets.find(sidecarClient, listeningEdge)

		reads := servicePlatformCountReads(t)
		for _, edge := range []server.Id{dialingEdge, listeningEdge} {
			for _, pair := range [][2]server.Id{{edge, sidecarClient}, {sidecarClient, edge}} {
				serviceTestIssued(t, fmt.Sprintf("%s->%s", pair[0], pair[1]), serviceTestCreate(t, ctx, pair[0], pair[1], false))
			}
		}
		connect.AssertEqual(t, *reads, 4)
		for _, pair := range [][2]server.Id{{strayEdge, sidecarClient}, {sidecarClient, strayEdge}} {
			if result := serviceTestCreate(t, ctx, pair[0], pair[1], false); !serviceTestRefused(result) {
				t.Errorf("%s->%s with no pair of its own: %+v", pair[0], pair[1], result)
			}
		}

		// the edge-keyed pair a ur/ Find would have written before QD-728
		edgeKeyed := nets.client(nets.sidecarNetworkId, model.ProvideModeNetwork, model.ProvideModeService)
		wrote, _, err := model.NewSocketStore(nil, nil).SetServicePair(ctx, connect.Id(listeningEdge), connect.Id(edgeKeyed),
			uint64(server.NowUtc().Add(time.Hour).UnixMilli()), time.Hour, 1)
		if err != nil || !wrote {
			t.Fatalf("the edge-keyed pair: %t, %v", wrote, err)
		}
		for _, pair := range [][2]server.Id{{edgeKeyed, listeningEdge}, {listeningEdge, edgeKeyed}} {
			for _, companion := range []bool{false, true} {
				if result := serviceTestCreate(t, ctx, pair[0], pair[1], companion); !serviceTestRefused(result) {
					t.Errorf("%s->%s companion %t on an edge-keyed pair: %+v", pair[0], pair[1], companion, result)
				}
			}
		}
	})
}

// A ur/ pair lives its service's grant and ends there (§9.6, QD-190, QD-728): its field's ttl on Redis is the grant's
// life, 125 min for smtpedge, and the decision issues until the grant's end and refuses at it. The edge's unlisten does
// not reach the pair, which stands under the sidecar's key, so the stream keeps Service until the grant ends.
func TestCreateContractServicePlatformPairEndsAtItsTtl(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		nets := newServiceTestNets(t, ctx)
		sockets := newServicePlatformSockets(t, ctx, nets)
		edge := nets.client(nets.edgeNetworkId, model.ProvideModeService)
		dialer := nets.client(nets.sidecarNetworkId, model.ProvideModeNetwork, model.ProvideModeService)
		sockets.listEdge(edge, 1, true)
		grantExpire := sockets.find(dialer, edge)

		// §2.8's sock-3: the pair is field <edge> of {svc_<dialer>}, its ttl the grant's life
		server.Redis(ctx, func(client server.RedisClient) {
			ttls, err := client.HPTTL(ctx, fmt.Sprintf("{svc_%s}", dialer), hex.EncodeToString(edge.Bytes())).Result()
			if err != nil || len(ttls) != 1 {
				t.Fatalf("the pair's ttl %v: %v", ttls, err)
			}
			if ttls[0] <= (sockets.grantLifetime-time.Minute).Milliseconds() || sockets.grantLifetime.Milliseconds() < ttls[0] {
				t.Errorf("the pair lives %d ms, want the grant's %v", ttls[0], sockets.grantLifetime)
			}
		})

		config := server.CurrentSidecarConfig()
		for _, pair := range [][2]server.Id{{dialer, edge}, {edge, dialer}} {
			details := model.GetProvideRelationshipDetails(ctx, pair[0], pair[1])
			last := decideServiceContract(ctx, pair[0], pair[1], &details, config, grantExpire.Add(-time.Millisecond))
			if last.outcome != contractServiceIssue || !last.deadline.Equal(grantExpire) {
				t.Errorf("%s->%s a millisecond before the grant's end: %+v", pair[0], pair[1], last)
			}
			ended := decideServiceContract(ctx, pair[0], pair[1], &details, config, grantExpire)
			if ended.outcome != contractServiceRefuse || ended.cause != "service_no_pair" {
				t.Errorf("%s->%s at the grant's end: %+v", pair[0], pair[1], ended)
			}
		}

		// the edge's last unlisten leaves the pair under the dialer's key, and Service with it
		sockets.listEdge(edge, 2, false)
		if pairGrantExpire, found := servicePlatformPair(t, ctx, dialer, edge); !found || !pairGrantExpire.Equal(grantExpire) {
			t.Fatalf("the pair after the edge's unlisten: %t, %v", found, pairGrantExpire)
		}
		serviceTestIssued(t, "the sidecar to the edge after its unlisten", serviceTestCreate(t, ctx, dialer, edge, false))
		serviceTestIssued(t, "the edge's reply after its unlisten", serviceTestCreate(t, ctx, edge, dialer, true))
	})
}

// A revoked sidecar's ur/ pair goes with it (§7.10, §9.8, QD-728): the removal deactivates its client, whose
// {X}c_removed reaches the engine's ClientRemoved, and the store's removal takes the sidecar's key, its ur/ pair with
// it, leaving a floor no position passes. CreateContract then refuses both ways, a Find that judged before the removal
// writes nothing, and the engine refuses the sidecar's next Find; another sidecar's pair to the same edge stands.
func TestCreateContractServicePlatformPairGoesWithTheSidecar(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		nets := newServiceTestNets(t, ctx)
		sockets := newServicePlatformSockets(t, ctx, nets)
		edge := nets.client(nets.edgeNetworkId, model.ProvideModeService)
		revoked := nets.client(nets.sidecarNetworkId, model.ProvideModeNetwork, model.ProvideModeService)
		other := nets.client(nets.sidecarNetworkId, model.ProvideModeNetwork, model.ProvideModeService)
		sockets.listEdge(edge, 1, true)
		grantExpire := sockets.find(revoked, edge)
		sockets.find(other, edge)
		serviceTestIssued(t, "the sidecar to the edge before its removal", serviceTestCreate(t, ctx, revoked, edge, false))

		removals, _ := sockets.engine.Removals()
		sockets.engine.ClientRemoved(connect.Id(revoked))
		for {
			count, changed := sockets.engine.Removals()
			if removals < count {
				break
			}
			select {
			case <-changed:
			case <-ctx.Done():
				t.Fatal("the removal never ran")
			}
		}

		if _, found := servicePlatformPair(t, ctx, revoked, edge); found {
			t.Fatal("a removed sidecar's ur/ pair stands")
		}
		if _, found := servicePlatformPair(t, ctx, other, edge); !found {
			t.Fatal("another sidecar's ur/ pair went with the removal")
		}
		before := rejectionCount(t, "internal", "service_no_pair", "false")
		for _, pair := range [][2]server.Id{{revoked, edge}, {edge, revoked}} {
			if result := serviceTestCreate(t, ctx, pair[0], pair[1], false); !serviceTestRefused(result) {
				t.Errorf("%s->%s after the removal: %+v", pair[0], pair[1], result)
			}
		}
		connect.AssertEqual(t, rejectionCount(t, "internal", "service_no_pair", "false"), before+2)
		serviceTestIssued(t, "another sidecar to the edge", serviceTestCreate(t, ctx, other, edge, false))

		// a Find another process judged before the removal, at any position, writes nothing past the floor
		wrote, floor, err := model.NewSocketStore(nil, nil).SetServicePair(ctx, connect.Id(revoked), connect.Id(edge),
			uint64(grantExpire.UnixMilli()), sockets.grantLifetime, socketsservice.SocketRemovedClientPosition-1)
		if err != nil || wrote || floor != socketsservice.SocketRemovedClientPosition {
			t.Fatalf("a late pair write: wrote %t, floor %d, %v", wrote, floor, err)
		}
		if code, _ := sockets.findAnswer(revoked); code != http.StatusUnauthorized {
			t.Fatalf("the removed sidecar's Find answered %d", code)
		}
	})
}
