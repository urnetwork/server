package api_test

// The configured SDK crosses the actual production route, JWT/session owner,
// controller and PostgreSQL transaction. Faults alter only the physical reply
// after that transaction returns; no authentication or allocation is injected.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	serverapi "github.com/urnetwork/server/v2026/api"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
)

type registrationSdkRequest struct {
	body          []byte
	authorization string
}

type registrationSdkCommit struct {
	clientId server.Id
	deviceId server.Id
	clients  int
	devices  int
	bindings int
	err      error
}

type registrationSdkRouteFixture struct {
	ctx        context.Context
	cancel     context.CancelFunc
	endpoint   *httptest.Server
	router     *router.Router
	claims     *jwt.ByJwt
	breakReply bool
	committed  chan registrationSdkCommit
	stateLock  sync.Mutex
	requests   []registrationSdkRequest
	legacy     uint64
}

func newRegistrationSdkRouteFixture(t testing.TB, breakReply bool) *registrationSdkRouteFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	networkId, userId := server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "synthetic-sdk-registration", userId)
	claims := jwt.NewByJwt(networkId, userId, "synthetic-sdk-registration", false, false)
	claims.Roles, claims.Principal = []string{"operator", "validator"}, "synthetic-registration-owner"
	self := &registrationSdkRouteFixture{ctx: ctx, cancel: cancel, claims: claims, breakReply: breakReply, committed: make(chan registrationSdkCommit, 1)}
	self.router = router.NewRouter(ctx, serverapi.Routes())
	self.endpoint = httptest.NewServer(http.HandlerFunc(self.serve))
	return self
}

func (self *registrationSdkRouteFixture) close() {
	self.cancel()
	self.endpoint.Close()
}

// A fresh SDK/strategy uses the same production HTTP path after a caller
// restart. Explicit close joins its credential worker before server teardown.
func (self *registrationSdkRouteFixture) client(t testing.TB, claims *jwt.ByJwt) (*sdk.Api, func()) {
	t.Helper()
	settings := connect.DefaultClientStrategySettings()
	settings.EnableResilient = false
	settings.RequestTimeout = 5 * time.Minute
	settings.ConnectTimeout = time.Minute
	settings.ReconnectTimeout = time.Millisecond // Pacing only; actual completed requests prove replay.
	strategy := connect.NewClientStrategy(self.ctx, settings)
	client := sdk.NewApi(self.ctx, strategy, self.endpoint.URL)
	client.SetByJwt(claims.Sign())
	var once sync.Once
	return client, func() {
		once.Do(func() {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			strategy.Close()
		})
	}
}

