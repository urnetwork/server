//go:build sidecar_pending

package api

// POST /sidecar/push-token through the router the API serves (§18.7's step 0, SDK-M8-09), against the
// database with MG8 and the test stack's Redis: dark until sidecar.yml's wake switch, then a client's own
// token, set, deleted and refused as the route says, Client class for a strict_exp bearer, and never a
// token in a log line or an answer (I-13).

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A token no log or answer holds by chance.
const testRoutePushToken = "push-token-canary-route-5d1e"

// The network as the only preview network, with the wake switch as on says.
func (self *routeAccessDbFixture) wakeYaml(on bool) string {
	return fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n  wake: {enabled: %t}\n", self.networkId, on)
}

// A strict_exp client of the fixture's network, as a sidecar's enrollment leaves one, and its bearer.
func (self *routeAccessDbFixture) strictPushClient(t testing.TB) (server.Id, string) {
	t.Helper()
	result := self.mintMarked(t, self.root(), map[string]any{"admin_refused": true, "strict_exp": true}, nil)
	return *result.ClientId, "Bearer " + *result.ByClientJwt
}

// Serves a request whose body is body as it is, which may not decode.
func (self *routeAccessDbFixture) serveRaw(method string, path string, authorization string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.45:4000"
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	self.router.ServeHTTP(w, r)
	return w
}

// The network credential's session, as the model takes it.
func (self *routeAccessDbFixture) rootSession(t testing.TB) *session.ClientSession {
	t.Helper()
	return session.Testing_CreateClientSession(self.ctx, &session.ByJwt{NetworkId: self.networkId, UserId: self.userId, NetworkName: self.networkName})
}

// Dark: with the wake switch off, the route answers 503 with Retry-After before it authenticates, and
// writes nothing.
func TestSidecarPushTokenRouteIsDarkUntilTheWakeSwitch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)
		defer server.Testing_PushSidecarConfig(self.wakeYaml(false))()
		clientId, bearer := self.strictPushClient(t)
		for _, authorization := range []string{"", bearer, "Bearer not-a-token"} {
			w := self.serve(http.MethodPost, "/sidecar/push-token", authorization, map[string]any{"platform": "apns", "token": testRoutePushToken})
			if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
				t.Fatalf("%q: %d %v", authorization, w.Code, w.Header())
			}
		}
		if row, err := model.GetPushToken(ctx, clientId); err != nil || row != nil {
			t.Fatalf("a dark route wrote %v: %v", row, err)
		}
	})
}

