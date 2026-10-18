// API-key retries cross the production router with fresh request identities.
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	serverapi "github.com/urnetwork/server/api"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Each fixture owns one generated network/key and one immutable request body.
type apiKeyRegistrationSessionFixture struct {
	ctx      context.Context
	router   *router.Router
	root     *session.ClientSession
	apiKey   string
	apiKeyId server.Id
	request  []byte
}

// Authentication and registration use their real constructors and SQL owners.
func newApiKeyRegistrationSessionFixture(t testing.TB) *apiKeyRegistrationSessionFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	networkId, userId := server.NewId(), server.NewId()
	networkName := "synthetic-registration-" + networkId.String()
	model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
	root := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, networkName, false, false))
	t.Cleanup(root.Cancel)
	created, err := model.CreateApiKey(&model.CreateApiKeyArgs{Name: "synthetic registration key"}, root)
	if err != nil || created == nil || created.Error != nil {
		t.Fatal("could not create fixture API key", err)
	}
	request, err := json.Marshal(&model.RegisterNetworkClientArgs{
		Schema: model.NetworkClientRegistrationSchema, RegistrationId: strings.Repeat("12", 32), ScopeSha256: strings.Repeat("34", 32), Description: "synthetic registration", DeviceSpec: "synthetic device",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &apiKeyRegistrationSessionFixture{ctx: ctx, router: router.NewRouter(ctx, serverapi.Routes()), root: root, apiKey: created.ApiKey, apiKeyId: created.Id, request: request}
}

// Every invocation creates and authenticates a new physical request object.
func (self *apiKeyRegistrationSessionFixture) register(t testing.TB) (*model.RegisterNetworkClientResult, int) {
	t.Helper()
	request := httptest.NewRequestWithContext(self.ctx, http.MethodPost, "https://api.example.test/network/register-client-v1", bytes.NewReader(self.request))
	request.RemoteAddr = "192.0.2.81:41760"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+self.apiKey)
	response := httptest.NewRecorder()
	self.router.ServeHTTP(response, request)
	var result model.RegisterNetworkClientResult
	if response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal("invalid production registration response", err)
		}
	}
	return &result, response.Code
}

// Both independent requests must succeed before inspecting retained metadata,
// so the pre-fix control fails at the real lost-response replay boundary.
func (self *apiKeyRegistrationSessionFixture) requireRegistered(t testing.TB) *session.ByJwt {
	t.Helper()
	result, status := self.register(t)
	if status != http.StatusOK || result.Error != nil || result.ByClientJwt == nil || result.ClientId == nil || result.DeviceId == nil {
		t.Fatalf("API-key registration refused: status=%d error=%+v", status, result.Error)
	}
	claims, err := session.ParseByJwt(self.ctx, *result.ByClientJwt)
	if err != nil || claims == nil || claims.SessionId == nil || claims.ClientId == nil || claims.DeviceId == nil || claims.NetworkId != self.root.ByJwt.NetworkId || *claims.ClientId != *result.ClientId || *claims.DeviceId != *result.DeviceId {
		t.Fatal("registration returned incomplete or foreign session authority", err)
	}
	if err := session.ValidateByJwtState(self.ctx, claims, true); err != nil {
		t.Fatal("registration credential is not live", err)
	}
	return claims
}

// The successful response, mutable association, and immutable operation tuple
// must identify the same original session with its exact first horizon.
func (self *apiKeyRegistrationSessionFixture) requireOriginalTuple(t testing.TB, claims *session.ByJwt) {
	t.Helper()
	server.Db(self.ctx, func(conn server.PgConn) {
		var registeredId, associatedId server.Id
		var registeredTime, associatedTime, acceptUntil time.Time
		server.Raise(conn.QueryRow(self.ctx, `SELECT r.session_id,r.session_create_time,r.session_accept_until,c.session_id,c.session_create_time FROM network_client_registration r JOIN network_client c ON c.client_id=r.client_id WHERE r.network_id=$1`, claims.NetworkId).Scan(&registeredId, &registeredTime, &acceptUntil, &associatedId, &associatedTime))
		if registeredId != *claims.SessionId || associatedId != registeredId || !registeredTime.Equal(claims.CreateTime) || !associatedTime.Equal(registeredTime) || !acceptUntil.Equal(claims.AcceptUntil()) {
			t.Fatal("registration did not retain its original session tuple and horizon")
		}
	})
}

