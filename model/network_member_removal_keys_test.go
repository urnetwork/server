//go:build sidecar_pending

package model

// A team member's removal ends the auth keys its user made in the team (QD-1211; §7.10's api row, §7.5's "a
// re-invite never revives an old token", I-15, D-TEAM-31), on Postgres with MG0, MG1 and MG6. An auth key names its
// creator by user, never by membership, so before the fix a key the removal left enrolled again once the same person
// re-joined under a new member_id. Here each of the removed member's keys, untagged, tagged and pre-approved, answers
// 401 auth_key_invalid after a re-join, spending, minting and posting nothing, while a key made after the re-join
// enrolls; the removal owes each of the team's Owners and Admins one auth key notice naming each key once (QD-1215)
// and an auth_key.revoke admin event for each, with the removal's actor and request, before the removal's own event;
// a MemberRevoke that lands with no denial before it ends the keys the same way; and a creation racing the removal
// leaves no key behind, whether the removal commits between the creation's role read and its transaction or the
// creation commits under the removal's snapshot.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The admin event of a removal the system made, with a request id of its own, as a denial's Event.
func newTestMemberRemovalEvent(networkId server.Id, memberId server.Id) *AdminEvent {
	requestId := server.NewId()
	return &AdminEvent{
		NetworkId:  networkId,
		ActorKind:  AdminEventActorSystem,
		Action:     AdminEventActionMemberRemove,
		TargetKind: AdminEventTargetMember,
		TargetId:   &memberId,
		Result:     AdminEventResultOk,
		RequestId:  &requestId,
	}
}

