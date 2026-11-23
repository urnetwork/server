//go:build sidecar_pending

package controller

// A Team's epoch earnings email and its USDC payout-sent notice reach its billing recipients, its Owners and Billing
// admins (QD-1222: QD-1219's rule carried to the mail about a network's earnings and payouts, in QD-1218's order and
// bound), on Postgres with MG0, MG1 and MG6. Both found their recipient through the network's admin user, which in a
// Team is the synthetic owner of record team-create writes with no sign-in (D-TEAM-2): a Team's epoch earnings were
// never emailed, the run counting it skipped, and `bringyourctl payouts usdc-sent-notice` reported that the Team's
// admin had no email login and sent nothing. Each Team here is team_billing_mail_test.go's: two Owners, a Billing
// admin, a person in every other role and a pending Billing admin, each signing in at synthetic addresses of its own.
// A personal network's mail is unchanged.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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

// A completed payment of payout to an active Solana wallet of the network, which sent it on chain.
func (self *usdcSentNoticeTestFixture) completedPaymentOf(networkId server.Id, payout model.NanoCents) server.Id {
	walletId := server.NewId()
	self.exec(
		`INSERT INTO account_wallet(wallet_id,network_id,wallet_type,blockchain,wallet_address,active,default_token_type)
			VALUES($1,$2,'external','SOL',$3,true,'USDC')`,
		walletId,
		networkId,
		"Synthetic"+strings.ReplaceAll(networkId.String(), "-", "")+"WxYz",
	)
	paymentId := server.NewId()
	self.exec(
		`INSERT INTO account_payment(payment_id,payment_plan_id,network_id,wallet_id,payout_byte_count,payout_nano_cents,min_sweep_time,create_time)
			VALUES($1,$2,$3,$4,0,$5,now(),now())`,
		paymentId,
		server.NewId(),
		networkId,
		walletId,
		payout,
	)
	self.completed(paymentId)
	return paymentId
}

// One journaled usdc_payout_sent_notice event.
type teamEarningsMailJournal struct {
	eventId      server.Id
	eventDetails map[string]any
}

// The payment's journaled notices, by event id.
func teamEarningsMailJournalOf(t testing.TB, f *usdcSentNoticeTestFixture, paymentId server.Id) map[server.Id]*teamEarningsMailJournal {
	t.Helper()
	journals := map[server.Id]*teamEarningsMailJournal{}
	server.Db(f.ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			f.ctx,
			`SELECT event_id, event_details FROM audit_account_payment WHERE payment_id = $1 AND event_type = $2`,
			paymentId,
			UsdcPayoutSentNoticeEventType,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				journal := &teamEarningsMailJournal{}
				var eventDetails string
				server.Raise(result.Scan(&journal.eventId, &eventDetails))
				server.Raise(json.Unmarshal([]byte(eventDetails), &journal.eventDetails))
				journals[journal.eventId] = journal
			}
		})
	})
	return journals
}

