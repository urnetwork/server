package handlers

// The internal gate's projection reads (ACL-M4-05; QD-680) on a sequencer over T2's fake first cluster, with no
// Postgres: sidecar.AclGateApiSource against the real handlers in place of its fake api, a gate cache over it reaching
// the log's tip and the rows held to their hash; paging from the retention point; and every request without a gate
// host's bearer refused 403 before a read. route_internal_gate_test.go runs them through api's whole table.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// The world's appends through api's sequencer.
type internalGateTestSequencer struct {
	sequencer *model.ConfigLogSequencer
}

func (self internalGateTestSequencer) AppendStatement(ctx context.Context, networkId sidecar.Id, signed []byte) (*bridge.AuthPin, error) {
	return self.sequencer.AppendStatement(ctx, server.Id(networkId), signed)
}

func (self internalGateTestSequencer) AppendBase(ctx context.Context, networkId sidecar.Id, signedHeader []byte, rows []byte, genesis []byte, configPushes [][]byte) (*bridge.AuthPin, error) {
	return self.sequencer.AppendBase(ctx, server.Id(networkId), &model.ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
}

func (self internalGateTestSequencer) AppendTick(ctx context.Context, networkId sidecar.Id, tick *bridge.ApiTick) (*bridge.AuthPin, error) {
	return self.sequencer.AppendTick(ctx, server.Id(networkId), tick)
}

// A network's log at api, served on the two routes to the bearers of its verify file.
type internalGateTestWorld struct {
	t         *testing.T
	ctx       context.Context
	cluster   sidecar.BlobCluster
	world     *sidecartest.ConfigLogSuiteWorld
	sequencer *model.ConfigLogSequencer
	networkId server.Id
	baseRows  []byte
	tip       *bridge.AuthPin
	server    *httptest.Server
	bearers   map[server.InternalGateCaller]string
}

// A fresh bearer and its digest's hex.
func newInternalGateTestBearer(t *testing.T) (string, string) {
	raw := make([]byte, server.DefaultInternalGateSettings().BearerByteCount)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	bearer := base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(bearer))
	return bearer, hex.EncodeToString(digest[:])
}

// A personal network at base 1 and memberAdmitCount statements after it, its sequencer reading pageCount entries a
// page, served to connect's, sockets' and evs's bearers.
func newInternalGateTestWorld(t *testing.T, memberAdmitCount int, pageCount int) *internalGateTestWorld {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	networkId := server.NewId()
	world := sidecartest.NewConfigLogSuiteWorldFor(ctx, cluster, sidecar.Id(networkId), sidecar.NewId(), sidecar.NewId())
	settings := model.DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	settings.ReadPageCount = pageCount
	sequencer := model.NewConfigLogSequencer(cluster, settings, nil)
	if err := world.Start(internalGateTestSequencer{sequencer: sequencer}); err != nil {
		t.Fatalf("base 1: %v", err)
	}
	for range memberAdmitCount {
		pin, err := world.PostMemberAdmit(internalGateTestSequencer{sequencer: sequencer})
		if err != nil {
			t.Fatalf("a member admit: %v", err)
		}
		world.SetTip(pin)
	}
	tip, err := sequencer.Tip(ctx, networkId)
	if err != nil {
		t.Fatal(err)
	}
	_, baseRows, _, _ := world.BaseOne()
	self := &internalGateTestWorld{
		t:         t,
		ctx:       ctx,
		cluster:   cluster,
		world:     world,
		sequencer: sequencer,
		networkId: networkId,
		baseRows:  baseRows,
		tip:       tip,
		bearers:   map[server.InternalGateCaller]string{},
	}
	verifyFile := ""
	for _, caller := range []server.InternalGateCaller{server.InternalGateCallerConnect, server.InternalGateCallerSockets, server.InternalGateCallerEvs} {
		bearer, digest := newInternalGateTestBearer(t)
		self.bearers[caller] = bearer
		verifyFile += string(caller) + ": {class: " + string(caller) + ", sha256: [" + digest + "]}\n"
	}
	verifier, err := server.NewInternalGateVerifier([]byte(verifyFile))
	if err != nil {
		t.Fatal(err)
	}
	handlers := NewInternalGateProjectionHandlers(&InternalGateProjectionDeps{
		Verifier:  verifier,
		Sequencer: func() (*model.ConfigLogSequencer, error) { return sequencer, nil },
	})
	self.server = httptest.NewServer(router.NewRouter(ctx, handlers.Routes()))
	t.Cleanup(self.server.Close)
	return self
}

