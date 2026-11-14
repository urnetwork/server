package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urfoundation/sn/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Posts a reserved upload through the router the api serves, as the lb delivers
// one: the body is complete and the header carries an opaque consent.
func serveReservedUploadRequest(t testing.TB, handler http.Handler, token string, consent ...string) *httptest.ResponseRecorder {
	t.Helper()
	const data = "{\"lane\":1}\n"
	query := url.Values{"kind": {"metadata"}, "hash": {fmt.Sprintf("0x%x", sha256.Sum256([]byte(data)))}}
	request := httptest.NewRequest(http.MethodPost, "/sn/attempt-artifact?"+query.Encode(), strings.NewReader(data))
	request.RemoteAddr = "198.51.100.32:4000"
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	for _, value := range consent {
		request.Header.Add(protocol.ValidatorAttemptUploadHeader, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// A process whose lane failed to start answers a validator with a status every
// deployed validator and the lb treat as retryable (a 503: the lb tries a
// sibling block, the validator's next poll tries again without spending its
// failure budget). The 403 it answered counted toward the panic that restarts
// the validator, and in the settlement refresh ended the release at once.
//
// Only a real refusal stays a 403: a lane no configuration asks for, and a
// consent header that is not one value.
func TestReservedUploadLaneOffAnswersRetryably(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer controller.SetStConfig(nil)
		t.Cleanup(func() { reservedUploadStartupFailedGauge.Set(0) })
		networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(t.Context(), networkId, "reserved-lane-"+networkId.String(), userId)
		model.Testing_CreateDevice(t.Context(), networkId, deviceId, clientId, "reserved-lane", "test")
		token := session.NewByJwt(networkId, userId, "reserved-lane", false, false).Client(deviceId, clientId).Testing_Sign()

		// A configured lane whose startup fails: its authority names no deployment.
		controller.SetStConfig(&controller.StConfig{Enabled: true, DeploymentId: "reserved-lane-off", ReservedAttemptUpload: &controller.StReservedAttemptUploadConfig{}})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		apiRouter, closeRouter, err := NewRouter(ctx, ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := testutil.ToFloat64(reservedUploadStartupFailedGauge); got != 1 {
			t.Fatalf("startup failure gauge = %v, want 1", got)
		}
		response := serveReservedUploadRequest(t, apiRouter, token, "opaque-consent")
		if response.Code != http.StatusServiceUnavailable || response.Body.String() != "Validator reserved upload is not running on this instance.\n" {
			t.Fatalf("lane off: status=%d body=%q, want 503 and the instance's retryable answer", response.Code, response.Body.String())
		}
		response = serveReservedUploadRequest(t, apiRouter, token, "one", "two")
		if response.Code != http.StatusForbidden || response.Body.String() != "Validator reserved upload is unavailable or ambiguous.\n" {
			t.Fatalf("an ambiguous consent with the lane off: status=%d body=%q, want the 403", response.Code, response.Body.String())
		}
		// the retry ends with the router's close
		closeRouter()
		cancel()

		// No lane is configured here, on any instance. A refusal, not a wait.
		controller.SetStConfig(&controller.StConfig{Enabled: true, DeploymentId: "reserved-lane-none"})
		ctx, cancel = context.WithCancel(t.Context())
		defer cancel()
		apiRouter, closeRouter, err = NewRouter(ctx, ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer closeRouter()
		if got := testutil.ToFloat64(reservedUploadStartupFailedGauge); got != 0 {
			t.Fatalf("startup failure gauge = %v without a configured lane, want 0", got)
		}
		response = serveReservedUploadRequest(t, apiRouter, token, "opaque-consent")
		if response.Code != http.StatusForbidden || response.Body.String() != "Validator reserved upload is unavailable or ambiguous.\n" {
			t.Fatalf("no configured lane: status=%d body=%q, want the 403", response.Code, response.Body.String())
		}
	})
}
