package api

import (
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/router"
)

// Routes returns the full set of api routes.
//
// It is shared by the apicli command and by integration tests that serve the
// api in-process (e.g. the proxy integration test), so both exercise the exact
// same handler set.
func Routes() []*router.Route {
	return []*router.Route{
		router.NewRoute("GET", "/privacy.txt", router.Txt),
		router.NewRoute("GET", "/terms.txt", router.Txt),
		router.NewRoute("GET", "/vdp.txt", router.Txt),
		router.NewRoute("GET", "/status", router.WarpStatus),
		router.NewRoute("GET", "/stats/last-90", handlers.StatsLast90),
		router.NewRoute("GET", "/stats/providers", handlers.StatsProviders),
		router.NewRoute("POST", "/stats/providers-last-n", handlers.StatsProvidersLastN),
		router.NewRoute("POST", "/stats/provider-last-n", handlers.StatsProvider),
		router.NewRoute("POST", "/stats/providers-overview-last-n", handlers.StatsProvidersOverview),
		// legacy aliases (kept for existing callers during migration)
		router.NewRoute("GET", "/stats/providers-overview-last-90", handlers.StatsProvidersOverviewLast90),
		router.NewRoute("POST", "/stats/provider-last-90", handlers.StatsProviderLast90),
		router.NewRoute("POST", "/stats/leaderboard", handlers.GetLeaderboard),
		router.NewRoute("POST", "/auth/login", handlers.AuthLogin),
		router.NewRoute("POST", "/auth/wallet-nonce", handlers.AuthWalletNonce),
		router.NewRoute("POST", "/auth/login-with-password", handlers.AuthLoginWithPassword),
		router.NewRoute("POST", "/auth/verify", handlers.AuthVerify),
		router.NewRoute("GET", "/auth/refresh", handlers.AuthRefreshToken),
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
		router.NewRoute("GET", "/network/peers", handlers.NetworkPeers),
		router.NewRoute("GET", "/network/provider-locations", handlers.NetworkGetProviderLocations),
		router.NewRoute("POST", "/network/find-provider-locations", handlers.NetworkFindProviderLocations),
		router.NewRoute("POST", "/network/find-providers2", handlers.NetworkFindProviders2), router.NewRoute("GET", "/network/user", handlers.GetNetworkUser),
		router.NewRoute("POST", "/network/user/update", handlers.UpdateNetworkName),
		router.NewRoute("GET", "/network/ranking", handlers.GetLeaderboardNetworkRanking),
		router.NewRoute("POST", "/network/ranking-visibility", handlers.SetNetworkLeaderboardPublic),

		// block locations
		router.NewRoute("POST", "/network/block-location", handlers.NetworkBlockLocation),
		router.NewRoute("POST", "/network/unblock-location", handlers.NetworkUnblockLocation),
		router.NewRoute("GET", "/network/blocked-locations", handlers.GetNetworkBlockedLocations),

		// reliability
		router.NewRoute("GET", "/network/reliability", handlers.GetNetworkReliability),

		router.NewRoute("POST", "/preferences/set-preferences", handlers.AccountPreferencesSet),
		router.NewRoute("GET", "/preferences", handlers.AccountPreferencesGet),
		router.NewRoute("POST", "/feedback/send-feedback", handlers.FeedbackSend),
		router.NewRoute("POST", "/pay/stripe", handlers.StripeWebhook),
		router.NewRoute("POST", "/pay/coinbase", handlers.CoinbaseWebhook),
		router.NewRoute("POST", "/pay/circle", handlers.CircleWebhook), // todo - deprecate this
		router.NewRoute("POST", "/pay/play", handlers.PlayWebhook),
		router.NewRoute("POST", "/pay/solana", handlers.HeliusWebhook),
		router.NewRoute("POST", "/solana/payment-intent", handlers.CreateSolanaPaymentIntent),
		router.NewRoute("POST", "/stripe/payment-intent", handlers.CreateStripePaymentIntent),
		router.NewRoute("POST", "/stripe/customer-portal", handlers.StripeCreateCustomerPortal),
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
		router.NewRoute("POST", "/device/set-name", handlers.DeviceSetName), router.NewRoute("POST", "/connect/control", handlers.ConnectControl),
		// Unauthenticated public-key lookup; see handlers.GetClientKey.
		router.NewRoute("GET", "/key/([^/]+)", handlers.GetClientKey),
		// routing verification (sn/VALIDATOR.md); auth is the protocol's own
		// Ed25519 signatures, not a JWT — see handlers.Verify
		router.NewRoute("POST", "/verify", handlers.Verify),
		router.NewRoute("GET", "/verify/keys", handlers.GetVerifyKeys),
		// subnet control plane (sn/PLAN.md §5, D-13)
		router.NewRoute("POST", "/sn/wallet", handlers.SnSetWallet),
		router.NewRoute("GET", "/sn/pool/claim", handlers.SnPoolClaim),
		router.NewRoute("GET", "/sn/epoch", handlers.SnEpoch),
		router.NewRoute("GET", "/hello", handlers.Hello),
		router.NewRoute("POST", "/account/api-key", handlers.CreateApiKey),
		router.NewRoute("POST", "/account/api-key/remove", handlers.DeleteApiKey),
		router.NewRoute("GET", "/account/api-keys", handlers.GetApiKeys),
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
		router.NewRoute("GET", "/account/balance-codes", handlers.GetNetworkRedeemedBalanceCodes),
		router.NewRoute("POST", "/referral-code/validate", handlers.ValidateReferralCode),
		router.NewRoute("GET", "/transfer/stats", handlers.TransferStats),
		router.NewRoute("GET", "/connect", handlers.AuthConnect),
		router.NewRoute("POST", "/connect", handlers.AuthConnect),
		router.NewRoute("POST", "/apple/notification", handlers.AppleNotification),
		router.NewRoute("GET", "/my-ip-info", handlers.MyIPInfo),
		router.NewRoute("POST", "/updates/brevo", handlers.BrevoWebhook),
		router.NewRoute("POST", "/log/([^/]+)/upload", handlers.LogUpload),
	}
}
