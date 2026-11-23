//go:build sidecar_pending

package model

// A team's auth key and API key security notices reach its Owners and Admins (QD-1215; §7.8's notices row,
// R-TEAM-12, D-TEAM-2), on Postgres with MG0, MG1 and MG6. A team's admin_user_id is a synthetic owner of record
// that team-create writes with no sign-in, and both notices used to go to that user's sign-in email addresses, so
// every key notice of a team was owed to nobody, counted as no_email: no one heard of a key created, revoked,
// deleted or ended by a removal. Each team here is made by team-create, so its owner of record is as a real team's
// is, and each person has sign-in email addresses of its own. A personal network's notices still reach its owner
// alone, and a team none of whose Owners and Admins has an email address still owes its notices to nobody, counted
// as no_email.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A person as a signup leaves one, and, once it is in a team, its membership there.
type keyNoticePerson struct {
	userId server.Id
	// its sign-in email addresses, normalized; none for a person that signs in with a phone number alone
	emails []string
	// a session of its own network
	personalSession *session.ClientSession
	member          *NetworkMember
	// a session of its sign-in to the team
	teamSession *session.ClientSession
}

// A synthetic sign-in address of the test, unique to it.
func newKeyNoticeEmail(label string) string {
	return fmt.Sprintf("%s-%s@team-key-notice.example", label, strings.ReplaceAll(server.NewId().String(), "-", "")[24:32])
}

// Gives userId an account as a signup leaves one: a network it owns and one verified sign-in, userAuth, an email
// address or a phone number.
func newKeyNoticeAccount(t testing.TB, ctx context.Context, userId server.Id, userAuth string) *keyNoticePerson {
	t.Helper()
	normalUserAuth, userAuthType := NormalUserAuth(userAuth)
	person := &keyNoticePerson{userId: userId, emails: []string{}}
	authType := ""
	switch userAuthType {
	case UserAuthTypeEmail:
		authType = "email"
		person.emails = append(person.emails, normalUserAuth)
	case UserAuthTypePhone:
		authType = "phone"
	default:
		t.Fatalf("%q is neither an email address nor a phone number", userAuth)
	}
	networkId := server.NewId()
	networkName := "kn-person-" + strings.ReplaceAll(networkId.String(), "-", "")[16:32]
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`,
			networkId,
			networkName,
			userId,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_user (user_id, user_name, auth_type, user_auth, verified) VALUES ($1, 'person', $2, $3, true)`,
			userId,
			AuthTypePassword,
			normalUserAuth,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_user_auth_password (user_id, user_auth, auth_type, verified) VALUES ($1, $2, $3, true)`,
			userId,
			normalUserAuth,
			authType,
		))
	})
	person.personalSession = session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId:   networkId,
		UserId:      userId,
		NetworkName: networkName,
	})
	return person
}

// A new person whose one sign-in is userAuth.
func newKeyNoticePerson(t testing.TB, ctx context.Context, userAuth string) *keyNoticePerson {
	t.Helper()
	return newKeyNoticeAccount(t, ctx, server.NewId(), userAuth)
}

// Adds a Google sign-in at another email address to the person.
func (self *keyNoticePerson) addGoogleSignIn(t testing.TB, ctx context.Context, email string) {
	t.Helper()
	normalEmail, userAuthType := NormalUserAuth(email)
	if userAuthType != UserAuthTypeEmail {
		t.Fatalf("%q is no email address", email)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_user_auth_sso (user_id, auth_type, auth_jwt, user_auth) VALUES ($1, $2, 'synthetic', $3)`,
			self.userId,
			AuthTypeGoogle,
			normalEmail,
		))
	})
	self.emails = append(self.emails, normalEmail)
}

// A team made by team-create, which writes its owner of record with no sign-in (D-TEAM-2).
type keyNoticeTeam struct {
	ctx             context.Context
	networkId       server.Id
	ownerOfRecordId server.Id
}

