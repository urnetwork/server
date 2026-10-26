// The Redis revocation record answers tagged credentials (owner decision,
// 2026-10-26): a projected answer costs one read and no state query, anything
// the record cannot answer goes to SQL, and no revocation, store outage or
// lost record turns back into acceptance.
package session

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Adds an active client under parent (top level when nil) and returns its
// tagged credential, whose root claim names its actual root.
func (self *testingStateRefusalNetwork) taggedClient(ctx context.Context, parent *ByJwt) *ByJwt {
	clientId := server.NewId()
	issuer := NewByJwt(self.networkId, self.userId, "revocation-record", false, false)
	var parentClientId *server.Id
	if parent != nil {
		issuer = parent
		parentClientId = parent.ClientId
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,device_id,description,active,source_client_id) VALUES($1,$2,$3,'synthetic',true,$4)`, clientId, self.networkId, self.deviceId, parentClientId))
	})
	return issuer.Client(self.deviceId, clientId)
}

// The network's sign-in credential, tagged with a session of its own.
func (self *testingStateRefusalNetwork) sessionCredential() *ByJwt {
	credential := NewByJwt(self.networkId, self.userId, "revocation-record", false, false)
	sessionId := server.NewId()
	credential.SessionId = &sessionId
	return credential
}

// Deactivates clients as production does: their records refuse them before
// the deactivation commits.
func (self *testingStateRefusalNetwork) deactivate(ctx context.Context, clients ...*ByJwt) {
	clientIds := []server.Id{}
	for _, client := range clients {
		clientIds = append(clientIds, *client.ClientId)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SELECT client_id FROM network_client WHERE client_id = ANY($1) FOR UPDATE`, clientIds))
		server.Raise(RevokeClientRecordsInTx(ctx, tx, self.networkId, clientIds))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id = ANY($1)`, clientIds))
	})
}

// Every state query attempt for a source and credential shape, any outcome.
func testingStateQueries(source StateQuerySource, client bool) float64 {
	total := 0.0
	for outcome := range stateQueryOutcomeCount {
		total += stateQueryCount(source, client, outcome)
	}
	return total
}

func testingRevocationField(ctx context.Context, t testing.TB, key string, field string) (string, bool) {
	var value string
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		value, err = r.HGet(ctx, key, field).Result()
		return err
	})
	if errors.Is(err, server.RedisNil) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return value, true
}

// Simulates Redis losing every record of a network.
func testingDropRevocationRecords(ctx context.Context, t testing.TB, networkId server.Id) {
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		for shard := range revocationRecordShardCount {
			if err := r.Del(ctx, revocationRecordShardKey(networkId, shard)).Err(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Every credential shape the record answers costs one state query, for the
// first check; the record answers each later check with no query.
func TestRevocationRecordAnswersProjectedCredentialsWithoutStateQuery(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := WithStateQuerySource(t.Context(), StateQueryConnectH1)
		network := newTestingStateRefusalNetwork(ctx)
		top := network.taggedClient(ctx, nil)
		child := network.taggedClient(ctx, top)
		parented := network.taggedClient(ctx, top)
		sessionChild := network.taggedClient(ctx, top)
		sessionId := server.NewId()
		sessionChild.SessionId = &sessionId
		account := network.sessionCredential()
		for _, check := range []struct {
			name   string
			client bool
			run    func() error
		}{
			{"top-level client", true, func() error { return ValidateByJwtState(ctx, top, true) }},
			{"child on its root branch", true, func() error { return ValidateByJwtState(ctx, child, true) }},
			{"child of its parent", true, func() error { return ValidateByJwtStateForParent(ctx, parented, *top.ClientId) }},
			{"child on its session branch", true, func() error { return ValidateByJwtState(ctx, sessionChild, true) }},
			{"network session", false, func() error { return ValidateByJwtState(ctx, account, false) }},
		} {
			before := testingStateQueries(StateQueryConnectH1, check.client)
			for attempt := range 3 {
				if err := check.run(); err != nil {
					t.Fatalf("%s: check %d refused a valid credential: %v", check.name, attempt, err)
				}
			}
			if queries := testingStateQueries(StateQueryConnectH1, check.client) - before; queries != 1 {
				t.Errorf("%s: three checks ran %.0f state queries, want only the first", check.name, queries)
			}
		}
		// the parent the record stored is the one a parent check must name
		before := testingStateQueries(StateQueryConnectH1, true)
		if err := ValidateByJwtStateForParent(ctx, parented, *child.ClientId); !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("a parent check naming another client was not refused: %v", err)
		}
		if queries := testingStateQueries(StateQueryConnectH1, true) - before; queries != 0 {
			t.Errorf("the record's parent refusal ran %.0f state queries", queries)
		}
	})
}

// A deactivated client is refused from its record before any query, and a
// deactivated root refuses its root-branch descendants. A session-branch
// credential never depends on its root (REVOKE-FINAL.md §1).
func TestRevocationRecordRefusesDeactivatedClientsWithoutStateQuery(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := WithStateQuerySource(t.Context(), StateQueryConnectH1)
		network := newTestingStateRefusalNetwork(ctx)
		top := network.taggedClient(ctx, nil)
		child := network.taggedClient(ctx, top)
		sibling := network.taggedClient(ctx, top)
		sessionChild := network.taggedClient(ctx, top)
		sessionId := server.NewId()
		sessionChild.SessionId = &sessionId
		for _, credential := range []*ByJwt{top, child, sibling, sessionChild} {
			if err := ValidateByJwtState(ctx, credential, true); err != nil {
				t.Fatal("projection check refused a valid credential", err)
			}
		}
		expect := func(name string, credential *ByJwt, refused bool) {
			t.Helper()
			before := testingStateQueries(StateQueryConnectH1, true)
			err := ValidateByJwtState(ctx, credential, true)
			if refused && !errors.Is(err, ErrByJwtInactive) {
				t.Errorf("%s: want refused as inactive, got %v", name, err)
			}
			if !refused && err != nil {
				t.Errorf("%s: want valid, got %v", name, err)
			}
			if queries := testingStateQueries(StateQueryConnectH1, true) - before; queries != 0 {
				t.Errorf("%s: the record's answer ran %.0f state queries", name, queries)
			}
		}
		network.deactivate(ctx, child)
		expect("deactivated child", child, true)
		expect("its active sibling", sibling, false)
		network.deactivate(ctx, top)
		expect("deactivated root", top, true)
		expect("root-branch child of the deactivated root", sibling, true)
		expect("session-branch child of the deactivated root", sessionChild, false)
	})
}

// A revoked session is refused in every shard of its network: a network
// credential's record is keyed by the session, a client credential's by its
// root, and both are refused without a query.
func TestRevocationRecordRefusesRevokedSessionInEveryShard(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := WithStateQuerySource(t.Context(), StateQueryApiControl)
		network := newTestingStateRefusalNetwork(ctx)
		account := network.sessionCredential()
		top := network.taggedClient(ctx, nil)
		child := network.taggedClient(ctx, top)
		child.SessionId = account.SessionId
		if err := ValidateByJwtState(ctx, account, false); err != nil {
			t.Fatal(err)
		}
		if err := ValidateByJwtState(ctx, child, true); err != nil {
			t.Fatal(err)
		}
		if err := RevokeSessionRecords(ctx, network.networkId, []server.Id{*account.SessionId}); err != nil {
			t.Fatal(err)
		}
		for shard := range revocationRecordShardCount {
			value, ok := testingRevocationField(ctx, t, revocationRecordShardKey(network.networkId, shard), revocationSessionField(*account.SessionId))
			if !ok || value != revocationTombstone {
				t.Fatalf("shard %d does not refuse the revoked session: %q %v", shard, value, ok)
			}
		}
		accountBefore := testingStateQueries(StateQueryApiControl, false)
		clientBefore := testingStateQueries(StateQueryApiControl, true)
		if err := ValidateByJwtState(ctx, account, false); !errors.Is(err, ErrSessionRevoked) {
			t.Errorf("revoked network session: %v", err)
		}
		if err := ValidateByJwtState(ctx, child, true); !errors.Is(err, ErrSessionRevoked) {
			t.Errorf("client credential of the revoked session: %v", err)
		}
		if queries := testingStateQueries(StateQueryApiControl, false) - accountBefore + testingStateQueries(StateQueryApiControl, true) - clientBefore; queries != 0 {
			t.Errorf("revoked-session refusals ran %.0f state queries", queries)
		}
	})
}

// A credential rotation refuses older credentials and a deletion refuses the
// whole network, both from the records the change wrote before committing.
func TestRevocationRecordRefusesRotatedAndDeletedNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := WithStateQuerySource(t.Context(), StateQueryApiControl)
		network := newTestingStateRefusalNetwork(ctx)
		top := network.taggedClient(ctx, nil)
		if err := ValidateByJwtState(ctx, top, true); err != nil {
			t.Fatal(err)
		}
		cutoff := top.CreateTime.Add(time.Second)
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(RotateCredentialRecordsInTx(ctx, tx, network.userId, cutoff))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, network.userId, cutoff))
		})
		before := testingStateQueries(StateQueryApiControl, true)
		if err := ValidateByJwtState(ctx, top, true); !errors.Is(err, ErrByJwtInactive) {
			t.Errorf("a credential created before the rotation: %v", err)
		}
		if queries := testingStateQueries(StateQueryApiControl, true) - before; queries != 0 {
			t.Errorf("the rotation refusal ran %.0f state queries", queries)
		}
		fresh := network.taggedClient(ctx, nil)
		fresh.CreateTime = cutoff.Add(time.Millisecond)
		if err := ValidateByJwtState(ctx, fresh, true); err != nil {
			t.Fatal("a credential created after the rotation was refused", err)
		}
		if err := RetireNetworkRecords(ctx, network.networkId); err != nil {
			t.Fatal(err)
		}
		before = testingStateQueries(StateQueryApiControl, true)
		if err := ValidateByJwtState(ctx, fresh, true); !errors.Is(err, ErrByJwtInactive) {
			t.Errorf("a credential of a deleted network: %v", err)
		}
		if queries := testingStateQueries(StateQueryApiControl, true) - before; queries != 0 {
			t.Errorf("the deletion refusal ran %.0f state queries", queries)
		}
	})
}

// The record is the authority: when Redis does not answer, nothing is
// accepted and SQL is not asked instead.
func TestRevocationRecordUnavailableRefusesWithoutStateQuery(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := WithStateQuerySource(t.Context(), StateQueryConnectH1)
		network := newTestingStateRefusalNetwork(ctx)
		top := network.taggedClient(ctx, nil)
		read := readRevocationRecord
		readRevocationRecord = func(context.Context, string, []string) ([]any, error) {
			return nil, errors.New("synthetic revocation store outage")
		}
		defer func() { readRevocationRecord = read }()
		before := testingStateQueries(StateQueryConnectH1, true)
		if err := ValidateByJwtState(ctx, top, true); !errors.Is(err, ErrAuthUnavailable) {
			t.Fatalf("an unavailable record answered: %v", err)
		}
		if queries := testingStateQueries(StateQueryConnectH1, true) - before; queries != 0 {
			t.Errorf("an unavailable record fell back to %.0f state queries", queries)
		}
	})
}

// Losing every record costs queries, never acceptance: SQL refuses the
// deactivated client, and the session journal refuses a revoked session whose
// network marker was lost too, or holds one a prepared operation may revoke.
func TestRevocationRecordLossCannotReviveRevokedCredentials(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := WithStateQuerySource(t.Context(), StateQueryConnectH1)
		network := newTestingStateRefusalNetwork(ctx)
		top := network.taggedClient(ctx, nil)
		child := network.taggedClient(ctx, top)
		if err := ValidateByJwtState(ctx, child, true); err != nil {
			t.Fatal(err)
		}
		network.deactivate(ctx, child)
		testingDropRevocationRecords(ctx, t, network.networkId)
		if err := ValidateByJwtState(ctx, child, true); !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("a deactivated client was accepted after its record was lost: %v", err)
		}
		if _, ok := testingRevocationField(ctx, t, revocationRecordKey(network.networkId, *top.ClientId), revocationClientField(*child.ClientId)); ok {
			t.Error("the refused client was projected")
		}

		journal := func(sessionId server.Id, status string, action string, keep *server.Id) {
			now := server.NowUtc()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_session_operation(network_id,operation_id,action,fingerprint,actor,credential_epoch,target_session_id,kept_session_id,status,target_session_ids,create_time,update_time,retain_until) VALUES($1,$2,$3,'fixture','{}',$4,$5,$6,$7,$8,$4,$4,$9)`,
					network.networkId, server.NewId(), action, now, sessionId, keep, status, `["`+sessionId.String()+`"]`, now.Add(24*time.Hour)))
			})
		}
		revoked := network.sessionCredential()
		if err := ValidateByJwtState(ctx, revoked, false); err != nil {
			t.Fatal(err)
		}
		// an enforced revocation whose network marker and record are both gone
		journal(*revoked.SessionId, "enforced", "single", nil)
		testingDropRevocationRecords(ctx, t, network.networkId)
		if err := ValidateByJwtState(ctx, revoked, false); !errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("a journaled revocation was accepted after Redis lost it: %v", err)
		}
		pending := network.sessionCredential()
		if err := ValidateByJwtState(ctx, pending, false); err != nil {
			t.Fatal(err)
		}
		kept := server.NewId()
		journal(server.NewId(), "prepared", "others", &kept)
		testingDropRevocationRecords(ctx, t, network.networkId)
		if err := ValidateByJwtState(ctx, pending, false); !errors.Is(err, ErrAuthUnavailable) {
			t.Fatalf("a session a prepared operation may revoke was answered: %v", err)
		}
		for _, sessionId := range []server.Id{*revoked.SessionId, *pending.SessionId} {
			if _, ok := testingRevocationField(ctx, t, revocationRecordKey(network.networkId, sessionId), revocationSessionField(sessionId)); ok {
				t.Error("a refused session was projected")
			}
		}
	})
}

