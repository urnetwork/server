package controller

// `bringyourctl crypto credit-usdc` (support inbox 8228, 8229 and 8241; owner
// queue OQ-26): credits a USDC invoice from one recorded unfulfilled Solana
// payment, once the owner approved it. It is for a payment sent without its
// reference (memo-less) that the webhook could not match by its unique amount
// because an exchange added or took its own fee, so the amount is not the
// quote's. The webhook records such a payment as no_intent
// (solana_unfulfilled_payment); the reconciler credits a no_intent record only
// once its reference resolves to an open intent, which a memo-less payment
// never has; and `crypto credit` credits only xmr and zec from their receipts.
//
// The dry run reads the invoice, its network, its Solana Pay intent, the
// record and every record that names the signature as paid, in one read-only
// transaction, decides, and prints a digest of everything the credit acts on.
// --commit --expect=<digest> reads and decides again, refuses unless the digest
// is the same, and credits through the one credit (cryptoCreditIntentInConn)
// with the payment's signature: the invoice one-shot, the Solana intent's
// one-shot and the record's removal commit in one transaction. After it, a
// Helius redelivery of the payment finds its signature credited and is
// skipped, and a reconcile run clears a record that a redelivery racing the
// credit wrote again, by the same rule; neither credits anything.
//
// The value is the webhook's rule (cryptoUsdcCreditUsd): the price when the
// payment was sent by the quote's expiry, else what arrived, capped at the
// price. It is judged at the payment's on-chain time, or at its record time
// when the webhook carried none.
//
// It refuses a missing invoice or one not usdc_solana; one credited or void;
// one unlinked from its network unless it delivers a code; one whose network
// was deleted or whose Solana Pay intent is gone or paid; a missing record or
// one not no_intent; a signature already credited anywhere; a payment under
// the quote (an underpayment stays the owner's own case); and a payment its
// record ties to another network.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

const (
	// the payment can credit the invoice
	CryptoUsdcCreditStateCredit = "credit"
	// a commit credited it
	CryptoUsdcCreditStateCredited = "credited"
	// a check failed; nothing will be written
	CryptoUsdcCreditStateRefused = "refused"
)

var ErrCryptoUsdcCreditRefused = errors.New("crypto credit-usdc: refused; nothing was written")

var ErrCryptoUsdcCreditDigestMismatch = errors.New("crypto credit-usdc: --expect does not match the digest of this credit; nothing was written")

// The invoice one-shot lost: something credited or voided the invoice between
// the plan and the credit.
var ErrCryptoUsdcCreditNotCredited = errors.New("crypto credit-usdc: not credited: the invoice was credited or voided concurrently; nothing was written")

// CryptoUsdcCreditInvoice is the invoice as the plan shows it. It has no
// email and no status token.
type CryptoUsdcCreditInvoice struct {
	IntentId server.Id
	Asset    string
	Delivery string
	// nil for a code, and once unlinked
	NetworkId *server.Id
	// whether that network still exists
	NetworkExists bool
	// a plan (yearly, yearly_onboarding) or a data item
	Item              string
	Tier              string
	PriceUsdNanoCents model.NanoCents
	// the quote in micro-USDC: the price and its unique suffix
	AmountAtomic int64
	QuotedAt     time.Time
	ExpiresAt    time.Time
	CreateTime   time.Time
	CreditedAt   *time.Time
	VoidedAt     *time.Time
	UnlinkedAt   *time.Time
	// the linked Solana Pay intent: whether it is there, and the signature
	// that paid it
	SolanaIntentFound     bool
	SolanaIntentSignature *string
}

// CryptoUsdcCreditPayment is the recorded payment as the plan shows it.
type CryptoUsdcCreditPayment struct {
	Signature string
	Reason    string
	// what arrived, in micro-USDC
	AmountAtomic int64
	// the on-chain time, when the webhook carried one
	TransactionTime *time.Time
	// when the webhook recorded it
	RecordTime time.Time
	// the networks the record ties the payment to: its network, and every
	// network its match note names
	TiedNetworkIds []server.Id
}

// PaidAt is when the payment was sent: its on-chain time, else its record
// time, which is later.
func (self *CryptoUsdcCreditPayment) PaidAt() time.Time {
	if self.TransactionTime != nil {
		return *self.TransactionTime
	}
	return self.RecordTime
}

