package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/docopt/docopt-go"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/router"
	"bringyour.com/service/api/handlers"
)

func main() {
	usage := `BringYour API server.

Usage:
  api [--port=<port>]
  api -h | --help
  api --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], bringyour.RequireVersion())
	if err != nil {
		panic(err)
	}

	// FIXME signal cancel
	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	routes := []*router.Route{
		router.NewRoute("GET", "/privacy.txt", router.Txt),
		router.NewRoute("GET", "/terms.txt", router.Txt),
		router.NewRoute("GET", "/vdp.txt", router.Txt),
		router.NewRoute("GET", "/status", router.WarpStatus),
		router.NewRoute("GET", "/stats/last-90", handlers.StatsLast90),
		router.NewRoute("GET", "/stats/providers-overview-last-90", handlers.StatsProvidersOverviewLast90),
		router.NewRoute("GET", "/stats/providers", handlers.StatsProviders),
		router.NewRoute("POST", "/stats/provider-last-90", handlers.StatsProviderLast90),
		router.NewRoute("POST", "/auth/login", handlers.AuthLogin),
		router.NewRoute("POST", "/auth/login-with-password", handlers.AuthLoginWithPassword),
		router.NewRoute("POST", "/auth/verify", handlers.AuthVerify),
		router.NewRoute("POST", "/auth/verify-send", handlers.AuthVerifySend),
		router.NewRoute("POST", "/auth/password-reset", handlers.AuthPasswordReset),
		router.NewRoute("POST", "/auth/password-set", handlers.AuthPasswordSet),
		router.NewRoute("POST", "/auth/network-check", handlers.NetworkCheck),
		router.NewRoute("POST", "/auth/network-create", handlers.NetworkCreate),
		router.NewRoute("POST", "/auth/code-create", handlers.AuthCodeCreate),
		router.NewRoute("POST", "/auth/code-login", handlers.AuthCodeLogin),
		router.NewRoute("POST", "/network/auth-client", handlers.AuthNetworkClient),
		router.NewRoute("POST", "/network/remove-client", handlers.RemoveNetworkClient),
		router.NewRoute("GET", "/network/clients", handlers.NetworkClients),
		router.NewRoute("GET", "/network/provider-locations", handlers.NetworkGetProviderLocations),
		router.NewRoute("POST", "/network/find-provider-locations", handlers.NetworkFindProviderLocations),
		router.NewRoute("POST", "/network/find-locations", handlers.NetworkFindLocations),
		router.NewRoute("POST", "/network/find-providers", handlers.NetworkFindProviders),
		router.NewRoute("POST", "/network/find-providers2", handlers.NetworkFindProviders2),
		router.NewRoute("POST", "/network/create-provider-spec", handlers.NetworkCreateProviderSpec),
		router.NewRoute("GET", "/network/user", handlers.GetNetworkUser),
		router.NewRoute("POST", "/preferences/set-preferences", handlers.PreferencesSet),
		router.NewRoute("POST", "/feedback/send-feedback", handlers.FeedbackSend),
		router.NewRoute("POST", "/pay/stripe", handlers.StripeWebhook),
		router.NewRoute("POST", "/pay/coinbase", handlers.CoinbaseWebhook),
		router.NewRoute("POST", "/pay/circle", handlers.CircleWebhook), // todo - deprecate this
		router.NewRoute("POST", "/pay/play", handlers.PlayWebhook),
		router.NewRoute("GET", "/wallet/balance", handlers.WalletBalance),
		router.NewRoute("POST", "/wallet/validate-address", handlers.WalletValidateAddress),
		router.NewRoute("POST", "/wallet/circle-init", handlers.WalletCircleInit),
		router.NewRoute("POST", "/wallet/circle-transfer-out", handlers.WalletCircleTransferOut),
		router.NewRoute("GET", "/subscription/balance", handlers.SubscriptionBalance),
		router.NewRoute("POST", "/subscription/check-balance-code", handlers.SubscriptionCheckBalanceCode),
		router.NewRoute("POST", "/subscription/redeem-balance-code", handlers.SubscriptionRedeemBalanceCode),
		router.NewRoute("POST", "/subscription/create-payment-id", handlers.SubscriptionCreatePaymentId),
		router.NewRoute("POST", "/device/add", handlers.DeviceAdd),
		router.NewRoute("POST", "/device/create-share-code", handlers.DeviceCreateShareCode),
		router.NewRoute("GET", "/device/share-code/([^/]+)/qr.png", handlers.DeviceShareCodeQR),
		router.NewRoute("POST", "/device/share-status", handlers.DeviceShareStatus),
		router.NewRoute("POST", "/device/confirm-share", handlers.DeviceConfirmShare),
		router.NewRoute("POST", "/device/create-adopt-code", handlers.DeviceCreateAdoptCode),
		router.NewRoute("GET", "/device/adopt-code/([^/]+)/qr.png", handlers.DeviceAdoptCodeQR),
		router.NewRoute("POST", "/device/adopt-status", handlers.DeviceAdoptStatus),
		router.NewRoute("POST", "/device/confirm-adopt", handlers.DeviceConfirmAdopt),
		router.NewRoute("POST", "/device/remove-adopt-code", handlers.DeviceRemoveAdoptCode),
		router.NewRoute("GET", "/device/associations", handlers.DeviceAssociations),
		router.NewRoute("POST", "/device/remove-association", handlers.DeviceRemoveAssociation),
		router.NewRoute("POST", "/device/set-association-name", handlers.DeviceSetAssociationName),
		router.NewRoute("POST", "/device/set-provide", handlers.DeviceSetProvide),
		router.NewRoute("POST", "/connect/control", handlers.ConnectControl),
		router.NewRoute("GET", "/hello", handlers.Hello),
		router.NewRoute("POST", "/account/payout-wallet", handlers.SetPayoutWallet),
		router.NewRoute("GET", "/account/payout-wallet", handlers.GetPayoutWallet),
		router.NewRoute("POST", "/account/circle-wallet", handlers.CircleWebhook),
		router.NewRoute("POST", "/account/wallet", handlers.CreateAccountWallet),
		router.NewRoute("GET", "/account/wallets", handlers.GetAccountWallets),
		router.NewRoute("GET", "/account/payments", handlers.GetAccountPayments),
		router.NewRoute("GET", "/account/referral-code", handlers.GetNetworkReferralCode),
	}

	// bringyour.Logger().Printf("%s\n", opts)

	port, _ := opts.Int("--port")

	bringyour.Logger().Printf(
		"Serving %s %s on *:%d\n",
		bringyour.RequireEnv(),
		bringyour.RequireVersion(),
		port,
	)

	routerHandler := router.NewRouter(cancelCtx, routes)
	err = http.ListenAndServe(fmt.Sprintf(":%d", port), routerHandler)

	bringyour.Logger().Fatal(err)
}
