package controller

import (
	"math/big"
	"regexp"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Every template renders through the shared layout with sample data. This pins the
// invariants the deliverability plan relies on (EMAIL1.md §5): a one-line subject,
// no unresolved template actions, only the two wordmark images from the sending
// domain, every link on ur.io, a plain-text part without markup, escaped sample
// values, and an SMS body short enough to be one or two messages.
func TestEmailTemplatesRender(t *testing.T) {
	// the one link that legitimately leaves ur.io is the UR protocol site the
	// epoch earnings email points at
	const protocolSite = "https://ur.xyz"
	samples := []struct {
		template Template
		// sms is whether the template carries its own short body for phone accounts
		sms bool
	}{
		{&AuthVerifyTemplate{VerifyCode: "482917"}, true},
		{&AuthPasswordResetTemplate{ResetCode: strings.Repeat("ab", 64)}, true},
		{&AuthPasswordSetTemplate{}, true},
		{&NetworkWelcomeTemplate{}, true},
		{&EpochEarningsTemplate{
			Epoch:          42,
			Points:         1234.5,
			ShareBps:       71,
			Rank:           17,
			Total:          5210,
			Top200Eligible: true,
			Top200Rank:     143,
			HasWallet:      true,
			UnclaimedRao:   big.NewInt(3_241_000_000),
			EpochEnd:       server.NowUtc(),
		}, true},
		{&EpochEarningsTemplate{
			Epoch:       43,
			Points:      2,
			Top200Bound: true,
			Top200Uid:   17,
			Top200Rank:  9,
			EpochEnd:    server.NowUtc(),
		}, true},
		{&MissingWalletTemplate{PaymentId: server.NewId(), AmountUsd: "5.00"}, true},
		{&SubscriptionTransferBalanceCodeTemplate{Secret: "K7QX3M2PNB4DLZ8R9YWC5AGHT6", BalanceByteCount: 10 * model.Tib}, false},
		{&SubscriptionDataAppliedTemplate{Secret: "K7QX3M2PNB4DLZ8R9YWC5AGHT6", BalanceByteCount: 1 * model.Tib, NetworkName: "brien"}, false},
		{&X402ReceiptTemplate{
			Description:      "1 TiB data pack & more",
			PriceUsd:         5,
			Asset:            "USDC",
			Network:          "base",
			Transaction:      "0xdeadbeef",
			Pro:              true,
			BalanceByteCount: model.Tib,
		}, false},
	}

	imageSrc := regexp.MustCompile(`src="([^"]+)"`)
	linkHref := regexp.MustCompile(`href="([^"]+)"`)
	allowedImages := map[string]bool{
		"https://ur.io/images/emails/ur-wordmark-black-bg-320.png": true,
		"https://ur.io/images/emails/ur-wordmark-white-320.png":    true,
	}
	allowedLink := func(href string) bool {
		if href == "https://ur.io" || strings.HasPrefix(href, "https://ur.io/") {
			return true
		}
		if href == protocolSite || strings.HasPrefix(href, protocolSite+"/") {
			return true
		}
		return strings.HasPrefix(href, "mailto:") && strings.HasSuffix(href, "@ur.io")
	}

	for _, sample := range samples {
		name := sample.template.Name()
		subject, bodyHtml, bodyText, err := RenderEmailTemplate(sample.template)
		if err != nil {
			t.Fatalf("%s: render: %v", name, err)
		}
		if subject == "" || subject != strings.TrimSpace(subject) || strings.ContainsAny(subject, "\r\n") {
			t.Errorf("%s: subject must be one trimmed line, got %q", name, subject)
		}
		if !strings.HasPrefix(bodyHtml, "<!DOCTYPE html") || !strings.Contains(bodyHtml, `lang="en"`) {
			t.Errorf("%s: html part is not a full document", name)
		}
		if strings.Contains(bodyHtml, "{{") || strings.Contains(bodyText, "{{") || strings.Contains(subject, "{{") {
			t.Errorf("%s: unresolved template action", name)
		}
		if len(bodyHtml) > 100*1024 {
			t.Errorf("%s: html part is %d bytes, over Gmail's clipping threshold", name, len(bodyHtml))
		}
		for _, match := range imageSrc.FindAllStringSubmatch(bodyHtml, -1) {
			if !allowedImages[match[1]] {
				t.Errorf("%s: image %q is not one of the hosted wordmarks (update monitor 19.2 if this is deliberate)", name, match[1])
			}
		}
		for _, match := range linkHref.FindAllStringSubmatch(bodyHtml, -1) {
			if !allowedLink(match[1]) {
				t.Errorf("%s: link %q leaves ur.io", name, match[1])
			}
		}
		if strings.Contains(bodyText, "<") || strings.Contains(bodyText, "&amp;") {
			t.Errorf("%s: text part contains markup or entities", name)
		}
		if !strings.Contains(bodyText, "https://ur.io/support") || !strings.Contains(bodyHtml, "https://ur.io/support") {
			t.Errorf("%s: footer support link missing", name)
		}

		sms, err := RenderSmsTemplate(sample.template)
		if err != nil {
			t.Fatalf("%s: sms: %v", name, err)
		}
		if sample.sms {
			if sms == "" || strings.Contains(sms, "\n") || len(sms) > 320 {
				t.Errorf("%s: sms body must be one short line, got %d bytes: %q", name, len(sms), sms)
			}
		} else if sms != bodyText {
			t.Errorf("%s: template without an sms body must fall back to the text part", name)
		}
	}
}

func TestEmailTemplatesEscapeSampleValues(t *testing.T) {
	_, bodyHtml, bodyText, err := RenderEmailTemplate(&X402ReceiptTemplate{
		Description: `1 TiB data pack & <b>more</b>`,
		PriceUsd:    5,
		Asset:       "USDC",
		Network:     "base",
		Transaction: "0xdeadbeef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bodyHtml, "<b>more</b>") || !strings.Contains(bodyHtml, "&amp; &lt;b&gt;more&lt;/b&gt;") {
		t.Errorf("html part did not escape the description: %s", bodyHtml)
	}
	if !strings.Contains(bodyText, "1 TiB data pack & <b>more</b>") {
		t.Errorf("text part altered the description")
	}
}

func TestEmailTemplatesDeepLinks(t *testing.T) {
	// with a wallet and a leaf: the claim route; eligible: the top200 route
	_, bodyHtml, bodyText, err := RenderEmailTemplate(&EpochEarningsTemplate{
		Epoch: 42, Points: 12.5, ShareBps: 71, Rank: 3, Total: 100,
		Top200Eligible: true, Top200Rank: 5, HasWallet: true,
		UnclaimedRao: big.NewInt(3_241_000_000), EpochEnd: server.NowUtc(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"https://ur.io/app/account/claim",
		"https://ur.io/app/account/top200",
		"https://ur.xyz",
		"3.2410 SN25α",
		"#3 of 100",
		"0.71%",
		"Top 200 · you qualify",
	} {
		if !strings.Contains(bodyHtml, want) || !strings.Contains(bodyText, want) {
			t.Errorf("epoch earnings email missing %q", want)
		}
	}
	// no wallet: the connect prompt links the claim route, no claim button
	_, bodyHtml, bodyText, err = RenderEmailTemplate(&EpochEarningsTemplate{
		Epoch: 42, Points: 12.5, Rank: 3, Total: 100, EpochEnd: server.NowUtc(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodyText, "Connect a Bittensor wallet") || strings.Contains(bodyText, "Claim your SN25α") {
		t.Errorf("no-wallet epoch earnings email should prompt for a wallet, not a claim")
	}
	if strings.Contains(bodyHtml, "Top 200") {
		t.Errorf("a network outside the cutoff must not get the Top 200 badge")
	}
	// bound: the uid line, no claim-your-spot button
	_, bodyHtml, _, err = RenderEmailTemplate(&EpochEarningsTemplate{
		Epoch: 42, Points: 1, Top200Bound: true, Top200Uid: 17, Top200Rank: 9, EpochEnd: server.NowUtc(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodyHtml, "Top 200 · UID 17 · rank #9") || strings.Contains(bodyHtml, "Claim your head spot") {
		t.Errorf("bound network should show its uid line only")
	}

	_, bodyHtml, bodyText, err = RenderEmailTemplate(&SubscriptionTransferBalanceCodeTemplate{
		Secret: "K7QX3M2PNB4DLZ8R9YWC5AGHT6", BalanceByteCount: model.Tib,
	})
	if err != nil {
		t.Fatal(err)
	}
	// the code rides in the fragment, which never reaches a server log; the
	// Balance codes screen prefills from it
	codeLink := "https://ur.io/app/balance-codes#code=K7QX3M2PNB4DLZ8R9YWC5AGHT6"
	if !strings.Contains(bodyHtml, `href="`+codeLink+`"`) || !strings.Contains(bodyText, codeLink) {
		t.Errorf("balance code deep link missing: want %s", codeLink)
	}
}
