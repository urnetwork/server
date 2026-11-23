//go:build sidecar_pending

package controller

// A Team's failed-payment (dunning) notices and its missing-wallet payout notice reach its billing recipients, its
// Owners and Billing admins (QD-1219: QD-1217's rule for its wallet security notices, in QD-1218's order and bound), on
// Postgres with MG0, MG1 and MG6. Both mails went to the network's admin user by network.admin_user_id, which in a
// Team is the synthetic owner of record team-create writes with no sign-in (D-TEAM-2), so both found no address: the
// dunning code took the empty address for an account with no email and owed the in-app notice alone, and the
// missing-wallet notice was skipped. So a Team's failed renewal, which ends its credit line at once (§20.9), emailed
// nobody, and its withheld payout was never mentioned. Each Team here is made by team-create, so its owner of record
// is as a real Team's is, and each person signs in at synthetic addresses of its own. A personal network's mail is
// unchanged: one notice to its account's address, keyed as it always was.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The template names of the two mails, as strings, so the tests read the same before and after a rename.
const (
	teamBillingMailPaymentFailedTemplate = "subscription_payment_failed"
	teamBillingMailMissingWalletTemplate = "subscription_missing_wallet"
)

// Eight random hex characters, unique to the call.
func teamBillingMailSuffix() string {
	return strings.ReplaceAll(server.NewId().String(), "-", "")[24:32]
}

// A synthetic sign-in address of the test, unique to it, normalized.
func newTeamBillingMailAddress(t testing.TB, label string) string {
	t.Helper()
	address, userAuthType := model.NormalUserAuth(fmt.Sprintf("%s-%s@team-billing-mail.example", label, teamBillingMailSuffix()))
	if userAuthType != model.UserAuthTypeEmail {
		t.Fatalf("%q is no email address", address)
	}
	return address
}

// A person as a signup leaves one: a network it owns and verified sign-ins at email addresses of its own.
type teamBillingMailPerson struct {
	userId server.Id
	// its sign-in email addresses, normalized, its own sign-in's first
	addresses []string
	// a session of its own network
	personal *session.ClientSession
}

// A new person signing in at one address, its own network's account email.
func newTeamBillingMailPerson(t testing.TB, ctx context.Context, label string) *teamBillingMailPerson {
	t.Helper()
	userId := server.NewId()
	networkId := server.NewId()
	networkName := "tbm-" + teamBillingMailSuffix() + teamBillingMailSuffix()
	address := newTeamBillingMailAddress(t, label)
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
			model.AuthTypePassword,
			address,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_user_auth_password (user_id, user_auth, auth_type, verified) VALUES ($1, $2, 'email', true)`,
			userId,
			address,
		))
	})
	personal := session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId:   networkId,
		UserId:      userId,
		NetworkName: networkName,
	})
	t.Cleanup(personal.Cancel)
	return &teamBillingMailPerson{
		userId:    userId,
		addresses: []string{address},
		personal:  personal,
	}
}

// Adds a Google sign-in at another address.
func (self *teamBillingMailPerson) addGoogleSignIn(t testing.TB, ctx context.Context, label string) {
	t.Helper()
	address := newTeamBillingMailAddress(t, label)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_user_auth_sso (user_id, auth_type, auth_jwt, user_auth) VALUES ($1, $2, 'synthetic', $3)`,
			self.userId,
			model.AuthTypeGoogle,
			address,
		))
	})
	self.addresses = append(self.addresses, address)
}

// The sign-in addresses of the people, sorted.
func teamBillingMailAddressesOf(people ...*teamBillingMailPerson) []string {
	addresses := []string{}
	for _, person := range people {
		addresses = append(addresses, person.addresses...)
	}
	slices.Sort(addresses)
	return addresses
}

// A Team made by team-create, with a person in every role and a pending Billing admin, each signing in at addresses of
// its own: its first Owner at two, the rest at one.
type teamBillingMailTeam struct {
	networkId server.Id
	// a session of its first Owner's sign-in to it
	ownerSession *session.ClientSession
	owner        *teamBillingMailPerson
	secondOwner  *teamBillingMailPerson
	billingAdmin *teamBillingMailPerson
	// the Billing admin's membership
	billingAdminMemberId server.Id
	// the people whose roles hold no billing, the Admin's first, and the pending Billing admin
	others []*teamBillingMailPerson
}

