package controller

// App Store sandbox purchases credit only internal testers
// (apple_sandbox_testers.go; owner, 2026-11-17: "Both fixes", "Keep buttons,
// neutral notice"). The credit gate and the grace hold run on the in-memory
// ledger (salesLedgerTx), and the verify answer is judged before any database
// read, so none of these needs a database.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// installAppleSandboxTesters makes the networks the internal testers whose
// sandbox purchases are credited, for the test.
func installAppleSandboxTesters(t testing.TB, networkIds ...server.Id) {
	previous := appleSandboxTesterNetworkIdsFunc
	t.Cleanup(func() {
		appleSandboxTesterNetworkIdsFunc = previous
	})
	testers := map[server.Id]bool{}
	for _, networkId := range networkIds {
		testers[networkId] = true
	}
	appleSandboxTesterNetworkIdsFunc = func() map[server.Id]bool {
		return testers
	}
}

// The defect: a TestFlight (sandbox) purchase is free, and it was credited as
// real Pro for any network. Now only an internal tester's sandbox purchase is
// credited; anyone else's writes nothing, not even the ledger row, and a
// production purchase is credited as before.
func TestAppleSandboxCreditOnlyForInternalTesters(t *testing.T) {
	captureSalesNotices(t)
	ctx := context.Background()
	testerNetworkId := server.NewId()
	networkId := server.NewId()
	installAppleSandboxTesters(t, testerNetworkId)
	tx := newSalesLedgerTx(testerNetworkId, networkId)

	refused := appleSalesTransaction(networkId, "2000000000003001")
	refused.environment = appleEnvironmentSandbox
	connect.AssertEqual(t, appleCreditSubscriptionTransactionInTx(tx, ctx, server.NewId(), refused), false)
	connect.AssertEqual(t, tx.rows["apple_subscription_transaction/2000000000003001"], false)
	connect.AssertEqual(t, len(tx.renewals), 0)
	connect.AssertEqual(t, len(tx.balances), 0)

	tester := appleSalesTransaction(testerNetworkId, "2000000000003002")
	tester.environment = appleEnvironmentSandbox
	connect.AssertEqual(t, appleCreditSubscriptionTransactionInTx(tx, ctx, server.NewId(), tester), true)
	connect.AssertEqual(t, len(tx.renewals), 1)
	connect.AssertEqual(t, tx.renewals[0].networkId, testerNetworkId)

	for i, environment := range []string{appleEnvironmentProduction, ""} {
		production := appleSalesTransaction(networkId, "150000000000300"+string(rune('3'+i)))
		production.environment = environment
		connect.AssertEqual(t, appleCreditSubscriptionTransactionInTx(tx, ctx, server.NewId(), production), true)
	}
	connect.AssertEqual(t, len(tx.renewals), 3)
}

// With no tester list (a missing or empty apple.yml key) no sandbox purchase is
// credited at all.
func TestAppleSandboxCreditWithoutATesterListCreditsNoSandboxPurchase(t *testing.T) {
	captureSalesNotices(t)
	ctx := context.Background()
	networkId := server.NewId()
	installAppleSandboxTesters(t)
	tx := newSalesLedgerTx(networkId)

	sandbox := appleSalesTransaction(networkId, "2000000000003101")
	sandbox.environment = appleEnvironmentSandbox
	connect.AssertEqual(t, appleCreditSubscriptionTransactionInTx(tx, ctx, server.NewId(), sandbox), false)
	connect.AssertEqual(t, len(tx.renewals), 0)

	connect.AssertEqual(t, len(appleSandboxTesterNetworkIds(nil)), 0)
}

// The billing grace hold of a sandbox subscription holds Pro only for a tester.
func TestAppleSandboxGraceHoldOnlyForInternalTesters(t *testing.T) {
	ctx := context.Background()
	testerNetworkId := server.NewId()
	networkId := server.NewId()
	installAppleSandboxTesters(t, testerNetworkId)

	hold := func(networkId server.Id, environment string) (bool, int) {
		tx := newSalesLedgerTx(networkId)
		transaction := appleSalesTransaction(networkId, "2000000000003201")
		transaction.environment = environment
		graceExpires := transaction.expiresTime.Add(16 * 24 * time.Hour)
		held, err := appleHoldGraceEntitlementInTx(tx, ctx, transaction, graceExpires, transaction.expiresTime.Add(time.Hour))
		connect.AssertEqual(t, err, nil)
		return held, len(tx.renewals)
	}

	held, renewals := hold(networkId, appleEnvironmentSandbox)
	connect.AssertEqual(t, held, false)
	connect.AssertEqual(t, renewals, 0)
	held, renewals = hold(testerNetworkId, appleEnvironmentSandbox)
	connect.AssertEqual(t, held, true)
	connect.AssertEqual(t, renewals, 1)
	held, _ = hold(networkId, appleEnvironmentProduction)
	connect.AssertEqual(t, held, true)
}

