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
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// An activation whose budget is held past the lock bound by another spend --
// another api process's, which the in-process lock does not see -- is answered
// as busy, with the wait in retry_after_seconds and nothing spent
// (connect#230). Before, the wait's 55P03 raised out of the spend and the
// route answered it as an unexpected 500, which the activator backed off ten
// minutes doubling from and which the 5xx alert counts.
func TestExtenderActivateAnswersABudgetLockWaitAsBusy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installExtenderActivateTestConfig(t)

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

		networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "extender-busy-"+networkId.String(), userId)
		model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "extender", "extender-busy")
		byJwt := session.NewByJwt(networkId, userId, "extender-busy", false, false).Client(deviceId, clientId)

		// another process's spend of the account's ipv4 budget holds its
		// database lock until the answer is in
		lockKey := fmt.Sprintf("account-action-budget/v1:%s:%s", model.AccountActionExtenderActivateAccountV4, userId)
		held := make(chan struct{})
		answered := make(chan struct{})
		released := make(chan struct{})
		go func() {
			defer close(released)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey))
				close(held)
				<-answered
			}, server.TxReadCommitted)
		}()
		<-held

		apiRouter := router.NewRouter(ctx, []*router.Route{
			router.NewRoute("POST", "/network/extender-activate", ExtenderActivate),
		})
		req := httptest.NewRequest(http.MethodPost, "/network/extender-activate", strings.NewReader(body))
		req.Host = "api-v4.example"
		req.RemoteAddr = "127.0.0.1:54321"
		req.Header.Set("Authorization", "Bearer "+byJwt.Testing_Sign())
		w := httptest.NewRecorder()
		start := time.Now()
		apiRouter.ServeHTTP(w, req)
		close(answered)
		<-released
		t.Logf("answered %d after %s: %s", w.Code, time.Since(start).Round(time.Millisecond), strings.TrimSpace(w.Body.String()))

		if w.Code != http.StatusOK {
			t.Fatalf("a budget lock wait past its bound was answered %d, not the busy refusal", w.Code)
		}
		result := map[string]any{}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("the answer is not json: %v", err)
		}
		if activated, _ := result["activated"].(bool); activated {
			t.Fatalf("the activation was taken while its budget was held: %v", result)
		}
		if message, _ := result["error"].(string); !strings.Contains(message, "busy") {
			t.Errorf("the refusal %q does not say the operator is busy", message)
		}
		if retryAfterSeconds, _ := result["retry_after_seconds"].(float64); retryAfterSeconds <= 0 {
			t.Errorf("the busy refusal asks for no wait: %v", result)
		}
		if count := extenderActivateTestAttemptCount(ctx, userId) + extenderActivateTestAttemptCount(ctx, clientId); count != 0 {
			t.Errorf("the busy refusal spent %d budget attempts", count)
		}
	})
}
