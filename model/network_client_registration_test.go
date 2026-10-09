package model

// These tests use the real allocation transaction, server-issued identities
// and retained dedup rows. No successful allocation/dedup verdict is injected.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// Every fixture owns its generated principal and exact canonical request.
func newNetworkClientRegistrationTest(t testing.TB, ctx context.Context) (*session.ClientSession, RegisterNetworkClientArgs) {
	t.Helper()
	networkId, userId := server.NewId(), server.NewId()
	Testing_CreateNetwork(ctx, networkId, "synthetic-registration", userId)
	clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, Roles: []string{"operator", "validator"}, Principal: "synthetic-production-owner"})
	request := RegisterNetworkClientArgs{Schema: NetworkClientRegistrationSchema, RegistrationId: strings.Repeat("12", 32), ScopeSha256: strings.Repeat("34", 32), Description: "synthetic validator", DeviceSpec: "synthetic headless"}
	return clientSession, request
}

// A typed result must be bound to the exact request, complete client/device
// rows and real renewable server credential before a caller may install it.
func requireNetworkClientRegistrationTestResult(t testing.TB, request RegisterNetworkClientArgs, result *RegisterNetworkClientResult, err error) {
	t.Helper()
	raw, encodeErr := json.Marshal(request)
	digest := sha256.Sum256(raw)
	if encodeErr != nil || err != nil || result == nil || result.Error != nil || result.ClientId == nil || result.DeviceId == nil || result.ByClientJwt == nil || *result.ByClientJwt == "" || result.Schema != request.Schema || result.RegistrationId != request.RegistrationId || result.RequestSha256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("registration did not return its complete original operation: error=%v encode=%v result=%+v", err, encodeErr, result)
	}
}

