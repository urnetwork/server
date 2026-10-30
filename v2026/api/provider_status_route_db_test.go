package api

// GET /network/provider-status end to end through the router the API serves,
// against the database and redis, with real tokens: the client tokens are
// minted with POST /network/auth-client by the network's root token, as the
// apps' clients are. The query (client_id, format) answers one client of the
// caller's network or the compact form, every client id the caller may not
// read gets one answer, every 200 carries an ETag of its body scoped to the
// caller's network and the query and is answered 304 when If-None-Match names
// it, the network's answer counts its provider clients (total_clients), every
// status codes its reason (reason_code), and the answer without a query is the
// one before but for those additive fields. The appearance histograms are read
// at a pinned clock, so two answers of one test read one window. Run with
// `cd server/api && ../test.sh -run ProviderStatusRoute`.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

// A network with three provider clients and one client that is not a
// provider, each with its own client token, and another network with a
// provider client.
type providerStatusRouteFixture struct {
	ctx    context.Context
	router *router.Router

	networkId server.Id
	rootToken string
	// the caller: the last provider client minted
	callerClientId server.Id
	callerToken    string
	// every provider client of the network, the caller's included, in client
	// id order
	providerClientIds []server.Id
	idleClientId      server.Id
	idleToken         string

	otherNetworkId server.Id
	otherRootToken string
	otherClientId  server.Id
}

// The provide keys of a fixture's provider clients.
var providerStatusRouteProvideKeys = map[model.ProvideMode][]byte{
	model.ProvideModePublic:  bytes.Repeat([]byte{1}, 32),
	model.ProvideModeNetwork: bytes.Repeat([]byte{2}, 32),
}

// A new network and its root token.
func newProviderStatusRouteNetwork(ctx context.Context) (server.Id, string) {
	networkId := server.NewId()
	userId := server.NewId()
	networkName := fmt.Sprintf("pstatus%s", strings.ReplaceAll(networkId.String(), "-", "")[:12])
	model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
	return networkId, session.NewByJwt(networkId, userId, networkName, false, false).Testing_Sign()
}

// Pins the clock the answers read their appearance histograms at to the
// current time, for the rest of the test, and returns it.
func pinProviderStatusRouteClock(t testing.TB) time.Time {
	t.Helper()
	now := server.NowUtc()
	restore := controller.Testing_SetProviderStatusNow(func() time.Time { return now })
	t.Cleanup(restore)
	return now
}

func newProviderStatusRouteFixture(t testing.TB, ctx context.Context) *providerStatusRouteFixture {
	t.Helper()
	self := &providerStatusRouteFixture{
		ctx:    ctx,
		router: router.NewRouter(ctx, Routes()),
	}

	self.networkId, self.rootToken = newProviderStatusRouteNetwork(ctx)
	for range 3 {
		clientId, token := self.mintClient(t, self.rootToken)
		model.SetProvide(ctx, clientId, providerStatusRouteProvideKeys)
		self.providerClientIds = append(self.providerClientIds, clientId)
		self.callerClientId = clientId
		self.callerToken = token
	}
	slices.SortFunc(self.providerClientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	self.idleClientId, self.idleToken = self.mintClient(t, self.rootToken)

	self.otherNetworkId, self.otherRootToken = newProviderStatusRouteNetwork(ctx)
	self.otherClientId, _ = self.mintClient(t, self.otherRootToken)
	model.SetProvide(ctx, self.otherClientId, providerStatusRouteProvideKeys)
	return self
}

// mintClient creates a top-level client with POST /network/auth-client and
// returns its client token.
func (self *providerStatusRouteFixture) mintClient(t testing.TB, rootToken string) (server.Id, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"description": "provider", "device_spec": "test"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/network/auth-client", bytes.NewReader(body))
	r.RemoteAddr = "198.51.100.43:4000"
	r.Header.Set("Authorization", "Bearer "+rootToken)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	self.router.ServeHTTP(w, r)
	var result model.AuthNetworkClientResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Error != nil || result.ClientId == nil || result.ByClientJwt == nil {
		t.Fatalf("auth-client: %d %s", w.Code, w.Body.String())
	}
	return *result.ClientId, *result.ByClientJwt
}

// serve serves GET path with the token and the If-None-Match headers given.
func (self *providerStatusRouteFixture) serve(t testing.TB, path string, token string, ifNoneMatch ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = "198.51.100.43:4000"
	r.Header.Set("Authorization", "Bearer "+token)
	for _, value := range ifNoneMatch {
		r.Header.Add("If-None-Match", value)
	}
	w := httptest.NewRecorder()
	self.router.ServeHTTP(w, r)
	return w
}

// get serves GET path with the token and returns the body of a 200.
func (self *providerStatusRouteFixture) get(t testing.TB, path string, token string) string {
	t.Helper()
	w := self.serve(t, path, token)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// getTagged serves GET path with the token and returns the body of a 200 and
// its ETag, which must be a strong tag sent with Cache-Control private,
// no-cache.
func (self *providerStatusRouteFixture) getTagged(t testing.TB, path string, token string) (string, string) {
	t.Helper()
	w := self.serve(t, path, token)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", path, w.Code, w.Body.String())
	}
	etag := w.Header().Get("ETag")
	if !providerStatusRouteTag.MatchString(etag) || w.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("GET %s: ETag %q, Cache-Control %q", path, etag, w.Header().Get("Cache-Control"))
	}
	return w.Body.String(), etag
}

