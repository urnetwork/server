package controller

// The copy of the billing and payout mails that reach a Team's Owners and Billing admins (QD-1223). A Team's
// failed-payment, missing-wallet, epoch earnings and USDC payout-sent mail names the Team, "your URnetwork Team
// <name>"; says the Team, or the Team's subscription, where a personal network's says you, your network or UR Pro;
// says in its footer that it went to the Team's owners and billing admins; and tells the reader to act signed in to
// the Team, since the ur.io pages it links act on the network the reader is signed in to. A personal network's mail
// renders byte for byte as the templates rendered it before QD-1223, which testdata/personal_billing_mail_copy keeps.
// Whether the network is a Team, and its name, are read when the mail goes out and are never stored with it.
// team_billing_mail_copy_team_test.go sends each of the four for a Team that team-create made, on Postgres with MG6.

import (
	"context"
	"encoding/json"
	"fmt"
	htmltemplate "html/template"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	texttemplate "text/template"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The four templates' subject, html and text files as they were before QD-1223: the copy a personal network's mail
// keeps. A deliberate change to a personal network's copy changes these with it.
const personalBillingMailCopyDir = "testdata/personal_billing_mail_copy"

// The Team the render tests name.
const billingMailCopyTeamName = "synthetic-team"

// The Stripe-hosted invoice page the failed-payment samples link.
const billingMailCopyHostedInvoiceUrl = "https://invoice.stripe.com/i/acct_synthetic/live_synthetic?s=ap"

// The time the samples print, fixed so that a render is the same on every run.
var billingMailCopyTime = time.Date(2026, 10, 30, 9, 15, 0, 0, time.UTC)

// "your URnetwork Team <name>", as a Team's mail names the Team.
func billingMailCopyTeam(teamName string) string {
	return "your URnetwork Team " + teamName
}

// The template named as a Team's mail is when it goes out, for the Team called teamName.
func namedTeamBillingMail[T accountMessageBillingNamer](template T, teamName string) T {
	template.nameBillingMailNetwork(teamName, true)
	return template
}

// Renders the template as RenderEmailTemplate does, with its subject and body files read from dir and the layout
// every email uses.
func renderEmailTemplateFrom(t testing.TB, template Template, dir string) (subject string, bodyHtml string, bodyText string) {
	t.Helper()
	read := func(part string) string {
		t.Helper()
		contents, err := os.ReadFile(filepath.Join(dir, template.Name()+part))
		if err != nil {
			t.Fatal(err)
		}
		return string(contents)
	}

	subjectTemplate, err := texttemplate.New("subject").Funcs(TemplateFuncs(template)).Parse(read(".subject.txt"))
	if err != nil {
		t.Fatal(err)
	}
	out := &strings.Builder{}
	if err := subjectTemplate.Execute(out, template); err != nil {
		t.Fatal(err)
	}
	subject = strings.TrimSpace(out.String())

	htmlTemplate, err := htmltemplate.New("_layout.html").Funcs(TemplateFuncs(template)).ParseFS(emailTemplates, emailTemplateDir+"/_layout.html")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := htmlTemplate.New(template.Name() + ".html").Parse(read(".html")); err != nil {
		t.Fatal(err)
	}
	out = &strings.Builder{}
	if err := htmlTemplate.ExecuteTemplate(out, "_layout.html", template); err != nil {
		t.Fatal(err)
	}
	bodyHtml = out.String()

	textTemplate, err := texttemplate.New("_layout.txt").Funcs(TemplateFuncs(template)).ParseFS(emailTemplates, emailTemplateDir+"/_layout.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := textTemplate.New(template.Name() + ".txt").Parse(read(".txt")); err != nil {
		t.Fatal(err)
	}
	out = &strings.Builder{}
	if err := textTemplate.ExecuteTemplate(out, "_layout.txt", template); err != nil {
		t.Fatal(err)
	}
	bodyText = out.String()
	return
}

// Holds that the template renders exactly as the templates before QD-1223 render it: a personal network's copy.
func requirePersonalBillingMailCopy(t testing.TB, template Template) {
	t.Helper()
	subject, bodyHtml, bodyText, err := RenderEmailTemplate(template)
	if err != nil {
		t.Fatalf("%s: render: %v", template.Name(), err)
	}
	wantSubject, wantHtml, wantText := renderEmailTemplateFrom(t, template, personalBillingMailCopyDir)
	difference := func(got string, want string) string {
		i := 0
		for i < len(got) && i < len(want) && got[i] == want[i] {
			i += 1
		}
		from := max(i-60, 0)
		return fmt.Sprintf("at byte %d: %q, want %q", i, got[from:min(i+60, len(got))], want[from:min(i+60, len(want))])
	}
	if subject != wantSubject {
		t.Fatalf("%s: subject %q, want the personal copy's %q", template.Name(), subject, wantSubject)
	}
	if bodyHtml != wantHtml {
		t.Fatalf("%s: the html part is not the personal copy, %s", template.Name(), difference(bodyHtml, wantHtml))
	}
	if bodyText != wantText {
		t.Fatalf("%s: the text part is not the personal copy, %s", template.Name(), difference(bodyText, wantText))
	}
}

// Renders the template, the text part on one line with each run of space one space, so a phrase reads the same
// wherever the text wraps.
func renderBillingMailCopy(t testing.TB, template Template) (subject string, bodyHtml string, bodyText string) {
	t.Helper()
	subject, bodyHtml, bodyText, err := RenderEmailTemplate(template)
	if err != nil {
		t.Fatalf("%s: render: %v", template.Name(), err)
	}
	return subject, bodyHtml, strings.Join(strings.Fields(bodyText), " ")
}

// Holds that each of the parts says each of the phrases and none of the absent ones. Both match case and all, so
// "UR Pro" is not found in the layout's "UR protocol".
func requireBillingMailCopySays(t testing.TB, name string, parts map[string]string, phrases []string, absent []string) {
	t.Helper()
	for part, body := range parts {
		for _, phrase := range phrases {
			if !strings.Contains(body, phrase) {
				t.Fatalf("%s: the %s does not say %q:\n%s", name, part, phrase, body)
			}
		}
		for _, phrase := range absent {
			if strings.Contains(body, phrase) {
				t.Fatalf("%s: the %s says %q:\n%s", name, part, phrase, body)
			}
		}
	}
}

// Personal samples of the four mails, covering each branch of their personal copy.
func personalBillingMailCopySamples() []Template {
	nextAttempt := billingMailCopyTime.Add(3 * 24 * time.Hour)
	return []Template{
		&SubscriptionPaymentFailedTemplate{HostedInvoiceUrl: billingMailCopyHostedInvoiceUrl, AmountDue: 14293, Currency: "usd", NextAttemptTime: &nextAttempt},
		&SubscriptionPaymentFailedTemplate{Final: true, HostedInvoiceUrl: billingMailCopyHostedInvoiceUrl, AmountDue: 500, Currency: "usd"},
		&SubscriptionPaymentFailedTemplate{AmountDue: 1999, Currency: "eur"},
		&SubscriptionPaymentFailedTemplate{Final: true, AmountDue: 4000, Currency: "jpy", NextAttemptTime: &nextAttempt},
		&MissingWalletTemplate{PaymentId: server.NewId(), Payout: model.UsdToNanoCents(5)},
		&EpochEarningsTemplate{
			Epoch: 42, Points: 1234.5, ShareBps: 71, Rank: 17, Total: 5210, Top200Eligible: true, Top200Rank: 143,
			HasWallet: true, UnclaimedRao: big.NewInt(3_241_000_000), EpochEnd: billingMailCopyTime,
		},
		&EpochEarningsTemplate{Epoch: 43, Points: 2, Top200Bound: true, Top200Uid: 17, Top200Rank: 9, EpochEnd: billingMailCopyTime},
		// a wallet and nothing to claim
		&EpochEarningsTemplate{Epoch: 44, Points: 12.5, Rank: 3, Total: 100, HasWallet: true, EpochEnd: billingMailCopyTime},
		// a share whose alpha was not read
		&EpochEarningsTemplate{Epoch: 45, Points: 7, ShareBps: 12, HasWallet: true, EpochEnd: billingMailCopyTime},
		&UsdcPayoutSentTemplate{Transfer: model.UsdToNanoCents(428.860284), WalletSuffix: "Wx<z", SentTime: billingMailCopyTime, TxHash: "5synthetictransactionhash"},
		&UsdcPayoutSentTemplate{Transfer: model.UsdToNanoCents(29.94), WalletSuffix: "AbCd", SentTime: billingMailCopyTime},
	}
}

// A personal network's mail is unchanged: each sample renders its subject, html and text byte for byte as the
// templates before QD-1223 did, before it is named and after the delivery names it a personal network, which reads
// its name and shows none.
func TestPersonalBillingMailCopyIsUnchanged(t *testing.T) {
	for _, sample := range personalBillingMailCopySamples() {
		requirePersonalBillingMailCopy(t, sample)
		sample.(accountMessageBillingNamer).nameBillingMailNetwork("synthetic-personal", false)
		requirePersonalBillingMailCopy(t, sample)
		_, bodyHtml, bodyText, err := RenderEmailTemplate(sample)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(bodyHtml, "synthetic-personal") || strings.Contains(bodyText, "synthetic-personal") {
			t.Fatalf("%s: a personal network's mail names it", sample.Name())
		}
	}
}

// A Team's failed-payment notice names the Team in its subject and both parts, speaks of the Team's subscription and
// never of UR Pro or the reader's own account, says it went to the Team's owners and billing admins, and tells the
// reader to switch to the Team at ur.io before updating the payment method: the first and the final notice, with and
// without the invoice's Stripe page.
func TestTeamFailedPaymentNoticeNamesTheTeam(t *testing.T) {
	nextAttempt := billingMailCopyTime.Add(3 * 24 * time.Hour)
	team := billingMailCopyTeam(billingMailCopyTeamName)
	for _, template := range []*SubscriptionPaymentFailedTemplate{
		{HostedInvoiceUrl: billingMailCopyHostedInvoiceUrl, AmountDue: 14293, Currency: "usd", NextAttemptTime: &nextAttempt},
		{Final: true, HostedInvoiceUrl: billingMailCopyHostedInvoiceUrl, AmountDue: 14293, Currency: "usd"},
		{AmountDue: 1999, Currency: "eur"},
		{Final: true, AmountDue: 1999, Currency: "eur", NextAttemptTime: &nextAttempt},
	} {
		namedTeamBillingMail(template, billingMailCopyTeamName)
		subject, bodyHtml, bodyText := renderBillingMailCopy(t, template)
		wantSubject := "The payment for " + team + " didn't go through"
		if template.Final {
			wantSubject = "Final notice: the payment for " + team + " didn't go through"
		}
		if subject != wantSubject {
			t.Fatalf("subject %q, want %q", subject, wantSubject)
		}
		absent := []string{"UR Pro", "your URnetwork account", "your payment method", "If you already paid"}
		requireBillingMailCopySays(t, template.Name(), map[string]string{"subject": subject}, []string{team}, absent)
		phrases := []string{
			team,
			"the Team's subscription",
			"Sent to the sign-in email addresses of the owners and billing admins of " + team,
			"sign in at ur.io and switch to " + team + " first, since Manage Subscription opens the billing of the network you're signed in to",
			StripeUpdatePaymentMethodUrl,
		}
		if template.Final {
			phrases = append(phrases, "if the payment doesn't go through, the Team's subscription will end")
		} else {
			phrases = append(phrases, "We tried to renew the subscription of "+team+", but the payment of "+template.Amount()+" didn't go through")
		}
		if template.HostedInvoiceUrl != "" {
			phrases = append(phrases, template.HostedInvoiceUrl)
		}
		requireBillingMailCopySays(t, template.Name(), map[string]string{"html": bodyHtml, "text": bodyText}, phrases, absent)
	}
}

// A Team's missing-wallet notice names the Team, says the Team's payout is withheld, says it went to the Team's
// owners and billing admins, and tells the reader to connect the wallet signed in to the Team; it never says the
// reader earned it or that a wallet is missing from the reader's account.
func TestTeamMissingWalletNoticeNamesTheTeam(t *testing.T) {
	team := billingMailCopyTeam(billingMailCopyTeamName)
	template := namedTeamBillingMail(&MissingWalletTemplate{PaymentId: server.NewId(), Payout: model.UsdToNanoCents(5)}, billingMailCopyTeamName)
	subject, bodyHtml, bodyText := renderBillingMailCopy(t, template)
	if want := "Connect a Solana wallet to " + team + " to receive 5.00 USDC"; subject != want {
		t.Fatalf("subject %q, want %q", subject, want)
	}
	absent := []string{"You earned", "you earned", "on your account", "while you have new"}
	requireBillingMailCopySays(t, template.Name(), map[string]string{"html": bodyHtml, "text": bodyText}, []string{
		team,
		"5.00 USDC waiting",
		"The payout of " + team + " is withheld: the Team earned 5.00 USDC providing on URnetwork, but it has no Solana wallet to send it to.",
		"Connect it signed in to the Team: sign in at ur.io and switch to " + team + " first, since a wallet is connected to the network you're signed in to.",
		"Sent to the sign-in email addresses of the owners and billing admins of " + team + " once per pay period while the Team has new USDC earnings",
		"https://ur.io/app/account/earnings?wallet=solana",
	}, absent)
}

// A Team's epoch earnings email names the Team, says the Team earned and the Team qualifies or holds its Top 200 spot,
// says it went to the Team's owners and billing admins, and tells the reader to switch to the Team before claiming or
// connecting a wallet; it never says your network: with a claim, with a bound head spot, with nothing to claim, and
// with no wallet.
func TestTeamEpochEarningsEmailNamesTheTeam(t *testing.T) {
	team := billingMailCopyTeam(billingMailCopyTeamName)
	for _, c := range []struct {
		template *EpochEarningsTemplate
		phrases  []string
	}{
		{
			template: &EpochEarningsTemplate{
				Epoch: 42, Points: 1234.5, ShareBps: 71, Rank: 17, Total: 5210, Top200Eligible: true, Top200Rank: 143,
				HasWallet: true, UnclaimedRao: big.NewInt(3_241_000_000), EpochEnd: billingMailCopyTime,
			},
			phrases: []string{"Top 200 · the Team qualifies", "The Team qualifies for a Top 200 head mining spot.", "3.2410 SN25α", "https://ur.io/app/account/top200", "https://ur.io/app/account/claim"},
		},
		{
			template: &EpochEarningsTemplate{Epoch: 43, Points: 2, Top200Bound: true, Top200Uid: 17, Top200Rank: 9, EpochEnd: billingMailCopyTime},
			phrases:  []string{"Top 200 · UID 17 · rank #9", "The Team holds a head mining spot on the UR protocol.", "Connect a Bittensor wallet"},
		},
		{
			template: &EpochEarningsTemplate{Epoch: 44, Points: 12.5, Rank: 3, Total: 100, HasWallet: true, EpochEnd: billingMailCopyTime},
			phrases:  []string{"Nothing to claim for epoch 44 yet. Keep providing and the Team's next epoch settles as SN25α"},
		},
	} {
		namedTeamBillingMail(c.template, billingMailCopyTeamName)
		subject, bodyHtml, bodyText := renderBillingMailCopy(t, c.template)
		if want := fmt.Sprintf("In epoch %d, %s earned %s points", c.template.Epoch, team, c.template.PointsText()); subject != want {
			t.Fatalf("subject %q, want %q", subject, want)
		}
		absent := []string{"your network", "Your network", "you qualify", "your points", "your next epoch", "your head spot", "your SN25α", "your claims"}
		requireBillingMailCopySays(t, c.template.Name(), map[string]string{"subject": subject}, []string{team}, absent)
		phrases := append([]string{
			team,
			fmt.Sprintf("Earnings of %s for providing on URnetwork in epoch %d", team, c.template.Epoch),
			fmt.Sprintf("Sent to the sign-in email addresses of the owners and billing admins of %s because the Team earned on URnetwork in epoch %d.", team, c.template.Epoch),
			"These pages act on the network you're signed in to: sign in at ur.io and switch to " + team + " first.",
		}, c.phrases...)
		requireBillingMailCopySays(t, c.template.Name(), map[string]string{"html": bodyHtml, "text": bodyText}, phrases, absent)
	}
}

// A Team's USDC payout-sent notice names the Team and its wallet, says it went to the Team's owners and billing
// admins, and tells the reader to switch to the Team before claiming; it never says your network or that the address
// administers a network: with and without the transaction's hash.
func TestTeamUsdcPayoutSentNoticeNamesTheTeam(t *testing.T) {
	team := billingMailCopyTeam(billingMailCopyTeamName)
	for _, template := range []*UsdcPayoutSentTemplate{
		{Transfer: model.UsdToNanoCents(428.860284), WalletSuffix: "WxYz", SentTime: billingMailCopyTime, TxHash: "5synthetictransactionhash"},
		{Transfer: model.UsdToNanoCents(29.94), WalletSuffix: "AbCd", SentTime: billingMailCopyTime},
	} {
		namedTeamBillingMail(template, billingMailCopyTeamName)
		subject, bodyHtml, bodyText := renderBillingMailCopy(t, template)
		if want := "The " + template.AmountUsdc() + " USDC payout of " + team + " was sent"; subject != want {
			t.Fatalf("subject %q, want %q", subject, want)
		}
		absent := []string{"your network", "Your network", "this address administers", "Your earnings", "your earnings"}
		phrases := []string{
			team,
			"We sent " + template.AmountUsdc() + " USDC to the Solana wallet of " + team + ", ending in " + template.WalletSuffix + ", on October 30, 2026.",
			"Sent to the sign-in email addresses of the owners and billing admins of " + team + ", which was owed a USDC payout.",
			"The Team's earnings for providing on URnetwork now settle as SN25α. Connect a Bittensor wallet to the Team to claim them",
			"The claim acts on the network you're signed in to: sign in at ur.io and switch to " + team + " first.",
			"https://ur.io/app/account/claim",
		}
		if template.TxHash != "" {
			phrases = append(phrases, "Solana transaction: "+template.TxHash)
		}
		requireBillingMailCopySays(t, template.Name(), map[string]string{"subject": subject}, []string{team}, absent)
		requireBillingMailCopySays(t, template.Name(), map[string]string{"html": bodyHtml, "text": bodyText}, phrases, absent)
	}
}

// A Team's name shows on one line in reading order, as a security notice shows a network's (control and format
// characters count as space), is escaped in the html part, and a Team with no name to show is "your URnetwork Team".
func TestBillingMailTeamNameShowsOnOneLine(t *testing.T) {
	template := namedTeamBillingMail(&SubscriptionPaymentFailedTemplate{AmountDue: 100, Currency: "usd"}, "synthetic\nteam\t<b>bold</b>\u202e")
	subject, bodyHtml, bodyText, err := RenderEmailTemplate(template)
	if err != nil {
		t.Fatal(err)
	}
	if want := "The payment for your URnetwork Team synthetic team <b>bold</b> didn't go through"; subject != want {
		t.Fatalf("subject %q, want %q", subject, want)
	}
	if !strings.Contains(bodyHtml, "your URnetwork Team synthetic team &lt;b&gt;bold&lt;/b&gt;") || strings.Contains(bodyHtml, "<b>bold</b>") {
		t.Fatal("the html part did not escape the Team's name")
	}
	if !strings.Contains(strings.Join(strings.Fields(bodyText), " "), "your URnetwork Team synthetic team <b>bold</b>") {
		t.Fatal("the text part does not name the Team")
	}

	unnamed := namedTeamBillingMail(&MissingWalletTemplate{PaymentId: server.NewId(), Payout: model.UsdToNanoCents(2)}, "")
	subject, _, _, err = RenderEmailTemplate(unnamed)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Connect a Solana wallet to your URnetwork Team to receive 2.00 USDC"; subject != want {
		t.Fatalf("a Team with no name to show: subject %q, want %q", subject, want)
	}
}

