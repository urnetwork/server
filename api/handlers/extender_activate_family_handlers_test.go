package handlers

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// `POST /network/extender-activate` and the family an activation is for
// (connect/EXTENDER.md C2, support inbox 8083, connect#229).

// Installs an extender network configuration with a fresh root key, as an
// operator that can activate extenders has.
func installExtenderActivateTestConfig(t testing.TB) {
	t.Helper()
	rootKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(rootKeySeed)
	if err != nil {
		t.Fatal(err)
	}
	pop := server.Vault.PushSimpleResource("extender.yml", []byte(strings.Join([]string{
		fmt.Sprintf("root_private_key_hex: %s", connect.ExtenderKeySeedHex(rootKeySeed)),
		"root_public_keys_hex:",
		fmt.Sprintf("  - %s", hex.EncodeToString(rootPrivateKey.Public().(ed25519.PublicKey))),
		"network_host: ur.example",
		"network_hosts:",
		"  - ur.example",
		"  - api.example",
		"api_url: https://api.example",
		"dns:",
		"  enabled: false",
	}, "\n")))
	controller.Testing_ResetExtenderConfig()
	t.Cleanup(func() {
		pop()
		controller.Testing_ResetExtenderConfig()
	})
}

// The budget attempts recorded under one id, over every action.
func extenderActivateTestAttemptCount(ctx context.Context, id server.Id) int {
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COUNT(*) FROM network_user_action_attempt WHERE user_id = $1`,
			id,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

// An activation posted to one family's api host that arrives over the other
// family is refused before anything is probed, with both families named, and
// spends no budget. Before, the handler probed whatever family the request
// arrived on, so an ipv4 activation that an alt path carried over ipv6
// activated the caller's ipv6 address and answered as activated, and the
// extender recorded it as its ipv4 (connect#229).
//
// This drives the whole request: the host the request was addressed to, the
// address it arrived from and a real client jwt, through the real handler. An
// activation that arrives on its host's own family goes on to the probe, which
// finds nothing listening and is counted.
func TestExtenderActivateRefusesAPostThatArrivedOnTheOtherFamily(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installExtenderActivateTestConfig(t)

		// a tcp port nothing is bound to, so a probe that does run is refused
		// at once
		closedListener, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatal(err)
		}
		closedPort := closedListener.Addr().(*net.TCPAddr).Port
		closedListener.Close()
		body := fmt.Sprintf(
			`{"public_key_hex":%q,"tcp_port":%d,"carriers":["tcp"]}`,
			hex.EncodeToString(make([]byte, ed25519.PublicKeySize)),
			closedPort,
		)

		cases := []struct {
			host        string
			remoteAddr  string
			wantError   string
			wantSpent   int
			description string
		}{
			{
				host:        "api-v4.example",
				remoteAddr:  "[::1]:54321",
				wantError:   "the activation for ipv4 arrived over ipv6",
				wantSpent:   0,
				description: "an ipv4 activation over ipv6",
			},
			{
				host:        "api-v6.example:443",
				remoteAddr:  "127.0.0.1:54321",
				wantError:   "the activation for ipv6 arrived over ipv4",
				wantSpent:   0,
				description: "an ipv6 activation over ipv4",
			},
			{
				host:        "api-v4.example",
				remoteAddr:  "127.0.0.1:54321",
				wantError:   "tcp carrier",
				wantSpent:   1,
				description: "an ipv4 activation over ipv4",
			},
		}
		for _, c := range cases {
			networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, "extender-family-"+networkId.String(), userId)
			model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "extender", "extender-test")
			byJwt := session.NewByJwt(networkId, userId, "extender-family", false, false).Client(deviceId, clientId)

			req := httptest.NewRequest(http.MethodPost, "/network/extender-activate", strings.NewReader(body))
			req.Host = c.host
			req.RemoteAddr = c.remoteAddr
			req.Header.Set("Authorization", "Bearer "+byJwt.Testing_Sign())
			w := httptest.NewRecorder()
			ExtenderActivate(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("%s: status = %d (%s), want a 200 answer", c.description, w.Code, strings.TrimSpace(w.Body.String()))
			}
			result := &controller.ExtenderActivateResult{}
			if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
				t.Fatalf("%s: the answer did not decode: %v", c.description, err)
			}
			if result.Activated || !strings.Contains(result.Error, c.wantError) {
				t.Errorf("%s: activated %t, error %q, want a refusal %q", c.description, result.Activated, result.Error, c.wantError)
			}
			if spent := extenderActivateTestAttemptCount(ctx, clientId); spent != c.wantSpent {
				t.Errorf("%s: %d of the client's budget was spent, want %d", c.description, spent, c.wantSpent)
			}
			if spent := extenderActivateTestAttemptCount(ctx, userId); spent != c.wantSpent {
				t.Errorf("%s: %d of the account's budget was spent, want %d", c.description, spent, c.wantSpent)
			}
		}
	})
}
