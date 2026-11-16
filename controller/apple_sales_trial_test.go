package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Root-cause tests for what an App Store free trial is posted as. The
// trial's start was posted as a sale of "0.00 USD", and its first charge as a
// "renewal": Apple renews the trial into its first paid period. Each starts
// from the signed claims (validateAppleTransaction) and runs the real credit
// gate (appleCreditSubscriptionTransactionInTx) on salesLedgerTx.

// appleSalesTrialClaims are the claims of a yearly subscription's transaction
// bought at purchase, the subscription's first purchase at firstPurchase.
func appleSalesTrialClaims(
	networkId server.Id,
	transactionId string,
	reason string,
	firstPurchase time.Time,
	purchase time.Time,
	expires time.Time,
	priceMilliunits int64,
) AppleNotificationDecodedPayload {
	claims := map[string]any{
		"appAccountToken":       networkId.String(),
		"transactionId":         transactionId,
		"originalTransactionId": "2000",
		"transactionReason":     reason,
		"productId":             "supporter_yearly_26",
		"purchaseDate":          float64(purchase.UnixMilli()),
		"originalPurchaseDate":  float64(firstPurchase.UnixMilli()),
		"expiresDate":           float64(expires.UnixMilli()),
		"price":                 float64(priceMilliunits),
		"currency":              "USD",
		"environment":           "Production",
	}
	if priceMilliunits == 0 {
		// the yearly product's introductory offer
		claims["offerType"] = float64(appleOfferTypeIntroductory)
		claims["offerDiscountType"] = "FREE_TRIAL"
	}
	return AppleNotificationDecodedPayload{
		NotificationType: "SUBSCRIBED",
		NotificationUUID: server.NewId().String(),
		SignedDate:       purchase.UnixMilli(),
		TransactionInfo:  claims,
	}
}

func appleSalesTrialText(headline string, item string, amount string) string {
	return "*" + headline + ":* " + item + "\n*Network:* acme\n*Amount:* " + amount + "\n*Payment method:* Apple App Store"
}

// appleSalesTrialCredit validates the claims and credits them.
func appleSalesTrialCredit(t *testing.T, tx *salesLedgerTx, claims AppleNotificationDecodedPayload) {
	t.Helper()
	transaction, err := validateAppleTransaction(claims, []string{"supporter_monthly_26", "supporter_yearly_26"}, true)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, appleCreditSubscriptionTransactionInTx(tx, context.Background(), server.NewId(), transaction), true)
}

func TestAppleTrialStartPostsATrialWithNoCharge(t *testing.T) {
	notices := captureSalesNotices(t)
	networkId := server.NewId()
	tx := newSalesLedgerTx(networkId)
	start := time.Date(2026, 11, 8, 4, 7, 27, 0, time.UTC)
	trialEnd := start.Add(14 * 24 * time.Hour)

	appleSalesTrialCredit(t, tx, appleSalesTrialClaims(networkId, "2000", "PURCHASE", start, start, trialEnd, 0))
	got := notices()
	connect.AssertEqual(t, len(got), 1)
	connect.AssertEqual(t, salesNoticeLines(got[0], "acme"), appleSalesTrialText("Trial", "Pro yearly (new)", "no charge (free trial until 2026-11-22)"))
}

func TestAppleTrialConversionIsTheFirstSale(t *testing.T) {
	notices := captureSalesNotices(t)
	networkId := server.NewId()
	tx := newSalesLedgerTx(networkId)
	start := time.Date(2026, 11, 8, 4, 7, 27, 0, time.UTC)
	trialEnd := start.Add(14 * 24 * time.Hour)
	year := 365 * 24 * time.Hour

	appleSalesTrialCredit(t, tx, appleSalesTrialClaims(networkId, "2000", "PURCHASE", start, start, trialEnd, 0))
	// Apple renews the trial into its first paid year
	appleSalesTrialCredit(t, tx, appleSalesTrialClaims(networkId, "2001", "RENEWAL", start, trialEnd, trialEnd.Add(year), 39990))
	// and a year later renews again
	appleSalesTrialCredit(t, tx, appleSalesTrialClaims(networkId, "2002", "RENEWAL", start, trialEnd.Add(year), trialEnd.Add(2*year), 39990))

	texts := []string{}
	for _, notice := range notices() {
		texts = append(texts, salesNoticeLines(notice, "acme"))
	}
	connect.AssertEqual(t, texts, []string{
		appleSalesTrialText("Trial", "Pro yearly (new)", "no charge (free trial until 2026-11-22)"),
		appleSalesTrialText("Sale", "Pro yearly (new, after a free trial)", "39.99 USD"),
		appleSalesTrialText("Sale", "Pro yearly (renewal)", "39.99 USD"),
	})
}

// A monthly plan's first renewal comes a month after its purchase: a
// renewal, not a trial's conversion. Claims that do not name the first
// purchase read as a renewal too.
func TestAppleRenewalAfterAPaidPeriodIsARenewal(t *testing.T) {
	networkId := server.NewId()
	start := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	// February: the shortest month
	renewalTime := start.Add(28 * 24 * time.Hour)
	claims := appleSalesTrialClaims(networkId, "3001", "RENEWAL", start, renewalTime, renewalTime.Add(31*24*time.Hour), 4990)
	claims.TransactionInfo["productId"] = "supporter_monthly_26"
	transaction, err := validateAppleTransaction(claims, []string{"supporter_monthly_26"}, true)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, transaction.originalPurchaseTime.Equal(start), true)
	connect.AssertEqual(t, appleSalesNotice(transaction).Kind, "renewal")

	delete(claims.TransactionInfo, "originalPurchaseDate")
	claims.TransactionInfo["purchaseDate"] = float64(start.Add(14 * 24 * time.Hour).UnixMilli())
	transaction, err = validateAppleTransaction(claims, []string{"supporter_monthly_26"}, true)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, transaction.originalPurchaseTime.IsZero(), true)
	connect.AssertEqual(t, appleSalesNotice(transaction).Kind, "renewal")
	connect.AssertEqual(t, strings.HasPrefix(salesNoticeLines(appleSalesNotice(transaction), "acme"), "*Sale:*"), true)
}
