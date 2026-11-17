package controller

// App Store sandbox purchases are free: TestFlight builds, App Review and
// sandbox testers buy in Apple's sandbox, and no money moves. They used to be
// credited like any other purchase (with no revenue), so a TestFlight user got
// real Pro without paying. The owner, 2026-11-17: "Both fixes", then "Keep
// buttons, neutral notice": the apps keep their purchase buttons (App Review
// must be able to buy), and the server credits a sandbox purchase only for an
// internal tester's network. For anyone else the verify answer names the
// refusal (PurchaseErrorCodeSandboxPurchase), and the app shows a neutral line.
//
// The testers are vault apple.yml app_store_notifications
// .sandbox_tester_network_ids: a list of network ids. An App Store purchase's
// appAccountToken is the network id, so the one list covers both. The App
// Review demo account's network belongs on it. A missing or empty list
// credits no sandbox purchase. Production purchases are unchanged, and nothing
// already credited is ended: a refused sandbox purchase only adds nothing.

import (
	"strings"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// What the verify answer says beside PurchaseErrorCodeSandboxPurchase, for an
// app that has no line of its own for the code. Neutral on purpose: it names
// no other way to pay (the App Store's rule on steering).
const purchaseSandboxPurchaseMessage = "Test purchases in test builds don't unlock Pro."

// The networks whose App Store sandbox purchases are credited. Replaceable
// only by tests; production reads the vault on every call, so a list the
// owner delivers takes effect without a release.
var appleSandboxTesterNetworkIdsFunc = appleSandboxTesterNetworkIdsFromVault

// vault apple.yml app_store_notifications.sandbox_tester_network_ids. A
// missing file, block or key is an empty list.
func appleSandboxTesterNetworkIdsFromVault() map[server.Id]bool {
	resource, err := server.Vault.SimpleResource("apple.yml")
	if err != nil {
		return map[server.Id]bool{}
	}
	var config struct {
		AppStoreNotifications *struct {
			SandboxTesterNetworkIds []string `yaml:"sandbox_tester_network_ids"`
		} `yaml:"app_store_notifications"`
	}
	if err := resource.UnmarshalYamlE(&config); err != nil || config.AppStoreNotifications == nil {
		return map[server.Id]bool{}
	}
	return appleSandboxTesterNetworkIds(config.AppStoreNotifications.SandboxTesterNetworkIds)
}

// The tester list's network ids. A value that is not a network id is left
// out, and logged without its text.
func appleSandboxTesterNetworkIds(values []string) map[server.Id]bool {
	networkIds := map[server.Id]bool{}
	for i, value := range values {
		networkId, err := server.ParseId(strings.TrimSpace(value))
		if err != nil || networkId == (server.Id{}) {
			glog.Errorf("[sub]apple.yml app_store_notifications.sandbox_tester_network_ids[%d] is not a network id; left out\n", i)
			continue
		}
		networkIds[networkId] = true
	}
	return networkIds
}

// Whether the transaction is a sandbox purchase the network may not be
// credited for: the network is not an internal tester. A production purchase
// is never refused here.
func appleSandboxCreditRefused(transaction *validatedAppleTransaction, networkId server.Id) bool {
	if !transaction.sandbox() {
		return false
	}
	if appleSandboxTesterNetworkIdsFunc()[networkId] {
		return false
	}
	glog.Infof(
		"[sub]apple sandbox transaction %s: network %s is not an internal tester (apple.yml app_store_notifications.sandbox_tester_network_ids); not credited\n",
		transaction.transactionId,
		networkId,
	)
	return true
}

// The verify answer for a refused sandbox purchase: invalid, so the app
// finishes the transaction, with the code that tells it why.
func appleSandboxPurchaseRefusal() *VerifyStorePurchaseResult {
	return &VerifyStorePurchaseResult{
		Status: VerifyPurchaseStatusInvalid,
		Error: &OnboardingError{
			Code:    PurchaseErrorCodeSandboxPurchase,
			Message: purchaseSandboxPurchaseMessage,
		},
	}
}
