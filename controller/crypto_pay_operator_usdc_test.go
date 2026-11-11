package controller

// Pure tests of `bringyourctl crypto credit-usdc`'s plan
// (crypto_pay_operator_usdc.go): the decision from the facts, the shared
// on-time and late rule, the digest, and what the plan carries. Fixed clock,
// fixed facts, no Postgres. The database-backed tests are in
// crypto_pay_operator_usdc_db_test.go.

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var cryptoUsdcCreditTestQuotedAt = time.Date(2026, 11, 7, 5, 27, 20, 0, time.UTC)

// The facts of support inbox 8228: a year of regional Pro quoted 4.005505
// USDC for an hour, and 4.093109 USDC sent without the reference 32 minutes
// later, recorded no_intent because no intent was quoted that amount.
func cryptoUsdcCreditTestFacts() *cryptoUsdcCreditFacts {
	networkId := server.RequireParseId("01a1aebf-f4a5-9b31-e011-df52b3850ae7")
	reference := "SyntheticReference1111111111111111111111111"
	sentAt := cryptoUsdcCreditTestQuotedAt.Add(32*time.Minute + 9*time.Second)
	note := "memo-less: no intent was quoted 4.093109 USDC"
	sender := "Sender1111111111111111111111111111111111111"
	return &cryptoUsdcCreditFacts{
		intentId:  server.RequireParseId("01a1aec3-338d-4e20-5da1-6f179e570654"),
		signature: "SyntheticSignature1111111111111111111111111111111111111111111111111111111111111111111",
		intent: &model.CryptoPaymentIntent{
			IntentId:                 server.RequireParseId("01a1aec3-338d-4e20-5da1-6f179e570654"),
			StatusTokenHash:          []byte("synthetic-status-token-hash"),
			Asset:                    model.CryptoAssetUsdcSolana,
			Delivery:                 model.CryptoDeliveryAccount,
			NetworkId:                &networkId,
			Item:                     model.SolanaPlanYearly,
			Tier:                     "regional",
			PriceUsdNanoCents:        model.UsdToNanoCents(4),
			RegularPriceUsdNanoCents: model.UsdToNanoCents(4),
			Email:                    "buyer@synthetic.example",
			Address:                  solanaPaymentRecipient(),
			SolanaReference:          &reference,
			AmountAtomic:             4_005_505,
			RateUsd:                  1,
			QuotedAt:                 cryptoUsdcCreditTestQuotedAt,
			ExpiresAt:                cryptoUsdcCreditTestQuotedAt.Add(time.Hour),
			CreateTime:               cryptoUsdcCreditTestQuotedAt,
		},
		networkExists: true,
		solanaIntent: &model.SolanaPaymentIntent{
			PaymentReference:  reference,
			NetworkId:         networkId,
			ExpectedAmountUsd: 4.005505,
			SubscriptionPlan:  model.SolanaPlanYearly,
			CreatedAt:         cryptoUsdcCreditTestQuotedAt,
		},
		payment: &model.UnfulfilledSolanaPayment{
			TxSignature:         "SyntheticSignature1111111111111111111111111111111111111111111111111111111111111111111",
			Reason:              model.SolanaUnfulfilledReasonNoIntent,
			TokenAmountUsd:      4.093109,
			ReferenceCandidates: []string{sender, solanaPaymentRecipient()},
			TransactionTime:     &sentAt,
			SenderAccount:       &sender,
			MatchNote:           &note,
			RecordTime:          sentAt.Add(2 * time.Second),
		},
		signatureCredits: &model.SolanaSignatureCredits{},
	}
}

var cryptoUsdcCreditTestNow = time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC)