// The team the person creates from its own network by team-create, the person its Owner and signed in to it.
func newKeyNoticeTeam(t testing.TB, ctx context.Context, owner *keyNoticePerson) *keyNoticeTeam {
	t.Helper()
	created := createTestTeam(t, testNetworkTeams(server.NowUtc), owner.personalSession, testTeamName("key-notice"))
	ownerOfRecordId := testNetworkAdminUserId(ctx, *created.NetworkId)
	if ownerOfRecordId == nil || *ownerOfRecordId == owner.userId {
		t.Fatalf("the team's owner of record is %v", ownerOfRecordId)
	}
	team := &keyNoticeTeam{
		ctx:             ctx,
		networkId:       *created.NetworkId,
		ownerOfRecordId: *ownerOfRecordId,
	}
	team.signIn(t, owner)
	if owner.member.Role != NetworkMemberRoleOwner {
		t.Fatalf("the team's creator is its %s", owner.member.Role)
	}
	return team
}

// Signs the person in to the team, with its membership.
func (self *keyNoticeTeam) signIn(t testing.TB, person *keyNoticePerson) {
	t.Helper()
	signIn := ResolveNetworkSignIn(self.ctx, person.userId, self.networkId, server.NowUtc())
	if signIn == nil || signIn.MemberId == nil {
		t.Fatalf("the person's sign-in to the team: %+v", signIn)
	}
	person.member = GetNetworkMember(self.ctx, *signIn.MemberId)
	if person.member == nil {
		t.Fatalf("the person's membership %s is gone", *signIn.MemberId)
	}
	person.teamSession = session.NewLocalClientSession(self.ctx, "192.0.2.1:1", NewNetworkSignInByJwt(signIn, false))
	t.Cleanup(person.teamSession.Cancel)
}

// The person joins the team with the role, as an invite's acceptance makes a membership, and signs in to it.
func (self *keyNoticeTeam) join(t testing.TB, person *keyNoticePerson, role NetworkMemberRole) {
	t.Helper()
	addTestSignInMember(t, self.ctx, self.networkId, person.userId, role, false)
	self.signIn(t, person)
}

// The sign-in email addresses of the people, sorted.
func keyNoticeEmailsOf(people ...*keyNoticePerson) []string {
	emails := []string{}
	for _, person := range people {
		emails = append(emails, person.emails...)
	}
	slices.Sort(emails)
	return emails
}

