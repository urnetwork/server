//go:build sidecar_pending

package model

// A team's security notices reach every Owner first, at all its addresses, and are cut at whole people (QD-1218; §7.8's
// notices row, R-TEAM-12, QD-1215, QD-1217, QD-702), on Postgres with MG0, MG1 and MG6, through the recipient rules the
// notices call and the notices themselves: the key and authority rule, a team's Owners and Admins
// (securityNoticeAuthorityRecipientsInTx), and the payout rule, its Owners and Billing admins
// (securityNoticeBillingRecipientsInTx). Both kept only the 8 addresses that sorted first, one account's bound, so in a
// team whose Owners and Admins held more than eight addresses between them an Owner whose addresses sorted after the
// Admins' heard of no key or authority change, nor of a payout change past eight Billing admins' addresses. Each team
// here is made by team-create, with the people of the key notices' tests (security_notice_team_recipients_test.go); the
// order and the cut alone, with no database, are security_notice_recipient_order_test.go's.

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

// A new person signing in at four email addresses, one of each kind a person holds: its own network_user.user_auth,
// its verified email sign-in, and an Apple and a Google sign-in, each address beginning with the label.
func newFourAddressNoticePerson(t testing.TB, ctx context.Context, label string) *keyNoticePerson {
	t.Helper()
	person := newKeyNoticePerson(t, ctx, newKeyNoticeEmail(label+"-legacy"))
	emailSignIn, _ := NormalUserAuth(newKeyNoticeEmail(label + "-email"))
	appleSignIn, _ := NormalUserAuth(newKeyNoticeEmail(label + "-apple"))
	server.Tx(ctx, func(tx server.PgTx) {
		// the email sign-in moves to an address of its own, the legacy user_auth keeping the first
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_user_auth_password SET user_auth = $2 WHERE user_id = $1 AND auth_type = 'email'`,
			person.userId,
			emailSignIn,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_user_auth_sso (user_id, auth_type, auth_jwt, user_auth) VALUES ($1, $2, 'synthetic', $3)`,
			person.userId,
			AuthTypeApple,
			appleSignIn,
		))
	})
	person.emails = append(person.emails, emailSignIn, appleSignIn)
	person.addGoogleSignIn(t, ctx, newKeyNoticeEmail(label+"-google"))
	if len(person.emails) != 4 {
		t.Fatalf("the person signs in at %v, want four addresses", person.emails)
	}
	return person
}

// The person joins the team with the role, as an invite's acceptance makes a membership, without signing in to it.
func addNoticeTeamMember(t testing.TB, ctx context.Context, team *keyNoticeTeam, person *keyNoticePerson, role NetworkMemberRole) {
	t.Helper()
	person.member = addTestSignInMember(t, ctx, team.networkId, person.userId, role, false)
}

// The people's email addresses in the order a team's notice reaches them: the people in the order given, each one's
// addresses sorted.
func noticeEmailsInOrder(people ...*keyNoticePerson) []string {
	emails := []string{}
	for _, person := range people {
		personEmails := slices.Clone(person.emails)
		slices.Sort(personEmails)
		emails = append(emails, personEmails...)
	}
	return emails
}

func sortedNoticeEmails(emails []string) []string {
	sorted := slices.Clone(emails)
	slices.Sort(sorted)
	return sorted
}

// The network's recipients by the key and authority rule and by the payout rule, read as the notices read them.
func noticeRecipientsByRule(ctx context.Context, networkId server.Id) (authority []string, billing []string) {
	server.Tx(ctx, func(tx server.PgTx) {
		authority = securityNoticeAuthorityRecipientsInTx(ctx, tx, networkId)
		billing = securityNoticeBillingRecipientsInTx(ctx, tx, networkId)
	})
	return
}

// Selects a new wallet as the team's payout wallet with the session, and returns its address.
func setTestTeamPayoutWallet(t testing.TB, team *keyNoticeTeam, person *keyNoticePerson) string {
	t.Helper()
	address := testSolanaAddress(t)
	walletId := CreateAccountWalletExternal(person.teamSession, &CreateAccountWalletExternalArgs{
		NetworkId:        team.networkId,
		Blockchain:       SOL.String(),
		WalletAddress:    address,
		DefaultTokenType: "USDC",
	})
	if walletId == nil {
		t.Fatal("the team's wallet was not created")
	}
	if err := SetPayoutWallet(person.teamSession.Ctx, team.networkId, *walletId); err != nil {
		t.Fatal(err)
	}
	return address
}