func (self *registrationSdkRouteFixture) serve(w http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/network/register-client-v1" {
		if request.URL.Path == "/network/auth-client" {
			func() { self.stateLock.Lock(); defer self.stateLock.Unlock(); self.legacy++ }()
		}
		self.router.ServeHTTP(w, request)
		return
	}
	// Consume and close the real input before replacing it with the same
	// bounded bytes for production admission. No blocked unread POST remains.
	raw, readErr := io.ReadAll(io.LimitReader(request.Body, 16*1024+1))
	closeErr := request.Body.Close()
	if readErr != nil || closeErr != nil || len(raw) > 16*1024 {
		http.Error(w, "synthetic fixture input failed", http.StatusBadRequest)
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(raw))
	first := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, registrationSdkRequest{body: bytes.Clone(raw), authorization: request.Header.Get("Authorization")})
		return len(self.requests) == 1
	}()
	if !self.breakReply || !first {
		self.router.ServeHTTP(w, request)
		return
	}
	response := httptest.NewRecorder()
	self.router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		for key, values := range response.Header() {
			w.Header()[key] = slices.Clone(values)
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
		return
	}
	var issued model.RegisterNetworkClientResult
	commit := registrationSdkCommit{}
	if err := json.Unmarshal(response.Body.Bytes(), &issued); err != nil || issued.Error != nil || issued.ClientId == nil || issued.DeviceId == nil {
		commit.err = errors.New("production route did not return a committed client identity")
	} else {
		commit.clientId, commit.deviceId = *issued.ClientId, *issued.DeviceId
		server.Db(request.Context(), func(conn server.PgConn) {
			var clientId, deviceId server.Id
			commit.err = conn.QueryRow(request.Context(), `SELECT client_id, device_id FROM network_client_registration WHERE network_id=$1 AND registration_id=$2`, self.claims.NetworkId, issued.RegistrationId).Scan(&clientId, &deviceId)
			if commit.err == nil && (clientId != commit.clientId || deviceId != commit.deviceId) {
				commit.err = errors.New("production reply differs from the committed binding")
			}
			if commit.err == nil {
				commit.err = conn.QueryRow(request.Context(), `SELECT (SELECT count(*) FROM network_client WHERE network_id=$1), (SELECT count(*) FROM device WHERE network_id=$1), (SELECT count(*) FROM network_client_registration WHERE network_id=$1)`, self.claims.NetworkId).Scan(&commit.clients, &commit.devices, &commit.bindings)
			}
		})
	}
	self.committed <- commit
	// The real transaction has completed. Break HTTP framing after one byte;
	// the configured transport must replay the original opaque operation.
	connection, buffer, err := w.(http.Hijacker).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_, _ = fmt.Fprint(buffer, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 128\r\nConnection: close\r\n\r\n{")
	_ = buffer.Flush()
	_ = connection.Close()
}

func registrationSdkArgs() *sdk.RegisterNetworkClientArgs {
	return &sdk.RegisterNetworkClientArgs{Schema: sdk.NetworkClientRegistrationSchema, RegistrationId: strings.Repeat("12", 32), ScopeSha256: strings.Repeat("34", 32), DeviceDescription: "synthetic SDK validator", DeviceSpec: "synthetic headless"}
}

func requireRegistrationSdkIdentity(t testing.TB, fixture *registrationSdkRouteFixture, result *sdk.RegisterNetworkClientResult, err error) *jwt.ByJwt {
	t.Helper()
	if err != nil || result == nil || result.Error != nil || result.ClientId == nil || result.DeviceId == nil || result.ByClientJwt == "" {
		t.Fatalf("real versioned route did not recover complete SDK identity: error=%v", err)
	}
	claims, err := jwt.ParseByJwt(fixture.ctx, result.ByClientJwt)
	if err != nil {
		t.Fatalf("actual server credential failed signature verification: %v", err)
	}
	if err := jwt.ValidateByJwtState(fixture.ctx, claims, false); err != nil {
		t.Fatalf("actual server credential is not active: %v", err)
	}
	if claims.ClientId == nil || claims.DeviceId == nil || claims.ClientId.String() != result.ClientId.String() || claims.DeviceId.String() != result.DeviceId.String() || claims.NetworkId != fixture.claims.NetworkId || claims.UserId != fixture.claims.UserId || claims.Principal != fixture.claims.Principal || !slices.Equal(claims.Roles, fixture.claims.Roles) {
		t.Fatal("SDK identity differs from authenticated server principal/device")
	}
	return claims
}

func requireRegistrationSdkCounts(t testing.TB, fixture *registrationSdkRouteFixture, wantActive int) {
	t.Helper()
	server.Db(fixture.ctx, func(conn server.PgConn) {
		var clients, devices, bindings, active int
		server.Raise(conn.QueryRow(fixture.ctx, `SELECT (SELECT count(*) FROM network_client WHERE network_id=$1), (SELECT count(*) FROM device WHERE network_id=$1), (SELECT count(*) FROM network_client_registration WHERE network_id=$1), (SELECT count(*) FROM network_client WHERE network_id=$1 AND active)`, fixture.claims.NetworkId).Scan(&clients, &devices, &bindings, &active))
		if clients != 1 || devices != 1 || bindings != 1 || active != wantActive {
			t.Fatalf("actual route replaced its original allocation: clients=%d devices=%d bindings=%d active=%d", clients, devices, bindings, active)
		}
	})
}

func TestSdkVersionedRegistrationRecoversLostCommittedReply(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newRegistrationSdkRouteFixture(t, true)
		defer fixture.close()
		client, closeClient := fixture.client(t, fixture.claims)
		defer closeClient()
		args := registrationSdkArgs()
		original, err := sdk.EncodeNetworkClientRegistration(args)
		if err != nil {
			t.Fatal(err)
		}
		bearer := client.GetByJwt()
		result, err := client.RegisterNetworkClientSyncWithContext(fixture.ctx, args)
		claims := requireRegistrationSdkIdentity(t, fixture, result, err)
		select {
		case committed := <-fixture.committed:
			if committed.err != nil || committed.clients != 1 || committed.devices != 1 || committed.bindings != 1 || committed.clientId != *claims.ClientId || committed.deviceId != *claims.DeviceId {
				t.Fatalf("lost committed reply acquired a different allocation: %v", committed.err)
			}
		default:
			t.Fatal("reply-loss fixture never reached actual committed registration")
		}
		requests, legacy := func() ([]registrationSdkRequest, uint64) {
			fixture.stateLock.Lock()
			defer fixture.stateLock.Unlock()
			return slices.Clone(fixture.requests), fixture.legacy
		}()
		if len(requests) < 2 || legacy != 0 {
			t.Fatal("committed reply loss did not replay the original versioned route")
		}
		for _, request := range requests {
			if !bytes.Equal(request.body, original) || request.authorization != "Bearer "+bearer {
				t.Fatal("physical replay changed the original request or authority")
			}
		}
		closeClient()
		refreshed := jwt.NewByJwt(fixture.claims.NetworkId, fixture.claims.UserId, fixture.claims.NetworkName, false, false)
		refreshed.Roles, refreshed.Principal = slices.Clone(fixture.claims.Roles), fixture.claims.Principal
		restarted, closeRestarted := fixture.client(t, refreshed)
		defer closeRestarted()
		if restarted.GetByJwt() == bearer {
			t.Fatal("restart fixture did not renew its network credential")
		}
		recovered, err := restarted.RegisterNetworkClientSyncWithContext(fixture.ctx, args)
		recoveredClaims := requireRegistrationSdkIdentity(t, fixture, recovered, err)
		if *recoveredClaims.ClientId != *claims.ClientId || *recoveredClaims.DeviceId != *claims.DeviceId {
			t.Fatal("new SDK owner or renewed bearer replaced the committed identity")
		}
		requireRegistrationSdkCounts(t, fixture, 1)
	})
}

