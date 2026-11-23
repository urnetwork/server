//go:build sidecar_pending

package controller

// POST /network/member-remove ends the auth keys the member's user made in the team (QD-1211; §7.10, D-TEAM-31): the
// answer counts them and, when one was pre-approved, says that a copy of its bundle still admits devices until its
// Delegation ends, as a key's revoke answers; each key owes an auth_key.revoke admin event naming the remover before
// the removal's member.remove.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The member's new auth key, made with its own credential.
func (self *memberRemoveFixture) addAuthKey(t testing.TB, role model.NetworkMemberRole, args *model.CreateAuthKeyArgs) server.Id {
	t.Helper()
	args.DurationSeconds, args.MaxUses = 3600, 1
	created, err := model.CreateAuthKey(args, self.sessions[role])
	if err != nil || created.Error != nil || created.AuthKeyId == nil {
		t.Fatalf("the %s's auth key: %v %+v", role, err, created)
	}
	return *created.AuthKeyId
}

// An Owner removes an Admin that made an untagged and a pre-approved key: the answer counts both, says the
// pre-approved key's bundle still admits and what to do about it; the team's events end in an auth_key.revoke for
// each key, by the Owner, then the member.remove. A Member whose one key is untagged is answered without the note.
func TestMemberRemoveEndsTheMembersAuthKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newMemberRemoveFixture(t, "remove-keys")
		untaggedKeyId := self.addAuthKey(t, model.NetworkMemberRoleAdmin, &model.CreateAuthKeyArgs{})
		preApprovedKeyId := self.addAuthKey(t, model.NetworkMemberRoleAdmin, &model.CreateAuthKeyArgs{PreApproved: true})
		memberKeyId := self.addAuthKey(t, model.NetworkMemberRoleMember, &model.CreateAuthKeyArgs{})
		headCount := len(self.eventActions())

		admin := self.members[model.NetworkMemberRoleAdmin]
		owner := self.members[model.NetworkMemberRoleOwner]
		result, err := self.removals.remove(&RemoveNetworkMemberArgs{MemberId: admin.MemberId}, self.sessions[model.NetworkMemberRoleOwner])
		connect.AssertEqual(t, err, nil)
		if result.Error != nil {
			t.Fatalf("the removal: %+v", result.Error)
		}
		connect.AssertEqual(t, result.RevokedAuthKeyCount, int64(2))
		connect.AssertEqual(t, result.BundleStillAdmits, true)
		connect.AssertEqual(t, result.Note, networkMemberRemoveBundleNote)
		connect.AssertEqual(t, result.SignatureOwed, true)
		answerJson, err := json.Marshal(result)
		connect.AssertEqual(t, err, nil)
		for _, field := range []string{`"revoked_auth_key_count":2`, `"bundle_still_admits":true`, `"note":`} {
			if !strings.Contains(string(answerJson), field) {
				t.Fatalf("the answer %s lacks %s", answerJson, field)
			}
		}

		events := model.GetNetworkAdminEvents(self.ctx, self.teamNetworkId, 0, 100)[headCount:]
		if len(events) != 3 {
			t.Fatalf("the removal recorded %d events, want two auth_key.revoke and its member.remove", len(events))
		}
		revokedIds := map[server.Id]bool{}
		for _, event := range events[:2] {
			connect.AssertEqual(t, event.Action, model.AdminEventActionAuthKeyRevoke)
			connect.AssertEqual(t, event.ActorKind, model.AdminEventActorUser)
			connect.AssertEqual(t, event.ActorId != nil && *event.ActorId == owner.UserId, true)
			revokedIds[*event.TargetId] = true
		}
		connect.AssertEqual(t, revokedIds, map[server.Id]bool{untaggedKeyId: true, preApprovedKeyId: true})
		connect.AssertEqual(t, events[2].Action, model.AdminEventActionMemberRemove)
		connect.AssertEqual(t, *events[2].TargetId, admin.MemberId)

		member := self.members[model.NetworkMemberRoleMember]
		result, err = self.removals.remove(&RemoveNetworkMemberArgs{MemberId: member.MemberId}, self.sessions[model.NetworkMemberRoleOwner])
		connect.AssertEqual(t, err, nil)
		if result.Error != nil {
			t.Fatalf("the Member's removal: %+v", result.Error)
		}
		connect.AssertEqual(t, result.RevokedAuthKeyCount, int64(1))
		connect.AssertEqual(t, result.BundleStillAdmits, false)
		connect.AssertEqual(t, result.Note, "")
		actions := self.eventActions()
		connect.AssertEqual(t, actions[len(actions)-2:], []string{model.AdminEventActionAuthKeyRevoke, model.AdminEventActionMemberRemove})
		server.Db(self.ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(
				self.ctx,
				`SELECT COUNT(*) FROM network_auth_key WHERE auth_key_id = ANY($1)`,
				[]server.Id{untaggedKeyId, preApprovedKeyId, memberKeyId},
			).Scan(&count))
			connect.AssertEqual(t, count, 0)
		})
	})
}
