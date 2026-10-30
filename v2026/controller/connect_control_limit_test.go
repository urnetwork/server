package controller

// How /connect/control applies the per-client limit before the client state
// query (connect_control_limit.go), with the decision replaced: these tests
// read no PostgreSQL or Redis. The decision itself is tested against Redis in
// model/connect_control_limit_test.go, and end to end through the api router
// in api/connect_control_limit_db_test.go (support inbox 8093, 8077).

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// One decision of the replaced limit.
type connectControlLimitTestCall struct {
	clientId  server.Id
	settings  model.ConnectControlLimitSettings
	nowMicros int64
}

// Replaces the decision with answer, records each call, and applies the
// infrastructure check excluded, until the test ends.
func setConnectControlLimitTestAdmit(
	t *testing.T,
	answer func(server.Id) (model.ConnectControlLimitResult, error),
	excluded func(netip.Addr) bool,
) *[]connectControlLimitTestCall {
	t.Helper()
	calls := &[]connectControlLimitTestCall{}
	previousAdmit, previousExcluded := connectControlLimitAdmit, connectControlLimitExcluded
	connectControlLimitAdmit = func(ctx context.Context, clientId server.Id, settings model.ConnectControlLimitSettings, nowMicros int64) (model.ConnectControlLimitResult, error) {
		*calls = append(*calls, connectControlLimitTestCall{clientId: clientId, settings: settings, nowMicros: nowMicros})
		return answer(clientId)
	}
	if excluded != nil {
		connectControlLimitExcluded = excluded
	}
	t.Cleanup(func() {
		connectControlLimitAdmit, connectControlLimitExcluded = previousAdmit, previousExcluded
	})
	return calls
}

// The context a request's session gives a BeforeStateAnswer: the session's,
// with the address the ingress header carried.
func connectControlLimitTestSessionContext(t *testing.T, clientAddress string) context.Context {
	t.Helper()
	request := httptest.NewRequest("POST", "/connect/control", nil)
	request.RemoteAddr = "10.0.0.1:443"
	request.Header.Set("X-UR-Forwarded-For", clientAddress)
	clientSession, err := session.NewClientSessionFromRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clientSession.Cancel)
	return clientSession.Ctx
}

// A verified client over its limit is answered before its state query with a
// 429 that names its wait, and the decision is the client's own: keyed on the
// verified client id, with the process's settings and the shared clock.
func TestPrepareConnectControlAnswersAClientOverItsLimit(t *testing.T) {
	limitedId, allowedId := server.NewId(), server.NewId()
	calls := setConnectControlLimitTestAdmit(t, func(clientId server.Id) (model.ConnectControlLimitResult, error) {
		if clientId == limitedId {
			return model.ConnectControlLimitResult{Allowed: false, RetryAfter: 1500 * time.Millisecond}, nil
		}
		return model.ConnectControlLimitResult{Allowed: true}, nil
	}, nil)
	defer Testing_SetConnectControlLimit(model.ConnectControlLimitSettings{RequestsPerMinute: 60, Burst: 4}, nil)()
	ctx := connectControlLimitTestSessionContext(t, "198.51.100.42:4000")

	limitedBefore := refusalReplayCounter(t, "urnetwork_connect_control_client_limit_total", map[string]string{"outcome": "limited"})
	allowedBefore := refusalReplayCounter(t, "urnetwork_connect_control_client_limit_total", map[string]string{"outcome": "allowed"})

	answer, answered := PrepareConnectControl(ctx, limitedId, refusalReplayArgs(t, refusalReplayCreate(server.NewId(), true)))
	if !answered || answer == nil {
		t.Fatal("a client over its limit was not answered before its state query")
	}
	result, err := ReplyConnectControl(ctx, answer)
	if result != nil || err == nil || !strings.HasPrefix(err.Error(), "429 ") {
		t.Fatalf("the answer = %v, %v; want a 429", result, err)
	}
	var retryAfter interface{ RetryAfterSeconds() int }
	if !errors.As(err, &retryAfter) || retryAfter.RetryAfterSeconds() != 2 {
		t.Fatalf("retry after = %v, want 2 s", retryAfter)
	}

	// an allowed client goes on to its state query: no origin notifications
	// owner here, so no refusal can be replayed
	if answer, answered := PrepareConnectControl(ctx, allowedId, refusalReplayArgs(t, refusalReplayCreate(server.NewId(), true))); answered || answer != nil {
		t.Fatalf("an allowed client was answered before its state query: %+v", answer)
	}

	if len(*calls) != 2 || (*calls)[0].clientId != limitedId || (*calls)[1].clientId != allowedId {
		t.Fatalf("decisions = %+v, want one per request keyed on the verified client id", *calls)
	}
	for _, call := range *calls {
		if call.settings != (model.ConnectControlLimitSettings{RequestsPerMinute: 60, Burst: 4}) || call.nowMicros != -1 {
			t.Fatalf("decision %+v, want the process settings and the Redis node's clock (-1)", call)
		}
	}
	if limited := refusalReplayCounter(t, "urnetwork_connect_control_client_limit_total", map[string]string{"outcome": "limited"}) - limitedBefore; limited != 1 {
		t.Fatalf("limited decisions counted %v, want 1", limited)
	}
	if allowed := refusalReplayCounter(t, "urnetwork_connect_control_client_limit_total", map[string]string{"outcome": "allowed"}) - allowedBefore; allowed != 1 {
		t.Fatalf("allowed decisions counted %v, want 1", allowed)
	}
}