// A refusal must produce no usable credential and preserve the allocation count.
func (self *apiKeyRegistrationSessionFixture) requireRefused(t testing.TB) {
	t.Helper()
	result, status := self.register(t)
	if status == http.StatusOK && result.Error == nil || result.ByClientJwt != nil {
		t.Fatal("registration replay revived unavailable original authority")
	}
	self.requireCounts(t, 1)
}

// No replay or rolled-back attempt may allocate another durable identity.
func (self *apiKeyRegistrationSessionFixture) requireCounts(t testing.TB, want int) {
	t.Helper()
	server.Db(self.ctx, func(conn server.PgConn) {
		var clients, devices, registrations int
		server.Raise(conn.QueryRow(self.ctx, `SELECT (SELECT count(*) FROM network_client WHERE network_id=$1),(SELECT count(*) FROM device WHERE network_id=$1),(SELECT count(*) FROM network_client_registration WHERE network_id=$1)`, self.root.ByJwt.NetworkId).Scan(&clients, &devices, &registrations))
		if clients != want || devices != want || registrations != want {
			t.Fatalf("registration allocation count: clients=%d devices=%d registrations=%d want=%d", clients, devices, registrations, want)
		}
	})
}

// The first committed reply is discarded by the caller; the next request must
// recover the original session despite API authentication minting fresh claims.
func TestApiKeyRegistrationSessionRecoversLostResponse(t *testing.T) {
	defer session.Testing_SetSessionCreationEnabled(true)()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture := newApiKeyRegistrationSessionFixture(t)
		original := fixture.requireRegistered(t)
		replayed := fixture.requireRegistered(t)
		if *replayed.ClientId != *original.ClientId || *replayed.DeviceId != *original.DeviceId || *replayed.SessionId != *original.SessionId || !replayed.CreateTime.Equal(original.CreateTime) {
			t.Fatal("lost response recovery replaced the original identity or lineage")
		}
		fixture.requireOriginalTuple(t, original)
		fixture.requireOriginalTuple(t, replayed)
		fixture.requireCounts(t, 1)
	})
}

// A retained earlier horizon models an older committed reply without relying
// on sleeps. Fresh request authentication cannot renew that operation's expiry.
func TestApiKeyRegistrationSessionReplayDoesNotRenewOriginalHorizon(t *testing.T) {
	defer session.Testing_SetSessionCreationEnabled(true)()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture := newApiKeyRegistrationSessionFixture(t)
		original := fixture.requireRegistered(t)
		horizon := original.AcceptUntil().Add(-time.Hour)
		server.Tx(fixture.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(fixture.ctx, `UPDATE network_client_registration SET session_accept_until=$2 WHERE network_id=$1`, original.NetworkId, horizon))
		})
		replayed := fixture.requireRegistered(t)
		if !replayed.AcceptUntil().Equal(horizon) || !replayed.CreateTime.Equal(original.CreateTime) || *replayed.SessionId != *original.SessionId {
			t.Fatal("replay renewed the retained horizon or replaced original session authority")
		}
		fixture.requireOriginalTuple(t, replayed)
		fixture.requireCounts(t, 1)
	})
}

// An explicit network reauthentication changes the mutable client association,
// but the original operation can never adopt that new independent session.
func TestApiKeyRegistrationSessionRejectsReboundClient(t *testing.T) {
	defer session.Testing_SetSessionCreationEnabled(true)()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture := newApiKeyRegistrationSessionFixture(t)
		original := fixture.requireRegistered(t)
		rebound, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{ClientId: original.ClientId, Description: "synthetic rebind", DeviceSpec: "synthetic device"}, fixture.root)
		if err != nil || rebound == nil || rebound.Error != nil || rebound.ByClientJwt == nil {
			t.Fatal("explicit rebind failed", err)
		}
		claims, err := session.ParseByJwt(fixture.ctx, *rebound.ByClientJwt)
		if err != nil || claims.SessionId == nil || *claims.SessionId == *original.SessionId {
			t.Fatal("fixture did not create an independent rebind", err)
		}
		fixture.requireRefused(t)
	})
}