func TestSdkVersionedRegistrationKeepsConflictsAndRevocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newRegistrationSdkRouteFixture(t, false)
		defer fixture.close()
		client, closeClient := fixture.client(t, fixture.claims)
		defer closeClient()
		args := registrationSdkArgs()
		result, err := client.RegisterNetworkClientSyncWithContext(fixture.ctx, args)
		issued := requireRegistrationSdkIdentity(t, fixture, result, err)
		originalToken := client.GetByJwt()
		for _, field := range []string{"description", "scope", "request", "principal"} {
			changed := *args
			client.SetByJwt(originalToken)
			switch field {
			case "description":
				changed.DeviceDescription += " changed"
			case "scope":
				changed.ScopeSha256 = strings.Repeat("56", 32)
			case "request":
				changed.RegistrationId = strings.Repeat("78", 32)
			case "principal":
				foreign := *fixture.claims
				foreign.Principal += " changed"
				client.SetByJwt(foreign.Sign())
			}
			refused, err := client.RegisterNetworkClientSyncWithContext(fixture.ctx, &changed)
			if err != nil || refused == nil || refused.Error == nil || refused.Error.Code != "registration_conflict" || refused.ClientId != nil || refused.DeviceId != nil || refused.ByClientJwt != "" {
				t.Fatalf("actual %s conflict crossed SDK/server identity ownership: %v", field, err)
			}
			requireRegistrationSdkCounts(t, fixture, 1)
		}
		client.SetByJwt(originalToken)
		retained, err := client.RegisterNetworkClientSyncWithContext(fixture.ctx, args)
		retainedClaims := requireRegistrationSdkIdentity(t, fixture, retained, err)
		if *retainedClaims.ClientId != *issued.ClientId || *retainedClaims.DeviceId != *issued.DeviceId {
			t.Fatal("complete conflicts displaced the original registration")
		}
		removed, err := client.RemoveNetworkClientSyncWithContextAndJwt(fixture.ctx, &sdk.RemoveNetworkClientArgs{ClientId: result.ClientId}, originalToken)
		if err != nil || removed == nil || removed.Error != nil {
			t.Fatalf("actual SDK remove-client route did not revoke original identity: %v", err)
		}
		refreshed := jwt.NewByJwt(fixture.claims.NetworkId, fixture.claims.UserId, fixture.claims.NetworkName, false, false)
		refreshed.Roles, refreshed.Principal = slices.Clone(fixture.claims.Roles), fixture.claims.Principal
		client.SetByJwt(refreshed.Sign())
		refused, err := client.RegisterNetworkClientSyncWithContext(fixture.ctx, args)
		if err != nil || refused == nil || refused.Error == nil || refused.Error.Code != "identity_unavailable" || refused.ClientId != nil || refused.DeviceId != nil || refused.ByClientJwt != "" {
			t.Fatalf("renewed network credential revived revoked registration: %v", err)
		}
		if err := jwt.ValidateByJwtState(fixture.ctx, issued, false); err == nil {
			t.Fatal("revoked server-issued client credential stayed active")
		}
		requireRegistrationSdkCounts(t, fixture, 0)
		fixture.stateLock.Lock()
		legacy := fixture.legacy
		fixture.stateLock.Unlock()
		if legacy != 0 {
			t.Fatal("conflict or revocation fell back to legacy allocation")
		}
	})
}