// One GET with an authorization header, its status and body.
func (self *internalGateTestWorld) get(path string, authorization string) (int, []byte) {
	self.t.Helper()
	request, err := http.NewRequestWithContext(self.ctx, http.MethodGet, self.server.URL+path, nil)
	if err != nil {
		self.t.Fatal(err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		self.t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		self.t.Fatal(err)
	}
	return response.StatusCode, body
}

// A source carrying caller's bearer.
func (self *internalGateTestWorld) source(caller server.InternalGateCaller) *sidecar.AclGateApiSource {
	self.t.Helper()
	bearer := self.bearers[caller]
	source, err := sidecar.NewAclGateApiSource(self.server.URL, nil, func() (string, error) { return bearer, nil }, nil)
	if err != nil {
		self.t.Fatal(err)
	}
	return source
}

// connect's source reads the genesis, every entry page by page and a base's rows held to their hash from the real
// routes, and a gate cache over it loads the network at the log's tip.
func TestInternalGateProjectionServesAGateSource(t *testing.T) {
	world := newInternalGateTestWorld(t, 4, 2)
	source := world.source(server.InternalGateCallerConnect)
	networkId := sidecar.Id(world.networkId)

	genesis, err := source.Genesis(world.ctx, networkId)
	if err != nil || string(genesis) != string(world.world.SignedGenesis()) {
		t.Fatalf("the genesis: %v", err)
	}
	entries, err := source.Entries(world.ctx, networkId, nil)
	if err != nil || len(entries) != int(world.tip.GetVersion()) {
		t.Fatalf("the entries: %d of %d, %v", len(entries), world.tip.GetVersion(), err)
	}
	later, err := source.Entries(world.ctx, networkId, &bridge.AuthPin{Version: 3})
	if err != nil || len(later) != int(world.tip.GetVersion())-3 {
		t.Fatalf("the entries after 3: %d, %v", len(later), err)
	}
	rowsSha256 := sha256.Sum256(world.baseRows)
	rows, err := source.Rows(world.ctx, networkId, rowsSha256[:])
	if err != nil || string(rows) != string(world.baseRows) {
		t.Fatalf("base 1's rows: %v", err)
	}
	missing := sha256.Sum256([]byte("no such rows"))
	if _, err := source.Rows(world.ctx, networkId, missing[:]); err != sidecar.ErrBlobNotFound {
		t.Errorf("rows the store does not hold: %v", err)
	}
	if _, err := world.source(server.InternalGateCallerSockets).Entries(world.ctx, networkId, nil); err != nil {
		t.Errorf("sockets' bearer: %v", err)
	}
	if _, err := world.source(server.InternalGateCallerEvs).Entries(world.ctx, networkId, nil); err == nil {
		t.Error("evs's bearer read the log, which evs reads from the store itself")
	}

	loads := make(chan *bridge.AuthPin, 16)
	gateSettings := sidecar.DefaultAclGateSettings()
	gateSettings.PollInterval = 0
	gateSettings.FailureBackoff = 0
	gateSettings.Log = sidecar.NewNoopLogger()
	gateSettings.OnLoad = func(loadedNetworkId sidecar.Id, pin *bridge.AuthPin, err error) {
		if loadedNetworkId == networkId && err == nil && pin != nil {
			loads <- pin
		}
	}
	cache := sidecar.NewAclGateCache(world.ctx, source, world.world.Clock(), gateSettings)
	defer cache.Close()
	cache.Changed(networkId)
	select {
	case pin := <-loads:
		if pin.GetVersion() != world.tip.GetVersion() || string(pin.GetChain()) != string(world.tip.GetChain()) {
			t.Fatalf("the cache loaded %d, the tip is %d", pin.GetVersion(), world.tip.GetVersion())
		}
	case <-time.After(time.Minute):
		t.Fatal("the cache over the source never loaded")
	}
	snapshot, status := cache.State(networkId)
	if status != sidecar.AclGateLoaded || snapshot == nil || snapshot.Acl == nil {
		t.Fatalf("the cache's state: %v", status)
	}
}

// A page starts at the retention point when the position asked is before it: the entries the store no longer holds
// are skipped, the genesis still comes with a read from 0, and the pages follow on to the tip.
func TestInternalGateProjectionPagesFromTheRetentionPoint(t *testing.T) {
	world := newInternalGateTestWorld(t, 4, 2)
	for _, n := range []uint64{1, 2} {
		if err := world.cluster.Delete(world.ctx, sidecar.ConfigEntryKey(sidecar.Id(world.networkId), n)); err != nil {
			t.Fatal(err)
		}
	}
	authorization := "Bearer " + world.bearers[server.InternalGateCallerConnect]
	read := func(after string) *controller.NetworkConfigProjectionResult {
		t.Helper()
		status, body := world.get(sidecar.AclGateApiProjectionPath+world.networkId.String()+"?after="+after, authorization)
		if status != http.StatusOK {
			t.Fatalf("after %s answered %d", after, status)
		}
		page := &controller.NetworkConfigProjectionResult{}
		if err := json.Unmarshal(body, page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	first := read("0")
	if len(first.Genesis) == 0 || len(first.Entries) != 2 || !first.More || first.Tip == nil || first.Tip.N != 4 {
		t.Fatalf("the first page: genesis %d B, %d entries, more %t, tip %+v", len(first.Genesis), len(first.Entries), first.More, first.Tip)
	}
	second := read("4")
	if len(second.Genesis) != 0 || len(second.Entries) != 1 || second.More || second.Tip == nil || second.Tip.N != world.tip.GetVersion() {
		t.Fatalf("the second page: %d entries, more %t, tip %+v", len(second.Entries), second.More, second.Tip)
	}
	if string(second.Tip.Chain) != string(world.tip.GetChain()) {
		t.Error("the last page's tip is not the log's")
	}
	none := read("1")
	if len(none.Entries) != 2 || none.Tip.N != 4 {
		t.Errorf("a read after a dropped position: %d entries, tip %+v", len(none.Entries), none.Tip)
	}
	empty := server.NewId()
	status, body := world.get(sidecar.AclGateApiProjectionPath+empty.String(), authorization)
	if status != http.StatusOK || string(body) == "" {
		t.Fatalf("a network with no log answered %d", status)
	}
	page := &controller.NetworkConfigProjectionResult{}
	if err := json.Unmarshal(body, page); err != nil || len(page.Entries) != 0 || page.More || page.Tip != nil {
		t.Errorf("a network with no log: %+v, %v", page, err)
	}
}

// Every request without a gate host's bearer, a malformed id, position or hash, and a process with no sequencer, each
// answered before or without a read.
func TestInternalGateProjectionRefusals(t *testing.T) {
	world := newInternalGateTestWorld(t, 1, 8)
	stranger, strangerDigest := newInternalGateTestBearer(t)
	connect := "Bearer " + world.bearers[server.InternalGateCallerConnect]
	rowsSha256 := sha256.Sum256(world.baseRows)
	projection := sidecar.AclGateApiProjectionPath + world.networkId.String()
	rows := sidecar.AclGateApiRowsPath + world.networkId.String() + "/" + hex.EncodeToString(rowsSha256[:])
	for _, path := range []string{projection, rows} {
		for _, authorization := range []string{"", "Bearer " + stranger, "Bearer " + strangerDigest, "Bearer " + world.bearers[server.InternalGateCallerEvs], world.bearers[server.InternalGateCallerConnect]} {
			if status, _ := world.get(path, authorization); status != http.StatusForbidden {
				t.Errorf("%s with %q answered %d", path, authorization, status)
			}
		}
		if status, _ := world.get(path, connect); status != http.StatusOK {
			t.Errorf("%s with connect's bearer answered %d", path, status)
		}
	}
	cases := []struct {
		path   string
		status int
	}{
		{path: sidecar.AclGateApiProjectionPath + "not-an-id", status: http.StatusBadRequest},
		{path: sidecar.AclGateApiProjectionPath + server.Id{}.String(), status: http.StatusBadRequest},
		{path: projection + "?after=-1", status: http.StatusBadRequest},
		{path: projection + "?after=x", status: http.StatusBadRequest},
		{path: sidecar.AclGateApiRowsPath + world.networkId.String() + "/" + hex.EncodeToString(rowsSha256[:16]), status: http.StatusBadRequest},
		{path: sidecar.AclGateApiRowsPath + world.networkId.String() + "/" + "AB" + hex.EncodeToString(rowsSha256[1:]), status: http.StatusBadRequest},
		{path: sidecar.AclGateApiRowsPath + server.NewId().String() + "/" + hex.EncodeToString(rowsSha256[:]), status: http.StatusNotFound},
	}
	for _, c := range cases {
		if status, _ := world.get(c.path, connect); status != c.status {
			t.Errorf("%s answered %d, want %d", c.path, status, c.status)
		}
	}

	unavailable := NewInternalGateProjectionHandlers(&InternalGateProjectionDeps{
		Verifier: mustInternalGateVerifier(t, world),
		Sequencer: func() (*model.ConfigLogSequencer, error) {
			return nil, &model.ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable"}
		},
	})
	handler := router.NewRouter(world.ctx, unavailable.Routes())
	for _, path := range []string{projection, rows} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", connect)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with no sequencer answered %d", path, recorder.Code)
		}
	}
	var unloaded *server.InternalGateVerifier
	noVerifier := &InternalGateProjectionHandlers{
		verifier:  func() *server.InternalGateVerifier { return unloaded },
		sequencer: func() (*model.ConfigLogSequencer, error) { return world.sequencer, nil },
	}
	handler = router.NewRouter(world.ctx, noVerifier.Routes())
	request := httptest.NewRequest(http.MethodGet, projection, nil)
	request.Header.Set("Authorization", connect)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("no verify file answered %d", recorder.Code)
	}
}

// The world's verify file, connect's bearer among its digests.
func mustInternalGateVerifier(t *testing.T, world *internalGateTestWorld) *server.InternalGateVerifier {
	t.Helper()
	digest := sha256.Sum256([]byte(world.bearers[server.InternalGateCallerConnect]))
	verifier, err := server.NewInternalGateVerifier([]byte("connect: {class: connect, sha256: [" + hex.EncodeToString(digest[:]) + "]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}