// Holds that the network owes the template's notice to each of recipients once and to nobody else, each notice
// listing changeCount changes, every one of them action; then settles them, so the next change owes its own.
func requireKeyNoticeRecipients(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	templateName string,
	action string,
	changeCount int,
	recipients []string,
) {
	t.Helper()
	notices := unsentTestNotices(t, ctx, networkId, templateName)
	got := []string{}
	for _, notice := range notices {
		got = append(got, notice.userAuth)
		if notice.deliverTime == nil {
			t.Fatalf("%s: the notice to %s is held, want due", templateName, notice.userAuth)
		}
		changes := notice.changes(t)
		if len(changes) != changeCount {
			t.Fatalf("%s: the notice to %s lists %d changes, want %d: %s", templateName, notice.userAuth, len(changes), changeCount, notice.templateJson)
		}
		for _, change := range changes {
			if fmt.Sprint(change["action"]) != action {
				t.Fatalf("%s: the notice to %s lists a change %v, want %s: %s", templateName, notice.userAuth, change["action"], action, notice.templateJson)
			}
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, recipients) {
		t.Fatalf("%s %s: owed to %v, want one each to %v", templateName, action, got, recipients)
	}
	settleTestNotices(ctx, networkId)
}

// A team's auth key notices reach each of its Owners and Admins, at each of their sign-in email addresses, and nobody
// else: a key an Admin creates, the Owner's revoke of it, a key a Member creates for its own devices, and the key the
// Member's removal ends each owe one notice to each, listing the change, and none to the Member, a Network admin, a
// pending Admin or the owner of record, whose notices used to be all a team's key changes owed and which has no
// address. None is counted as no_email.
func TestTeamAuthKeyNoticesReachItsOwnersAndAdmins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		owner := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("owner"))
		owner.addGoogleSignIn(t, ctx, newKeyNoticeEmail("owner-google"))
		team := newKeyNoticeTeam(t, ctx, owner)
		admin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("admin"))
		team.join(t, admin, NetworkMemberRoleAdmin)
		networkAdmin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("network-admin"))
		team.join(t, networkAdmin, NetworkMemberRoleNetworkAdmin)
		member := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("member"))
		team.join(t, member, NetworkMemberRoleMember)
		pendingAdmin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("pending-admin"))
		addTestSignInMember(t, ctx, team.networkId, pendingAdmin.userId, NetworkMemberRoleAdmin, true)
		recipients := keyNoticeEmailsOf(owner, admin)
		noEmailBefore := securityNoticeCount(SecurityNoticeAuthKey, securityNoticeNoEmail)

		adminKey := createTestAuthKey(t, admin.teamSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 2}, server.NowUtc())
		requireKeyNoticeRecipients(t, ctx, team.networkId, testAuthKeyNoticeTemplate, SecurityAuthKeyCreated, 1, recipients)

		revoked, err := RevokeAuthKey(&RevokeAuthKeyArgs{AuthKeyId: *adminKey.AuthKeyId}, owner.teamSession)
		if err != nil || revoked.Error != nil {
			t.Fatalf("the Owner's revoke of the Admin's key: %v %+v", err, revoked)
		}
		requireKeyNoticeRecipients(t, ctx, team.networkId, testAuthKeyNoticeTemplate, SecurityAuthKeyRevoked, 1, recipients)

		// a Member's key for its own devices: the Owners and Admins hear of it, the Member does not
		memberKey := createTestAuthKey(t, member.teamSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, server.NowUtc())
		requireKeyNoticeRecipients(t, ctx, team.networkId, testAuthKeyNoticeTemplate, SecurityAuthKeyCreated, 1, recipients)

		// the Member's removal ends its key (QD-1211), and the notice of it reaches them alike
		denied := denyTestMember(t, ctx, team.networkId, member.member.MemberId, newTestMemberRemovalEvent(team.networkId, member.member.MemberId))
		if len(denied.RevokedAuthKeys) != 1 || denied.RevokedAuthKeys[0].AuthKeyId != *memberKey.AuthKeyId {
			t.Fatalf("the Member's removal ended %+v, want its one key", denied.RevokedAuthKeys)
		}
		requireKeyNoticeRecipients(t, ctx, team.networkId, testAuthKeyNoticeTemplate, SecurityAuthKeyRevoked, 1, recipients)

		if delta := securityNoticeCount(SecurityNoticeAuthKey, securityNoticeNoEmail) - noEmailBefore; delta != 0 {
			t.Fatalf("counted %v no_email auth key notices, want none: every change had its Owners and Admins to tell", delta)
		}
	})
}

