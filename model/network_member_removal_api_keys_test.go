//go:build sidecar_pending

package model

// A team member's removal ends the API keys its user made in the team (QD-1214; §7.10's api row, §7.5's "a
// re-invite never revives an old token", §7.8's API key row, I-15), on Postgres with MG0, MG1 and MG6. An API key
// names its creator by user, never by membership, and in a team apikey.GetNetworkByApiKey resolves its caller through
// network_member by that user, so before the fix every key the removal left acted again, with the new role's reach,
// once the same person re-joined under a new member_id. Here each of the removed member's keys authenticates nothing
// after a re-join and its auth codes are gone, while a key made after the re-join acts as the new membership; the
// removal owes each of the team's Owners and Admins one API key notice naming each key once (QD-1215) and an
// api_key.delete admin event for each, with the removal's actor and request, after the auth keys' events and before
// the removal's own; a MemberRevoke that lands with no denial before it ends the keys the same way; another member's
// key and the removed person's personal network are unchanged; and a creation racing the removal leaves no key
// behind, whether the removal commits before the creation's transaction or the creation commits under the removal's
// snapshot.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/apikey"
	"github.com/urnetwork/server/session"
)

// Whether the API key's row exists.
func testApiKeyExists(t testing.TB, ctx context.Context, apiKeyId server.Id) (exists bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM account_api_key WHERE api_key_id = $1)`, apiKeyId).Scan(&exists))
	})
	return
}

// How many API keys of the network name the user as their creator.
func countTestApiKeysOf(t testing.TB, ctx context.Context, networkId server.Id, creatorUserId server.Id) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COUNT(*) FROM account_api_key WHERE network_id = $1 AND creator_user_id = $2`,
			networkId,
			creatorUserId,
		).Scan(&count))
	})
	return
}

// An auth code that descends from the API key, with a role, as a code the key's session made is stored (§7.4).
func insertTestApiKeyAuthCode(t testing.TB, ctx context.Context, networkId server.Id, userId server.Id, apiKeyId server.Id) (authCodeId server.Id) {
	t.Helper()
	authCodeId = server.NewId()
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO auth_code (auth_code_id, network_id, user_id, auth_code, create_time, end_time, uses, remaining_uses, api_key_id)
				VALUES ($1, $2, $3, $4, $5, $6, 1, 1, $7)
			`,
			authCodeId,
			networkId,
			userId,
			"synthetic-code-"+authCodeId.String(),
			now,
			now.Add(time.Hour),
			apiKeyId,
		))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO auth_code_role (auth_code_id, role) VALUES ($1, $2)`, authCodeId, "synthetic"))
	})
	return
}

// How many auth_code and auth_code_role rows the code has.
func countTestAuthCodeRows(t testing.TB, ctx context.Context, authCodeId server.Id) (codeCount int, roleCount int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM auth_code WHERE auth_code_id = $1`, authCodeId).Scan(&codeCount))
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM auth_code_role WHERE auth_code_id = $1`, authCodeId).Scan(&roleCount))
	})
	return
}

// The ids of keys, in their order.
func testApiKeyIds(apiKeys []*PublicAccountApiKey) []server.Id {
	apiKeyIds := []server.Id{}
	for _, apiKey := range apiKeys {
		apiKeyIds = append(apiKeyIds, apiKey.Id)
	}
	return apiKeyIds
}

