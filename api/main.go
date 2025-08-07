package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"syscall"

	// "time"

	"github.com/docopt/docopt-go"
	// "github.com/prometheus/client_golang/prometheus"
	// "github.com/prometheus/client_golang/prometheus/push"

	"github.com/golang/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/router"
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

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}

	quitEvent := server.NewEventWithContext(context.Background())
	defer quitEvent.Set()

	closeFn := quitEvent.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM)
	defer closeFn()

	routes := []*router.Route{
		router.NewRoute("GET", "/privacy.txt", router.Txt),
		router.NewRoute("GET", "/terms.txt", router.Txt),
		router.NewRoute("GET", "/vdp.txt", router.Txt),
		router.NewRoute("GET", "/status", router.WarpStatus),
		router.NewRoute("GET", "/stats/last-90", handlers.StatsLast90),
		router.NewRoute("GET", "/stats/providers-overview-last-90", handlers.StatsProvidersOverviewLast90),
		router.NewRoute("GET", "/stats/providers", handlers.StatsProviders),
		router.NewRoute("POST", "/stats/provider-last-90", handlers.StatsProviderLast90),
		router.NewRoute("POST", "/stats/leaderboard", handlers.GetLeaderboard),
		router.NewRoute("POST", "/auth/login", handlers.AuthLogin),
		router.NewRoute("POST", "/auth/login-with-password", handlers.AuthLoginWithPassword),
		router.NewRoute("POST", "/auth/verify", handlers.AuthVerify),
		router.NewRoute("POST", "/auth/verify-send", handlers.AuthVerifySend),
		router.NewRoute("POST", "/auth/password-reset", handlers.AuthPasswordReset),
		router.NewRoute("POST", "/auth/password-set", handlers.AuthPasswordSet),
		router.NewRoute("POST", "/auth/network-check", handlers.NetworkCheck),
		router.NewRoute("POST", "/auth/network-create", handlers.NetworkCreate),
		router.NewRoute("POST", "/auth/network-delete", handlers.RemoveNetwork),
		router.NewRoute("POST", "/auth/code-create", handlers.AuthCodeCreate),
		router.NewRoute("POST", "/auth/code-login", handlers.AuthCodeLogin),
		router.NewRoute("POST", "/auth/upgrade-guest", handlers.UpgradeGuest),
		router.NewRoute("POST", "/auth/upgrade-guest-existing", handlers.UpgradeGuestExisting),
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
		router.NewRoute("POST", "/network/user/update", handlers.UpdateNetworkName),
		router.NewRoute("GET", "/network/ranking", handlers.GetLeaderboardNetworkRanking),
		router.NewRoute("POST", "/network/ranking-visibility", handlers.SetNetworkLeaderboardPublic),

		// block locations
		router.NewRoute("POST", "/network/block-location", handlers.NetworkBlockLocation),
		router.NewRoute("POST", "/network/unblock-location", handlers.NetworkUnblockLocation),
		router.NewRoute("GET", "/network/blocked-locations", handlers.GetNetworkBlockedLocations),

		router.NewRoute("POST", "/preferences/set-preferences", handlers.AccountPreferencesSet),
		router.NewRoute("GET", "/preferences", handlers.AccountPreferencesGet),
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
		router.NewRoute("POST", "/account/wallets/remove", handlers.RemoveWallet),
		router.NewRoute("POST", "/account/wallets/verify-seeker", handlers.VerifyHoldingSeekerToken),
		router.NewRoute("GET", "/account/payments", handlers.GetAccountPayments),
		router.NewRoute("GET", "/account/referral-code", handlers.GetNetworkReferralCode),
		router.NewRoute("GET", "/account/referral-network", handlers.GetReferralNetwork),
		router.NewRoute("GET", "/account/unlink-referral-network", handlers.UnlinkReferralNetwork),
		router.NewRoute("POST", "/account/set-referral", handlers.SetNetworkReferral),
		router.NewRoute("GET", "/account/points", handlers.GetAccountPoints),
		router.NewRoute("POST", "/referral-code/validate", handlers.ValidateReferralCode),
		router.NewRoute("GET", "/transfer/stats", handlers.TransferStats),
		router.NewRoute("GET", "/connect", handlers.AuthConnect),
		router.NewRoute("POST", "/connect", handlers.AuthConnect),
		router.NewRoute("POST", "/apple/notification", handlers.AppleNotification),
		router.NewRoute("GET", "/my-ip-info", handlers.MyIPInfo),
	}

	// server.().Printf("%s\n", opts)

	port, _ := opts.Int("--port")

	server.Warmup()

	glog.Infof(
		"[api]serving %s %s on *:%d\n",
		server.RequireEnv(),
		server.RequireVersion(),
		port,
	)

	// if os.Getenv("SKIP_METRICS") == "" {
	// 	pushMetrics := push.New("https://push-gateway.cluster.bringyour.dev", "my_job").
	// 		Gatherer(prometheus.DefaultGatherer).
	// 		Grouping("warp_block", server.RequireBlock()).
	// 		Grouping("warp_env", server.RequireEnv()).
	// 		Grouping("warp_version", server.RequireVersion()).
	// 		Grouping("warp_service", server.RequireService()).
	// 		Grouping("warp_config_version", server.RequireConfigVersion()).
	// 		Grouping("warp_host", server.RequireHost())

	// 	go func() {
	// 		for {
	// 			select {
	// 			case <-quitEvent.Ctx.Done():
	// 				return
	// 			case <-time.NewTicker(30 * time.Second).C:
	// 				err := pushMetrics.Push()
	// 				if err != nil {
	// 					glog.Errorf("[api]pushMetrics.Push = %s\n", err)
	// 				}
	// 			}
	// 		}
	// 	}()
	// }
	routerHandler := router.NewRouter(quitEvent.Ctx, routes)
	err = http.ListenAndServe(fmt.Sprintf(":%d", port), routerHandler)
	glog.Errorf("[api]close = %s\n", err)
}
