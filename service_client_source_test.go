package server

// An edge block's platform clients (TLS-M5-11, QD-812, QD-873, QD-1054): edge.yml admits only the block, host, slot
// and index it names; the derivation is fixed; two processes of one block on one host in different slots hold
// disjoint clients and both connect their pools, and one slot holds the same clients across restarts; WARP_SLOT
// reads only a slot the key admits; and PlatformClientSource asks api for each index at start, holds each client's
// id for its life, keeps each client's sockets token beside its jwt, refreshes both at the refresh point of the
// shorter-lived, rides an api outage on the jwt and the token it holds until each ends, refuses an answer with no
// sockets token, presents each client's token, never its jwt, to sockets, and never puts the bearer, a jwt or a token
// in an error. api is an httptest server and time a fake clock: nothing sleeps.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
)

func TestEdgeClientsConfigAdmitsOnlyWhatItNames(t *testing.T) {
	config, err := ParseEdgeClientsConfig([]byte(
		"platform_url: wss://connect.edge.example\n"+
			"blocks:\n"+
			"  tlsedge: {clients: 4, hosts: [lb-a.example, lb-b.example]}\n"+
			"  smtpedge: {clients: 1, hosts: [lb-a.example]}\n",
	), nil)
	if err != nil {
		t.Fatal(err)
	}
	if config.PlatformUrl() != "wss://connect.edge.example" || config.ClientCount("tlsedge") != 4 || config.ClientCount("mxedge") != 0 {
		t.Fatalf("read %q %d %d", config.PlatformUrl(), config.ClientCount("tlsedge"), config.ClientCount("mxedge"))
	}
	admitted := []*EdgeClientMintArgs{
		{Block: "tlsedge", Host: "lb-a.example", Slot: 0, Index: 0},
		{Block: "tlsedge", Host: "lb-b.example", Slot: 0, Index: 3},
		// a deploy's new process beside the old one
		{Block: "tlsedge", Host: "lb-b.example", Slot: 1, Index: 3},
		{Block: "smtpedge", Host: "lb-a.example", Slot: 1, Index: 0},
	}
	for _, args := range admitted {
		if err := config.Admits(args); err != nil {
			t.Errorf("%+v: %v", args, err)
		}
	}
	refused := []*EdgeClientMintArgs{
		nil,
		// a block the file does not list
		{Block: "mxedge", Host: "lb-a.example", Index: 0},
		{Block: "", Host: "lb-a.example", Index: 0},
		// a host the block does not list, though another block does
		{Block: "smtpedge", Host: "lb-b.example", Index: 0},
		{Block: "tlsedge", Host: "lb-c.example", Index: 0},
		{Block: "tlsedge", Host: "", Index: 0},
		// a slot at or past the slot count, 2
		{Block: "tlsedge", Host: "lb-a.example", Slot: 2, Index: 0},
		{Block: "tlsedge", Host: "lb-a.example", Slot: -1, Index: 0},
		// an index at or past the block's K
		{Block: "tlsedge", Host: "lb-a.example", Index: 4},
		{Block: "smtpedge", Host: "lb-a.example", Index: 1},
		{Block: "tlsedge", Host: "lb-a.example", Index: -1},
	}
	for _, args := range refused {
		if err := config.Admits(args); !errors.Is(err, ErrEdgeClientRefused) {
			t.Errorf("%+v: %v, want refused", args, err)
		}
	}
	// the slot's refusal names the part, and the slot count is the key settings' the file was read under
	if err := config.Admits(&EdgeClientMintArgs{Block: "tlsedge", Host: "lb-a.example", Slot: 2, Index: 0}); err == nil || !strings.HasSuffix(err.Error(), ": slot") {
		t.Errorf("slot 2's refusal: %v", err)
	}
	keySettings := DefaultEdgeClientKeySettings()
	keySettings.SlotCount = 3
	wider, err := ParseEdgeClientsConfig([]byte("platform_url: wss://connect.edge.example\nblocks: {tlsedge: {clients: 4, hosts: [lb-a.example]}}\n"), keySettings)
	if err != nil {
		t.Fatal(err)
	}
	if err := wider.Admits(&EdgeClientMintArgs{Block: "tlsedge", Host: "lb-a.example", Slot: 2, Index: 0}); err != nil {
		t.Errorf("slot 2 under a slot count of 3: %v", err)
	}
	keySettings.SlotCount = 0
	none, err := ParseEdgeClientsConfig([]byte("platform_url: wss://connect.edge.example\nblocks: {tlsedge: {clients: 4, hosts: [lb-a.example]}}\n"), keySettings)
	if err != nil {
		t.Fatal(err)
	}
	if err := none.Admits(&EdgeClientMintArgs{Block: "tlsedge", Host: "lb-a.example", Slot: 0, Index: 0}); !errors.Is(err, ErrEdgeClientRefused) {
		t.Errorf("a slot count of 0 admitted slot 0: %v", err)
	}
}