type CryptoUsdcCreditPlan struct {
	// when the plan was read
	Time      time.Time
	IntentId  server.Id
	Signature string
	State     string
	// why the credit is refused; empty unless the state is refused
	Refusals []string

	// nil when there is no such invoice
	Invoice *CryptoUsdcCreditInvoice
	// nil when no payment is recorded with the signature
	Payment *CryptoUsdcCreditPayment
	// the records that name the signature as paid
	SignatureCredits *model.SolanaSignatureCredits

	// the credit, set when the state is credit or credited: late when the
	// payment was sent after the quote's expiry, and the USD it credits
	Late         bool
	UsdNanoCents model.NanoCents
	// what arrived over the quote, in micro-USDC
	OverQuoteAtomic int64
	// the payment was sent before the invoice was made (beyond clock skew)
	SentBeforeInvoice bool
	// the digest of what the credit acts on, set when the state is credit or
	// credited
	Digest string

	// the invoice as read, for the commit's credit
	intent *model.CryptoPaymentIntent
}

func (self *CryptoUsdcCreditPlan) refuse(format string, args ...any) {
	self.Refusals = append(self.Refusals, fmt.Sprintf(format, args...))
}

// Everything the plan reads, in one transaction.
type cryptoUsdcCreditFacts struct {
	intentId  server.Id
	signature string
	// nil when there is no such invoice
	intent *model.CryptoPaymentIntent
	// whether the invoice's network exists
	networkExists bool
	// the invoice's Solana Pay intent, nil when it has none or it is gone
	solanaIntent *model.SolanaPaymentIntent
	// nil when no payment is recorded with the signature
	payment          *model.UnfulfilledSolanaPayment
	signatureCredits *model.SolanaSignatureCredits
}

// CryptoOperatorPlanUsdcCredit is the dry run. It reads in one read-only
// transaction and writes nothing.
func CryptoOperatorPlanUsdcCredit(
	ctx context.Context,
	intentId server.Id,
	signature string,
) (plan *CryptoUsdcCreditPlan, returnErr error) {
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			facts := readCryptoUsdcCreditFactsInTx(tx, ctx, intentId, signature)
			plan = cryptoDecideUsdcCredit(facts, server.NowUtc())
		}, pgx.ReadOnly)
	}, func(err error) {
		returnErr = err
	})
	return
}

// CryptoOperatorCommitUsdcCredit credits the invoice from the recorded
// payment when the digest of the plan it reads again is expect. It returns
// that plan, and an error when it refuses or fails, in which case nothing was
// written.
func CryptoOperatorCommitUsdcCredit(
	clientSession *session.ClientSession,
	intentId server.Id,
	signature string,
	expect string,
) (*CryptoUsdcCreditPlan, error) {
	plan, err := CryptoOperatorPlanUsdcCredit(clientSession.Ctx, intentId, signature)
	if err != nil {
		return plan, err
	}
	if plan.State != CryptoUsdcCreditStateCredit {
		return plan, ErrCryptoUsdcCreditRefused
	}
	if plan.Digest != expect {
		return plan, ErrCryptoUsdcCreditDigestMismatch
	}
	credited, err := cryptoCreditIntentInConn(nil, clientSession, plan.intent, &cryptoCredit{
		usdNanoCents:        plan.UsdNanoCents,
		solanaSignature:     plan.Signature,
		late:                plan.Late,
		resolvesUnfulfilled: true,
	})
	if err != nil {
		return plan, err
	}
	if !credited {
		return plan, ErrCryptoUsdcCreditNotCredited
	}
	plan.State = CryptoUsdcCreditStateCredited
	glog.Infof(
		"[cryptopay]credit-usdc: invoice %s credited from recorded payment %s (late %t), digest %s\n",
		plan.IntentId,
		plan.Signature,
		plan.Late,
		plan.Digest,
	)
	return plan, nil
}

func readCryptoUsdcCreditFactsInTx(
	tx server.PgTx,
	ctx context.Context,
	intentId server.Id,
	signature string,
) *cryptoUsdcCreditFacts {
	facts := &cryptoUsdcCreditFacts{
		intentId:  intentId,
		signature: signature,
		intent:    model.GetCryptoPaymentIntentInTx(tx, ctx, intentId),
	}
	if intent := facts.intent; intent != nil {
		if intent.NetworkId != nil {
			facts.networkExists = model.NetworkExistsInTx(tx, ctx, *intent.NetworkId)
		}
		if intent.SolanaReference != nil {
			facts.solanaIntent = model.GetSolanaPaymentIntentInTx(tx, ctx, *intent.SolanaReference)
		}
	}
	facts.payment = model.GetUnfulfilledSolanaPaymentInTx(tx, ctx, signature)
	facts.signatureCredits = model.GetSolanaSignatureCreditsInTx(tx, ctx, signature)
	return facts
}

