//go:build sidecar_pending

package controller

// A Team's epoch earnings email reaches its billing recipients, its Owners and Billing admins (QD-1222: QD-1219's rule
// carried to the mail about a network's earnings, in QD-1218's order and bound), on Postgres with MG0, MG1 and MG6. It
// found its recipient through the network's admin user, which in a Team is the synthetic owner of record team-create
// writes with no sign-in (D-TEAM-2), so a Team's epoch earnings were never emailed, the run counting it skipped. Each
// Team here is team_billing_mail_test.go's: two Owners, a Billing admin, a person in every other role and a pending
// Billing admin, each signing in at synthetic addresses of its own. A personal network's mail is unchanged.

import (
	"slices"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The Team's billing recipients' addresses in the order its mail reaches them: its Owners first, its first Owner,
// which made the Team, at both its addresses in sort order, then the second Owner, then the Billing admin, who joined
// before the second Owner.
func teamEarningsMailBillingOrder(team *teamBillingMailTeam) []string {
	addresses := slices.Clone(team.owner.addresses)
	slices.Sort(addresses)
	addresses = append(addresses, team.secondOwner.addresses...)
	return append(addresses, team.billingAdmin.addresses...)
}

// A Team's epoch earnings email reaches each of its Owners and its Billing admin, at each of their sign-in addresses,
// the Owners first, one email each with the Team's share and alpha; and nobody else: not its Admin, Network admin, IT
// admin, Auditor or Member, which hold no billing or payouts, nor its pending Billing admin. Before QD-1222 the Team's
// earnings were emailed to nobody and counted skipped. A personal network earning in the same epoch is emailed at its
// admin's sign-in alone, as it always was, though its owner also signs in with Google.
func TestTeamEpochEarningsReachItsOwnersAndBillingAdmins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newEpochEarningsTestFixture(t)
		ctx := f.ctx
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		team := newTeamBillingMailTeam(t, ctx)
		person := newTeamBillingMailPerson(t, ctx, "personal")
		person.addGoogleSignIn(t, ctx, "personal-google")
		personalNetworkId := person.personal.ByJwt.NetworkId
		f.finalize(7, map[server.Id]int{
			team.networkId:    6000,
			personalNetworkId: 4000,
		})

		sender := &epochEarningsTestSender{}
		sent, skipped := f.send(7, sender)
		billingOrder := teamEarningsMailBillingOrder(team)
		if sent != len(billingOrder)+1 || skipped != 0 {
			t.Fatalf("sent %d and skipped %d, want one email for each of the Team's %d addresses and the personal network's one, none skipped", sent, skipped, len(billingOrder))
		}
		teamAddresses := []string{}
		personalAddresses := []string{}
		for _, send := range sender.taken() {
			switch {
			case slices.Contains(billingOrder, send.userAuth):
				teamAddresses = append(teamAddresses, send.userAuth)
				if send.template.Epoch != 7 || send.template.ShareBps != 6000 || send.template.UnclaimedRao.Cmp(epochEarningsTestAlpha(6000)) != 0 {
					t.Fatalf("the Team's email to %s: epoch %d, share %d, alpha %s; want the Team's share of epoch 7", send.userAuth, send.template.Epoch, send.template.ShareBps, send.template.UnclaimedRao)
				}
			case send.userAuth == person.addresses[0]:
				personalAddresses = append(personalAddresses, send.userAuth)
				if send.template.ShareBps != 4000 {
					t.Fatalf("the personal network's email has share %d, want its own 4000", send.template.ShareBps)
				}
			case slices.Contains(team.otherAddresses(), send.userAuth):
				t.Fatalf("a member whose role holds no billing was emailed the Team's earnings at %s", send.userAuth)
			default:
				t.Fatalf("an email went to %s, which is neither the Team's billing recipients' nor the personal network's admin's", send.userAuth)
			}
		}
		if !slices.Equal(teamAddresses, billingOrder) {
			t.Fatalf("the Team's earnings went to %v, want each Owner's and Billing admin's address once, Owners first: %v", teamAddresses, billingOrder)
		}
		if !slices.Equal(personalAddresses, person.addresses[:1]) {
			t.Fatalf("the personal network's earnings went to %v, want its admin's sign-in alone, %s", personalAddresses, person.addresses[0])
		}
	})
}
