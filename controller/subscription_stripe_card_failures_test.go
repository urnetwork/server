package controller

// Hermetic tests of the card-failure watch (subscription_stripe_card_failures.go):
// the decline classes, when a burst is due, the burst line the monitor reads,
// and the whole path from the webhook with an in-memory window in place of
// Redis. The window in Redis is exercised in
// subscription_stripe_card_failures_db_test.go.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestStripeCardFailureReasonClass(t *testing.T) {
	for _, c := range []struct {
		code        string
		declineCode string
		outcomeType string
		want        string
	}{
		{code: "card_declined", declineCode: "insufficient_funds", outcomeType: "issuer_declined", want: stripeCardFailureReasonInsufficientFunds},
		{code: "card_declined", declineCode: "generic_decline", outcomeType: "issuer_declined", want: stripeCardFailureReasonDeclined},
		{code: "incorrect_number", declineCode: "", outcomeType: "invalid", want: stripeCardFailureReasonInvalidCard},
		{code: "expired_card", declineCode: "expired_card", outcomeType: "issuer_declined", want: stripeCardFailureReasonInvalidCard},
		{code: "incorrect_cvc", declineCode: "", outcomeType: "", want: stripeCardFailureReasonInvalidCard},
		{code: "card_declined", declineCode: "stolen_card", outcomeType: "issuer_declined", want: stripeCardFailureReasonLostStolen},
		{code: "card_declined", declineCode: "fraudulent", outcomeType: "issuer_declined", want: stripeCardFailureReasonFraud},
		// a Radar block, whatever its reason
		{code: "card_declined", declineCode: "highest_risk_level", outcomeType: "blocked", want: stripeCardFailureReasonBlocked},
		{code: "card_declined", declineCode: "rule", outcomeType: "blocked", want: stripeCardFailureReasonBlocked},
		{code: "card_declined", declineCode: "", outcomeType: "blocked", want: stripeCardFailureReasonBlocked},
		{code: "setup_intent_authentication_failure", declineCode: "", outcomeType: "", want: stripeCardFailureReasonAuthentication},
		{code: "card_declined", declineCode: "authentication_required", outcomeType: "", want: stripeCardFailureReasonAuthentication},
		{code: "processing_error", declineCode: "", outcomeType: "", want: stripeCardFailureReasonProcessing},
		// an unknown decline code falls back to the error code
		{code: "card_declined", declineCode: "a_new_decline_code", outcomeType: "issuer_declined", want: stripeCardFailureReasonDeclined},
		{code: "a_new_error_code", declineCode: "", outcomeType: "", want: stripeCardFailureReasonOther},
		{code: "", declineCode: "", outcomeType: "", want: stripeCardFailureReasonOther},
	} {
		if got := stripeCardFailureReasonClass(c.code, c.declineCode, c.outcomeType); got != c.want {
			t.Errorf("%q %q %q: %q, want %q", c.code, c.declineCode, c.outcomeType, got, c.want)
		}
	}
}

// A burst is due at the first failure past the burst count, then at each
// burst count further; never when bursts are off.
func TestStripeCardFailureBurstDue(t *testing.T) {
	due := []int64{}
	for count := int64(1); count <= 21; count += 1 {
		if stripeCardFailureBurstDue(count, 5) {
			due = append(due, count)
		}
	}
	if want := []int64{6, 11, 16, 21}; len(due) != len(want) || due[0] != 6 || due[1] != 11 || due[2] != 16 || due[3] != 21 {
		t.Fatalf("bursts due at %v, want %v", due, want)
	}
	for _, burstCount := range []int{0, -1} {
		if stripeCardFailureBurstDue(100, burstCount) {
			t.Fatalf("burst count %d: no burst is due", burstCount)
		}
	}
}

// The burst line is the one the monitor's payment-card-failure-burst class
// reads (monitor/tailer.go paymentCardFailureBurstRe, pinned by its own test
// to this same text).
func TestStripeCardFailureBurstLineHasTheMonitorShape(t *testing.T) {
	line := stripeCardFailureBurstLine(&stripeCardFailure{
		Kind:        stripeCardFailureKindSetup,
		ReasonClass: stripeCardFailureReasonInvalidCard,
		OwnerKind:   stripeCardFailureOwnerIntent,
		OwnerId:     "seti_synthetic_burst",
	}, 6, 10*time.Minute)
	if want := "[stripe]card failure burst: 6 failed card checks within 10 minutes for intent seti_synthetic_burst (latest setup invalid_card)"; line != want {
		t.Fatalf("burst line\n%q, want\n%q", line, want)
	}
}