// The purchase report names the refusal, so the app can show its neutral line
// rather than "couldn't be verified": status invalid (the app finishes the
// transaction) with the sandbox_purchase code. A tester's sandbox purchase and
// a production purchase go on to the usual rules (here: another live
// network's purchase, wrong_network).
func TestVerifyAppleSandboxPurchaseNamesTheRefusal(t *testing.T) {
	// another network's purchase goes to the restore move rules
	// (apple_subscription_move_controller.go); their fake store answers
	// without a database, and the other network is live, so it stays
	// wrong_network
	moveEnv := installMoveFakes(t)
	now := time.Date(2026, 11, 17, 20, 30, 0, 0, time.UTC)
	previousNow := appleVerifyNowFunc
	t.Cleanup(func() {
		appleVerifyNowFunc = previousNow
	})
	appleVerifyNowFunc = func() time.Time {
		return now
	}
	products := []string{"supporter_monthly_26"}
	claims := func(token *server.Id, environment string) map[string]any {
		purchase := now.Add(-time.Hour)
		claims := map[string]any{
			"transactionId":         "2000000000003301",
			"originalTransactionId": "2000000000003300",
			"productId":             "supporter_monthly_26",
			"purchaseDate":          float64(purchase.UnixMilli()),
			"expiresDate":           float64(purchase.Add(30 * 24 * time.Hour).UnixMilli()),
			"price":                 float64(4990),
			"currency":              "USD",
			"environment":           environment,
		}
		if token != nil {
			claims["appAccountToken"] = token.String()
		}
		return claims
	}
	sessionFor := func(networkId server.Id) *session.ClientSession {
		return session.Testing_CreateClientSession(context.Background(), &session.ByJwt{
			NetworkId: networkId,
			UserId:    server.NewId(),
		})
	}
	testerNetworkId := server.NewId()
	networkId := server.NewId()
	otherNetworkId := server.NewId()
	for _, live := range []server.Id{testerNetworkId, networkId, otherNetworkId} {
		moveEnv.state.networks[live] = true
	}
	installAppleSandboxTesters(t, testerNetworkId)

	// a TestFlight purchase of this network, and an offer-code redemption
	// without a token, for a network that is not a tester
	for _, token := range []*server.Id{&networkId, nil} {
		result, err := VerifyAppleTransactionClaims(claims(token, appleEnvironmentSandbox), products, sessionFor(networkId))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusInvalid)
		connect.AssertEqual(t, result.Error != nil, true)
		connect.AssertEqual(t, result.Error.Code, PurchaseErrorCodeSandboxPurchase)
		connect.AssertEqual(t, result.Error.Message, "Test purchases in test builds don't unlock Pro.")
	}

	// a tester's sandbox purchase passes the gate
	result, err := VerifyAppleTransactionClaims(claims(&otherNetworkId, appleEnvironmentSandbox), products, sessionFor(testerNetworkId))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Status, VerifyPurchaseStatusWrongNetwork)
	connect.AssertEqual(t, result.Error == nil, true)

	// a production purchase is never judged by the tester list
	result, err = VerifyAppleTransactionClaims(claims(&otherNetworkId, appleEnvironmentProduction), products, sessionFor(networkId))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Status, VerifyPurchaseStatusWrongNetwork)
	connect.AssertEqual(t, result.Error == nil, true)
}

// The vault list: network ids, spaces trimmed; anything else is left out.
func TestAppleSandboxTesterNetworkIdsParsesTheList(t *testing.T) {
	first := server.NewId()
	second := server.NewId()
	testers := appleSandboxTesterNetworkIds([]string{
		first.String(),
		" " + second.String() + " ",
		"not-a-network-id",
		"",
		"00000000-0000-0000-0000-000000000000",
	})
	connect.AssertEqual(t, len(testers), 2)
	connect.AssertEqual(t, testers[first], true)
	connect.AssertEqual(t, testers[second], true)
}