// A strong tag: 32 lowercase hex digits in double quotes.
var providerStatusRouteTag = regexp.MustCompile(`^"[0-9a-f]{32}"$`)

// The answer of a 200 as JSON objects.
type providerStatusRouteAnswer struct {
	Providers    []map[string]any `json:"providers"`
	Truncated    *bool            `json:"truncated"`
	TotalClients *int             `json:"total_clients"`
	Error        *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func decodeProviderStatusRouteAnswer(t testing.TB, body string) *providerStatusRouteAnswer {
	t.Helper()
	answer := &providerStatusRouteAnswer{}
	if err := json.Unmarshal([]byte(body), answer); err != nil {
		t.Fatalf("%s: %s", err, body)
	}
	return answer
}

// The keys of a JSON object, sorted.
func providerStatusRouteKeys(t testing.TB, value any) []string {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object", value)
	}
	keys := []string{}
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func providerStatusRouteClientIds(answer *providerStatusRouteAnswer) []string {
	clientIds := []string{}
	for _, provider := range answer.Providers {
		clientId, _ := provider["client_id"].(string)
		clientIds = append(clientIds, clientId)
	}
	return clientIds
}

const providerStatusRouteNotFound = `{"providers":[],"error":{"message":"Provider client not found in this network."}}`

// client_id answers the status of the caller's own client, alone, with its
// texts and a fresh histogram; and of another provider client of the
// caller's network, with the client token or the root token. A client that is
// not a provider reads its own status.
func TestProviderStatusRouteAnswersTheCallersClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newProviderStatusRouteFixture(t, context.Background())

		answer := decodeProviderStatusRouteAnswer(t, fixture.get(t, "/network/provider-status?client_id="+fixture.callerClientId.String(), fixture.callerToken))
		if answer.Error != nil || answer.Truncated != nil || !slices.Equal(providerStatusRouteClientIds(answer), []string{fixture.callerClientId.String()}) {
			t.Fatalf("the caller's client: %+v", answer)
		}
		status := answer.Providers[0]
		if status["reason"] != model.ProviderStatusReasonNotConnected || status["reason_text"] == "" {
			t.Fatalf("status = %v", status)
		}
		appearances, _ := status["appearances"].(map[string]any)
		if buckets, _ := appearances["appearances_per_minute"].([]any); len(buckets) != model.ProviderAppearanceWindowBuckets {
			t.Fatalf("appearances = %v", status["appearances"])
		}

		for _, clientId := range fixture.providerClientIds {
			for _, token := range []string{fixture.callerToken, fixture.rootToken} {
				answer := decodeProviderStatusRouteAnswer(t, fixture.get(t, "/network/provider-status?client_id="+clientId.String(), token))
				if answer.Error != nil || !slices.Equal(providerStatusRouteClientIds(answer), []string{clientId.String()}) {
					t.Fatalf("provider client %s: %+v", clientId, answer)
				}
			}
		}

		idle := decodeProviderStatusRouteAnswer(t, fixture.get(t, "/network/provider-status?client_id="+fixture.idleClientId.String(), fixture.idleToken))
		if idle.Error != nil || !slices.Equal(providerStatusRouteClientIds(idle), []string{fixture.idleClientId.String()}) || idle.Providers[0]["reason"] != model.ProviderStatusReasonNotProviding {
			t.Fatalf("own client that is not a provider: %+v", idle)
		}
	})
}