func TestEdgeClientsConfigRefusesFilesItCannotUse(t *testing.T) {
	files := []string{
		"",
		"blocks: {tlsedge: {clients: 4, hosts: []}}\n",
		"platform_url: https://connect.edge.example\nblocks: {}\n",
		"platform_url: wss://connect.edge.example\nblocks: {tlsedge: {clients: 0, hosts: []}}\n",
		"platform_url: wss://connect.edge.example\nblocks: {tlsedge: {clients: 65, hosts: []}}\n",
		"platform_url: wss://connect.edge.example\nblocks: {tlsedge: {clients: 4, hosts: [\"lb a\"]}}\n",
		"platform_url: wss://connect.edge.example\nblocks: {tlsedge: {clients: 4, hosts: [LB-A]}}\n",
		"platform_url: wss://connect.edge.example\nblocks: {\"tls edge\": {clients: 4, hosts: []}}\n",
		"platform_url: wss://connect.edge.example\nblocks: {tlsedge: }\n",
		"platform_url: wss://connect.edge.example\nblocks: {tlsedge: {clients: 4, hosts: [], extra: 1}}\n",
		"platform_url: wss://connect.edge.example\nhosts: []\n",
	}
	for _, file := range files {
		if _, err := ParseEdgeClientsConfig([]byte(file), nil); !errors.Is(err, ErrEdgeClientRefused) {
			t.Errorf("%q: %v, want refused", file, err)
		}
	}
	// a file with no host admits nothing, and is still a file
	config, err := ParseEdgeClientsConfig([]byte("platform_url: wss://connect.edge.example\nblocks: {tlsedge: {clients: 4, hosts: []}}\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Admits(&EdgeClientMintArgs{Block: "tlsedge", Host: "lb-a.example", Index: 0}); !errors.Is(err, ErrEdgeClientRefused) {
		t.Fatalf("a file with no host admitted one: %v", err)
	}
}

func TestDeriveEdgeClientIdIsFixed(t *testing.T) {
	// the derivation's vectors: a change here is a new set of clients in urnetwork-edge
	// computed apart from this code: the first 16 bytes of SHA-256 over the label and the five parts, each but
	// the last followed by 0x00
	clientId := DeriveEdgeClientId("main", "lb-a.example", "tlsedge", 0, 0)
	if got := hex.EncodeToString(clientId[:]); got != "27b49c5732a69faf303d00c2f028ca3e" {
		t.Fatalf("client id %s", got)
	}
	deviceId := DeriveEdgeDeviceId("main", "lb-a.example", "tlsedge", 0, 0)
	if got := hex.EncodeToString(deviceId[:]); got != "49f2230c2ac17d2df595268b4ffb4450" {
		t.Fatalf("device id %s", got)
	}
	otherSlotId := DeriveEdgeClientId("main", "lb-a.example", "tlsedge", 1, 0)
	if got := hex.EncodeToString(otherSlotId[:]); got != "e2790ef9a1428e11aaf51a7d1dc46c27" {
		t.Fatalf("slot 1's client id %s", got)
	}
	again := DeriveEdgeClientId("main", "lb-a.example", "tlsedge", 0, 0)
	if clientId != again || clientId == (Id{}) {
		t.Fatalf("the derivation is not a function: %s %s", clientId, again)
	}
	others := []Id{
		DeriveEdgeClientId("canary", "lb-a.example", "tlsedge", 0, 0),
		DeriveEdgeClientId("main", "lb-a.example", "smtpedge", 0, 0),
		DeriveEdgeClientId("main", "lb-b.example", "tlsedge", 0, 0),
		DeriveEdgeClientId("main", "lb-a.example", "tlsedge", 0, 1),
		DeriveEdgeClientId("main", "lb-a.example", "tlsedge", 1, 0),
		DeriveEdgeClientId("main", "lb-a.example", "tlsedge", 1, 1),
		DeriveEdgeDeviceId("main", "lb-a.example", "tlsedge", 0, 0),
		// the host and the block are not interchangeable
		DeriveEdgeClientId("main", "tlsedge", "lb-a.example", 0, 0),
		// no part runs into the next
		DeriveEdgeClientId("main", "lb-a.exampletls", "edge", 0, 0),
		DeriveEdgeClientId("main", "lb-a.example", "tlsedge", 10, 0),
		DeriveEdgeClientId("main", "lb-a.example", "tlsedge", 1, 10),
	}
	seen := map[Id]bool{clientId: true}
	for index, other := range others {
		if seen[other] {
			t.Errorf("derivation %d collides", index)
		}
		seen[other] = true
	}
}

// api's edge client route as a test stands it up: a jwt and a sockets token per ask, each ending its life after
// the ask on the clock, for the client derived from the ask's host, block, slot and index under the environment
// "test", and a slot past the default slot count refused as api refuses it.
type platformClientTestApi struct {
	t      testing.TB
	clock  *sidecartest.FakeClock
	bearer string
	life   time.Duration
	// a sockets token's life: a service token's 15 minutes
	socketsTokenLife time.Duration
	server           *httptest.Server

	stateLock sync.Mutex
	// {slot, index} -> asks answered
	slotIndexAskCounts map[[2]int]int
	// while not 0, every ask is answered with this status
	failStatus int
	// while set, every answer names this client id for every index
	clientIdOverride string
	// while set, every answer carries no sockets token
	noSocketsToken bool
}

func newPlatformClientTestApi(t testing.TB, clock *sidecartest.FakeClock) *platformClientTestApi {
	self := &platformClientTestApi{
		t:                  t,
		clock:              clock,
		bearer:             "edge-bearer-secret",
		life:               time.Hour,
		socketsTokenLife:   15 * time.Minute,
		slotIndexAskCounts: map[[2]int]int{},
	}
	self.server = httptest.NewServer(http.HandlerFunc(self.serve))
	t.Cleanup(self.server.Close)
	return self
}

func (self *platformClientTestApi) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != EdgeClientMintPath || r.Header.Get("Authorization") != "Bearer "+self.bearer {
		http.Error(w, "Forbidden.", http.StatusForbidden)
		return
	}
	args := &EdgeClientMintArgs{}
	if err := json.NewDecoder(r.Body).Decode(args); err != nil || args.Block != "tlsedge" || args.Host != "lb-a.example" ||
		!DefaultEdgeClientKeySettings().slotValid(args.Slot) {
		http.Error(w, "Forbidden.", http.StatusForbidden)
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.failStatus != 0 {
		http.Error(w, "Unavailable.", self.failStatus)
		return
	}
	key := [2]int{args.Slot, args.Index}
	self.slotIndexAskCounts[key] += 1
	clientId := DeriveEdgeClientId("test", args.Host, args.Block, args.Slot, args.Index).String()
	if self.clientIdOverride != "" {
		clientId = self.clientIdOverride
	}
	result := &EdgeClientMintResult{
		ClientId:                     clientId,
		ByClientJwt:                  fmt.Sprintf("jwt-secret-%d-%d-%d", args.Slot, args.Index, self.slotIndexAskCounts[key]),
		ExpireTimeMillis:             self.clock.Now().Add(self.life).UnixMilli(),
		PlatformUrl:                  "wss://connect.edge.example",
		SocketsToken:                 fmt.Sprintf("sockets-secret-%d-%d-%d", args.Slot, args.Index, self.slotIndexAskCounts[key]),
		SocketsTokenExpireTimeMillis: self.clock.Now().Add(self.socketsTokenLife).UnixMilli(),
	}
	if self.noSocketsToken {
		result.SocketsToken = ""
		result.SocketsTokenExpireTimeMillis = 0
	}
	json.NewEncoder(w).Encode(result)
}

func (self *platformClientTestApi) askCount(slot int, index int) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.slotIndexAskCounts[[2]int{slot, index}]
}

