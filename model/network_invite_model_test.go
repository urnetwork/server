//go:build sidecar_pending

package model

// §7.5's invitations (network_invite_model.go; TEAM-M6-04), which need MG6: an Owner's invite, its acceptance by the
// invited email's person, and every refusal the route owes, with the team tier stubbed on, since QUOTA-M6-01's tier is
// not in transfer_balance yet.

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A team, its Owner's member credential, and the invites with the team tier on at a clock the test moves.
type inviteFixture struct {
	ctx           context.Context
	teamNetworkId server.Id
	ownerUserId   server.Id
	ownerMemberId server.Id
	owner         *session.ClientSession
	now           time.Time
	invites       *networkInvites
}

func newInviteFixture(t testing.TB) *inviteFixture {
	t.Helper()
	ctx := context.Background()
	_, restore := Testing_InMemoryNetworkNameSearch(ctx)
	t.Cleanup(restore)
	_, ownerUserId, _, personal := testTeamPersonalNetwork(t, ctx, "invite-owner")
	created := createTestTeam(t, testNetworkTeams(server.NowUtc), personal, testTeamName("invite-team"))
	self := &inviteFixture{
		ctx:           ctx,
		teamNetworkId: *created.NetworkId,
		ownerUserId:   ownerUserId,
		ownerMemberId: *created.MemberId,
		now:           server.NowUtc(),
	}
	self.owner = session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId:   self.teamNetworkId,
		UserId:      ownerUserId,
		NetworkName: created.NetworkName,
		MemberId:    &self.ownerMemberId,
	})
	self.invites = &networkInvites{
		settings: DefaultNetworkInviteSettings(),
		now:      func() time.Time { return self.now },
		tierOf: func(ctx context.Context, networkId server.Id) NetworkTier {
			return NetworkTierTeam
		},
	}
	return self
}

// Invites email to the team as role, failing the test on a refusal.
func (self *inviteFixture) invite(t testing.TB, email string, role NetworkMemberRole) *CreateNetworkMemberInviteResult {
	t.Helper()
	result, err := self.invites.create(&CreateNetworkMemberInviteArgs{Email: email, Role: role, ExpireDays: 7}, self.owner)
	if err != nil || result.Error != nil || result.InviteId == nil || result.InviteBearer == "" {
		t.Fatalf("the invite: %v %+v", err, result.Error)
	}
	return result
}

// The invite's row: whether it is there, and its email and role.
func (self *inviteFixture) inviteRow(t testing.TB, inviteId server.Id) (found bool, email string, role string, tokenSha256 []byte) {
	t.Helper()
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(self.ctx, `SELECT email, role, token_sha256 FROM network_invite WHERE invite_id = $1`, inviteId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&email, &role, &tokenSha256))
				found = true
			}
		})
	})
	return
}

// The team's admin event actions, in order.
func (self *inviteFixture) eventActions(t testing.TB) []string {
	t.Helper()
	actions := []string{}
	for _, event := range GetNetworkAdminEvents(self.ctx, self.teamNetworkId, 0, 100) {
		actions = append(actions, event.Action)
	}
	return actions
}