// Another network's provider client answers exactly what an unknown id
// answers, byte for byte, in both forms and with either token, though its own
// network reads it.
func TestProviderStatusRouteAnotherNetworksClientIsNotFound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newProviderStatusRouteFixture(t, context.Background())

		own := decodeProviderStatusRouteAnswer(t, fixture.get(t, "/network/provider-status?client_id="+fixture.otherClientId.String(), fixture.otherRootToken))
		if own.Error != nil || !slices.Equal(providerStatusRouteClientIds(own), []string{fixture.otherClientId.String()}) {
			t.Fatalf("its own network: %+v", own)
		}

		unknownClientId := server.NewId()
		for _, token := range []string{fixture.callerToken, fixture.rootToken} {
			for _, format := range []string{"", "&format=compact"} {
				other := fixture.get(t, "/network/provider-status?client_id="+fixture.otherClientId.String()+format, token)
				unknown := fixture.get(t, "/network/provider-status?client_id="+unknownClientId.String()+format, token)
				if other != providerStatusRouteNotFound || unknown != other {
					t.Fatalf("another network's client %s, an unknown id %s, want both %s", other, unknown, providerStatusRouteNotFound)
				}
			}
		}
	})
}

// An unknown id answers that the network has no such provider client, as a
// client of the network that is not a provider does for any caller but
// itself. A malformed id is invalid.
func TestProviderStatusRouteUnknownClientIsNotFound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newProviderStatusRouteFixture(t, context.Background())

		for _, token := range []string{fixture.callerToken, fixture.rootToken} {
			if body := fixture.get(t, "/network/provider-status?client_id="+server.NewId().String(), token); body != providerStatusRouteNotFound {
				t.Fatalf("unknown id: %s", body)
			}
			if body := fixture.get(t, "/network/provider-status?client_id="+fixture.idleClientId.String(), token); body != providerStatusRouteNotFound {
				t.Fatalf("a client that is not a provider: %s", body)
			}
			if body := fixture.get(t, "/network/provider-status?client_id=not-a-client-id", token); body != `{"providers":[],"error":{"message":"Invalid client_id."}}` {
				t.Fatalf("malformed id: %s", body)
			}
		}
	})
}