func (self *platformClientTestApi) setFailStatus(status int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failStatus = status
}

// A source for slot 0.
func (self *platformClientTestApi) newSource(ctx context.Context, clientCount int) *PlatformClientSource {
	return self.newSlotSource(ctx, 0, clientCount)
}

func (self *platformClientTestApi) newSlotSource(ctx context.Context, slot int, clientCount int) *PlatformClientSource {
	settings := DefaultPlatformClientSourceSettings()
	settings.ClientCount = clientCount
	source, err := NewPlatformClientSource(ctx, &PlatformClientSourceArgs{
		ApiUrl:   self.server.URL,
		Block:    "tlsedge",
		Host:     "lb-a.example",
		Slot:     slot,
		Bearer:   self.bearer,
		Clock:    self.clock,
		Settings: settings,
	})
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(source.Close)
	return source
}

// Waits until the source has kept answerCount answers.
func awaitPlatformClientAnswers(t testing.TB, ctx context.Context, source *PlatformClientSource, answerCount uint64) {
	t.Helper()
	for {
		count, changed := source.answerCount.Get()
		if answerCount <= count {
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("the source kept %d answers, want %d", count, answerCount)
		}
	}
}

// The client ids a ready source holds, by index.
func platformClientIds(t testing.TB, ctx context.Context, source *PlatformClientSource, clientCount int) []Id {
	t.Helper()
	clientIds := []Id{}
	for index := range clientCount {
		auth, err := source.ServiceClient(ctx, index)
		if err != nil {
			t.Fatal(err)
		}
		clientIds = append(clientIds, Id(auth.ClientId))
	}
	return clientIds
}

