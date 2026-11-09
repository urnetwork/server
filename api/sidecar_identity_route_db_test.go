//go:build sidecar_pending

package api

// T3 of CORE-M1-13's schedules (Q-TEST-1): what sidecartest's fake api answers a sidecar on T2
// (sidecar/sidecartest/fake_api_core.go), asked of server's real routes through the router the API
// serves, against the database with MG1. Each case below has its T2 twin in
// sidecar/sidecartest/fake_api_core_test.go, and the statuses and codes here are the ones the fake
// answers, so a divergence fails one side or the other: a client binds once and its retry is
// idempotent; the lookups answer a sidecar of the caller's network and nothing of another's; a
// strict_exp refresh needs the bound key's proof; a client whose binding api lost answers
// restore_required, and a re-bind of the newest binding restores it; and a refused client's sidecar
// binds its next generation under the same identity.
//
// Not here: POST /network/enroll, which is TEAM-M1-04's, so each client is minted with POST
// /network/auth-client and the network credential, as mintStrictExpBearer does; and 21 days of
// refreshes, since server's token clock is real time.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// One sidecar's client on the routes: its token, ClientKey and identity key.
type sidecarRouteClient struct {
	fixture     *routeAccessDbFixture
	networkId   server.Id
	clientId    server.Id
	sidecarId   server.Id
	token       string
	clientKey   ed25519.PrivateKey
	identityKey ed25519.PrivateKey
	jti         int
}

// A strict_exp client of the fixture's network, as a sidecar's enrollment leaves one: marked,
// unbound, with a fresh ClientKey and, unless identityKey is given, a fresh identity.
func newSidecarRouteClient(t *testing.T, self *routeAccessDbFixture, sidecarId *server.Id, identityKey ed25519.PrivateKey) *sidecarRouteClient {
	t.Helper()
	result := self.mintMarked(t, self.root(), map[string]any{"admin_refused": true, "strict_exp": true}, nil)
	_, clientKey, err := ed25519.GenerateKey(rand.Reader)
	server.Raise(err)
	if identityKey == nil {
		_, identityKey, err = ed25519.GenerateKey(rand.Reader)
		server.Raise(err)
	}
	client := &sidecarRouteClient{
		fixture:     self,
		networkId:   self.networkId,
		clientId:    *result.ClientId,
		sidecarId:   server.NewId(),
		token:       *result.ByClientJwt,
		clientKey:   clientKey,
		identityKey: identityKey,
	}
	if sidecarId != nil {
		client.sidecarId = *sidecarId
	}
	return client
}

func (self *sidecarRouteClient) authorization() string {
	return "Bearer " + self.token
}

// The client's binding of generation.
func (self *sidecarRouteClient) binding(generation int64) []byte {
	signed, err := sidecar.NewSidecarIdentityBinding(&sidecar.SidecarIdentityBinding{
		NetworkId:  sidecar.Id(self.networkId),
		SidecarId:  sidecar.Id(self.sidecarId),
		ClientId:   sidecar.Id(self.clientId),
		Generation: generation,
		IssuedAt:   server.NowUtc(),
	}, self.identityKey.Seed(), self.clientKey.Seed())
	server.Raise(err)
	return signed
}

// POST /sidecar/bind: the status, the code and the generation.
func (self *sidecarRouteClient) bind(binding []byte) (int, string, int64) {
	w := self.fixture.serve(http.MethodPost, "/sidecar/bind", self.authorization(), map[string]any{"binding": binding})
	result := map[string]any{}
	json.Unmarshal(w.Body.Bytes(), &result)
	code, _ := result["code"].(string)
	generation, _ := result["generation"].(float64)
	return w.Code, code, int64(generation)
}

// GET /auth/refresh with a fresh proof by the client's key, or none: the status, the code in the
// answer and the new token.
func (self *sidecarRouteClient) refresh(withProof bool) (int, string, string) {
	var edit func(r *http.Request)
	if withProof {
		self.jti += 1
		jti := fmt.Sprintf("t3-jti-%d", self.jti)
		edit = func(r *http.Request) {
			url := session.Testing_DpopApiUrl(r.URL.Path)
			r.Header.Set(session.DpopHeader, session.Testing_SignDpopProof(self.clientKey, r.Method, url, self.token, jti, server.NowUtc()))
		}
	}
	w := self.fixture.serveEdited(http.MethodGet, "/auth/refresh", self.authorization(), nil, edit)
	result := map[string]any{}
	json.Unmarshal(w.Body.Bytes(), &result)
	code, _ := result["code"].(string)
	if refusal, ok := result["error"].(map[string]any); ok {
		code, _ = refusal["code"].(string)
	}
	byJwt, _ := result["by_jwt"].(string)
	return w.Code, code, byJwt
}

