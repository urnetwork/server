package api

// End to end through the router the api serves, against the database and
// Redis: POST /connect/control limits each verified client on its own, and
// the limit holds across api instances, which share only Redis. A client over
// its limit is answered 429 with Retry-After (support inbox 8093, 8077).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
)

// A control request body with one keep-alive frame, which the api answers
// with no work beyond its checks.
func connectControlLimitTestBody(t testing.TB) []byte {
	t.Helper()
	frame, err := connect.ToFrame(&protocol.ControlPing{}, connect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	packBytes, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{frame}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(&controller.ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(packBytes)})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestConnectControlLimitHoldsPerClientAcrossApiInstances(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// five at once, then one a minute, on a fixed clock: the refusal's
		// wait is exactly the interval
		const nowMicros = int64(1_800_000_000_000_000)
		defer controller.Testing_SetConnectControlLimit(
			model.ConnectControlLimitSettings{RequestsPerMinute: 1, Burst: 4},
			func() int64 { return nowMicros },
		)()

		self := newRouteAccessDbFixture(t, ctx)
		// two api instances: each its own router, sharing only Redis and the
		// database, as the lb spreads one client's requests over the blocks
		instances := []*router.Router{self.router, router.NewRouter(ctx, Routes())}
		_, flooding := self.mintClient(t, self.root(), nil)
		_, other := self.mintClient(t, self.root(), nil)

		body := connectControlLimitTestBody(t)
		control := func(instance *router.Router, clientToken string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodPost, "/connect/control", bytes.NewReader(body))
			r.RemoteAddr = "198.51.100.42:4000"
			r.Header.Set("Authorization", "Bearer "+clientToken)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			instance.ServeHTTP(w, r)
			return w
		}
		requireStatus := func(name string, w *httptest.ResponseRecorder, status int) {
			t.Helper()
			if w.Code != status {
				t.Fatalf("%s: status %d, want %d: %s", name, w.Code, status, w.Body.String())
			}
		}

		// the flooding client alternates instances: its burst and one are
		// admitted in all, not on each
		for i := range 5 {
			w := control(instances[i%2], flooding)
			requireStatus("the flooding client within its budget", w, http.StatusOK)
			var result controller.ConnectControlResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Error != nil {
				t.Fatalf("an admitted control request = %s (%v)", w.Body.String(), err)
			}
		}
		for i := range 4 {
			w := control(instances[i%2], flooding)
			requireStatus("the flooding client over its budget", w, http.StatusTooManyRequests)
			if retryAfter := w.Header().Get("Retry-After"); retryAfter != "60" {
				t.Fatalf("Retry-After = %q, want the 60 s interval", retryAfter)
			}
		}

		// another client on the same instances has its own whole budget
		for i := range 5 {
			requireStatus("the other client within its budget", control(instances[i%2], other), http.StatusOK)
		}
		requireStatus("the other client over its budget", control(instances[1], other), http.StatusTooManyRequests)
	})
}