func TestPlatformClientSourceAsksEachIndexAndRefreshesBeforeItLapses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	api := newPlatformClientTestApi(t, clock)
	source := api.newSource(ctx, 2)
	if source.Ready() && api.askCount(0, 0) == 0 {
		t.Fatal("ready before any answer")
	}
	// each index is asked at start, before anyone calls ServiceClient
	awaitPlatformClientAnswers(t, ctx, source, 2)
	if !source.Ready() {
		t.Fatal("not ready with every index answered")
	}
	first := []string{}
	for index := range 2 {
		auth, err := source.ServiceClient(ctx, index)
		if err != nil {
			t.Fatal(err)
		}
		wantClientId := DeriveEdgeClientId("test", "lb-a.example", "tlsedge", 0, index)
		if Id(auth.ClientId) != wantClientId || auth.ByJwt != fmt.Sprintf("jwt-secret-0-%d-1", index) ||
			auth.ApiUrl != api.server.URL || auth.PlatformUrl != "wss://connect.edge.example" {
			t.Fatalf("client %d: %+v", index, auth.ClientId)
		}
		first = append(first, auth.ByJwt)
	}
	// before the refresh point a call costs api nothing
	if api.askCount(0, 0) != 1 || api.askCount(0, 1) != 1 {
		t.Fatalf("asked %d %d times", api.askCount(0, 0), api.askCount(0, 1))
	}
	// at half the shorter life, the sockets token's 15 minutes, each index is asked again, with nobody calling
	if err := clock.AwaitTimers(ctx, 2); err != nil {
		t.Fatal(err)
	}
	clock.Advance(7*time.Minute + 30*time.Second)
	awaitPlatformClientAnswers(t, ctx, source, 4)
	for index := range 2 {
		auth, err := source.ServiceClient(ctx, index)
		if err != nil {
			t.Fatal(err)
		}
		if auth.ByJwt == first[index] || auth.ByJwt != fmt.Sprintf("jwt-secret-0-%d-2", index) {
			t.Fatalf("client %d was not refreshed", index)
		}
		if Id(auth.ClientId) != DeriveEdgeClientId("test", "lb-a.example", "tlsedge", 0, index) {
			t.Fatalf("client %d changed its id", index)
		}
	}
	if _, err := source.ServiceClient(ctx, 2); !errors.Is(err, ErrPlatformClientSource) {
		t.Fatalf("an index past K: %v", err)
	}
	if _, err := source.ServiceClient(ctx, -1); !errors.Is(err, ErrPlatformClientSource) {
		t.Fatalf("a negative index: %v", err)
	}
}

// A deploy's old and new process of one block on one host (QD-873): in different slots they ask for, hold and are
// ready on disjoint clients, each its own K.
func TestPlatformClientSourcesInTwoSlotsHoldDisjointClients(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	api := newPlatformClientTestApi(t, clock)
	clientCount := 4
	oldSource := api.newSlotSource(ctx, 0, clientCount)
	awaitPlatformClientAnswers(t, ctx, oldSource, uint64(clientCount))
	// the new process starts while the old one still holds its clients
	newSource := api.newSlotSource(ctx, 1, clientCount)
	awaitPlatformClientAnswers(t, ctx, newSource, uint64(clientCount))
	if !oldSource.Ready() || !newSource.Ready() {
		t.Fatalf("ready old %t new %t", oldSource.Ready(), newSource.Ready())
	}
	oldClientIds := platformClientIds(t, ctx, oldSource, clientCount)
	newClientIds := platformClientIds(t, ctx, newSource, clientCount)
	seen := map[Id]bool{}
	for index := range clientCount {
		if oldClientIds[index] != DeriveEdgeClientId("test", "lb-a.example", "tlsedge", 0, index) ||
			newClientIds[index] != DeriveEdgeClientId("test", "lb-a.example", "tlsedge", 1, index) {
			t.Fatalf("index %d holds another slot's client", index)
		}
		for _, clientId := range []Id{oldClientIds[index], newClientIds[index]} {
			if seen[clientId] {
				t.Fatalf("index %d's client is held twice", index)
			}
			seen[clientId] = true
		}
	}
	// each asked only for its own: one ask a slot and index
	for slot := range 2 {
		for index := range clientCount {
			if api.askCount(slot, index) != 1 {
				t.Fatalf("slot %d index %d asked %d times", slot, index, api.askCount(slot, index))
			}
		}
	}
	// the old process ending costs the new one nothing
	oldSource.Close()
	if oldSource.Ready() || !newSource.Ready() {
		t.Fatalf("after the old process ended: ready old %t new %t", oldSource.Ready(), newSource.Ready())
	}
}

// connect as a test models it for one client id: connected from one process at a time, the newest to open it taking
// it over, so a process whose client id another process opened since is no longer connected. Each process opens
// its clients through process(name).
type platformClientTestPlatform struct {
	stateLock sync.Mutex
	// client id -> the process connected as it
	clientIdHolders map[connect.Id]string
}

