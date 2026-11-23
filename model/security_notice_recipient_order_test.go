package model

// The order and the bound of a team's security notice recipients (QD-1218; §7.8's notices row, R-TEAM-12, QD-702),
// with no database: securityNoticeMemberEmails over the rows the member query reads, in whatever order they come. A
// team's notice reaches every Owner first, at all its addresses, then its other members person by person in member_id
// order, up to the settings' TeamRecipientMemberMaxCount people. It used to keep the 8 addresses that sorted first, a
// bound made for one account's sign-ins, so in a team whose Owners and Admins held more than eight addresses between
// them an Owner whose addresses sorted after the Admins' heard of nothing. The tests on Postgres, with teams that
// team-create makes, are security_notice_recipient_order_team_test.go's.

import (
	"fmt"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

// A member as the member query reads it.
type recipientOrderMember struct {
	memberId  server.Id
	role      NetworkMemberRole
	userAuths []string
}

// A member that joins now, after every member made before it: server.NewId is strictly increasing.
func newRecipientOrderMember(role NetworkMemberRole, userAuths ...string) *recipientOrderMember {
	return &recipientOrderMember{memberId: server.NewId(), role: role, userAuths: userAuths}
}

// A synthetic address of these tests.
func recipientOrderEmail(label string) string {
	return label + "@recipient-order.example"
}

// The members' rows in an order the query might return them: each member's first sign-in, the member that joined last
// first, then each one's second, and so on, so no member's rows are together and member_id order is reversed.
func recipientOrderRows(members ...*recipientOrderMember) []*securityNoticeMemberUserAuth {
	userAuthMaxCount := 0
	for _, member := range members {
		userAuthMaxCount = max(userAuthMaxCount, len(member.userAuths))
	}
	rows := []*securityNoticeMemberUserAuth{}
	for i := range userAuthMaxCount {
		for _, member := range slices.Backward(members) {
			if i < len(member.userAuths) {
				rows = append(rows, &securityNoticeMemberUserAuth{
					memberId: member.memberId,
					role:     member.role,
					userAuth: member.userAuths[i],
				})
			}
		}
	}
	return rows
}

// The members' email addresses, each member's sorted, the members in the order given.
func recipientOrderEmails(members ...*recipientOrderMember) []string {
	emails := []string{}
	for _, member := range members {
		emails = append(emails, distinctSecurityNoticeEmails(member.userAuths)...)
	}
	return emails
}

// Settings with the team bound at memberMaxCount people.
func recipientOrderSettings(memberMaxCount int) *SecurityNoticeSettings {
	settings := DefaultSecurityNoticeSettings()
	settings.TeamRecipientMemberMaxCount = memberMaxCount
	return settings
}

// Holds that the notice reaches want, in its order.
func requireRecipientOrder(t testing.TB, got []string, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("the notice reaches\n  %v\nwant\n  %v", got, want)
	}
}

// A team of one Owner signing in at four addresses and eight Admins at one each, the Owner joining last and its
// addresses sorting after every Admin's, reaches the Owner at all four first and each Admin after it in the order they
// joined, not the order their addresses sort in: twelve addresses, where the cut at the 8 that sort first kept the
// Admins' alone and the Owner heard nothing.
func TestTeamNoticeReachesItsOwnerAtEveryAddressBeforeItsAdmins(t *testing.T) {
	admins := []*recipientOrderMember{}
	for i := range 8 {
		// the Admin that joins first sorts last among them
		admins = append(admins, newRecipientOrderMember(NetworkMemberRoleAdmin, recipientOrderEmail(fmt.Sprintf("admin-%d", 8-i))))
	}
	owner := newRecipientOrderMember(
		NetworkMemberRoleOwner,
		recipientOrderEmail("owner-legacy"),
		recipientOrderEmail("owner-email"),
		recipientOrderEmail("owner-apple"),
		recipientOrderEmail("owner-google"),
	)
	settings := DefaultSecurityNoticeSettings()
	members := append(slices.Clone(admins), owner)
	want := recipientOrderEmails(append([]*recipientOrderMember{owner}, admins...)...)
	if len(want) != 12 || len(want) <= settings.RecipientMaxCount {
		t.Fatalf("the team holds %d addresses, want 12, more than one account's bound of %d", len(want), settings.RecipientMaxCount)
	}
	// the cause: one account's bound, applied to the team, keeps the 8 that sort first, the Admins', and no Owner's
	for _, email := range securityNoticeEmails(recipientOrderEmails(members...)) {
		if slices.Contains(owner.userAuths, email) {
			t.Fatalf("one account's bound kept the Owner's %s; the test no longer holds the failing team", email)
		}
	}

	requireRecipientOrder(t, securityNoticeMemberEmails(recipientOrderRows(members...), settings), want)
}