// A team of one Owner signing in at four addresses, eight Admins and eight Billing admins at one address each, every
// one of the Owner's addresses sorting after every other member's, reaches the Owner at all four first and then each
// Admin, in the order they joined, by the key and authority rule, and the Owner and then each Billing admin by the
// payout rule: twelve addresses each, where the cut at the 8 that sorted first kept eight others and the Owner heard of
// nothing. An auth key the Owner creates owes its notice to the Owner's four addresses and the Admins', and a payout
// wallet the Owner selects to the Owner's four and the Billing admins'.
func TestTeamNoticesReachTheOwnerAtEveryAddressThenEachMemberInTurn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		owner := newFourAddressNoticePerson(t, ctx, "owner")
		team := newKeyNoticeTeam(t, ctx, owner)
		admins := []*keyNoticePerson{}
		billingAdmins := []*keyNoticePerson{}
		for i := range 8 {
			// the first of each role to join sorts last among them
			admin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail(fmt.Sprintf("admin-%d", 8-i)))
			addNoticeTeamMember(t, ctx, team, admin, NetworkMemberRoleAdmin)
			admins = append(admins, admin)
			billingAdmin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail(fmt.Sprintf("billing-admin-%d", 8-i)))
			addNoticeTeamMember(t, ctx, team, billingAdmin, NetworkMemberRoleBillingAdmin)
			billingAdmins = append(billingAdmins, billingAdmin)
		}
		authorityWant := noticeEmailsInOrder(append([]*keyNoticePerson{owner}, admins...)...)
		billingWant := noticeEmailsInOrder(append([]*keyNoticePerson{owner}, billingAdmins...)...)
		// the failing team: one account's bound keeps the 8 addresses that sort first, and none is the Owner's
		for _, want := range [][]string{authorityWant, billingWant} {
			for _, email := range securityNoticeEmails(want) {
				if slices.Contains(owner.emails, email) {
					t.Fatalf("one account's bound kept the Owner's %s; the test no longer holds the failing team", email)
				}
			}
		}

		authority, billing := noticeRecipientsByRule(ctx, team.networkId)
		requireRecipientOrder(t, authority, authorityWant)
		requireRecipientOrder(t, billing, billingWant)

		createTestAuthKey(t, owner.teamSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, server.NowUtc())
		requireKeyNoticeRecipients(t, ctx, team.networkId, testAuthKeyNoticeTemplate, SecurityAuthKeyCreated, 1, sortedNoticeEmails(authorityWant))

		address := setTestTeamPayoutWallet(t, team, owner)
		requireWalletNoticeRecipients(t, ctx, team.networkId, sortedNoticeEmails(billingWant), walletNoticeChange{
			destination: SecurityWalletDestinationUsdc,
			newWallet:   testMasked(address),
		})
	})
}

// A team one person past its notices' people bound: its Owner, then one member of the role fewer than the bound, each
// signing in at two addresses, then a second Owner, which joins after them all with an address sorting after
// everyone's.
type pastBoundNoticeTeam struct {
	*keyNoticeTeam
	owner     *keyNoticePerson
	lastOwner *keyNoticePerson
	// the members of the role, in the order they joined
	members []*keyNoticePerson
}

func newPastBoundNoticeTeam(t testing.TB, ctx context.Context, role NetworkMemberRole) *pastBoundNoticeTeam {
	t.Helper()
	memberMaxCount := DefaultSecurityNoticeSettings().TeamRecipientMemberMaxCount
	if memberMaxCount < 3 {
		t.Fatalf("a team's notice reaches %d people, too few for two Owners, a member reached and one cut", memberMaxCount)
	}
	owner := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("owner"))
	team := &pastBoundNoticeTeam{
		keyNoticeTeam: newKeyNoticeTeam(t, ctx, owner),
		owner:         owner,
		members:       []*keyNoticePerson{},
	}
	for i := range memberMaxCount - 1 {
		member := newKeyNoticePerson(t, ctx, newKeyNoticeEmail(fmt.Sprintf("%s-%03d", role, i)))
		member.addGoogleSignIn(t, ctx, newKeyNoticeEmail(fmt.Sprintf("%s-%03d-google", role, i)))
		addNoticeTeamMember(t, ctx, team.keyNoticeTeam, member, role)
		team.members = append(team.members, member)
	}
	team.lastOwner = newKeyNoticePerson(t, ctx, newKeyNoticeEmail("zz-last-owner"))
	addNoticeTeamMember(t, ctx, team.keyNoticeTeam, team.lastOwner, NetworkMemberRoleOwner)
	for _, member := range team.members {
		if team.lastOwner.member.MemberId.Cmp(member.member.MemberId) <= 0 {
			t.Fatalf("the last Owner's member_id %s is not after the %s member's %s", team.lastOwner.member.MemberId, role, member.member.MemberId)
		}
	}
	return team
}

