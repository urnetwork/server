package server

// An edge block's platform clients (TLS-M5-11, QD-812): edge.yml admits only the block, host and index it names;
// the derivation is fixed; and PlatformClientSource asks api for each index at start, holds each client's id for
// its life, refreshes each jwt at its refresh point, rides an api outage on the jwt it holds until that ends, and
// never puts the bearer or a jwt in an error. api is an httptest server and time a fake clock: nothing sleeps.

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
		{Block: "tlsedge", Host: "lb-a.example", Index: 0},
		{Block: "tlsedge", Host: "lb-b.example", Index: 3},
		{Block: "smtpedge", Host: "lb-a.example", Index: 0},
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
	// the derivation's vector: a change here is a new set of clients in urnetwork-edge
	// computed apart from this code: the first 16 bytes of SHA-256 over the label and the four parts, each but
	// the last followed by 0x00
	clientId := DeriveEdgeClientId("main", "tlsedge", "lb-a.example", 0)
	if got := hex.EncodeToString(clientId[:]); got != "957f8344e07e79deeec2d8c6687acec6" {
		t.Fatalf("client id %s", got)
	}
	deviceId := DeriveEdgeDeviceId("main", "tlsedge", "lb-a.example", 0)
	if got := hex.EncodeToString(deviceId[:]); got != "acfbed2b00cb8945a74ea8166883c5a8" {
		t.Fatalf("device id %s", got)
	}
	again := DeriveEdgeClientId("main", "tlsedge", "lb-a.example", 0)
	if clientId != again || clientId == (Id{}) {
		t.Fatalf("the derivation is not a function: %s %s", clientId, again)
	}
	others := []Id{
		DeriveEdgeClientId("canary", "tlsedge", "lb-a.example", 0),
		DeriveEdgeClientId("main", "smtpedge", "lb-a.example", 0),
		DeriveEdgeClientId("main", "tlsedge", "lb-b.example", 0),
		DeriveEdgeClientId("main", "tlsedge", "lb-a.example", 1),
		DeriveEdgeDeviceId("main", "tlsedge", "lb-a.example", 0),
		// no part runs into the next
		DeriveEdgeClientId("main", "tlsedgelb", "-a.example", 0),
	}
	seen := map[Id]bool{clientId: true}
	for index, other := range others {
		if seen[other] {
			t.Errorf("derivation %d collides", index)
		}
		seen[other] = true
	}
}

// api's edge client route as a test stands it up: a jwt per ask, each ending life after the ask on the clock.
type platformClientTestApi struct {
	t      testing.TB
	clock  *sidecartest.FakeClock
	bearer string
	life   time.Duration
	server *httptest.Server

	stateLock sync.Mutex
	// index -> asks answered
	indexAskCounts map[int]int
	// while not 0, every ask is answered with this status
	failStatus int
	// while set, every answer names this client id for every index
	clientIdOverride string
}

func newPlatformClientTestApi(t testing.TB, clock *sidecartest.FakeClock) *platformClientTestApi {
	self := &platformClientTestApi{
		t:              t,
		clock:          clock,
		bearer:         "edge-bearer-secret",
		life:           time.Hour,
		indexAskCounts: map[int]int{},
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
	if err := json.NewDecoder(r.Body).Decode(args); err != nil || args.Block != "tlsedge" || args.Host != "lb-a.example" {
		http.Error(w, "Forbidden.", http.StatusForbidden)
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.failStatus != 0 {
		http.Error(w, "Unavailable.", self.failStatus)
		return
	}
	self.indexAskCounts[args.Index] += 1
	clientId := DeriveEdgeClientId("test", args.Block, args.Host, args.Index).String()
	if self.clientIdOverride != "" {
		clientId = self.clientIdOverride
	}
	json.NewEncoder(w).Encode(&EdgeClientMintResult{
		ClientId:         clientId,
		ByClientJwt:      fmt.Sprintf("jwt-secret-%d-%d", args.Index, self.indexAskCounts[args.Index]),
		ExpireTimeMillis: self.clock.Now().Add(self.life).UnixMilli(),
		PlatformUrl:      "wss://connect.edge.example",
	})
}

func (self *platformClientTestApi) askCount(index int) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.indexAskCounts[index]
}

func (self *platformClientTestApi) setFailStatus(status int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failStatus = status
}

func (self *platformClientTestApi) newSource(ctx context.Context, clientCount int) *PlatformClientSource {
	settings := DefaultPlatformClientSourceSettings()
	settings.ClientCount = clientCount
	source, err := NewPlatformClientSource(ctx, &PlatformClientSourceArgs{
		ApiUrl:   self.server.URL,
		Block:    "tlsedge",
		Host:     "lb-a.example",
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

func TestPlatformClientSourceAsksEachIndexAndRefreshesBeforeItLapses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	api := newPlatformClientTestApi(t, clock)
	source := api.newSource(ctx, 2)
	if source.Ready() && api.askCount(0) == 0 {
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
		wantClientId := DeriveEdgeClientId("test", "tlsedge", "lb-a.example", index)
		if Id(auth.ClientId) != wantClientId || auth.ByJwt != fmt.Sprintf("jwt-secret-%d-1", index) ||
			auth.ApiUrl != api.server.URL || auth.PlatformUrl != "wss://connect.edge.example" {
			t.Fatalf("client %d: %+v", index, auth.ClientId)
		}
		first = append(first, auth.ByJwt)
	}
	// before the refresh point a call costs api nothing
	if api.askCount(0) != 1 || api.askCount(1) != 1 {
		t.Fatalf("asked %d %d times", api.askCount(0), api.askCount(1))
	}
	// at half the jwt's life each index is asked again, with nobody calling
	if err := clock.AwaitTimers(ctx, 2); err != nil {
		t.Fatal(err)
	}
	clock.Advance(30 * time.Minute)
	awaitPlatformClientAnswers(t, ctx, source, 4)
	for index := range 2 {
		auth, err := source.ServiceClient(ctx, index)
		if err != nil {
			t.Fatal(err)
		}
		if auth.ByJwt == first[index] || auth.ByJwt != fmt.Sprintf("jwt-secret-%d-2", index) {
			t.Fatalf("client %d was not refreshed", index)
		}
		if Id(auth.ClientId) != DeriveEdgeClientId("test", "tlsedge", "lb-a.example", index) {
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
	if err != nil || auth.ByJwt != "jwt-secret-0-1" {
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
	if err != nil || auth.ByJwt != "jwt-secret-0-2" || !source.Ready() {
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
	if err != nil || Id(auth.ClientId) != DeriveEdgeClientId("test", "tlsedge", "lb-a.example", 0) || auth.ByJwt != "jwt-secret-0-1" {
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
		func(args *PlatformClientSourceArgs) {
			args.Settings = DefaultPlatformClientSourceSettings()
			args.Settings.ClientCount = 0
		},
		func(args *PlatformClientSourceArgs) {
			args.Settings = DefaultPlatformClientSourceSettings()
			args.Settings.RefreshLifeFraction = 1
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
}