// format=compact answers the statuses of the answer without a query, in the
// same order with the same reasons and codes and the same total_clients,
// without the reason text and the explanations; with client_id, one
// client's.
func TestProviderStatusRouteCompactForm(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newProviderStatusRouteFixture(t, context.Background())

		fullBody := fixture.get(t, "/network/provider-status", fixture.callerToken)
		compactBody := fixture.get(t, "/network/provider-status?format=compact", fixture.callerToken)
		full := decodeProviderStatusRouteAnswer(t, fullBody)
		compact := decodeProviderStatusRouteAnswer(t, compactBody)
		if compact.Error != nil || len(compact.Providers) != 3 || !slices.Equal(providerStatusRouteClientIds(compact), providerStatusRouteClientIds(full)) {
			t.Fatalf("compact %s, full %s", compactBody, fullBody)
		}
		if compact.TotalClients == nil || *compact.TotalClients != 3 || full.TotalClients == nil || *full.TotalClients != 3 {
			t.Fatalf("total_clients: compact %s, full %s", compactBody, fullBody)
		}
		for _, key := range []string{`"reason_text"`, `"explanation"`, `"has_value"`, `"has_minimum"`, `"has_maximum"`} {
			if strings.Contains(compactBody, key) {
				t.Fatalf("the compact form carries %s: %s", key, compactBody)
			}
		}
		for i, status := range compact.Providers {
			if !slices.Equal(providerStatusRouteKeys(t, status), []string{"admission", "appearances", "client_id", "evaluate_time", "ranking", "reason", "reason_code"}) {
				t.Fatalf("compact status keys %v", providerStatusRouteKeys(t, status))
			}
			if status["reason"] != full.Providers[i]["reason"] || status["reason_code"] != full.Providers[i]["reason_code"] || status["reason_code"] != "not_connected" {
				t.Fatalf("compact reason %v %v, full %v %v", status["reason"], status["reason_code"], full.Providers[i]["reason"], full.Providers[i]["reason_code"])
			}
			ranking, _ := status["ranking"].([]any)
			fullRanking, _ := full.Providers[i]["ranking"].([]any)
			if len(ranking) == 0 || len(ranking) != len(fullRanking) {
				t.Fatalf("compact ranking %v, full %v", ranking, fullRanking)
			}
			for j, number := range ranking {
				name := number.(map[string]any)["name"]
				if name != fullRanking[j].(map[string]any)["name"] {
					t.Fatalf("compact number %d %v, full %v", j, number, fullRanking[j])
				}
			}
		}
		if 2*len(compactBody) >= len(fullBody) {
			t.Fatalf("the compact form is %d bytes, the full %d", len(compactBody), len(fullBody))
		}

		one := decodeProviderStatusRouteAnswer(t, fixture.get(t, "/network/provider-status?format=compact&client_id="+fixture.providerClientIds[0].String(), fixture.callerToken))
		if one.Error != nil || !slices.Equal(providerStatusRouteClientIds(one), []string{fixture.providerClientIds[0].String()}) || one.Providers[0]["reason_text"] != nil || one.Providers[0]["reason_code"] != "not_connected" || one.TotalClients != nil {
			t.Fatalf("one client in the compact form: %+v", one)
		}
	})
}

// The answer without a query is compatible with the one before: the caller's
// own client first and the other provider clients in client id order, every
// status with its texts, every number with its full fields, and nothing added
// but total_clients beside the providers and reason_code in each status.
func TestProviderStatusRouteDefaultAnswerCompatible(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newProviderStatusRouteFixture(t, context.Background())

		wantOrder := []string{fixture.callerClientId.String()}
		for _, clientId := range fixture.providerClientIds {
			if clientId != fixture.callerClientId {
				wantOrder = append(wantOrder, clientId.String())
			}
		}
		for _, token := range []string{fixture.callerToken, fixture.rootToken} {
			body := fixture.get(t, "/network/provider-status", token)
			var envelope map[string]any
			if err := json.Unmarshal([]byte(body), &envelope); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(providerStatusRouteKeys(t, envelope), []string{"providers", "total_clients"}) {
				t.Fatalf("answer keys %v: %s", providerStatusRouteKeys(t, envelope), body)
			}
			answer := decodeProviderStatusRouteAnswer(t, body)
			if answer.TotalClients == nil || *answer.TotalClients != len(fixture.providerClientIds) {
				t.Fatalf("total_clients %v, want %d", answer.TotalClients, len(fixture.providerClientIds))
			}
			clientIds := providerStatusRouteClientIds(answer)
			if token == fixture.callerToken && !slices.Equal(clientIds, wantOrder) {
				t.Fatalf("order %v, want %v", clientIds, wantOrder)
			}
			if token == fixture.rootToken {
				wantIds := []string{}
				for _, clientId := range fixture.providerClientIds {
					wantIds = append(wantIds, clientId.String())
				}
				if !slices.Equal(clientIds, wantIds) {
					t.Fatalf("order %v, want %v", clientIds, wantIds)
				}
			}
			for _, status := range answer.Providers {
				if !slices.Equal(providerStatusRouteKeys(t, status), []string{"admission", "appearances", "client_id", "evaluate_time", "ranking", "reason", "reason_code", "reason_text"}) {
					t.Fatalf("status keys %v", providerStatusRouteKeys(t, status))
				}
				if status["reason_text"] == "" || status["reason"] != "not_connected" || status["reason_code"] != "not_connected" {
					t.Fatalf("status = %v", status)
				}
				ranking, _ := status["ranking"].([]any)
				if len(ranking) == 0 {
					t.Fatalf("ranking = %v", status["ranking"])
				}
				for _, number := range ranking {
					if !slices.Equal(providerStatusRouteKeys(t, number), []string{"count", "explanation", "has_maximum", "has_minimum", "has_value", "maximum", "minimum", "name", "passes", "total", "value"}) {
						t.Fatalf("number keys %v", providerStatusRouteKeys(t, number))
					}
					if number.(map[string]any)["explanation"] == "" {
						t.Fatalf("number = %v", number)
					}
				}
			}
		}
	})
}