// The first reply can be discarded. A renewed bearer for the same principal
// resolves that original operation without allocating a second identity.
func TestNetworkClientRegistrationLostReplyPreservesIdentity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		clientSession, request := newNetworkClientRegistrationTest(t, t.Context())
		first, err := RegisterNetworkClient(&request, clientSession)
		requireNetworkClientRegistrationTestResult(t, request, first, err)
		clientSession.ByJwt.CreateTime = clientSession.ByJwt.CreateTime.Add(time.Hour)
		clientSession.ByJwt.Roles = []string{"validator", "operator"}
		second, err := RegisterNetworkClient(&request, clientSession)
		requireNetworkClientRegistrationTestResult(t, request, second, err)
		if *first.ClientId != *second.ClientId || *first.DeviceId != *second.DeviceId {
			t.Fatal("lost registration reply allocated another client/device")
		}
		server.Db(t.Context(), func(conn server.PgConn) {
			var clients, devices, requests int
			server.Raise(conn.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM network_client WHERE network_id=$1), (SELECT count(*) FROM device WHERE network_id=$1), (SELECT count(*) FROM network_client_registration WHERE network_id=$1)`, clientSession.ByJwt.NetworkId).Scan(&clients, &devices, &requests))
			if clients != 1 || devices != 1 || requests != 1 {
				t.Fatalf("replay counts differ: clients=%d devices=%d requests=%d", clients, devices, requests)
			}
		})
	})
}

// Simultaneously admitted duplicate requests contend in PostgreSQL rather
// than relying on a process-local mutex or optimistic response-side cache.
func TestNetworkClientRegistrationConcurrentDuplicates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		clientSession, request := newNetworkClientRegistrationTest(t, t.Context())
		// The isolated test database owns this sequence and trigger. PostgreSQL
		// sequence advances survive transaction rollback, exposing discarded
		// allocation work that the real retry owner correctly hides from callers.
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), fmt.Sprintf(`
CREATE SEQUENCE synthetic_registration_allocation_attempts;
CREATE FUNCTION synthetic_registration_count_allocation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.network_id = '%s'::uuid THEN
  PERFORM nextval('synthetic_registration_allocation_attempts');
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER synthetic_registration_count_allocation BEFORE INSERT ON device
FOR EACH ROW EXECUTE FUNCTION synthetic_registration_count_allocation();`, clientSession.ByJwt.NetworkId.String())))
		})
		connection, err := server.AcquireMaintenanceDbConn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Release()
		lock, err := connection.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		server.RaisePgResult(lock.Exec(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "network-client-registration/v1:"+clientSession.ByJwt.NetworkId.String()))
		var blockerPid int32
		server.Raise(lock.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blockerPid))
		writeCtx, cancelWrites := context.WithCancel(t.Context())
		clientSession.Ctx = writeCtx
		type outcome struct {
			result     *RegisterNetworkClientResult
			err        error
			panicValue any
		}
		results := make(chan outcome, 2)
		var joined sync.WaitGroup
		defer func() {
			cancelWrites()
			cleanup, cancelCleanup := context.WithTimeout(context.Background(), time.Minute)
			defer cancelCleanup()
			_ = lock.Rollback(cleanup)
			joined.Wait()
		}()
		for range 2 {
			joined.Go(func() {
				var value outcome
				value.panicValue = server.HandleError(func() { value.result, value.err = RegisterNetworkClient(&request, clientSession) })
				results <- value
			})
		}
		// Observe both actual PostgreSQL lock waits before release. Under
		// repeatable-read both retain the same pre-allocation view. Its failed
		// insert can recover through server.Tx retry, so identity equality alone
		// cannot prove the post-lock lookup avoided repeating allocation work.
		for {
			var blocked int
			server.Raise(lock.QueryRow(t.Context(), `SELECT count(DISTINCT pid) FROM pg_locks WHERE locktype='advisory' AND NOT granted AND $1::int=ANY(pg_blocking_pids(pid))`, blockerPid).Scan(&blocked))
			if blocked == 2 {
				break
			}
			select {
			case result := <-results:
				t.Fatalf("registration escaped the actual allocation barrier: %v %v", result.err, result.panicValue)
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			default:
				runtime.Gosched()
			}
		}
		server.Raise(lock.Rollback(t.Context()))
		joined.Wait()
		first, second := <-results, <-results
		if first.panicValue != nil || second.panicValue != nil {
			t.Fatalf("concurrent registration did not observe the committed original: %v %v", first.panicValue, second.panicValue)
		}
		requireNetworkClientRegistrationTestResult(t, request, first.result, first.err)
		requireNetworkClientRegistrationTestResult(t, request, second.result, second.err)
		if *first.result.ClientId != *second.result.ClientId || *first.result.DeviceId != *second.result.DeviceId {
			t.Fatal("concurrent duplicate requests allocated two identities")
		}
		server.Db(t.Context(), func(conn server.PgConn) {
			var attempts int64
			server.Raise(conn.QueryRow(t.Context(), `SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM synthetic_registration_allocation_attempts`).Scan(&attempts))
			if attempts != 1 {
				t.Fatalf("concurrent duplicates repeated already committed allocation work: attempts=%d", attempts)
			}
		})
	})
}

// Changes to payload, scope or stable authenticated principal cannot consume
// another caller's operation. The unchanged original remains recoverable.
func TestNetworkClientRegistrationConflictingScopeAndAuthority(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		clientSession, request := newNetworkClientRegistrationTest(t, t.Context())
		first, err := RegisterNetworkClient(&request, clientSession)
		requireNetworkClientRegistrationTestResult(t, request, first, err)
		for _, field := range []string{"description", "scope", "principal", "roles", "user", "new-request-same-scope"} {
			changed, ownSession, claims := request, *clientSession, *clientSession.ByJwt
			claims.Roles = slices.Clone(claims.Roles)
			ownSession.ByJwt = &claims
			switch field {
			case "description":
				changed.Description += " changed"
			case "scope":
				changed.ScopeSha256 = strings.Repeat("56", 32)
			case "principal":
				claims.Principal += " changed"
			case "roles":
				claims.Roles = []string{"operator"}
			case "user":
				claims.UserId = server.NewId()
			case "new-request-same-scope":
				changed.RegistrationId = strings.Repeat("78", 32)
			}
			result, err := RegisterNetworkClient(&changed, &ownSession)
			if err != nil || result == nil || result.Error == nil || result.Error.Code != "registration_conflict" || result.ClientId != nil || result.ByClientJwt != nil {
				t.Fatalf("%s conflict acquired original identity: error=%v result=%+v", field, err, result)
			}
		}
		retained, err := RegisterNetworkClient(&request, clientSession)
		requireNetworkClientRegistrationTestResult(t, request, retained, err)
		if *retained.ClientId != *first.ClientId {
			t.Fatal("a conflict replaced the original binding")
		}
	})
}

// Server revocation and later physical client deletion leave the operation's
// tombstone. A replay never recreates or reactivates the removed client.
func TestNetworkClientRegistrationRevocationAndDeletion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		clientSession, request := newNetworkClientRegistrationTest(t, t.Context())
		first, err := RegisterNetworkClient(&request, clientSession)
		requireNetworkClientRegistrationTestResult(t, request, first, err)
		removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: *first.ClientId}, clientSession)
		if err != nil || removed == nil || removed.Error != nil {
			t.Fatalf("actual client revocation failed: %v %+v", err, removed)
		}
		for _, deleted := range []bool{false, true} {
			if deleted {
				server.Tx(t.Context(), func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(t.Context(), `DELETE FROM network_client_role WHERE client_id=$1`, *first.ClientId))
					server.RaisePgResult(tx.Exec(t.Context(), `DELETE FROM network_client WHERE client_id=$1`, *first.ClientId))
				})
			}
			result, err := RegisterNetworkClient(&request, clientSession)
			if err != nil || result == nil || result.Error == nil || result.Error.Code != "identity_unavailable" || result.ByClientJwt != nil {
				t.Fatalf("revoked/deleted=%t identity was recreated: %v %+v", deleted, err, result)
			}
		}
	})
}

// A failure at the binding insert rolls back both freshly allocated rows.
// Retrying after that storage recovery can commit exactly one whole operation.
func TestNetworkClientRegistrationBindingRollback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		clientSession, request := newNetworkClientRegistrationTest(t, t.Context())
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), `CREATE FUNCTION synthetic_registration_refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic registration bind refusal'; END $$; CREATE TRIGGER synthetic_registration_refuse BEFORE INSERT ON network_client_registration FOR EACH ROW EXECUTE FUNCTION synthetic_registration_refuse()`))
		})
		failed := func() (err error) {
			defer func() {
				if value := recover(); value != nil {
					err = fmt.Errorf("synthetic database refusal: %v", value)
				}
			}()
			_, err = RegisterNetworkClient(&request, clientSession)
			return err
		}()
		if failed == nil {
			t.Fatal("binding insert failure was not observed")
		}
		server.Tx(t.Context(), func(tx server.PgTx) {
			var count int
			server.Raise(tx.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM network_client WHERE network_id=$1)+(SELECT count(*) FROM device WHERE network_id=$1)+(SELECT count(*) FROM network_client_registration WHERE network_id=$1)`, clientSession.ByJwt.NetworkId).Scan(&count))
			if count != 0 {
				t.Fatalf("failed binding leaked %d allocated rows", count)
			}
			server.RaisePgResult(tx.Exec(t.Context(), `DROP TRIGGER synthetic_registration_refuse ON network_client_registration; DROP FUNCTION synthetic_registration_refuse()`))
		})
		result, err := RegisterNetworkClient(&request, clientSession)
		requireNetworkClientRegistrationTestResult(t, request, result, err)
	})
}

// Exact versioned tags and string values are admitted before mutation. Case
// folding, escaped duplicates and null cannot change an ownership field.
func TestNetworkClientRegistrationRejectsUnknownWireIdentity(t *testing.T) {
	original := RegisterNetworkClientArgs{Schema: NetworkClientRegistrationSchema, RegistrationId: strings.Repeat("12", 32), ScopeSha256: strings.Repeat("34", 32), Description: "synthetic validator", DeviceSpec: "synthetic headless"}
	canonical, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{string(canonical), strings.Replace(string(canonical), `"schema":`, `"\u0073chema":`, 1)} {
		var request RegisterNetworkClientArgs
		if err := json.Unmarshal([]byte(raw), &request); err != nil || request != original {
			t.Fatalf("canonical versioned registration changed: %v", err)
		}
	}
	reject := func(raw string) {
		request := original
		if err := json.Unmarshal([]byte(raw), &request); err == nil {
			t.Fatalf("versioned registration accepted noncanonical request grammar: %s", raw)
		}
		if request != original {
			t.Fatal("rejected registration mutated the original request")
		}
	}
	for _, name := range []string{"schema", "registration_id", "scope_sha256", "description", "device_spec"} {
		for _, alias := range []string{strings.ToUpper(name[:1]) + name[1:], strings.ToUpper(name), strings.Replace(name, "s", "\u017f", 1)} {
			reject(strings.Replace(string(canonical), `"`+name+`":`, `"`+alias+`":`, 1))
			reject(`{"` + name + `":"synthetic","` + alias + `":"replacement"}`)
			reject(`{"` + alias + `":"replacement","` + name + `":"synthetic"}`)
			reject(`{"` + name + `":"synthetic","` + alias + `":null}`)
		}
		for _, value := range []string{`null`, `true`, `17`, `[]`, `{}`} {
			reject(`{"` + name + `":` + value + `}`)
		}
		reject(`{"` + name + `":"first","` + name + `":"second"}`)
	}
	for _, raw := range []string{
		`{"schema":"urnetwork-client-registration-v1","client_id":"synthetic"}`,
		`{"proxy_config":{}}`, `{"extension":null}`, `{} {}`, `null`, `[]`, `17`, `"synthetic"`,
		`{"schema":"first","\u0073chema":"second"}`, `{"schema":"synthetic",`,
		`{"schema":"synthetic","description":{"schema":"nested"}}`,
	} {
		reject(raw)
	}
}
