//go:build sidecar_pending

package session

// A team member's credentials (TEAM-M6-02; R-TEAM-2, R-TEAM-3, D-TEAM-2): the
// state check reads network_member for a credential naming a member, never the
// team's synthetic owner of record, on Postgres with MG6.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A team: its synthetic owner of record, which never signs in, and a device.
type testMemberTeam struct {
	networkId   server.Id
	ownerUserId server.Id
	deviceId    server.Id
}

func newTestMemberTeam(t testing.TB, ctx context.Context) *testMemberTeam {
	t.Helper()
	team := &testMemberTeam{networkId: server.NewId(), ownerUserId: server.NewId(), deviceId: server.NewId()}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user(user_id,user_name,auth_type,verified) VALUES($1,'owner-of-record','password',true)`, team.ownerUserId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id) VALUES($1,$2,$3)`, team.networkId, "team-"+team.networkId.String(), team.ownerUserId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team(network_id) VALUES($1)`, team.networkId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO device(device_id,network_id,device_name,device_spec) VALUES($1,$2,'synthetic','synthetic')`, team.deviceId, team.networkId))
	})
	return team
}

// A user with a personal network of its own.
func newTestMemberUser(t testing.TB, ctx context.Context) (userId server.Id, personalNetworkId server.Id) {
	t.Helper()
	userId, personalNetworkId = server.NewId(), server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user(user_id,user_name,auth_type,verified) VALUES($1,'member','password',true)`, userId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id) VALUES($1,$2,$3)`, personalNetworkId, "personal-"+personalNetworkId.String(), userId))
	})
	return
}

// Adds userId to the team and returns the membership's id.
func (self *testMemberTeam) join(t testing.TB, ctx context.Context, userId server.Id, pending bool, reauthDeadline *time.Time) server.Id {
	t.Helper()
	memberId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_member(member_id,network_id,user_id,role,pending,reauth_deadline) VALUES($1,$2,$3,'member',$4,$5)`,
			memberId, self.networkId, userId, pending, reauthDeadline,
		))
	})
	return memberId
}

// A member's network credential for the team.
func (self *testMemberTeam) credential(userId server.Id, memberId server.Id) *ByJwt {
	byJwt := NewByJwt(self.networkId, userId, "team", false, false)
	byJwt.MemberId = &memberId
	return byJwt
}

// A member's token validates in its team; the same user with no member claim,
// or the member claim in another network, or another membership's id, does
// not, since the owner of record is another user.
func TestMemberTokenValidatesOnlyInItsNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		team := newTestMemberTeam(t, ctx)
		userId, personalNetworkId := newTestMemberUser(t, ctx)
		memberId := team.join(t, ctx, userId, false, nil)

		if err := ValidateByJwtState(ctx, team.credential(userId, memberId), false); err != nil {
			t.Fatalf("the member's token: %v", err)
		}
		// no member claim: the team's owner check, which the member is not
		if err := ValidateByJwtState(ctx, NewByJwt(team.networkId, userId, "team", false, false), false); err == nil {
			t.Fatal("a member passed as the team's owner")
		}
		// the member claim in the user's personal network names no row there
		elsewhere := NewByJwt(personalNetworkId, userId, "personal", false, false)
		elsewhere.MemberId = &memberId
		if err := ValidateByJwtState(ctx, elsewhere, false); err == nil {
			t.Fatal("a membership validated in another network")
		}
		// another membership's id
		otherMemberId := server.NewId()
		if err := ValidateByJwtState(ctx, team.credential(userId, otherMemberId), false); err == nil {
			t.Fatal("an unknown membership validated")
		}
		// the owner of record's own credential is no member's
		if err := ValidateByJwtState(ctx, team.credential(team.ownerUserId, memberId), false); err == nil {
			t.Fatal("the owner of record took a member's credential")
		}
	})
}

// A removed member's token fails: the row is gone, and a re-join is a new
// member_id, which the old token does not name.
func TestRemovedMemberTokenFails(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		team := newTestMemberTeam(t, ctx)
		userId, _ := newTestMemberUser(t, ctx)
		memberId := team.join(t, ctx, userId, false, nil)
		credential := team.credential(userId, memberId)
		if err := ValidateByJwtState(ctx, credential, false); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_member WHERE member_id = $1`, memberId))
		})
		if err := ValidateByJwtState(ctx, credential, false); err == nil {
			t.Fatal("a removed member's token still validates")
		}
		rejoinedId := team.join(t, ctx, userId, false, nil)
		if err := ValidateByJwtState(ctx, credential, false); err == nil {
			t.Fatal("a re-join revived the earlier membership's token")
		}
		if err := ValidateByJwtState(ctx, team.credential(userId, rejoinedId), false); err != nil {
			t.Fatalf("the new membership's token: %v", err)
		}
	})
}

