package gkm

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// How long a step that waits on another goroutine may take before the test fails: a guard, never the proof.
const hostTestStepTimeout = 30 * time.Second

// The block's status port as warp names it.
const hostTestStatusPort = 80

// What a test says gkm's newest verified state holds: each admitted sidecar's identity key and the rights it holds on
// every repo, the log at position 10 on one chain, and no holder dropped anywhere.
type hostTestAuthState struct {
	stateLock     sync.Mutex
	identityKeys  map[sidecar.Id][]byte
	sidecarRights map[sidecar.Id]sidecar.AclEvsRights
}

func newHostTestAuthState() *hostTestAuthState {
	return &hostTestAuthState{identityKeys: map[sidecar.Id][]byte{}, sidecarRights: map[sidecar.Id]sidecar.AclEvsRights{}}
}

// Admits a member device with its identity key and rights.
func (self *hostTestAuthState) admit(sidecarId sidecar.Id, identityKey []byte, rights sidecar.AclEvsRights) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.identityKeys[sidecarId] = identityKey
	self.sidecarRights[sidecarId] = rights
}

func (self *hostTestAuthState) CheckRevocation(ctx context.Context, principal *sidecar.Principal) error {
	return nil
}

func (self *hostTestAuthState) Sidecar(principal *sidecar.Principal, sidecarId sidecar.Id) (*gkmservice.GkmSidecarFacts, gkmservice.GkmAuthAnswer) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	identityKey, ok := self.identityKeys[sidecarId]
	if !ok {
		return nil, gkmservice.GkmAuthDenied
	}
	memberId := sidecarId
	return &gkmservice.GkmSidecarFacts{IdentityPublicKey: identityKey, MemberDevice: true, MemberId: &memberId}, gkmservice.GkmAuthAllowed
}

func (self *hostTestAuthState) HonorScope(principal *sidecar.Principal, repoId sidecar.Id, right sidecar.AclEvsRights, now time.Time) gkmservice.GkmAuthAnswer {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if right != 0 && self.sidecarRights[sidecar.Id(*principal.SidecarId)].Has(right) {
		return gkmservice.GkmAuthAllowed
	}
	return gkmservice.GkmAuthDenied
}

func (self *hostTestAuthState) Reads(principal *sidecar.Principal, repoId sidecar.Id, sidecarId sidecar.Id, now time.Time) gkmservice.GkmAuthAnswer {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.sidecarRights[sidecarId].Has(sidecar.AclEvsRead) {
		return gkmservice.GkmAuthAllowed
	}
	return gkmservice.GkmAuthDenied
}

func (self *hostTestAuthState) PendingTargets(principal *sidecar.Principal, sidecarId sidecar.Id, now time.Time) ([]*gkmservice.GkmPendingTarget, gkmservice.GkmAuthAnswer) {
	return nil, gkmservice.GkmAuthAllowed
}

func (self *hostTestAuthState) RepoReaders(principal *sidecar.Principal, repoId sidecar.Id, now time.Time) ([]sidecar.Id, gkmservice.GkmAuthAnswer) {
	return nil, gkmservice.GkmAuthAllowed
}

func (self *hostTestAuthState) RepoWriters(principal *sidecar.Principal, repoId sidecar.Id, now time.Time) ([]sidecar.Id, gkmservice.GkmAuthAnswer) {
	return nil, gkmservice.GkmAuthAllowed
}

func (self *hostTestAuthState) CheckPin(principal *sidecar.Principal, pin *bridge.AuthPin) gkmservice.GkmAuthAnswer {
	if 10 < pin.GetVersion() {
		return gkmservice.GkmAuthUnknown
	}
	return gkmservice.GkmAuthAllowed
}

func (self *hostTestAuthState) DelegationReads(principal *sidecar.Principal, repoId sidecar.Id, delegationId []byte, now time.Time) gkmservice.GkmAuthAnswer {
	return gkmservice.GkmAuthDenied
}

func (self *hostTestAuthState) LateHolders(principal *sidecar.Principal, repoId sidecar.Id, recipient string, now time.Time) ([]sidecar.Id, gkmservice.GkmAuthAnswer) {
	return nil, gkmservice.GkmAuthAllowed
}

func (self *hostTestAuthState) HoldersDropped(principal *sidecar.Principal, repoId sidecar.Id, pin *bridge.AuthPin, since time.Time, now time.Time) gkmservice.GkmAuthAnswer {
	return gkmservice.GkmAuthDenied
}

// The fake clusters, whose sentinel the test says is read or not, as server.FileClusters.Ready answers.
type hostTestClusters struct {
	*sidecartest.FakeBlobs
	ready bool
}

func (self *hostTestClusters) Ready() bool {
	return self.ready
}

func (self *hostTestClusters) Close() {}

