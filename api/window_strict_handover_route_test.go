package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The api router's find-providers2 answers carry the Redis control's
// windowStrictHandover: every router reads the control (api/run.go), so once
// the operator turns the switch off an answer carries
// "window_strict_handover": false, and once the field is gone, at the
// router's next read of the control, an answer carries none.
func TestFindProviders2RouteCarriesTheWindowStrictHandoverControl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		withDeadline := func(f func(ctx context.Context) error) {
			t.Helper()
			deadlineCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := f(deadlineCtx); err != nil {
				t.Fatal(err)
			}
		}
		withDeadline(func(ctx context.Context) error {
			return model.SetConnectMoveControl(ctx, map[string]string{
				model.ConnectMoveControlFieldWindowStrictHandover: "off",
			})
		})
		apiRouter, closeRouter, err := NewRouter(ctx, ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer closeRouter()

		answer := func() (value any, present bool) {
			t.Helper()
			request := httptest.NewRequest(
				http.MethodPost,
				"/network/find-providers2",
				strings.NewReader(`{"specs": [{"best_available": true}], "count": 1}`),
			)
			request.RemoteAddr = "198.51.100.41:4000"
			response := httptest.NewRecorder()
			apiRouter.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("find-providers2 answered %d: %s", response.Code, response.Body.String())
			}
			values := map[string]any{}
			if err := json.Unmarshal(response.Body.Bytes(), &values); err != nil {
				t.Fatal(err)
			}
			value, present = values["window_strict_handover"]
			return value, present
		}
		waitFor := func(name string, want func(value any, present bool) bool, timeout time.Duration) {
			t.Helper()
			deadline := time.Now().Add(timeout)
			for {
				value, present := answer()
				if want(value, present) {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s: the answer's window_strict_handover = %v (present %t)", name, value, present)
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(50 * time.Millisecond):
				}
			}
		}

		// the router reads the control as it starts
		waitFor("turned off", func(value any, present bool) bool {
			return present && value == false
		}, 10*time.Second)

		// the operator removes the field: the router's next read overrides
		// nothing
		withDeadline(func(ctx context.Context) error {
			return model.RemoveConnectMoveControl(ctx)
		})
		refreshInterval := controller.DefaultWindowStrictHandoverControlSettings().ControlRefreshInterval
		waitFor("removed", func(value any, present bool) bool {
			return !present
		}, 2*refreshInterval+5*time.Second)
	})
}