// A projection never replaces a tombstone, is never built from a read older
// than the freshness bound, and never lowers a credential cutoff.
func TestRevocationRecordProjectionKeepsTombstonesAndRefusesStaleReads(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, userId, deviceId := server.NewId(), server.NewId(), server.NewId()
		facts := &revocationRecordFacts{credentialChangeTime: time.UnixMicro(1000).UTC(), clientActive: true, deviceId: &deviceId, rootMatches: true}
		credential := func() *ByJwt {
			return NewByJwt(networkId, userId, "revocation-record", false, false).Client(deviceId, server.NewId())
		}

		tombstoned := credential()
		key := revocationRecordKey(networkId, *tombstoned.ClientId)
		if err := writeRevocationTombstones(ctx, map[string][]string{key: {revocationClientField(*tombstoned.ClientId)}}); err != nil {
			t.Fatal(err)
		}
		projectRevocationRecord(ctx, tombstoned, *tombstoned.ClientId, server.NowUtc(), facts)
		if value, _ := testingRevocationField(ctx, t, key, revocationClientField(*tombstoned.ClientId)); value != revocationTombstone {
			t.Errorf("a projection replaced a tombstone with %q", value)
		}

		stale := credential()
		staleKey := revocationRecordKey(networkId, *stale.ClientId)
		projectRevocationRecord(ctx, stale, *stale.ClientId, server.NowUtc().Add(-2*revocationRecordFreshness), facts)
		if _, ok := testingRevocationField(ctx, t, staleKey, revocationClientField(*stale.ClientId)); ok {
			t.Error("a stale read was projected")
		}

		if err := writeRevocationNetworkFields(ctx, []server.Id{networkId}, "cred", "5000000"); err != nil {
			t.Fatal(err)
		}
		lower := credential()
		projectRevocationRecord(ctx, lower, *lower.ClientId, server.NowUtc(), facts)
		if value, _ := testingRevocationField(ctx, t, revocationRecordKey(networkId, *lower.ClientId), revocationFieldCredential); value != "5000000" {
			t.Errorf("a projection lowered the credential cutoff to %q", value)
		}

		// tombstones and projections expire; the key outlives both
		var fieldTtls []int64
		var keyTtl time.Duration
		err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			var err error
			fieldTtls, err = r.HPTTL(ctx, key, revocationClientField(*tombstoned.ClientId)).Result()
			if err != nil {
				return err
			}
			keyTtl, err = r.PTTL(ctx, key).Result()
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(fieldTtls) != 1 || fieldTtls[0] <= 0 || revocationRecordTombstoneTtl.Milliseconds() < fieldTtls[0] {
			t.Errorf("tombstone ttl %v, want at most %s", fieldTtls, revocationRecordTombstoneTtl)
		}
		if keyTtl < revocationRecordTombstoneTtl || revocationRecordKeyTtl < keyTtl {
			t.Errorf("record key ttl %s", keyTtl)
		}
	})
}