// An in-memory owner window with the Redis window's rules: a failure is keyed
// by its event id, the failures older than the window leave, and the count is
// what remains. Safe for concurrent use.
type stripeCardFailureTestWindow struct {
	stateLock sync.Mutex
	// owner key -> event id -> failure time
	ownerFailureTimes map[string]map[string]time.Time
}

func (self *stripeCardFailureTestWindow) add(ctx context.Context, failure *stripeCardFailure, now time.Time, window time.Duration) (int64, bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	failureTimes, ok := self.ownerFailureTimes[failure.ownerKey()]
	if !ok {
		failureTimes = map[string]time.Time{}
		self.ownerFailureTimes[failure.ownerKey()] = failureTimes
	}
	_, exists := failureTimes[failure.EventId]
	if !exists {
		failureTimes[failure.EventId] = failure.Time
	}
	cutoff := now.Add(-window)
	for eventId, failureTime := range failureTimes {
		if failureTime.Before(cutoff) {
			delete(failureTimes, eventId)
		}
	}
	return int64(len(failureTimes)), !exists, nil
}

// Installs a recorder on an in-memory window at the default settings and a
// fixed clock, and returns the burst lines it writes.
func installStripeCardFailureTestRecorder(t testing.TB, now time.Time, customerNetworkIds map[string]server.Id) func() []string {
	window := &stripeCardFailureTestWindow{ownerFailureTimes: map[string]map[string]time.Time{}}
	linesLock := sync.Mutex{}
	lines := []string{}
	previous := stripeCardFailures
	stripeCardFailures = &stripeCardFailureRecorder{
		settings: defaultStripeCardFailureSettings(),
		customerNetworkId: func(ctx context.Context, customerId string) *server.Id {
			if networkId, ok := customerNetworkIds[customerId]; ok {
				return &networkId
			}
			return nil
		},
		addToWindow: window.add,
		reportBurst: func(line string) {
			linesLock.Lock()
			defer linesLock.Unlock()
			lines = append(lines, line)
		},
		now: func() time.Time { return now },
	}
	t.Cleanup(func() {
		stripeCardFailures = previous
	})
	return func() []string {
		linesLock.Lock()
		defer linesLock.Unlock()
		return append([]string{}, lines...)
	}
}

// A setup_intent.setup_failed event of the intent, at the time.
func stripeSetupFailedTestEvent(t testing.TB, setupIntentId string, customerId string, code string, declineCode string, eventTime time.Time) *StripeWebhookArgs {
	var customer any
	if customerId != "" {
		customer = customerId
	}
	event := stripeWebhookEvent(t, "setup_intent.setup_failed", map[string]any{
		"id":       setupIntentId,
		"object":   "setup_intent",
		"customer": customer,
		"status":   "requires_payment_method",
		"last_setup_error": map[string]any{
			"code":           code,
			"decline_code":   declineCode,
			"type":           "card_error",
			"payment_method": map[string]any{"id": "pm_synthetic", "type": "card"},
		},
	})
	event.Created = eventTime.Unix()
	return event
}

// Card testing through one Checkout session's trial check: every failure is
// counted by kind and class, a redelivered event once, and the sixth failure
// within the window logs one burst line for the session's intent, then the
// eleventh another. Five failures each on two other sessions log nothing.
func TestStripeCardFailuresBurstOnOneCheckout(t *testing.T) {
	now := time.Date(2026, 11, 13, 4, 10, 0, 0, time.UTC)
	burstLines := installStripeCardFailureTestRecorder(t, now, nil)
	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	failures := stripeCardFailuresTotal.WithLabelValues(stripeCardFailureKindSetup, stripeCardFailureReasonInvalidCard)
	bursts := stripeCardFailureBurstsTotal.WithLabelValues(stripeCardFailureOwnerIntent)
	failuresBefore, burstsBefore := testutil.ToFloat64(failures), testutil.ToFloat64(bursts)

	deliver := func(event *StripeWebhookArgs) {
		result, err := StripeWebhook(event, clientSession)
		if err != nil || result == nil {
			t.Fatalf("%s: %+v %v", event.Type, result, err)
		}
	}

	// two other sessions with five failures each: no burst
	for _, setupIntentId := range []string{"seti_synthetic_other_a", "seti_synthetic_other_b"} {
		for i := range 5 {
			deliver(stripeSetupFailedTestEvent(t, setupIntentId, "", "incorrect_number", "", now.Add(-time.Duration(i)*time.Second)))
		}
	}
	if lines := burstLines(); len(lines) != 0 {
		t.Fatalf("five failures on a session are no burst, got %q", lines)
	}

	events := []*StripeWebhookArgs{}
	for i := range 6 {
		events = append(events, stripeSetupFailedTestEvent(t, "seti_synthetic_burst", "", "incorrect_number", "", now.Add(-time.Duration(60-i)*time.Second)))
	}
	for _, event := range events {
		deliver(event)
	}
	// a redelivery counts once
	deliver(events[0])
	lines := burstLines()
	if len(lines) != 1 || !strings.Contains(lines[0], "6 failed card checks within 10 minutes for intent seti_synthetic_burst") {
		t.Fatalf("the sixth failure on one session must log one burst, got %q", lines)
	}
	for i := range 5 {
		deliver(stripeSetupFailedTestEvent(t, "seti_synthetic_burst", "", "incorrect_number", "", now.Add(-time.Duration(30-i)*time.Second)))
	}
	if lines := burstLines(); len(lines) != 2 || !strings.Contains(lines[1], "11 failed card checks") {
		t.Fatalf("the burst going on must log again at the eleventh failure, got %q", lines)
	}

	if got := testutil.ToFloat64(failures) - failuresBefore; got != 21 {
		t.Fatalf("failed card checks counted %v, want 21 (a redelivery counts once)", got)
	}
	if got := testutil.ToFloat64(bursts) - burstsBefore; got != 2 {
		t.Fatalf("bursts counted %v, want 2", got)
	}
}

