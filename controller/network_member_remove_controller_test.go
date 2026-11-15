//go:build sidecar_pending

package controller

// POST /network/member-remove (network_member_remove_controller.go; TEAM-M6-11, §7.10), which needs MG6: the
// server's denial at once, its member's untagged clients and rev/ object with it, its tagged agents left; every
// refusal the route owes; and a one-command admin token spent once. The team tier is stubbed on, since QUOTA-M6-01's
// tier is not in transfer_balance yet.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A team with an Owner, an Admin, an Auditor and a Member, each with a credential, and the removals with the tier on.
type memberRemoveFixture struct {
	ctx           context.Context
	teamNetworkId server.Id
	members       map[model.NetworkMemberRole]*model.NetworkMember
	sessions      map[model.NetworkMemberRole]*session.ClientSession
	removals      *networkMemberRemovals
	now           time.Time
}

func newMemberRemoveFixture(t testing.TB, label string) *memberRemoveFixture {
	t.Helper()
	ctx := context.Background()
	_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
	t.Cleanup(restore)
	_, ownerUserId, personal := testSwitchPersonalNetwork(t, ctx, label+"-owner")
	teamName := fmt.Sprintf(label+"-team-%s", strings.ReplaceAll(server.NewId().String(), "-", "")[24:32])
	created, err := model.CreateNetworkTeam(&model.CreateNetworkTeamArgs{NetworkName: teamName}, personal)
	if err != nil || created.Error != nil || created.NetworkId == nil {
		t.Fatalf("team-create: %v %+v", err, created)
	}
	self := &memberRemoveFixture{
		ctx:           ctx,
		teamNetworkId: *created.NetworkId,
		members:       map[model.NetworkMemberRole]*model.NetworkMember{},
		sessions:      map[model.NetworkMemberRole]*session.ClientSession{},
		now:           server.NowUtc(),
	}
	self.removals = &networkMemberRemovals{
		now:         func() time.Time { return self.now },
		requireTier: func(ctx context.Context, networkId server.Id) error { return nil },
	}
	owner := model.GetNetworkMemberOfUser(ctx, self.teamNetworkId, ownerUserId)
	self.members[model.NetworkMemberRoleOwner] = owner
	for _, role := range []model.NetworkMemberRole{model.NetworkMemberRoleAdmin, model.NetworkMemberRoleAuditor, model.NetworkMemberRoleMember} {
		self.members[role] = self.addMember(t, role)
	}
	for role, member := range self.members {
		memberId := member.MemberId
		self.sessions[role] = session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId:   self.teamNetworkId,
			UserId:      member.UserId,
			NetworkName: teamName,
			MemberId:    &memberId,
		})
	}
	return self
}

// A new member of the team with role, as the config repo's mirror would set it.
func (self *memberRemoveFixture) addMember(t testing.TB, role model.NetworkMemberRole) *model.NetworkMember {
	t.Helper()
	var member *model.NetworkMember
	server.Tx(self.ctx, func(tx server.PgTx) {
		var err error
		member, err = model.AddNetworkMemberInTx(self.ctx, tx, &model.AddNetworkMemberArgs{
			NetworkId: self.teamNetworkId,
			UserId:    server.NewId(),
			Role:      role,
		})
		server.Raise(err)
	})
	return member
}

// A client of the member's, tagged or not, as an enrollment's owner row records it.
func (self *memberRemoveFixture) addClient(t testing.TB, member *model.NetworkMember, tagged bool) server.Id {
	t.Helper()
	clientId := server.NewId()
	model.Testing_CreateDevice(self.ctx, self.teamNetworkId, server.NewId(), clientId, "remove-device", "test")
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			self.ctx,
			`INSERT INTO network_client_owner (client_id, owner_user_id, member_id, tagged) VALUES ($1, $2, $3, $4)`,
			clientId,
			member.UserId,
			member.MemberId,
			tagged,
		))
	})
	return clientId
}

