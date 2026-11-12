//go:build sidecar_localstack

// S1 on T3 (§22.7; F1, I-6, D-TRUST-1): a sidecar's token, which api mints strict_exp and admin_refused through POST
// /network/auth-client as a sidecar's enrollment does, is refused POST /auth/code-create with 403 "A sidecar token
// cannot call this route." by server's own router, which a fake api proves nothing about, and the refused request
// stores nothing: the canary in its body reaches no object, Redis key or sidecar table, as the runner's scan of T3's
// stores shows, with the guard and the stored-state asserts beside it.
//
// S1's other two clauses, the token never in MCP, env or logs and a genesis other than the invite's refused, need a
// sidecar enrolled on T3 through server's api, which sidecart3 does not host yet (a T3 Net refuses AddSidecar until
// TEAM-M1-04's enrollment runs here): they read missing in sidecartest's scenario registry until they are built.

package sidecart3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// One request through server's router, as api's listener would serve it.
func serveApi(handler http.Handler, method string, path string, authorization string, body any) *httptest.ResponseRecorder {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		server.Raise(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	// RFC 5737's documentation range
	request.RemoteAddr = "198.51.100.71:4000"
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestScenarioS1CodeCreateOnT3(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(tb testing.TB) {
		RunScenario(t, &sidecartest.Scenario{Id: "S1", Run: func(ctx context.Context, run *sidecartest.ScenarioRun) error {
			apiCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			handler := ApiRouter(apiCtx)
			networkId := server.NewId()
			userId := server.NewId()
			networkName := fmt.Sprintf("s1%s", strings.ReplaceAll(networkId.String(), "-", "")[:12])
			model.Testing_CreateNetwork(apiCtx, networkId, networkName, userId)
			defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", networkId))()
			root := "Bearer " + session.NewByJwt(networkId, userId, networkName, false, false).Testing_Sign()

			// the sidecar's client, marked as its enrollment marks it
			minted := serveApi(handler, http.MethodPost, "/network/auth-client", root, map[string]any{
				"description": "sidecar", "device_spec": "sidecar", "admin_refused": true, "strict_exp": true,
			})
			if minted.Code != http.StatusOK {
				return fmt.Errorf("auth-client: %d %s", minted.Code, minted.Body.String())
			}
			result := &model.AuthNetworkClientResult{}
			if err := json.Unmarshal(minted.Body.Bytes(), result); err != nil {
				return err
			}
			if result.ByClientJwt == nil || !result.StrictExp || !result.AdminRefused {
				return fmt.Errorf("the client is not a sidecar's: %s", minted.Body.String())
			}
			run.Net.Capture().AddSecret("a sidecar's client token", []byte(*result.ByClientJwt))

			// code-create with the sidecar's token: 403, never 401, and nothing of the request kept
			refused := serveApi(handler, http.MethodPost, "/auth/code-create", "Bearer "+*result.ByClientJwt, map[string]any{
				"uses": 1, "duration_minutes": 5, "description": run.CanaryText("a code's description"),
			})
			if refused.Code != http.StatusForbidden || strings.TrimSpace(refused.Body.String()) != router.StrictExpCredentialRefusedMessage {
				return fmt.Errorf("code-create with a sidecar's token: %d %q", refused.Code, refused.Body.String())
			}
			// the same route with the network's own credential is the route's own, so the 403 is the token's
			if allowed := serveApi(handler, http.MethodPost, "/auth/code-create", root, map[string]any{"uses": 1, "duration_minutes": 5}); allowed.Code != http.StatusOK {
				return fmt.Errorf("code-create with the network's credential: %d %s", allowed.Code, allowed.Body.String())
			}
			// server's own table holds the route off the allowlist, and the scenario's gate is that table
			if ServerRouteAllowed(http.MethodPost, "/auth/code-create") || !ServerRouteAllowed(http.MethodGet, "/auth/refresh") {
				return fmt.Errorf("server's route table and §7.4 part at /auth/code-create or /auth/refresh")
			}
			return nil
		}}, nil)
	})
}
