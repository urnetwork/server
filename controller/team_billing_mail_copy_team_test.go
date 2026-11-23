//go:build sidecar_pending

package controller

// A Team's billing and payout mail names the Team when it goes out (QD-1223), on Postgres with MG0, MG1 and MG6: the
// failed-payment and missing-wallet notices through the account message outbox, whose delivery reads the Team's name
// and whether the network is a Team, and the epoch earnings email and the USDC payout-sent notice, which read them
// with their recipients. Each Team is team_billing_mail_test.go's, made by team-create, and each of its Owners and its
// Billing admin has a personal network of its own, which no Team mail names. A personal network's mail beside it
// keeps its copy byte for byte. Before QD-1223 every one of these went to a Team's billing members in the copy written
// for a personal network's owner, about its own account and UR Pro.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The names of the personal networks of the Team's Owners and Billing admin, which none of the Team's mail names.
func (self *teamBillingMailTeam) billingPersonalNetworkNames() []string {
	return []string{
		self.owner.personal.ByJwt.NetworkName,
		self.secondOwner.personal.ByJwt.NetworkName,
		self.billingAdmin.personal.ByJwt.NetworkName,
	}
}

// Holds that the template goes out as the mail of the Team called teamName, naming it in its subject and both parts
// and saying none of the absent phrases, and returns the parts, the text on one line.
func requireTeamBillingMail(t testing.TB, template Template, teamName string, absent []string) (subject string, bodyHtml string, bodyText string) {
	t.Helper()
	namer, ok := template.(interface {
		Team() bool
		TeamNetwork() string
	})
	if !ok || !namer.Team() || namer.TeamNetwork() != billingMailCopyTeam(teamName) {
		t.Fatalf("%s goes out unnamed or misnamed, want named for the Team %s: %+v", template.Name(), teamName, template)
	}
	subject, bodyHtml, bodyText = renderBillingMailCopy(t, template)
	requireBillingMailCopySays(
		t,
		template.Name(),
		map[string]string{"subject": subject, "html": bodyHtml, "text": bodyText},
		[]string{billingMailCopyTeam(teamName)},
		absent,
	)
	return
}

// Holds that a stored mail's fields hold neither the Team's name nor whether the network is a Team.
func requireBillingMailFieldsHoldNoTeam(t testing.TB, templateName string, fields map[string]any, teamName string) {
	t.Helper()
	fieldsJson, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(fieldsJson), teamName) {
		t.Fatalf("%s: the outbox holds the Team's name: %s", templateName, fieldsJson)
	}
	for key := range fields {
		if lowerKey := strings.ToLower(key); strings.Contains(lowerKey, "team") || strings.Contains(lowerKey, "network") {
			t.Fatalf("%s: the outbox holds a field %s: %s", templateName, key, fieldsJson)
		}
	}
}

