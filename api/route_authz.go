package api

import (
	"context"

	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// Who may call each route, by credential (AUTHZ1.md).
//
// The network credential is the network's root token or an API key. A client
// token is the by_jwt of one client: an embed customer's backend hands one to
// each installation of its app, so whoever holds the app holds the token. A
// client token never administers the network or the account. The router
// refuses it on the admin routes before the handler runs, whatever wrapper the
// handler uses, and the model limits the routes it keeps to its own clients.

type routeAccess int

const (
	// no credential, or a credential of its own kind: an operator secret, a
	// webhook signature, a signed object, a competition or admin token, or the
	// OAuth protocol. A by_jwt grants nothing more.
	routeAccessPublic routeAccess = iota + 1
	// any credential, a client token included: the call acts for the caller's
	// own client, reads what every installation shows, or buys or credits
	// something the caller pays for
	routeAccessClient
	// a client token is accepted, and the model limits it to its own client and
	// the clients it created
	routeAccessOwnClient
	// the provider payout of the caller's own client: its subnet wallet
	// mapping, the mapping's consent, the wallet read and its fleet binding.
	// The provider apps call these with their client token, and the model
	// limits the writes to its own client. A client token of a network that hands its tokens to a third
	// party (the Embed plan) is refused: what the network's clients provide is
	// the network's to be paid for, not its users' (AUTHZ1.md, decision 3).
	// Unlike the app admin routes, these stay open to an ordinary network's
	// client tokens
	routeAccessOwnClientPayout
	// network or account administration that the URnetwork apps call with
	// their client token today. A client token of a network that hands its
	// tokens to a third party (the Embed plan) is refused; for every other
	// network this waits on the owner (AUTHZ1.md, decision 1)
	routeAccessAppAdmin
	// network or account administration: the network credential only. A client
	// token is refused with 403
	routeAccessNetwork
)

// routeAccessByRoute classifies every route Routes() serves, by "METHOD
// pattern". TestEveryRouteIsClassified fails for a route with no entry and for
// an entry with no route; an unclassified route refuses client tokens.
var routeAccessByRoute = map[string]routeAccess{
	"POST /provider-work/v1/owners":                 routeAccessPublic,
	"GET /provider-work/v1/owners":                  routeAccessPublic,
	"GET /provider-work/v1/requests":                routeAccessPublic,
	"POST /provider-work/v1/requests":               routeAccessPublic,
	"GET /provider-work/v1/requests/([^/]+)":        routeAccessPublic,
	"POST /provider-work/v1/cuts":                   routeAccessPublic,
	"GET /provider-work/v1/cuts/([^/]+)":            routeAccessPublic,
	"POST /provider-work/v1/authorities":            routeAccessPublic,
	"GET /provider-work/v1/windows":                 routeAccessPublic,
	"GET /privacy.txt":                              routeAccessPublic,
	"GET /terms.txt":                                routeAccessPublic,
	"GET /vdp.txt":                                  routeAccessPublic,
	"GET /status":                                   routeAccessPublic,
	"GET /clock":                                    routeAccessPublic,
	"GET /competition/healthz":                      routeAccessPublic,
	"GET /competition/readyz":                       routeAccessPublic,
	"GET /competition/info":                         routeAccessPublic,
	"GET /competition/leaderboard":                  routeAccessPublic,
	"GET /competition/round/([^/]+)/providers.yml":  routeAccessPublic,
	"POST /competition/generate-staging-round":      routeAccessPublic,
	"POST /competition/close-staging-round":         routeAccessPublic,
	"POST /competition/generate-round":              routeAccessPublic,
	"POST /competition/score":                       routeAccessPublic,
	"GET /competition/score/([^/]+)":                routeAccessPublic,
	"GET /stats/last-90":                            routeAccessPublic,
	"GET /stats/providers-map":                      routeAccessPublic,
	"GET /stats/providers":                          routeAccessNetwork, // the network's provider earnings (account read)
	"POST /stats/providers-last-n":                  routeAccessNetwork, // the network's provider earnings (account read)
	"POST /stats/provider-last-n":                   routeAccessNetwork, // the network's provider earnings (account read)
	"POST /stats/providers-overview-last-n":         routeAccessNetwork, // the network's provider earnings (account read)
	"GET /stats/providers-overview-last-90":         routeAccessNetwork, // the network's provider earnings (account read)
	"POST /stats/provider-last-90":                  routeAccessNetwork, // the network's provider earnings (account read)
	"POST /stats/leaderboard":                       routeAccessClient,
	"POST /stats/points-leaderboard":                routeAccessPublic,
	"POST /auth/login":                              routeAccessPublic,
	"POST /auth/wallet-nonce":                       routeAccessPublic,
	"POST /auth/login-with-password":                routeAccessPublic,
	"POST /auth/verify":                             routeAccessPublic,
	"POST /auth/wallet-challenge":                   routeAccessPublic,
	"GET /auth/refresh":                             routeAccessClient,
	"POST /auth/verify-send":                        routeAccessPublic,
	"POST /auth/password-reset":                     routeAccessPublic,
	"POST /auth/password-set":                       routeAccessPublic,
	"POST /auth/network-check":                      routeAccessPublic,
	"POST /auth/network-create":                     routeAccessPublic,
	"POST /auth/network-delete":                     routeAccessAppAdmin, // deletes the account
	"POST /auth/code-create":                        routeAccessAppAdmin, // auth code → /auth/code-login returns a NETWORK token (escalation)
	"POST /auth/code-login":                         routeAccessPublic,
	"POST /auth/apple/callback":                     routeAccessPublic,
	"GET /auth/apple/callback":                      routeAccessPublic,
	"GET /auth/google/callback":                     routeAccessPublic,
	"POST /auth/add-auth":                           routeAccessAppAdmin,  // adds a sign-in method (takeover)
	"POST /auth/remove-auth":                        routeAccessAppAdmin,  // removes a sign-in method (takeover)
	"POST /auth/regenerate-seedphrase":              routeAccessAppAdmin,  // returns a sign-in seedphrase (escalation)
	"POST /auth/generate-seedphrase":                routeAccessAppAdmin,  // returns a sign-in seedphrase (escalation)
	"POST /network/auth-client":                     routeAccessOwnClient, // mint/reissue client tokens
	"POST /network/register-client-v1":              routeAccessNetwork,   // top-level client registration
	"POST /network/remove-client":                   routeAccessOwnClient, // deactivate a client
	"POST /network/remove-clients":                  routeAccessNetwork,   // bulk deactivate up to 1M clients
	"POST /network/extender-activate":               routeAccessClient,
	"GET /network/extender-hint":                    routeAccessPublic,
	"POST /network/extender-latency":                routeAccessClient,
	"POST /network/extender-release":                routeAccessClient,
	"POST /network/extender-block-report":           routeAccessClient,
	"POST /network/ping-report":                     routeAccessClient,
	"POST /network/provider-egress-location":        routeAccessPublic,
	"GET /network/provider-egress-due":              routeAccessPublic,
	"GET /network/provider-blackhole-due":           routeAccessPublic,
	"POST /network/provider-blackhole-checks":       routeAccessPublic,
	"POST /network/provider-egress-attempt":         routeAccessPublic,
	"GET /network/provider-egress-destinations":     routeAccessPublic,
	"GET /network/prober-credential":                routeAccessPublic,
	"GET /network/geolocation-source-pins":          routeAccessPublic,
	"GET /network/provider-bandwidth-test":          routeAccessPublic,
	"POST /network/provider-bandwidth-result":       routeAccessPublic,
	"POST /network/provider-bandwidth-reserve":      routeAccessPublic,
	"POST /network/provider-egress-health":          routeAccessPublic,
	"POST /network/provider-verdict":                routeAccessClient,
	"GET /network/clients":                          routeAccessAppAdmin, // lists every client of the network
	"GET /network/proxies":                          routeAccessNetwork,  // lists the network's proxy clients
	"GET /network/peers":                            routeAccessClient,
	"GET /network/provider-locations":               routeAccessPublic,
	"POST /network/find-provider-locations":         routeAccessPublic,
	"POST /network/find-providers2":                 routeAccessPublic,
	"GET /network/user":                             routeAccessAppAdmin, // account email/phone and sign-in methods
	"GET /network/provider-status":                  routeAccessClient,
	"POST /network/user/update":                     routeAccessNetwork, // renames the network
	"GET /network/ranking":                          routeAccessClient,
	"POST /network/ranking-visibility":              routeAccessAppAdmin, // network leaderboard visibility
	"POST /network/points-ranking-visibility":       routeAccessAppAdmin, // points leaderboard visibility
	"POST /network/emoji":                           routeAccessAppAdmin, // network emoji tag
	"POST /network/block-location":                  routeAccessAppAdmin, // network-wide location block
	"POST /network/unblock-location":                routeAccessAppAdmin, // network-wide location unblock
	"GET /network/blocked-locations":                routeAccessClient,
	"GET /network/reliability":                      routeAccessClient,
	"POST /network/client-data-cap":                 routeAccessNetwork,   // set a client's data cap
	"GET /network/client-data-cap":                  routeAccessOwnClient, // read a data cap
	"GET /network/client-data-caps":                 routeAccessNetwork,   // list every data cap
	"POST /network/client-acl-group":                routeAccessNetwork,   // set a client's ACL group
	"GET /network/client-acl-group":                 routeAccessOwnClient, // read an ACL group
	"GET /network/embed":                            routeAccessNetwork,   // the network's Embed state; the model refuses a client token too
	"POST /services/contact-sales":                  routeAccessPublic,
	"POST /preferences/set-preferences":             routeAccessAppAdmin, // account email preferences
	"GET /preferences":                              routeAccessClient,
	"POST /feedback/send-feedback":                  routeAccessClient,
	"POST /pay/stripe":                              routeAccessPublic,
	"POST /pay/coinbase":                            routeAccessPublic,
	"POST /pay/circle":                              routeAccessPublic,
	"POST /pay/play":                                routeAccessPublic,
	"POST /pay/solana":                              routeAccessPublic,
	"POST /solana/payment-intent":                   routeAccessClient,
	"POST /solana/payment-transaction":              routeAccessClient,
	"POST /stripe/payment-intent":                   routeAccessClient,
	"POST /stripe/customer-portal":                  routeAccessAppAdmin, // Stripe billing portal (cancel, payment methods, invoices)
	"POST /stripe/create-checkout-session":          routeAccessClient,
	"POST /pay/data/checkout":                       routeAccessPublic,
	"POST /pay/data/network-lookup":                 routeAccessPublic,
	"POST /pay/data/solana-intent":                  routeAccessPublic,
	"POST /pay/data/solana-status":                  routeAccessPublic,
	"GET /wallet/balance":                           routeAccessNetwork, // Circle wallet balance
	"POST /wallet/validate-address":                 routeAccessClient,
	"POST /wallet/circle-init":                      routeAccessNetwork, // creates the Circle wallet
	"POST /wallet/circle-transfer-out":              routeAccessNetwork, // moves USDC out of the Circle wallet
	"GET /subscription/balance":                     routeAccessClient,
	"POST /test/balance-drain":                      routeAccessNetwork, // drains the balance (test)
	"POST /test/balance-restore":                    routeAccessNetwork, // restores the balance (test)
	"POST /onboarding/offer/issue":                  routeAccessClient,
	"POST /onboarding/click":                        routeAccessPublic,
	"GET /onboarding/feedback/([^/]+)":              routeAccessPublic,
	"POST /client/events":                           routeAccessClient,
	"GET /admin/onboarding/results":                 routeAccessPublic,
	"GET /admin/onboarding/email-tracker":           routeAccessPublic,
	"GET /admin/onboarding/experiments":             routeAccessPublic,
	"POST /subscription/stripe/payment-sheet":       routeAccessClient,
	"GET /subscription/stripe/prices":               routeAccessClient,
	"GET /subscription/details":                     routeAccessNetwork, // subscription and billing details
	"POST /subscription/cancel":                     routeAccessNetwork, // cancels the subscription
	"POST /subscription/resume":                     routeAccessNetwork, // resumes the subscription
	"POST /subscription/check-balance-code":         routeAccessClient,
	"POST /subscription/redeem-balance-code":        routeAccessClient,
	"POST /subscription/create-payment-id":          routeAccessClient,
	"POST /subscription/verify-play-purchase":       routeAccessClient,
	"POST /subscription/verify-apple-transaction":   routeAccessClient,
	"GET /x402/skus":                                routeAccessPublic,
	"POST /x402/purchase":                           routeAccessClient,
	"POST /device/add":                              routeAccessNetwork, // device sharing/adoption into the network
	"POST /device/create-share-code":                routeAccessNetwork, // device sharing/adoption into the network
	"GET /device/share-code/([^/]+)/qr.png":         routeAccessNetwork, // device sharing/adoption into the network
	"POST /device/share-status":                     routeAccessNetwork, // device sharing/adoption into the network
	"POST /device/confirm-share":                    routeAccessNetwork, // device sharing/adoption into the network
	"POST /device/create-adopt-code":                routeAccessPublic,
	"GET /device/adopt-code/([^/]+)/qr.png":         routeAccessPublic,
	"POST /device/adopt-status":                     routeAccessPublic,
	"POST /device/confirm-adopt":                    routeAccessPublic,
	"POST /device/remove-adopt-code":                routeAccessPublic,
	"GET /device/associations":                      routeAccessNetwork,   // device sharing/adoption into the network
	"POST /device/remove-association":               routeAccessNetwork,   // device sharing/adoption into the network
	"POST /device/set-association-name":             routeAccessNetwork,   // device sharing/adoption into the network
	"POST /device/set-name":                         routeAccessOwnClient, // renames a device
	"POST /connect/control":                         routeAccessClient,
	"GET /key/([^/]+)":                              routeAccessPublic,
	"GET /key/([^/]+)/history":                      routeAccessPublic,
	"POST /sn/client-key/observation":               routeAccessClient,
	"POST /sn/client-key/observations":              routeAccessClient,
	"POST /verify":                                  routeAccessPublic,
	"GET /verify/keys":                              routeAccessPublic,
	"GET /verify/stats":                             routeAccessPublic,
	"GET /verify/proofs":                            routeAccessPublic,
	"POST /verify/original":                         routeAccessPublic,
	"POST /verify/original/close":                   routeAccessPublic,
	"POST /sn/wallet":                               routeAccessOwnClientPayout, // provider coldkey mapping
	"POST /sn/wallet/consent":                       routeAccessOwnClientPayout, // per-client wallet consent
	"POST /sn/wallet/consent/history":               routeAccessPublic,
	"POST /sn/wallet/network-consent":               routeAccessNetwork, // network-wide payout wallet consent
	"POST /sn/wallet/network-consent/history":       routeAccessPublic,
	"POST /sn/wallet/hotkey-consent":                routeAccessNetwork, // hotkey wallet consent
	"POST /sn/wallet/hotkey-consent/history":        routeAccessPublic,
	"POST /sn/wallet/hotkey-delegation":             routeAccessNetwork, // hotkey delegation
	"POST /sn/wallet/hotkey-delegation/history":     routeAccessPublic,
	"GET /sn/wallet":                                routeAccessOwnClientPayout, // wallet read
	"POST /sn/wallet/validate":                      routeAccessPublic,
	"GET /sn/head":                                  routeAccessClient,
	"POST /sn/head/binding":                         routeAccessOwnClientPayout, // binds the caller's head
	"GET /sn/pool/claim":                            routeAccessClient,
	"GET /sn/epoch":                                 routeAccessPublic,
	"GET /sn/artifact":                              routeAccessPublic,
	"GET /sn/attempt-artifact":                      routeAccessPublic,
	"POST /sn/attempt-artifact":                     routeAccessClient,
	"GET /sn/artifacts":                             routeAccessPublic,
	"GET /sn/evidence":                              routeAccessPublic,
	"POST /sn/evidence":                             routeAccessPublic,
	"GET /sn/evidence/history":                      routeAccessPublic,
	"GET /hello":                                    routeAccessPublic,
	"POST /account/api-key":                         routeAccessNetwork,  // creates an API key = a full network credential (escalation)
	"POST /account/api-key/remove":                  routeAccessNetwork,  // revokes an API key
	"GET /account/api-keys":                         routeAccessNetwork,  // lists API keys
	"POST /account/payout-wallet":                   routeAccessAppAdmin, // redirects payouts
	"GET /account/payout-wallet":                    routeAccessAppAdmin, // payout wallet read
	"POST /account/circle-wallet":                   routeAccessPublic,
	"POST /account/wallet":                          routeAccessAppAdmin, // adds a payout wallet
	"GET /account/wallets":                          routeAccessAppAdmin, // account wallets
	"POST /account/wallets/remove":                  routeAccessAppAdmin, // removes a payout wallet
	"POST /account/wallets/verify-seeker":           routeAccessAppAdmin, // marks a wallet as a Seeker holder
	"GET /account/payments":                         routeAccessAppAdmin, // payout history
	"GET /account/referral-code":                    routeAccessClient,
	"GET /account/referral-network":                 routeAccessClient,
	"GET /account/unlink-referral-network":          routeAccessAppAdmin, // unlinks the referrer
	"POST /account/set-referral":                    routeAccessAppAdmin, // sets the referrer
	"POST /account/change-name":                     routeAccessAppAdmin, // renames the network
	"POST /account/claim-name":                      routeAccessAppAdmin, // claims a network name
	"GET /account/points":                           routeAccessClient,
	"GET /account/epochs":                           routeAccessClient,
	"GET /account/balance-codes":                    routeAccessAppAdmin, // purchase history (redeemed codes)
	"POST /referral-code/validate":                  routeAccessPublic,
	"GET /transfer/stats":                           routeAccessClient,
	"GET /connect":                                  routeAccessPublic,
	"POST /connect":                                 routeAccessPublic,
	"POST /apple/notification":                      routeAccessPublic,
	"GET /my-ip-info":                               routeAccessPublic,
	"POST /updates/brevo":                           routeAccessPublic,
	"POST /log/([^/]+)/upload":                      routeAccessClient,
	"GET /\\.well-known/oauth-authorization-server": routeAccessPublic,
	"GET /\\.well-known/openid-configuration":       routeAccessPublic,
	"GET /\\.well-known/jwks\\.json":                routeAccessPublic,
	"POST /oauth/token":                             routeAccessPublic,
	"POST /oauth/register":                          routeAccessPublic,
	"POST /oauth/revoke":                            routeAccessPublic,
	"GET /oauth/userinfo":                           routeAccessPublic,
	"POST /oauth/authorize":                         routeAccessNetwork, // grants a third-party OAuth client access to the account
	"POST /oauth/consent":                           routeAccessNetwork, // OAuth consent screen data
}

func routeAccessKey(route *router.Route) string {
	return route.Method() + " " + route.Pattern()
}

func routeAccessFor(route *router.Route) routeAccess {
	if access, ok := routeAccessByRoute[routeAccessKey(route)]; ok {
		return access
	}
	return routeAccessNetwork
}

// networkRefusesClientAdmin decides whether a network's client tokens are
// refused on the app admin and own client payout routes too. Tests replace it.
var networkRefusesClientAdmin = model.NetworkRefusesClientAdmin

func refuseClientAdmin(ctx context.Context, byJwt *jwt.ByJwt) bool {
	return networkRefusesClientAdmin(ctx, byJwt.NetworkId)
}

// applyRouteAccess puts the client token gate in front of every admin route and
// every own client payout route.
func applyRouteAccess(routes []*router.Route) []*router.Route {
	applied := make([]*router.Route, 0, len(routes))
	for _, route := range routes {
		switch routeAccessFor(route) {
		case routeAccessNetwork:
			route = router.RefuseClientCredentials(route, router.RefuseEveryClientCredential)
		case routeAccessAppAdmin, routeAccessOwnClientPayout:
			route = router.RefuseClientCredentials(route, refuseClientAdmin)
		}
		applied = append(applied, route)
	}
	return applied
}
