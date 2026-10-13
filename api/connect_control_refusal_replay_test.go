package api

// Old provider sdks retry a refused contract route every 5s for hours. The
// owned api router answers a control request whose every frame repeats a
// refusal recorded for the credential's own client before the client state
// query, and authenticates every other request as before. These tests drive
// the real router, credentials and controller against the test database and
// count the statements each request issues.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type controlReplayStatementsKey struct{}

// Records the statements issued under one marked request context, so other
// users of the fixture pools cannot change a count.
type controlReplayStatements struct {
	stateLock  sync.Mutex
	statements []string
}

func (self *controlReplayStatements) record(ctx context.Context, sql string) {
	if marker, _ := ctx.Value(controlReplayStatementsKey{}).(*controlReplayStatements); marker != self {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.statements = append(self.statements, strings.Join(strings.Fields(sql), " "))
}

func (self *controlReplayStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.record(ctx, data.SQL)
	return ctx
}

func (self *controlReplayStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (self *controlReplayStatements) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		self.record(ctx, query.SQL)
	}
	return ctx
}

func (self *controlReplayStatements) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}

func (self *controlReplayStatements) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}

// Returns the statements recorded since the last take.
func (self *controlReplayStatements) take() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	statements := self.statements
	self.statements = nil
	return statements
}

func hasStateQuery(statements []string) bool {
	for _, statement := range statements {
		if strings.Contains(statement, "network_user.credential_change_time") {
			return true
		}
	}
	return false
}

// A provider, its network and credential, and two retired windows it may
// still reply to, all served by one owned api router.
type controlReplayFixture struct {
	handler    http.Handler
	networkId  server.Id
	userId     server.Id
	deviceId   server.Id
	providerId server.Id
	retiredIds []server.Id
	statements *controlReplayStatements
	observed   context.Context
}

// The caller must defer the returned close inside its TestEnv callback.
func newControlReplayFixture(t testing.TB, ctx context.Context) (*controlReplayFixture, func()) {
	t.Helper()
	fixture := &controlReplayFixture{
		networkId:  server.NewId(),
		userId:     server.NewId(),
		deviceId:   server.NewId(),
		providerId: server.NewId(),
		retiredIds: []server.Id{server.NewId(), server.NewId()},
	}
	model.Testing_CreateNetwork(ctx, fixture.networkId, "replay-"+fixture.networkId.String(), fixture.userId)
	model.Testing_CreateDevice(ctx, fixture.networkId, fixture.deviceId, fixture.providerId, "synthetic-provider", "synthetic")
	for _, retiredId := range fixture.retiredIds {
		model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), retiredId, "synthetic-retired-window", "synthetic")
		deactivateControlReplayClient(ctx, retiredId)
	}
	// the router's owners use the traced pools, and close before the scope
	fixture.statements = &controlReplayStatements{}
	scope, err := server.NewTestPgQueryScope(ctx, fixture.statements)
	if err != nil {
		t.Fatal(err)
	}
	owned, closeOwned, err := NewRouter(ctx, ctx)
	if err != nil {
		scope.Close()
		t.Fatal(err)
	}
	fixture.handler = owned
	fixture.observed = context.WithValue(ctx, controlReplayStatementsKey{}, fixture.statements)
	return fixture, func() {
		closeOwned()
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}
}