// cryptoDecideUsdcCredit checks the facts and decides the credit. Pure.
func cryptoDecideUsdcCredit(facts *cryptoUsdcCreditFacts, now time.Time) *CryptoUsdcCreditPlan {
	plan := &CryptoUsdcCreditPlan{
		Time:             now,
		IntentId:         facts.intentId,
		Signature:        facts.signature,
		State:            CryptoUsdcCreditStateRefused,
		SignatureCredits: facts.signatureCredits,
	}
	formatTime := func(t time.Time) string {
		return t.UTC().Format(time.RFC3339)
	}
	formatUsdc := func(amountAtomic int64) string {
		return cryptoFormatAtomic(amountAtomic, cryptoAssetDecimals(model.CryptoAssetUsdcSolana)) + " USDC"
	}

	intent := facts.intent
	usdc := intent != nil && intent.Asset == model.CryptoAssetUsdcSolana
	if intent == nil {
		plan.refuse("no invoice has id %s", facts.intentId)
	} else {
		plan.intent = intent
		plan.Invoice = cryptoUsdcCreditInvoiceView(facts)
		if !usdc {
			plan.refuse("the invoice is %s, not %s: `crypto credit` credits it from its receipts", intent.Asset, model.CryptoAssetUsdcSolana)
		}
		if intent.CreditedAt != nil {
			by := ""
			if facts.solanaIntent != nil && facts.solanaIntent.TxSignature != nil && *facts.solanaIntent.TxSignature == facts.signature {
				by = ", by this payment"
			}
			plan.refuse("the invoice is already credited (%s)%s", formatTime(*intent.CreditedAt), by)
		}
		if intent.VoidedAt != nil {
			plan.refuse("the invoice is void (%s): it is never credited", formatTime(*intent.VoidedAt))
		}
		if intent.Delivery != model.CryptoDeliveryCode && intent.NetworkId == nil {
			plan.refuse("the invoice is unlinked from its network (retention): grant by hand")
		}
		if intent.NetworkId != nil && !facts.networkExists {
			plan.refuse("the invoice's network %s no longer exists", *intent.NetworkId)
		}
		if usdc && intent.CreditedAt == nil {
			switch {
			case intent.SolanaReference == nil || facts.solanaIntent == nil:
				plan.refuse("the invoice has no Solana Pay intent left to consume")
			case facts.solanaIntent.TxSignature != nil:
				plan.refuse("the invoice's Solana Pay intent is already paid by %s", *facts.solanaIntent.TxSignature)
			}
			if intent.AmountAtomic <= 0 {
				plan.refuse("the invoice has no quote")
			}
		}
	}

	payment := facts.payment
	if payment == nil {
		plan.refuse("no unfulfilled Solana payment is recorded with signature %s", facts.signature)
	} else {
		plan.Payment = cryptoUsdcCreditPaymentView(payment)
		if payment.Reason != model.SolanaUnfulfilledReasonNoIntent {
			reason := fmt.Sprintf("the payment is recorded as %s, not %s", payment.Reason, model.SolanaUnfulfilledReasonNoIntent)
			if payment.Reason == model.SolanaUnfulfilledReasonUnderpaid {
				reason += ": an underpayment stays the owner's own case"
			}
			plan.refuse("%s", reason)
		}
	}

	if credits := facts.signatureCredits; credits != nil && credits.Credited() {
		places := []string{}
		if 0 < credits.IntentCount {
			places = append(places, "it paid a Solana Pay intent")
		}
		if 0 < credits.RenewalCount {
			places = append(places, fmt.Sprintf("%d solana or x402 renewals name it", credits.RenewalCount))
		}
		if 0 < credits.BalanceCount {
			places = append(places, fmt.Sprintf("%d balances were bought with it", credits.BalanceCount))
		}
		plan.refuse("the signature is already credited: %s", strings.Join(places, "; "))
	}

	if usdc && plan.Payment != nil {
		if plan.Payment.AmountAtomic < intent.AmountAtomic {
			plan.refuse(
				"the payment of %s is under the quote of %s: an underpayment stays the owner's own case",
				formatUsdc(plan.Payment.AmountAtomic),
				formatUsdc(intent.AmountAtomic),
			)
		}
		for _, networkId := range plan.Payment.TiedNetworkIds {
			switch {
			case intent.NetworkId == nil:
				plan.refuse("the payment's record ties it to network %s, and the invoice has no network", networkId)
			case *intent.NetworkId != networkId:
				plan.refuse("the payment's record ties it to network %s, not the invoice's network %s", networkId, *intent.NetworkId)
			}
		}
	}

	if 0 < len(plan.Refusals) {
		return plan
	}
	paidAt := plan.Payment.PaidAt()
	plan.UsdNanoCents, plan.Late = cryptoUsdcCreditUsd(intent, payment.TokenAmountUsd, paidAt)
	plan.OverQuoteAtomic = plan.Payment.AmountAtomic - intent.AmountAtomic
	plan.SentBeforeInvoice = paidAt.Before(intent.CreateTime.Add(-solanaMemolessClockSkew))
	plan.State = CryptoUsdcCreditStateCredit
	plan.Digest = cryptoUsdcCreditDigest(facts.digestLines(plan.UsdNanoCents, plan.Late))
	return plan
}