// A new Team. The Billing admin joins before the second Owner, so the Owners' coming first is the rule's and not the
// order they joined in.
func newTeamBillingMailTeam(t testing.TB, ctx context.Context) *teamBillingMailTeam {
	t.Helper()
	owner := newTeamBillingMailPerson(t, ctx, "owner")
	owner.addGoogleSignIn(t, ctx, "owner-google")
	teamName := "tbm-team-" + teamBillingMailSuffix()
	created, err := model.CreateNetworkTeam(&model.CreateNetworkTeamArgs{NetworkName: teamName}, owner.personal)
	if err != nil || created.Error != nil || created.NetworkId == nil || created.MemberId == nil {
		t.Fatalf("team-create: %v %+v", err, created)
	}
	ownerSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId:   *created.NetworkId,
		UserId:      owner.userId,
		NetworkName: teamName,
		MemberId:    created.MemberId,
	})
	t.Cleanup(ownerSession.Cancel)
	team := &teamBillingMailTeam{
		networkId:    *created.NetworkId,
		ownerSession: ownerSession,
		owner:        owner,
	}
	join := func(label string, role model.NetworkMemberRole, pending bool) (*teamBillingMailPerson, server.Id) {
		person := newTeamBillingMailPerson(t, ctx, label)
		var member *model.NetworkMember
		server.Tx(ctx, func(tx server.PgTx) {
			var err error
			member, err = model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{
				NetworkId: team.networkId,
				UserId:    person.userId,
				Role:      role,
				Pending:   pending,
			})
			server.Raise(err)
		})
		return person, member.MemberId
	}
	team.billingAdmin, team.billingAdminMemberId = join("billing-admin", model.NetworkMemberRoleBillingAdmin, false)
	team.secondOwner, _ = join("second-owner", model.NetworkMemberRoleOwner, false)
	for _, c := range []struct {
		label string
		role  model.NetworkMemberRole
	}{
		{label: "admin", role: model.NetworkMemberRoleAdmin},
		{label: "network-admin", role: model.NetworkMemberRoleNetworkAdmin},
		{label: "it-admin", role: model.NetworkMemberRoleItAdmin},
		{label: "auditor", role: model.NetworkMemberRoleAuditor},
		{label: "member", role: model.NetworkMemberRoleMember},
	} {
		person, _ := join(c.label, c.role, false)
		team.others = append(team.others, person)
	}
	pendingBillingAdmin, _ := join("pending-billing-admin", model.NetworkMemberRoleBillingAdmin, true)
	team.others = append(team.others, pendingBillingAdmin)
	return team
}

// The addresses of its Owners and its Billing admin, sorted.
func (self *teamBillingMailTeam) billingAddresses() []string {
	return teamBillingMailAddressesOf(self.owner, self.secondOwner, self.billingAdmin)
}

// The addresses of everyone else in it, sorted.
func (self *teamBillingMailTeam) otherAddresses() []string {
	return teamBillingMailAddressesOf(self.others...)
}

// Ends the Billing admin's membership as a removal does.
func (self *teamBillingMailTeam) removeBillingAdmin(t testing.TB, ctx context.Context) {
	t.Helper()
	requestId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := model.DenyNetworkMemberInTx(ctx, tx, &model.NetworkMemberDenial{
			NetworkId: self.networkId,
			MemberId:  self.billingAdminMemberId,
			Time:      server.NowUtc(),
			Event: &model.AdminEvent{
				NetworkId:  self.networkId,
				ActorKind:  model.AdminEventActorSystem,
				Action:     model.AdminEventActionMemberRemove,
				TargetKind: model.AdminEventTargetMember,
				TargetId:   &self.billingAdminMemberId,
				Result:     model.AdminEventResultOk,
				RequestId:  &requestId,
			},
		})
		server.Raise(err)
	})
}

// Makes the Team's Stripe subscription live, as the payment of its first invoice does, and returns it.
func payTeamBillingMailTeam(
	t testing.TB,
	ctx context.Context,
	env *teamStripeTestEnv,
	periods *testTeamPeriods,
	team *teamBillingMailTeam,
	name string,
) (subscriptionId string) {
	t.Helper()
	subscriptionId = "sub_synthetic_" + name
	env.setSubscription(subscriptionId, team.networkId, true, "active")
	env.setInvoice(
		"in_synthetic_first_"+name, subscriptionId, "paid", "subscription_create", 139,
		testTeamInvoiceLine(subscriptionId, testTeamPriceBase, 139, periods.firstStart, periods.anchor),
	)
	restore := testSetStripeDunningNow(periods.firstStart.Add(time.Minute))
	env.deliverInvoiceEvent(t, team.ownerSession, "invoice.paid", "in_synthetic_first_"+name)
	restore()
	if live := model.GetLiveTeamSubscription(ctx, team.networkId); live == nil || live.SubscriptionId != subscriptionId {
		t.Fatalf("the Team's live subscription is %+v, want %s", live, subscriptionId)
	}
	return
}