func deactivateControlReplayClient(ctx context.Context, clientId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE network_client
			SET active = false, deactivate_time = clock_timestamp() AT TIME ZONE 'UTC'
			WHERE client_id = $1
		`, clientId))
	})
}

// A client credential of the fixture's network for clientId.
func (self *controlReplayFixture) token(deviceId server.Id, clientId server.Id, sessionId *server.Id) string {
	byJwt := session.NewByJwt(self.networkId, self.userId, "replay", false, false).Client(deviceId, clientId)
	byJwt.SessionId = sessionId
	return byJwt.Testing_Sign()
}

func createContractFrame(t testing.TB, destinationId server.Id) *protocol.Frame {
	t.Helper()
	frame, err := connect.ToFrame(&protocol.CreateContract{
		DestinationId:     destinationId.Bytes(),
		TransferByteCount: uint64(1024 * 1024),
	}, connect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// Posts one control pack and returns the status, the contract results and the
// statements the request issued.
func (self *controlReplayFixture) post(t testing.TB, token string, frames ...*protocol.Frame) (int, []*protocol.CreateContractResult, []string) {
	t.Helper()
	packBytes, err := proto.Marshal(&protocol.Pack{Frames: frames})
	for _, frame := range frames {
		connect.MessagePoolReturn(frame.MessageBytes)
	}
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(&controller.ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(packBytes)})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/connect/control", bytes.NewReader(body)).WithContext(self.observed)
	request.RemoteAddr = "192.0.2.1:4000"
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	self.handler.ServeHTTP(response, request)
	statements := self.statements.take()
	if response.Code != http.StatusOK {
		return response.Code, nil, statements
	}
	var result controller.ConnectControlResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	resultBytes, err := base64.StdEncoding.DecodeString(result.Pack)
	if err != nil {
		t.Fatal(err)
	}
	var resultPack protocol.Pack
	if err := proto.Unmarshal(resultBytes, &resultPack); err != nil {
		t.Fatal(err)
	}
	var contractResults []*protocol.CreateContractResult
	for _, frame := range resultPack.Frames {
		message, err := connect.FromFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		if contractResult, ok := message.(*protocol.CreateContractResult); ok {
			contractResults = append(contractResults, contractResult)
		}
	}
	return response.Code, contractResults, statements
}

func isReliabilityRefusal(results []*protocol.CreateContractResult, count int) bool {
	if len(results) != count {
		return false
	}
	for _, result := range results {
		if result.Contract != nil || result.Error == nil || *result.Error != protocol.ContractError_Reliability {
			return false
		}
	}
	return true
}

// The value of one counter child, zero when absent.
func controlReplayCounter(t testing.TB, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			matched := 0
			for _, label := range metric.GetLabel() {
				if value, ok := labels[label.GetName()]; ok && value == label.GetValue() {
					matched += 1
				}
			}
			if matched == len(labels) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func controlStateQueries(t testing.TB) float64 {
	return controlReplayCounter(t, "urnetwork_jwt_state_queries_total", map[string]string{
		"caller": "api", "operation": "control", "credential": "client", "outcome": "state_valid",
	})
}

// A provider replying to a retired window is refused once through the state
// query and the lifecycle read. Its retry of that exact route is answered from
// the recorded refusal, with no statement at all: not even the state query.
func TestConnectControlRecordedRefusalIsAnsweredBeforeStateQuery(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fixture, closeFixture := newControlReplayFixture(t, ctx)
		defer closeFixture()
		token := fixture.token(fixture.deviceId, fixture.providerId, nil)
		retiredId := fixture.retiredIds[0]

		statesBefore := controlStateQueries(t)
		code, results, statements := fixture.post(t, token, createContractFrame(t, retiredId))
		if code != http.StatusOK || !isReliabilityRefusal(results, 1) {
			t.Fatalf("the first reply to a retired window was not refused as an unreliable route: status=%d results=%+v", code, results)
		}
		if !hasStateQuery(statements) || controlStateQueries(t) != statesBefore+1 {
			t.Fatalf("the first request did not run its state query: %q", statements)
		}

		replaysBefore := controlReplayCounter(t, "urnetwork_connect_control_refusal_replays_total", map[string]string{"cause": "inactive_destination"})
		statesBefore = controlStateQueries(t)
		code, results, statements = fixture.post(t, token, createContractFrame(t, retiredId))
		if code != http.StatusOK || !isReliabilityRefusal(results, 1) {
			t.Fatalf("the retry was not refused as an unreliable route: status=%d results=%+v", code, results)
		}
		if len(statements) != 0 {
			t.Fatalf("the retry of a recorded refusal used %d statements, want 0: %q", len(statements), statements)
		}
		if controlStateQueries(t) != statesBefore {
			t.Fatal("the retry of a recorded refusal counted a state query")
		}
		replays := controlReplayCounter(t, "urnetwork_connect_control_refusal_replays_total", map[string]string{"cause": "inactive_destination"})
		if replays != replaysBefore+1 {
			t.Fatalf("the answered retry counted %v refusal replays, want 1", replays-replaysBefore)
		}
	})
}

// A refusal answers only its own client and its exact route. Another client
// of the same network, a route with no recorded refusal, a pack that adds such
// a route, and a pack that adds another message all run the state query.
func TestConnectControlStateQueryRunsWithoutRecordedRefusal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fixture, closeFixture := newControlReplayFixture(t, ctx)
		defer closeFixture()
		token := fixture.token(fixture.deviceId, fixture.providerId, nil)
		recordedId, otherId := fixture.retiredIds[0], fixture.retiredIds[1]
		if code, results, _ := fixture.post(t, token, createContractFrame(t, recordedId)); code != http.StatusOK || !isReliabilityRefusal(results, 1) {
			t.Fatalf("the refusal was not recorded: status=%d results=%+v", code, results)
		}

		otherProviderId, otherDeviceId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, fixture.networkId, otherDeviceId, otherProviderId, "synthetic-other-provider", "synthetic")
		otherToken := fixture.token(otherDeviceId, otherProviderId, nil)
		closeFrame, err := connect.ToFrame(&protocol.CloseContract{ContractId: server.NewId().Bytes()}, connect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		for _, request := range []struct {
			name   string
			token  string
			frames []*protocol.Frame
		}{
			{name: "another client on the recorded route", token: otherToken, frames: []*protocol.Frame{createContractFrame(t, recordedId)}},
			{name: "a route without a recorded refusal", token: token, frames: []*protocol.Frame{createContractFrame(t, otherId)}},
			{name: "a pack adding a route without a recorded refusal", token: token, frames: []*protocol.Frame{createContractFrame(t, recordedId), createContractFrame(t, server.NewId())}},
			{name: "a pack adding another message", token: token, frames: []*protocol.Frame{createContractFrame(t, recordedId), closeFrame}},
		} {
			statesBefore := controlStateQueries(t)
			code, _, statements := fixture.post(t, request.token, request.frames...)
			if code != http.StatusOK {
				t.Fatalf("%s: status=%d", request.name, code)
			}
			if !hasStateQuery(statements) || controlStateQueries(t) != statesBefore+1 {
				t.Fatalf("%s did not run the state query: %q", request.name, statements)
			}
		}
	})
}

// Revocation stays prompt. A removed client is refused with 401 on a route
// without a recorded refusal, and from then on also on its recorded routes,
// because the state query's refusal is remembered and checked before any
// answer. A revoked session is refused with 401 even on a recorded route,
// because its revocation marker is checked before any answer too.
func TestConnectControlRevokedClientIsStillRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fixture, closeFixture := newControlReplayFixture(t, ctx)
		defer closeFixture()
		recordedId, otherId := fixture.retiredIds[0], fixture.retiredIds[1]

		token := fixture.token(fixture.deviceId, fixture.providerId, nil)
		if code, results, _ := fixture.post(t, token, createContractFrame(t, recordedId)); code != http.StatusOK || !isReliabilityRefusal(results, 1) {
			t.Fatalf("the refusal was not recorded: status=%d results=%+v", code, results)
		}
		deactivateControlReplayClient(ctx, fixture.providerId)
		if code, results, _ := fixture.post(t, token, createContractFrame(t, otherId)); code != http.StatusUnauthorized {
			t.Fatalf("a removed client on a route without a recorded refusal got status=%d results=%+v, want 401", code, results)
		}
		if code, results, _ := fixture.post(t, token, createContractFrame(t, recordedId)); code != http.StatusUnauthorized {
			t.Fatalf("a removed client refused by the state query got status=%d results=%+v on its recorded route, want 401", code, results)
		}

		sessionProviderId, sessionDeviceId, sessionId := server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, fixture.networkId, sessionDeviceId, sessionProviderId, "synthetic-session-provider", "synthetic")
		sessionToken := fixture.token(sessionDeviceId, sessionProviderId, &sessionId)
		if code, results, _ := fixture.post(t, sessionToken, createContractFrame(t, recordedId)); code != http.StatusOK || !isReliabilityRefusal(results, 1) {
			t.Fatalf("the session's refusal was not recorded: status=%d results=%+v", code, results)
		}
		err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			return r.Set(ctx, session.SessionMarkerKey(fixture.networkId, sessionId), "1", time.Hour).Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		if code, results, _ := fixture.post(t, sessionToken, createContractFrame(t, recordedId)); code != http.StatusUnauthorized {
			t.Fatalf("a revoked session got status=%d results=%+v on its recorded route, want 401", code, results)
		}
	})
}

// The accepted bound: until a request of its own reaches the state query, a
// removed client is told again only the refusals recorded for it. The answer
// creates and grants nothing and reads nothing.
func TestConnectControlRemovedClientIsToldOnlyItsRecordedRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fixture, closeFixture := newControlReplayFixture(t, ctx)
		defer closeFixture()
		recordedId := fixture.retiredIds[0]

		token := fixture.token(fixture.deviceId, fixture.providerId, nil)
		if code, results, _ := fixture.post(t, token, createContractFrame(t, recordedId)); code != http.StatusOK || !isReliabilityRefusal(results, 1) {
			t.Fatalf("the refusal was not recorded: status=%d results=%+v", code, results)
		}
		deactivateControlReplayClient(ctx, fixture.providerId)
		code, results, statements := fixture.post(t, token, createContractFrame(t, recordedId))
		if code != http.StatusOK || !isReliabilityRefusal(results, 1) {
			t.Fatalf("a removed client's recorded route was not refused again: status=%d results=%+v", code, results)
		}
		if len(statements) != 0 {
			t.Fatalf("a removed client's recorded refusal used %d statements, want 0: %q", len(statements), statements)
		}
	})
}