// Waits for the engine's first probe of the first cluster to finish.
func awaitHostTestProbe(t testing.TB, engine *gkmservice.GkmEngine) {
	t.Helper()
	deadline := time.After(hostTestStepTimeout)
	for {
		count, changed := engine.Probes()
		if 1 <= count {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("the engine never probed its first cluster")
		}
	}
}

// One sidecar of the test: its identity key signs as its daemon would.
type hostTestDevice struct {
	sidecarId  sidecar.Id
	publicKey  ed25519.PublicKey
	privateKey ed25519.PrivateKey
}

func newHostTestDevice(t testing.TB) *hostTestDevice {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &hostTestDevice{sidecarId: sidecar.NewId(), publicKey: publicKey, privateKey: privateKey}
}

func (self *hostTestDevice) sign(purpose string, signedType bridge.SignedType, body []byte, agentId *sidecar.Id) (*bridge.Signed, error) {
	return &bridge.Signed{
		Body:      bytes.Clone(body),
		SignerId:  self.sidecarId.Bytes(),
		Signature: ed25519.Sign(self.privateKey, bridge.SignedBytes(purpose, signedType, body)),
		Type:      uint32(signedType),
	}, nil
}

// A host on a fake ServiceHost with the authorization state given, its first probe done.
func newHostTestHost(t testing.TB, state gkmservice.GkmAuthState, adjust func(host *sidecar.ServiceHost)) (*Host, *sidecartest.FakeHost) {
	t.Helper()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fakeHost := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	host := fakeHost.ServiceHost()
	if adjust != nil {
		adjust(host)
	}
	deps := &HostDeps{}
	if state != nil {
		deps.AuthState = state
	}
	gkmHost, err := NewHost(t.Context(), host, nil, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gkmHost.Close)
	awaitHostTestProbe(t, gkmHost.Engine())
	return gkmHost, fakeHost
}

// A gkm token for device in network with scopes, as the exchange mints it (§7.8).
func hostTestToken(host *sidecartest.FakeHost, networkId connect.Id, device *hostTestDevice, scopes ...string) string {
	connectSidecarId := connect.Id(device.sidecarId)
	now := host.Clock.Now()
	return host.Authenticator.Issue(&sidecar.Principal{
		NetworkId: networkId,
		ClientId:  connect.NewId(),
		SidecarId: &connectSidecarId,
		Audience:  gkmservice.GkmAudience,
		Scopes:    scopes,
		IssuedAt:  now,
		ExpiresAt: now.Add(15 * time.Minute),
	})
}

// One request through handler: its status and body.
func hostTestServe(handler http.Handler, method string, path string, token string, body []byte) (int, []byte) {
	request := httptest.NewRequest(method, "http://gkm.example"+path, bytes.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.Bytes()
}

// Every seam given and the first cluster answering: ready, with nothing missing.
func TestHostReadyWithEverySeam(t *testing.T) {
	gkmHost, _ := newHostTestHost(t, newHostTestAuthState(), nil)
	if missing := gkmHost.Missing(); len(missing) != 0 {
		t.Fatalf("missing %v", missing)
	}
	if !gkmHost.Ready() {
		t.Fatal("not ready with every seam and the first cluster answering")
	}
}

// gkm's authorization state, which server has no implementation of yet, keeps the block not ready and says so, and
// the engine still answers through its guard: a request with no token is refused, one with a token 503.
func TestHostNotReadyWithoutTheAuthState(t *testing.T) {
	gkmHost, fakeHost := newHostTestHost(t, nil, nil)
	missing := gkmHost.Missing()
	if len(missing) != 1 || !strings.Contains(missing[0], "GkmAuthState") {
		t.Fatalf("missing %v", missing)
	}
	if gkmHost.Ready() {
		t.Fatal("ready with no authorization state")
	}
	if code, _ := hostTestServe(gkmHost.Handler(), http.MethodPost, "/gkm/reader-key", "", nil); code != http.StatusUnauthorized {
		t.Errorf("a request with no token: %d", code)
	}
	networkId := connect.NewId()
	fakeHost.Gates.OpenService(sidecar.ServiceGateGkm, networkId)
	device := newHostTestDevice(t)
	token := hostTestToken(fakeHost, networkId, device, "repo:"+sidecar.NewId().String()+":r")
	if code, _ := hostTestServe(gkmHost.Handler(), http.MethodPost, "/gkm/reader-key", token, nil); code != http.StatusServiceUnavailable {
		t.Errorf("a request with no state to judge it: %d", code)
	}
}

// §19.4: ready also needs each cluster's sentinel, whatever the engine's own probe found; Close ends readiness.
func TestHostWaitsForTheClustersSentinel(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fakeHost := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	clusters := &hostTestClusters{FakeBlobs: fakeHost.Blobs}
	gkmHost, err := NewHost(t.Context(), fakeHost.ServiceHost(), nil, &HostDeps{AuthState: newHostTestAuthState(), ClustersReady: clusters.Ready})
	if err != nil {
		t.Fatal(err)
	}
	awaitHostTestProbe(t, gkmHost.Engine())
	if gkmHost.Ready() {
		t.Fatal("ready before every cluster's sentinel read")
	}
	clusters.ready = true
	if !gkmHost.Ready() {
		t.Fatal("not ready once the sentinels read")
	}
	gkmHost.Close()
	if gkmHost.Ready() {
		t.Fatal("ready after Close")
	}
}

// A host with no clusters has nothing to serve from, and one with no write budget may price none of its writes
// (§11.13): each refuses to start.
func TestHostRefusesAHostWithNoClustersOrBudget(t *testing.T) {
	host := sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil).ServiceHost()
	host.Blobs = nil
	if _, err := NewHost(t.Context(), host, nil, &HostDeps{AuthState: newHostTestAuthState()}); err == nil {
		t.Error("a host with no clusters started")
	}
	host = sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil).ServiceHost()
	host.Budget = nil
	if _, err := NewHost(t.Context(), host, nil, &HostDeps{AuthState: newHostTestAuthState()}); err == nil {
		t.Error("a host with no write budget started")
	}
}