// The 8228 case credits: on time (sent before the quote expired), at the
// price, with the overpayment shown and the digest set.
func TestCryptoUsdcCreditDecisionCreditsAnOverpaidMemolessPayment(t *testing.T) {
	plan := cryptoDecideUsdcCredit(cryptoUsdcCreditTestFacts(), cryptoUsdcCreditTestNow)
	connect.AssertEqual(t, plan.State, CryptoUsdcCreditStateCredit)
	connect.AssertEqual(t, len(plan.Refusals), 0)
	connect.AssertEqual(t, plan.Late, false)
	connect.AssertEqual(t, plan.UsdNanoCents, model.UsdToNanoCents(4))
	connect.AssertEqual(t, plan.OverQuoteAtomic, int64(87_604))
	connect.AssertEqual(t, plan.SentBeforeInvoice, false)
	connect.AssertEqual(t, regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(plan.Digest), true)
	connect.AssertEqual(t, plan.Invoice.AmountAtomic, int64(4_005_505))
	connect.AssertEqual(t, plan.Invoice.NetworkExists, true)
	connect.AssertEqual(t, plan.Payment.AmountAtomic, int64(4_093_109))
	connect.AssertEqual(t, plan.Payment.PaidAt(), *cryptoUsdcCreditTestFacts().payment.TransactionTime)
	connect.AssertEqual(t, len(plan.Payment.TiedNetworkIds), 0)
	// the commit credits the invoice the plan read
	connect.AssertEqual(t, plan.intent.IntentId, plan.IntentId)
}

// The webhook's rule, shared: the price when paid by the quote's expiry, else
// what arrived capped at the price. The plan judges the payment at its
// on-chain time, or at its record time when the webhook carried none.
func TestCryptoUsdcCreditUsdIsTheWebhooksRule(t *testing.T) {
	intent := cryptoUsdcCreditTestFacts().intent
	usd, late := cryptoUsdcCreditUsd(intent, 4.093109, intent.ExpiresAt)
	connect.AssertEqual(t, late, false)
	connect.AssertEqual(t, usd, intent.PriceUsdNanoCents)
	usd, late = cryptoUsdcCreditUsd(intent, 4.093109, intent.ExpiresAt.Add(time.Second))
	connect.AssertEqual(t, late, true)
	connect.AssertEqual(t, usd, intent.PriceUsdNanoCents)
	usd, late = cryptoUsdcCreditUsd(intent, 3.5, intent.ExpiresAt.Add(time.Second))
	connect.AssertEqual(t, late, true)
	connect.AssertEqual(t, usd, model.UsdToNanoCents(3.5))

	// sent after the expiry: late, what arrived capped at the price
	facts := cryptoUsdcCreditTestFacts()
	sentLate := facts.intent.ExpiresAt.Add(3 * time.Hour)
	facts.payment.TransactionTime = &sentLate
	plan := cryptoDecideUsdcCredit(facts, cryptoUsdcCreditTestNow)
	connect.AssertEqual(t, plan.State, CryptoUsdcCreditStateCredit)
	connect.AssertEqual(t, plan.Late, true)
	connect.AssertEqual(t, plan.UsdNanoCents, model.UsdToNanoCents(4))

	// no on-chain time: the record time decides, and it is later
	facts = cryptoUsdcCreditTestFacts()
	facts.payment.TransactionTime = nil
	facts.payment.RecordTime = facts.intent.ExpiresAt
	plan = cryptoDecideUsdcCredit(facts, cryptoUsdcCreditTestNow)
	connect.AssertEqual(t, plan.Late, false)
	facts.payment.RecordTime = facts.intent.ExpiresAt.Add(time.Second)
	plan = cryptoDecideUsdcCredit(facts, cryptoUsdcCreditTestNow)
	connect.AssertEqual(t, plan.Late, true)

	// when the command runs does not matter
	facts = cryptoUsdcCreditTestFacts()
	connect.AssertEqual(t, cryptoDecideUsdcCredit(facts, facts.intent.ExpiresAt.Add(30*24*time.Hour)).Late, false)

	// sent before the invoice was made: credited, and noted
	facts = cryptoUsdcCreditTestFacts()
	early := facts.intent.CreateTime.Add(-solanaMemolessClockSkew - time.Second)
	facts.payment.TransactionTime = &early
	plan = cryptoDecideUsdcCredit(facts, cryptoUsdcCreditTestNow)
	connect.AssertEqual(t, plan.State, CryptoUsdcCreditStateCredit)
	connect.AssertEqual(t, plan.SentBeforeInvoice, true)

	// exactly the quote is not under it
	facts = cryptoUsdcCreditTestFacts()
	facts.payment.TokenAmountUsd = 4.005505
	plan = cryptoDecideUsdcCredit(facts, cryptoUsdcCreditTestNow)
	connect.AssertEqual(t, plan.State, CryptoUsdcCreditStateCredit)
	connect.AssertEqual(t, plan.OverQuoteAtomic, int64(0))
}