// A team's API key notices reach each of its Owners and Admins and nobody else: a key an Admin creates, the Owner's
// delete of it and a key the Owner creates each owe one notice to each, naming the key, and none to a Member, a
// pending Admin or the owner of record. None is counted as no_email.
func TestTeamApiKeyNoticesReachItsOwnersAndAdmins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		owner := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("owner"))
		team := newKeyNoticeTeam(t, ctx, owner)
		admin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("admin"))
		team.join(t, admin, NetworkMemberRoleAdmin)
		secondAdmin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("second-admin"))
		team.join(t, secondAdmin, NetworkMemberRoleAdmin)
		member := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("member"))
		team.join(t, member, NetworkMemberRoleMember)
		pendingAdmin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("pending-admin"))
		addTestSignInMember(t, ctx, team.networkId, pendingAdmin.userId, NetworkMemberRoleAdmin, true)
		recipients := keyNoticeEmailsOf(owner, admin, secondAdmin)
		noEmailBefore := securityNoticeCount(testApiKeyNoticeLabel, securityNoticeNoEmail)

		adminKey := createTestApiKey(t, admin.teamSession, "ci-deploy")
		notices := unsentTestNotices(t, ctx, team.networkId, testApiKeyNoticeTemplate)
		for _, notice := range notices {
			if changes := notice.changes(t); len(changes) != 1 || changes[0]["name"] != "ci-deploy" {
				t.Fatalf("the notice to %s does not name the key: %s", notice.userAuth, notice.templateJson)
			}
		}
		requireTestNoticesHoldNoKey(t, ctx, team.networkId, adminKey)
		requireKeyNoticeRecipients(t, ctx, team.networkId, testApiKeyNoticeTemplate, SecurityApiKeyCreated, 1, recipients)

		if err := DeleteApiKey(&adminKey.Id, owner.teamSession); err != nil {
			t.Fatal(err)
		}
		if _, ok := testApiKeysOf(t, owner.teamSession)[adminKey.Id]; ok {
			t.Fatal("the deleted key is still listed")
		}
		requireKeyNoticeRecipients(t, ctx, team.networkId, testApiKeyNoticeTemplate, SecurityApiKeyDeleted, 1, recipients)

		createTestApiKey(t, owner.teamSession, "owner-automation")
		requireKeyNoticeRecipients(t, ctx, team.networkId, testApiKeyNoticeTemplate, SecurityApiKeyCreated, 1, recipients)

		if delta := securityNoticeCount(testApiKeyNoticeLabel, securityNoticeNoEmail) - noEmailBefore; delta != 0 {
			t.Fatalf("counted %v no_email API key notices, want none", delta)
		}
	})
}

// A personal network's key notices are as they were: its owner's auth key and API key changes reach its owner's
// sign-in email addresses alone, though the owner is an Owner of a team that has an Admin, and owe the team nothing.
func TestPersonalNetworkKeyNoticesStillReachItsOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		owner := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("owner"))
		owner.addGoogleSignIn(t, ctx, newKeyNoticeEmail("owner-google"))
		team := newKeyNoticeTeam(t, ctx, owner)
		admin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("admin"))
		team.join(t, admin, NetworkMemberRoleAdmin)
		personalNetworkId := owner.personalSession.ByJwt.NetworkId
		recipients := keyNoticeEmailsOf(owner)

		authKey := createTestAuthKey(t, owner.personalSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, server.NowUtc())
		requireKeyNoticeRecipients(t, ctx, personalNetworkId, testAuthKeyNoticeTemplate, SecurityAuthKeyCreated, 1, recipients)
		revoked, err := RevokeAuthKey(&RevokeAuthKeyArgs{AuthKeyId: *authKey.AuthKeyId}, owner.personalSession)
		if err != nil || revoked.Error != nil {
			t.Fatalf("the owner's revoke: %v %+v", err, revoked)
		}
		requireKeyNoticeRecipients(t, ctx, personalNetworkId, testAuthKeyNoticeTemplate, SecurityAuthKeyRevoked, 1, recipients)

		apiKey := createTestApiKey(t, owner.personalSession, "personal-bot")
		requireKeyNoticeRecipients(t, ctx, personalNetworkId, testApiKeyNoticeTemplate, SecurityApiKeyCreated, 1, recipients)
		if err := DeleteApiKey(&apiKey.Id, owner.personalSession); err != nil {
			t.Fatal(err)
		}
		requireKeyNoticeRecipients(t, ctx, personalNetworkId, testApiKeyNoticeTemplate, SecurityApiKeyDeleted, 1, recipients)

		// the team, the owner an Owner of it, owes nothing for its Owner's own network's keys
		for _, templateName := range []string{testAuthKeyNoticeTemplate, testApiKeyNoticeTemplate} {
			if notices := unsentTestNotices(t, ctx, team.networkId, templateName); len(notices) != 0 {
				t.Fatalf("the team owes %d %s notices for its Owner's own network: %s", len(notices), templateName, notices[0].templateJson)
			}
		}
	})
}

