// Actual hosted operations expose per-request, per-pack and duplicate-wrapper work.
package localclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"google.golang.org/protobuf/proto"
)

// A bounded-label snapshot; gathering does not acquire a database connection.
func authorityStateQueryMetric(t testing.TB, caller, operation, credential, outcome string) float64 {
	t.Helper()
	return authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", map[string]string{"caller": caller, "operation": operation, "credential": credential, "outcome": outcome})
}

// Exact labels select one cell; nil labels sum all fixed cells of a family.
func authorityObservedCounter(t testing.TB, name string, want map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		var sum float64
		for _, metric := range family.Metric {
			sum += metric.GetCounter().GetValue()
			matched := len(metric.Label) == len(want)
			for _, label := range metric.Label {
				matched = matched && want[label.GetName()] == label.GetValue()
			}
			if matched {
				return metric.GetCounter().GetValue()
			}
		}
		if want == nil {
			return sum
		}
	}
	t.Fatalf("counter or child is missing: %s", name)
	return 0
}

// The retained raw mint wrapper validates twice; typed SDK mint validates
// once. This measures current behavior without silently changing authority.
func TestAuthorityStateQueryMetricActualOperations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(session.WithStateQuerySource(t.Context(), session.StateQueryApiControl), 30*time.Second)
		defer cancel()
		before := authorityStateQueryMetric(t, "hosted", "bootstrap", "client", "state_valid")
		owner, parent := authorityTestOwner(t, ctx)
		var apiCalls atomic.Int64
		apiTrap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiCalls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer apiTrap.Close()
		owner.apiUrl = apiTrap.URL
		if authorityStateQueryMetric(t, "hosted", "bootstrap", "client", "state_valid") != before+1 {
			t.Fatal("bootstrap source missing")
		}
		before = authorityStateQueryMetric(t, "hosted", "mint", "client", "state_valid")
		child, childClaims := authorityTestMint(t, ctx, owner)
		if authorityStateQueryMetric(t, "hosted", "mint", "client", "state_valid") != before+1 {
			t.Fatal("typed mint did not report one parent validation")
		}
		source := connect.Id(owner.clientId)
		body, err := json.Marshal(&connect.AuthNetworkClientArgs{SourceClientId: &source, Description: "synthetic", DeviceSpec: "synthetic"})
		if err != nil {
			t.Fatal(err)
		}
		before = authorityStateQueryMetric(t, "hosted", "mint", "client", "state_valid")
		wire, err := owner.Post(ctx, owner.apiUrl+"/network/auth-client", body, owner.token())
		var rawMint connect.AuthNetworkClientResult
		if err != nil || json.Unmarshal(wire, &rawMint) != nil || rawMint.ByClientJwt == "" || rawMint.Error != nil {
			t.Fatal("raw mint failed", err)
		}
		if authorityStateQueryMetric(t, "hosted", "mint", "client", "state_valid") != before+2 {
			t.Fatal("raw-wrapper validation multiplicity was not visible")
		}
		frame, err := connect.ToFrame(&protocol.ControlPing{}, connect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		defer connect.MessagePoolReturn(frame.MessageBytes)
		frames := make([]*protocol.Frame, 16)
		for i := range frames {
			frames[i] = frame
		}
		pack, err := proto.Marshal(&protocol.Pack{Frames: frames})
		if err != nil {
			t.Fatal(err)
		}
		args := &connect.ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(pack)}
		const requests = 32
		httpLabels := map[string]string{"ingress": "http", "message": "control_ping", "outcome": "handler_ok"}
		internalLabels := map[string]string{"ingress": "internal", "message": "control_ping", "outcome": "handler_ok"}
		poolLabels := map[string]string{"pool": "default", "outcome": "acquired"}
		httpBefore := authorityObservedCounter(t, "urnetwork_connect_control_frames_total", httpLabels)
		internalBefore := authorityObservedCounter(t, "urnetwork_connect_control_frames_total", internalLabels)
		acquiresBefore := authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels)
		allQueriesBefore := authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil)
		before = authorityStateQueryMetric(t, "hosted", "control", "client", "state_valid")
		start := make(chan struct{})
		failures := make(chan error, requests)
		var workers sync.WaitGroup
		for range requests {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				result, err := owner.ConnectControl(ctx, child.ByClientJwt, args)
				if err == nil && (result == nil || result.Error != nil) {
					err = errors.New("control ping pack was not processed")
				}
				failures <- err
			}()
		}
		close(start)
		workers.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
		if authorityStateQueryMetric(t, "hosted", "control", "client", "state_valid") != before+requests {
			t.Fatal("hosted control queries counted frames or distinct clients instead of requests")
		}
		if authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil) != allQueriesBefore+requests ||
			authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels) != acquiresBefore+requests {
			t.Fatal("hosted control must combine durable ownership and live JWT state in one query per pack")
		}
		if authorityObservedCounter(t, "urnetwork_connect_control_frames_total", httpLabels) != httpBefore+float64(requests*len(frames)) ||
			authorityObservedCounter(t, "urnetwork_connect_control_frames_total", internalLabels) != internalBefore || apiCalls.Load() != 0 {
			t.Fatal("hosted local controller must count existing http ingress frames with zero HTTP requests")
		}
		before = authorityStateQueryMetric(t, "hosted", "retire", "client", "state_valid")
		if _, err := owner.RemoveNetworkClient(ctx, &connect.RemoveNetworkClientArgs{ClientId: connect.Id(*childClaims.ClientId)}); err != nil {
			t.Fatal(err)
		}
		if authorityStateQueryMetric(t, "hosted", "retire", "client", "state_valid") != before+1 {
			t.Fatal("retirement source missing")
		}
		before = authorityStateQueryMetric(t, "hosted", "control", "client", "no_active_row")
		acquiresBefore = authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels)
		_, err = owner.ConnectControl(ctx, child.ByClientJwt, args)
		authorityTestUnauthorized(t, err)
		if authorityStateQueryMetric(t, "hosted", "control", "client", "no_active_row") != before+1 {
			t.Fatal("combined hosted ownership and live-state refusal was not observed")
		}
		if authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels) != acquiresBefore+1 {
			t.Fatal("retired hosted child must stop after the combined state query")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, parent.UserId, server.NowUtc().Add(time.Minute)))
		})
		before = authorityStateQueryMetric(t, "hosted", "control", "client", "credential_rotated")
		_, err = owner.ConnectControl(ctx, owner.token(), args)
		authorityTestUnauthorized(t, err)
		if authorityStateQueryMetric(t, "hosted", "control", "client", "credential_rotated") != before+1 {
			t.Fatal("hosted rotation row was counted as accepted")
		}
		if apiCalls.Load() != 0 {
			t.Fatal("local authority attempted a public API fallback")
		}
	})
}