// The ids in id order.
func sortedTestIds(ids ...server.Id) []server.Id {
	sorted := slices.Clone(ids)
	slices.SortFunc(sorted, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return sorted
}

// Holds that the API key authenticates as the membership: its user and member_id in the team.
func requireTestApiKeyActsAs(t testing.TB, ctx context.Context, bearer string, member *NetworkMember) {
	t.Helper()
	byKey := apikey.GetNetworkByApiKey(bearer, ctx)
	if byKey == nil || byKey.NetworkId != member.NetworkId || byKey.UserId != member.UserId || byKey.MemberId == nil || *byKey.MemberId != member.MemberId {
		t.Fatalf("the key acts as %+v, want member %s of user %s", byKey, member.MemberId, member.UserId)
	}
}

// Holds that events, a removal's run of the chain, are one auth_key.revoke event for each of authKeyIds, then one
// api_key.delete event for each of apiKeyIds, each in order and with removal's actor and request, then removal's own
// event.
func requireTestKeyRemovalEvents(t testing.TB, events []*NetworkAdminEvent, authKeyIds []server.Id, apiKeyIds []server.Id, removal *AdminEvent) {
	t.Helper()
	if len(events) != len(authKeyIds)+len(apiKeyIds)+1 {
		actions := []string{}
		for _, event := range events {
			actions = append(actions, event.Action)
		}
		t.Fatalf("the removal recorded %v, want %d auth_key.revoke events, %d api_key.delete events and its own", actions, len(authKeyIds), len(apiKeyIds))
	}
	sameId := func(a *server.Id, b *server.Id) bool {
		return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
	}
	type keyEvent struct {
		action     string
		targetKind string
		targetId   server.Id
	}
	want := []keyEvent{}
	for _, authKeyId := range authKeyIds {
		want = append(want, keyEvent{action: AdminEventActionAuthKeyRevoke, targetKind: AdminEventTargetAuthKey, targetId: authKeyId})
	}
	for _, apiKeyId := range apiKeyIds {
		want = append(want, keyEvent{action: AdminEventActionApiKeyDelete, targetKind: AdminEventTargetApiKey, targetId: apiKeyId})
	}
	for i, w := range want {
		event := events[i]
		if event.Action != w.action || event.TargetKind != w.targetKind || !sameId(event.TargetId, &w.targetId) {
			t.Fatalf("event %d is %s of %s %v, want %s of %s %s", i, event.Action, event.TargetKind, event.TargetId, w.action, w.targetKind, w.targetId)
		}
		if event.ActorKind != removal.ActorKind || !sameId(event.ActorId, removal.ActorId) || !sameId(event.RequestId, removal.RequestId) {
			t.Fatalf("%s %s names actor %s %v and request %v, not the removal's %s %v and %v",
				w.action, w.targetId, event.ActorKind, event.ActorId, event.RequestId, removal.ActorKind, removal.ActorId, removal.RequestId)
		}
		connect.AssertEqual(t, event.Result, AdminEventResultOk)
	}
	last := events[len(events)-1]
	if last.Action != removal.Action || last.TargetKind != removal.TargetKind || !sameId(last.TargetId, removal.TargetId) {
		t.Fatalf("the removal's last event is %s of %s %v, want its own %s", last.Action, last.TargetKind, last.TargetId, removal.Action)
	}
}

// An Admin's API keys, one of the all scope for a year and one for an hour, end with its removal: the denial deletes
// them, answers them as the key listing shows them, and deletes the auth code that descends from one with its role.
// Once the same person joins the team again, under a membership of its own, neither old key authenticates, and a key
// the re-joined member makes acts as its new membership.
func TestARemovedMembersApiKeysAuthenticateNothingOnceItRejoins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTeamEnrollFixture(t, NetworkMemberRoleAdmin)
		yearKey := createTestApiKey(t, self.creatorSession, "ci-deploy")
		hourSeconds := int64(3600)
		hourKey, err := CreateApiKey(&CreateApiKeyArgs{Name: "ci-hourly", DurationSeconds: &hourSeconds}, self.creatorSession)
		if err != nil || hourKey == nil || hourKey.Error != nil {
			t.Fatalf("create the hour's key: %v %+v", err, hourKey)
		}
		authCodeId := insertTestApiKeyAuthCode(t, self.ctx, self.networkId, self.creatorUserId, yearKey.Id)

		// live before the removal, as the member
		for _, created := range []*CreateApiKeyResult{yearKey, hourKey} {
			requireTestApiKeyActsAs(t, self.ctx, created.ApiKey, self.creator)
		}

		denied := denyTestMember(t, self.ctx, self.networkId, self.creator.MemberId, newTestMemberRemovalEvent(self.networkId, self.creator.MemberId))
		connect.AssertEqual(t, testApiKeyIds(denied.DeletedApiKeys), sortedTestIds(yearKey.Id, hourKey.Id))
		created := map[server.Id]*CreateApiKeyResult{yearKey.Id: yearKey, hourKey.Id: hourKey}
		for _, apiKey := range denied.DeletedApiKeys {
			want := created[apiKey.Id]
			connect.AssertEqual(t, apiKey.Name, want.Name)
			connect.AssertEqual(t, apiKey.Scopes, []string{ApiKeyScopeAll})
			if apiKey.Creator == nil || *apiKey.Creator != self.creatorUserId {
				t.Fatalf("the deleted key %s names creator %v, want %s", apiKey.Id, apiKey.Creator, self.creatorUserId)
			}
			if apiKey.ExpireTime == nil || want.ExpireTime == nil || apiKey.ExpireTime.Sub(*want.ExpireTime).Abs() > time.Millisecond {
				t.Fatalf("the deleted key %s expires %v, want %v", apiKey.Id, apiKey.ExpireTime, want.ExpireTime)
			}
			connect.AssertEqual(t, testApiKeyExists(t, self.ctx, apiKey.Id), false)
		}
		codeCount, roleCount := countTestAuthCodeRows(t, self.ctx, authCodeId)
		connect.AssertEqual(t, codeCount, 0)
		connect.AssertEqual(t, roleCount, 0)
		for _, created := range []*CreateApiKeyResult{yearKey, hourKey} {
			if byKey := apikey.GetNetworkByApiKey(created.ApiKey, self.ctx); byKey != nil {
				t.Fatalf("a removed member's key acts as %+v", byKey)
			}
		}

		rejoined, rejoinedSession := joinTestTeamMember(t, self.ctx, self.networkId, self.creatorUserId, NetworkMemberRoleAdmin)
		if rejoined.MemberId == self.creator.MemberId {
			t.Fatal("the re-join kept the removed member_id")
		}
		for _, created := range []*CreateApiKeyResult{yearKey, hourKey} {
			if byKey := apikey.GetNetworkByApiKey(created.ApiKey, self.ctx); byKey != nil {
				t.Fatalf("the removed member's key %s acts again after the re-join, as %+v", created.Id, byKey)
			}
		}
		connect.AssertEqual(t, countTestApiKeysOf(t, self.ctx, self.networkId, self.creatorUserId), 0)

		// the re-joined member's own key acts as its new membership
		newKey := createTestApiKey(t, rejoinedSession, "ci-deploy")
		requireTestApiKeyActsAs(t, self.ctx, newKey.ApiKey, rejoined)
	})
}