// The addresses of the USDC notices sent, in send order.
func teamEarningsMailUsdcUserAuthsOf(sends []usdcSentNoticeTestSend) []string {
	userAuths := []string{}
	for _, send := range sends {
		userAuths = append(userAuths, send.userAuth)
	}
	return userAuths
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

// `payouts usdc-sent-notice` sends a Team's notice to each of its billing recipients once: its dry run, in a read only
// process, shows a row for each address, Owners first, and sends nothing; the send emails each address and journals
// each by the payment and the address, with no address in the journal; a rerun finds each journaled and sends none
// again; and an address the Team gains after the run gets its own notice in the next while the others stay sent.
// Before QD-1222 the command held the Team's notice, its admin having no email login. A personal network's payment
// listed with it keeps its one row to its admin's sign-in, journaled under the payment alone.
func TestTeamUsdcPayoutSentNoticeReachesEachBillingRecipientOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newUsdcSentNoticeTestFixture(t)
		ctx := f.ctx
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		team := newTeamBillingMailTeam(t, ctx)
		person := newTeamBillingMailPerson(t, ctx, "personal")
		person.addGoogleSignIn(t, ctx, "personal-google")
		teamPayout := model.UsdToNanoCents(428.870284)
		teamPaymentId := f.completedPaymentOf(team.networkId, teamPayout)
		personalPaymentId := f.completedPaymentOf(person.personal.ByJwt.NetworkId, model.UsdToNanoCents(29.951303))
		billingOrder := teamEarningsMailBillingOrder(team)

		// the dry run's process is read only, as the command makes it, and the Team's read is a read only transaction
		server.RequirePgReadOnly()
		t.Cleanup(server.Testing_ReleasePgReadOnly)
		dryRun := f.run(false, teamPaymentId, personalPaymentId)
		server.Testing_ReleasePgReadOnly()
		if len(dryRun.Rows) != len(billingOrder)+1 {
			t.Fatalf("the dry run has %d rows, want one for each of the Team's %d addresses and the personal payment's one", len(dryRun.Rows), len(billingOrder))
		}
		for i, row := range dryRun.Rows[:len(billingOrder)] {
			if row.PaymentId != teamPaymentId || !row.Team || row.RecipientNumber != i+1 || row.RecipientCount != len(billingOrder) || row.Recipient != "email" || row.Problem != "" || row.AlreadyNotified || row.Notified {
				t.Fatalf("the Team's row %d: %+v, want address %d of %d, due", i, row, i+1, len(billingOrder))
			}
			if row.userAuth != billingOrder[i] {
				t.Fatalf("the Team's row %d goes to %s, want %s: Owners first", i, row.userAuth, billingOrder[i])
			}
		}
		if row := dryRun.Rows[len(billingOrder)]; row.PaymentId != personalPaymentId || row.Team || row.RecipientNumber != 0 || row.Recipient != "email" || row.userAuth != person.addresses[0] || row.Problem != "" {
			t.Fatalf("the personal payment's row: %+v to %s, want one row to its admin's sign-in %s", row, row.userAuth, person.addresses[0])
		}
		if sends := f.sender.taken(); len(sends) != 0 {
			t.Fatalf("the dry run sent %d notices", len(sends))
		}
		dryRunText := dryRun.Text()
		for _, want := range []string{
			fmt.Sprintf("payment 0, Team address 1 of %d: 428.86 USDC, sent by record, recipient email, due", len(billingOrder)),
			fmt.Sprintf("payment 0, Team address %d of %d:", len(billingOrder), len(billingOrder)),
			"payment 1: 29.94 USDC, sent by record, recipient email, due",
			fmt.Sprintf("%d notice(s) due", len(billingOrder)+1),
		} {
			if !strings.Contains(dryRunText, want) {
				t.Fatalf("the dry run's report lacks %q:\n%s", want, dryRunText)
			}
		}
		for _, address := range append(slices.Clone(billingOrder), person.addresses...) {
			if strings.Contains(dryRunText, address) {
				t.Fatal("the report printed a recipient's address")
			}
		}

		sent := f.run(true, teamPaymentId, personalPaymentId)
		for _, row := range sent.Rows {
			if !row.Notified || row.Problem != "" {
				t.Fatalf("a row was not notified: %+v", row)
			}
		}
		sends := f.sender.taken()
		wantSendOrder := append(slices.Clone(billingOrder), person.addresses[0])
		if got := teamEarningsMailUsdcUserAuthsOf(sends); !slices.Equal(got, wantSendOrder) {
			t.Fatalf("the send emailed %v, want each Team billing address once and the personal admin once, %v", got, wantSendOrder)
		}
		for _, send := range sends {
			if slices.Contains(team.otherAddresses(), send.userAuth) {
				t.Fatalf("a member whose role holds no billing was emailed at %s", send.userAuth)
			}
			if send.userAuth != person.addresses[0] && (send.template.Transfer != teamPayout-model.UsdToNanoCents(providerPaymentFeeUsd) || send.template.WalletSuffix != "WxYz" || send.template.TxHash != "5recordhash") {
				t.Fatalf("the Team's notice to %s: transfer %d, suffix %q, hash %q", send.userAuth, send.template.Transfer, send.template.WalletSuffix, send.template.TxHash)
			}
		}

		teamJournals := teamEarningsMailJournalOf(t, f, teamPaymentId)
		if len(teamJournals) != len(billingOrder) {
			t.Fatalf("the Team's payment has %d journaled notices, want one for each of its %d addresses", len(teamJournals), len(billingOrder))
		}
		for _, address := range billingOrder {
			journal, ok := teamJournals[usdcPayoutSentTeamNoticeEventId(teamPaymentId, address)]
			if !ok {
				t.Fatalf("the notice to %s is not journaled by the payment and the address", address)
			}
			if team, _ := journal.eventDetails["team"].(bool); !team {
				t.Fatalf("the Team's journaled notice does not say it is a Team's: %v", journal.eventDetails)
			}
			eventDetails, _ := json.Marshal(journal.eventDetails)
			for _, billingAddress := range billingOrder {
				if strings.Contains(string(eventDetails), billingAddress) {
					t.Fatalf("the journal holds an address: %s", eventDetails)
				}
			}
		}
		if _, ok := teamJournals[usdcPayoutSentNoticeEventId(teamPaymentId)]; ok {
			t.Fatal("a Team's notice was journaled under a personal network's key")
		}
		personalJournals := teamEarningsMailJournalOf(t, f, personalPaymentId)
		if _, ok := personalJournals[usdcPayoutSentNoticeEventId(personalPaymentId)]; len(personalJournals) != 1 || !ok {
			t.Fatalf("the personal payment has %d journaled notices, want its one under the payment's key", len(personalJournals))
		}

		rerun := f.run(true, teamPaymentId, personalPaymentId)
		for _, row := range rerun.Rows {
			if !row.AlreadyNotified || row.Notified {
				t.Fatalf("a rerun did not find a journaled notice: %+v", row)
			}
		}
		if sends := f.sender.taken(); len(sends) != 0 {
			t.Fatalf("a rerun sent %d notices, to %v", len(sends), teamEarningsMailUsdcUserAuthsOf(sends))
		}

		// a second Billing admin joins after the run: the next run owes it alone
		newcomer := newTeamBillingMailPerson(t, ctx, "billing-newcomer")
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{
				NetworkId: team.networkId,
				UserId:    newcomer.userId,
				Role:      model.NetworkMemberRoleBillingAdmin,
			})
			server.Raise(err)
		})
		joined := f.run(true, teamPaymentId)
		if len(joined.Rows) != len(billingOrder)+1 {
			t.Fatalf("after the join the Team's payment has %d rows, want %d", len(joined.Rows), len(billingOrder)+1)
		}
		for _, row := range joined.Rows {
			newRow := row.userAuth == newcomer.addresses[0]
			if newRow != row.Notified || newRow == row.AlreadyNotified {
				t.Fatalf("after the join, the row to %s: notified %t, already notified %t; want only the newcomer's sent", row.userAuth, row.Notified, row.AlreadyNotified)
			}
		}
		if got := teamEarningsMailUsdcUserAuthsOf(f.sender.taken()); !slices.Equal(got, newcomer.addresses) {
			t.Fatalf("after the join the run emailed %v, want the newcomer alone, %v", got, newcomer.addresses)
		}
	})
}