// Every refusal the owner listed, and the credit's own preconditions: each
// refuses with its reason and no digest.
func TestCryptoUsdcCreditDecisionRefusals(t *testing.T) {
	otherNetworkId := server.RequireParseId("01a1aebf-0000-4000-8000-000000000001")
	otherSignature := "OtherSignature1111111111111111111111111111111111111111111111111111111111111111111111"
	cases := []struct {
		name    string
		mutate  func(facts *cryptoUsdcCreditFacts)
		refusal string
	}{
		{"the invoice is missing", func(facts *cryptoUsdcCreditFacts) {
			facts.intent = nil
			facts.solanaIntent = nil
		}, "no invoice has id"},
		{"the invoice is not usdc_solana", func(facts *cryptoUsdcCreditFacts) {
			facts.intent.Asset = model.CryptoAssetXmr
		}, "the invoice is xmr, not usdc_solana"},
		{"the invoice is credited", func(facts *cryptoUsdcCreditFacts) {
			creditedAt := cryptoUsdcCreditTestQuotedAt.Add(time.Minute)
			facts.intent.CreditedAt = &creditedAt
			facts.solanaIntent.TxSignature = &otherSignature
		}, "the invoice is already credited (2026-11-07T05:28:20Z)"},
		{"the invoice is credited by this payment", func(facts *cryptoUsdcCreditFacts) {
			creditedAt := cryptoUsdcCreditTestQuotedAt.Add(time.Minute)
			facts.intent.CreditedAt = &creditedAt
			facts.solanaIntent.TxSignature = &facts.signature
			facts.signatureCredits.IntentCount = 1
		}, "already credited (2026-11-07T05:28:20Z), by this payment"},
		{"the invoice is void", func(facts *cryptoUsdcCreditFacts) {
			voidedAt := cryptoUsdcCreditTestQuotedAt.Add(time.Minute)
			facts.intent.VoidedAt = &voidedAt
		}, "the invoice is void"},
		{"the invoice is unlinked from its network", func(facts *cryptoUsdcCreditFacts) {
			unlinkedAt := cryptoUsdcCreditTestQuotedAt.Add(31 * 24 * time.Hour)
			facts.intent.NetworkId = nil
			facts.intent.UnlinkedAt = &unlinkedAt
			facts.networkExists = false
		}, "the invoice is unlinked from its network"},
		{"the invoice's network was deleted", func(facts *cryptoUsdcCreditFacts) {
			facts.networkExists = false
		}, "no longer exists"},
		{"the invoice's Solana Pay intent is gone", func(facts *cryptoUsdcCreditFacts) {
			facts.solanaIntent = nil
		}, "no Solana Pay intent left to consume"},
		{"the invoice's Solana Pay intent is paid", func(facts *cryptoUsdcCreditFacts) {
			facts.solanaIntent.TxSignature = &otherSignature
		}, "the invoice's Solana Pay intent is already paid by " + otherSignature},
		{"the payment is not recorded", func(facts *cryptoUsdcCreditFacts) {
			facts.payment = nil
		}, "no unfulfilled Solana payment is recorded with signature"},
		{"the payment is recorded underpaid", func(facts *cryptoUsdcCreditFacts) {
			facts.payment.Reason = model.SolanaUnfulfilledReasonUnderpaid
		}, "recorded as underpaid, not no_intent: an underpayment stays the owner's own case"},
		{"the signature paid a Solana Pay intent", func(facts *cryptoUsdcCreditFacts) {
			facts.signatureCredits.IntentCount = 1
		}, "the signature is already credited: it paid a Solana Pay intent"},
		{"a renewal names the signature", func(facts *cryptoUsdcCreditFacts) {
			facts.signatureCredits.RenewalCount = 1
		}, "the signature is already credited: 1 solana or x402 renewals name it"},
		{"a balance was bought with the signature", func(facts *cryptoUsdcCreditFacts) {
			facts.signatureCredits.BalanceCount = 1
		}, "the signature is already credited: 1 balances were bought with it"},
		{"the payment is one micro-USDC under the quote", func(facts *cryptoUsdcCreditFacts) {
			facts.payment.TokenAmountUsd = 4.005504
		}, "the payment of 4.005504 USDC is under the quote of 4.005505 USDC"},
		{"the plain price without the suffix", func(facts *cryptoUsdcCreditFacts) {
			facts.payment.TokenAmountUsd = 4
		}, "is under the quote of 4.005505 USDC"},
		{"the record names another network", func(facts *cryptoUsdcCreditFacts) {
			facts.payment.NetworkId = &otherNetworkId
		}, "the payment's record ties it to network " + otherNetworkId.String() + ", not the invoice's network"},
		{"the match note names another network", func(facts *cryptoUsdcCreditFacts) {
			note := "memo-less: intent SomeReference (network " + strings.ToUpper(otherNetworkId.String()) + ", plan yearly, quoted 4.093109 USDC) was already paid by " + otherSignature
			facts.payment.MatchNote = &note
		}, "the payment's record ties it to network " + otherNetworkId.String() + ", not the invoice's network"},
		{"a code invoice and a payment tied to a network", func(facts *cryptoUsdcCreditFacts) {
			facts.intent.Delivery = model.CryptoDeliveryCode
			facts.intent.NetworkId = nil
			facts.networkExists = false
			facts.payment.NetworkId = &otherNetworkId
		}, "the payment's record ties it to network " + otherNetworkId.String() + ", and the invoice has no network"},
		{"the invoice has no quote", func(facts *cryptoUsdcCreditFacts) {
			facts.intent.AmountAtomic = 0
		}, "the invoice has no quote"},
	}
	for _, c := range cases {
		facts := cryptoUsdcCreditTestFacts()
		c.mutate(facts)
		plan := cryptoDecideUsdcCredit(facts, cryptoUsdcCreditTestNow)
		if plan.State != CryptoUsdcCreditStateRefused || plan.Digest != "" {
			t.Fatalf("%s: state %s, digest %q, want refused without a digest", c.name, plan.State, plan.Digest)
		}
		found := false
		for _, refusal := range plan.Refusals {
			if strings.Contains(refusal, c.refusal) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: refusals %q do not say %q", c.name, plan.Refusals, c.refusal)
		}
	}

	// not refusals: a code invoice has no network to be unlinked from; a note
	// that names the invoice's own network, a candidate with no network, or an
	// unlinked one ties the payment to no other network
	for _, mutate := range []func(facts *cryptoUsdcCreditFacts){
		func(facts *cryptoUsdcCreditFacts) {
			facts.intent.Delivery = model.CryptoDeliveryCode
			facts.intent.NetworkId = nil
			facts.networkExists = false
		},
		func(facts *cryptoUsdcCreditFacts) {
			note := "memo-less: intent R (network " + facts.intent.NetworkId.String() + ", plan yearly, quoted 4.093109 USDC): sent at 2026-11-07T06:30:00Z, after the quote expired at 2026-11-07T06:27:20Z"
			facts.payment.MatchNote = &note
			facts.payment.NetworkId = facts.intent.NetworkId
		},
		func(facts *cryptoUsdcCreditFacts) {
			note := "memo-less: intent R (network 00000000-0000-0000-0000-000000000000, plan data_1tib, quoted 4.093109 USDC) has no network"
			facts.payment.MatchNote = &note
		},
		func(facts *cryptoUsdcCreditFacts) {
			note := "memo-less: intent R (network (unlinked), plan yearly, quoted 4.093109 USDC) was already paid by S"
			facts.payment.MatchNote = &note
		},
	} {
		facts := cryptoUsdcCreditTestFacts()
		mutate(facts)
		plan := cryptoDecideUsdcCredit(facts, cryptoUsdcCreditTestNow)
		if plan.State != CryptoUsdcCreditStateCredit {
			t.Fatalf("refused: %q", plan.Refusals)
		}
	}
}