// Failures older than the window leave it: six failures spread over more than
// the window are no burst.
func TestStripeCardFailuresOutsideTheWindowAreNoBurst(t *testing.T) {
	now := time.Date(2026, 11, 13, 4, 30, 0, 0, time.UTC)
	burstLines := installStripeCardFailureTestRecorder(t, now, nil)
	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	for i := range 6 {
		// one every three minutes: the first two are past the 10 minute window
		event := stripeSetupFailedTestEvent(t, "seti_synthetic_slow", "", "card_declined", "generic_decline", now.Add(-time.Duration(15-3*i)*time.Minute))
		if _, err := StripeWebhook(event, clientSession); err != nil {
			t.Fatal(err)
		}
	}
	if lines := burstLines(); len(lines) != 0 {
		t.Fatalf("failures spread past the window are no burst, got %q", lines)
	}
}

// A failure counts for the network the server made its Stripe customer for,
// so card testing through several sessions of one network is one burst; a
// customer the server did not make counts for itself. A failed bank debit is
// not a card check and is not counted.
func TestStripeCardFailuresCountForTheNetworkOfTheCustomer(t *testing.T) {
	now := time.Date(2026, 11, 13, 5, 0, 0, 0, time.UTC)
	networkId := server.NewId()
	burstLines := installStripeCardFailureTestRecorder(t, now, map[string]server.Id{"cus_synthetic_sheet": networkId})
	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	charges := stripeCardFailuresTotal.WithLabelValues(stripeCardFailureKindCharge, stripeCardFailureReasonBlocked)
	chargesBefore := testutil.ToFloat64(charges)

	for i := range 6 {
		// a new payment sheet intent each time, one customer
		event := stripeSetupFailedTestEvent(t, "seti_synthetic_sheet_"+string(rune('a'+i)), "cus_synthetic_sheet", "card_declined", "fraudulent", now.Add(-time.Duration(i)*time.Minute))
		if _, err := StripeWebhook(event, clientSession); err != nil {
			t.Fatal(err)
		}
	}
	lines := burstLines()
	if len(lines) != 1 || !strings.Contains(lines[0], "for network "+networkId.String()) {
		t.Fatalf("one network's failures across its intents must make one burst for the network, got %q", lines)
	}

	charge := func(chargeId string, customerId string, paymentMethodType string) *StripeWebhookArgs {
		event := stripeWebhookEvent(t, "charge.failed", map[string]any{
			"id":                     chargeId,
			"object":                 "charge",
			"customer":               customerId,
			"payment_intent":         "pi_synthetic_charge",
			"failure_code":           "card_declined",
			"outcome":                map[string]any{"type": "blocked", "reason": "highest_risk_level"},
			"payment_method_details": map[string]any{"type": paymentMethodType},
		})
		event.Created = now.Unix()
		return event
	}
	for i := range 6 {
		if _, err := StripeWebhook(charge("ch_synthetic_"+string(rune('a'+i)), "cus_synthetic_checkout", "card"), clientSession); err != nil {
			t.Fatal(err)
		}
	}
	lines = burstLines()
	if len(lines) != 2 || !strings.Contains(lines[1], "for customer cus_synthetic_checkout (latest charge blocked)") {
		t.Fatalf("a customer the server did not make counts for itself, got %q", lines)
	}
	if _, err := StripeWebhook(charge("ch_synthetic_debit", "cus_synthetic_checkout", "us_bank_account"), clientSession); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(charges) - chargesBefore; got != 6 {
		t.Fatalf("blocked card charges counted %v, want 6 (a failed bank debit is no card check)", got)
	}
}
