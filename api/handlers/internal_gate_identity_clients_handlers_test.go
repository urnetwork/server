package handlers

// api's sidecar identity clients route (plan-KEYS-hint-addressing, QD-146, QD-1171): only gkm's and mxedge's bearers
// reach it, and no other service's, client's or missing bearer does, the lookup never running for them; the answer
// holds each sidecar the lookup resolved in the request's order and omits the rest; a request beyond the format, no id,
// a zero id, more than SidecarIdMaxCount, a field beyond the format's or a second value, is refused 400 with no lookup;
// a lookup that cannot run is 503 with Retry-After; and server.SidecarIdentityClientsResolver, the hosts' end, sends
// the format alone, keeps what it learned and asks api only for what it does not keep.
// gkm/hint_resolver_db_test.go has the route over MG1's tables with a hint landing on Redis.

import (
	"context"
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
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// The route on a verify file of gkm's, mxedge's and evs's bearers, with a lookup that records each call and answers
// from a map the test fills.
type identityClientsTestWorld struct {
	t      testing.TB
	server *httptest.Server
	gkm    string
	mxedge string
	evs    string

	stateLock sync.Mutex
	clients   map[server.Id]*model.SidecarCurrentClient
	err       error
	lookups   [][]server.Id
}

func newIdentityClientsTestWorld(t *testing.T) *identityClientsTestWorld {
	gkm, gkmDigest := newInternalGateTestBearer(t)
	mxedge, mxedgeDigest := newInternalGateTestBearer(t)
	evs, evsDigest := newInternalGateTestBearer(t)
	verifier, err := server.NewInternalGateVerifier([]byte(
		"gkm: {class: gkm, sha256: [" + gkmDigest + "]}\n" +
			"mxedge: {class: mxedge, sha256: [" + mxedgeDigest + "]}\n" +
			"evs: {class: evs, sha256: [" + evsDigest + "]}\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	self := &identityClientsTestWorld{t: t, gkm: gkm, mxedge: mxedge, evs: evs, clients: map[server.Id]*model.SidecarCurrentClient{}}
	handlers := NewInternalGateIdentityClientsHandlers(&InternalGateIdentityClientsDeps{
		Verifier: verifier,
		Lookup: func(ctx context.Context, sidecarIds []server.Id) (map[server.Id]*model.SidecarCurrentClient, error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.lookups = append(self.lookups, append([]server.Id{}, sidecarIds...))
			if self.err != nil {
				return nil, self.err
			}
			answer := map[server.Id]*model.SidecarCurrentClient{}
			for _, sidecarId := range sidecarIds {
				if client, ok := self.clients[sidecarId]; ok {
					answer[sidecarId] = client
				}
			}
			return answer, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	self.server = httptest.NewServer(router.NewRouter(ctx, handlers.Routes()))
	t.Cleanup(self.server.Close)
	return self
}

// Binds a new sidecar to a new client at generation, as the lookup will answer it.
func (self *identityClientsTestWorld) bind(generation int64) (server.Id, server.Id) {
	sidecarId, clientId := server.NewId(), server.NewId()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clients[sidecarId] = &model.SidecarCurrentClient{ClientId: clientId, Generation: generation}
	return sidecarId, clientId
}

// How many lookups ran.
func (self *identityClientsTestWorld) lookupCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.lookups)
}

// One post with a bearer, none for empty, and a raw body: its status, answer and Retry-After.
func (self *identityClientsTestWorld) post(bearer string, body string) (int, *server.SidecarIdentityClientsGateResult, string) {
	self.t.Helper()
	request, err := http.NewRequest(http.MethodPost, self.server.URL+server.SidecarIdentityClientsGatePath, strings.NewReader(body))
	if err != nil {
		self.t.Fatal(err)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := self.server.Client().Do(request)
	if err != nil {
		self.t.Fatal(err)
	}
	defer response.Body.Close()
	result := &server.SidecarIdentityClientsGateResult{}
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(result); err != nil {
			self.t.Fatal(err)
		}
	}
	return response.StatusCode, result, response.Header.Get("Retry-After")
}

// A body naming sidecarIds.
func identityClientsTestBody(t testing.TB, sidecarIds ...server.Id) string {
	t.Helper()
	body, err := json.Marshal(&server.SidecarIdentityClientsGateArgs{SidecarIds: sidecarIds})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestInternalGateIdentityClientsTakesOnlyGkmAndMxedge(t *testing.T) {
	world := newIdentityClientsTestWorld(t)
	sidecarId, _ := world.bind(1)
	body := identityClientsTestBody(t, sidecarId)
	for _, bearer := range []string{"", world.evs, "not-a-bearer"} {
		if status, _, _ := world.post(bearer, body); status != http.StatusForbidden {
			t.Errorf("a bearer of no class the route takes: %d", status)
		}
	}
	if count := world.lookupCount(); count != 0 {
		t.Fatalf("%d lookups ran for refused callers", count)
	}
	for _, bearer := range []string{world.gkm, world.mxedge} {
		if status, result, _ := world.post(bearer, body); status != http.StatusOK || len(result.Clients) != 1 {
			t.Errorf("gkm's or mxedge's bearer: %d, %d clients", status, len(result.Clients))
		}
	}
}

func TestInternalGateIdentityClientsAnswersInOrderOmittingTheRest(t *testing.T) {
	world := newIdentityClientsTestWorld(t)
	first, firstClient := world.bind(3)
	second, secondClient := world.bind(1)
	unknown := server.NewId()
	status, result, _ := world.post(world.gkm, identityClientsTestBody(t, second, unknown, first, second))
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if len(result.Clients) != 2 {
		t.Fatalf("%d clients, want the two api resolved, each once", len(result.Clients))
	}
	if result.Clients[0].SidecarId != second || result.Clients[0].ClientId != secondClient || result.Clients[0].Generation != 1 {
		t.Errorf("the first answer: %+v", result.Clients[0])
	}
	if result.Clients[1].SidecarId != first || result.Clients[1].ClientId != firstClient || result.Clients[1].Generation != 3 {
		t.Errorf("the second answer: %+v", result.Clients[1])
	}
	// none resolved is an empty list, not a refusal
	if status, result, _ := world.post(world.gkm, identityClientsTestBody(t, unknown)); status != http.StatusOK || result.Clients == nil || len(result.Clients) != 0 {
		t.Errorf("an unknown sidecar alone: %d %+v", status, result.Clients)
	}
}

func TestInternalGateIdentityClientsRefusesWhatTheFormatDoesNotTake(t *testing.T) {
	world := newIdentityClientsTestWorld(t)
	sidecarId, _ := world.bind(1)
	tooMany := make([]server.Id, server.DefaultSidecarIdentityClientsGateFormatSettings().SidecarIdMaxCount+1)
	for index := range tooMany {
		tooMany[index] = server.NewId()
	}
	for name, body := range map[string]string{
		"no id":           `{"sidecar_ids":[]}`,
		"no field":        `{}`,
		"a zero id":       identityClientsTestBody(t, sidecarId, server.Id{}),
		"too many ids":    identityClientsTestBody(t, tooMany...),
		"another field":   fmt.Sprintf(`{"sidecar_ids":[%q],"network_id":%q}`, sidecarId.String(), server.NewId().String()),
		"a second value":  identityClientsTestBody(t, sidecarId) + identityClientsTestBody(t, sidecarId),
		"no json at all":  `sidecar_ids`,
		"an id not an id": `{"sidecar_ids":["sidecar"]}`,
	} {
		if status, _, _ := world.post(world.gkm, body); status != http.StatusBadRequest {
			t.Errorf("%s: %d", name, status)
		}
	}
	if count := world.lookupCount(); count != 0 {
		t.Fatalf("%d lookups ran for refused requests", count)
	}
	// the format's bound itself is taken
	atBound := tooMany[:len(tooMany)-1]
	if status, _, _ := world.post(world.gkm, identityClientsTestBody(t, atBound...)); status != http.StatusOK {
		t.Fatalf("the format's bound: %d", status)
	}
}

func TestInternalGateIdentityClientsUnavailableAsksAgainLater(t *testing.T) {
	world := newIdentityClientsTestWorld(t)
	sidecarId, _ := world.bind(1)
	for _, err := range []error{model.ErrSidecarIdentityClientsUnlinked, errors.New("the store did not answer")} {
		func() {
			world.stateLock.Lock()
			defer world.stateLock.Unlock()
			world.err = err
		}()
		status, _, retryAfter := world.post(world.gkm, identityClientsTestBody(t, sidecarId))
		if status != http.StatusServiceUnavailable || retryAfter != "5" {
			t.Errorf("%v: %d, Retry-After %q", err, status, retryAfter)
		}
	}
}

// The resolver over the route: what it keeps answers from memory until CacheTimeout, an absence included; what it
// does not keep goes to api in calls of at most the format's count; a refusal is an error that keeps nothing; and
// past CacheMaxCount the oldest kept go first.
func TestSidecarIdentityClientsResolverKeepsAndBatches(t *testing.T) {
	world := newIdentityClientsTestWorld(t)
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	settings := server.DefaultSidecarIdentityClientsResolverSettings()
	settings.Format = &server.SidecarIdentityClientsGateFormatSettings{SidecarIdMaxCount: 2}
	settings.CacheMaxCount = 4
	resolver, err := server.NewSidecarIdentityClientsResolver(&server.SidecarIdentityClientsResolverArgs{
		ApiUrl:     world.server.URL,
		Bearer:     world.gkm,
		Settings:   settings,
		HttpClient: world.server.Client(),
		Clock:      clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, aClient := world.bind(1)
	b, bClient := world.bind(2)
	c, cClient := world.bind(1)
	unknown := server.NewId()
	ids := []connect.Id{connect.Id(a), connect.Id(b), connect.Id(c), connect.Id(unknown)}
	clientIds, err := resolver.PushClientIds(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(clientIds) != 3 || clientIds[connect.Id(a)] != connect.Id(aClient) || clientIds[connect.Id(b)] != connect.Id(bClient) ||
		clientIds[connect.Id(c)] != connect.Id(cClient) {
		t.Fatalf("resolved %v", clientIds)
	}
	if count := world.lookupCount(); count != 2 {
		t.Fatalf("%d calls for four ids at two a call", count)
	}
	// kept: no call, the unknown one's absence included
	if again, err := resolver.PushClientIds(t.Context(), ids); err != nil || len(again) != 3 || world.lookupCount() != 2 {
		t.Fatalf("a repeat within the timeout: %v, %d answers, %d calls", err, len(again), world.lookupCount())
	}
	// past the timeout api is asked again, and a client generation that moved is seen
	clock.Advance(settings.CacheTimeout)
	_, aMoved := world.bind(2)
	func() {
		world.stateLock.Lock()
		defer world.stateLock.Unlock()
		world.clients[a] = &model.SidecarCurrentClient{ClientId: aMoved, Generation: 2}
	}()
	if moved, err := resolver.PushClientIds(t.Context(), []connect.Id{connect.Id(a)}); err != nil || moved[connect.Id(a)] != connect.Id(aMoved) || world.lookupCount() != 3 {
		t.Fatalf("past the timeout: %v %v, %d calls", err, moved, world.lookupCount())
	}
	// a refused call is an error and keeps nothing it did not learn
	func() {
		world.stateLock.Lock()
		defer world.stateLock.Unlock()
		world.err = errors.New("the store did not answer")
	}()
	fresh, _ := world.bind(1)
	if _, err := resolver.PushClientIds(t.Context(), []connect.Id{connect.Id(fresh)}); !errors.Is(err, server.ErrSidecarIdentityClientsGate) {
		t.Fatalf("a 503: %v", err)
	}
	if kept := resolver.KeptCount(); settings.CacheMaxCount < kept {
		t.Fatalf("%d kept past the bound %d", kept, settings.CacheMaxCount)
	}
	// past CacheMaxCount the oldest go: four more ids push the first four out
	func() {
		world.stateLock.Lock()
		defer world.stateLock.Unlock()
		world.err = nil
	}()
	more := []connect.Id{}
	for range 4 {
		sidecarId, _ := world.bind(1)
		more = append(more, connect.Id(sidecarId))
	}
	if _, err := resolver.PushClientIds(t.Context(), more); err != nil {
		t.Fatal(err)
	}
	if kept := resolver.KeptCount(); kept != settings.CacheMaxCount {
		t.Fatalf("%d kept, want the bound %d", kept, settings.CacheMaxCount)
	}
	calls := world.lookupCount()
	if _, err := resolver.PushClientIds(t.Context(), more); err != nil || world.lookupCount() != calls {
		t.Fatalf("the newest kept are asked again: %v, %d calls after %d", err, world.lookupCount(), calls)
	}
	if _, err := resolver.PushClientIds(t.Context(), []connect.Id{connect.Id(b)}); err != nil || world.lookupCount() != calls+1 {
		t.Fatalf("an evicted sidecar is not asked again: %v, %d calls after %d", err, world.lookupCount(), calls)
	}
}

// A resolver refuses settings and an api url it cannot use, and never sends its bearer anywhere but api's own route.
func TestSidecarIdentityClientsResolverRefusesWhatItCannotUse(t *testing.T) {
	for name, args := range map[string]*server.SidecarIdentityClientsResolverArgs{
		"no bearer":        {ApiUrl: "https://api.example"},
		"no url":           {Bearer: "bearer"},
		"a final slash":    {ApiUrl: "https://api.example/", Bearer: "bearer"},
		"another scheme":   {ApiUrl: "ftp://api.example", Bearer: "bearer"},
		"no cache":         {ApiUrl: "https://api.example", Bearer: "bearer", Settings: &server.SidecarIdentityClientsResolverSettings{RequestTimeout: time.Second, AnswerMaxByteCount: 1}},
		"a zero call size": {ApiUrl: "https://api.example", Bearer: "bearer", Settings: &server.SidecarIdentityClientsResolverSettings{RequestTimeout: time.Second, AnswerMaxByteCount: 1, CacheMaxCount: 1, Format: &server.SidecarIdentityClientsGateFormatSettings{}}},
	} {
		if _, err := server.NewSidecarIdentityClientsResolver(args); !errors.Is(err, server.ErrSidecarIdentityClientsGate) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// an answer naming a sidecar the call did not ask about is no answer
	stray := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(&server.SidecarIdentityClientsGateResult{Clients: []*server.SidecarIdentityClientsGateClient{
			{SidecarId: server.NewId(), ClientId: server.NewId(), Generation: 1},
		}})
	}))
	defer stray.Close()
	resolver, err := server.NewSidecarIdentityClientsResolver(&server.SidecarIdentityClientsResolverArgs{ApiUrl: stray.URL, Bearer: "bearer", HttpClient: stray.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.PushClientIds(t.Context(), []connect.Id{connect.NewId()}); !errors.Is(err, server.ErrSidecarIdentityClientsGate) {
		t.Fatalf("a stray answer: %v", err)
	}
}