// The removal owes the API key notice once to each of the team's Owners and Admins (QD-1215), the removed Admin
// among them, as it still is when the removal reads them, each listing one deleted change for each key it ended at
// the removal's time, with the key's name as a notice shows it and its creation time and nothing of the key itself,
// and an api_key.delete admin event for each key in api_key_id order, naming the removal's actor and request, after
// the auth key's auth_key.revoke and before the removal's own event. Another member's key in the team and the removed
// person's key in its own network stay and still act; a removal of a member that made no API key owes no API key
// notice and no api_key.delete event.
func TestAMemberRemovalOwesOneApiKeyNoticeAndAnEventPerKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTeamEnrollFixture(t, NetworkMemberRoleAdmin)
		_, authKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{})
		deployKey := createTestApiKey(t, self.creatorSession, "deploy-bot")
		opsKey := createTestApiKey(t, self.creatorSession, " ops\nbot‮ ")
		nightlyKey := createTestApiKey(t, self.creatorSession, "nightly")
		listed := testApiKeysOf(t, self.creatorSession)

		// another member of the team, and the removed person's own network
		bystanderUserId := server.NewId()
		bystanderEmail := Testing_CreateNetwork(self.ctx, server.NewId(), "bystander-"+bystanderUserId.String(), bystanderUserId)
		bystander, bystanderSession := joinTestTeamMember(t, self.ctx, self.networkId, bystanderUserId, NetworkMemberRoleAdmin)
		bystanderKey := createTestApiKey(t, bystanderSession, "bystander-bot")
		var personalNetworkId server.Id
		var creatorEmail string
		server.Db(self.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(self.ctx, `SELECT network_id FROM network WHERE admin_user_id = $1`, self.creatorUserId).Scan(&personalNetworkId))
			server.Raise(conn.QueryRow(self.ctx, `SELECT user_auth FROM network_user WHERE user_id = $1`, self.creatorUserId).Scan(&creatorEmail))
		})
		// the team's two Admins, the removed one and the bystander; the fixture's team has no Owner member
		wantRecipients := []string{strings.ToLower(creatorEmail), strings.ToLower(bystanderEmail)}
		slices.Sort(wantRecipients)
		recipientsOf := func(notices []*testOutboxNotice) []string {
			recipients := []string{}
			for _, notice := range notices {
				recipients = append(recipients, notice.userAuth)
			}
			slices.Sort(recipients)
			return recipients
		}
		personalSession := session.NewLocalClientSession(self.ctx, "192.0.2.1:1", session.NewByJwt(personalNetworkId, self.creatorUserId, "creator-"+self.creatorUserId.String(), false, false))
		defer personalSession.Cancel()
		personalKey := createTestApiKey(t, personalSession, "personal-bot")
		settleTestNotices(self.ctx, personalNetworkId)

		settleTestNotices(self.ctx, self.networkId)
		headSeq := testAdminEventHead(self.ctx, self.networkId)
		removerUserId, requestId := server.NewId(), server.NewId()
		memberId := self.creator.MemberId
		removal := &AdminEvent{
			NetworkId:  self.networkId,
			ActorKind:  AdminEventActorUser,
			ActorId:    &removerUserId,
			Action:     AdminEventActionMemberRemove,
			TargetKind: AdminEventTargetMember,
			TargetId:   &memberId,
			Result:     AdminEventResultOk,
			RequestId:  &requestId,
		}
		// a removal time apart from now, so the notice's change time shows whose it is
		removalTime := server.NowUtc().Add(-time.Minute).Truncate(time.Millisecond)
		var denied *NetworkMemberDenied
		server.Tx(self.ctx, func(tx server.PgTx) {
			var err error
			denied, err = DenyNetworkMemberInTx(self.ctx, tx, &NetworkMemberDenial{NetworkId: self.networkId, MemberId: memberId, Time: removalTime, Event: removal})
			server.Raise(err)
		})
		deletedIds := testApiKeyIds(denied.DeletedApiKeys)
		connect.AssertEqual(t, deletedIds, sortedTestIds(deployKey.Id, opsKey.Id, nightlyKey.Id))
		connect.AssertEqual(t, testAuthKeyIds(denied.RevokedAuthKeys), []server.Id{authKeyId})

		requireTestKeyRemovalEvents(t, testAdminEventsAfter(self.ctx, self.networkId, headSeq), []server.Id{authKeyId}, deletedIds, removal)

		notices := unsentTestNotices(t, self.ctx, self.networkId, testApiKeyNoticeTemplate)
		if recipients := recipientsOf(notices); !slices.Equal(recipients, wantRecipients) {
			t.Fatalf("the removal's API key notices went to %v, want one each to the two Admins %v", recipients, wantRecipients)
		}
		for _, notice := range notices {
			changes := notice.changes(t)
			if len(changes) != 3 {
				t.Fatalf("the removal's notice lists %d changes, want one per key: %s", len(changes), notice.templateJson)
			}
			byName := map[string]map[string]any{}
			for _, change := range changes {
				connect.AssertEqual(t, fmt.Sprint(change["action"]), SecurityApiKeyDeleted)
				changeTime, err := time.Parse(time.RFC3339Nano, fmt.Sprint(change["change_time"]))
				if err != nil || !changeTime.Equal(removalTime) {
					t.Fatalf("a change names time %v (%v), want the removal's %v: %s", change["change_time"], err, removalTime, notice.templateJson)
				}
				byName[fmt.Sprint(change["name"])] = change
			}
			for name, apiKeyId := range map[string]server.Id{"deploy-bot": deployKey.Id, "ops bot": opsKey.Id, "nightly": nightlyKey.Id} {
				change, ok := byName[name]
				if !ok {
					t.Fatalf("the notice names no key %q: %s", name, notice.templateJson)
				}
				if keyCreateTime := testKeyCreateTime(t, change); !keyCreateTime.Equal(listed[apiKeyId].CreateTime) {
					t.Fatalf("the notice names %q created %v, want %v as listed", name, keyCreateTime, listed[apiKeyId].CreateTime)
				}
			}
		}
		for _, created := range []*CreateApiKeyResult{deployKey, opsKey, nightlyKey} {
			requireTestNoticesHoldNoKey(t, self.ctx, self.networkId, created)
		}
		// the auth key's own notice, apart from the API keys', to the same two
		authKeyNotices := unsentTestNotices(t, self.ctx, self.networkId, testAuthKeyNoticeTemplate)
		if recipients := recipientsOf(authKeyNotices); !slices.Equal(recipients, wantRecipients) {
			t.Fatalf("the removal's auth key notices went to %v, want one each to the two Admins %v", recipients, wantRecipients)
		}
		for _, notice := range authKeyNotices {
			if changes := notice.changes(t); len(changes) != 1 {
				t.Fatalf("the removal's auth key notice lists %d changes, want its one key: %s", len(changes), notice.templateJson)
			}
		}

		// the other member's key and the personal network's stay, and act as before
		connect.AssertEqual(t, testApiKeyExists(t, self.ctx, bystanderKey.Id), true)
		requireTestApiKeyActsAs(t, self.ctx, bystanderKey.ApiKey, bystander)
		connect.AssertEqual(t, testApiKeyExists(t, self.ctx, personalKey.Id), true)
		personal := apikey.GetNetworkByApiKey(personalKey.ApiKey, self.ctx)
		if personal == nil || personal.NetworkId != personalNetworkId || personal.UserId != self.creatorUserId || personal.MemberId != nil {
			t.Fatalf("the removed person's personal key acts as %+v", personal)
		}
		requireNoTestApiKeyNotice(t, self.ctx, personalNetworkId, "the team removal")

		// a removal of a member that made no API key owes no API key notice and no key event
		quietUserId := server.NewId()
		Testing_CreateNetwork(self.ctx, server.NewId(), "quiet-"+quietUserId.String(), quietUserId)
		quiet, _ := joinTestTeamMember(t, self.ctx, self.networkId, quietUserId, NetworkMemberRoleMember)
		settleTestNotices(self.ctx, self.networkId)
		headSeq = testAdminEventHead(self.ctx, self.networkId)
		quietRemoval := newTestMemberRemovalEvent(self.networkId, quiet.MemberId)
		quietDenied := denyTestMember(t, self.ctx, self.networkId, quiet.MemberId, quietRemoval)
		connect.AssertEqual(t, len(quietDenied.DeletedApiKeys), 0)
		requireTestKeyRemovalEvents(t, testAdminEventsAfter(self.ctx, self.networkId, headSeq), nil, nil, quietRemoval)
		requireNoTestApiKeyNotice(t, self.ctx, self.networkId, "a removal of a member with no API key")
	})
}

