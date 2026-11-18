package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// §14.14's four routes take their classes (SMTP-M9-19, AUTHZ1): the three Client routes reach a sidecar's strict_exp
// token and an ordinary client token, and POST /smtp/appeal, Network class and off §7.4's allowlist, refuses both,
// the network's child client tokens, before its handler runs, and serves the network's credential, asking an
// Owner's or Admin's role of a member.
func TestSmtpRoutesTakeTheirClasses(t *testing.T) {
	want := map[string]routeAccess{
		"POST /smtp/dkim-keys": routeAccessClient,
		"GET /smtp/status":     routeAccessClient,
		"GET /smtp/reports":    routeAccessClient,
		"POST /smtp/appeal":    routeAccessNetwork,
	}
	for key, access := range want {
		if routeAccessByRoute[key] != access {
			t.Fatalf("%s is class %d, want %d", key, routeAccessByRoute[key], access)
		}
		onAllowlist := key != "POST /smtp/appeal"
		if strictExpRoutes[key] != onAllowlist || forwardOnlyRoutes[key] {
			t.Fatalf("%s: on the strict_exp allowlist %t, on the forwarding one %t", key, strictExpRoutes[key], forwardOnlyRoutes[key])
		}
	}
	if routePermissionFor("POST /smtp/appeal") != model.RolePermissionAdmin {
		t.Fatal("the appeal asks the wrong permission of a member's role")
	}

	strict := newStrictExpTokens()
	tokens := newRouteAccessTokens()
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return false
	})
	setClientForwardOnly(t, func(ctx context.Context, networkId server.Id, clientId server.Id) (bool, error) {
		return false, nil
	})
	routes := Routes()
	byKey := func(key string) *router.Route {
		index := slices.IndexFunc(routes, func(route *router.Route) bool { return routeAccessKey(route) == key })
		if index < 0 {
			t.Fatalf("no route %s", key)
		}
		return routes[index]
	}
	for key := range want {
		w, reached := serveStubbed(byKey(key), "Bearer "+strict.strictToken, nil)
		if key == "POST /smtp/appeal" {
			if reached || w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.StrictExpCredentialRefusedMessage {
				t.Errorf("%s: a strict_exp token got %d %q, handler ran = %t; want the sidecar refusal", key, w.Code, w.Body.String(), reached)
			}
		} else if !reached || w.Code != http.StatusOK {
			t.Errorf("%s: a strict_exp token got %d, handler ran = %t; want the handler", key, w.Code, reached)
		}

		w, reached = serveStubbed(byKey(key), "Bearer "+tokens.clientToken, nil)
		if key == "POST /smtp/appeal" {
			if reached || w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.ClientCredentialRefusedMessage {
				t.Errorf("%s: a client token got %d %q, handler ran = %t; want the client token refusal", key, w.Code, w.Body.String(), reached)
			}
		} else if !reached || w.Code != http.StatusOK {
			t.Errorf("%s: a client token got %d, handler ran = %t; want the handler", key, w.Code, reached)
		}

		if w, reached := serveStubbed(byKey(key), "Bearer "+tokens.networkToken, nil); !reached || w.Code != http.StatusOK {
			t.Errorf("%s: the network's token got %d, handler ran = %t; want the handler", key, w.Code, reached)
		}
	}
}