// Neither the Team's name nor whether it is a Team is stored with an outbox mail: a named template's stored fields
// are an unnamed one's, and a stored mail decodes unnamed, to be named when it goes out.
func TestBillingMailNetworkIsNotStored(t *testing.T) {
	nextAttempt := billingMailCopyTime.Add(3 * 24 * time.Hour)
	for _, c := range []struct {
		unnamed Template
		named   Template
	}{
		{
			unnamed: &SubscriptionPaymentFailedTemplate{HostedInvoiceUrl: billingMailCopyHostedInvoiceUrl, AmountDue: 14293, Currency: "usd", NextAttemptTime: &nextAttempt},
			named:   namedTeamBillingMail(&SubscriptionPaymentFailedTemplate{HostedInvoiceUrl: billingMailCopyHostedInvoiceUrl, AmountDue: 14293, Currency: "usd", NextAttemptTime: &nextAttempt}, billingMailCopyTeamName),
		},
		{
			unnamed: &MissingWalletTemplate{PaymentId: server.RequireIdFromBytes(make([]byte, 16)), Payout: model.UsdToNanoCents(2)},
			named:   namedTeamBillingMail(&MissingWalletTemplate{PaymentId: server.RequireIdFromBytes(make([]byte, 16)), Payout: model.UsdToNanoCents(2)}, billingMailCopyTeamName),
		},
	} {
		unnamedJson, err := json.Marshal(c.unnamed)
		if err != nil {
			t.Fatal(err)
		}
		namedJson, err := json.Marshal(c.named)
		if err != nil {
			t.Fatal(err)
		}
		if string(namedJson) != string(unnamedJson) {
			t.Fatalf("%s: a named Team's stored fields %s differ from an unnamed template's %s", c.named.Name(), namedJson, unnamedJson)
		}
		if strings.Contains(string(namedJson), billingMailCopyTeamName) || strings.Contains(strings.ToLower(string(namedJson)), "team") {
			t.Fatalf("%s: the stored fields hold the Team: %s", c.named.Name(), namedJson)
		}
		decoded, err := decodeAccountMessageTemplate(c.named.Name(), string(namedJson))
		if err != nil {
			t.Fatal(err)
		}
		if namer := decoded.(interface {
			Team() bool
			TeamNetwork() string
		}); namer.Team() || namer.TeamNetwork() != "" {
			t.Fatalf("%s: a stored mail decodes named", decoded.Name())
		}
	}
}