// A lease's rechecks read the record: a projected credential renews without a
// query, and a deactivation retires the transport with the invalid cause at
// the next recheck, which a session event wakes at once.
func TestAuthorizationLeaseRechecksReadTheRevocationRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		top := network.taggedClient(ctx, nil)
		closes := make(chan int, 1)
		lease, err := StartAuthorizationLease(ctx, top, func(code int) { closes <- code })
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		results := make(chan error, 4)
		lease.Testing_InterposeRecheck(func(check func(context.Context, *ByJwt) error) func(context.Context, *ByJwt) error {
			return func(ctx context.Context, credential *ByJwt) error {
				err := check(ctx, credential)
				results <- err
				return err
			}
		})
		recheck := func() error {
			t.Helper()
			lease.Recheck()
			select {
			case err := <-results:
				return err
			case <-time.After(10 * time.Second):
				t.Fatal("the recheck never ran")
				return nil
			}
		}
		before := testingStateQueries(StateQueryUnknown, true)
		if err := recheck(); err != nil {
			t.Fatal("a projected credential's recheck refused it", err)
		}
		if queries := testingStateQueries(StateQueryUnknown, true) - before; queries != 0 {
			t.Errorf("a projected recheck ran %.0f state queries", queries)
		}
		network.deactivate(ctx, top)
		if err := recheck(); !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("the recheck after deactivation: %v", err)
		}
		select {
		case code := <-closes:
			if code != AuthorizationRejectedCloseCode {
				t.Errorf("retired with cause %d, want invalid %d", code, AuthorizationRejectedCloseCode)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the deactivated client's transport was not retired")
		}
		if queries := testingStateQueries(StateQueryUnknown, true) - before; queries != 0 {
			t.Errorf("the lease's record answers ran %.0f state queries", queries)
		}
	})
}

