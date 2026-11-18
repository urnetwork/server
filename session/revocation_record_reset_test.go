package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A network with a unique name, its admin user and a device.
type testingResetNetwork struct {
	networkId server.Id
	userId    server.Id
	deviceId  server.Id
}

func newTestingResetNetwork(ctx context.Context) *testingResetNetwork {
	network := &testingResetNetwork{networkId: server.NewId(), userId: server.NewId(), deviceId: server.NewId()}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user(user_id,user_name,auth_type,verified) VALUES($1,'session-reset','password',true)`, network.userId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id) VALUES($1,$2,$3)`, network.networkId, "session-reset-"+network.networkId.String(), network.userId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO device(device_id,network_id,device_name,device_spec) VALUES($1,$2,'synthetic','synthetic')`, network.deviceId, network.networkId))
	})
	return network
}

// A client row bound to the credential's session, under parent (top level when
// nil), and the client's credential carrying the session.
func (self *testingResetNetwork) sessionClient(ctx context.Context, issuer *ByJwt, parent *ByJwt) *ByJwt {
	clientId := server.NewId()
	var parentClientId *server.Id
	if parent != nil {
		issuer = parent
		parentClientId = parent.ClientId
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,device_id,description,active,source_client_id,session_id,session_create_time) VALUES($1,$2,$3,'synthetic',true,$4,$5,$6)`, clientId, self.networkId, self.deviceId, parentClientId, issuer.SessionId, issuer.CreateTime))
	})
	return issuer.Client(self.deviceId, clientId)
}

// F4 (approved 2026-11-18): every revoked session was tombstoned in all 64
// record shards, so a sign-out-all of a network with 5,000 sessions wrote
// 320k fields. A password reset (sign-out-all) and a network deletion write
// no session tombstone now: the credential cutoff or the deletion mark their
// own transaction wrote to every shard refuses every target. Every path that
// reads s:<session> still refuses the session's credentials, including the
// client and network credentials whose positive s:<session> projections sit
// in shards no tombstone reached.
func TestResetAndDeletionWriteNoSessionTombstonesYetRefuseEveryTarget(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer Testing_SetRejectExpired(false)()
		for _, action := range []string{"reset", "delete"} {
			restoreRecord := Testing_SetRevocationRecordEnabled(true)
			ctx := WithStateQuerySource(t.Context(), StateQueryApiControl)
			network := newTestingResetNetwork(ctx)
			signIn := NewByJwt(network.networkId, network.userId, "session-reset", false, false)
			sid := server.NewId()
			signIn.SessionId = &sid
			if _, err := RegisterNetworkSession(ctx, signIn, "password", nil, false, server.NowUtc()); err != nil {
				t.Fatal(err)
			}
			client := network.sessionClient(ctx, signIn, nil)
			child := network.sessionClient(ctx, signIn, client)

			// positive projections of the session in its own shard and its
			// client's root shard
			for name, check := range map[string]func() error{
				"sign-in":      func() error { return ValidateByJwtState(ctx, signIn, false) },
				"client":       func() error { return ValidateByJwtState(ctx, client, true) },
				"hosted child": func() error { return ValidateByJwtStateForParent(ctx, child, *client.ClientId) },
			} {
				if err := check(); err != nil {
					t.Fatalf("%s %s before the %s: %v", action, name, action, err)
				}
			}
			for _, lineage := range []server.Id{sid, *client.ClientId} {
				if value, ok := testingRevocationField(ctx, t, revocationRecordKey(network.networkId, lineage), revocationSessionField(sid)); !ok || value != revocationActive {
					t.Fatalf("%s: the session was not projected in the shard of %s: %q %v", action, lineage, value, ok)
				}
			}
			closes := make(chan int, 1)
			lease, err := StartAuthorizationLease(WithStateQuerySource(ctx, StateQueryConnectH1), client, func(code int) { closes <- code })
			if err != nil {
				t.Fatal(err)
			}

			op := server.NewId()
			cutoff := signIn.CreateTime.Add(time.Second)
			server.Tx(ctx, func(tx server.PgTx) {
				switch action {
				case "reset":
					server.Raise(JournalSessionRetirementInTx(ctx, tx, network.networkId, op, "reset", cutoff))
					server.Raise(RotateCredentialRecordsInTx(ctx, tx, network.userId, cutoff))
					server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, network.userId, cutoff))
				case "delete":
					server.Raise(JournalSessionRetirementInTx(ctx, tx, network.networkId, op, "delete", server.NowUtc()))
					server.Raise(RetireNetworkRecords(ctx, network.networkId))
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network WHERE network_id=$1`, network.networkId))
				}
			})
			result, err := EnforceSessionOperation(ctx, network.networkId, op)
			if err != nil || result.State != "enforced" || len(result.TargetSessionIds) != 1 || result.TargetSessionIds[0] != sid {
				t.Fatalf("%s: the operation was not enforced on the session: %v %v", action, result, err)
			}

			// no session tombstone in any shard
			for shard := range revocationRecordShardCount {
				if value, ok := testingRevocationField(ctx, t, revocationRecordShardKey(network.networkId, shard), revocationSessionField(sid)); ok && value == revocationTombstone {
					t.Fatalf("%s wrote a session tombstone in shard %d", action, shard)
				}
			}

			refused := func(path string) {
				t.Helper()
				for name, check := range map[string]func() error{
					"sign-in":      func() error { return ValidateByJwtState(ctx, signIn, false) },
					"client":       func() error { return ValidateByJwtState(ctx, client, true) },
					"hosted child": func() error { return ValidateByJwtStateForParent(ctx, child, *client.ClientId) },
					"transaction": func() (err error) {
						server.Tx(ctx, func(tx server.PgTx) { err = ValidateByJwtStateInTx(ctx, tx, client, true) })
						return
					},
				} {
					if err := check(); err == nil || errors.Is(err, ErrAuthUnavailable) {
						t.Fatalf("%s %s %s answered %v", action, path, name, err)
					}
				}
			}
			// the records, whose positive projections are still in place
			refused("record")
			lease.Recheck()
			select {
			case code := <-closes:
				if code != AuthorizationRejectedCloseCode && code != AuthorizationRevokedCloseCode {
					t.Fatalf("%s: the lease closed with %d", action, code)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s: the lease recheck did not retire the transport", action)
			}
			lease.Close()
			// the records lost: the SQL fallback, through the marker and journal
			testingDropRevocationRecords(ctx, t, network.networkId)
			refused("fallback")
			// the record off: the session marker
			restoreRecord()
			restoreMarker := Testing_SetRevocationRecordEnabled(false)
			refused("marker")
			restoreMarker()
		}
	})
}