// A personal network's failed-payment and missing-wallet notices go out through the outbox with a personal
// network's copy, byte for byte, though where MG6 is linked the delivery reads the network's name and whether it is
// a Team; neither names the network.
func TestPersonalBillingMailIsDeliveredUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		missingWalletTestRequestsOn(t)
		ctx := context.Background()
		networkId := server.NewId()
		networkName := securityNoticeTestNetworkName("billing-personal")
		userAuth := model.Testing_CreateNetwork(ctx, networkId, networkName, server.NewId())
		nextAttempt := billingMailCopyTime.Add(3 * 24 * time.Hour)
		for _, template := range []Template{
			&SubscriptionPaymentFailedTemplate{HostedInvoiceUrl: billingMailCopyHostedInvoiceUrl, AmountDue: 4000, Currency: "usd", NextAttemptTime: &nextAttempt},
			&SubscriptionPaymentFailedTemplate{Final: true, AmountDue: 4000, Currency: "usd"},
			&MissingWalletTemplate{PaymentId: server.NewId(), Payout: model.UsdToNanoCents(2)},
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				if !addAccountMessageInTx(ctx, tx, &accountMessage{
					key:       "synthetic-billing-copy:" + server.NewId().String(),
					networkId: &networkId,
					userAuth:  userAuth,
					template:  template,
				}) {
					t.Fatalf("%s was not added", template.Name())
				}
			})
		}

		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, server.NowUtc())
		sentCount := 0
		for _, send := range sender.sent() {
			namer, ok := send.template.(interface{ Team() bool })
			if !ok {
				continue
			}
			sentCount += 1
			if send.userAuth != userAuth || namer.Team() {
				t.Fatalf("%s went to %s as a Team's: %t", send.template.Name(), send.userAuth, namer.Team())
			}
			requirePersonalBillingMailCopy(t, send.template)
			subject, bodyHtml, bodyText, err := RenderEmailTemplate(send.template)
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range []string{subject, bodyHtml, bodyText} {
				if strings.Contains(part, networkName) {
					t.Fatalf("%s names the personal network: %q", send.template.Name(), part)
				}
			}
		}
		if sentCount != 3 {
			t.Fatalf("sent %d billing notices, want 3", sentCount)
		}
	})
}

