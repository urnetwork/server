package server

// An edge block's platform clients (TLS-M5-11, QD-812, QD-873, QD-1011, QD-1054): edge.yml admits only the block,
// host, slot and index it names, slots 0 to 3; the derivation is fixed; the four processes of an edge service one
// host can run, its beta and g1 processes and a deploy's new process beside either, in slots 0 to 3 hold disjoint
// clients and all connect their pools, and one slot holds the same clients across restarts; WARP_SLOT reads only
// a slot the key admits; and PlatformClientSource asks api for each index at start, holds each client's
// id for its life, keeps each client's sockets token beside its jwt, refreshes both at the refresh point of the
// shorter-lived, rides an api outage on the jwt and the token it holds until each ends, refuses an answer with no
// sockets token, presents each client's token, never its jwt, to sockets, answers each client's identity seed, the same
// across restarts, and never puts the bearer, a jwt or a token in an error. api is an httptest server and time a fake
// clock: nothing sleeps.

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
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
		// the other warp block's process, and a deploy's new process beside either (QD-1011)
		{Block: "tlsedge", Host: "lb-b.example", Slot: 1, Index: 3},
		{Block: "tlsedge", Host: "lb-b.example", Slot: 2, Index: 3},
		{Block: "tlsedge", Host: "lb-b.example", Slot: 3, Index: 0},
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
		// a slot at or past the slot count, 4
		{Block: "tlsedge", Host: "lb-a.example", Slot: 4, Index: 0},
		{Block: "tlsedge", Host: "lb-a.example", Slot: 5, Index: 0},
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
	if err := config.Admits(&EdgeClientMintArgs{Block: "tlsedge", Host: "lb-a.example", Slot: 4, Index: 0}); err == nil || !strings.HasSuffix(err.Error(), ": slot") {
		t.Errorf("slot 4's refusal: %v", err)
	}
	keySettings := DefaultEdgeClientKeySettings()
	keySettings.SlotCount = 5
	wider, err := ParseEdgeClientsConfig([]byte("platform_url: wss://connect.edge.example\nblocks: {tlsedge: {clients: 4, hosts: [lb-a.example]}}\n"), keySettings)
	if err != nil {
		t.Fatal(err)
	}
	if err := wider.Admits(&EdgeClientMintArgs{Block: "tlsedge", Host: "lb-a.example", Slot: 4, Index: 0}); err != nil {
		t.Errorf("slot 4 under a slot count of 5: %v", err)
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
	// the slots QD-1011 added, under the same derivation
	slotTwoId := DeriveEdgeClientId("main", "lb-a.example", "tlsedge", 2, 0)
	if got := hex.EncodeToString(slotTwoId[:]); got != "b90cf0a0a21879f23b500b72a31df692" {
		t.Fatalf("slot 2's client id %s", got)
	}
	slotThreeId := DeriveEdgeClientId("main", "lb-a.example", "tlsedge", 3, 0)
	if got := hex.EncodeToString(slotThreeId[:]); got != "539eca59b668c48dd39e08b085d73c00" {
		t.Fatalf("slot 3's client id %s", got)
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
		DeriveEdgeClientId("main", "lb-a.example", "tlsedge", 2, 0),
		DeriveEdgeClientId("main", "lb-a.example", "tlsedge", 2, 1),
		DeriveEdgeClientId("main", "lb-a.example", "tlsedge", 3, 0),
		DeriveEdgeClientId("main", "lb-a.example", "tlsedge", 3, 1),
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
	// The fake mint advances each stable slot's authentication time under its lock.
	slotAuthTimes map[[2]int]int64
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
		slotAuthTimes:      map[[2]int]int64{},
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
	self.slotAuthTimes[key] = max(self.clock.Now().UnixMilli(), self.slotAuthTimes[key]+1)
	clientId := DeriveEdgeClientId("test", args.Host, args.Block, args.Slot, args.Index).String()
	if self.clientIdOverride != "" {
		clientId = self.clientIdOverride
	}
	result := &EdgeClientMintResult{
		ClientId:                     clientId,
		AuthTimeMillis:               self.slotAuthTimes[key],
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

// The processes of one edge service on one host (QD-873, QD-1011): its beta and g1 processes side by side and a
// deploy's new process beside either, at most four, in slots 0 to 3 ask for, hold and are ready on pairwise
// disjoint clients, each its own K, and one ending costs the others nothing.
func TestPlatformClientSourcesInFourSlotsHoldDisjointClients(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	api := newPlatformClientTestApi(t, clock)
	clientCount := 4
	slotCount := DefaultEdgeClientKeySettings().SlotCount
	if slotCount != 4 {
		t.Fatalf("the slot count is %d, want 4 (QD-1011), the same as warpctl's DeploySlotSettings.SlotCount", slotCount)
	}
	// beta's and g1's processes, then a deploy's new process beside each, each starting while the others hold
	// their clients
	sources := []*PlatformClientSource{}
	for slot := range slotCount {
		source := api.newSlotSource(ctx, slot, clientCount)
		awaitPlatformClientAnswers(t, ctx, source, uint64(clientCount))
		sources = append(sources, source)
	}
	seen := map[Id]int{}
	for slot, source := range sources {
		if !source.Ready() {
			t.Fatalf("slot %d is not ready", slot)
		}
		for index, clientId := range platformClientIds(t, ctx, source, clientCount) {
			if clientId != DeriveEdgeClientId("test", "lb-a.example", "tlsedge", slot, index) {
				t.Fatalf("slot %d index %d holds another slot's client", slot, index)
			}
			if heldSlot, ok := seen[clientId]; ok {
				t.Fatalf("slot %d index %d holds slot %d's client", slot, index, heldSlot)
			}
			seen[clientId] = slot
		}
	}
	// each asked only for its own: one ask a slot and index
	for slot := range slotCount {
		for index := range clientCount {
			if api.askCount(slot, index) != 1 {
				t.Fatalf("slot %d index %d asked %d times", slot, index, api.askCount(slot, index))
			}
		}
	}
	// a drained process ending costs the others nothing
	sources[0].Close()
	if sources[0].Ready() {
		t.Fatal("a closed source is ready")
	}
	for slot := 1; slot < slotCount; slot += 1 {
		if !sources[slot].Ready() {
			t.Fatalf("after slot 0 ended, slot %d is not ready", slot)
		}
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

// The four processes one edge service can run on one host, beta's and g1's side by side and a deploy's new process
// beside each, each its own K clients in an EdgeClientPool on its slot's source: all connect and stay connected,
// on a platform that lets one client id be connected from one process alone. A fifth process in a held slot, as
// beta's and g1's both were in slot 0 before QD-1011, takes that slot's clients over and no other's, which is what
// a slot of its own keeps from each process.
func TestPlatformClientSourcesInFourSlotsAllConnect(t *testing.T) {
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
	names := []string{"beta", "g1", "beta-new", "g1-new"}
	pools := []*sidecar.EdgeClientPool{}
	for slot, name := range names {
		pool := openPool(name, slot)
		pools = append(pools, pool)
		for startedSlot, startedPool := range pools {
			if !startedPool.Ready() {
				t.Fatalf("after %s started, %s holds %d of %d clients", name, names[startedSlot], startedPool.ConnectedCount(), clientCount)
			}
		}
	}
	for slot, pool := range pools {
		for index := range clientCount {
			if pool.ClientIds()[index] != sidecar.Id(DeriveEdgeClientId("test", "lb-a.example", "tlsedge", slot, index)) {
				t.Fatalf("%s's client %d is not its slot's", names[slot], index)
			}
		}
	}
	sameSlotPool := openPool("same-slot", 1)
	if !sameSlotPool.Ready() || pools[1].Ready() || pools[1].ConnectedCount() != 0 {
		t.Fatalf("a process in g1's slot: g1 %d, same slot %d connected", pools[1].ConnectedCount(), sameSlotPool.ConnectedCount())
	}
	for _, slot := range []int{0, 2, 3} {
		if !pools[slot].Ready() {
			t.Fatalf("a process in g1's slot took %s's clients: %d connected", names[slot], pools[slot].ConnectedCount())
		}
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

func TestPlatformClientSourceReservesRangesAcrossSlotRestart(t *testing.T) {
	clock := sidecartest.NewFakeClock(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))
	api := newPlatformClientTestApi(t, clock)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var previous uint64
	for range 2 {
		source := api.newSource(ctx, 1)
		auth, err := source.ServiceClient(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			idRange, err := source.ReserveStreamIdRange(ctx, 0, sidecar.Id(auth.ClientId))
			if err != nil || idRange <= previous {
				t.Fatalf("range %d after %d: %v", idRange, previous, err)
			}
			previous = idRange
		}
		source.Close()
	}
}

// Each client's identity seed is the same across a restart of its slot on the same bearer, as its id is, so the pool
// presents the same connect identity (QD tlsedge-restart-contracts-edge-identity); it differs between clients and on
// another bearer, is the file header's derivation, and is answered only for the client the index's answers named.
func TestPlatformClientSourceKeepsEachClientsIdentitySeedAcrossRestarts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	api := newPlatformClientTestApi(t, clock)
	clientCount := 2
	seeds := func() ([]Id, [][]byte) {
		source := api.newSource(ctx, clientCount)
		defer source.Close()
		awaitPlatformClientAnswers(t, ctx, source, uint64(clientCount))
		clientIds := platformClientIds(t, ctx, source, clientCount)
		indexSeeds := [][]byte{}
		for index, clientId := range clientIds {
			seed, err := source.EdgeClientIdentitySeed(ctx, index, sidecar.Id(clientId))
			if err != nil || len(seed) != sidecar.EdgeClientIdentitySeedByteCount {
				t.Fatalf("index %d's seed: %d bytes, %v", index, len(seed), err)
			}
			indexSeeds = append(indexSeeds, seed)
			// another client's id is answered nothing
			if _, err := source.EdgeClientIdentitySeed(ctx, index, sidecar.Id(NewId())); !errors.Is(err, ErrPlatformClientSource) {
				t.Errorf("index %d answered another client's seed: %v", index, err)
			}
		}
		if _, err := source.EdgeClientIdentitySeed(ctx, clientCount, sidecar.Id(clientIds[0])); !errors.Is(err, ErrPlatformClientSource) {
			t.Errorf("an index past the source's answered a seed: %v", err)
		}
		return clientIds, indexSeeds
	}
	firstClientIds, firstSeeds := seeds()
	restartedClientIds, restartedSeeds := seeds()
	for index := range clientCount {
		if firstClientIds[index] != restartedClientIds[index] || string(firstSeeds[index]) != string(restartedSeeds[index]) {
			t.Errorf("index %d's seed changed across a restart", index)
		}
		want, err := hkdf.Key(sha256.New, []byte(api.bearer), nil, "urnetwork-edge-client-identity-v1\x00"+string(firstClientIds[index][:]), 32)
		if err != nil || string(firstSeeds[index]) != string(want) {
			t.Errorf("index %d's seed is not the header's derivation", index)
		}
		otherBearer, err := DeriveEdgeClientIdentitySeed("another-edge-bearer", sidecar.Id(firstClientIds[index]))
		if err != nil || string(otherBearer) == string(firstSeeds[index]) {
			t.Errorf("index %d's seed is the same on another bearer", index)
		}
	}
	if string(firstSeeds[0]) == string(firstSeeds[1]) {
		t.Error("two clients share a seed")
	}
	if _, err := DeriveEdgeClientIdentitySeed("", sidecar.Id(firstClientIds[0])); !errors.Is(err, ErrPlatformClientSource) {
		t.Errorf("no bearer derived a seed: %v", err)
	}
	if _, err := DeriveEdgeClientIdentitySeed(api.bearer, sidecar.Id{}); !errors.Is(err, ErrPlatformClientSource) {
		t.Errorf("no client derived a seed: %v", err)
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
		// a slot api would refuse: at the slot count, 4, or below 0
		func(args *PlatformClientSourceArgs) { args.Slot = 4 },
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
	// slots 1 to 3 run, as slot 0 does
	for slot := 1; slot < 4; slot += 1 {
		args := good()
		args.Slot = slot
		source, err := NewPlatformClientSource(ctx, args)
		if err != nil {
			t.Fatalf("slot %d: %v", slot, err)
		}
		source.Close()
	}
}

// WARP_SLOT as an edge block reads it (QD-873, QD-1011): a slot below the slot count, 4, in one spelling, anything
// else refused; with none set, slot 0 until the source's settings require it, then refused.
func TestLoadEdgeClientSlotReadsOnlyASlotTheKeyAdmits(t *testing.T) {
	settings := DefaultPlatformClientSourceSettings()
	for value, want := range map[string]int{"0": 0, "1": 1, "2": 2, "3": 3} {
		slot, err := parseEdgeClientSlot(value, true, settings)
		if err != nil || slot != want {
			t.Errorf("%q: %d %v, want %d", value, slot, err, want)
		}
	}
	for _, value := range []string{"4", "5", "-1", "+1", "01", " 1", "1 ", "x", "1.0", "0x1"} {
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
	wider.Key.SlotCount = 5
	if slot, err := parseEdgeClientSlot("4", true, wider); err != nil || slot != 4 {
		t.Errorf("4 under a slot count of 5: %d %v", slot, err)
	}
	// and the environment's own value is what LoadEdgeClientSlot reads
	t.Setenv(EdgeClientSlotEnvName, "3")
	if slot, err := LoadEdgeClientSlot(required); err != nil || slot != 3 {
		t.Errorf("WARP_SLOT=3: %d %v", slot, err)
	}
	t.Setenv(EdgeClientSlotEnvName, "4")
	if _, err := LoadEdgeClientSlot(nil); !errors.Is(err, ErrPlatformClientSource) {
		t.Errorf("WARP_SLOT=4: %v", err)
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
