//go:build sidecar_pending

// T3 for a team member's sidecar at the block (owner decision 561, QD-1013), on the private test Postgres and Redis
// with MG6: a member's by client jwt, carrying member_id and its sidecar, projected into the team's revocation record by
// the SQL fallback as connect's recheck projects it, is answered by the block's own Authenticator, which reads only the
// record; once the membership's removal commits, its tombstone first, the same token answers 401.

package main

import (
	"net/http"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestHostAnswersATeamMembersClientToken(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer session.Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		jwks, err := session.Testing_ByJwtPublicJwks()
		if err != nil {
			t.Fatal(err)
		}
		defer server.Vault.PushSimpleResource(session.ByJwtPublicJwksResourceName, jwks)()

		// a team, its synthetic owner of record, and a member with a client of the team
		teamId, ownerOfRecordId, userId, memberId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user(user_id,user_name,auth_type,verified) VALUES($1,'owner-of-record','password',true)`, ownerOfRecordId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id) VALUES($1,$2,$3)`, teamId, "team-"+teamId.String(), ownerOfRecordId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team(network_id) VALUES($1)`, teamId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO device(device_id,network_id,device_name,device_spec) VALUES($1,$2,'synthetic','synthetic')`, deviceId, teamId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user(user_id,user_name,auth_type,verified) VALUES($1,'member','password',true)`, userId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_member(member_id,network_id,user_id,role,pending) VALUES($1,$2,$3,'member',false)`, memberId, teamId, userId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,device_id,description,create_time,auth_time,active) VALUES($1,$2,$3,'synthetic',$4,$4,true)`, clientId, teamId, deviceId, server.NowUtc()))
		})
		member := session.NewByJwt(teamId, userId, "team", false, false)
		member.MemberId = &memberId
		client := member.Client(deviceId, clientId)
		sidecarId := server.NewId()
		client.SidecarId = &sidecarId
		token := client.Testing_Sign()
		projectClientAuthTestRecord(t, ctx, client)

		world := newHostTestWorld(t)
		host := world.newHost(t, func(deps *hostDeps) {
			deps.Authenticator = nil
		})
		code, result := clientAuthTestList(t, host, token)
		if code != http.StatusOK || result.Error != nil {
			t.Fatalf("a projected member's token with its sidecar answered %d, %+v", code, result)
		}

		// the membership's removal, as TEAM's writers commit it: the record's tombstone first
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(session.RevokeMemberRecords(ctx, map[server.Id][]server.Id{teamId: {memberId}}))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_member WHERE member_id = $1`, memberId))
		})
		if code, result := clientAuthTestList(t, host, token); code != http.StatusUnauthorized {
			t.Fatalf("a removed member's token answered %d, %+v", code, result)
		}
	})
}