// In a binary that links MG6, a read of the network that fails leaves a billing mail unnamed and returns the error, so
// the delivery tries the message again rather than send a Team's mail as a personal network's; in one that does not,
// which has no Teams, a billing mail reads nothing, as before QD-1223, even where a read would fail. A row without a
// network reads nothing, and a personal network's mail is named a personal network's.
func TestNameAccountMessageNetworkReturnsAFailedBillingRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, securityNoticeTestNetworkName("billing-read"), server.NewId())
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		for _, template := range []interface {
			Template
			Team() bool
			TeamNetwork() string
		}{
			&SubscriptionPaymentFailedTemplate{AmountDue: 100, Currency: "usd"},
			&MissingWalletTemplate{PaymentId: server.NewId(), Payout: model.UsdToNanoCents(2)},
		} {
			err := nameAccountMessageNetwork(canceledCtx, &networkId, template)
			if server.SidecarMg6Linked() && err == nil {
				t.Fatalf("%s: a failed read returned no error", template.Name())
			}
			if !server.SidecarMg6Linked() && err != nil {
				t.Fatalf("%s: a binary without Teams read the network: %v", template.Name(), err)
			}
			if template.Team() || template.TeamNetwork() != "" {
				t.Fatalf("%s: a failed read named the network", template.Name())
			}
			if err := nameAccountMessageNetwork(canceledCtx, nil, template); err != nil {
				t.Fatalf("%s: a row without a network read it: %v", template.Name(), err)
			}
			if err := nameAccountMessageNetwork(ctx, &networkId, template); err != nil {
				t.Fatal(err)
			}
			if template.Team() || template.TeamNetwork() != "" {
				t.Fatalf("%s: a personal network was named a Team", template.Name())
			}
		}
	})
}