// A block whose vault holds no file-clusters-gkm.yml cannot start: its engine is never ready and serves no route, so
// /status says not ready and a deploy reverts.
func TestBlockWithNoClustersFileIsNeverReady(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WARP_VAULT_HOME", filepath.Join(home, "vault"))
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(home, "config"))
	engine := newBlockEngine(t.Context(), nil, &HostDeps{AuthState: newHostTestAuthState()})
	defer engine.Close()
	if _, ok := engine.(notReadyEngine); !ok {
		t.Fatalf("a block with no clusters file built %T", engine)
	}
	options := blockRunOptions(engine, hostTestStatusPort, &HostDeps{})
	if options.Http != nil {
		t.Error("a block that cannot start serves routes")
	}
	if err := options.Validate(); err != nil {
		t.Error(err)
	}
}

// §10.14 and §22.7: the block holds no Postgres pool and reads no pending group's tables, so sidecarhost.Run never
// reads migration_audit for it.
func TestBlockHoldsNoPostgres(t *testing.T) {
	gkmHost, _ := newHostTestHost(t, newHostTestAuthState(), nil)
	options := blockRunOptions(gkmHost, hostTestStatusPort, &HostDeps{})
	if options.Postgres || 0 < len(options.Groups) {
		t.Errorf("the block holds Postgres %t with groups %v", options.Postgres, options.Groups)
	}
	if options.Service != "gkm" || options.Http == nil || options.Stream != nil {
		t.Errorf("options %+v", options)
	}
	if err := options.Validate(); err != nil {
		t.Error(err)
	}
}

// §22.7's no-Postgres assertion (plan-KEYS-hint-addressing): under a process-wide Postgres tripwire the block's host
// starts, a device uploads its ReaderKey and another reads it back, and nothing reached for a Postgres connection.
func TestHostServesUnderAPostgresTripwire(t *testing.T) {
	attempts, stop := server.DenyPostgresForTest(t)
	defer stop()
	state := newHostTestAuthState()
	gkmHost, fakeHost := newHostTestHost(t, state, nil)
	networkId := connect.NewId()
	fakeHost.Gates.OpenService(sidecar.ServiceGateGkm, networkId)
	repoId := sidecar.NewId()
	device := newHostTestDevice(t)
	state.admit(device.sidecarId, device.publicKey, sidecar.AclEvsRead)
	token := hostTestToken(fakeHost, networkId, device, "repo:"+repoId.String()+":r")
	now := fakeHost.Clock.Now()
	signed, _, err := sidecar.NewGkmReaderKey(device.sidecarId, now, now.Add(6*24*time.Hour), device.sign)
	if err != nil {
		t.Fatal(err)
	}
	readerKey, err := proto.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := hostTestServe(gkmHost.Handler(), http.MethodPost, "/gkm/reader-key", token, readerKey); code != http.StatusOK {
		t.Fatalf("the ReaderKey's upload: %d", code)
	}
	code, served := hostTestServe(gkmHost.Handler(), http.MethodGet, "/gkm/reader-key/"+sidecar.EvsKeyId(device.sidecarId), token, nil)
	if code != http.StatusOK || !bytes.Equal(served, readerKey) {
		t.Fatalf("the ReaderKey read back: %d", code)
	}
	body := &protocol.ReaderKey{}
	if err := proto.Unmarshal(signed.GetBody(), body); err != nil || !bytes.Equal(body.GetSidecarId(), device.sidecarId.Bytes()) {
		t.Fatalf("the ReaderKey's body: %v", err)
	}
	if count := attempts(); count != 0 {
		t.Fatalf("gkm reached for Postgres %d times", count)
	}
}