// A Team none of whose Owners and Billing admins has an email address, its two Owners and its Billing admin each
// signing in by phone alone while its Admin and the rest, which hold no billing, sign in at addresses, gets neither
// mail: its epoch earnings are skipped, and its USDC notice is held on one row that says why, sent to nobody and
// journaled nowhere.
func TestTeamWithoutABillingEmailGetsNoEarningsOrPayoutMail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		earnings := newEpochEarningsTestFixture(t)
		usdc := newUsdcSentNoticeTestFixture(t)
		ctx := earnings.ctx
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		team := newTeamBillingMailTeam(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			// every billing member signs in by phone alone, at a fictional number of its own
			for i, person := range []*teamBillingMailPerson{team.owner, team.secondOwner, team.billingAdmin} {
				phone := fmt.Sprintf("+1415555017%d", i+1)
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

		earnings.finalize(8, map[server.Id]int{team.networkId: 10_000})
		sender := &epochEarningsTestSender{}
		if sent, skipped := earnings.send(8, sender); sent != 0 || skipped != 1 {
			t.Fatalf("sent %d and skipped %d, want the Team skipped", sent, skipped)
		}
		if sends := sender.taken(); len(sends) != 0 {
			t.Fatalf("the Team's earnings went to %v, want nobody", epochEarningsTestUserAuthsOf(sends))
		}

		paymentId := usdc.completedPaymentOf(team.networkId, model.UsdToNanoCents(7))
		report := usdc.run(true, paymentId)
		if len(report.Rows) != 1 {
			t.Fatalf("the Team's payment has %d rows, want one", len(report.Rows))
		}
		if row := report.Rows[0]; !row.Team || row.Recipient != "none" || row.Problem != "the Team has no Owner or Billing admin with an email login" || row.Notified || row.RecipientCount != 0 {
			t.Fatalf("the Team's row: %+v, want held for having no billing email", row)
		}
		if !strings.Contains(report.Text(), "payment 0, Team: 6.99 USDC, sent by record, recipient none, held: the Team has no Owner or Billing admin with an email login") {
			t.Fatalf("the report: %s", report.Text())
		}
		if sends := usdc.sender.taken(); len(sends) != 0 {
			t.Fatalf("the Team's notice went to %v, want nobody", teamEarningsMailUsdcUserAuthsOf(sends))
		}
		if journals := teamEarningsMailJournalOf(t, usdc, paymentId); len(journals) != 0 {
			t.Fatalf("%d notices were journaled, want none", len(journals))
		}
	})
}