// A MemberRevoke an admin device signed with no denial before it ends the member's API keys when its effects run, as
// a denial does: the rows go with their auth codes, each owes an api_key.delete event naming the entry's signer and
// request before the entry's own event, one notice lists both and reaches the team's Owner, the member being a
// Member by then (QD-1215), and a re-joined person's old keys authenticate nothing.
func TestAMemberRevokeWithNoDenialEndsTheMembersApiKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newCreatorRoleTeam(t)
		// the Owner's own account, whose sign-in the notice reaches
		owner := newKeyNoticeAccount(t, self.ctx, self.owner.UserId, newKeyNoticeEmail("owner"))
		deployKey := createTestApiKey(t, self.creatorSession, "deploy-bot")
		nightlyKey := createTestApiKey(t, self.creatorSession, "nightly")
		authCodeId := insertTestApiKeyAuthCode(t, self.ctx, self.networkId, self.creatorUserId, deployKey.Id)
		// out of the AdminSet, so an admin's MemberRevoke removes it alone
		self.setCreatorAdmin(t, false)

		settleTestNotices(self.ctx, self.networkId)
		headSeq := testAdminEventHead(self.ctx, self.networkId)
		self.land(t, bridge.SignedType_SIGNED_MEMBER_REVOKE, &bridge.MemberRevoke{
			Header:   self.world.Header(),
			MemberId: sidecar.Id(self.creator.MemberId).Bytes(),
		})
		connect.AssertEqual(t, GetNetworkMember(self.ctx, self.creator.MemberId) == nil, true)
		connect.AssertEqual(t, testApiKeyExists(t, self.ctx, deployKey.Id), false)
		connect.AssertEqual(t, testApiKeyExists(t, self.ctx, nightlyKey.Id), false)
		codeCount, roleCount := countTestAuthCodeRows(t, self.ctx, authCodeId)
		connect.AssertEqual(t, codeCount, 0)
		connect.AssertEqual(t, roleCount, 0)

		events := testAdminEventsAfter(self.ctx, self.networkId, headSeq)
		if len(events) == 0 {
			t.Fatal("the MemberRevoke's effects recorded no event")
		}
		entry := events[len(events)-1]
		connect.AssertEqual(t, entry.Action, AdminEventActionConfigPrefix+configLogSignedTypeWord(bridge.SignedType_SIGNED_MEMBER_REVOKE))
		requireTestKeyRemovalEvents(t, events, nil, sortedTestIds(deployKey.Id, nightlyKey.Id), &entry.AdminEvent)
		notices := unsentTestNotices(t, self.ctx, self.networkId, testApiKeyNoticeTemplate)
		if len(notices) != 1 || !slices.Contains(owner.emails, notices[0].userAuth) || len(notices[0].changes(t)) != 2 {
			recipients := []string{}
			for _, notice := range notices {
				recipients = append(recipients, notice.userAuth)
			}
			t.Fatalf("the MemberRevoke's API key notices went to %v, want one, to the Owner's %v, listing both keys", recipients, owner.emails)
		}

		_, _ = joinTestTeamMember(t, self.ctx, self.networkId, self.creatorUserId, NetworkMemberRoleAdmin)
		for _, created := range []*CreateApiKeyResult{deployKey, nightlyKey} {
			if byKey := apikey.GetNetworkByApiKey(created.ApiKey, self.ctx); byKey != nil {
				t.Fatalf("the revoked member's key %s acts again after the re-join, as %+v", created.Id, byKey)
			}
		}
	})
}

