package api

// GET /network/provider-status end to end through the router the API serves,
// against the database and redis, with real tokens: the client tokens are
// minted with POST /network/auth-client by the network's root token, as the
// apps' clients are. The query (client_id, format) answers one client of the
// caller's network or the compact form, every client id the caller may not
// read gets one answer, and the answer without a query is unchanged. Run with
// `cd server/api && ../test.sh -run ProviderStatusRoute`.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// A network with three provider clients and one client that is not a
// provider, each with its own client token, and another network with a
// provider client.
type providerStatusRouteFixture struct {
	ctx    context.Context
	router *router.Router

	rootToken string
	// the caller: the last provider client minted
	callerClientId server.Id
	callerToken    string
	// every provider client of the network, the caller's included, in client
	// id order
	providerClientIds []server.Id
	idleClientId      server.Id
	idleToken         string

	otherRootToken string
	otherClientId  server.Id
}

func newProviderStatusRouteFixture(t testing.TB, ctx context.Context) *providerStatusRouteFixture {
	t.Helper()
	self := &providerStatusRouteFixture{
		ctx:    ctx,
		router: router.NewRouter(ctx, Routes()),
	}
	keys := map[model.ProvideMode][]byte{
		model.ProvideModePublic:  bytes.Repeat([]byte{1}, 32),
		model.ProvideModeNetwork: bytes.Repeat([]byte{2}, 32),
	}
	newNetwork := func() string {
		networkId := server.NewId()
		userId := server.NewId()
		networkName := fmt.Sprintf("pstatus%s", strings.ReplaceAll(networkId.String(), "-", "")[:12])
		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
		return session.NewByJwt(networkId, userId, networkName, false, false).Testing_Sign()
	}

	self.rootToken = newNetwork()
	for range 3 {
		clientId, token := self.mintClient(t, self.rootToken)
		model.SetProvide(ctx, clientId, keys)
		self.providerClientIds = append(self.providerClientIds, clientId)
		self.callerClientId = clientId
		self.callerToken = token
	}
	slices.SortFunc(self.providerClientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	self.idleClientId, self.idleToken = self.mintClient(t, self.rootToken)

	self.otherRootToken = newNetwork()
	self.otherClientId, _ = self.mintClient(t, self.otherRootToken)
	model.SetProvide(ctx, self.otherClientId, keys)
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

// get serves GET path with the token and returns the body of a 200.
func (self *providerStatusRouteFixture) get(t testing.TB, path string, token string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = "198.51.100.43:4000"
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	self.router.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// The answer of a 200 as JSON objects.
type providerStatusRouteAnswer struct {
	Providers []map[string]any `json:"providers"`
	Truncated *bool            `json:"truncated"`
	Error     *struct {
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
// same order with the same reasons, without the reason text and the
// explanations; with client_id, one client's.
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
		for _, key := range []string{`"reason_text"`, `"explanation"`, `"has_value"`, `"has_minimum"`, `"has_maximum"`} {
			if strings.Contains(compactBody, key) {
				t.Fatalf("the compact form carries %s: %s", key, compactBody)
			}
		}
		for i, status := range compact.Providers {
			if !slices.Equal(providerStatusRouteKeys(t, status), []string{"admission", "appearances", "client_id", "evaluate_time", "ranking", "reason"}) {
				t.Fatalf("compact status keys %v", providerStatusRouteKeys(t, status))
			}
			if status["reason"] != full.Providers[i]["reason"] {
				t.Fatalf("compact reason %v, full %v", status["reason"], full.Providers[i]["reason"])
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
		if one.Error != nil || !slices.Equal(providerStatusRouteClientIds(one), []string{fixture.providerClientIds[0].String()}) || one.Providers[0]["reason_text"] != nil {
			t.Fatalf("one client in the compact form: %+v", one)
		}
	})
}

// The answer without a query is unchanged: the caller's own client first and
// the other provider clients in client id order, every status with its texts,
// every number with its full fields, and nothing else in the answer.
func TestProviderStatusRouteDefaultAnswerUnchanged(t *testing.T) {
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
			if !slices.Equal(providerStatusRouteKeys(t, envelope), []string{"providers"}) {
				t.Fatalf("answer keys %v: %s", providerStatusRouteKeys(t, envelope), body)
			}
			answer := decodeProviderStatusRouteAnswer(t, body)
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
				if !slices.Equal(providerStatusRouteKeys(t, status), []string{"admission", "appearances", "client_id", "evaluate_time", "ranking", "reason", "reason_text"}) {
					t.Fatalf("status keys %v", providerStatusRouteKeys(t, status))
				}
				if status["reason_text"] == "" {
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