// An Owner invites an email as a role: the row holds the email lowercased and only the SHA-256 of the bearer's
// secret; the invited person accepts with its own sign-in and joins as a member, answered the invite's role for the
// admitting device; the invite is spent, and both steps are admin events.
func TestMemberInviteCreateAndAccept(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newInviteFixture(t)
		_, inviteeUserId, inviteeEmail, invitee := testTeamPersonalNetwork(t, self.ctx, "invitee")

		created := self.invite(t, "  "+strings.ToUpper(inviteeEmail)+" ", NetworkMemberRoleAdmin)
		found, email, role, tokenSha256 := self.inviteRow(t, *created.InviteId)
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, email, strings.ToLower(inviteeEmail))
		connect.AssertEqual(t, role, string(NetworkMemberRoleAdmin))
		inviteId, secret, ok := parseNetworkInviteBearer(created.InviteBearer)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, inviteId, *created.InviteId)
		secretSha256 := sha256.Sum256(secret)
		connect.AssertEqual(t, tokenSha256, secretSha256[:])
		connect.AssertEqual(t, created.ExpireTime.Sub(self.now.UTC().Truncate(time.Microsecond)), 7*24*time.Hour)
		connect.AssertEqual(t, strings.Contains(created.String()+created.GoString(), created.InviteBearer), false)

		accepted, err := self.invites.accept(&AcceptNetworkMemberInviteArgs{InviteBearer: created.InviteBearer}, invitee)
		connect.AssertEqual(t, err, nil)
		if accepted.Error != nil {
			t.Fatalf("the accept: %+v", accepted.Error)
		}
		connect.AssertEqual(t, *accepted.NetworkId, self.teamNetworkId)
		connect.AssertEqual(t, accepted.InvitedRole, NetworkMemberRoleAdmin)
		member := GetNetworkMemberOfUser(self.ctx, self.teamNetworkId, inviteeUserId)
		connect.AssertEqual(t, member != nil, true)
		connect.AssertEqual(t, member.MemberId, *accepted.MemberId)
		// the role comes from the config repo, never the invite (TEAM-M6-03)
		connect.AssertEqual(t, member.Role, NetworkMemberRoleMember)
		connect.AssertEqual(t, member.Pending, false)
		found, _, _, _ = self.inviteRow(t, *created.InviteId)
		connect.AssertEqual(t, found, false)

		actions := self.eventActions(t)
		connect.AssertEqual(t, actions[len(actions)-2:], []string{AdminEventActionMemberInviteCreate, AdminEventActionMemberAdd})

		// spent: a second accept finds no invite
		again, err := self.invites.accept(&AcceptNetworkMemberInviteArgs{InviteBearer: created.InviteBearer}, invitee)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again.Error.Message, networkInviteInvalidMessage)
	})
}

// The refusals: who may invite, the tier, the invite's fields; and at the accept, another email's person, a client
// token, an expired invite, a wrong secret, a malformed bearer and a person already a member. A refusal at the accept
// leaves the invite unspent but for the expired one, which is gone.
func TestMemberInviteRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newInviteFixture(t)
		_, _, inviteeEmail, invitee := testTeamPersonalNetwork(t, self.ctx, "invitee")
		_, _, _, stranger := testTeamPersonalNetwork(t, self.ctx, "stranger")

		requireCreateRefused := func(name string, caller *session.ClientSession, args *CreateNetworkMemberInviteArgs, message string) {
			t.Helper()
			result, err := self.invites.create(args, caller)
			if message == "" {
				if err == nil {
					t.Fatalf("%s: answered %+v", name, result)
				}
				return
			}
			if err != nil || result.Error == nil || result.Error.Message != message {
				t.Fatalf("%s: %v %+v", name, err, result)
			}
		}
		args := func() *CreateNetworkMemberInviteArgs {
			return &CreateNetworkMemberInviteArgs{Email: inviteeEmail, Role: NetworkMemberRoleMember, ExpireDays: 7}
		}
		requireCreateRefused("by a stranger's credential", stranger, args(), "")
		clientId := server.NewId()
		ownerClient := session.Testing_CreateClientSession(self.ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: self.ownerUserId, ClientId: &clientId, MemberId: &self.ownerMemberId})
		requireCreateRefused("by a client token", ownerClient, args(), "")
		noEmail := args()
		noEmail.Email = "no at sign"
		requireCreateRefused("with no email", self.owner, noEmail, networkInviteEmailMessage)
		noRole := args()
		noRole.Role = "chief"
		requireCreateRefused("with no role", self.owner, noRole, networkInviteRoleMessage)
		for _, days := range []int{0, 31} {
			tooLong := args()
			tooLong.ExpireDays = days
			requireCreateRefused("for an invalid life", self.owner, tooLong, "An invite lives 1 to 30 days.")
		}
		personalTier := *self.invites
		personalTier.tierOf = func(ctx context.Context, networkId server.Id) NetworkTier {
			return NetworkTierFree
		}
		if _, err := personalTier.create(args(), self.owner); err == nil {
			t.Fatal("an invite outside the team tier")
		}

		requireAcceptRefused := func(name string, caller *session.ClientSession, bearer string, message string) {
			t.Helper()
			result, err := self.invites.accept(&AcceptNetworkMemberInviteArgs{InviteBearer: bearer}, caller)
			if message == "" {
				if err == nil {
					t.Fatalf("%s: answered %+v", name, result)
				}
				return
			}
			if err != nil || result.Error == nil || result.Error.Message != message {
				t.Fatalf("%s: %v %+v", name, err, result)
			}
		}
		created := self.invite(t, inviteeEmail, NetworkMemberRoleMember)
		requireAcceptRefused("by another email's person", stranger, created.InviteBearer, networkInviteOtherEmailMessage)
		inviteeClientId := server.NewId()
		inviteeClient := session.Testing_CreateClientSession(self.ctx, &session.ByJwt{NetworkId: invitee.ByJwt.NetworkId, UserId: invitee.ByJwt.UserId, ClientId: &inviteeClientId})
		requireAcceptRefused("by a client token", inviteeClient, created.InviteBearer, "")
		inviteId, _, _ := parseNetworkInviteBearer(created.InviteBearer)
		requireAcceptRefused("with a wrong secret", invitee, networkInviteBearer(inviteId, make([]byte, networkInviteSecretByteCount)), networkInviteInvalidMessage)
		requireAcceptRefused("a malformed bearer", invitee, "ur-invite-nothing", networkInviteInvalidMessage)
		found, _, _, _ := self.inviteRow(t, *created.InviteId)
		connect.AssertEqual(t, found, true)

		// past its life an invite is refused and gone
		self.now = created.ExpireTime.Add(time.Second)
		requireAcceptRefused("expired", invitee, created.InviteBearer, networkInviteExpiredMessage)
		found, _, _, _ = self.inviteRow(t, *created.InviteId)
		connect.AssertEqual(t, found, false)
		self.now = server.NowUtc()

		// the team's own Owner is already a member: the invite is left unspent
		ownerEmail := ""
		server.Tx(self.ctx, func(tx server.PgTx) {
			if email := networkMemberEmailOfUserInTx(self.ctx, tx, self.ownerUserId); email != nil {
				ownerEmail = *email
			}
		})
		ownerPersonal := session.Testing_CreateClientSession(self.ctx, &session.ByJwt{NetworkId: server.NewId(), UserId: self.ownerUserId})
		forOwner := self.invite(t, ownerEmail, NetworkMemberRoleMember)
		requireAcceptRefused("by a member", ownerPersonal, forOwner.InviteBearer, networkInviteAlreadyMemberMessage)
		found, _, _, _ = self.inviteRow(t, *forOwner.InviteId)
		connect.AssertEqual(t, found, true)
	})
}