// A team none of whose Owners and Admins has a sign-in email address owes its key notices to nobody, counted as
// no_email, as a network whose owner signs in with a phone number alone does: its Owner signs in with a phone number,
// its Member and its pending Admin, which hear of no key, have email addresses, and its owner of record has no
// sign-in. Each key change still goes through, and no message is owed to anyone, by email or SMS.
func TestTeamKeyNoticeWithNoOwnerOrAdminEmailIsOwedToNobody(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		owner := newKeyNoticePerson(t, ctx, "+1 415-555-0168")
		team := newKeyNoticeTeam(t, ctx, owner)
		member := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("member"))
		team.join(t, member, NetworkMemberRoleMember)
		pendingAdmin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("pending-admin"))
		addTestSignInMember(t, ctx, team.networkId, pendingAdmin.userId, NetworkMemberRoleAdmin, true)
		authKeyNoEmailBefore := securityNoticeCount(SecurityNoticeAuthKey, securityNoticeNoEmail)
		authKeyQueuedBefore := securityNoticeCount(SecurityNoticeAuthKey, securityNoticeQueued)
		apiKeyNoEmailBefore := securityNoticeCount(testApiKeyNoticeLabel, securityNoticeNoEmail)
		apiKeyQueuedBefore := securityNoticeCount(testApiKeyNoticeLabel, securityNoticeQueued)

		ownerKey := createTestAuthKey(t, owner.teamSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, server.NowUtc())
		createTestAuthKey(t, member.teamSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, server.NowUtc())
		denied := denyTestMember(t, ctx, team.networkId, member.member.MemberId, newTestMemberRemovalEvent(team.networkId, member.member.MemberId))
		if len(denied.RevokedAuthKeys) != 1 {
			t.Fatalf("the Member's removal ended %d keys, want its one", len(denied.RevokedAuthKeys))
		}
		listed, err := GetNetworkAuthKeys(owner.teamSession)
		if err != nil || len(listed.AuthKeys) != 1 || listed.AuthKeys[0].AuthKeyId != *ownerKey.AuthKeyId {
			t.Fatalf("the team's keys are %+v %v, want the Owner's alone", listed, err)
		}
		apiKey := createTestApiKey(t, owner.teamSession, "phone-owner-bot")
		if err := DeleteApiKey(&apiKey.Id, owner.teamSession); err != nil {
			t.Fatal(err)
		}
		if _, ok := testApiKeysOf(t, owner.teamSession)[apiKey.Id]; ok {
			t.Fatal("the deleted key is still listed")
		}

		if count := countRows(ctx, `SELECT COUNT(*) FROM account_message_outbox WHERE network_id = $1`, team.networkId); count != 0 {
			t.Fatalf("the team owes %d messages, want none", count)
		}
		for _, c := range []struct {
			notice string
			result string
			before float64
			want   float64
		}{
			{SecurityNoticeAuthKey, securityNoticeNoEmail, authKeyNoEmailBefore, 3},
			{SecurityNoticeAuthKey, securityNoticeQueued, authKeyQueuedBefore, 0},
			{testApiKeyNoticeLabel, securityNoticeNoEmail, apiKeyNoEmailBefore, 2},
			{testApiKeyNoticeLabel, securityNoticeQueued, apiKeyQueuedBefore, 0},
		} {
			if delta := securityNoticeCount(c.notice, c.result) - c.before; delta != c.want {
				t.Errorf("counted %v %s %s notices, want %v", delta, c.result, c.notice, c.want)
			}
		}
	})
}
