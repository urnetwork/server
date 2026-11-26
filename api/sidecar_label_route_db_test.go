//go:build sidecar_pending

package api

// GET /sidecar/label through the router the API serves (§12.5; owner decision 421, QD-935), against the database
// with MG2 and the test stack's Redis: a sidecar's token reads its network's label in exactly the form the sidecar's
// GetSidecarLabel reads, and a second request answers the same label with no draw; the route's Client class serves
// every credential of the network, a legacy client's, its child's and the network's own among them, and refuses the
// forwarding client of an app's VPN component before anything is allocated; and nothing in a request makes the route
// answer another network. Two first requests at once are the controller's test
// (TestSidecarLabelTwoFirstRequestsAllocateOneLabel), held at the label lock below this router.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// §12.5's label: 10 characters, a letter and then letters or digits.
var testSidecarLabelRoutePattern = regexp.MustCompile(`^[a-z][a-z0-9]{9}$`)

// A random source every read of fails, so a draw made while it is in place answers an error.
type failingSidecarLabelRouteReader struct{}

func (self failingSidecarLabelRouteReader) Read(p []byte) (int, error) {
	return 0, errors.New("no label may be drawn here")
}

// A dns root no other test serves, as dns.yml names one.
func testSidecarLabelRouteRoot() string {
	id := strings.ReplaceAll(server.NewId().String(), "-", "")
	return "label-route-" + id[len(id)-12:] + ".example"
}