// A removal that commits after the route's role gate read the member's role and before the key's transaction
// refuses the creation, which reads the membership under its lock and finds none: 403, no key, no api_key.create
// event and no notice, so no key outlives the removal to act again once the same person re-joins. The removed
// membership's session makes none after the re-join either, while the re-joined one's does.
func TestARemovedMemberMakesNoApiKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTeamEnrollFixture(t, NetworkMemberRoleAdmin)
		// the session the route's role gate let through, its membership removed before the creation's transaction
		denyTeamEnrollMember(t, self.ctx, self.networkId, self.creator.MemberId)
		settleTestNotices(self.ctx, self.networkId)
		headSeq := testAdminEventHead(self.ctx, self.networkId)

		requireRefused := func(what string) {
			t.Helper()
			created, err := CreateApiKey(&CreateApiKeyArgs{Name: "late-bot"}, self.creatorSession)
			if err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("%d ", http.StatusForbidden)) || created != nil {
				var createdId *server.Id
				if created != nil {
					createdId = &created.Id
				}
				t.Fatalf("%s: a removed member's creation answered %v, making key %v", what, err, createdId)
			}
		}
		requireRefused("after the removal")
		connect.AssertEqual(t, countTestApiKeysOf(t, self.ctx, self.networkId, self.creatorUserId), 0)
		connect.AssertEqual(t, len(testAdminEventsAfter(self.ctx, self.networkId, headSeq)), 0)
		requireNoTestApiKeyNotice(t, self.ctx, self.networkId, "a removed member's creation")

		rejoined, rejoinedSession := joinTestTeamMember(t, self.ctx, self.networkId, self.creatorUserId, NetworkMemberRoleAdmin)
		requireRefused("after the re-join")
		connect.AssertEqual(t, countTestApiKeysOf(t, self.ctx, self.networkId, self.creatorUserId), 0)
		newKey := createTestApiKey(t, rejoinedSession, "late-bot")
		requireTestApiKeyActsAs(t, self.ctx, newKey.ApiKey, rejoined)
	})
}