// The digest is the sha256 of the canonical text, pinned here so a change is
// deliberate. It binds every fact the credit acts on and the credit itself,
// and not when the plan was read.
func TestCryptoUsdcCreditDigest(t *testing.T) {
	facts := cryptoUsdcCreditTestFacts()
	plan := cryptoDecideUsdcCredit(facts, cryptoUsdcCreditTestNow)
	lines := facts.digestLines(plan.UsdNanoCents, plan.Late)
	connect.AssertEqual(t, strings.Join(lines, "\n"), strings.Join([]string{
		"urnetwork crypto credit-usdc v1",
		"invoice 01a1aec3-338d-4e20-5da1-6f179e570654",
		"asset usdc_solana",
		"delivery account",
		"network 01a1aebf-f4a5-9b31-e011-df52b3850ae7",
		`item "yearly"`,
		`tier "regional"`,
		"price_nano_cents 4000000000",
		"quote_atomic 4005505",
		"quoted_at 2026-11-07T05:27:20Z",
		"expires_at 2026-11-07T06:27:20Z",
		"create_time 2026-11-07T05:27:20Z",
		`solana_reference "SyntheticReference1111111111111111111111111"`,
		`payment "SyntheticSignature1111111111111111111111111111111111111111111111111111111111111111111"`,
		`reason "no_intent"`,
		"amount_atomic 4093109",
		"transaction_time 2026-11-07T05:59:29Z",
		"record_time 2026-11-07T05:59:31Z",
		"payment_network none",
		"payment_reference none",
		`match_note "memo-less: no intent was quoted 4.093109 USDC"`,
		"credit_nano_cents 4000000000 late false",
	}, "\n"))
	connect.AssertEqual(t, plan.Digest, cryptoUsdcCreditDigest(lines))

	// not when it was read
	connect.AssertEqual(t, cryptoDecideUsdcCredit(cryptoUsdcCreditTestFacts(), cryptoUsdcCreditTestNow.Add(48*time.Hour)).Digest, plan.Digest)

	// each fact moves it
	digests := map[string]string{"": plan.Digest}
	for name, mutate := range map[string]func(facts *cryptoUsdcCreditFacts){
		"invoice": func(facts *cryptoUsdcCreditFacts) {
			facts.intentId = server.NewId()
			facts.intent.IntentId = facts.intentId
		},
		"item":  func(facts *cryptoUsdcCreditFacts) { facts.intent.Item = model.SolanaPlanYearlyOnboarding },
		"tier":  func(facts *cryptoUsdcCreditFacts) { facts.intent.Tier = "standard" },
		"price": func(facts *cryptoUsdcCreditFacts) { facts.intent.PriceUsdNanoCents = model.UsdToNanoCents(3.99) },
		"quote": func(facts *cryptoUsdcCreditFacts) { facts.intent.AmountAtomic = 4_005_506 },
		"quoted_at": func(facts *cryptoUsdcCreditFacts) {
			facts.intent.QuotedAt = facts.intent.QuotedAt.Add(time.Microsecond)
		},
		"expires_at": func(facts *cryptoUsdcCreditFacts) {
			facts.intent.ExpiresAt = facts.intent.ExpiresAt.Add(time.Minute)
		},
		"create_time": func(facts *cryptoUsdcCreditFacts) {
			facts.intent.CreateTime = facts.intent.CreateTime.Add(-time.Minute)
		},
		"reference": func(facts *cryptoUsdcCreditFacts) {
			reference := "AnotherReference"
			facts.intent.SolanaReference = &reference
		},
		"signature": func(facts *cryptoUsdcCreditFacts) {
			facts.signature = "AnotherSignature"
			facts.payment.TxSignature = facts.signature
		},
		"amount": func(facts *cryptoUsdcCreditFacts) { facts.payment.TokenAmountUsd = 4.09311 },
		"transaction_time": func(facts *cryptoUsdcCreditFacts) {
			sentAt := facts.payment.TransactionTime.Add(time.Second)
			facts.payment.TransactionTime = &sentAt
		},
		"record_time": func(facts *cryptoUsdcCreditFacts) {
			facts.payment.RecordTime = facts.payment.RecordTime.Add(time.Second)
		},
		"payment_network": func(facts *cryptoUsdcCreditFacts) { facts.payment.NetworkId = facts.intent.NetworkId },
		"payment_reference": func(facts *cryptoUsdcCreditFacts) {
			reference := "AnyReference"
			facts.payment.PaymentReference = &reference
		},
		"match_note": func(facts *cryptoUsdcCreditFacts) { facts.payment.MatchNote = nil },
		"late": func(facts *cryptoUsdcCreditFacts) {
			sentAt := facts.intent.ExpiresAt.Add(time.Second)
			facts.payment.TransactionTime = &sentAt
		},
		"code": func(facts *cryptoUsdcCreditFacts) {
			facts.intent.Delivery = model.CryptoDeliveryCode
			facts.intent.NetworkId = nil
		},
	} {
		facts := cryptoUsdcCreditTestFacts()
		mutate(facts)
		moved := cryptoDecideUsdcCredit(facts, cryptoUsdcCreditTestNow)
		if moved.State != CryptoUsdcCreditStateCredit {
			t.Fatalf("%s: refused: %q", name, moved.Refusals)
		}
		for other, digest := range digests {
			if digest == moved.Digest {
				t.Fatalf("%s: the digest is %q's", name, other)
			}
		}
		digests[name] = moved.Digest
	}
}

