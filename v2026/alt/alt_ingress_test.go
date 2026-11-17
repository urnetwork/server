package alt

// The api front's ingress headers end to end (ingress.go): a real http3
// client of the fixture alt, over the H3 listener and over whodis, forging the
// forwarding headers a client of alt can send.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/router"
)

// A client of alt that names another address in X-UR-Forwarded-For, and sends
// the standard forwarding headers and the lb's streaming mark, is still the
// QUIC peer it dialed from to every route behind the api front. Before alt set
// the header itself, the session took the forged value: the route acted for
// 203.0.113.7:443.
func TestAltApiFrontResolvesTheQuicPeerOverAForgedForwardedFor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newAltEnv(ctx, t, nil)
		defer env.Close()

		for _, whodis := range []bool{false, true} {
			client := env.apiClient(testApiHost, whodis)
			request, err := http.NewRequestWithContext(
				ctx,
				http.MethodGet,
				fmt.Sprintf("https://%s/client-address", testApiHost),
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-UR-Forwarded-For", "203.0.113.7:443")
			request.Header.Set("X-Forwarded-For", "203.0.113.8")
			request.Header.Set("X-Forwarded-Source-Port", "41001")
			request.Header.Set(router.RequestBufferingHeader, router.RequestBufferingOff)
			requestCtx, requestCancel := context.WithTimeout(ctx, testProgressTimeout)
			response, err := client.Do(request.WithContext(requestCtx))
			if err != nil {
				requestCancel()
				t.Fatalf("whodis=%t: %v", whodis, err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			requestCancel()
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("whodis=%t: %d %q", whodis, response.StatusCode, body)
			}
			result := &testClientAddressResult{}
			if err := json.Unmarshal(body, result); err != nil {
				t.Fatalf("whodis=%t: %v %q", whodis, err, body)
			}

			var peer string
			select {
			case peer = <-env.apiClientLocalAddrs:
			case <-time.After(testProgressTimeout):
				t.Fatalf("whodis=%t: the client dialed no socket", whodis)
			}
			if result.ClientAddress != peer {
				t.Fatalf("whodis=%t: the quic peer %s resolved as client %q", whodis, peer, result.ClientAddress)
			}
			if !slices.Equal(result.UrForwardedFor, []string{peer}) {
				t.Fatalf("whodis=%t: X-UR-Forwarded-For reached the router as %q, want [%q]", whodis, result.UrForwardedFor, peer)
			}
			if len(result.ForwardedFor) != 0 || len(result.ForwardedSourcePort) != 0 || len(result.RequestBuffering) != 0 {
				t.Fatalf("whodis=%t: forwarding headers reached the router: %+v", whodis, result)
			}
		}
	})
}
