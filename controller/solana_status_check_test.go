package controller

// Hermetic tests (no database) of the status reads' chain check
// (solana_status_check.go): when a read reads the chain, and that it acts on
// what it finds with the webhook's own decision, once. The chain is the chain
// leg's fake RPC and fixtures.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/session"
)

// A read reads the chain only for an intent past its first moments and not
// long expired, and once per interval per reference.
func TestSolanaStatusCheckDue(t *testing.T) {
	settings := defaultSolanaStatusCheckSettings()
	limiter := newSolanaStatusCheckLimiter(settings)
	solanaStatusCheckDue := limiter.due
	now := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Hour)

	// just made: left to its webhook
	connect.AssertEqual(t, solanaStatusCheckDue("ref-young", now.Add(-10*time.Second), &expiresAt, now), false)
	// expired more than a day ago: past the reconcile's window too
	longExpired := now.Add(-25 * time.Hour)
	connect.AssertEqual(t, solanaStatusCheckDue("ref-old", now.Add(-26*time.Hour), &longExpired, now), false)
	// no reference: nothing to read
	connect.AssertEqual(t, solanaStatusCheckDue("", now.Add(-time.Minute), &expiresAt, now), false)

	// due, then not again within the interval, then due again
	connect.AssertEqual(t, solanaStatusCheckDue("ref-open", now.Add(-time.Minute), &expiresAt, now), true)
	connect.AssertEqual(t, solanaStatusCheckDue("ref-open", now.Add(-time.Minute), &expiresAt, now.Add(5*time.Second)), false)
	connect.AssertEqual(t, solanaStatusCheckDue("ref-open", now.Add(-time.Minute), &expiresAt, now.Add(settings.Interval)), true)
	// another reference has its own interval
	connect.AssertEqual(t, solanaStatusCheckDue("ref-other", now.Add(-time.Minute), &expiresAt, now.Add(5*time.Second)), true)

	// recently expired: a late payment still credits, so it is read
	recentlyExpired := now.Add(-2 * time.Hour)
	connect.AssertEqual(t, solanaStatusCheckDue("ref-late", now.Add(-3*time.Hour), &recentlyExpired, now), true)
}

// The read credits a payment whose webhook never came, through the webhook's
// decision and credit, with its onboarding record; what is already handled is
// not read again.
func TestSolanaStatusCheckCreditsALostDelivery(t *testing.T) {
	test := newSolanaChainLegTest(t)
	test.intent()
	previousLookups := solanaStatusCheckLookupsFor
	solanaStatusCheckLookupsFor = func(clientSession *session.ClientSession) solanaPaymentLookups { return test.lookups }
	t.Cleanup(func() { solanaStatusCheckLookupsFor = previousLookups })
	clientSession := &session.ClientSession{Ctx: context.Background()}

	connect.AssertEqual(t, solanaCheckReferenceNow(clientSession, defaultSolanaStatusCheckSettings(), solanaFixtureUsdcReference), true)
	connect.AssertEqual(t, test.credits, []string{fmt.Sprintf("%s %s %.3f", solanaFixtureUsdcReference, solanaFixtureUsdcSignature, solanaFixtureUsdcAmount)})
	connect.AssertEqual(t, test.onboarded, 1)
	// the native transfer in the same history pays no USDC intent: not recorded
	connect.AssertEqual(t, len(test.records), 0)

	// once credited, the signature is handled: a later read reads its history
	// but not the transaction again, and credits nothing
	test.handled[solanaFixtureUsdcSignature] = true
	test.handled[solanaFixtureNativeSignature] = true
	transactionsRead := test.rpc.calls["getTransaction"]
	connect.AssertEqual(t, solanaCheckReferenceNow(clientSession, defaultSolanaStatusCheckSettings(), solanaFixtureUsdcReference), false)
	connect.AssertEqual(t, test.rpc.calls["getTransaction"], transactionsRead)
	connect.AssertEqual(t, len(test.credits), 1)
}

// Without Helius configured, or when the chain does not answer, the read
// changes nothing and the status answers from the database.
func TestSolanaStatusCheckWithoutTheChain(t *testing.T) {
	test := newSolanaChainLegTest(t)
	test.intent()
	clientSession := &session.ClientSession{Ctx: context.Background()}

	solanaChainRpcUrl = func() (string, bool) { return "", false }
	connect.AssertEqual(t, solanaCheckReferenceNow(clientSession, defaultSolanaStatusCheckSettings(), solanaFixtureUsdcReference), false)
	connect.AssertEqual(t, len(test.rpc.calls), 0)

	solanaChainRpcUrl = func() (string, bool) { return "https://synthetic.invalid/?api-key=synthetic", true }
	test.rpc.err = fmt.Errorf("synthetic rpc outage")
	connect.AssertEqual(t, solanaCheckReferenceNow(clientSession, defaultSolanaStatusCheckSettings(), solanaFixtureUsdcReference), false)
	connect.AssertEqual(t, len(test.credits), 0)
}