// Whether the client is active.
func (self *memberRemoveFixture) clientActive(t testing.TB, clientId server.Id) (active bool) {
	t.Helper()
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT active FROM network_client WHERE client_id = $1`, clientId).Scan(&active))
	})
	return
}

// The team's admin event actions, in order.
func (self *memberRemoveFixture) eventActions() []string {
	actions := []string{}
	for _, event := range model.GetNetworkAdminEvents(self.ctx, self.teamNetworkId, 0, 100) {
		actions = append(actions, event.Action)
	}
	return actions
}

// Whether a pending rev/ write task owes an object of subjectId.
func (self *memberRemoveFixture) revOwed(t testing.TB, subjectId server.Id) bool {
	t.Helper()
	for _, pending := range task.GetTasks(self.ctx, task.ListPendingTasks(self.ctx)...) {
		if !strings.HasSuffix(pending.FunctionName, "WriteSidecarRevTask") {
			continue
		}
		args := &model.WriteSidecarRevArgs{}
		if err := json.Unmarshal([]byte(pending.ArgsJson), args); err != nil {
			t.Fatal(err)
		}
		for _, revocation := range args.Revocations {
			if revocation.SubjectId == subjectId {
				return true
			}
		}
	}
	return false
}

// An Owner removes a Member: the membership's row goes, so its role reads as none and its credentials stop; its
// untagged client is deactivated and its tagged agent stays; the member's rev/ object is owed; one admin event; and
// the answer says the MemberRevoke is still owed, outside the AdminSet.
func TestMemberRemoveDeniesAtOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newMemberRemoveFixture(t, "remove")
		target := self.members[model.NetworkMemberRoleMember]
		own := self.addClient(t, target, false)
		agent := self.addClient(t, target, true)

		result, err := self.removals.remove(&RemoveNetworkMemberArgs{MemberId: target.MemberId}, self.sessions[model.NetworkMemberRoleOwner])
		connect.AssertEqual(t, err, nil)
		if result.Error != nil {
			t.Fatalf("the removal: %+v", result.Error)
		}
		connect.AssertEqual(t, *result.MemberId, target.MemberId)
		connect.AssertEqual(t, result.SignatureOwed, true)
		connect.AssertEqual(t, result.AdminSet, false)
		connect.AssertEqual(t, result.DeactivatedClientCount, int64(1))

		connect.AssertEqual(t, model.GetNetworkMember(self.ctx, target.MemberId) == nil, true)
		_, ok := model.MemberRole(self.ctx, self.teamNetworkId, target.MemberId)
		connect.AssertEqual(t, ok, false)
		connect.AssertEqual(t, self.clientActive(t, own), false)
		// a removed member's tagged agents keep their tags' grants (owner decision 83)
		connect.AssertEqual(t, self.clientActive(t, agent), true)
		connect.AssertEqual(t, self.revOwed(t, target.MemberId), true)
		actions := self.eventActions()
		connect.AssertEqual(t, actions[len(actions)-1], model.AdminEventActionMemberRemove)

		// a second removal finds no member, and changes nothing
		again, err := self.removals.remove(&RemoveNetworkMemberArgs{MemberId: target.MemberId}, self.sessions[model.NetworkMemberRoleOwner])
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again.Error.Message, model.ErrNetworkMemberNotFound.Error())
		connect.AssertEqual(t, len(self.eventActions()), len(actions))
	})
}

// An AdminSet member is removed only by an Owner, its answer naming k of n; the team's last Owner never.
func TestMemberRemoveOfTheAdminSet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newMemberRemoveFixture(t, "remove")
		otherAdmin := self.addMember(t, model.NetworkMemberRoleItAdmin)

		refused, err := self.removals.remove(&RemoveNetworkMemberArgs{MemberId: otherAdmin.MemberId}, self.sessions[model.NetworkMemberRoleAdmin])
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refused.Error.Message, networkMemberRemoveAdminSetMessage)
		connect.AssertEqual(t, model.GetNetworkMember(self.ctx, otherAdmin.MemberId) != nil, true)

		owner := self.members[model.NetworkMemberRoleOwner]
		refused, err = self.removals.remove(&RemoveNetworkMemberArgs{MemberId: owner.MemberId}, self.sessions[model.NetworkMemberRoleOwner])
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refused.Error.Message, networkMemberRemoveLastOwnerMessage)

		removed, err := self.removals.remove(&RemoveNetworkMemberArgs{MemberId: otherAdmin.MemberId}, self.sessions[model.NetworkMemberRoleOwner])
		connect.AssertEqual(t, err, nil)
		if removed.Error != nil {
			t.Fatalf("the removal: %+v", removed.Error)
		}
		connect.AssertEqual(t, removed.AdminSet, true)
		connect.AssertEqual(t, removed.SignatureOwed, true)

		// with a second Owner, the first may go
		second := self.addMember(t, model.NetworkMemberRoleOwner)
		connect.AssertEqual(t, second != nil, true)
		removed, err = self.removals.remove(&RemoveNetworkMemberArgs{MemberId: owner.MemberId}, self.sessions[model.NetworkMemberRoleOwner])
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removed.Error == nil && removed.AdminSet, true)
	})
}

// Who may not remove: an Auditor, a client token, an API key, a network outside the team tier; and what is not
// removed: an unknown member, another team's member. None changes the team.
func TestMemberRemoveRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newMemberRemoveFixture(t, "remove")
		target := self.members[model.NetworkMemberRoleMember]
		args := &RemoveNetworkMemberArgs{MemberId: target.MemberId}

		if _, err := self.removals.remove(args, self.sessions[model.NetworkMemberRoleAuditor]); err == nil || !strings.Contains(err.Error(), "403") {
			t.Errorf("an Auditor: %v", err)
		}
		owner := self.members[model.NetworkMemberRoleOwner]
		ownerMemberId := owner.MemberId
		clientId := server.NewId()
		client := session.Testing_CreateClientSession(self.ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: owner.UserId, MemberId: &ownerMemberId, ClientId: &clientId})
		if _, err := self.removals.remove(args, client); err == nil || !strings.Contains(err.Error(), "403") {
			t.Errorf("a client token: %v", err)
		}
		apiKey := session.Testing_CreateClientSession(self.ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: owner.UserId, MemberId: &ownerMemberId})
		apiKey.ApiKeyAuthenticated = true
		if _, err := self.removals.remove(args, apiKey); err == nil || !strings.Contains(err.Error(), "403") {
			t.Errorf("an API key: %v", err)
		}
		outside := &networkMemberRemovals{
			now: func() time.Time { return self.now },
			requireTier: func(ctx context.Context, networkId server.Id) error {
				return model.RequireTeamTier(ctx, server.NewId())
			},
		}
		if _, err := outside.remove(args, self.sessions[model.NetworkMemberRoleOwner]); err == nil {
			t.Error("a network outside the team tier removed a member")
		}

		unknown, err := self.removals.remove(&RemoveNetworkMemberArgs{MemberId: server.NewId()}, self.sessions[model.NetworkMemberRoleOwner])
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, unknown.Error.Message, model.ErrNetworkMemberNotFound.Error())

		other := newMemberRemoveFixture(t, "remove-other")
		foreign, err := self.removals.remove(&RemoveNetworkMemberArgs{MemberId: other.members[model.NetworkMemberRoleMember].MemberId}, self.sessions[model.NetworkMemberRoleOwner])
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, foreign.Error.Message, model.ErrNetworkMemberNotFound.Error())
		connect.AssertEqual(t, model.GetNetworkMember(self.ctx, other.members[model.NetworkMemberRoleMember].MemberId) != nil, true)

		connect.AssertEqual(t, model.GetNetworkMember(self.ctx, target.MemberId) != nil, true)
		for _, action := range self.eventActions() {
			connect.AssertEqual(t, action != model.AdminEventActionMemberRemove, true)
		}
	})
}

// A one-command admin token with the all scope removes once: a retry of its jti answers the first result and runs
// nothing; a token without all is refused before anything.
func TestMemberRemoveByAdminTokenOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newMemberRemoveFixture(t, "remove")
		target := self.members[model.NetworkMemberRoleMember]
		admin := self.members[model.NetworkMemberRoleAdmin]
		adminMemberId := admin.MemberId
		tokenSession := func(scopes []string, jti string) *session.ClientSession {
			clientSession := session.Testing_CreateClientSession(self.ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: admin.UserId, MemberId: &adminMemberId})
			clientSession.UnsignedIdentity = true
			clientSession.AdminCommand = &session.AdminCommandUse{
				NetworkId:  self.teamNetworkId,
				MemberId:   adminMemberId,
				Jti:        jti,
				Cmd:        "member-remove-test-cmd",
				Scopes:     scopes,
				ExpireTime: self.now.Add(time.Hour),
			}
			return clientSession
		}
		args := &RemoveNetworkMemberArgs{MemberId: target.MemberId}
		if _, err := self.removals.remove(args, tokenSession([]string{"auth_keys"}, server.NewId().String())); err == nil {
			t.Error("a token without the all scope removed a member")
		}

		jti := server.NewId().String()
		first, err := self.removals.remove(args, tokenSession([]string{"all"}, jti))
		connect.AssertEqual(t, err, nil)
		if first.Error != nil {
			t.Fatalf("the removal: %+v", first.Error)
		}
		eventCount := len(self.eventActions())
		replay, err := self.removals.remove(args, tokenSession([]string{"all"}, jti))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, replay.Error == nil && *replay.MemberId == target.MemberId && replay.SignatureOwed, true)
		connect.AssertEqual(t, len(self.eventActions()), eventCount)
	})
}