// The plan prints no email and no status token: none of its exported types
// carries one, nor the Solana reference.
func TestCryptoUsdcCreditPlanCarriesNoEmailOrToken(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)email|token|hash|reference|secret`)
	for _, value := range []any{CryptoUsdcCreditPlan{}, CryptoUsdcCreditInvoice{}, CryptoUsdcCreditPayment{}} {
		valueType := reflect.TypeOf(value)
		for i := range valueType.NumField() {
			field := valueType.Field(i)
			if field.IsExported() && forbidden.MatchString(field.Name) {
				t.Fatalf("%s.%s may carry an email, a token or a reference", valueType.Name(), field.Name)
			}
		}
	}
}

// The networks a match note names, once each, without the zero id and in any
// case, and none in a note the unlink rewrote.
func TestSolanaMatchNoteNetworkIds(t *testing.T) {
	a := server.NewId()
	b := server.NewId()
	note := "memo-less: intent R1 (network " + a.String() + ", plan yearly) and intent R2 (network " + strings.ToUpper(b.String()) +
		", plan yearly) and again (network " + a.String() + ") and (network 00000000-0000-0000-0000-000000000000) and (network (unlinked))"
	connect.AssertEqual(t, model.SolanaMatchNoteNetworkIds(note), []server.Id{a, b})
	connect.AssertEqual(t, model.SolanaMatchNoteNetworkIds("memo-less: no intent was quoted 4.093109 USDC"), []server.Id{})
}