// Drops the identity rows of the network, as a Postgres restore to before them leaves api.
func forgetSidecarIdentities(ctx context.Context, networkId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM sidecar_identity_binding
				USING sidecar_identity
				WHERE sidecar_identity_binding.sidecar_id = sidecar_identity.sidecar_id AND sidecar_identity.network_id = $1
			`,
			networkId,
		))
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM sidecar_identity WHERE network_id = $1`, networkId))
	})
}

// T2's TestFakeApiBindFollowsTheBindingRules and TestFakeApiIdentityLookups on server: generation 1,
// a byte-identical retry answering it, other bytes client_bound, another client's binding
// binding_invalid, a skipped generation generation_conflict, the lookups by sidecar and by client, and
// 404 for a sidecar of another network.
func TestSidecarIdentityRoutesBindAndLookUp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(tb, ctx)
		defer server.Testing_PushSidecarConfig(self.previewYaml())()

		laptop := newSidecarRouteClient(t, self, nil, nil)
		first := laptop.binding(1)
		if status, code, generation := laptop.bind(first); status != http.StatusOK || generation != 1 {
			t.Fatalf("generation 1: %d %s %d", status, code, generation)
		}
		if status, _, generation := laptop.bind(first); status != http.StatusOK || generation != 1 {
			t.Errorf("a retry: %d %d", status, generation)
		}
		if status, code, _ := laptop.bind(laptop.binding(1)); status != http.StatusConflict || code != "client_bound" {
			t.Errorf("other bytes: %d %s", status, code)
		}

		phone := newSidecarRouteClient(t, self, &laptop.sidecarId, laptop.identityKey)
		other := phone.binding(2)
		phone.token = laptop.token
		if status, code, _ := phone.bind(other); status != http.StatusBadRequest || code != "binding_invalid" {
			t.Errorf("another client's binding: %d %s", status, code)
		}
		phone = newSidecarRouteClient(t, self, &laptop.sidecarId, laptop.identityKey)
		if status, code, _ := phone.bind(phone.binding(3)); status != http.StatusConflict || code != "generation_conflict" {
			t.Errorf("a skipped generation: %d %s", status, code)
		}
		if status, code, generation := phone.bind(phone.binding(2)); status != http.StatusOK || generation != 2 {
			t.Fatalf("generation 2: %d %s %d", status, code, generation)
		}

		result := map[string]any{}
		self.call(t, http.MethodGet, "/sidecar/identity/"+laptop.sidecarId.String(), laptop.authorization(), nil, http.StatusOK, &result)
		if bindings, _ := result["bindings"].([]any); len(bindings) != 2 || result["status"] != "active" {
			t.Errorf("by sidecar: %v", result)
		}
		byClient := map[string]any{}
		self.call(t, http.MethodGet, "/sidecar/identity?client_id="+laptop.clientId.String(), phone.authorization(), nil, http.StatusOK, &byClient)
		if byClient["sidecar_id"] != laptop.sidecarId.String() {
			t.Errorf("by client: %v", byClient)
		}

		stranger := newRouteAccessDbFixture(tb, ctx)
		defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q, %q]\n", self.networkId, stranger.networkId))()
		strangerClient := newSidecarRouteClient(t, stranger, nil, nil)
		if w := stranger.serve(http.MethodGet, "/sidecar/identity/"+laptop.sidecarId.String(), strangerClient.authorization(), nil); w.Code != http.StatusNotFound {
			t.Errorf("another network's sidecar: %d %s", w.Code, w.Body.String())
		}
	})
}