func newPlatformClientTestPlatform() *platformClientTestPlatform {
	return &platformClientTestPlatform{clientIdHolders: map[connect.Id]string{}}
}

func (self *platformClientTestPlatform) process(name string) sidecar.SidecarPlatform {
	return &platformClientTestProcess{platform: self, name: name}
}

// One process's view of the platform.
type platformClientTestProcess struct {
	platform *platformClientTestPlatform
	name     string
}

func (self *platformClientTestProcess) OpenClient(ctx context.Context, args *sidecar.SidecarPlatformClientArgs) (*sidecar.SidecarPlatformClient, error) {
	platform := self.platform
	clientId := args.ClientId
	func() {
		platform.stateLock.Lock()
		defer platform.stateLock.Unlock()
		platform.clientIdHolders[clientId] = self.name
	}()
	client := connect.NewClient(ctx, clientId, connect.NewNoContractClientOob(), args.ClientSettings)
	return &sidecar.SidecarPlatformClient{
		Client:  client,
		SetAuth: func(auth *connect.ClientAuth) {},
		Connected: func() (bool, <-chan struct{}) {
			platform.stateLock.Lock()
			defer platform.stateLock.Unlock()
			return platform.clientIdHolders[clientId] == self.name, nil
		},
		CloseAndWait: func(ctx context.Context) error {
			client.Cancel()
			platform.stateLock.Lock()
			defer platform.stateLock.Unlock()
			if platform.clientIdHolders[clientId] == self.name {
				delete(platform.clientIdHolders, clientId)
			}
			return nil
		},
	}, nil
}

// A deploy's old and new process of one block on one host, each its own K clients in an EdgeClientPool on its slot's
// source: both connect and stay connected, on a platform that lets one client id be connected from one process
// alone. A third process in the old one's slot, as before QD-873, takes the old one's clients over, which is what
// the slot keeps from a deploy.
func TestPlatformClientSourcesInTwoSlotsBothConnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	api := newPlatformClientTestApi(t, clock)
	platform := newPlatformClientTestPlatform()
	clientCount := 2
	openPool := func(name string, slot int) *sidecar.EdgeClientPool {
		settings := sidecar.DefaultEdgeClientPoolSettings()
		settings.ClientCount = clientCount
		settings.AppVersion = "0.0.0-test"
		pool, err := sidecar.NewEdgeClientPool(ctx, &sidecar.EdgeClientPoolArgs{
			Source:   api.newSlotSource(ctx, slot, clientCount),
			Platform: platform.process(name),
			Settings: settings,
			Clock:    clock,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	oldPool := openPool("old", 0)
	if !oldPool.Ready() {
		t.Fatal("the old process is not connected")
	}
	newPool := openPool("new", 1)
	if !oldPool.Ready() || !newPool.Ready() {
		t.Fatalf("connected: old %d new %d of %d", oldPool.ConnectedCount(), newPool.ConnectedCount(), clientCount)
	}
	for index := range clientCount {
		if oldPool.ClientIds()[index] != sidecar.Id(DeriveEdgeClientId("test", "lb-a.example", "tlsedge", 0, index)) ||
			newPool.ClientIds()[index] != sidecar.Id(DeriveEdgeClientId("test", "lb-a.example", "tlsedge", 1, index)) {
			t.Fatalf("index %d's clients are not their slots'", index)
		}
	}
	sameSlotPool := openPool("same-slot", 0)
	if !sameSlotPool.Ready() || oldPool.Ready() || oldPool.ConnectedCount() != 0 || !newPool.Ready() {
		t.Fatalf("a process in the old one's slot: old %d, new %d, same slot %d connected", oldPool.ConnectedCount(), newPool.ConnectedCount(), sameSlotPool.ConnectedCount())
	}
}

// A process's restart in the same slot asks for and holds the clients it held before, so nothing piles up in
// urnetwork-edge.
func TestPlatformClientSourceRepeatsItsSlotsClientsAcrossRestarts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	api := newPlatformClientTestApi(t, clock)
	clientCount := 3
	first := api.newSlotSource(ctx, 1, clientCount)
	awaitPlatformClientAnswers(t, ctx, first, uint64(clientCount))
	firstClientIds := platformClientIds(t, ctx, first, clientCount)
	first.Close()
	restarted := api.newSlotSource(ctx, 1, clientCount)
	awaitPlatformClientAnswers(t, ctx, restarted, uint64(clientCount))
	restartedClientIds := platformClientIds(t, ctx, restarted, clientCount)
	for index := range clientCount {
		if firstClientIds[index] != restartedClientIds[index] {
			t.Fatalf("index %d's client changed across a restart", index)
		}
		auth, err := restarted.ServiceClient(ctx, index)
		if err != nil {
			t.Fatal(err)
		}
		// a new jwt for the same client
		if auth.ByJwt != fmt.Sprintf("jwt-secret-1-%d-2", index) {
			t.Fatalf("index %d's jwt after a restart", index)
		}
	}
}

func TestPlatformClientSourceRidesAnOutageUntilItsJwtEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	api := newPlatformClientTestApi(t, clock)
	source := api.newSource(ctx, 1)
	awaitPlatformClientAnswers(t, ctx, source, 1)
	api.setFailStatus(http.StatusServiceUnavailable)
	// past the refresh point with api down: the held jwt still answers
	clock.Advance(45 * time.Minute)
	auth, err := source.ServiceClient(ctx, 0)
	if err != nil || auth.ByJwt != "jwt-secret-0-0-1" {
		t.Fatalf("the held jwt did not answer in an outage: %v", err)
	}
	// once it has ended, nothing answers, and the source is not ready
	clock.Advance(15 * time.Minute)
	_, err = source.ServiceClient(ctx, 0)
	if !errors.Is(err, ErrPlatformClientSource) {
		t.Fatalf("an ended jwt answered: %v", err)
	}
	if strings.Contains(err.Error(), api.bearer) || strings.Contains(err.Error(), "jwt-secret") {
		t.Fatalf("the error holds a secret: %v", err)
	}
	if source.Ready() {
		t.Fatal("ready on an ended jwt")
	}
	// api back: the next call asks and answers
	api.setFailStatus(0)
	auth, err = source.ServiceClient(ctx, 0)
	if err != nil || auth.ByJwt != "jwt-secret-0-0-2" || !source.Ready() {
		t.Fatalf("the source did not recover: %v", err)
	}
}

