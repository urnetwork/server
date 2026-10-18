// A terminal renewal's second Google fetch must keep the reconciliation
// session even while the ordinary PostgreSQL pool is deliberately occupied.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The real selected-store run sees ACTIVE first, then every supported terminal
// transition and one still-active control. The existing acquisition tracer
// cancels a nested checkout at entry, making the pre-fix failure immediate.
func TestPaymentReconcilePlaySecondFetchKeepsRetainedConnection(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		pop := server.Config.PushSimpleResource(server.DefaultPgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
		popMaintenance := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() { popMaintenance(); pop(); server.PgReset() }()
		defer disableAllReconcileStores(t)()
		playEnv := newPlayWebhookTestEnv(t, map[string]*Sku{})
		playReconcileHasCredentials = func() bool { return true }
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Second)
		start := now.Add(-24 * time.Hour)
		expiry := now.Add(24 * time.Hour)
		// No account link or sku authorizes repair from the first response.
		// This isolates session ownership from stale-first-response repair.
		type playSecondFetchCase struct {
			networkId server.Id
			token     string
			state     string
			terminal  bool
			first     *PlaySubscription
			second    *PlaySubscription
			calls     atomic.Int32
		}
		cases := []*playSecondFetchCase{}
		caseKVs := map[string]*playSecondFetchCase{}
		for index, state := range []string{"gone", "SUBSCRIPTION_STATE_EXPIRED", "SUBSCRIPTION_STATE_PENDING_PURCHASE_CANCELED", "SUBSCRIPTION_STATE_CANCELED", "SUBSCRIPTION_STATE_ACTIVE"} {
			networkId := server.NewId()
			token := fmt.Sprintf("synthetic-second-fetch-%d", index)
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("syntheticsecondfetch%d", index), server.NewId())
			seedPlayTerminalEntitlement(t, ctx, networkId, token, start, expiry.Add(SubscriptionGracePeriod), true)
			seedPlayTerminalEntitlement(t, ctx, networkId, token, start, expiry.Add(SubscriptionGracePeriod), false)
			first := playTestSubscription(networkId, playTerminalTestSubscriptionId, start, expiry)
			first.ExternalAccountIdentifiers = nil
			second := playTestSubscription(networkId, playTerminalTestSubscriptionId, start, expiry)
			second.ExternalAccountIdentifiers = nil
			second.SubscriptionState = state
			terminal := state != "SUBSCRIPTION_STATE_ACTIVE"
			if terminal {
				second.LineItems[0].ExpiryTime = now.Add(-time.Minute).Format(time.RFC3339)
			}
			test := &playSecondFetchCase{networkId: networkId, token: token, state: state, terminal: terminal, first: first, second: second}
			cases = append(cases, test)
			caseKVs[token] = test
		}
		prefix := fmt.Sprintf("/androidpublisher/v3/applications/%s/purchases/subscriptionsv2/tokens/", playEnv.packageName)
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			test := caseKVs[strings.TrimPrefix(request.URL.Path, prefix)]
			if test == nil || request.Method != http.MethodGet {
				t.Error("unexpected synthetic Play request", request.Method, request.URL.Path)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			call := test.calls.Add(1)
			response := test.first
			if call == 2 {
				if test.state == "gone" {
					w.WriteHeader(http.StatusGone)
					return
				}
				response = test.second
			} else if call != 1 {
				t.Error("unexpected repeated synthetic Play fetch", test.token, call)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(response); err != nil {
				t.Error(err)
			}
		}))
		defer endpoint.Close()
		playPublisherApiBaseUrl = endpoint.URL
		owner := reconcileTestSession(t, ctx)

		ready := make(chan error, 1)
		release := make(chan struct{})
		holderDone := make(chan struct{})
		go func() {
			defer close(holderDone)
			server.HandleError(func() {
				server.Db(ctx, func(server.PgConn) {
					ready <- nil
					<-release
				}, server.OptNoRetry())
			}, func(err error) { ready <- err })
		}()
		var releaseOnce sync.Once
		releaseOrdinary := func() {
			releaseOnce.Do(func() { close(release) })
			<-holderDone
		}
		defer releaseOrdinary()
		if err := <-ready; err != nil {
			t.Fatal("could not occupy the ordinary fixture pool", err)
		}
		var result *PaymentReconcileRunResult
		var runErr error
		server.HandleError(func() {
			result, runErr = RunPaymentReconciliationWithOptions(owner, &PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketGoogle}})
		}, func(err error) { runErr = err })
		state := owner.Ctx.Value(paymentReconcileConnectionKey{}).(*paymentReconcileConnectionState)
		state.mutex.Lock()
		acquisitions, transactions, retained := state.acquisitions, state.transactions, state.owner != nil
		state.mutex.Unlock()
		// Assertions below use the ordinary pool only after the owned run ended.
		releaseOrdinary()
		if runErr != nil || result == nil || result.Errors != 0 || context.Cause(owner.Ctx) != nil {
			t.Fatal("second-fetch reconciliation failed with the ordinary pool occupied", result, runErr, context.Cause(owner.Ctx))
		}
		if acquisitions != 1 || transactions < 4 || retained {
			t.Fatal("terminal second fetch failed to reuse and release the run session", acquisitions, transactions, retained)
		}
		for _, test := range cases {
			wantActive := 1
			if test.terminal {
				wantActive = 0
			}
			if test.calls.Load() != 2 || countActivePlayRenewals(t, ctx, test.networkId, test.token) != wantActive ||
				countActivePlayBalances(t, ctx, test.networkId, test.token, true) != wantActive ||
				countActivePlayBalances(t, ctx, test.networkId, test.token, false) != 1 {
				t.Fatal("second-fetch outcome changed its purchase scope or missed the terminal write", test.state, test.calls.Load())
			}
			if model.IsProNetwork(ctx, test.networkId) != !test.terminal {
				t.Fatal("second-fetch terminal write did not refresh the pro projection", test.state)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var locked bool
			server.Raise(conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, paymentReconcileRunLockKey).Scan(&locked))
			if !locked {
				t.Error("terminal second-fetch run retained its advisory lock")
				return
			}
			server.RaisePgResult(conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, paymentReconcileRunLockKey))
		}, server.OptNoRetry())
	})
}