// An allowed request is still answered from the refusals recorded for it, so
// the limit and the refusal replay compose: the limit counts it first.
func TestPrepareConnectControlReplaysRefusalsOfAnAllowedClient(t *testing.T) {
	calls := setConnectControlLimitTestAdmit(t, func(server.Id) (model.ConnectControlLimitResult, error) {
		return model.ConnectControlLimitResult{Allowed: true}, nil
	}, nil)
	ctx := newRefusalReplayOwnerContext(t)
	owner := model.GetContractOriginNotifications(ctx)
	clientId, retiredId := server.NewId(), server.NewId()
	owner.RecordRouteRejection(clientId, retiredId, false, refusalReplayInactive(), nil)

	answer, answered := PrepareConnectControl(ctx, clientId, refusalReplayArgs(t, refusalReplayCreate(retiredId, false)))
	if !answered || answer == nil || answer.limited || answer.refusals == nil || answer.refusals.cause != "inactive_destination" {
		t.Fatalf("answer = %+v, %t; want the recorded refusal", answer, answered)
	}
	if len(*calls) != 1 {
		t.Fatalf("decisions = %d, want the request counted once", len(*calls))
	}
}

// Infrastructure (exclude_subnets) is exempt and spends no budget, as at the
// load balancer: its decision is never asked for.
func TestPrepareConnectControlExemptsInfrastructureAddresses(t *testing.T) {
	infrastructure := netip.MustParseAddr("192.0.2.7")
	calls := setConnectControlLimitTestAdmit(t, func(server.Id) (model.ConnectControlLimitResult, error) {
		return model.ConnectControlLimitResult{Allowed: false, RetryAfter: time.Second}, nil
	}, func(addr netip.Addr) bool { return addr == infrastructure })

	excludedBefore := refusalReplayCounter(t, "urnetwork_connect_control_client_limit_total", map[string]string{"outcome": "excluded"})
	ctx := connectControlLimitTestSessionContext(t, "192.0.2.7:5000")
	if answer, answered := PrepareConnectControl(ctx, server.NewId(), refusalReplayArgs(t, refusalReplayCreate(server.NewId(), true))); answered || answer != nil {
		t.Fatalf("an infrastructure address was answered: %+v", answer)
	}
	if len(*calls) != 0 {
		t.Fatalf("an infrastructure address spent budget: %+v", *calls)
	}
	if excluded := refusalReplayCounter(t, "urnetwork_connect_control_client_limit_total", map[string]string{"outcome": "excluded"}) - excludedBefore; excluded != 1 {
		t.Fatalf("excluded decisions counted %v, want 1", excluded)
	}

	// any other address is decided, and refused here
	ctx = connectControlLimitTestSessionContext(t, "198.51.100.42:4000")
	if _, answered := PrepareConnectControl(ctx, server.NewId(), refusalReplayArgs(t, refusalReplayCreate(server.NewId(), true))); !answered || len(*calls) != 1 {
		t.Fatalf("a public address was not decided: answered %t, %d decisions", answered, len(*calls))
	}
}

// A Redis failure admits the request undecided: the limit never refuses
// because Redis is slow or down.
func TestPrepareConnectControlAdmitsUndecidedWhenRedisFails(t *testing.T) {
	setConnectControlLimitTestAdmit(t, func(server.Id) (model.ConnectControlLimitResult, error) {
		return model.ConnectControlLimitResult{}, context.DeadlineExceeded
	}, nil)
	unavailableBefore := refusalReplayCounter(t, "urnetwork_connect_control_client_limit_total", map[string]string{"outcome": "unavailable"})
	ctx := connectControlLimitTestSessionContext(t, "198.51.100.42:4000")
	if answer, answered := PrepareConnectControl(ctx, server.NewId(), refusalReplayArgs(t, refusalReplayCreate(server.NewId(), true))); answered || answer != nil {
		t.Fatalf("a request with no decision was answered: %+v", answer)
	}
	if unavailable := refusalReplayCounter(t, "urnetwork_connect_control_client_limit_total", map[string]string{"outcome": "unavailable"}) - unavailableBefore; unavailable != 1 {
		t.Fatalf("unavailable decisions counted %v, want 1", unavailable)
	}
}