func TestPlatformClientSourceRefusesAnotherClientId(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	api := newPlatformClientTestApi(t, clock)
	source := api.newSource(ctx, 1)
	awaitPlatformClientAnswers(t, ctx, source, 1)
	func() {
		api.stateLock.Lock()
		defer api.stateLock.Unlock()
		api.clientIdOverride = NewId().String()
	}()
	clock.Advance(45 * time.Minute)
	// the answer naming another id is not installed: the held jwt, of the index's own client, still answers
	auth, err := source.ServiceClient(ctx, 0)
	if err != nil || Id(auth.ClientId) != DeriveEdgeClientId("test", "lb-a.example", "tlsedge", 0, 0) || auth.ByJwt != "jwt-secret-0-0-1" {
		t.Fatalf("another client's jwt was installed: %v", err)
	}
}

func TestPlatformClientSourceRefusesWhatItCannotRunOn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	good := func() *PlatformClientSourceArgs {
		return &PlatformClientSourceArgs{ApiUrl: "https://api.edge.example", Block: "tlsedge", Host: "lb-a.example", Bearer: "bearer"}
	}
	mutations := []func(args *PlatformClientSourceArgs){
		func(args *PlatformClientSourceArgs) { args.ApiUrl = "" },
		func(args *PlatformClientSourceArgs) { args.ApiUrl = "api.edge.example" },
		func(args *PlatformClientSourceArgs) { args.ApiUrl = "https://api.edge.example/" },
		func(args *PlatformClientSourceArgs) { args.Block = "" },
		func(args *PlatformClientSourceArgs) { args.Host = "LB A" },
		func(args *PlatformClientSourceArgs) { args.Bearer = "" },
		// a slot api would refuse: at the slot count, or below 0
		func(args *PlatformClientSourceArgs) { args.Slot = 2 },
		func(args *PlatformClientSourceArgs) { args.Slot = -1 },
		func(args *PlatformClientSourceArgs) {
			args.Settings = DefaultPlatformClientSourceSettings()
			args.Settings.ClientCount = 0
		},
		func(args *PlatformClientSourceArgs) {
			args.Settings = DefaultPlatformClientSourceSettings()
			args.Settings.RefreshLifeFraction = 1
		},
		func(args *PlatformClientSourceArgs) {
			args.Settings = DefaultPlatformClientSourceSettings()
			args.Settings.Key.SlotCount = 1
			args.Slot = 1
		},
	}
	for index, mutate := range mutations {
		args := good()
		mutate(args)
		if source, err := NewPlatformClientSource(ctx, args); !errors.Is(err, ErrPlatformClientSource) {
			if source != nil {
				source.Close()
			}
			t.Errorf("mutation %d: %v, want refused", index, err)
		}
	}
	if _, err := NewPlatformClientSource(ctx, nil); !errors.Is(err, ErrPlatformClientSource) {
		t.Errorf("nil args: %v", err)
	}
	// slot 1 runs
	args := good()
	args.Slot = 1
	source, err := NewPlatformClientSource(ctx, args)
	if err != nil {
		t.Fatalf("slot 1: %v", err)
	}
	source.Close()
}

