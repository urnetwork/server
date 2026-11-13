// The Circle client of the customer wallets: wallet initialization, balances,
// customer transfers and notification signatures (circle_read.go,
// circle_wallet_controller.go, circle_transfer_request.go). The developer
// wallet transfers that paid providers in USDC ended with USDC provider
// payouts (owner decisions 2026-11-12 and 2026-11-13: "settle closed rows,
// let's close out usdc entirely").
package controller

type CoreCircleApiClient struct {
	readSettings PaymentReadSettings
	readHooks    paymentReadHooks
	readToken    func() string
}
