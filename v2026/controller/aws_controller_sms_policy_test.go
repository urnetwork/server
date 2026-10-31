package controller

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A phone-number account receives account security by SMS and nothing else.
// AWS SMS Trust & Safety paused the account's SMS sending after the welcome
// email went out as a text (AWS case 177660552500706). This pins the policy
// the reply to that case commits to: the verification code, the password reset
// link and the password-changed notice reach the phone, and every other
// account message is email-only and never reaches the SMS provider.
func TestPhoneAccountsReceiveOnlySecuritySms(t *testing.T) {
	endpoint := newAwsEndpoint(t, "application/x-amz-json-1.0", `{"MessageId":"synthetic-message-id"}`)
	sender := endpoint.sender(5 * time.Second)
	const phone = "+1 555 555 0100"

	security := []Template{
		&AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"},
		&AuthPasswordResetTemplate{ResetCode: strings.Repeat("ab", 64)},
		&AuthPasswordSetTemplate{},
	}
	for _, template := range security {
		if !SmsAllowed(template) {
			t.Fatalf("%s: account security must be allowed by SMS", template.Name())
		}
		err := sendWithGuard(t, func() error {
			return sender.SendAccountMessageTemplate(phone, template)
		})
		if err != nil {
			t.Fatalf("%s: send error = %v, want none", template.Name(), err)
		}
		request := endpoint.receivedRequest()
		if request == nil || request.Header.Get("X-Amz-Target") != "PinpointSMSVoiceV2.SendTextMessage" {
			t.Fatalf("%s: the endpoint saw %v, want a Pinpoint SendTextMessage call", template.Name(), request)
		}
		var input struct {
			DestinationPhoneNumber string
			MessageBody            string
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		want, err := RenderSmsTemplate(template)
		if err != nil {
			t.Fatal(err)
		}
		// the short sms body, never the email text
		if input.DestinationPhoneNumber != "+15555550100" || input.MessageBody != want || strings.Contains(input.MessageBody, "\n") {
			t.Fatalf("%s: Pinpoint call = %+v, want the one-line sms body to +15555550100", template.Name(), input)
		}
	}

	emailOnly := []Template{
		&NetworkWelcomeTemplate{},
		&SubscriptionEndedTemplate{},
		&EpochEarningsTemplate{
			Epoch:        42,
			Points:       1234.5,
			ShareBps:     71,
			Rank:         17,
			Total:        5210,
			HasWallet:    true,
			UnclaimedRao: big.NewInt(3_241_000_000),
			EpochEnd:     server.NowUtc(),
		},
		&MissingWalletTemplate{PaymentId: server.NewId(), Payout: model.UsdToNanoCents(5.00)},
		&ProviderUpgradeNoticeTemplate{NetworkName: "Synthetic network"},
		&SubscriptionTransferBalanceCodeTemplate{Secret: "K7QX3M2PNB4DLZ8R9YWC5AGHT6", BalanceByteCount: 10 * model.Tib},
		&SubscriptionDataAppliedTemplate{Secret: "K7QX3M2PNB4DLZ8R9YWC5AGHT6", BalanceByteCount: model.Tib, NetworkName: "synthetic"},
		&X402ReceiptTemplate{
			Description:      "1 TiB data pack",
			PriceUsd:         5,
			Asset:            "USDC",
			Network:          "base",
			Transaction:      "0xdeadbeef",
			BalanceByteCount: model.Tib,
		},
	}
	for _, template := range emailOnly {
		if SmsAllowed(template) {
			t.Fatalf("%s: only account security may be sent by SMS", template.Name())
		}
		err := sendWithGuard(t, func() error {
			return sender.SendAccountMessageTemplate(phone, template)
		})
		if err != nil {
			t.Fatalf("%s: send error = %v, want none (an email-only message is not an sms failure)", template.Name(), err)
		}
		if request := endpoint.receivedRequest(); request != nil {
			t.Fatalf("%s reached the SMS provider (%s); a phone account must not receive it", template.Name(), request.Header.Get("X-Amz-Target"))
		}
	}
}