// Every 200 answer carries a strong ETag of its body, with Cache-Control
// private, no-cache: the network's answer in both forms, one client's in both
// forms, and a refused query. A request whose If-None-Match names the tag,
// strong, weak or in a list, is answered 304 with the tag and no body; one
// that names another tag is answered in full.
func TestProviderStatusRouteAnswersCarryAnETag(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newProviderStatusRouteFixture(t, context.Background())
		pinProviderStatusRouteClock(t)

		for _, path := range []string{
			"/network/provider-status",
			"/network/provider-status?format=full",
			"/network/provider-status?format=compact",
			"/network/provider-status?client_id=" + fixture.providerClientIds[0].String(),
			"/network/provider-status?client_id=" + fixture.providerClientIds[0].String() + "&format=compact",
			"/network/provider-status?client_id=" + server.NewId().String(),
			"/network/provider-status?client_id=not-a-client-id",
			"/network/provider-status?format=xml",
		} {
			for _, token := range []string{fixture.callerToken, fixture.rootToken} {
				body, etag := fixture.getTagged(t, path, token)
				for _, ifNoneMatch := range []string{etag, "W/" + etag, `"0123", ` + etag} {
					w := fixture.serve(t, path, token, ifNoneMatch)
					if w.Code != http.StatusNotModified || w.Body.Len() != 0 || w.Header().Get("ETag") != etag || w.Header().Get("Cache-Control") != "private, no-cache" {
						t.Fatalf("GET %s, If-None-Match %s: %d %q %v; want 304 for %s", path, ifNoneMatch, w.Code, w.Body.String(), w.Header(), body)
					}
				}
				w := fixture.serve(t, path, token, `"00000000000000000000000000000000"`)
				if w.Code != http.StatusOK || w.Body.String() != body || w.Header().Get("ETag") != etag {
					t.Fatalf("GET %s with another tag: %d %q %v", path, w.Code, w.Body.String(), w.Header())
				}
			}
		}
	})
}

