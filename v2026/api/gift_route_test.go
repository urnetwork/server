package api

// The gift routes (controller/gift_*_controller.go, mmm/GIFTOFPRIVACY.md
// §10.2): each is public, so a signed-in buyer's bearer reaches the handler
// and a signed-out one is served, and each answers through the router the API
// serves. An order's token travels only in a request body: the routes that
// take it are POSTs. Pure: no request here reaches the database.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
)

var giftRouteKeys = []string{
	"GET /pay/gift/config",
	"POST /pay/gift/checkout",
	"POST /pay/gift/crypto-intent",
	"POST /pay/gift/order",
	"POST /pay/gift/cards\\.pdf",
	"POST /pay/gift/check-code",
}

func TestGiftRoutesArePublic(t *testing.T) {
	routes := map[string]*router.Route{}
	for _, route := range Routes() {
		routes[routeAccessKey(route)] = route
	}
	for _, key := range giftRouteKeys {
		route, ok := routes[key]
		if !ok {
			t.Fatalf("%s is not served", key)
		}
		if access := routeAccessFor(route); access != routeAccessPublic {
			t.Fatalf("%s: access %d, want public", key, access)
		}
		if route.RefusesClientCredentials() || route.RefusesIntendedNetworkMismatch() {
			t.Fatalf("%s carries a credential gate", key)
		}
	}
}

// serveGiftRoute serves one request with a JSON body, signed out.
func serveGiftRoute(handler http.Handler, method string, path string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.41:4000"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// While gift sales are closed the config says so and both purchases refuse
// in-band (HTTP 200, error.code gift_sales_closed); an order or cards a
// request cannot open are 404, never 403; the order routes take no GET, so a
// token can never be sent in a url; and a secret that cannot be a code is not
// found.
func TestGiftRoutesThroughTheRouter(t *testing.T) {
	closed := model.Testing_GiftConfig()
	closed.Enabled = false
	t.Cleanup(model.Testing_SetGiftConfig(closed))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	apiRouter := router.NewRouter(ctx, Routes())

	w := serveRouteAccess(apiRouter, "GET", "/pay/gift/config", "")
	if w.Code != http.StatusOK {
		t.Fatalf("config: status %d body %q", w.Code, w.Body.String())
	}
	var config controller.GiftConfigResult
	if err := json.Unmarshal(w.Body.Bytes(), &config); err != nil {
		t.Fatalf("config: %v", err)
	}
	if config.Enabled || config.MaxQuantity != 20 || len(config.Tiers) != 3 || config.Tiers[0].UnitPriceCents != 3999 ||
		config.ProDays != 365 || config.Currency != "usd" || config.CryptoAssets == nil || len(config.CryptoAssets) != 0 {
		t.Fatalf("config: %s", w.Body.String())
	}

	for _, path := range []string{"/pay/gift/checkout", "/pay/gift/crypto-intent"} {
		w = serveRouteAccess(apiRouter, "POST", path, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d body %q", path, w.Code, w.Body.String())
		}
		var refusal struct {
			OrderId string                `json:"order_id"`
			Error   *controller.GiftError `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &refusal); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if refusal.Error == nil || refusal.Error.Code != controller.GiftErrorCodeSalesClosed || refusal.OrderId != "" {
			t.Fatalf("%s: %s", path, w.Body.String())
		}
	}

	for _, path := range []string{"/pay/gift/order", "/pay/gift/cards.pdf"} {
		for _, body := range []string{
			`{"order_id": "nonsense", "token": "ORDERTOKEN"}`,
			`{"order_id": "01a1d62a-ad5a-c336-67d9-73f0321321f8", "token": ""}`,
			`{}`,
		} {
			w = serveGiftRoute(apiRouter, "POST", path, body)
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s %s: status %d, want 404", path, body, w.Code)
			}
		}
		// not a route: the token is never read from a query
		w = serveGiftRoute(apiRouter, "GET", path+"?order_id=nonsense&token=ORDERTOKEN", "")
		if w.Code == http.StatusOK {
			t.Fatalf("GET %s is served", path)
		}
	}

	w = serveRouteAccess(apiRouter, "POST", "/pay/gift/check-code", "")
	if w.Code != http.StatusOK {
		t.Fatalf("check-code: status %d body %q", w.Code, w.Body.String())
	}
	var checked controller.GiftCheckCodeResult
	if err := json.Unmarshal(w.Body.Bytes(), &checked); err != nil {
		t.Fatalf("check-code: %v", err)
	}
	if checked.Status != model.GiftCodeStatusNotFound || checked.Kind != "" {
		t.Fatalf("check-code: %s", w.Body.String())
	}
}