// WARP_SLOT as an edge block reads it (QD-873): a slot below the slot count in one spelling, anything else
// refused; with none set, slot 0 until the source's settings require it, then refused.
func TestLoadEdgeClientSlotReadsOnlyASlotTheKeyAdmits(t *testing.T) {
	settings := DefaultPlatformClientSourceSettings()
	for value, want := range map[string]int{"0": 0, "1": 1} {
		slot, err := parseEdgeClientSlot(value, true, settings)
		if err != nil || slot != want {
			t.Errorf("%q: %d %v, want %d", value, slot, err, want)
		}
	}
	for _, value := range []string{"2", "-1", "+1", "01", " 1", "1 ", "x", "1.0", "0x1"} {
		if slot, err := parseEdgeClientSlot(value, true, settings); !errors.Is(err, ErrPlatformClientSource) {
			t.Errorf("%q: %d %v, want refused", value, slot, err)
		}
	}
	// not set, or set empty: slot 0 while warp's change rolls out
	for _, set := range []bool{false, true} {
		if slot, err := parseEdgeClientSlot("", set, settings); err != nil || slot != 0 {
			t.Errorf("no value (set %t): %d %v, want 0", set, slot, err)
		}
	}
	required := DefaultPlatformClientSourceSettings()
	required.SlotRequired = true
	for _, set := range []bool{false, true} {
		if _, err := parseEdgeClientSlot("", set, required); !errors.Is(err, ErrPlatformClientSource) || !strings.Contains(err.Error(), EdgeClientSlotEnvName) {
			t.Errorf("no value (set %t) where it is required: %v", set, err)
		}
	}
	if slot, err := parseEdgeClientSlot("1", true, required); err != nil || slot != 1 {
		t.Errorf("1 where it is required: %d %v", slot, err)
	}
	// the bound is the key settings' slot count
	wider := DefaultPlatformClientSourceSettings()
	wider.Key.SlotCount = 3
	if slot, err := parseEdgeClientSlot("2", true, wider); err != nil || slot != 2 {
		t.Errorf("2 under a slot count of 3: %d %v", slot, err)
	}
	// and the environment's own value is what LoadEdgeClientSlot reads
	t.Setenv(EdgeClientSlotEnvName, "1")
	if slot, err := LoadEdgeClientSlot(required); err != nil || slot != 1 {
		t.Errorf("WARP_SLOT=1: %d %v", slot, err)
	}
	t.Setenv(EdgeClientSlotEnvName, "2")
	if _, err := LoadEdgeClientSlot(nil); !errors.Is(err, ErrPlatformClientSource) {
		t.Errorf("WARP_SLOT=2: %v", err)
	}
}

// Each client's sockets token (QD-1054) is kept beside its jwt from the same answer, answered by index and by the
// client's id, renewed at half its life with nobody calling, and the jwt with it; a client id the source holds no
// answer for has no token.
func TestPlatformClientSourceKeepsEachSocketsTokenFresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	api := newPlatformClientTestApi(t, clock)
	source := api.newSource(ctx, 2)
	awaitPlatformClientAnswers(t, ctx, source, 2)
	for index := range 2 {
		token, err := source.SocketsToken(ctx, index)
		if err != nil || token != fmt.Sprintf("sockets-secret-0-%d-1", index) {
			t.Fatalf("client %d's sockets token: %v", index, err)
		}
		clientId := DeriveEdgeClientId("test", "lb-a.example", "tlsedge", 0, index)
		byClient, err := source.ClientSocketsToken(ctx, sidecar.Id(clientId))
		if err != nil || byClient != token {
			t.Fatalf("client %d's sockets token by its id: %v", index, err)
		}
	}
	if _, err := source.ClientSocketsToken(ctx, sidecar.Id(NewId())); !errors.Is(err, ErrPlatformClientSource) {
		t.Fatalf("a client the source does not hold: %v", err)
	}
	for _, index := range []int{-1, 2} {
		if _, err := source.SocketsToken(ctx, index); !errors.Is(err, ErrPlatformClientSource) {
			t.Fatalf("index %d: %v", index, err)
		}
	}
	// half the token's life on: each index asked again unprompted, a new token and a new jwt for the same client
	if err := clock.AwaitTimers(ctx, 2); err != nil {
		t.Fatal(err)
	}
	clock.Advance(7*time.Minute + 30*time.Second)
	awaitPlatformClientAnswers(t, ctx, source, 4)
	for index := range 2 {
		token, err := source.SocketsToken(ctx, index)
		if err != nil || token != fmt.Sprintf("sockets-secret-0-%d-2", index) {
			t.Fatalf("client %d's sockets token was not renewed: %v", index, err)
		}
		auth, err := source.ServiceClient(ctx, index)
		if err != nil || auth.ByJwt != fmt.Sprintf("jwt-secret-0-%d-2", index) {
			t.Fatalf("client %d's jwt did not come with its token: %v", index, err)
		}
	}
	if api.askCount(0, 0) != 2 || api.askCount(0, 1) != 2 {
		t.Fatalf("asked %d %d times", api.askCount(0, 0), api.askCount(0, 1))
	}
	if !source.Ready() {
		t.Fatal("not ready on fresh tokens")
	}
}