// The ETag changes exactly when the answer does: an appearance recorded for
// the caller's client changes its histogram, so every answer that holds that
// client has a new body and a new tag, the old tag is answered in full and
// the new one 304; another client's answer keeps its body and tag.
func TestProviderStatusRouteETagFollowsTheAnswer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newProviderStatusRouteFixture(t, ctx)
		now := pinProviderStatusRouteClock(t)

		var otherClientId server.Id
		for _, clientId := range fixture.providerClientIds {
			if clientId != fixture.callerClientId {
				otherClientId = clientId
			}
		}
		changedPaths := []string{
			"/network/provider-status",
			"/network/provider-status?format=compact",
			"/network/provider-status?client_id=" + fixture.callerClientId.String(),
			"/network/provider-status?client_id=" + fixture.callerClientId.String() + "&format=compact",
		}
		unchangedPath := "/network/provider-status?client_id=" + otherClientId.String()
		bodies := map[string]string{}
		etags := map[string]string{}
		for _, path := range append(changedPaths, unchangedPath) {
			bodies[path], etags[path] = fixture.getTagged(t, path, fixture.callerToken)
		}

		appearances := model.NewProviderAppearances(ctx, model.DefaultProviderAppearanceSettings())
		appearances.Record([]server.Id{fixture.callerClientId}, now)
		appearances.Close()

		for _, path := range changedPaths {
			w := fixture.serve(t, path, fixture.callerToken, etags[path])
			etag := w.Header().Get("ETag")
			if w.Code != http.StatusOK || w.Body.String() == bodies[path] || !providerStatusRouteTag.MatchString(etag) || etag == etags[path] {
				t.Fatalf("GET %s after an appearance, with the old tag %s: %d %s %q", path, etags[path], w.Code, etag, w.Body.String())
			}
			if w := fixture.serve(t, path, fixture.callerToken, etag); w.Code != http.StatusNotModified || w.Header().Get("ETag") != etag {
				t.Fatalf("GET %s with the new tag %s: %d", path, etag, w.Code)
			}
		}
		if w := fixture.serve(t, unchangedPath, fixture.callerToken, etags[unchangedPath]); w.Code != http.StatusNotModified || w.Header().Get("ETag") != etags[unchangedPath] {
			t.Fatalf("GET %s, which the appearance does not change: %d %q", unchangedPath, w.Code, w.Body.String())
		}
	})
}

// The ETag is scoped to the caller's network and the query. Two networks
// without providers answer one body with different tags, and one's tag is
// answered in full for the other. Each query has its own tag; no format and
// format=full, one answer, share one, as do spellings of one client id. A
// client id the caller may not read gets the tag of the not-found answer for
// that id: another network's client reads the same once it is removed.
func TestProviderStatusRouteETagIsScopedToNetworkAndQuery(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newProviderStatusRouteFixture(t, ctx)
		pinProviderStatusRouteClock(t)

		_, emptyToken := newProviderStatusRouteNetwork(ctx)
		_, otherEmptyToken := newProviderStatusRouteNetwork(ctx)
		emptyBody, emptyTag := fixture.getTagged(t, "/network/provider-status", emptyToken)
		otherEmptyBody, otherEmptyTag := fixture.getTagged(t, "/network/provider-status", otherEmptyToken)
		if emptyBody != `{"providers":[],"total_clients":0}` || otherEmptyBody != emptyBody || otherEmptyTag == emptyTag {
			t.Fatalf("two networks without providers: %s %s, %s %s", emptyBody, emptyTag, otherEmptyBody, otherEmptyTag)
		}
		if w := fixture.serve(t, "/network/provider-status", otherEmptyToken, emptyTag); w.Code != http.StatusOK || w.Body.String() != emptyBody {
			t.Fatalf("another network's tag: %d %q", w.Code, w.Body.String())
		}

		clientId := fixture.providerClientIds[0].String()
		paths := map[string]string{}
		for _, path := range []string{
			"/network/provider-status",
			"/network/provider-status?format=compact",
			"/network/provider-status?client_id=" + clientId,
			"/network/provider-status?client_id=" + clientId + "&format=compact",
			"/network/provider-status?client_id=" + fixture.providerClientIds[1].String(),
			"/network/provider-status?client_id=" + server.NewId().String(),
			"/network/provider-status?client_id=" + server.NewId().String(),
		} {
			_, etag := fixture.getTagged(t, path, fixture.rootToken)
			if other, ok := paths[etag]; ok {
				t.Fatalf("%s and %s share the tag %s", path, other, etag)
			}
			paths[etag] = path
		}
		for _, same := range [][2]string{
			{"/network/provider-status", "/network/provider-status?format=full"},
			{"/network/provider-status?client_id=" + clientId, "/network/provider-status?format=full&client_id=" + strings.ToUpper(clientId)},
			{"/network/provider-status?client_id=" + clientId, "/network/provider-status?client_id=" + strings.ReplaceAll(clientId, "-", "")},
		} {
			firstBody, firstTag := fixture.getTagged(t, same[0], fixture.rootToken)
			secondBody, secondTag := fixture.getTagged(t, same[1], fixture.rootToken)
			if firstBody != secondBody || firstTag != secondTag {
				t.Fatalf("%s %s and %s %s, one answer", same[0], firstTag, same[1], secondTag)
			}
		}

		otherPath := "/network/provider-status?client_id=" + fixture.otherClientId.String()
		otherBody, otherTag := fixture.getTagged(t, otherPath, fixture.rootToken)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, fixture.otherClientId))
		})
		removedBody, removedTag := fixture.getTagged(t, otherPath, fixture.rootToken)
		if otherBody != providerStatusRouteNotFound || removedBody != otherBody || removedTag != otherTag {
			t.Fatalf("another network's client %s %s, once removed %s %s", otherBody, otherTag, removedBody, removedTag)
		}
	})
}