// T2's TestFakeApiRefreshNeedsTheBoundKeysProof and TestRestoreRequiredRebindsAndRefreshes on server:
// no proof is refused and the bound key's proof renews; a registration api lost answers
// restore_required in the 200's error object, never a refusal; the newest binding posted again is a
// byte-identical retry, and once the registration is back the next refresh renews.
//
// A restore that lost the identity rows themselves is where server and T2 still differ: §7.4's proof
// key falls back to network_client_owner.client_public_key (plan-TEAM-enroll-client-key), a column
// TEAM-M1-04 adds, so until then server cannot check the proof of a client whose binding it lost and
// refuses the refresh outright. This asks only that the refresh renews nothing there.
func TestSidecarRefreshNeedsItsProofAndRestoresThroughRebind(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(tb, ctx)
		defer server.Testing_PushSidecarConfig(self.previewYaml())()

		laptop := newSidecarRouteClient(t, self, nil, nil)
		binding := laptop.binding(1)
		if status, code, _ := laptop.bind(binding); status != http.StatusOK {
			t.Fatalf("bind: %d %s", status, code)
		}
		if status, _, byJwt := laptop.refresh(false); status == http.StatusOK && byJwt != "" {
			t.Fatal("a refresh with no proof renewed")
		}
		status, code, refreshed := laptop.refresh(true)
		if status != http.StatusOK || refreshed == "" {
			t.Fatalf("a good proof: %d %s", status, code)
		}

		// a component's registration a restore lost, until the sidecar posts it again
		const checkName = "core-m1-13-t3"
		registered := false
		model.AddSidecarRegistrationCheck(checkName, func(ctx context.Context, networkId server.Id, clientId server.Id) (bool, error) {
			return registered || clientId != laptop.clientId, nil
		})
		defer model.Testing_RemoveSidecarRegistrationCheck(checkName)
		if status, code, byJwt := laptop.refresh(true); status != http.StatusOK || code != "restore_required" || byJwt != "" {
			t.Fatalf("after the restore: %d %q", status, code)
		}
		if status, code, generation := laptop.bind(binding); status != http.StatusOK || generation != 1 {
			t.Fatalf("the re-bind: %d %s %d", status, code, generation)
		}
		registered = true
		if status, code, byJwt := laptop.refresh(true); status != http.StatusOK || byJwt == "" {
			t.Errorf("after the re-registration: %d %s", status, code)
		}

		forgetSidecarIdentities(ctx, self.networkId)
		if _, _, byJwt := laptop.refresh(true); byJwt != "" {
			t.Error("a client whose binding api lost renewed")
		}
	})
}

// T2's TestRefusedRefreshThenLoginKeepsTheSidecarId on server: a removed client's refresh is refused,
// never restore_required; the sidecar's next client binds generation 2 under the same identity, which
// the lookup answers as its newest.
func TestSidecarRefusedClientRebindsTheNextGeneration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(tb, ctx)
		defer server.Testing_PushSidecarConfig(self.previewYaml())()

		first := newSidecarRouteClient(t, self, nil, nil)
		if status, code, _ := first.bind(first.binding(1)); status != http.StatusOK {
			t.Fatalf("bind: %d %s", status, code)
		}
		w := self.serve(http.MethodPost, "/network/remove-client", first.authorization(), map[string]any{"client_id": first.clientId})
		if w.Code != http.StatusOK {
			t.Fatalf("remove-client: %d %s", w.Code, w.Body.String())
		}
		status, code, byJwt := first.refresh(true)
		if byJwt != "" || code == "restore_required" {
			t.Fatalf("a removed client's refresh: %d %q renewed or restored", status, code)
		}
		// the fake answers a removed client's token 401 at every route, as server's session check does
		if status != http.StatusUnauthorized {
			t.Errorf("a removed client's refresh answers %d", status)
		}

		second := newSidecarRouteClient(t, self, &first.sidecarId, first.identityKey)
		if status, code, generation := second.bind(second.binding(2)); status != http.StatusOK || generation != 2 {
			t.Fatalf("generation 2: %d %s %d", status, code, generation)
		}
		result := map[string]any{}
		self.call(t, http.MethodGet, "/sidecar/identity/"+first.sidecarId.String(), second.authorization(), nil, http.StatusOK, &result)
		current, _ := result["binding"].(string)
		if current == "" {
			t.Fatalf("no current binding: %v", result)
		}
		if status, code, byJwt := second.refresh(true); status != http.StatusOK || byJwt == "" {
			t.Errorf("generation 2's refresh: %d %s", status, code)
		}
	})
}