func cryptoUsdcCreditInvoiceView(facts *cryptoUsdcCreditFacts) *CryptoUsdcCreditInvoice {
	intent := facts.intent
	view := &CryptoUsdcCreditInvoice{
		IntentId:          intent.IntentId,
		Asset:             intent.Asset,
		Delivery:          intent.Delivery,
		NetworkId:         intent.NetworkId,
		NetworkExists:     facts.networkExists,
		Item:              intent.Item,
		Tier:              intent.Tier,
		PriceUsdNanoCents: intent.PriceUsdNanoCents,
		AmountAtomic:      intent.AmountAtomic,
		QuotedAt:          intent.QuotedAt,
		ExpiresAt:         intent.ExpiresAt,
		CreateTime:        intent.CreateTime,
		CreditedAt:        intent.CreditedAt,
		VoidedAt:          intent.VoidedAt,
		UnlinkedAt:        intent.UnlinkedAt,
	}
	if facts.solanaIntent != nil {
		view.SolanaIntentFound = true
		view.SolanaIntentSignature = facts.solanaIntent.TxSignature
	}
	return view
}

func cryptoUsdcCreditPaymentView(payment *model.UnfulfilledSolanaPayment) *CryptoUsdcCreditPayment {
	view := &CryptoUsdcCreditPayment{
		Signature:       payment.TxSignature,
		Reason:          payment.Reason,
		AmountAtomic:    model.SolanaUsdToMicro(payment.TokenAmountUsd),
		TransactionTime: payment.TransactionTime,
		RecordTime:      payment.RecordTime,
		TiedNetworkIds:  []server.Id{},
	}
	tie := func(networkId server.Id) {
		if networkId != (server.Id{}) && !slices.Contains(view.TiedNetworkIds, networkId) {
			view.TiedNetworkIds = append(view.TiedNetworkIds, networkId)
		}
	}
	if payment.NetworkId != nil {
		tie(*payment.NetworkId)
	}
	if payment.MatchNote != nil {
		for _, networkId := range model.SolanaMatchNoteNetworkIds(*payment.MatchNote) {
			tie(networkId)
		}
	}
	return view
}

// The canonical text of the digest, one fact per line: every field of the
// invoice and of the record the credit acts on or depends on, and the credit.
// Never printed: the Solana reference is in it.
func (self *cryptoUsdcCreditFacts) digestLines(usdNanoCents model.NanoCents, late bool) []string {
	intent := self.intent
	payment := self.payment
	formatTime := func(t time.Time) string {
		return t.UTC().Format(time.RFC3339Nano)
	}
	optionalTime := func(t *time.Time) string {
		if t == nil {
			return "none"
		}
		return formatTime(*t)
	}
	optionalId := func(id *server.Id) string {
		if id == nil {
			return "none"
		}
		return id.String()
	}
	optionalText := func(text *string) string {
		if text == nil {
			return "none"
		}
		return strconv.Quote(*text)
	}
	return []string{
		"urnetwork crypto credit-usdc v1",
		"invoice " + intent.IntentId.String(),
		"asset " + intent.Asset,
		"delivery " + intent.Delivery,
		"network " + optionalId(intent.NetworkId),
		"item " + strconv.Quote(intent.Item),
		"tier " + strconv.Quote(intent.Tier),
		fmt.Sprintf("price_nano_cents %d", intent.PriceUsdNanoCents),
		fmt.Sprintf("quote_atomic %d", intent.AmountAtomic),
		"quoted_at " + formatTime(intent.QuotedAt),
		"expires_at " + formatTime(intent.ExpiresAt),
		"create_time " + formatTime(intent.CreateTime),
		"solana_reference " + optionalText(intent.SolanaReference),
		"payment " + strconv.Quote(payment.TxSignature),
		"reason " + strconv.Quote(payment.Reason),
		fmt.Sprintf("amount_atomic %d", model.SolanaUsdToMicro(payment.TokenAmountUsd)),
		"transaction_time " + optionalTime(payment.TransactionTime),
		"record_time " + formatTime(payment.RecordTime),
		"payment_network " + optionalId(payment.NetworkId),
		"payment_reference " + optionalText(payment.PaymentReference),
		"match_note " + optionalText(payment.MatchNote),
		fmt.Sprintf("credit_nano_cents %d late %t", usdNanoCents, late),
	}
}

// The sha256, in hex, of the canonical text.
func cryptoUsdcCreditDigest(lines []string) string {
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}
