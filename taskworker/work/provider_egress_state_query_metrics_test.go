// The prober uses its real credential registry, state query and controller.
package work

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"google.golang.org/protobuf/proto"
)

// A bounded-label snapshot; gathering does not acquire a database connection.
func proberStateQueryMetric(t testing.TB, caller, operation, credential, outcome string) float64 {
	t.Helper()
	return proberObservedCounter(t, "urnetwork_jwt_state_queries_total", map[string]string{"caller": caller, "operation": operation, "credential": credential, "outcome": outcome})
}

// Exact labels select one cell; nil labels sum all fixed cells of a family.
func proberObservedCounter(t testing.TB, name string, want map[string]string) float64 {
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

// Retained child membership is distinct from current state: external
// revocation reaches the JWT query, while completed owner retirement does not.
func TestProviderEgressStateQueryMetricActualOperations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(session.WithStateQuerySource(t.Context(), session.StateQueryApiControl), 30*time.Second)
		defer cancel()
		networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "prober-state-query", userId)
		model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "synthetic", "synthetic")
		token := session.NewByJwt(networkId, userId, "prober-state-query", false, false).Client(deviceId, clientId).Testing_Sign()
		credentials, err := newProviderEgressCredentials(&model.ProberIdentity{NetworkId: &networkId, UserId: &userId, ClientId: &clientId, ByClientJwt: token})
		if err != nil {
			t.Fatal(err)
		}
		source := connect.Id(clientId)
		before := proberStateQueryMetric(t, "prober", "mint", "client", "state_valid")
		child, err := credentials.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &source})
		if err != nil || child == nil || child.ByClientJwt == "" {
			t.Fatal("mint failed", err)
		}
		if proberStateQueryMetric(t, "prober", "mint", "client", "state_valid") != before+1 {
			t.Fatal("mint source missing")
		}
		claims, err := session.ParseByJwtForAudience(ctx, child.ByClientJwt, session.ByJwtAudienceApi)
		if err != nil {
			t.Fatal(err)
		}
		notifications := model.NewContractOriginNotifications(ctx, model.DefaultContractOriginNotificationSettings())
		defer notifications.Close()
		owner, err := newProviderEgressControl(credentials, notifications)
		if err != nil {
			t.Fatal(err)
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
		internalLabels := map[string]string{"ingress": "internal", "message": "control_ping", "outcome": "handler_ok"}
		httpLabels := map[string]string{"ingress": "http", "message": "control_ping", "outcome": "handler_ok"}
		poolLabels := map[string]string{"pool": "default", "outcome": "acquired"}
		internalBefore := proberObservedCounter(t, "urnetwork_connect_control_frames_total", internalLabels)
		httpBefore := proberObservedCounter(t, "urnetwork_connect_control_frames_total", httpLabels)
		acquiresBefore := proberObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels)
		allQueriesBefore := proberObservedCounter(t, "urnetwork_jwt_state_queries_total", nil)
		before = proberStateQueryMetric(t, "prober", "control", "client", "state_valid")
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
		if proberStateQueryMetric(t, "prober", "control", "client", "state_valid") != before+requests {
			t.Fatal("prober control queries counted frames or distinct clients instead of requests")
		}
		if proberObservedCounter(t, "urnetwork_jwt_state_queries_total", nil) != allQueriesBefore+requests ||
			proberObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels) != acquiresBefore+requests {
			t.Fatal("direct prober control must preserve exactly one JWT query per pack")
		}
		if proberObservedCounter(t, "urnetwork_connect_control_frames_total", internalLabels) != internalBefore+float64(requests*len(frames)) ||
			proberObservedCounter(t, "urnetwork_connect_control_frames_total", httpLabels) != httpBefore {
			t.Fatal("direct prober control must retain internal frame ingress")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, *claims.ClientId))
		})
		before = proberStateQueryMetric(t, "prober", "control", "client", "no_active_row")
		if _, err := owner.ConnectControl(ctx, child.ByClientJwt, args); err == nil {
			t.Fatal("inactive child accepted")
		}
		if proberStateQueryMetric(t, "prober", "control", "client", "no_active_row") != before+1 {
			t.Fatal("retained membership bypassed current state or lost query attribution")
		}
		before = proberStateQueryMetric(t, "prober", "retire", "client", "state_valid")
		if _, err := credentials.RemoveNetworkClient(ctx, &connect.RemoveNetworkClientArgs{ClientId: connect.Id(*claims.ClientId)}); err != nil {
			t.Fatal(err)
		}
		if proberStateQueryMetric(t, "prober", "retire", "client", "state_valid") != before+1 {
			t.Fatal("retirement source missing")
		}
		before = proberStateQueryMetric(t, "prober", "control", "client", "no_active_row")
		if _, err := owner.ConnectControl(ctx, child.ByClientJwt, args); err == nil {
			t.Fatal("retired child accepted")
		}
		if proberStateQueryMetric(t, "prober", "control", "client", "no_active_row") != before {
			t.Fatal("registry refusal was mislabeled as a JWT query")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, userId, server.NowUtc().Add(time.Minute)))
		})
		before = proberStateQueryMetric(t, "prober", "mint", "client", "credential_rotated")
		if _, err := credentials.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &source}); err == nil {
			t.Fatal("rotated parent minted a child")
		}
		if proberStateQueryMetric(t, "prober", "mint", "client", "credential_rotated") != before+1 {
			t.Fatal("prober rotation row was counted as accepted")
		}
	})
}