// A creation that commits under the removal's snapshot, after the denial's transaction began and before it reads
// the keys, is not in what the denial's delete sees; its api_key.create event in the network's chain then fails the
// denial's own events as a serialization failure, and the denial's second run, under a new snapshot, deletes the key
// and names it once.
func TestAnApiKeyCreationUnderTheRemovalsSnapshotEndsWithIt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTeamEnrollFixture(t, NetworkMemberRoleAdmin)
		headSeq := testAdminEventHead(self.ctx, self.networkId)
		removal := newTestMemberRemovalEvent(self.networkId, self.creator.MemberId)
		runCount := 0
		var created *CreateApiKeyResult
		var denied *NetworkMemberDenied
		server.Tx(self.ctx, func(tx server.PgTx) {
			runCount += 1
			// the transaction's snapshot, taken before the creation commits
			server.RaisePgResult(tx.Exec(self.ctx, `SELECT 1`))
			if runCount == 1 {
				created = createTestApiKey(t, self.creatorSession, "racing-bot")
			}
			var err error
			denied, err = DenyNetworkMemberInTx(self.ctx, tx, &NetworkMemberDenial{
				NetworkId: self.networkId,
				MemberId:  self.creator.MemberId,
				Time:      server.NowUtc(),
				Event:     removal,
			})
			server.Raise(err)
		})
		connect.AssertEqual(t, runCount, 2)
		connect.AssertEqual(t, testApiKeyIds(denied.DeletedApiKeys), []server.Id{created.Id})
		connect.AssertEqual(t, testApiKeyExists(t, self.ctx, created.Id), false)
		events := testAdminEventsAfter(self.ctx, self.networkId, headSeq)
		if len(events) == 0 || events[0].Action != AdminEventActionApiKeyCreate {
			t.Fatalf("the creation's event does not come first: %+v", events)
		}
		requireTestKeyRemovalEvents(t, events[1:], nil, []server.Id{created.Id}, removal)
	})
}