// A Team's failed renewal emails each of its Owners and its Billing admin a notice naming the Team as it is called
// when the notice goes out, renamed after the notice was owed: "The payment for your URnetwork Team <name> didn't go
// through", about the Team's subscription and never UR Pro, saying it went to the Team's owners and billing admins
// and telling the reader to switch to the Team before updating the payment method. The outbox holds neither the name
// nor whether the network is a Team. The first Owner's own network's failed-payment notice, sent in the same delivery,
// is a personal network's, byte for byte.
func TestTeamFailedPaymentEmailNamesTheTeamAsItIsCalledWhenSent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()
		periods := newTestTeamPeriods()
		team := newTeamBillingMailTeam(t, ctx)
		subscriptionId := payTeamBillingMailTeam(t, ctx, env, periods, team, "tbmc_failed")

		const invoiceId = "in_synthetic_tbmc_failed"
		failTeamBillingMailRenewal(t, env, periods, team, subscriptionId, invoiceId)
		billingAddresses := team.billingAddresses()
		owed := teamBillingMailMessages(t, ctx, team.networkId, teamBillingMailPaymentFailedTemplate)
		if got := teamBillingMailUserAuthsOf(owed); !slices.Equal(got, billingAddresses) {
			t.Fatalf("the failed renewal owes notices to %v, want %v", got, billingAddresses)
		}
		for _, message := range owed {
			requireBillingMailFieldsHoldNoTeam(t, teamBillingMailPaymentFailedTemplate, message.fields, team.name)
		}

		// renamed while its notices wait
		renamed := "tbmc-renamed-" + teamBillingMailSuffix()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET network_name = $2 WHERE network_id = $1`, team.networkId, renamed))
		})
		// the first Owner's own network owes it a failed-payment notice of its own
		ownNetworkId := team.owner.personal.ByJwt.NetworkId
		server.Tx(ctx, func(tx server.PgTx) {
			if !addAccountMessageInTx(ctx, tx, &accountMessage{
				key:       "synthetic-own-renewal:" + server.NewId().String(),
				networkId: &ownNetworkId,
				userAuth:  team.owner.addresses[0],
				template:  &SubscriptionPaymentFailedTemplate{AmountDue: 500, Currency: "usd"},
			}) {
				t.Fatal("the Owner's own notice was not added")
			}
		})

		named := billingMailCopyTeam(renamed)
		absent := append([]string{team.name, "UR Pro", "your URnetwork account", "your payment method"}, team.billingPersonalNetworkNames()...)
		teamAddresses := []string{}
		ownCount := 0
		for _, send := range deliverTeamBillingMail(ctx, teamBillingMailPaymentFailedTemplate) {
			template := send.template.(*SubscriptionPaymentFailedTemplate)
			if !template.Team() {
				ownCount += 1
				if send.userAuth != team.owner.addresses[0] {
					t.Fatalf("a personal network's notice went to %s", send.userAuth)
				}
				requirePersonalBillingMailCopy(t, template)
				continue
			}
			teamAddresses = append(teamAddresses, send.userAuth)
			subject, bodyHtml, bodyText := requireTeamBillingMail(t, template, renamed, absent)
			if want := "The payment for " + named + " didn't go through"; subject != want {
				t.Fatalf("the notice to %s: subject %q, want %q", send.userAuth, subject, want)
			}
			requireBillingMailCopySays(t, template.Name(), map[string]string{"html": bodyHtml, "text": bodyText}, []string{
				"We tried to renew the subscription of " + named + ", but the payment of $142.93 didn't go through.",
				"To keep the Team's subscription",
				"Sent to the sign-in email addresses of the owners and billing admins of " + named,
				"sign in at ur.io and switch to " + named + " first, since Manage Subscription opens the billing of the network you're signed in to",
			}, nil)
		}
		slices.Sort(teamAddresses)
		if !slices.Equal(teamAddresses, billingAddresses) {
			t.Fatalf("the Team's notices went to %v, want %v", teamAddresses, billingAddresses)
		}
		if ownCount != 1 {
			t.Fatalf("the Owner's own network's notice went out %d times, want once", ownCount)
		}
	})
}

// A Team's withheld payout notifies each of its Owners and its Billing admin naming the Team: its payout is withheld,
// the reader is to connect the wallet signed in to the Team, and the notice went to the Team's owners and billing
// admins. The outbox holds neither the name nor whether the network is a Team, held or released. A personal network's
// notice in the same run is a personal network's, byte for byte.
func TestTeamMissingWalletEmailSentNamesTheTeam(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		missingWalletTestRequestsOn(t)
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		team := newTeamBillingMailTeam(t, ctx)
		provider := newTeamBillingMailPerson(t, ctx, "provider")
		providerNetworkId := provider.personal.ByJwt.NetworkId

		server.Tx(ctx, func(tx server.PgTx) {
			holdMissingWalletNoticesInTx(ctx, tx, missingWalletTestPlan(map[server.Id]model.NanoCents{
				team.networkId:    model.UsdToNanoCents(2),
				providerNetworkId: model.UsdToNanoCents(1.25),
			}, nil))
		})
		for _, message := range teamBillingMailMessages(t, ctx, team.networkId, teamBillingMailMissingWalletTemplate) {
			requireBillingMailFieldsHoldNoTeam(t, teamBillingMailMissingWalletTemplate, message.fields, team.name)
		}
		releaseMissingWalletNotices(ctx, server.NowUtc(), missingWalletTestMinPayout(model.UsdToNanoCents(1)))
		for _, message := range teamBillingMailMessages(t, ctx, team.networkId, teamBillingMailMissingWalletTemplate) {
			requireBillingMailFieldsHoldNoTeam(t, teamBillingMailMissingWalletTemplate, message.fields, team.name)
		}

		named := billingMailCopyTeam(team.name)
		absent := append([]string{"You earned", "on your account", "while you have new"}, team.billingPersonalNetworkNames()...)
		teamAddresses := []string{}
		personalCount := 0
		for _, send := range deliverTeamBillingMail(ctx, teamBillingMailMissingWalletTemplate) {
			template := send.template.(*MissingWalletTemplate)
			if !template.Team() {
				personalCount += 1
				if send.userAuth != provider.addresses[0] {
					t.Fatalf("a personal network's notice went to %s", send.userAuth)
				}
				requirePersonalBillingMailCopy(t, template)
				continue
			}
			teamAddresses = append(teamAddresses, send.userAuth)
			subject, bodyHtml, bodyText := requireTeamBillingMail(t, template, team.name, absent)
			if want := "Connect a Solana wallet to " + named + " to receive 2.00 USDC"; subject != want {
				t.Fatalf("the notice to %s: subject %q, want %q", send.userAuth, subject, want)
			}
			requireBillingMailCopySays(t, template.Name(), map[string]string{"html": bodyHtml, "text": bodyText}, []string{
				"The payout of " + named + " is withheld: the Team earned 2.00 USDC providing on URnetwork",
				"sign in at ur.io and switch to " + named + " first, since a wallet is connected to the network you're signed in to",
				"Sent to the sign-in email addresses of the owners and billing admins of " + named,
			}, nil)
		}
		slices.Sort(teamAddresses)
		if !slices.Equal(teamAddresses, team.billingAddresses()) {
			t.Fatalf("the Team's notices went to %v, want %v", teamAddresses, team.billingAddresses())
		}
		if personalCount != 1 {
			t.Fatalf("the personal network's notice went out %d times, want once", personalCount)
		}
	})
}

// A Team's epoch earnings email names the Team to each of its Owners and its Billing admin: "In epoch 7, your
// URnetwork Team <name> earned ...", the Team's earnings and the claim, and the switch to the Team before claiming,
// never your network. A personal network's email in the same epoch is a personal network's, byte for byte.
func TestTeamEpochEarningsEmailSentNamesTheTeam(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newEpochEarningsTestFixture(t)
		ctx := f.ctx
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		team := newTeamBillingMailTeam(t, ctx)
		person := newTeamBillingMailPerson(t, ctx, "personal")
		f.finalize(7, map[server.Id]int{
			team.networkId:                  6000,
			person.personal.ByJwt.NetworkId: 4000,
		})

		sender := &epochEarningsTestSender{}
		if sent, skipped := f.send(7, sender); sent != len(team.billingAddresses())+1 || skipped != 0 {
			t.Fatalf("sent %d and skipped %d, want one email for each of the Team's addresses and the personal network's", sent, skipped)
		}
		named := billingMailCopyTeam(team.name)
		absent := append([]string{"your network", "Your network", "you qualify", "your points", "your SN25α", "your next epoch"}, team.billingPersonalNetworkNames()...)
		teamAddresses := []string{}
		personalCount := 0
		for _, send := range sender.taken() {
			if !send.template.Team() {
				personalCount += 1
				if send.userAuth != person.addresses[0] {
					t.Fatalf("a personal network's email went to %s", send.userAuth)
				}
				requirePersonalBillingMailCopy(t, send.template)
				continue
			}
			teamAddresses = append(teamAddresses, send.userAuth)
			subject, bodyHtml, bodyText := requireTeamBillingMail(t, send.template, team.name, absent)
			if want := fmt.Sprintf("In epoch 7, %s earned %s points", named, send.template.PointsText()); subject != want {
				t.Fatalf("the email to %s: subject %q, want %q", send.userAuth, subject, want)
			}
			requireBillingMailCopySays(t, send.template.Name(), map[string]string{"html": bodyHtml, "text": bodyText}, []string{
				"Earnings of " + named + " for providing on URnetwork in epoch 7",
				"Sent to the sign-in email addresses of the owners and billing admins of " + named + " because the Team earned on URnetwork in epoch 7.",
				"These pages act on the network you're signed in to: sign in at ur.io and switch to " + named + " first.",
			}, nil)
		}
		slices.Sort(teamAddresses)
		if !slices.Equal(teamAddresses, team.billingAddresses()) {
			t.Fatalf("the Team's emails went to %v, want %v", teamAddresses, team.billingAddresses())
		}
		if personalCount != 1 {
			t.Fatalf("the personal network's email went out %d times, want once", personalCount)
		}
	})
}

// `payouts usdc-sent-notice` sends a Team's notice naming the Team to each of its Owners and its Billing admin: "The
// 428.86 USDC payout of your URnetwork Team <name> was sent", to the Team's wallet, and the switch to the Team before
// claiming, never your network; the journal holds no name. A personal network's notice listed with it is a personal
// network's, byte for byte.
func TestTeamUsdcPayoutSentNoticeOfTheCommandNamesTheTeam(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newUsdcSentNoticeTestFixture(t)
		ctx := f.ctx
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		team := newTeamBillingMailTeam(t, ctx)
		person := newTeamBillingMailPerson(t, ctx, "personal")
		teamPaymentId := f.completedPaymentOf(team.networkId, model.UsdToNanoCents(428.870284))
		personalPaymentId := f.completedPaymentOf(person.personal.ByJwt.NetworkId, model.UsdToNanoCents(29.951303))

		f.run(true, teamPaymentId, personalPaymentId)
		named := billingMailCopyTeam(team.name)
		absent := append([]string{"your network", "Your network", "this address administers", "Your earnings"}, team.billingPersonalNetworkNames()...)
		teamAddresses := []string{}
		personalCount := 0
		for _, send := range f.sender.taken() {
			if !send.template.Team() {
				personalCount += 1
				if send.userAuth != person.addresses[0] {
					t.Fatalf("a personal network's notice went to %s", send.userAuth)
				}
				requirePersonalBillingMailCopy(t, send.template)
				continue
			}
			teamAddresses = append(teamAddresses, send.userAuth)
			subject, bodyHtml, bodyText := requireTeamBillingMail(t, send.template, team.name, absent)
			if want := "The 428.86 USDC payout of " + named + " was sent"; subject != want {
				t.Fatalf("the notice to %s: subject %q, want %q", send.userAuth, subject, want)
			}
			requireBillingMailCopySays(t, send.template.Name(), map[string]string{"html": bodyHtml, "text": bodyText}, []string{
				"We sent 428.86 USDC to the Solana wallet of " + named + ", ending in WxYz, on",
				"Sent to the sign-in email addresses of the owners and billing admins of " + named + ", which was owed a USDC payout.",
				"The claim acts on the network you're signed in to: sign in at ur.io and switch to " + named + " first.",
			}, nil)
		}
		slices.Sort(teamAddresses)
		if !slices.Equal(teamAddresses, team.billingAddresses()) {
			t.Fatalf("the Team's notices went to %v, want %v", teamAddresses, team.billingAddresses())
		}
		if personalCount != 1 {
			t.Fatalf("the personal network's notice went out %d times, want once", personalCount)
		}
		for _, journal := range teamEarningsMailJournalOf(t, f, teamPaymentId) {
			eventDetails, err := json.Marshal(journal.eventDetails)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(eventDetails), team.name) {
				t.Fatalf("the journal holds the Team's name: %s", eventDetails)
			}
		}
	})
}