// total_clients counts the network's provider clients past the ones an
// answer decides. With more provider clients than ProviderStatusMaxClients,
// the answer decides the lowest client ids, is truncated, and counts them
// all, in both forms; the caller's own provider client is listed first and
// counted; one client's answer has no count.
func TestProviderStatusRouteTotalClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := &providerStatusRouteFixture{
			ctx:    ctx,
			router: router.NewRouter(ctx, Routes()),
		}
		networkId, rootToken := newProviderStatusRouteNetwork(ctx)
		providerClientIds := []server.Id{}
		for range model.ProviderStatusMaxClients + 2 {
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "provider", "test")
			model.SetProvide(ctx, clientId, providerStatusRouteProvideKeys)
			providerClientIds = append(providerClientIds, clientId)
		}
		// clients the answer does not draw from
		idleClientId, _ := fixture.mintClient(t, rootToken)
		callerClientId, callerToken := fixture.mintClient(t, rootToken)
		model.SetProvide(ctx, callerClientId, providerStatusRouteProvideKeys)
		providerClientIds = append(providerClientIds, callerClientId)
		slices.SortFunc(providerClientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		total := len(providerClientIds)

		lowest := []string{}
		for _, clientId := range providerClientIds[:model.ProviderStatusMaxClients] {
			lowest = append(lowest, clientId.String())
		}
		callerOrder := []string{callerClientId.String()}
		for _, clientId := range lowest {
			if clientId != callerClientId.String() {
				callerOrder = append(callerOrder, clientId)
			}
		}

		for _, query := range []string{"", "?format=compact"} {
			root := decodeProviderStatusRouteAnswer(t, fixture.get(t, "/network/provider-status"+query, rootToken))
			if root.Truncated == nil || !*root.Truncated || root.TotalClients == nil || *root.TotalClients != total || !slices.Equal(providerStatusRouteClientIds(root), lowest) {
				t.Fatalf("%q with the root token: truncated %v, total_clients %v, %v; want %d of %d", query, root.Truncated, root.TotalClients, providerStatusRouteClientIds(root), len(lowest), total)
			}
			caller := decodeProviderStatusRouteAnswer(t, fixture.get(t, "/network/provider-status"+query, callerToken))
			if caller.Truncated == nil || !*caller.Truncated || caller.TotalClients == nil || *caller.TotalClients != total || !slices.Equal(providerStatusRouteClientIds(caller), callerOrder) {
				t.Fatalf("%q with the caller's token: truncated %v, total_clients %v, %v", query, caller.Truncated, caller.TotalClients, providerStatusRouteClientIds(caller))
			}
		}

		last := providerClientIds[len(providerClientIds)-1].String()
		one := decodeProviderStatusRouteAnswer(t, fixture.get(t, "/network/provider-status?client_id="+last, rootToken))
		if one.Error != nil || !slices.Equal(providerStatusRouteClientIds(one), []string{last}) || one.TotalClients != nil || one.Truncated != nil {
			t.Fatalf("one client past the decided ones: %+v", one)
		}
		if body := fixture.get(t, "/network/provider-status?client_id="+idleClientId.String(), rootToken); body != providerStatusRouteNotFound {
			t.Fatalf("a client that is not a provider: %s", body)
		}
	})
}
