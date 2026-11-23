//go:build sidecar_pending

package model

// A team's billing recipients read for many networks at once (QD-1217, QD-1219), on Postgres with MG0, MG1 and MG6:
// the one rule a team's payout and subnet wallet notices, its failed-payment notices and its missing-wallet notice
// share, read the way a payout slice reads it, for every wallet-less network of the slice in two queries. Each team
// here is made by team-create, with the people of the key notices' tests (security_notice_team_recipients_test.go).

import (
	"context"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

// Among two teams, a personal network and a network that is gone, read at once, only the teams have entries. A team's
// entry is its Owners first, each at all its sign-in addresses in sort order, then its Billing admin, though the
// Billing admin joined before the second Owner, and none of its Admin, its pending Billing admin or its owner of
// record. A team whose only Owner signs in by phone and whose Admin has an address has an entry, and it is empty, so a
// caller mails it nothing rather than taking it for a personal network. Each team's entry is the payout wallet notice's
// recipients.
func TestTeamBillingRecipientsOfManyNetworksAtOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()

		owner := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("billing-owner"))
		owner.addGoogleSignIn(t, ctx, newKeyNoticeEmail("billing-owner-google"))
		team := newKeyNoticeTeam(t, ctx, owner)
		billingAdmin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("billing-admin"))
		team.join(t, billingAdmin, NetworkMemberRoleBillingAdmin)
		secondOwner := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("billing-second-owner"))
		team.join(t, secondOwner, NetworkMemberRoleOwner)
		team.join(t, newKeyNoticePerson(t, ctx, newKeyNoticeEmail("billing-team-admin")), NetworkMemberRoleAdmin)
		pendingBillingAdmin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("billing-pending"))
		addTestSignInMember(t, ctx, team.networkId, pendingBillingAdmin.userId, NetworkMemberRoleBillingAdmin, true)

		phoneOwner := newKeyNoticePerson(t, ctx, "+1 415-555-0167")
		silentTeam := newKeyNoticeTeam(t, ctx, phoneOwner)
		silentTeam.join(t, newKeyNoticePerson(t, ctx, newKeyNoticeEmail("silent-team-admin")), NetworkMemberRoleAdmin)

		personalNetworkId := owner.personalSession.ByJwt.NetworkId
		goneNetworkId := server.NewId()

		var networkIdRecipients map[server.Id][]string
		var payoutWalletRecipients []string
		server.Tx(ctx, func(tx server.PgTx) {
			networkIdRecipients = GetTeamBillingRecipientsInTx(
				ctx,
				tx,
				[]server.Id{personalNetworkId, team.networkId, goneNetworkId, silentTeam.networkId},
			)
			payoutWalletRecipients = securityNoticeBillingRecipientsInTx(ctx, tx, team.networkId)
		})

		if len(networkIdRecipients) != 2 {
			t.Fatalf("entries for %d networks, want the two teams': %v", len(networkIdRecipients), networkIdRecipients)
		}
		ownerEmails := slices.Clone(owner.emails)
		slices.Sort(ownerEmails)
		want := append(append(ownerEmails, secondOwner.emails...), billingAdmin.emails...)
		if got := networkIdRecipients[team.networkId]; !slices.Equal(got, want) {
			t.Fatalf("the team's billing recipients are %v, want its Owners first and then its Billing admin, %v", got, want)
		}
		if !slices.Equal(payoutWalletRecipients, want) {
			t.Fatalf("the team's payout wallet notice goes to %v, want its billing recipients %v", payoutWalletRecipients, want)
		}
		silentRecipients, ok := networkIdRecipients[silentTeam.networkId]
		if !ok || len(silentRecipients) != 0 {
			t.Fatalf("the team whose Owner signs in by phone: %v (an entry: %t), want an entry and no recipient", silentRecipients, ok)
		}
		for _, networkId := range []server.Id{personalNetworkId, goneNetworkId} {
			if recipients, ok := networkIdRecipients[networkId]; ok {
				t.Fatalf("network %s, no team, has an entry %v", networkId, recipients)
			}
		}

		// nothing to read reads nothing
		server.Tx(ctx, func(tx server.PgTx) {
			if empty := GetTeamBillingRecipientsInTx(ctx, tx, []server.Id{}); len(empty) != 0 {
				t.Fatalf("no networks have entries %v", empty)
			}
		})
	})
}
