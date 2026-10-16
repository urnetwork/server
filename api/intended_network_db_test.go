package api

// End to end through the router the API serves, against the database: the
// owner's report of 2026-10-16. A ur.io tab still showed network A after
// another tab signed in to network B, and its account deletion sent the jwt
// the browser had stored for B, which deleted B. With
// X-UR-Intended-Network-Id naming A, every credential of B is refused and B is
// untouched. A request meant for B, and a client that does not send the
// header, are served as before (AUTHZ1.md §10).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type intendedNetworkDbNetwork struct {
	networkId server.Id
	userId    server.Id
}

func newIntendedNetworkDbNetwork(t testing.TB, ctx context.Context) *intendedNetworkDbNetwork {
	t.Helper()
	networkId := server.NewId()
	userId := server.NewId()
	networkName := fmt.Sprintf("intended%s", strings.ReplaceAll(networkId.String(), "-", "")[:12])
	model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
	return &intendedNetworkDbNetwork{networkId: networkId, userId: userId}
}

// serveIntended serves one request through the fixture's router, with the
// intended network header when one is given.
func (self *routeAccessDbFixture) serveIntended(method string, path string, authorization string, intendedNetworkId *server.Id, body any) *httptest.ResponseRecorder {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		server.Raise(err)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(string(bodyBytes)))
	r.RemoteAddr = "198.51.100.45:4000"
	r.Header.Set("Authorization", authorization)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if intendedNetworkId != nil {
		r.Header.Set("X-UR-Intended-Network-Id", intendedNetworkId.String())
	}
	w := httptest.NewRecorder()
	self.router.ServeHTTP(w, r)
	return w
}

func requireIntendedNetworkMismatch(t testing.TB, name string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusPreconditionFailed || intendedNetworkRefusalCode(w) != "intended_network_mismatch" {
		t.Fatalf("%s meant for another network: status = %d body = %q; want 412 intended_network_mismatch", name, w.Code, w.Body.String())
	}
}

func requireNetworkExists(t testing.TB, ctx context.Context, name string, networkId server.Id, userId server.Id) {
	t.Helper()
	if model.GetNetworkUser(ctx, userId) == nil {
		t.Fatalf("%s: network %s was deleted by a request meant for another network", name, networkId)
	}
}

// The reported defect, for every credential of B: the root token ur.io
// stores, an API key, and the client token the apps hold.
func TestAStaleTabCannotDeleteTheNetworkAnotherTabSignedIn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// network B, signed in by the other tab
		b := newRouteAccessDbFixture(t, ctx)
		_, clientToken := b.mintClient(t, b.root(), nil)
		// network A, still on the stale tab's screen
		a := newIntendedNetworkDbNetwork(t, ctx)

		for name, authorization := range map[string]string{
			"root token":   b.root(),
			"api key":      b.key(),
			"client token": "Bearer " + clientToken,
		} {
			w := b.serveIntended(http.MethodPost, "/auth/network-delete", authorization, &a.networkId, nil)
			requireIntendedNetworkMismatch(t, "network delete with B's "+name, w)
			requireNetworkExists(t, ctx, name, b.networkId, b.userId)
			requireNetworkExists(t, ctx, name, a.networkId, a.userId)
		}
	})
}

// A money route end to end: a balance code redeemed by a stale tab is refused
// and stays unredeemed; redeemed by the tab meant for B, or by a client that
// does not send the header, it credits B as before.
func TestTheIntendedNetworkAndOlderClientsRedeemABalanceCode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		b := newRouteAccessDbFixture(t, ctx)
		a := newIntendedNetworkDbNetwork(t, ctx)

		createCode := func() string {
			balanceCode, err := model.CreateBalanceCode(ctx, model.ByteCount(1024*1024*1024), 30*24*time.Hour, model.NanoCents(0), server.NewId().String(), "intended network test", "intended@bringyour.com")
			if err != nil {
				t.Fatal(err)
			}
			return balanceCode.Secret
		}
		redeem := func(name string, authorization string, intendedNetworkId *server.Id, secret string) {
			t.Helper()
			w := b.serveIntended(http.MethodPost, "/subscription/redeem-balance-code", authorization, intendedNetworkId, map[string]any{"secret": secret})
			var result model.RedeemBalanceCodeResult
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Error != nil || result.TransferBalance == nil {
				t.Fatalf("%s: status = %d body = %q; want the code redeemed", name, w.Code, w.Body.String())
			}
		}

		// the stale tab: refused, and the code is still unredeemed, so the tab
		// meant for B redeems it
		secret := createCode()
		for name, authorization := range map[string]string{
			"root token": b.root(),
			"api key":    b.key(),
		} {
			w := b.serveIntended(http.MethodPost, "/subscription/redeem-balance-code", authorization, &a.networkId, map[string]any{"secret": secret})
			requireIntendedNetworkMismatch(t, "balance code redeem with B's "+name, w)
		}
		redeem("root token meant for B", b.root(), &b.networkId, secret)

		redeem("api key meant for B", b.key(), &b.networkId, createCode())
		// a client that does not send the header
		redeem("root token without the header", b.root(), nil, createCode())
	})
}