// One person in two networks: its personal token stays in its personal
// network and its member token in the team, neither reaching the other.
func TestOnePersonInTwoNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		team := newTestMemberTeam(t, ctx)
		userId, personalNetworkId := newTestMemberUser(t, ctx)
		memberId := team.join(t, ctx, userId, false, nil)

		personal := NewByJwt(personalNetworkId, userId, "personal", false, false)
		if err := ValidateByJwtState(ctx, personal, false); err != nil {
			t.Fatalf("the personal token: %v", err)
		}
		if err := ValidateByJwtState(ctx, team.credential(userId, memberId), false); err != nil {
			t.Fatalf("the member token: %v", err)
		}
		crossed := NewByJwt(team.networkId, userId, "personal", false, false)
		if err := ValidateByJwtState(ctx, crossed, false); err == nil {
			t.Fatal("the personal credential reached the team")
		}
	})
}

// A JIT pending member reaches nothing, and a member past its reauth_deadline
// is refused until the IdP signs it in again (§7.6).
func TestPendingAndLapsedMembersAreRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		team := newTestMemberTeam(t, ctx)
		pendingUserId, _ := newTestMemberUser(t, ctx)
		pendingId := team.join(t, ctx, pendingUserId, true, nil)
		if err := ValidateByJwtState(ctx, team.credential(pendingUserId, pendingId), false); err == nil {
			t.Fatal("a pending member's token validated")
		}
		lapsedUserId, _ := newTestMemberUser(t, ctx)
		lapsed := server.NowUtc().Add(-time.Minute)
		lapsedId := team.join(t, ctx, lapsedUserId, false, &lapsed)
		if err := ValidateByJwtState(ctx, team.credential(lapsedUserId, lapsedId), false); err == nil {
			t.Fatal("a member past its reauth_deadline validated")
		}
		liveUserId, _ := newTestMemberUser(t, ctx)
		future := server.NowUtc().Add(time.Hour)
		liveId := team.join(t, ctx, liveUserId, false, &future)
		if err := ValidateByJwtState(ctx, team.credential(liveUserId, liveId), false); err != nil {
			t.Fatalf("a member inside its reauth_deadline: %v", err)
		}
	})
}

// A member's client credential keeps its member claim, and validates against
// the membership and the active client together.
func TestMemberClientCredential(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		team := newTestMemberTeam(t, ctx)
		userId, _ := newTestMemberUser(t, ctx)
		memberId := team.join(t, ctx, userId, false, nil)
		clientId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,device_id,description,create_time,auth_time,active) VALUES($1,$2,$3,'synthetic',$4,$4,true)`, clientId, team.networkId, team.deviceId, server.NowUtc()))
		})
		credential := team.credential(userId, memberId).Client(team.deviceId, clientId)
		if credential.MemberId == nil || *credential.MemberId != memberId {
			t.Fatal("a client credential lost its member claim")
		}
		if err := ValidateByJwtState(ctx, credential, true); err != nil {
			t.Fatalf("the member's client: %v", err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_member WHERE member_id = $1`, memberId))
		})
		if err := ValidateByJwtState(ctx, credential, true); err == nil {
			t.Fatal("a removed member's client still validates")
		}
	})
}
