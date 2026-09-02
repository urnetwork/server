package controller

import (
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
	// the one link that legitimately leaves ur.io is the payout's on-chain
	// transaction, whose base comes from the payment data (ExplorerBasePath)
	const explorerBasePath = "https://explorer.solana.com/tx"
	samples := []struct {
		template Template
		// sms is whether the template carries its own short body for phone accounts
		sms bool
	}{
		{&AuthVerifyTemplate{VerifyCode: "482917"}, true},
		{&AuthPasswordResetTemplate{ResetCode: strings.Repeat("ab", 64)}, true},
		{&AuthPasswordSetTemplate{}, true},
		{&NetworkWelcomeTemplate{}, true},
		{&SendPaymentTemplate{
			PaymentId:          server.NewId(),
			TxHash:             "5UyQnJf4h2Lw9xkT7ZbA3vRcPmN6sDqE1yG8tHrK2oXbVjW4eLpSa9dCz3nMfQ7u",
			ExplorerBasePath:   explorerBasePath,
			Blockchain:         "Solana",
			DestinationAddress: "7Gx4kQ9pT2nVb8sLmR3wYcJ6dHfA5eN1zK2uP9tXq4Wb",
			AmountUsd:          "5.00",
			PaymentCreatedAt:   server.NowUtc(),
		}, true},
		{&MissingWalletTemplate{PaymentId: server.NewId(), AmountUsd: "5.00"}, true},
		{&SubscriptionTransferBalanceCodeTemplate{Secret: "K7QX3M2PNB4DLZ8R9YWC5AGHT6", BalanceByteCount: 10 * model.Tib}, false},
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
		if strings.HasPrefix(href, explorerBasePath+"/") {
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
	paymentId := server.NewId()
	_, bodyHtml, bodyText, err := RenderEmailTemplate(&SendPaymentTemplate{
		PaymentId: paymentId, TxHash: "tx", ExplorerBasePath: "https://explorer.solana.com/tx",
		Blockchain: "Solana", DestinationAddress: "addr", AmountUsd: "5.00", PaymentCreatedAt: server.NowUtc(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// PayoutDetail on ur.io reads ?id= and matches it against payment_id in
	// /account/payments, which prints server.Id the same way
	payoutLink := "https://ur.io/app/account/payouts?id=" + paymentId.String()
	if !strings.Contains(bodyHtml, `href="`+payoutLink+`"`) || !strings.Contains(bodyText, payoutLink) {
		t.Errorf("payout deep link missing: want %s", payoutLink)
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