// §22.5's §7 row: a one-command admin token's invite works once and answers a retry after a lost response with the
// stored result, so no second invite is made; its email and role are the ones it made (the token's cmd binds the body
// at the authenticator, oauth's admin_token.go). A token without the all scope makes none.
func TestMemberInviteAdminTokenWorksOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newInviteFixture(t)
		_, _, inviteeEmail, _ := testTeamPersonalNetwork(t, self.ctx, "invitee")
		use := &session.AdminCommandUse{
			NetworkId:  self.teamNetworkId,
			MemberId:   self.ownerMemberId,
			Jti:        "invite-" + server.NewId().String(),
			Cmd:        "a command digest",
			Scopes:     []string{networkInviteAdminScope},
			ExpireTime: self.now.Add(time.Hour),
		}
		tokenSession := session.Testing_CreateClientSession(self.ctx, &session.ByJwt{
			NetworkId: self.teamNetworkId, UserId: self.ownerUserId, MemberId: &self.ownerMemberId,
		})
		tokenSession.AdminCommand = use
		tokenSession.UnsignedIdentity = true
		args := &CreateNetworkMemberInviteArgs{Email: inviteeEmail, Role: NetworkMemberRoleAuditor, ExpireDays: 3}

		first, err := self.invites.create(args, tokenSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, first.Error == nil && first.InviteId != nil, true)
		retry, err := self.invites.create(args, tokenSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *retry.InviteId, *first.InviteId)
		connect.AssertEqual(t, retry.InviteBearer, first.InviteBearer)
		count := testTeamCount(self.ctx, `SELECT COUNT(*) FROM network_invite WHERE network_id = $1`, self.teamNetworkId)
		connect.AssertEqual(t, count, 1)
		_, email, role, _ := self.inviteRow(t, *first.InviteId)
		connect.AssertEqual(t, email, strings.ToLower(inviteeEmail))
		connect.AssertEqual(t, role, string(NetworkMemberRoleAuditor))

		narrow := *use
		narrow.Jti = "invite-" + server.NewId().String()
		narrow.Scopes = []string{"auth_keys"}
		tokenSession.AdminCommand = &narrow
		if _, err := self.invites.create(args, tokenSession); err == nil {
			t.Fatal("an admin token without the all scope made an invite")
		}
	})
}