// An operation is enforced only once every record of its network refuses its
// targets. A failed record write leaves it prepared, and neither enforcement
// nor a status read of its applied receipt claims enforced; the next status
// read or replay finishes the tombstones from the saved receipt.
func TestSessionOperationEnforcedOnlyAfterEveryRecordRefusesItsTargets(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		actor := recoverySessionActor(t)
		ctx := actor.Ctx
		networkId := actor.ByJwt.NetworkId
		target := sessionFixtureCredential(networkId, server.NowUtc())
		if _, err := RegisterNetworkSession(ctx, target, "password", nil, false, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		tombstoned := func() int {
			count := 0
			for shard := range revocationRecordShardCount {
				if value, ok := testingRevocationField(ctx, t, revocationRecordShardKey(networkId, shard), revocationSessionField(*target.SessionId)); ok && value == revocationTombstone {
					count++
				}
			}
			return count
		}
		op := server.NewId()
		if err := prepareSessionOperation(actor, op, "single", target.SessionId, nil); err != nil {
			t.Fatal(err)
		}
		reader := NewLocalClientSession(ctx, "192.0.2.1:1", NewByJwt(networkId, actor.ByJwt.UserId, "session-recovery", false, false))
		defer reader.Cancel()

		restore := Testing_FailRevocationRecordWrites(errors.New("synthetic revocation store outage"))
		_, enforceErr := EnforceSessionOperation(ctx, networkId, op)
		_, statusErr := GetSessionOperation(reader, op)
		restore()
		if enforceErr == nil {
			t.Fatal("an operation was enforced without its record writes")
		}
		if statusErr == nil {
			t.Fatal("an applied receipt was reported without its record writes")
		}
		if count := tombstoned(); count != 0 {
			t.Fatalf("%d shards refuse the target although every write failed", count)
		}
		var status string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT status FROM network_session_operation WHERE network_id=$1 AND operation_id=$2`, networkId, op).Scan(&status))
		})
		if status != "prepared" {
			t.Fatalf("an operation without its record writes is %s", status)
		}

		observed, err := GetSessionOperation(reader, op)
		if err != nil || observed.State != "enforced" {
			t.Fatal("the applied receipt's status read failed", observed, err)
		}
		if count := tombstoned(); count != revocationRecordShardCount {
			t.Fatalf("the status read tombstoned %d of %d shards", count, revocationRecordShardCount)
		}
		recovered, err := EnforceSessionOperation(ctx, networkId, op)
		if err != nil || recovered == nil || recovered.State != "enforced" {
			t.Fatal("the replay did not enforce", recovered, err)
		}
	})
}

// Every client-credential state query attempt, from any source.
func testingAllClientStateQueries() float64 {
	total := 0.0
	for source := range stateQuerySourceCount {
		total += testingStateQueries(source, true)
	}
	return total
}

// The API's answered path reads the record too: the checks before the answer
// leave the session to the record, a projected credential authenticates
// without a state query, and a deactivated one is refused without one.
func TestClientSessionAnswerPathReadsTheRevocationRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		top := network.taggedClient(ctx, nil)
		signed := top.Testing_Sign()
		authenticate := func() (bool, error) {
			request, err := http.NewRequest(http.MethodGet, "https://api.example.test/", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+signed)
			clientSession := NewLocalClientSession(ctx, "127.0.0.1:1", nil)
			defer clientSession.Cancel()
			return clientSession.AuthUnlessAnswered(request, func(context.Context, server.Id) bool { return false })
		}
		before := testingAllClientStateQueries()
		for attempt := range 2 {
			if answered, err := authenticate(); answered || err != nil {
				t.Fatalf("attempt %d: a projected credential was not authenticated: answered %v, %v", attempt, answered, err)
			}
		}
		if queries := testingAllClientStateQueries() - before; queries != 1 {
			t.Errorf("two answered-path authentications ran %.0f state queries, want only the first", queries)
		}
		network.deactivate(ctx, top)
		before = testingAllClientStateQueries()
		if answered, err := authenticate(); answered || err == nil {
			t.Fatalf("a deactivated client authenticated: answered %v, %v", answered, err)
		}
		if queries := testingAllClientStateQueries() - before; queries != 0 {
			t.Errorf("the record's refusal ran %.0f state queries", queries)
		}
	})
}
