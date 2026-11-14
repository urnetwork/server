package controller

// The status reads check the chain for a USDC payment (support inbox 7247, RC-D
// of support/plans/paid-not-applied-1112.md).
//
// A USDC on Solana payment credits when the Helius webhook delivers its
// transaction, or, when that delivery is lost, within the hour, when the
// payment reconcile's chain leg reads the intent's reference
// (reconcileSolanaChainReferences). Every other coin is read from the chain by
// its scanner every scan interval; USDC was the one payment that could only be
// found by a webhook. A buyer who paid while Helius was down, or whose
// delivery failed, waited up to an hour on "waiting for payment".
//
// The status reads the buyer's screen already polls now look themselves: POST
// /pay/crypto/status for a USDC invoice (every app's invoice screen through
// the sdk's CryptoInvoiceViewController, and ur.io's), and POST
// /pay/data/solana-status for a USDC data purchase by reference. While the
// intent is open, a read reads the chain for its reference, at most once per
// the settings' interval per reference, and acts on every finalized
// transaction that names it and that nothing has handled, with the webhook's
// own decision and credit: a credit through the intent's one-shot (a linked
// crypto invoice's too), or an unfulfilled record for an operator. A webhook
// or reconcile racing the read credits once.

import (
	"sync"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// How the status reads check the chain.
type solanaStatusCheckSettings struct {
	// A payment is broadcast after the intent is made: an intent this young
	// is left to its webhook.
	MinAge time.Duration
	// One chain read per reference per interval, whatever the poll rate and
	// however many screens poll it.
	Interval time.Duration
	// A late payment still credits (crypto invoices take one for a day), so
	// an intent is read until it expired this long ago, the reconcile's
	// window. Non-positive reads none once expired.
	MaxExpiredAge time.Duration
	// The references whose last read is remembered; past it, a read is
	// refused until one ages out.
	MaxTrackedReferences int
	// The newest signatures of a reference one read examines, as the
	// reconcile's chain leg.
	MaxSignatures int
}

// The settings the status reads run with.
func defaultSolanaStatusCheckSettings() *solanaStatusCheckSettings {
	return &solanaStatusCheckSettings{
		MinAge:               30 * time.Second,
		Interval:             20 * time.Second,
		MaxExpiredAge:        24 * time.Hour,
		MaxTrackedReferences: 4096,
		MaxSignatures:        solanaChainLegMaxSignatures,
	}
}

// The status check's clock and lookups. Replaceable only by tests.
var (
	solanaStatusCheckNow        = server.NowUtc
	solanaStatusCheckLookupsFor = func(clientSession *session.ClientSession) solanaPaymentLookups {
		return &solanaDbPaymentLookups{clientSession: clientSession}
	}
)

// Remembers when each reference was last read, so a screen polling every few
// seconds reads the chain once per interval. Safe for concurrent use.
type solanaStatusCheckLimiter struct {
	settings *solanaStatusCheckSettings

	stateLock              sync.Mutex
	lastReadReferenceTimes map[string]time.Time
}

// The process's limiter, which every status read shares.
var solanaStatusChecks = newSolanaStatusCheckLimiter(defaultSolanaStatusCheckSettings())

// A limiter with no reference read yet.
func newSolanaStatusCheckLimiter(settings *solanaStatusCheckSettings) *solanaStatusCheckLimiter {
	return &solanaStatusCheckLimiter{
		settings:               settings,
		lastReadReferenceTimes: map[string]time.Time{},
	}
}

// Whether a status read of an open USDC intent with this reference reads the
// chain now: the intent is past its first moments, not long expired, and its
// reference was not read within the interval. A yes marks the reference read.
func (self *solanaStatusCheckLimiter) due(reference string, createdAt time.Time, expiresAt *time.Time, now time.Time) bool {
	if reference == "" {
		return false
	}
	if now.Sub(createdAt) < self.settings.MinAge {
		return false
	}
	if expiresAt != nil && self.settings.MaxExpiredAge < now.Sub(*expiresAt) {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if lastReadTime, ok := self.lastReadReferenceTimes[reference]; ok && now.Sub(lastReadTime) < self.settings.Interval {
		return false
	}
	if self.settings.MaxTrackedReferences <= len(self.lastReadReferenceTimes) {
		for trackedReference, lastReadTime := range self.lastReadReferenceTimes {
			if self.settings.Interval <= now.Sub(lastReadTime) {
				delete(self.lastReadReferenceTimes, trackedReference)
			}
		}
		if self.settings.MaxTrackedReferences <= len(self.lastReadReferenceTimes) {
			// every tracked reference was read within the interval: a flood,
			// which the chain does not answer
			return false
		}
	}
	self.lastReadReferenceTimes[reference] = now
	return true
}

// Forgets every read, for tests.
func (self *solanaStatusCheckLimiter) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lastReadReferenceTimes = map[string]time.Time{}
}

// Reads the chain for one open USDC intent's reference and acts on each
// finalized transaction naming it that the webhook and the reconcile never
// handled, as the webhook acts. Returns true when it credited one. A failure
// is logged and changes nothing: the status read answers from the database,
// and the webhook and the reconcile still run.
func solanaCheckReferenceNow(clientSession *session.ClientSession, settings *solanaStatusCheckSettings, reference string) (credited bool) {
	ctx := clientSession.Ctx
	rpcUrl, ok := solanaChainRpcUrl()
	if !ok {
		return false
	}
	rpc := solanaChainRpcFor(rpcUrl)
	signatures, err := solanaFinalizedSignatures(ctx, rpc, reference, solanaChainLegReferencePages)
	if err != nil {
		glog.Infof("[sub]status check of solana reference %s: getSignaturesForAddress: %s\n", reference, err)
		return false
	}
	lookups := solanaStatusCheckLookupsFor(clientSession)
	for i, signature := range signatures {
		if settings.MaxSignatures <= i {
			break
		}
		if signature.Err != nil || solanaChainSignatureHandled(nil, ctx, signature.Signature) {
			continue
		}
		parsed, err := solanaFinalizedTransaction(ctx, rpc, signature.Signature)
		if err != nil {
			glog.Infof("[sub]status check of solana reference %s: getTransaction %s: %s\n", reference, signature.Signature, err)
			return credited
		}
		if parsed == nil {
			continue
		}
		decision := solanaDecidePayment(solanaTransactionFromParsed(signature.Signature, parsed), lookups)
		switch decision.action {
		case solanaPaymentActionError:
			glog.Infof("[sub]status check of solana reference %s: %s: %v\n", reference, signature.Signature, decision.err)
		case solanaPaymentActionRecord:
			if err := solanaChainRecord(nil, ctx, decision.unfulfilled); err != nil {
				glog.Errorf("[sub]status check of solana reference %s: could not record unfulfilled payment %s: %s\n", reference, signature.Signature, err)
				continue
			}
			glog.Infof("[sub]status check: a payment no webhook delivered bought nothing; recorded unfulfilled (%s)\n", decision.skipMessage)
		case solanaPaymentActionCredit:
			creditedNow, err := solanaChainCredit(nil, clientSession, decision.intent, signature.Signature, decision.tokenAmountUsd)
			if err != nil {
				glog.Errorf("[sub]status check of solana reference %s: could not credit %s: %s\n", reference, signature.Signature, err)
				continue
			}
			if creditedNow {
				solanaChainOnboarding(clientSession, decision.intent)
				glog.Infof("[sub]status check: credited solana reference %s from the chain before its webhook\n", reference)
				credited = true
			}
		}
	}
	return credited
}

// The chain read of a USDC invoice's status read: the invoice re-read when the
// read credited it, the invoice itself otherwise.
func cryptoUsdcStatusCheck(clientSession *session.ClientSession, intent *model.CryptoPaymentIntent) *model.CryptoPaymentIntent {
	if intent.Asset != model.CryptoAssetUsdcSolana || intent.SolanaReference == nil ||
		intent.CreditedAt != nil || intent.VoidedAt != nil || intent.UnlinkedAt != nil {
		return intent
	}
	expiresAt := intent.ExpiresAt
	if !solanaStatusChecks.due(*intent.SolanaReference, intent.CreateTime, &expiresAt, solanaStatusCheckNow()) {
		return intent
	}
	if !solanaCheckReferenceNow(clientSession, solanaStatusChecks.settings, *intent.SolanaReference) {
		return intent
	}
	if reread := model.GetCryptoPaymentIntent(clientSession.Ctx, intent.IntentId); reread != nil {
		return reread
	}
	return intent
}

// The chain read of a USDC data purchase's status read: the intent re-read
// when the read credited it, the intent itself otherwise.
func payDataSolanaStatusCheck(clientSession *session.ClientSession, intent *model.SolanaPaymentIntent) *model.SolanaPaymentIntent {
	if intent.TxSignature != nil {
		return intent
	}
	if !solanaStatusChecks.due(intent.PaymentReference, intent.CreatedAt, intent.ExpiresAt, solanaStatusCheckNow()) {
		return intent
	}
	if !solanaCheckReferenceNow(clientSession, solanaStatusChecks.settings, intent.PaymentReference) {
		return intent
	}
	if reread := model.GetSolanaPaymentIntent(clientSession.Ctx, intent.PaymentReference); reread != nil {
		return reread
	}
	return intent
}