// Fails the Team's renewal: its cycle invoice open after a failed charge, the subscription past due, and
// invoice.payment_failed delivered.
func failTeamBillingMailRenewal(t testing.TB, env *teamStripeTestEnv, periods *testTeamPeriods, team *teamBillingMailTeam, subscriptionId string, invoiceId string) {
	t.Helper()
	setTestTeamCycleInvoice(env, periods, subscriptionId, invoiceId, "open")
	env.setSubscriptionStatus(subscriptionId, "past_due")
	env.deliverInvoiceEvent(t, team.ownerSession, "invoice.payment_failed", invoiceId)
}

// Sets the invoice's failed attempts as Stripe counts them, its next attempt kept.
func setTeamBillingMailInvoiceAttempts(env *teamStripeTestEnv, invoiceId string, attemptCount int) {
	env.stateLock.Lock()
	defer env.stateLock.Unlock()
	env.invoices[invoiceId]["attempt_count"] = attemptCount
}

// One message of a template that the outbox holds for a network.
type teamBillingMailMessage struct {
	userAuth  string
	held      bool
	sent      bool
	abandoned bool
	lastError string
	// the template's fields
	fields map[string]any
}

// The template's messages that the outbox holds for the network, by address.
func teamBillingMailMessages(t testing.TB, ctx context.Context, networkId server.Id, templateName string) []*teamBillingMailMessage {
	t.Helper()
	messages := []*teamBillingMailMessage{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					user_auth,
					template_json::text,
					deliver_time IS NULL,
					sent_time IS NOT NULL,
					abandon_time IS NOT NULL,
					COALESCE(last_error, '')
				FROM account_message_outbox
				WHERE network_id = $1 AND template_name = $2
				ORDER BY user_auth, create_time, message_id
			`,
			networkId,
			templateName,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				message := &teamBillingMailMessage{}
				var templateJson string
				server.Raise(result.Scan(&message.userAuth, &templateJson, &message.held, &message.sent, &message.abandoned, &message.lastError))
				server.Raise(json.Unmarshal([]byte(templateJson), &message.fields))
				messages = append(messages, message)
			}
		})
	})
	return messages
}

// The addresses of the messages, sorted.
func teamBillingMailUserAuthsOf(messages []*teamBillingMailMessage) []string {
	userAuths := []string{}
	for _, message := range messages {
		userAuths = append(userAuths, message.userAuth)
	}
	slices.Sort(userAuths)
	return userAuths
}

// Delivers what is due a minute from now and returns the template's sends, by address.
func deliverTeamBillingMail(ctx context.Context, templateName string) []outboxTestSend {
	sender := newOutboxTestSender()
	deliverAccountMessagesAt(ctx, sender, server.NowUtc().Add(time.Minute))
	sends := []outboxTestSend{}
	for _, send := range sender.sent() {
		if send.template.Name() == templateName {
			sends = append(sends, send)
		}
	}
	slices.SortFunc(sends, func(a outboxTestSend, b outboxTestSend) int {
		return strings.Compare(a.userAuth, b.userAuth)
	})
	return sends
}

// The addresses of the sends, sorted.
func teamBillingMailSendUserAuthsOf(sends []outboxTestSend) []string {
	userAuths := []string{}
	for _, send := range sends {
		userAuths = append(userAuths, send.userAuth)
	}
	slices.Sort(userAuths)
	return userAuths
}

// A Team's failed renewal emails each of its Owners and its Billing admin, at each of their sign-in addresses, the
// first Owner at both of its, one notice each, and nobody else: not its Admin, Network admin, IT admin, Auditor or
// Member, which hold no billing, nor its pending Billing admin, nor its owner of record, the one recipient the notice
// used to have, which has no address. Each notice is keyed by the invoice, the notice and its address. Stripe's
// redeliveries of the event owe nothing more. Before QD-1219 the renewal emailed nobody.
func TestTeamFailedRenewalEmailsItsOwnersAndBillingAdmins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()
		periods := newTestTeamPeriods()
		team := newTeamBillingMailTeam(t, ctx)
		subscriptionId := payTeamBillingMailTeam(t, ctx, env, periods, team, "tbm_failed")

		const invoiceId = "in_synthetic_tbm_failed"
		failTeamBillingMailRenewal(t, env, periods, team, subscriptionId, invoiceId)
		for range 2 {
			env.deliverInvoiceEvent(t, team.ownerSession, "invoice.payment_failed", invoiceId)
		}

		failure := model.GetStripeInvoicePaymentFailure(ctx, invoiceId)
		if failure == nil || failure.NetworkId != team.networkId || failure.ResolveTime != nil || failure.FirstNoticeTime == nil {
			t.Fatalf("the failed renewal is on file as %+v, want open with its first notice owed", failure)
		}
		billingAddresses := team.billingAddresses()
		messages := teamBillingMailMessages(t, ctx, team.networkId, teamBillingMailPaymentFailedTemplate)
		if got := teamBillingMailUserAuthsOf(messages); !slices.Equal(got, billingAddresses) {
			t.Fatalf("the failed renewal owes notices to %v, want one each to the Owners and the Billing admin, %v", got, billingAddresses)
		}
		for _, address := range billingAddresses {
			key := stripePaymentFailedTeamNoticeKey(invoiceId, stripePaymentFailedNoticeFirst, address)
			if message := model.GetAccountMessageByKey(ctx, teamBillingMailPaymentFailedTemplate, key); message == nil || message.UserAuth != address {
				t.Fatalf("the notice to %s is not keyed by the invoice, the notice and the address: %+v", address, message)
			}
		}
		if message := model.GetAccountMessageByKey(ctx, teamBillingMailPaymentFailedTemplate, stripePaymentFailedNoticeKey(invoiceId, stripePaymentFailedNoticeFirst)); message != nil {
			t.Fatalf("a Team owes a notice under a personal network's key, to %s", message.UserAuth)
		}

		sends := deliverTeamBillingMail(ctx, teamBillingMailPaymentFailedTemplate)
		if got := teamBillingMailSendUserAuthsOf(sends); !slices.Equal(got, billingAddresses) {
			t.Fatalf("the failed renewal emailed %v, want %v", got, billingAddresses)
		}
		for _, send := range sends {
			if slices.Contains(team.otherAddresses(), send.userAuth) {
				t.Fatalf("a member whose role holds no billing was emailed at %s", send.userAuth)
			}
			subject, _, _, err := RenderEmailTemplate(send.template)
			if err != nil {
				t.Fatal(err)
			}
			if subject != "Your URnetwork payment didn't go through" {
				t.Fatalf("the notice to %s: subject %q, want the first notice's", send.userAuth, subject)
			}
			if template, ok := send.template.(*SubscriptionPaymentFailedTemplate); !ok || template.Final || template.Amount() != "$142.93" || template.HostedInvoiceUrl == "" {
				t.Fatalf("the notice to %s: %+v, want the first notice of $142.93 with the invoice's page", send.userAuth, send.template)
			}
		}
		if sends := deliverTeamBillingMail(ctx, teamBillingMailPaymentFailedTemplate); len(sends) != 0 {
			t.Fatalf("a second delivery sent %d notices more", len(sends))
		}
	})
}

// Paying the invoice withdraws each recipient's unsent notice, the notice of a Billing admin that left the Team after
// it was owed among them, so none of them is sent after the payment; each keeps the reason it was withdrawn.
func TestTeamPaidInvoiceWithdrawsEveryRecipientsNotice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()
		periods := newTestTeamPeriods()
		team := newTeamBillingMailTeam(t, ctx)
		subscriptionId := payTeamBillingMailTeam(t, ctx, env, periods, team, "tbm_paid")

		const invoiceId = "in_synthetic_tbm_paid"
		failTeamBillingMailRenewal(t, env, periods, team, subscriptionId, invoiceId)
		billingAddresses := team.billingAddresses()
		if got := teamBillingMailUserAuthsOf(teamBillingMailMessages(t, ctx, team.networkId, teamBillingMailPaymentFailedTemplate)); !slices.Equal(got, billingAddresses) {
			t.Fatalf("the failed renewal owes notices to %v, want %v", got, billingAddresses)
		}

		// the Billing admin leaves before the invoice is paid: its notice is still owed, to an address that is no
		// longer a billing recipient's
		team.removeBillingAdmin(t, ctx)

		setTestTeamCycleInvoice(env, periods, subscriptionId, invoiceId, "paid")
		env.setSubscriptionStatus(subscriptionId, "active")
		env.deliverInvoiceEvent(t, team.ownerSession, "invoice.paid", invoiceId)

		if failure := model.GetStripeInvoicePaymentFailure(ctx, invoiceId); failure == nil || failure.ResolveTime == nil || failure.ResolveReason != model.StripeInvoicePaymentFailureResolvePaid {
			t.Fatalf("the paid invoice's failure is %+v, want resolved as paid", failure)
		}
		messages := teamBillingMailMessages(t, ctx, team.networkId, teamBillingMailPaymentFailedTemplate)
		if got := teamBillingMailUserAuthsOf(messages); !slices.Equal(got, billingAddresses) {
			t.Fatalf("the outbox holds notices to %v, want the four owed, %v", got, billingAddresses)
		}
		for _, message := range messages {
			if message.sent || !message.abandoned || message.lastError != "withdrawn: the failed invoice is paid" {
				t.Fatalf("the notice to %s after the payment: sent %t, abandoned %t, %q; want withdrawn as paid", message.userAuth, message.sent, message.abandoned, message.lastError)
			}
		}
		if sends := deliverTeamBillingMail(ctx, teamBillingMailPaymentFailedTemplate); len(sends) != 0 {
			t.Fatalf("%d notices went out after the payment, to %v", len(sends), teamBillingMailSendUserAuthsOf(sends))
		}
	})
}

// A renewal whose retries are about to run out owes its final notice to each recipient as well, each keyed by the
// invoice, the final notice and its address, and the end of the subscription withdraws each of those still unsent,
// while the first notices, already sent, stay sent.
func TestTeamFinalNoticeReachesEachRecipientAndTheSubscriptionsEndWithdrawsIt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()
		periods := newTestTeamPeriods()
		team := newTeamBillingMailTeam(t, ctx)
		subscriptionId := payTeamBillingMailTeam(t, ctx, env, periods, team, "tbm_final")

		const invoiceId = "in_synthetic_tbm_final"
		failTeamBillingMailRenewal(t, env, periods, team, subscriptionId, invoiceId)
		billingAddresses := team.billingAddresses()
		if got := teamBillingMailSendUserAuthsOf(deliverTeamBillingMail(ctx, teamBillingMailPaymentFailedTemplate)); !slices.Equal(got, billingAddresses) {
			t.Fatalf("the first notice went to %v, want %v", got, billingAddresses)
		}

		// the eighth failed attempt leaves Stripe one more: the final notice is due
		setTeamBillingMailInvoiceAttempts(env, invoiceId, 8)
		env.deliverInvoiceEvent(t, team.ownerSession, "invoice.payment_failed", invoiceId)
		finalAddresses := []string{}
		for _, message := range teamBillingMailMessages(t, ctx, team.networkId, teamBillingMailPaymentFailedTemplate) {
			if final, _ := message.fields["Final"].(bool); final {
				finalAddresses = append(finalAddresses, message.userAuth)
				if message.sent || message.abandoned {
					t.Fatalf("the final notice to %s: sent %t, abandoned %t, want waiting", message.userAuth, message.sent, message.abandoned)
				}
			} else if !message.sent {
				t.Fatalf("the first notice to %s is no longer sent", message.userAuth)
			}
		}
		if !slices.Equal(finalAddresses, billingAddresses) {
			t.Fatalf("the final notice is owed to %v, want %v", finalAddresses, billingAddresses)
		}
		for _, address := range billingAddresses {
			key := stripePaymentFailedTeamNoticeKey(invoiceId, stripePaymentFailedNoticeFinal, address)
			if message := model.GetAccountMessageByKey(ctx, teamBillingMailPaymentFailedTemplate, key); message == nil || message.UserAuth != address {
				t.Fatalf("the final notice to %s is not keyed by the invoice, the notice and the address: %+v", address, message)
			}
		}

		// Stripe's final action ends the subscription before the notices go out
		env.setSubscriptionStatus(subscriptionId, "canceled")
		env.deliverSubscriptionDeleted(t, team.ownerSession, subscriptionId)

		if failure := model.GetStripeInvoicePaymentFailure(ctx, invoiceId); failure == nil || failure.ResolveTime == nil || failure.ResolveReason != model.StripeInvoicePaymentFailureResolveSubscriptionEnded {
			t.Fatalf("the failure is %+v, want resolved by the subscription's end", failure)
		}
		for _, message := range teamBillingMailMessages(t, ctx, team.networkId, teamBillingMailPaymentFailedTemplate) {
			final, _ := message.fields["Final"].(bool)
			switch {
			case final && (message.sent || !message.abandoned || message.lastError != "withdrawn: the failed invoice is subscription_ended"):
				t.Fatalf("the final notice to %s: sent %t, abandoned %t, %q; want withdrawn", message.userAuth, message.sent, message.abandoned, message.lastError)
			case !final && (!message.sent || message.abandoned):
				t.Fatalf("the first notice to %s: sent %t, abandoned %t; want sent", message.userAuth, message.sent, message.abandoned)
			}
		}
		if sends := deliverTeamBillingMail(ctx, teamBillingMailPaymentFailedTemplate); len(sends) != 0 {
			t.Fatalf("%d notices went out after the subscription ended", len(sends))
		}
	})
}

// A Team's withheld payout notifies each of its Owners and its Billing admin, at each of their addresses, once per
// address however many slices of the run withheld it, each notice with the Team's total; and nobody else. Each held
// notice is keyed by the payment and its address. A personal network in the same slices gets its one notice with its
// own total, keyed by the payment, as it always did. Before QD-1219 the Team got none.
func TestTeamWithheldPayoutNotifiesItsOwnersAndBillingAdmins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		missingWalletTestRequestsOn(t)
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		team := newTeamBillingMailTeam(t, ctx)
		provider := newTeamBillingMailPerson(t, ctx, "provider")
		provider.addGoogleSignIn(t, ctx, "provider-google")
		providerNetworkId := provider.personal.ByJwt.NetworkId

		slicePlans := []*model.PaymentPlan{
			missingWalletTestPlan(map[server.Id]model.NanoCents{
				team.networkId:    model.UsdToNanoCents(2),
				providerNetworkId: model.UsdToNanoCents(1.25),
			}, nil),
			missingWalletTestPlan(map[server.Id]model.NanoCents{
				team.networkId:    model.UsdToNanoCents(3),
				providerNetworkId: model.UsdToNanoCents(0.75),
			}, nil),
		}
		for _, slice := range slicePlans {
			server.Tx(ctx, func(tx server.PgTx) {
				holdMissingWalletNoticesInTx(ctx, tx, slice)
			})
		}

		billingAddresses := team.billingAddresses()
		held := teamBillingMailMessages(t, ctx, team.networkId, teamBillingMailMissingWalletTemplate)
		if len(held) != 2*len(billingAddresses) {
			t.Fatalf("the Team holds %d notices, want one per slice for each of %v", len(held), billingAddresses)
		}
		for _, message := range held {
			if !message.held {
				t.Fatalf("the notice to %s is due before its release", message.userAuth)
			}
		}
		for _, slice := range slicePlans {
			payment := slice.NetworkPayments[team.networkId]
			for _, address := range billingAddresses {
				if message := model.GetAccountMessageByKey(ctx, teamBillingMailMissingWalletTemplate, missingWalletTeamNoticeKey(payment.PaymentId, address)); message == nil || message.UserAuth != address {
					t.Fatalf("the Team's notice of payment %s to %s is not keyed by the payment and the address: %+v", payment.PaymentId, address, message)
				}
			}
			providerPayment := slice.NetworkPayments[providerNetworkId]
			if message := model.GetAccountMessageByKey(ctx, teamBillingMailMissingWalletTemplate, providerPayment.PaymentId.String()); message == nil || message.UserAuth != provider.addresses[0] {
				t.Fatalf("the personal network's notice of payment %s: %+v, want keyed by the payment to its account's address", providerPayment.PaymentId, message)
			}
		}

		if releasedCount := releaseMissingWalletNotices(ctx, server.NowUtc(), missingWalletTestMinPayout(model.UsdToNanoCents(1))); releasedCount != len(billingAddresses)+1 {
			t.Fatalf("released %d notices, want one for each of the Team's %d addresses and the personal network's one", releasedCount, len(billingAddresses))
		}
		sends := deliverTeamBillingMail(ctx, teamBillingMailMissingWalletTemplate)
		wantAddresses := append(slices.Clone(billingAddresses), provider.addresses[0])
		slices.Sort(wantAddresses)
		if got := teamBillingMailSendUserAuthsOf(sends); !slices.Equal(got, wantAddresses) {
			t.Fatalf("the withheld payouts notified %v, want the Team's Owners and Billing admin and the personal network's account, %v", got, wantAddresses)
		}
		for _, send := range sends {
			template, ok := send.template.(*MissingWalletTemplate)
			if !ok {
				t.Fatalf("the notice to %s is a %T", send.userAuth, send.template)
			}
			want := "5.00"
			if send.userAuth == provider.addresses[0] {
				want = "2.00"
			}
			if template.AmountUsd() != want {
				t.Fatalf("the notice to %s says %s USDC, want its network's total, %s", send.userAuth, template.AmountUsd(), want)
			}
		}
	})
}

// A Team none of whose Owners and Billing admins has an email address, its Owner signing in by phone alone and its
// Admin, which holds no billing, at an address, is emailed neither mail: its failed renewal owes the in-app past-due
// notice alone, as an account with no email does, and its withheld payout no notice.
func TestTeamWithoutABillingEmailGetsNeitherMail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		missingWalletTestRequestsOn(t)
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()
		periods := newTestTeamPeriods()

		team := newTeamBillingMailTeam(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			// every billing member signs in by phone alone, at a fictional number of its own
			for i, person := range []*teamBillingMailPerson{team.owner, team.secondOwner, team.billingAdmin} {
				phone := fmt.Sprintf("+1415555016%d", i+1)
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_user_auth_password WHERE user_id = $1`, person.userId))
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_user_auth_sso WHERE user_id = $1`, person.userId))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET user_auth = $2 WHERE user_id = $1`, person.userId, phone))
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_user_auth_password (user_id, user_auth, auth_type, verified) VALUES ($1, $2, 'phone', true)`,
					person.userId,
					phone,
				))
			}
		})
		subscriptionId := payTeamBillingMailTeam(t, ctx, env, periods, team, "tbm_silent")
		const invoiceId = "in_synthetic_tbm_silent"
		failTeamBillingMailRenewal(t, env, periods, team, subscriptionId, invoiceId)

		if failure := model.GetStripeInvoicePaymentFailure(ctx, invoiceId); failure == nil || failure.ResolveTime != nil || failure.FirstNoticeTime == nil {
			t.Fatalf("the failure is %+v, want open with its notice owed in the app", failure)
		}
		if pastDue := paymentPastDueForNetwork(ctx, team.networkId, server.NowUtc(), defaultStripeDunningSettings()); pastDue == nil {
			t.Fatal("the Team shows no past-due notice in the app")
		}
		if messages := teamBillingMailMessages(t, ctx, team.networkId, teamBillingMailPaymentFailedTemplate); len(messages) != 0 {
			t.Fatalf("the failed renewal owes notices to %v, want none", teamBillingMailUserAuthsOf(messages))
		}

		server.Tx(ctx, func(tx server.PgTx) {
			holdMissingWalletNoticesInTx(ctx, tx, missingWalletTestPlan(map[server.Id]model.NanoCents{
				team.networkId: model.UsdToNanoCents(2),
			}, nil))
		})
		if messages := teamBillingMailMessages(t, ctx, team.networkId, teamBillingMailMissingWalletTemplate); len(messages) != 0 {
			t.Fatalf("the withheld payout holds notices to %v, want none", teamBillingMailUserAuthsOf(messages))
		}
	})
}

// A personal network's mail is unchanged: its failed renewal emails its account's address alone, keyed by the invoice
// and the notice, though its owner also signs in with Google at another address, and its withheld payout holds one
// notice to its admin's own sign-in, keyed by the payment.
func TestPersonalNetworkBillingMailIsUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		missingWalletTestRequestsOn(t)
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		person := newTeamBillingMailPerson(t, ctx, "personal")
		person.addGoogleSignIn(t, ctx, "personal-google")
		networkId := person.personal.ByJwt.NetworkId
		accountAddress := person.addresses[0]

		created := server.NowUtc().Add(-time.Hour).Truncate(time.Second)
		next := created.Add(2 * 24 * time.Hour)
		env.setSubscription("sub_synthetic_tbm_personal", "cus_synthetic_tbm_personal", &networkId, "past_due")
		env.setInvoice("in_synthetic_tbm_personal", "sub_synthetic_tbm_personal", "cus_synthetic_tbm_personal", "open", 1, created, &next, 4000)
		deliverStripePaymentFailedTestEvent(t, person.personal, "in_synthetic_tbm_personal")

		messages := teamBillingMailMessages(t, ctx, networkId, teamBillingMailPaymentFailedTemplate)
		if got := teamBillingMailUserAuthsOf(messages); !slices.Equal(got, []string{accountAddress}) {
			t.Fatalf("the personal network's failed renewal owes notices to %v, want its account's address alone, %s", got, accountAddress)
		}
		if message := model.GetAccountMessageByKey(ctx, teamBillingMailPaymentFailedTemplate, stripePaymentFailedNoticeKey("in_synthetic_tbm_personal", stripePaymentFailedNoticeFirst)); message == nil || message.UserAuth != accountAddress {
			t.Fatalf("the personal network's notice is not keyed by the invoice and the notice: %+v", message)
		}

		plan := missingWalletTestPlan(map[server.Id]model.NanoCents{networkId: model.UsdToNanoCents(2)}, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			holdMissingWalletNoticesInTx(ctx, tx, plan)
		})
		held := teamBillingMailMessages(t, ctx, networkId, teamBillingMailMissingWalletTemplate)
		if got := teamBillingMailUserAuthsOf(held); !slices.Equal(got, []string{accountAddress}) {
			t.Fatalf("the personal network's withheld payout holds notices to %v, want its admin's sign-in alone, %s", got, accountAddress)
		}
		if message := model.GetAccountMessageByKey(ctx, teamBillingMailMissingWalletTemplate, plan.NetworkPayments[networkId].PaymentId.String()); message == nil || message.UserAuth != accountAddress {
			t.Fatalf("the personal network's held notice is not keyed by the payment: %+v", message)
		}
	})
}

// The release coalesces held notices into one for each address of each network, carried by the oldest notice owed to
// that address with that notice's payment, and gives each the network's total, every payment counted once however
// many addresses it was owed to: a Team's three addresses, one of them owed only the second slice's payment, each get
// the Team's total; a personal network's one address its own total; and a network whose total is under the minimum
// none. Every other held notice is removed, and no held notice is changed.
func TestCoalesceMissingWalletNoticesReleasesOneForEachAddressWithTheNetworksTotal(t *testing.T) {
	teamNetworkId, personalNetworkId, smallNetworkId := server.NewId(), server.NewId(), server.NewId()
	firstPaymentId, secondPaymentId := server.NewId(), server.NewId()
	firstPersonalPaymentId, secondPersonalPaymentId := server.NewId(), server.NewId()
	smallPaymentId := server.NewId()
	held := func(networkId server.Id, userAuth string, paymentId server.Id, payoutUsd float64) *heldMissingWalletNotice {
		return &heldMissingWalletNotice{
			messageId: server.NewId(),
			networkId: networkId,
			userAuth:  userAuth,
			paymentId: paymentId,
			payout:    model.UsdToNanoCents(payoutUsd),
		}
	}
	ownerFirst := held(teamNetworkId, "owner@team-billing-mail.example", firstPaymentId, 2)
	billingFirst := held(teamNetworkId, "billing@team-billing-mail.example", firstPaymentId, 2)
	personalFirst := held(personalNetworkId, "personal@team-billing-mail.example", firstPersonalPaymentId, 1.25)
	smallOwner := held(smallNetworkId, "small-owner@team-billing-mail.example", smallPaymentId, 0.10)
	smallBilling := held(smallNetworkId, "small-billing@team-billing-mail.example", smallPaymentId, 0.10)
	ownerSecond := held(teamNetworkId, "owner@team-billing-mail.example", secondPaymentId, 3)
	billingSecond := held(teamNetworkId, "billing@team-billing-mail.example", secondPaymentId, 3)
	// joined between the slices
	newcomerSecond := held(teamNetworkId, "newcomer@team-billing-mail.example", secondPaymentId, 3)
	personalSecond := held(personalNetworkId, "personal@team-billing-mail.example", secondPersonalPaymentId, 0.75)
	heldNotices := []*heldMissingWalletNotice{
		ownerFirst, billingFirst, personalFirst, smallOwner, smallBilling,
		ownerSecond, billingSecond, newcomerSecond, personalSecond,
	}

	released, removedMessageIds := coalesceMissingWalletNotices(heldNotices, model.UsdToNanoCents(1))

	type want struct {
		carrier *heldMissingWalletNotice
		payout  model.NanoCents
	}
	wants := []want{
		{carrier: ownerFirst, payout: model.UsdToNanoCents(5)},
		{carrier: billingFirst, payout: model.UsdToNanoCents(5)},
		{carrier: personalFirst, payout: model.UsdToNanoCents(2)},
		{carrier: newcomerSecond, payout: model.UsdToNanoCents(5)},
	}
	if len(released) != len(wants) {
		t.Fatalf("released %d notices, want %d: %+v", len(released), len(wants), released)
	}
	for i, w := range wants {
		notice := released[i]
		if notice.messageId != w.carrier.messageId || notice.networkId != w.carrier.networkId || notice.userAuth != w.carrier.userAuth || notice.paymentId != w.carrier.paymentId {
			t.Fatalf("released notice %d is %+v, want carried by %+v", i, notice, w.carrier)
		}
		if notice.payout != w.payout {
			t.Fatalf("the notice to %s carries %d, want its network's total %d", notice.userAuth, notice.payout, w.payout)
		}
	}
	wantRemoved := []server.Id{
		ownerSecond.messageId, billingSecond.messageId, personalSecond.messageId,
		smallOwner.messageId, smallBilling.messageId,
	}
	if !slices.Equal(removedMessageIds, wantRemoved) {
		t.Fatalf("removed %v, want the later slices' notices and the small network's, %v", removedMessageIds, wantRemoved)
	}
	if ownerFirst.payout != model.UsdToNanoCents(2) || newcomerSecond.payout != model.UsdToNanoCents(3) {
		t.Fatal("coalescing changed a held notice")
	}
}