// An api outage past the refresh point: the held sockets token answers until it ends, then nothing does and the
// source is not ready, while the held jwt still answers until its own end; api back, the next call recovers both.
func TestPlatformClientSourceRidesAnOutageOnItsSocketsToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	api := newPlatformClientTestApi(t, clock)
	source := api.newSource(ctx, 1)
	awaitPlatformClientAnswers(t, ctx, source, 1)
	api.setFailStatus(http.StatusServiceUnavailable)
	clock.Advance(10 * time.Minute)
	token, err := source.SocketsToken(ctx, 0)
	if err != nil || token != "sockets-secret-0-0-1" {
		t.Fatalf("the held token did not answer in an outage: %v", err)
	}
	if !source.Ready() {
		t.Fatal("not ready on a held token that has not ended")
	}
	clock.Advance(5 * time.Minute)
	_, err = source.SocketsToken(ctx, 0)
	if !errors.Is(err, ErrPlatformClientSource) {
		t.Fatalf("an ended token answered: %v", err)
	}
	if strings.Contains(err.Error(), "sockets-secret") || strings.Contains(err.Error(), api.bearer) {
		t.Fatalf("the error holds a secret: %v", err)
	}
	if source.Ready() {
		t.Fatal("ready on an ended sockets token")
	}
	if auth, err := source.ServiceClient(ctx, 0); err != nil || auth.ByJwt != "jwt-secret-0-0-1" {
		t.Fatalf("the held jwt, an hour's, did not answer: %v", err)
	}
	api.setFailStatus(0)
	token, err = source.SocketsToken(ctx, 0)
	if err != nil || token != "sockets-secret-0-0-2" || !source.Ready() {
		t.Fatalf("the source did not recover: %v", err)
	}
}

// An answer with no sockets token is no client's: it is never kept, so nothing answers and the source is not
// ready, and the error names no secret.
func TestPlatformClientSourceRefusesAnAnswerWithNoSocketsToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	api := newPlatformClientTestApi(t, clock)
	func() {
		api.stateLock.Lock()
		defer api.stateLock.Unlock()
		api.noSocketsToken = true
	}()
	source := api.newSource(ctx, 1)
	_, err := source.ServiceClient(ctx, 0)
	if !errors.Is(err, ErrPlatformClientSource) || !strings.Contains(err.Error(), "no sockets token") {
		t.Fatalf("an answer with no sockets token was kept: %v", err)
	}
	if strings.Contains(err.Error(), "jwt-secret") || strings.Contains(err.Error(), api.bearer) {
		t.Fatalf("the error holds a secret: %v", err)
	}
	if _, err := source.SocketsToken(ctx, 0); !errors.Is(err, ErrPlatformClientSource) {
		t.Fatalf("a sockets token answered: %v", err)
	}
	if source.Ready() {
		t.Fatal("ready with no sockets token")
	}
}

// What the edge blocks hand their dialer and their platform listener: a Find presents the first client's sockets
// token, a registration its own client's, never a jwt, each to the sockets url given and on a transport of its own.
func TestPlatformClientSourcePresentsSocketsTokensToSockets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	api := newPlatformClientTestApi(t, clock)
	source := api.newSource(ctx, 2)
	awaitPlatformClientAnswers(t, ctx, source, 2)

	findArgs := source.SocketFindHttpArgs("https://sockets.edge.example")
	if findArgs.SocketsUrl != "https://sockets.edge.example" || findArgs.Transport == nil {
		t.Fatalf("find args %q", findArgs.SocketsUrl)
	}
	bearer, err := findArgs.Bearer(ctx)
	if err != nil || bearer != "sockets-secret-0-0-1" {
		t.Fatalf("a Find's bearer is not the first client's sockets token: %v", err)
	}

	registrationsArgs := source.SocketPlatformRegistrationsArgs("https://sockets.edge.example")
	if registrationsArgs.SocketsUrl != "https://sockets.edge.example" || registrationsArgs.Transport == nil {
		t.Fatalf("registrations args %q", registrationsArgs.SocketsUrl)
	}
	for index := range 2 {
		clientId := sidecar.Id(DeriveEdgeClientId("test", "lb-a.example", "tlsedge", 0, index))
		bearer, err := registrationsArgs.Bearer(ctx, clientId)
		if err != nil || bearer != fmt.Sprintf("sockets-secret-0-%d-1", index) {
			t.Fatalf("client %d's registration bearer is not its sockets token: %v", index, err)
		}
	}
	if _, err := registrationsArgs.Bearer(ctx, sidecar.Id(NewId())); err == nil {
		t.Fatal("a client the block does not hold has a registration bearer")
	}
}