// Past the people bound the cut takes whole members, those that joined last, and never an Owner while another member
// is reached: at a bound of 4, two Owners, one that joined first and one that joined last with addresses sorting after
// everyone's, and two Admins are reached, each at both its addresses, and the two Admins that joined after them hear
// nothing, not one address of either, though a cut by addresses would have reached some of them.
func TestTeamNoticePastItsPeopleBoundCutsWholeMembersNeverAnOwner(t *testing.T) {
	firstOwner := newRecipientOrderMember(NetworkMemberRoleOwner, recipientOrderEmail("owner-first"))
	firstAdmin := newRecipientOrderMember(NetworkMemberRoleAdmin, recipientOrderEmail("admin-a"), recipientOrderEmail("admin-a-google"))
	secondAdmin := newRecipientOrderMember(NetworkMemberRoleAdmin, recipientOrderEmail("admin-b"), recipientOrderEmail("admin-b-google"))
	cutAdmin := newRecipientOrderMember(NetworkMemberRoleAdmin, recipientOrderEmail("admin-c"), recipientOrderEmail("admin-c-google"))
	lastCutAdmin := newRecipientOrderMember(NetworkMemberRoleAdmin, recipientOrderEmail("admin-d"))
	lastOwner := newRecipientOrderMember(NetworkMemberRoleOwner, recipientOrderEmail("zz-owner-last"), recipientOrderEmail("zz-owner-last-google"))
	rows := recipientOrderRows(firstOwner, firstAdmin, secondAdmin, cutAdmin, lastCutAdmin, lastOwner)

	got := securityNoticeMemberEmails(rows, recipientOrderSettings(4))
	requireRecipientOrder(t, got, recipientOrderEmails(firstOwner, lastOwner, firstAdmin, secondAdmin))
	for _, email := range append(slices.Clone(cutAdmin.userAuths), lastCutAdmin.userAuths...) {
		if slices.Contains(got, email) {
			t.Fatalf("the notice reaches %s of a member past the bound", email)
		}
	}

	// one more place reaches the next Admin whole, at both its addresses
	requireRecipientOrder(
		t,
		securityNoticeMemberEmails(rows, recipientOrderSettings(5)),
		recipientOrderEmails(firstOwner, lastOwner, firstAdmin, secondAdmin, cutAdmin),
	)
}

// When the Owners alone are more than the bound, the Owners that joined first are reached, the one that joined last
// is cut, and no Admin is reached, the one that joined before every Owner included.
func TestTeamNoticeWithMoreOwnersThanItsBoundReachesTheFirstOwnersAlone(t *testing.T) {
	admin := newRecipientOrderMember(NetworkMemberRoleAdmin, recipientOrderEmail("admin"))
	firstOwner := newRecipientOrderMember(NetworkMemberRoleOwner, recipientOrderEmail("owner-c"))
	secondOwner := newRecipientOrderMember(NetworkMemberRoleOwner, recipientOrderEmail("owner-b"))
	lastOwner := newRecipientOrderMember(NetworkMemberRoleOwner, recipientOrderEmail("owner-a"))

	requireRecipientOrder(
		t,
		securityNoticeMemberEmails(recipientOrderRows(admin, firstOwner, secondOwner, lastOwner), recipientOrderSettings(2)),
		recipientOrderEmails(firstOwner, secondOwner),
	)
}

// The bound counts the people the notice reaches. An Owner that signs in with a phone number alone reaches nobody and
// takes no place; an Admin whose one address an Owner's already reaches, spelled with other case and spaces, takes
// none either, and the address is reached once; so at a bound of 2 the Owner with an email address and the next Admin
// with an address of its own are reached, and the Admin after them is cut. A wallet is no address.
func TestTeamNoticeCountsOnlyThePeopleItReaches(t *testing.T) {
	phoneOwner := newRecipientOrderMember(NetworkMemberRoleOwner, "+1 415-555-0168")
	owner := newRecipientOrderMember(NetworkMemberRoleOwner, recipientOrderEmail("owner"), "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM")
	sharingAdmin := newRecipientOrderMember(NetworkMemberRoleAdmin, " "+recipientOrderEmail("OWNER")+" ")
	admin := newRecipientOrderMember(NetworkMemberRoleAdmin, recipientOrderEmail("admin"))
	cutAdmin := newRecipientOrderMember(NetworkMemberRoleAdmin, recipientOrderEmail("cut-admin"))

	requireRecipientOrder(
		t,
		securityNoticeMemberEmails(recipientOrderRows(phoneOwner, owner, sharingAdmin, admin, cutAdmin), recipientOrderSettings(2)),
		[]string{recipientOrderEmail("owner"), recipientOrderEmail("admin")},
	)
}

// The owner's defaults (QD-1218): a team's notice reaches at most 50 people, and one account's at most 8 addresses,
// each bound a settings field of its own.
func TestSecurityNoticeRecipientBoundsDefaultToTheOwnersValues(t *testing.T) {
	settings := DefaultSecurityNoticeSettings()
	if settings.TeamRecipientMemberMaxCount != 50 || settings.RecipientMaxCount != 8 {
		t.Fatalf("a team's notice reaches %d people and an account's %d addresses, want 50 and 8", settings.TeamRecipientMemberMaxCount, settings.RecipientMaxCount)
	}
}

// A non-positive bound bounds nothing: every member is reached, the Owner first, where the default bound reaches the
// Owner and the Billing admins that joined first, as many people as it allows.
func TestTeamNoticeWithANonPositiveBoundReachesEveryMember(t *testing.T) {
	memberMaxCount := DefaultSecurityNoticeSettings().TeamRecipientMemberMaxCount
	billingAdmins := []*recipientOrderMember{}
	for i := range memberMaxCount + 10 {
		billingAdmins = append(billingAdmins, newRecipientOrderMember(NetworkMemberRoleBillingAdmin, recipientOrderEmail(fmt.Sprintf("billing-admin-%03d", i))))
	}
	owner := newRecipientOrderMember(NetworkMemberRoleOwner, recipientOrderEmail("owner"))
	rows := recipientOrderRows(append(slices.Clone(billingAdmins), owner)...)
	want := recipientOrderEmails(append([]*recipientOrderMember{owner}, billingAdmins...)...)
	for _, nonPositive := range []int{0, -1} {
		requireRecipientOrder(t, securityNoticeMemberEmails(rows, recipientOrderSettings(nonPositive)), want)
	}
	// each Billing admin signs in at one address, so the default's people are as many addresses
	requireRecipientOrder(t, securityNoticeMemberEmails(rows, DefaultSecurityNoticeSettings()), want[:memberMaxCount])
}