// Open: a strict_exp client sets its token, deletes it, and is refused what the route does not take; a
// network credential, which names no client, is refused; an unmarked client token sets its own.
func TestSidecarPushTokenRouteSetsAndDeletes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)
		defer server.Testing_PushSidecarConfig(self.wakeYaml(true))()
		clientId, bearer := self.strictPushClient(t)

		w := self.serve(http.MethodPost, "/sidecar/push-token", bearer, map[string]any{"platform": "apns", "token": testRoutePushToken})
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" {
			t.Fatalf("set: %d %s", w.Code, w.Body.String())
		}
		row, err := model.GetPushToken(ctx, clientId)
		if err != nil || row == nil || row.Platform != sidecar.PushTokenPlatformApns || row.Token != testRoutePushToken {
			t.Fatalf("row %v: %v", row, err)
		}
		pushRedis := model.NewSidecarPushRedis(nil)
		tok, found, err := pushRedis.Get(ctx, pushservice.HintTokenKey(connect.Id(clientId)))
		if err != nil || !found || string(tok) != string(pushservice.WakeTokenValue(sidecar.PushTokenPlatformApns, testRoutePushToken)) {
			t.Fatalf("tok %q %t: %v", tok, found, err)
		}

		for _, body := range []map[string]any{
			{"platform": "gcm", "token": testRoutePushToken},
			{"platform": "", "token": testRoutePushToken},
			{"platform": "fcm", "token": "a b"},
			{"platform": "fcm", "token": strings.Repeat("a", sidecar.PushTokenMaxByteCount+1)},
		} {
			if w := self.serve(http.MethodPost, "/sidecar/push-token", bearer, body); w.Code != http.StatusBadRequest {
				t.Fatalf("%v: %d", body["platform"], w.Code)
			}
		}

		w = self.serve(http.MethodPost, "/sidecar/push-token", bearer, map[string]any{"platform": "", "token": ""})
		if w.Code != http.StatusOK {
			t.Fatalf("delete: %d %s", w.Code, w.Body.String())
		}
		deleted, err := model.GetPushToken(ctx, clientId)
		if err != nil || deleted == nil || !deleted.Tombstone() || deleted.Rev != row.Rev+1 {
			t.Fatalf("after the delete: %v %v", deleted, err)
		}
		if _, found, err := pushRedis.Get(ctx, pushservice.HintTokenKey(connect.Id(clientId))); err != nil || found {
			t.Fatalf("tok outlived its delete: %t %v", found, err)
		}

		if w := self.serve(http.MethodPost, "/sidecar/push-token", self.root(), map[string]any{"platform": "apns", "token": testRoutePushToken}); w.Code != http.StatusUnauthorized {
			t.Fatalf("a network credential: %d", w.Code)
		}

		legacyClientId, legacyToken := self.mintClient(t, self.root(), nil)
		if w := self.serve(http.MethodPost, "/sidecar/push-token", "Bearer "+legacyToken, map[string]any{"platform": "fcm", "token": testRoutePushToken}); w.Code != http.StatusOK {
			t.Fatalf("a client token: %d %s", w.Code, w.Body.String())
		}
		if row, err := model.GetPushToken(ctx, legacyClientId); err != nil || row == nil || row.Platform != sidecar.PushTokenPlatformFcm {
			t.Fatalf("the client's row %v: %v", row, err)
		}
	})
}

// No log line holds the token, at any verbosity, whatever the request: a set, an equal set, a delete, a
// refusal of a bad platform, a body that does not decode, and a removed client's set. Nor does any answer.
func TestNoTokenInLogs(t *testing.T) {
	logDir := t.TempDir()
	restore := func(name string) func() {
		value := flag.Lookup(name).Value.String()
		return func() { flag.Set(name, value) }
	}
	defer restore("logtostderr")()
	defer restore("v")()
	if err := glog.SetLogDir(logDir); err != nil {
		t.Fatal(err)
	}
	flag.Set("logtostderr", "false")
	flag.Set("v", "4")

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)
		defer server.Testing_PushSidecarConfig(self.wakeYaml(true))()
		clientId, bearer := self.strictPushClient(t)

		answers := []string{}
		post := func(authorization string, body string) {
			w := self.serveRaw(http.MethodPost, "/sidecar/push-token", authorization, body)
			answers = append(answers, fmt.Sprintf("%d %v %s", w.Code, w.Header(), w.Body.String()))
		}
		valid, err := json.Marshal(map[string]any{"platform": "apns", "token": testRoutePushToken})
		server.Raise(err)
		post(bearer, string(valid))
		post(bearer, string(valid))
		post(bearer, `{"platform":"gcm","token":"`+testRoutePushToken+`"}`)
		post(bearer, `{"platform":"apns","token":"`+testRoutePushToken+`"`)
		post(bearer, `{"platform":["`+testRoutePushToken+`"],"token":"`+testRoutePushToken+`"}`)
		post(bearer, `{"platform":"apns","token":""}`)
		post(bearer, string(valid))
		_, err = model.RemoveNetworkClient(&model.RemoveNetworkClientArgs{ClientId: clientId}, self.rootSession(t))
		server.Raise(err)
		post(bearer, string(valid))

		for _, answer := range answers {
			if strings.Contains(answer, testRoutePushToken) {
				t.Fatalf("an answer holds the token: %s", answer)
			}
		}
	})

	glog.Flush()
	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatal(err)
	}
	logBytes := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(logDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		logBytes += len(data)
		if strings.Contains(string(data), testRoutePushToken) {
			t.Fatalf("%s holds the token", entry.Name())
		}
	}
	if logBytes == 0 {
		t.Fatal("nothing was logged, so nothing was checked")
	}
}