// A retained SQL revocation remains authoritative after its finite Redis marker
// is gone, so an API key cannot revive a superseded registration credential.
func TestApiKeyRegistrationSessionRejectsRevokedOriginalAfterMarkerLoss(t *testing.T) {
	defer session.Testing_SetSessionCreationEnabled(true)()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture := newApiKeyRegistrationSessionFixture(t)
		original := fixture.requireRegistered(t)
		result, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *original.SessionId, OperationId: server.NewId()}, fixture.root)
		if err != nil || result == nil || result.RevokedCount != 1 {
			t.Fatal("fixture did not revoke the original session", err)
		}
		server.Raise(server.RedisAuth(fixture.ctx, func(ctx context.Context, r server.RedisClient) error {
			return r.Del(ctx, session.SessionMarkerKey(original.NetworkId, *original.SessionId)).Err()
		}))
		fixture.requireRefused(t)
	})
}

// Historical/malformed tuples and a passed original horizon are independent
// refusal boundaries even while the mutable client remains active and bound.
func TestApiKeyRegistrationSessionRejectsUnprovenOrExpiredOriginal(t *testing.T) {
	defer session.Testing_SetSessionCreationEnabled(true)()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, mutation := range []string{
			`UPDATE network_client_registration SET session_id=NULL,session_create_time=NULL,session_accept_until=NULL WHERE network_id=$1`,
			`UPDATE network_client_registration SET session_accept_until=timestamp '2000-01-01' WHERE network_id=$1`,
			`UPDATE network_client SET session_create_time=session_create_time+interval '1 second' WHERE network_id=$1`,
		} {
			fixture := newApiKeyRegistrationSessionFixture(t)
			fixture.requireRegistered(t)
			server.Tx(fixture.ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(fixture.ctx, mutation, fixture.root.ByJwt.NetworkId))
			})
			fixture.requireRefused(t)
		}
	})
}

// A late index-enqueue failure rolls back the already inserted registration
// tuple and associated client together, before a retry commits one operation.
func TestApiKeyRegistrationSessionRollbackRetainsNoPartialBinding(t *testing.T) {
	defer session.Testing_SetSessionCreationEnabled(true)()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture := newApiKeyRegistrationSessionFixture(t)
		server.Tx(fixture.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(fixture.ctx, `CREATE FUNCTION synthetic_registration_index_refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic registration index failure'; END $$; CREATE TRIGGER synthetic_registration_index_refuse BEFORE INSERT ON network_session_index_outbox FOR EACH ROW EXECUTE FUNCTION synthetic_registration_index_refuse()`))
		})
		result, status := fixture.register(t)
		if status == http.StatusOK && result.Error == nil || result.ByClientJwt != nil {
			t.Fatal("late transaction failure returned a registration credential")
		}
		fixture.requireCounts(t, 0)
		server.Tx(fixture.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(fixture.ctx, `DROP TRIGGER synthetic_registration_index_refuse ON network_session_index_outbox; DROP FUNCTION synthetic_registration_index_refuse()`))
		})
		original := fixture.requireRegistered(t)
		replayed := fixture.requireRegistered(t)
		if *original.SessionId != *replayed.SessionId {
			t.Fatal("recovered transaction did not retain its winning session")
		}
		fixture.requireCounts(t, 1)
	})
}

// API-key authentication still limits the operation to the key's network.
func TestApiKeyRegistrationSessionPreservesKeyNetworkScope(t *testing.T) {
	defer session.Testing_SetSessionCreationEnabled(true)()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		first, second := newApiKeyRegistrationSessionFixture(t), newApiKeyRegistrationSessionFixture(t)
		a, b := first.requireRegistered(t), second.requireRegistered(t)
		if a.NetworkId == b.NetworkId || *a.ClientId == *b.ClientId || *a.SessionId == *b.SessionId {
			t.Fatal("same registration key borrowed another API key's network identity")
		}
		server.Raise(model.DeleteApiKey(&first.apiKeyId, first.root))
		first.requireRefused(t)
		second.requireRegistered(t)
	})
}