// Whether the key's row exists.
func testAuthKeyExists(t testing.TB, ctx context.Context, authKeyId server.Id) (exists bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM network_auth_key WHERE auth_key_id = $1)`, authKeyId).Scan(&exists))
	})
	return
}

// Denies the member with event as the removal's own, in a transaction of its own.
func denyTestMember(t testing.TB, ctx context.Context, networkId server.Id, memberId server.Id, event *AdminEvent) *NetworkMemberDenied {
	t.Helper()
	var denied *NetworkMemberDenied
	server.Tx(ctx, func(tx server.PgTx) {
		var err error
		denied, err = DenyNetworkMemberInTx(ctx, tx, &NetworkMemberDenial{NetworkId: networkId, MemberId: memberId, Time: server.NowUtc(), Event: event})
		server.Raise(err)
	})
	return denied
}

// The user joins the team with role, as an invite's acceptance makes a membership, a re-invite's included: its own
// member_id, and a session of its sign-in there.
func joinTestTeamMember(t testing.TB, ctx context.Context, networkId server.Id, userId server.Id, role NetworkMemberRole) (*NetworkMember, *session.ClientSession) {
	t.Helper()
	member := addTestSignInMember(t, ctx, networkId, userId, role, false)
	signIn := ResolveNetworkSignIn(ctx, userId, networkId, server.NowUtc())
	if signIn == nil || signIn.MemberId == nil || *signIn.MemberId != member.MemberId {
		t.Fatalf("the joined member's sign-in: %+v", signIn)
	}
	memberSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", NewNetworkSignInByJwt(signIn, false))
	t.Cleanup(memberSession.Cancel)
	return member, memberSession
}

// The network's admin events after afterSeq.
func testAdminEventsAfter(ctx context.Context, networkId server.Id, afterSeq int64) []*NetworkAdminEvent {
	return GetNetworkAdminEvents(ctx, networkId, afterSeq, 1000)
}

// The seq of the network's newest admin event, 0 for none.
func testAdminEventHead(ctx context.Context, networkId server.Id) int64 {
	events := GetNetworkAdminEvents(ctx, networkId, 0, 1000)
	if len(events) == 0 {
		return 0
	}
	return events[len(events)-1].Seq
}

// The ids of keys, in auth_key_id order.
func testAuthKeyIds(authKeys []*NetworkAuthKey) []server.Id {
	authKeyIds := []server.Id{}
	for _, authKey := range authKeys {
		authKeyIds = append(authKeyIds, authKey.AuthKeyId)
	}
	return authKeyIds
}

// Holds that events, a removal's run of the chain, are one auth_key.revoke event for each of authKeyIds, in order,
// each with removal's actor and request, then removal's own event.
func requireTestRemovalEvents(t testing.TB, events []*NetworkAdminEvent, authKeyIds []server.Id, removal *AdminEvent) {
	t.Helper()
	if len(events) != len(authKeyIds)+1 {
		actions := []string{}
		for _, event := range events {
			actions = append(actions, event.Action)
		}
		t.Fatalf("the removal recorded %v, want %d auth_key.revoke events and its own", actions, len(authKeyIds))
	}
	sameId := func(a *server.Id, b *server.Id) bool {
		return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
	}
	for i, authKeyId := range authKeyIds {
		event := events[i]
		if event.Action != AdminEventActionAuthKeyRevoke || event.TargetKind != AdminEventTargetAuthKey || !sameId(event.TargetId, &authKeyId) {
			t.Fatalf("event %d is %s of %s %v, want auth_key.revoke of %s", i, event.Action, event.TargetKind, event.TargetId, authKeyId)
		}
		if event.ActorKind != removal.ActorKind || !sameId(event.ActorId, removal.ActorId) || !sameId(event.RequestId, removal.RequestId) {
			t.Fatalf("key %s's revoke names actor %s %v and request %v, not the removal's %s %v and %v",
				authKeyId, event.ActorKind, event.ActorId, event.RequestId, removal.ActorKind, removal.ActorId, removal.RequestId)
		}
		connect.AssertEqual(t, event.Result, AdminEventResultOk)
	}
	last := events[len(events)-1]
	if last.Action != removal.Action || last.TargetKind != removal.TargetKind || !sameId(last.TargetId, removal.TargetId) {
		t.Fatalf("the removal's last event is %s of %s %v, want its own %s", last.Action, last.TargetKind, last.TargetId, removal.Action)
	}
}

// An Admin's untagged, tagged and pre-approved keys, each enrolled once, end with its removal: the denial deletes
// them and answers them, a pre-approved one among them, so its bundle still admits until its Delegation ends. Once the
// same person joins the team again, under a membership of its own, each old key answers 401 auth_key_invalid, with an
// admission or without, and spends, mints and posts nothing; the tagged client the old key minted stays, the
// network's; and keys the re-joined member makes enroll, its own device on its new membership.
func TestARemovedMembersKeysDoNotEnrollOnceItRejoins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newCreatorRoleTeam(t)
		untaggedBearer, untaggedKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{})
		taggedBearer, taggedKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Tags: []string{teamEnrollTagHex}})
		preApprovedBearer, preApprovedKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{PreApproved: true})

		// live before the removal
		untaggedClient, err := self.enroll(t, untaggedBearer, nil)
		if err != nil {
			t.Fatalf("the untagged enrollment: %v", err)
		}
		taggedClient, err := self.enroll(t, taggedBearer, nil)
		if err != nil {
			t.Fatalf("the tagged enrollment: %v", err)
		}
		preApproved, err := self.enroll(t, preApprovedBearer, newCreatorRoleAdmission(t, self.networkId))
		if err != nil {
			t.Fatalf("the pre-approved enrollment: %v", err)
		}
		connect.AssertEqual(t, preApproved.ApprovalState, NetworkEnrollApprovalStateApproved)
		connect.AssertEqual(t, self.appendedCount, 1)

		denied := denyTestMember(t, self.ctx, self.networkId, self.creator.MemberId, newTestMemberRemovalEvent(self.networkId, self.creator.MemberId))
		wantIds := []server.Id{untaggedKeyId, taggedKeyId, preApprovedKeyId}
		slices.SortFunc(wantIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		connect.AssertEqual(t, testAuthKeyIds(denied.RevokedAuthKeys), wantIds)
		connect.AssertEqual(t, denied.BundleStillAdmits(), true)
		// the untagged key's device and the untagged pre-approved key's, both the member's own
		connect.AssertEqual(t, denied.DeactivatedClientCount, int64(2))
		connect.AssertEqual(t, readTeamEnrollClientActive(t, self.ctx, preApproved.ClientId), false)
		for _, authKey := range denied.RevokedAuthKeys {
			connect.AssertEqual(t, authKey.CreatorUserId, self.creatorUserId)
			connect.AssertEqual(t, authKey.UseCount, 1)
			connect.AssertEqual(t, authKey.PreApproved, authKey.AuthKeyId == preApprovedKeyId)
			connect.AssertEqual(t, 0 < len(authKey.TagIds), authKey.AuthKeyId == taggedKeyId)
		}
		for _, authKeyId := range wantIds {
			connect.AssertEqual(t, testAuthKeyExists(t, self.ctx, authKeyId), false)
		}
		connect.AssertEqual(t, readTeamEnrollClientActive(t, self.ctx, untaggedClient.ClientId), false)
		// a tagged client is the network's and outlives its sponsor (owner decision 83)
		connect.AssertEqual(t, readTeamEnrollClientActive(t, self.ctx, taggedClient.ClientId), true)

		rejoined, rejoinedSession := joinTestTeamMember(t, self.ctx, self.networkId, self.creatorUserId, NetworkMemberRoleAdmin)
		if rejoined.MemberId == self.creator.MemberId {
			t.Fatal("the re-join kept the removed member_id")
		}
		clientCount := countTeamEnrollClients(t, self.ctx, self.networkId)
		for _, c := range []struct {
			name      string
			bearer    string
			admission []byte
		}{
			{name: "untagged", bearer: untaggedBearer},
			{name: "tagged", bearer: taggedBearer},
			{name: "pre-approved with its admission", bearer: preApprovedBearer, admission: newCreatorRoleAdmission(t, self.networkId)},
			{name: "pre-approved", bearer: preApprovedBearer},
		} {
			_, err := self.enroll(t, c.bearer, c.admission)
			requireTeamEnrollRefusal(t, err, http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid)
		}
		connect.AssertEqual(t, countTeamEnrollClients(t, self.ctx, self.networkId), clientCount)
		connect.AssertEqual(t, self.appendedCount, 1)
		connect.AssertEqual(t, countTestAuthKeysOf(t, self.ctx, self.networkId, self.creatorUserId), 0)

		// the re-joined member's own keys enroll
		newBearer, newKeyId := newTeamEnrollAuthKey(t, rejoinedSession, &CreateAuthKeyArgs{})
		own, err := self.enroll(t, newBearer, nil)
		if err != nil {
			t.Fatalf("the re-joined member's untagged enrollment: %v", err)
		}
		ownJwt := requireTeamEnrollCredential(t, self.ctx, own)
		if ownJwt.MemberId == nil || *ownJwt.MemberId != rejoined.MemberId {
			t.Fatalf("the re-joined member's device names member %v, not its membership %s", ownJwt.MemberId, rejoined.MemberId)
		}
		row := readTeamEnrollOwnerRow(t, self.ctx, own.ClientId)
		connect.AssertEqual(t, row.memberId != nil && *row.memberId == rejoined.MemberId, true)
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, newKeyId), 1)
		newTaggedBearer, newTaggedKeyId := newTeamEnrollAuthKey(t, rejoinedSession, &CreateAuthKeyArgs{Tags: []string{teamEnrollTagHex}})
		if _, err := self.enroll(t, newTaggedBearer, nil); err != nil {
			t.Fatalf("the re-joined Admin's tagged enrollment: %v", err)
		}
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, newTaggedKeyId), 1)
		newPreApprovedBearer, newPreApprovedKeyId := newTeamEnrollAuthKey(t, rejoinedSession, &CreateAuthKeyArgs{PreApproved: true})
		if _, err := self.enroll(t, newPreApprovedBearer, newCreatorRoleAdmission(t, self.networkId)); err != nil {
			t.Fatalf("the re-joined Admin's pre-approved enrollment: %v", err)
		}
		connect.AssertEqual(t, self.appendedCount, 2)
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, newPreApprovedKeyId), 1)
	})
}

// The removal owes the auth key notice once to each of the team's Owners and Admins (QD-1215), the removed Admin
// among them, as it still is when the removal reads them, each listing one revoked change for each key it ended and
// nothing of the keys themselves, and an auth_key.revoke admin event for each key in auth_key_id order, naming the
// removal's actor and request, before the removal's own event. Another member's key in the team and the removed
// person's key in its own network stay.
func TestAMemberRemovalOwesOneNoticeAndAnEventPerKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newCreatorRoleTeam(t)
		// the Owner's own account, whose sign-in the notice reaches
		owner := newKeyNoticeAccount(t, self.ctx, self.owner.UserId, newKeyNoticeEmail("owner"))
		untagged := createTestAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 3}, server.NowUtc())
		tagged := createTestAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 4, Tags: []string{teamEnrollTagHex}}, server.NowUtc())
		preApproved := createTestAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 5, PreApproved: true}, server.NowUtc())

		// another member of the team, and the removed person's own network
		bystanderUserId := server.NewId()
		bystanderEmail := Testing_CreateNetwork(self.ctx, server.NewId(), "bystander-"+bystanderUserId.String(), bystanderUserId)
		_, bystanderSession := joinTestTeamMember(t, self.ctx, self.networkId, bystanderUserId, NetworkMemberRoleAdmin)
		bystanderKey := createTestAuthKey(t, bystanderSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, server.NowUtc())
		var personalNetworkId server.Id
		var creatorEmail string
		server.Db(self.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(self.ctx, `SELECT network_id FROM network WHERE admin_user_id = $1`, self.creatorUserId).Scan(&personalNetworkId))
			server.Raise(conn.QueryRow(self.ctx, `SELECT user_auth FROM network_user WHERE user_id = $1`, self.creatorUserId).Scan(&creatorEmail))
		})
		personalSession := session.NewLocalClientSession(self.ctx, "192.0.2.1:1", session.NewByJwt(personalNetworkId, self.creatorUserId, "creator-"+self.creatorUserId.String(), false, false))
		defer personalSession.Cancel()
		personalKey := createTestAuthKey(t, personalSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, server.NowUtc())

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
		denied := denyTestMember(t, self.ctx, self.networkId, memberId, removal)
		revokedIds := testAuthKeyIds(denied.RevokedAuthKeys)
		wantIds := []server.Id{*untagged.AuthKeyId, *tagged.AuthKeyId, *preApproved.AuthKeyId}
		slices.SortFunc(wantIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		connect.AssertEqual(t, revokedIds, wantIds)

		requireTestRemovalEvents(t, testAdminEventsAfter(self.ctx, self.networkId, headSeq), revokedIds, removal)

		notices := unsentTestNotices(t, self.ctx, self.networkId, testAuthKeyNoticeTemplate)
		recipients := []string{}
		for _, notice := range notices {
			recipients = append(recipients, notice.userAuth)
		}
		slices.Sort(recipients)
		wantRecipients := append(slices.Clone(owner.emails), strings.ToLower(creatorEmail), strings.ToLower(bystanderEmail))
		slices.Sort(wantRecipients)
		if !slices.Equal(recipients, wantRecipients) {
			t.Fatalf("the removal's auth key notices went to %v, want one each to the Owner and the two Admins %v", recipients, wantRecipients)
		}
		for _, notice := range notices {
			changes := notice.changes(t)
			if len(changes) != 3 {
				t.Fatalf("the removal's notice lists %d changes, want one per key: %s", len(changes), notice.templateJson)
			}
			byMaxUses := map[string]map[string]any{}
			for _, change := range changes {
				connect.AssertEqual(t, fmt.Sprint(change["action"]), SecurityAuthKeyRevoked)
				byMaxUses[fmt.Sprint(change["max_uses"])] = change
			}
			for maxUses, want := range map[string]map[string]string{
				"3": {"tag_count": "<nil>", "pre_approved": "<nil>"},
				"4": {"tag_count": "1", "pre_approved": "<nil>"},
				"5": {"tag_count": "<nil>", "pre_approved": "true"},
			} {
				change, ok := byMaxUses[maxUses]
				if !ok {
					t.Fatalf("the notice names no key of %s uses: %s", maxUses, notice.templateJson)
				}
				for key, value := range want {
					if got := fmt.Sprint(change[key]); got != value {
						t.Fatalf("the key of %s uses has %s %s, want %s: %s", maxUses, key, got, value, notice.templateJson)
					}
				}
			}
			for _, created := range []*CreateAuthKeyResult{untagged, tagged, preApproved} {
				idText, secretText, _ := strings.Cut(strings.TrimPrefix(created.AuthKey, "ur-authkey-"), "-")
				for _, secret := range []string{created.AuthKey, idText, secretText, created.AuthKeyId.String(), teamEnrollTagHex} {
					if strings.Contains(notice.templateJson, secret) {
						t.Fatalf("the notice holds %q: %s", secret, notice.templateJson)
					}
				}
			}
		}

		connect.AssertEqual(t, testAuthKeyExists(t, self.ctx, *bystanderKey.AuthKeyId), true)
		connect.AssertEqual(t, testAuthKeyExists(t, self.ctx, *personalKey.AuthKeyId), true)

		// a removal of a member that made no key owes no notice and no key event
		quietUserId := server.NewId()
		Testing_CreateNetwork(self.ctx, server.NewId(), "quiet-"+quietUserId.String(), quietUserId)
		quiet, _ := joinTestTeamMember(t, self.ctx, self.networkId, quietUserId, NetworkMemberRoleMember)
		settleTestNotices(self.ctx, self.networkId)
		headSeq = testAdminEventHead(self.ctx, self.networkId)
		quietRemoval := newTestMemberRemovalEvent(self.networkId, quiet.MemberId)
		quietDenied := denyTestMember(t, self.ctx, self.networkId, quiet.MemberId, quietRemoval)
		connect.AssertEqual(t, len(quietDenied.RevokedAuthKeys), 0)
		connect.AssertEqual(t, quietDenied.BundleStillAdmits(), false)
		requireTestRemovalEvents(t, testAdminEventsAfter(self.ctx, self.networkId, headSeq), nil, quietRemoval)
		connect.AssertEqual(t, len(unsentTestNotices(t, self.ctx, self.networkId, testAuthKeyNoticeTemplate)), 0)
	})
}

// A denial without the removal's admin event, or with another network's, is refused before it ends anything.
func TestADenialWithoutItsAdminEventIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTeamEnrollFixture(t, NetworkMemberRoleAdmin)
		_, authKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{})
		for _, event := range []*AdminEvent{nil, newTestMemberRemovalEvent(server.NewId(), self.creator.MemberId)} {
			var err error
			server.Tx(self.ctx, func(tx server.PgTx) {
				_, err = DenyNetworkMemberInTx(self.ctx, tx, &NetworkMemberDenial{
					NetworkId: self.networkId,
					MemberId:  self.creator.MemberId,
					Time:      server.NowUtc(),
					Event:     event,
				})
			})
			if err == nil {
				t.Fatalf("a denial with event %+v went through", event)
			}
		}
		connect.AssertEqual(t, GetNetworkMember(self.ctx, self.creator.MemberId) != nil, true)
		connect.AssertEqual(t, testAuthKeyExists(t, self.ctx, authKeyId), true)
	})
}

// A MemberRevoke an admin device signed with no denial before it ends the member's keys when its effects run, as a
// denial does: the rows go, each owes an auth_key.revoke event naming the entry's signer and request before the
// entry's own event, the notice of them reaches the team's Owner, the member being a Member by then (QD-1215), and a
// re-joined person's old keys answer 401 auth_key_invalid.
func TestAMemberRevokeWithNoDenialEndsTheMembersKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newCreatorRoleTeam(t)
		// the Owner's own account, whose sign-in the notice reaches
		owner := newKeyNoticeAccount(t, self.ctx, self.owner.UserId, newKeyNoticeEmail("owner"))
		untaggedBearer, untaggedKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{})
		taggedBearer, taggedKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Tags: []string{teamEnrollTagHex}})
		// out of the AdminSet, so an admin's MemberRevoke removes it alone
		self.setCreatorAdmin(t, false)

		settleTestNotices(self.ctx, self.networkId)
		headSeq := testAdminEventHead(self.ctx, self.networkId)
		self.land(t, bridge.SignedType_SIGNED_MEMBER_REVOKE, &bridge.MemberRevoke{
			Header:   self.world.Header(),
			MemberId: sidecar.Id(self.creator.MemberId).Bytes(),
		})
		connect.AssertEqual(t, GetNetworkMember(self.ctx, self.creator.MemberId) == nil, true)
		connect.AssertEqual(t, testAuthKeyExists(t, self.ctx, untaggedKeyId), false)
		connect.AssertEqual(t, testAuthKeyExists(t, self.ctx, taggedKeyId), false)

		events := testAdminEventsAfter(self.ctx, self.networkId, headSeq)
		if len(events) == 0 {
			t.Fatal("the MemberRevoke's effects recorded no event")
		}
		entry := events[len(events)-1]
		connect.AssertEqual(t, entry.Action, AdminEventActionConfigPrefix+configLogSignedTypeWord(bridge.SignedType_SIGNED_MEMBER_REVOKE))
		wantIds := []server.Id{untaggedKeyId, taggedKeyId}
		slices.SortFunc(wantIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		requireTestRemovalEvents(t, events, wantIds, &entry.AdminEvent)
		notices := unsentTestNotices(t, self.ctx, self.networkId, testAuthKeyNoticeTemplate)
		if len(notices) != 1 || !slices.Contains(owner.emails, notices[0].userAuth) || len(notices[0].changes(t)) != 2 {
			recipients := []string{}
			for _, notice := range notices {
				recipients = append(recipients, notice.userAuth)
			}
			t.Fatalf("the MemberRevoke's auth key notices went to %v, want one, to the Owner's %v, listing both keys", recipients, owner.emails)
		}

		_, _ = joinTestTeamMember(t, self.ctx, self.networkId, self.creatorUserId, NetworkMemberRoleAdmin)
		for _, bearer := range []string{untaggedBearer, taggedBearer} {
			_, err := self.enroll(t, bearer, nil)
			requireTeamEnrollRefusal(t, err, http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid)
		}
		connect.AssertEqual(t, countTeamEnrollClients(t, self.ctx, self.networkId), 0)
	})
}

// A removal that commits after a creation read the creator's reach and before the key's transaction refuses the
// creation, which then reads the membership under its lock and finds none: 403, and no key outlives the removal.
func TestARemovalBeforeACreationsTransactionRefusesIt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTeamEnrollFixture(t, NetworkMemberRoleAdmin)
		removed := false
		t.Cleanup(func() { Testing_CreateAuthKeyBeforeTx = nil })
		Testing_CreateAuthKeyBeforeTx = func() {
			if !removed {
				removed = true
				denyTeamEnrollMember(t, self.ctx, self.networkId, self.creator.MemberId)
			}
		}
		created, err := CreateAuthKey(&CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, self.creatorSession)
		connect.AssertEqual(t, removed, true)
		if err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("%d ", http.StatusForbidden)) || created != nil {
			t.Fatalf("a removed member's racing creation: %v %+v", err, created)
		}
		connect.AssertEqual(t, countTestAuthKeysOf(t, self.ctx, self.networkId, self.creatorUserId), 0)
	})
}

// A creation that commits under the removal's snapshot, after the denial's transaction began and before it reads
// the keys, is not in what the denial's delete sees; its auth_key.create event in the network's chain then fails the
// denial's own events as a serialization failure, and the denial's second run, under a new snapshot, deletes the key
// and names it once.
func TestACreationUnderTheRemovalsSnapshotEndsWithIt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTeamEnrollFixture(t, NetworkMemberRoleAdmin)
		headSeq := testAdminEventHead(self.ctx, self.networkId)
		removal := newTestMemberRemovalEvent(self.networkId, self.creator.MemberId)
		runCount := 0
		var authKeyId server.Id
		var denied *NetworkMemberDenied
		server.Tx(self.ctx, func(tx server.PgTx) {
			runCount += 1
			// the transaction's snapshot, taken before the creation commits
			server.RaisePgResult(tx.Exec(self.ctx, `SELECT 1`))
			if runCount == 1 {
				_, authKeyId = newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{})
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
		connect.AssertEqual(t, testAuthKeyIds(denied.RevokedAuthKeys), []server.Id{authKeyId})
		connect.AssertEqual(t, testAuthKeyExists(t, self.ctx, authKeyId), false)
		events := testAdminEventsAfter(self.ctx, self.networkId, headSeq)
		if len(events) == 0 || events[0].Action != AdminEventActionAuthKeyCreate {
			t.Fatalf("the creation's event does not come first: %+v", events)
		}
		requireTestRemovalEvents(t, events[1:], []server.Id{authKeyId}, removal)
	})
}