// The addresses the team's notice reaches past its bound, in order: both Owners, then every member of the role but
// the last to join, each at both its addresses; and the last member's, which it does not reach.
func (self *pastBoundNoticeTeam) reached() (want []string, cut []string) {
	reachedMembers := self.members[:len(self.members)-1]
	want = noticeEmailsInOrder(append([]*keyNoticePerson{self.owner, self.lastOwner}, reachedMembers...)...)
	cut = self.members[len(self.members)-1].emails
	return
}

func requireNoticeCut(t testing.TB, recipients []string, cut []string) {
	t.Helper()
	for _, email := range cut {
		if slices.Contains(recipients, email) {
			t.Fatalf("the notice reaches %s of the member past the bound", email)
		}
	}
}

// Past its people bound a team's key and authority notices cut whole Admins, the last to join, never an Owner: a team
// of two Owners and one Admin fewer than the bound, each Admin signing in at two addresses, the second Owner joining
// after every Admin with an address sorting after everyone's, reaches both Owners and then each Admin at both its
// addresses but the last to join, which hears of nothing at either, though the notice reaches more than the bound's
// count of addresses. An auth key the Owner creates owes its notice to those addresses alone.
func TestTeamKeyNoticePastItsPeopleBoundCutsWholeAdminsNeverAnOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		team := newPastBoundNoticeTeam(t, ctx, NetworkMemberRoleAdmin)
		want, cut := team.reached()

		authority, _ := noticeRecipientsByRule(ctx, team.networkId)
		requireRecipientOrder(t, authority, want)
		requireNoticeCut(t, authority, cut)

		createTestAuthKey(t, team.owner.teamSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, server.NowUtc())
		requireKeyNoticeRecipients(t, ctx, team.networkId, testAuthKeyNoticeTemplate, SecurityAuthKeyCreated, 1, sortedNoticeEmails(want))
	})
}

// Past its people bound a team's payout notices cut whole Billing admins likewise: both Owners, the one that joined
// last included, and each Billing admin but the last to join, at both its addresses, hear of a payout wallet the
// Owner selects, and the last Billing admin hears of nothing.
func TestTeamPayoutNoticePastItsPeopleBoundCutsWholeBillingAdminsNeverAnOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		team := newPastBoundNoticeTeam(t, ctx, NetworkMemberRoleBillingAdmin)
		want, cut := team.reached()

		_, billing := noticeRecipientsByRule(ctx, team.networkId)
		requireRecipientOrder(t, billing, want)
		requireNoticeCut(t, billing, cut)

		address := setTestTeamPayoutWallet(t, team.keyNoticeTeam, team.owner)
		requireWalletNoticeRecipients(t, ctx, team.networkId, sortedNoticeEmails(want), walletNoticeChange{
			destination: SecurityWalletDestinationUsdc,
			newWallet:   testMasked(address),
		})
	})
}

// A personal network's notices are as they were: its owner, signing in at four addresses and the Owner of a team with
// eight Admins, hears of its own network's changes at all four, in sort order, by the account rule, the key and
// authority rule and the payout rule alike, one account's addresses under one account's bound; an auth key it creates
// there owes its notice to those four alone and the team nothing.
func TestPersonalNetworkNoticesStillReachItsOwnerInSortOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		owner := newFourAddressNoticePerson(t, ctx, "owner")
		team := newKeyNoticeTeam(t, ctx, owner)
		for i := range 8 {
			addNoticeTeamMember(t, ctx, team, newKeyNoticePerson(t, ctx, newKeyNoticeEmail(fmt.Sprintf("admin-%d", i))), NetworkMemberRoleAdmin)
		}
		personalNetworkId := owner.personalSession.ByJwt.NetworkId
		want := sortedNoticeEmails(owner.emails)

		var account []string
		server.Tx(ctx, func(tx server.PgTx) {
			account = securityNoticeRecipientsInTx(ctx, tx, personalNetworkId)
		})
		authority, billing := noticeRecipientsByRule(ctx, personalNetworkId)
		for _, got := range [][]string{account, authority, billing} {
			requireRecipientOrder(t, got, want)
		}

		createTestAuthKey(t, owner.personalSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, server.NowUtc())
		requireKeyNoticeRecipients(t, ctx, personalNetworkId, testAuthKeyNoticeTemplate, SecurityAuthKeyCreated, 1, want)
		if notices := unsentTestNotices(t, ctx, team.networkId, testAuthKeyNoticeTemplate); len(notices) != 0 {
			t.Fatalf("the team owes %d auth key notices for its Owner's own network: %s", len(notices), notices[0].templateJson)
		}
	})
}
