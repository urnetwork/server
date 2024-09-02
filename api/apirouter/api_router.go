package apirouter

import (
	"bringyour.com/bringyour/router"
	"bringyour.com/service/api/handlers"
)

func NewAPIRouter() *router.Router {

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
		router.NewRoute("POST", "/pay/circle", handlers.CircleWebhook),
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
		router.NewRoute("POST", "/payout-wallet", handlers.SetPayoutWallet),
		router.NewRoute("POST", "/account-wallet", handlers.CreateAccountWallet),

		router.NewRoute("PUT", "/peer-to-peer/handshake/([^/]+)/offer/sdp", handlers.PeerToPeerHandshakeOfferSetSDP),
		router.NewRoute("GET", "/peer-to-peer/handshake/([^/]+)/offer/sdp", handlers.PeerToPeerHandshakeLongPollOfferSDP),
		router.NewRoute("POST", "/peer-to-peer/handshake/([^/]+)/offer/peer_candidates", handlers.PeerToPeerHandshakeOfferAddPeerCandidate),
		router.NewRoute("GET", "/peer-to-peer/handshake/([^/]+)/offer/peer_candidates", handlers.PeerToPeerHandshakeOfferLongPollNewPeerCandidates),
		router.NewRoute("PUT", "/peer-to-peer/handshake/([^/]+)/answer/sdp", handlers.PeerToPeerHandshakeAnswerSetSDP),
		router.NewRoute("GET", "/peer-to-peer/handshake/([^/]+)/answer/sdp", handlers.PeerToPeerHandshakeLongPollAnswerSDP),
		router.NewRoute("POST", "/peer-to-peer/handshake/([^/]+)/answer/peer_candidates", handlers.PeerToPeerHandshakeAnswerAddPeerCandidate),
		router.NewRoute("GET", "/peer-to-peer/handshake/([^/]+)/answer/peer_candidates", handlers.PeerToPeerHandshakeAnswerLongPollNewPeerCandidates),
	}

	return router.NewRouter(routes)

}
