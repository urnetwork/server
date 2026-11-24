//go:build sidecar_pending

package controller

// POST /network/member-remove counts the API keys the denial deleted beside the auth keys it revoked (owner decision
// 717; §7.10's api row, QD-1211, QD-1214): the model ends both kinds of key the member's user made in the team, and
// the answer says how many of each, so the remover learns what automation stopped. Only the removed member's keys are
// counted and deleted; an API key never sets bundle_still_admits, which is a pre-approved auth key's; a removal that
// ended no key answers as before, without either count, so a caller reading only the earlier fields sees the same
// answer; and a one-command admin token's replay answers the first use's counts.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The member's new API key, made with its own credential.
func (self *memberRemoveFixture) addApiKey(t testing.TB, role model.NetworkMemberRole, name string) server.Id {
	t.Helper()
	created, err := model.CreateApiKey(&model.CreateApiKeyArgs{Name: name}, self.sessions[role])
	if err != nil || created.Error != nil {
		t.Fatalf("the %s's API key: %v %+v", role, err, created)
	}
	return created.Id
}

// Whether the API key's row exists.
func (self *memberRemoveFixture) apiKeyExists(t testing.TB, apiKeyId server.Id) (exists bool) {
	t.Helper()
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT EXISTS (SELECT 1 FROM account_api_key WHERE api_key_id = $1)`, apiKeyId).Scan(&exists))
	})
	return
}

// Holds that the answer's JSON carries each of fields and none of absentFields.
func requireMemberRemoveAnswerJson(t testing.TB, result *RemoveNetworkMemberResult, fields []string, absentFields []string) {
	t.Helper()
	answerJson, err := json.Marshal(result)
	connect.AssertEqual(t, err, nil)
	for _, field := range fields {
		if !strings.Contains(string(answerJson), field) {
			t.Fatalf("the answer %s lacks %s", answerJson, field)
		}
	}
	for _, field := range absentFields {
		if strings.Contains(string(answerJson), field) {
			t.Fatalf("the answer %s carries %s", answerJson, field)
		}
	}
}

// An Owner removes an Admin that made two auth keys and one API key: the answer counts two auth keys revoked and one
// API key deleted, with no bundle note, since neither auth key was pre-approved and an API key has no bundle. The
// team's events end in an auth_key.revoke for each auth key, an api_key.delete for the API key, each by the Owner,
// then the member.remove. The Admin's API key is gone, and the API key a Member made stays.
func TestMemberRemoveAnswersAuthKeyAndApiKeyCounts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newMemberRemoveFixture(t, "remove-api-keys")
		firstAuthKeyId := self.addAuthKey(t, model.NetworkMemberRoleAdmin, &model.CreateAuthKeyArgs{})
		secondAuthKeyId := self.addAuthKey(t, model.NetworkMemberRoleAdmin, &model.CreateAuthKeyArgs{})
		apiKeyId := self.addApiKey(t, model.NetworkMemberRoleAdmin, "remove-api-keys-admin-ci")
		otherApiKeyId := self.addApiKey(t, model.NetworkMemberRoleMember, "remove-api-keys-member-ci")
		headCount := len(self.eventActions())

		admin := self.members[model.NetworkMemberRoleAdmin]
		owner := self.members[model.NetworkMemberRoleOwner]
		result, err := self.removals.remove(&RemoveNetworkMemberArgs{MemberId: admin.MemberId}, self.sessions[model.NetworkMemberRoleOwner])
		connect.AssertEqual(t, err, nil)
		if result.Error != nil {
			t.Fatalf("the removal: %+v", result.Error)
		}
		connect.AssertEqual(t, *result.MemberId, admin.MemberId)
		connect.AssertEqual(t, result.AdminSet, true)
		connect.AssertEqual(t, result.RevokedAuthKeyCount, int64(2))
		connect.AssertEqual(t, result.DeletedApiKeyCount, int64(1))
		connect.AssertEqual(t, result.BundleStillAdmits, false)
		connect.AssertEqual(t, result.Note, "")
		connect.AssertEqual(t, result.SignatureOwed, true)
		requireMemberRemoveAnswerJson(
			t,
			result,
			[]string{`"revoked_auth_key_count":2`, `"deleted_api_key_count":1`, `"signature_owed":true`},
			[]string{`"bundle_still_admits"`, `"note"`},
		)

		events := model.GetNetworkAdminEvents(self.ctx, self.teamNetworkId, 0, 100)[headCount:]
		actions := []string{}
		for _, event := range events {
			actions = append(actions, event.Action)
		}
		connect.AssertEqual(t, actions, []string{
			model.AdminEventActionAuthKeyRevoke,
			model.AdminEventActionAuthKeyRevoke,
			model.AdminEventActionApiKeyDelete,
			model.AdminEventActionMemberRemove,
		})
		for _, event := range events {
			connect.AssertEqual(t, event.ActorKind, model.AdminEventActorUser)
			connect.AssertEqual(t, event.ActorId != nil && *event.ActorId == owner.UserId, true)
		}
		revokedAuthKeyIds := map[server.Id]bool{*events[0].TargetId: true, *events[1].TargetId: true}
		connect.AssertEqual(t, revokedAuthKeyIds, map[server.Id]bool{firstAuthKeyId: true, secondAuthKeyId: true})
		connect.AssertEqual(t, *events[2].TargetId, apiKeyId)
		connect.AssertEqual(t, *events[3].TargetId, admin.MemberId)

		connect.AssertEqual(t, self.apiKeyExists(t, apiKeyId), false)
		connect.AssertEqual(t, self.apiKeyExists(t, otherApiKeyId), true)
	})
}

// The two counts are the denial's, each its own: a Member that made an API key and no auth key answers one API key
// deleted and no auth key count; an Auditor that made neither answers neither count, the answer's JSON as it was
// before the API key count, so a caller that reads only the earlier fields sees no change.
func TestMemberRemoveCountsEachKindOfKeyAlone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newMemberRemoveFixture(t, "remove-api-keys-alone")
		apiKeyId := self.addApiKey(t, model.NetworkMemberRoleMember, "remove-api-keys-alone-ci")

		member := self.members[model.NetworkMemberRoleMember]
		result, err := self.removals.remove(&RemoveNetworkMemberArgs{MemberId: member.MemberId}, self.sessions[model.NetworkMemberRoleOwner])
		connect.AssertEqual(t, err, nil)
		if result.Error != nil {
			t.Fatalf("the Member's removal: %+v", result.Error)
		}
		connect.AssertEqual(t, result.RevokedAuthKeyCount, int64(0))
		connect.AssertEqual(t, result.DeletedApiKeyCount, int64(1))
		requireMemberRemoveAnswerJson(t, result, []string{`"deleted_api_key_count":1`}, []string{`"revoked_auth_key_count"`, `"bundle_still_admits"`})
		connect.AssertEqual(t, self.apiKeyExists(t, apiKeyId), false)

		auditor := self.members[model.NetworkMemberRoleAuditor]
		result, err = self.removals.remove(&RemoveNetworkMemberArgs{MemberId: auditor.MemberId}, self.sessions[model.NetworkMemberRoleOwner])
		connect.AssertEqual(t, err, nil)
		if result.Error != nil {
			t.Fatalf("the Auditor's removal: %+v", result.Error)
		}
		connect.AssertEqual(t, result.RevokedAuthKeyCount, int64(0))
		connect.AssertEqual(t, result.DeletedApiKeyCount, int64(0))
		requireMemberRemoveAnswerJson(
			t,
			result,
			[]string{`"signature_owed":true`},
			[]string{`"revoked_auth_key_count"`, `"deleted_api_key_count"`, `"bundle_still_admits"`},
		)
	})
}

// A one-command admin token's removal stores its answer, the API key count with it, and a retry of the token's jti
// answers that first answer, both counts included, and runs nothing.
func TestMemberRemoveReplayAnswersTheApiKeyCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newMemberRemoveFixture(t, "remove-api-keys-replay")
		target := self.members[model.NetworkMemberRoleMember]
		self.addAuthKey(t, model.NetworkMemberRoleMember, &model.CreateAuthKeyArgs{})
		apiKeyId := self.addApiKey(t, model.NetworkMemberRoleMember, "remove-api-keys-replay-ci")

		admin := self.members[model.NetworkMemberRoleAdmin]
		adminMemberId := admin.MemberId
		jti := server.NewId().String()
		tokenSession := func() *session.ClientSession {
			clientSession := session.Testing_CreateClientSession(self.ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: admin.UserId, MemberId: &adminMemberId})
			clientSession.UnsignedIdentity = true
			clientSession.AdminCommand = &session.AdminCommandUse{
				NetworkId:  self.teamNetworkId,
				MemberId:   adminMemberId,
				Jti:        jti,
				Cmd:        "member-remove-api-keys-test-cmd",
				Scopes:     []string{"all"},
				ExpireTime: self.now.Add(time.Hour),
			}
			return clientSession
		}
		args := &RemoveNetworkMemberArgs{MemberId: target.MemberId}
		first, err := self.removals.remove(args, tokenSession())
		connect.AssertEqual(t, err, nil)
		if first.Error != nil {
			t.Fatalf("the removal: %+v", first.Error)
		}
		connect.AssertEqual(t, first.RevokedAuthKeyCount, int64(1))
		connect.AssertEqual(t, first.DeletedApiKeyCount, int64(1))
		connect.AssertEqual(t, self.apiKeyExists(t, apiKeyId), false)
		eventCount := len(self.eventActions())

		replay, err := self.removals.remove(args, tokenSession())
		connect.AssertEqual(t, err, nil)
		if replay.Error != nil {
			t.Fatalf("the replay: %+v", replay.Error)
		}
		connect.AssertEqual(t, *replay.MemberId, target.MemberId)
		connect.AssertEqual(t, replay.RevokedAuthKeyCount, int64(1))
		connect.AssertEqual(t, replay.DeletedApiKeyCount, int64(1))
		connect.AssertEqual(t, replay.SignatureOwed, true)
		connect.AssertEqual(t, len(self.eventActions()), eventCount)
	})
}