// The network's label rows in root, of any state, as dns_label holds them.
func testSidecarLabelRouteRows(ctx context.Context, networkId server.Id, root string) []string {
	labels := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT label FROM dns_label WHERE network_id = $1 AND root = $2 AND kind = $3 ORDER BY label`,
			networkId,
			root+".",
			model.DnsLabelKindLabel,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var label string
				server.Raise(result.Scan(&label))
				labels = append(labels, label)
			}
		})
	})
	return labels
}

// GET path (the route's, with whatever query a case adds) as the sidecar reads it, after edit: the status, the
// body's keys, the body decoded as the sidecar's own sidecar.SidecarLabel and the body. Keys and answer are nil
// unless the status is 200.
func (self *routeAccessDbFixture) sidecarLabel(t testing.TB, path string, authorization string, edit func(r *http.Request)) (int, []string, *sidecar.SidecarLabel, string) {
	t.Helper()
	w := self.serveEdited(http.MethodGet, path, authorization, nil, edit)
	if w.Code != http.StatusOK {
		return w.Code, nil, nil, w.Body.String()
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatalf("GET %s: %s: %s", path, err, w.Body.String())
	}
	answer := &sidecar.SidecarLabel{}
	if err := json.Unmarshal(w.Body.Bytes(), answer); err != nil {
		t.Fatalf("GET %s as the sidecar reads it: %s: %s", path, err, w.Body.String())
	}
	return w.Code, slices.Sorted(maps.Keys(fields)), answer, w.Body.String()
}

// A sidecar's first request answers {network_id, label} for its own network, the keys the sidecar's SidecarLabel
// reads with the alias left out, passing what GetSidecarLabel asks of an answer, and allocates one label; with every
// draw failing after that, its second request answers the same, so it drew nothing.
func TestSidecarLabelRouteAnswersASidecarItsNetworksLabel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(tb, ctx)
		defer server.Testing_PushSidecarConfig(self.previewYaml())()
		root := testSidecarLabelRouteRoot()
		defer model.Testing_SetDnsUserRoot(root)()
		laptop := newSidecarRouteClient(t, self, nil, nil)

		status, keys, first, body := self.sidecarLabel(t, "/sidecar/label", laptop.authorization(), nil)
		if status != http.StatusOK || !slices.Equal(keys, []string{"label", "network_id"}) {
			t.Fatalf("the first request: %d %v %s", status, keys, body)
		}
		if first.NetworkId != self.networkId || first.Label == "" || !testSidecarLabelRoutePattern.MatchString(first.Label) || first.Alias != "" {
			t.Fatalf("the first answer %+v", first)
		}
		if rows := testSidecarLabelRouteRows(ctx, self.networkId, root); !slices.Equal(rows, []string{first.Label}) {
			t.Fatalf("the first request left labels %v, answered %q", rows, first.Label)
		}

		defer model.Testing_SetDnsLabelRandom(failingSidecarLabelRouteReader{})()
		status, _, second, body := self.sidecarLabel(t, "/sidecar/label", laptop.authorization(), nil)
		if status != http.StatusOK || !reflect.DeepEqual(second, first) {
			t.Fatalf("the second request: %d %s, the first %+v", status, body, first)
		}
		if rows := testSidecarLabelRouteRows(ctx, self.networkId, root); !slices.Equal(rows, []string{first.Label}) {
			t.Fatalf("the second request left labels %v", rows)
		}
	})
}

// The Client class, on the private stack: the forwarding client an app's VPN component holds, minted by its
// sidecar's own bound client with that client's proof, is refused 403 before the handler and allocates nothing; then a
// sidecar's token, a legacy client's token (an unmarked top-level client, what the shipped apps and an embed backend's
// installations hold), that client's child's, an admin_refused client's, the network's root token and an API key each
// read the one label.
func TestSidecarLabelRouteServesItsClassAndRefusesAForwardingClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(tb, ctx)
		defer server.Testing_PushSidecarConfig(self.previewYaml())()
		root := testSidecarLabelRouteRoot()
		defer model.Testing_SetDnsUserRoot(root)()

		laptop := newSidecarRouteClient(t, self, nil, nil)
		if status, code, _ := laptop.bind(laptop.binding(1)); status != http.StatusOK {
			t.Fatalf("bind: %d %s", status, code)
		}
		forwardPublicKey, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		forwarding := self.mintMarked(t, laptop.authorization(), map[string]any{
			"source_client_id":  laptop.clientId,
			"admin_refused":     true,
			"strict_exp":        true,
			"forward_only":      true,
			"client_public_key": []byte(forwardPublicKey),
		}, laptop.proof())
		w := self.serve(http.MethodGet, "/sidecar/label", "Bearer "+*forwarding.ByClientJwt, nil)
		if w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.StrictExpCredentialRefusedMessage {
			t.Fatalf("a forwarding client's token: %d %s", w.Code, w.Body.String())
		}
		if rows := testSidecarLabelRouteRows(ctx, self.networkId, root); len(rows) != 0 {
			t.Fatalf("a refused forwarding client allocated %v", rows)
		}

		legacyClientId, legacyToken := self.mintClient(t, self.root(), nil)
		_, childToken := self.mintClient(t, "Bearer "+legacyToken, &legacyClientId)
		marked := self.mintMarked(t, self.root(), map[string]any{"admin_refused": true}, nil)
		var label string
		for _, caller := range []struct {
			name          string
			authorization string
		}{
			{name: "the sidecar's token", authorization: laptop.authorization()},
			{name: "a legacy client's token", authorization: "Bearer " + legacyToken},
			{name: "its child's token", authorization: "Bearer " + childToken},
			{name: "an admin_refused client's token", authorization: "Bearer " + *marked.ByClientJwt},
			{name: "the network's root token", authorization: self.root()},
			{name: "an API key", authorization: self.key()},
		} {
			status, _, answer, body := self.sidecarLabel(t, "/sidecar/label", caller.authorization, nil)
			if status != http.StatusOK || answer.NetworkId != self.networkId || !testSidecarLabelRoutePattern.MatchString(answer.Label) {
				t.Fatalf("%s: %d %s", caller.name, status, body)
			}
			if label == "" {
				label = answer.Label
			} else if answer.Label != label {
				t.Fatalf("%s answered %q, the first %q", caller.name, answer.Label, label)
			}
		}
		if rows := testSidecarLabelRouteRows(ctx, self.networkId, root); !slices.Equal(rows, []string{label}) {
			t.Fatalf("the network holds labels %v, answered %q", rows, label)
		}
	})
}

// The network is the token's whatever the request says: a query naming another network or its label, a body naming
// it and the same at another root answer the caller's own label; an intended network header naming the other network
// is refused 412 by its gate, answering no label; and a path naming it is no route.
func TestSidecarLabelRouteNamesNoOtherNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(tb, ctx)
		stranger := newRouteAccessDbFixture(tb, ctx)
		defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q, %q]\n", self.networkId, stranger.networkId))()
		root := testSidecarLabelRouteRoot()
		defer model.Testing_SetDnsUserRoot(root)()
		laptop := newSidecarRouteClient(t, self, nil, nil)

		status, _, strangers, body := stranger.sidecarLabel(t, "/sidecar/label", stranger.root(), nil)
		if status != http.StatusOK || strangers.NetworkId != stranger.networkId {
			t.Fatalf("the other network's own request: %d %s", status, body)
		}
		strangerId := stranger.networkId.String()
		requireOwn := func(name string, status int, answer *sidecar.SidecarLabel, body string) {
			t.Helper()
			if status != http.StatusOK || answer.NetworkId != self.networkId || answer.Label == strangers.Label ||
				strings.Contains(body, strangerId) || strings.Contains(body, strangers.Label) {
				t.Fatalf("%s: %d %s", name, status, body)
			}
		}
		status, _, own, body := self.sidecarLabel(t, "/sidecar/label", laptop.authorization(), nil)
		requireOwn("no parameter", status, own, body)

		for _, path := range []string{
			"/sidecar/label?network_id=" + strangerId,
			"/sidecar/label?networkId=" + strangerId + "&client_id=" + strangerId,
			"/sidecar/label?label=" + strangers.Label + "&root=other.example",
			"/sidecar/label?network_id=" + strangerId + "&network_id=" + self.networkId.String(),
		} {
			status, _, answer, body := self.sidecarLabel(t, path, laptop.authorization(), nil)
			requireOwn(path, status, answer, body)
			if !reflect.DeepEqual(answer, own) {
				t.Fatalf("%s answered %+v, with no parameter %+v", path, answer, own)
			}
		}

		w := self.serve(http.MethodGet, "/sidecar/label", laptop.authorization(), map[string]any{"network_id": stranger.networkId, "label": strangers.Label})
		answer := &sidecar.SidecarLabel{}
		if err := json.Unmarshal(w.Body.Bytes(), answer); err != nil {
			t.Fatalf("a body naming the other network: %d %s", w.Code, w.Body.String())
		}
		requireOwn("a body naming the other network", w.Code, answer, w.Body.String())

		w = self.serveEdited(http.MethodGet, "/sidecar/label", laptop.authorization(), nil, func(r *http.Request) {
			r.Header.Set(session.IntendedNetworkIdHeader, strangerId)
		})
		if w.Code != http.StatusPreconditionFailed || strings.Contains(w.Body.String(), strangers.Label) || strings.Contains(w.Body.String(), own.Label) {
			t.Fatalf("an intended network header naming the other network: %d %s", w.Code, w.Body.String())
		}

		if w := self.serve(http.MethodGet, "/sidecar/label/"+strangerId, laptop.authorization(), nil); w.Code != http.StatusNotFound {
			t.Fatalf("a path naming the other network: %d %s", w.Code, w.Body.String())
		}
		if rows := testSidecarLabelRouteRows(ctx, stranger.networkId, root); !slices.Equal(rows, []string{strangers.Label}) {
			t.Fatalf("the other network holds labels %v", rows)
		}
	})
}
