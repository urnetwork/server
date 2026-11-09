package api

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Who may call each route, by credential (AUTHZ1.md).
//
// The network credential is the network's root token or an API key. A client
// token is the by_jwt of one client: an embed customer's backend hands one to
// each installation of its app, so whoever holds the app holds the token. A
// client token never administers the network or the account. The router
// refuses it on the admin routes before the handler runs, whatever wrapper the
// handler uses, and the model limits the routes it keeps to its own clients.
//
// The P0 auth fix (§7.4's stage 1, D-TEAM-14) adds two refusals. A child or
// admin_refused client's token is refused on the app admin and own client
// payout routes, and a strict_exp token, a sidecar's, reaches only the routes
// strictExpRoutes lists, with a proof of its ClientKey where it renews or mints.
// Each refusal is counted in urnetwork_auth_child_admin_calls_total. M0 ran them
// log-only behind sidecar.yml's p0_enforce for stage 1's 14 days; from M1 they
// refuse in code, whatever that file says or whether it is there at all, so a
// missing or mistaken config push cannot reopen the P0 gap (D-TEAM-1).

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
	"GET /network/sessions":                         routeAccessNetwork,
	"POST /network/revoke-session":                  routeAccessNetwork,
	"POST /network/revoke-other-sessions":           routeAccessNetwork,
	"GET /network/session-operations/([^/]+)":       routeAccessNetwork,
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
	"GET /robots.txt":                               routeAccessPublic, // keeps crawlers out of /lag/ (lag.dev/PLAN.md amendment 11)
	"GET /status":                                   routeAccessPublic,
	"GET /clock":                                    routeAccessPublic,
	"GET /health":                                   routeAccessPublic,
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
	"POST /competition/v2/score":                    routeAccessPublic,
	"POST /competition/v2/score/preflight":          routeAccessPublic,
	"GET /competition/v2/score/([^/]+)":             routeAccessPublic,
	"GET /competition/v2/rounds/([^/]+)/result":     routeAccessPublic,
	"GET /competition/v2/rounds/([^/]+)/decisions":  routeAccessPublic,
	"POST /competition/v2/rounds/([^/]+)/seal":      routeAccessPublic,
	"POST /competition/v2/rounds/([^/]+)/reviews":   routeAccessPublic,
	"GET /competition/v2/events":                    routeAccessPublic,
	"POST /competition/v2/receipts":                 routeAccessPublic,
	"POST /competition/v2/source/([^/]+)/readiness": routeAccessPublic,
	"POST /competition/v2/holds":                    routeAccessPublic,
	"POST /competition/v2/source-handoffs":          routeAccessPublic,
	"POST /competition/v2/rounds/([^/]+)/incidents": routeAccessPublic,
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
	"POST /auth/network-refresh":                    routeAccessNetwork, // renews the network credential: a client token must never obtain one (3b48aa3d made /auth/refresh client only for that reason)
	"POST /auth/verify-send":                        routeAccessPublic,
	"POST /auth/password-reset":                     routeAccessPublic,
	"POST /auth/password-set":                       routeAccessPublic,
	"POST /auth/network-check":                      routeAccessPublic,
	"POST /auth/network-create":                     routeAccessPublic,
	"POST /auth/network-delete":                     routeAccessNetwork, // deletes the account; app admin until §7.4's stage 2 is live (stage2NetworkRoutes)
	"POST /auth/code-create":                        routeAccessNetwork, // auth code → /auth/code-login returns a NETWORK token (escalation); app admin until stage 2 is live
	"POST /auth/code-login":                         routeAccessPublic,
	"POST /auth/apple/callback":                     routeAccessPublic,
	"GET /auth/apple/callback":                      routeAccessPublic,
	"GET /auth/google/callback":                     routeAccessPublic,
	"POST /auth/add-auth":                           routeAccessNetwork,   // adds a sign-in method (takeover); app admin until stage 2 is live
	"POST /auth/remove-auth":                        routeAccessNetwork,   // removes a sign-in method (takeover); app admin until stage 2 is live
	"POST /auth/regenerate-seedphrase":              routeAccessNetwork,   // returns a sign-in seedphrase (escalation); app admin until stage 2 is live
	"POST /auth/generate-seedphrase":                routeAccessNetwork,   // returns a sign-in seedphrase (escalation); app admin until stage 2 is live
	"POST /network/auth-client":                     routeAccessOwnClient, // mint/reissue client tokens
	"POST /network/register-client-v1":              routeAccessNetwork,   // top-level client registration
	"POST /network/remove-client":                   routeAccessOwnClient, // deactivate a client
	"POST /network/remove-clients":                  routeAccessNetwork,   // bulk deactivate up to 1M clients
	"POST /network/extender-activate":               routeAccessClient,
	"GET /network/extender-hint":                    routeAccessPublic,
	"POST /network/extender-latency":                routeAccessClient,
	"POST /network/extender-release":                routeAccessClient,
	"POST /network/extender-block-report":           routeAccessClient,
	"POST /network/extender-camouflage-report":      routeAccessClient,
	"POST /network/ping-report":                     routeAccessClient,
	"GET /network/extender-measurement-cert":        routeAccessClient, // the caller's own extender's certificate (lag.dev/PLAN.md A8)
	"GET /v1/seed\\.bin":                            routeAccessPublic, // the public seed bundle (connect/EXTENDER-UPDATE2.md §2.3)
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
	"POST /network/proxy/rotate-exit":               routeAccessNetwork,  // moves a network's unique-exit proxy to a new exit
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
	"POST /friends/apply":                           routeAccessPublic,
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
	"GET /pay/crypto/assets":                        routeAccessPublic,
	"POST /pay/crypto/intent":                       routeAccessClient,
	"POST /pay/data/crypto-intent":                  routeAccessPublic,
	"POST /pay/crypto/status":                       routeAccessPublic,  // the invoice's status token is the credential
	"POST /pay/crypto/requote":                      routeAccessPublic,  // the invoice's status token is the credential
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
	"GET /admin/reengagement/results":               routeAccessPublic,
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
	"GET /sn/top":                                   routeAccessPublic, // the public board; a token only adds the caller's own row
	"GET /sn/top/me":                                routeAccessClient,
	"POST /sn/head/binding":                         routeAccessOwnClientPayout, // binds the caller's head
	"GET /sn/fleet/device":                          routeAccessOwnClientPayout, // the caller's own pending binding
	"POST /sn/fleet/device/signature":               routeAccessOwnClientPayout, // its own signature on it
	"GET /sn/fleet/device/binding":                  routeAccessOwnClientPayout, // its own binding, to leave
	"POST /sn/fleet/device/leave":                   routeAccessOwnClientPayout, // leaves with its own key
	"GET /sn/fleet/claim":                           routeAccessNetwork,         // the network's claim, for its card
	"POST /sn/fleet/authorization":                  routeAccessNetwork,         // the coldkey's authorization of a hotkey
	"POST /sn/fleet/hotkey-signatures":              routeAccessNetwork,         // the hotkey's signatures, from the key holder
	"POST /sn/fleet/top-up":                         routeAccessNetwork,         // the network's one top-up
	"GET /sn/pool/claim":                            routeAccessClient,
	"GET /sn/epoch":                                 routeAccessPublic,
	"GET /sn/artifact":                              routeAccessPublic,
	"GET /sn/ledger/object":                         routeAccessPublic,
	"GET /sn/ledger/account":                        routeAccessPublic,
	"GET /sn/claim-relay":                           routeAccessOwnClientPayout, // the network's own claim status
	"POST /sn/claim-relay/claim-now":                routeAccessOwnClientPayout, // queues a claim to the network's own coldkey
	"GET /sn/attempt-artifact":                      routeAccessPublic,
	"POST /sn/attempt-artifact":                     routeAccessClient,
	// the validator's client token; its hotkey signature is the authority
	"POST /sn/head-summary":                         routeAccessClient,
	"GET /sn/artifacts":                             routeAccessPublic,
	"GET /sn/evidence":                              routeAccessPublic,
	"POST /sn/evidence":                             routeAccessPublic,
	"GET /sn/evidence/history":                      routeAccessPublic,
	"GET /hello":                                    routeAccessPublic,
	"POST /account/api-key":                         routeAccessNetwork,  // creates an API key = a full network credential (escalation)
	"POST /account/api-key/remove":                  routeAccessNetwork,  // revokes an API key
	"GET /account/api-keys":                         routeAccessNetwork,  // lists API keys
	"GET /network/auth-keys":                        routeAccessNetwork,  // lists auth keys (Network until TEAM-M6-03's roles)
	"POST /network/auth-keys":                       routeAccessNetwork,  // creates an auth key, which enrolls devices (escalation)
	"POST /network/auth-key-revoke":                 routeAccessNetwork,  // revokes an auth key
	"POST /sidecar/bind":                            routeAccessClient,   // appends the bearer's own client generation (§5.15)
	"GET /sidecar/identity/([^/]+)":                 routeAccessClient,   // a sidecar of the caller's network (§5.15)
	"GET /sidecar/identity":                         routeAccessClient,   // ?client_id=: the sidecar a client of the caller's network binds
	"GET /sidecar/sync/poll":                        routeAccessClient,   // the bearer's own client's hints (§6.8)
	"GET /sidecar/sync/versions":                    routeAccessClient,   // the bearer's own client's versions (§6.8)
	"GET /sidecar/sync/([^/]+)":                     routeAccessClient,   // core: the bearer's own client's state (§6.8)
	"GET /sidecar/limits":                           routeAccessClient,   // the caller's network's limits (§20.7)
	"POST /acl/ack":                                 routeAccessClient,   // the bearer's own client's applied projection version (§8.9)
	"GET /network/bridge/share/([^/]+)":             routeAccessClient,   // a sidecar of either network of the share (§17.16)
	"PUT /network/bridge/share/([^/]+)":             routeAccessClient,   // a sidecar's own network's sealed table (§17.16)
	"POST /network/config/delta":                    routeAccessClient,   // a signed statement, verified at the tip (§7.9)
	"POST /network/config/projection":               routeAccessClient,   // a config writer's signed base (§7.9)
	"GET /network/config/projection":                routeAccessClient,   // the network's log, which the caller verifies (§7.9)
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
	"POST /lag/session":                             routeAccessPublic, // creates a lag.dev guest network and its one client (lag.dev/PLAN.md A1)
	"GET /lag/extenders":                            routeAccessClient,
	"POST /lag/track":                               routeAccessClient,
	"POST /network/extender-lag-report":             routeAccessClient,
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

// The only routes a lag.dev guest's client token is accepted on
// (lag.dev/PLAN.md A1): the two the browser calls with it. Every other route
// refuses it with 403 before its handler runs, whatever its class, and the
// connect transport refuses it too, so a guest never administers, buys or
// connects (session/AUTH.md §1).
var lagGuestRoutes = map[string]bool{
	"GET /lag/extenders": true,
	"POST /lag/track":    true,
}

// The only routes a strict_exp bearer reaches (§7.4). Every other route refuses
// it before its handler runs, whatever its class, a new or unclassified route
// included, so an omission here fails closed. A route classified sidecar by
// §5-§21 joins this list when it lands; §7.4 names those it knows
// (/sidecar/*, /network/config/*, /network/enroll, /network/bridge/share/*,
// /acl/*, POST /approval/cancel, /tls/*, /mail/*, /smtp/*, /socket/*, /dns/*,
// GET /abuse/status), and TestSidecarTokenRoutes fails a route under those paths
// that is not here. The route's own class still applies, so /smtp/appeal and
// /mail/direct-mx stay refused.
//
// POST /oauth/token is not here. §7.4 admits a sidecar there for its exchange
// alone (§7.8), and the exchange takes the client token in its form as the
// subject token, with its own DPoP proof (oauth/exchange.go), never as a bearer:
// the route is Public, so a sidecar's exchange reaches it with no bearer at all,
// and a strict_exp bearer is refused there on every grant like anywhere else.
var strictExpRoutes = map[string]bool{
	"GET /auth/refresh":           true, // with the proof
	"POST /network/auth-client":   true, // with the proof; its children inherit both marks
	"POST /network/remove-client": true, // its own client or children (the model)
	"GET /key/([^/]+)":            true,
	"POST /connect/control":       true,
	// §5.15's sidecar routes (CORE-M1-06)
	"POST /sidecar/bind":            true,
	"GET /sidecar/identity/([^/]+)": true,
	"GET /sidecar/identity":         true,
	// §6.8's sync routes (PROTO-M1-10)
	"GET /sidecar/sync/poll":     true,
	"GET /sidecar/sync/versions": true,
	"GET /sidecar/sync/([^/]+)":  true,
	// §20.7's limits (QUOTA-M1-03)
	"GET /sidecar/limits": true,
	// §8.9's ack (ACL-M2-12)
	"POST /acl/ack": true,
	// §17.16's share tables (BRIDGE-M6-02)
	"GET /network/bridge/share/([^/]+)": true,
	"PUT /network/bridge/share/([^/]+)": true,
	// §7.9's authorization log (TEAM-M2-13); an unadmitted client reads it and
	// posts a recovery-signed statement only (the controller)
	"POST /network/config/delta":      true,
	"POST /network/config/projection": true,
	"GET /network/config/projection":  true,
}

// The routes where a strict_exp bearer proves its ClientKey (§7.4). The third
// §7.4 names, POST /oauth/token's exchange, checks its own proof of its subject
// token (oauth/exchange.go).
var strictExpProofRoutes = map[string]bool{
	"GET /auth/refresh":         true,
	"POST /network/auth-client": true,
}

// §7.4's stage 2 (D-TEAM-1, D-TRUST-3): the six routes the shipped desktop app
// and extension still call with a network credential they persist readably
// (.auth_state, chrome.storage.local's by_jwt), which the moved routes refuse
// to app device tokens a same-user agent can read (owner decision 47). The
// table holds their end state, Network; until stage 2 is live in this binary
// (model.SidecarStage2Live) they keep the app admin class, so every binary
// built before D-SDK-11's releases ship keeps stage 1's behavior.
var stage2NetworkRoutes = map[string]bool{
	"POST /auth/code-create":           true,
	"POST /auth/add-auth":              true,
	"POST /auth/remove-auth":           true,
	"POST /auth/regenerate-seedphrase": true,
	"POST /auth/generate-seedphrase":   true,
	"POST /auth/network-delete":        true,
}

// Whether stage 2 is live in this binary. Tests replace it.
var sidecarStage2Live = model.SidecarStage2Live

func routeAccessKey(route *router.Route) string {
	return route.Method() + " " + route.Pattern()
}

// The class a route's gates apply: its table entry, and app admin for a stage 2
// route while stage 2 is not live.
func routeAccessFor(route *router.Route) routeAccess {
	key := routeAccessKey(route)
	if access, ok := routeAccessByRoute[key]; ok {
		if access == routeAccessNetwork && stage2NetworkRoutes[key] && !sidecarStage2Live() {
			return routeAccessAppAdmin
		}
		return access
	}
	return routeAccessNetwork
}

// networkRefusesClientAdmin decides whether a network's client tokens are
// refused on the app admin and own client payout routes too. Tests replace it.
var networkRefusesClientAdmin = model.NetworkRefusesClientAdmin

// Decides whether one client's tokens are, wherever its network stands (§7.4).
// Tests replace it.
var clientRefusesAdmin = model.ClientRefusesAdmin

// The key a strict_exp bearer's proof must be signed by. Tests replace it.
var clientProofPublicKey = model.ClientProofPublicKey

// What a refusal of the P0 auth fix was, as counted.
const (
	p0RefusalClientAdmin = "client_admin"
	p0RefusalStrictExp   = "strict_exp_route"
	p0RefusalProof       = "strict_exp_proof"
	// the one result since M1
	p0ResultRefused = "refused"
)

var childAdminCallCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "auth",
		Name:      "child_admin_calls_total",
		Help:      "Calls the P0 auth fix refuses, by route template, by what refuses them (client_admin: a child or admin_refused client on an app admin or own client payout route; strict_exp_route: a strict_exp token off its allowlist; strict_exp_proof: a strict_exp token without a good proof) and by result, refused since M1 enforces stage 1 (logged in M0's log-only days)",
	},
	[]string{"route", "refusal", "result"},
)

func init() {
	prometheus.MustRegister(childAdminCallCounter)
}

// Counts a call the P0 auth fix refuses, and refuses it. A refusal is driven by
// client input, so it is counted, with the detail at V(1). The result label stays
// for the series M0's log-only days wrote, which counted "logged" there.
func refuseP0Call(routeKey string, refusal string, byJwt *session.ByJwt) bool {
	childAdminCallCounter.WithLabelValues(routeKey, refusal, p0ResultRefused).Inc()
	if glog.V(1) {
		glog.Infof("[auth][%s]%s %s: client %s on %s\n", byJwt.NetworkId, refusal, p0ResultRefused, byJwt.ClientId, routeKey)
	}
	return true
}

// §7.4's check: networkRefusesClientAdmin || clientRefusesAdmin. An Embed
// network's client tokens are refused outright, and are not stage 1's to count;
// stage 1 adds any network's child and admin_refused clients, and a client whose
// marks cannot be read (D-TEAM-1).
func refuseClientAdmin(ctx context.Context, routeKey string, byJwt *session.ByJwt) bool {
	if networkRefusesClientAdmin(ctx, byJwt.NetworkId) {
		return true
	}
	if clientRefusesAdmin(ctx, byJwt.NetworkId, *byJwt.ClientId) {
		return refuseP0Call(routeKey, p0RefusalClientAdmin, byJwt)
	}
	return false
}

// Whether the request's proof shows the strict_exp bearer holds its ClientKey:
// its newest bound key, the proof naming the request's method and its path at
// the api's public origin. A proof spends its jti here, whatever the gate then
// decides.
func strictExpProofHolds(r *http.Request, bearerToken string, byJwt *session.ByJwt) bool {
	boundKey, err := clientProofPublicKey(r.Context(), *byJwt.ClientId)
	if err == nil {
		var dpopRequest *session.DpopRequest
		dpopRequest, err = session.NewApiDpopRequest(r, bearerToken, *byJwt.ClientId, boundKey)
		if err == nil {
			err = session.VerifyDpopProof(r.Context(), dpopRequest)
		}
	}
	if err != nil {
		if glog.V(1) {
			glog.Infof("[auth][%s]proof of client %s refused: %s\n", byJwt.NetworkId, byJwt.ClientId, err)
		}
		return false
	}
	return true
}

// applyRouteAccess puts the client token gate in front of every admin route and
// every own client payout route, the strict_exp gate in front of every route off
// a sidecar's allowlist and the proof gate in front of the routes where a sidecar
// renews or mints, the lag.dev guest gate in front of every route but the guest's
// own, and the intended network gate in front of every route that takes a
// URnetwork credential: every class but public, whose routes do not act for the
// bearer's network (AUTHZ1.md §10). The intended network gate runs first, so a
// request meant for another network is refused before any other gate or the
// handler looks at it. The strict_exp gate runs before the client token gate, so
// a sidecar's token hears its own refusal on every route off its list.
func applyRouteAccess(routes []*router.Route) []*router.Route {
	applied := make([]*router.Route, 0, len(routes))
	for _, route := range routes {
		access := routeAccessFor(route)
		routeKey := routeAccessKey(route)
		if strictExpProofRoutes[routeKey] {
			route = router.RequireStrictExpProof(route, func(r *http.Request, bearerToken string, byJwt *session.ByJwt) bool {
				return !strictExpProofHolds(r, bearerToken, byJwt) && refuseP0Call(routeKey, p0RefusalProof, byJwt)
			})
		}
		switch access {
		case routeAccessNetwork:
			route = router.RefuseClientCredentials(route, router.RefuseEveryClientCredential)
		case routeAccessAppAdmin, routeAccessOwnClientPayout:
			route = router.RefuseClientCredentials(route, func(ctx context.Context, byJwt *session.ByJwt) bool {
				return refuseClientAdmin(ctx, routeKey, byJwt)
			})
		}
		if !strictExpRoutes[routeKey] {
			route = router.RefuseStrictExpCredentials(route, func(ctx context.Context, byJwt *session.ByJwt) bool {
				return refuseP0Call(routeKey, p0RefusalStrictExp, byJwt)
			})
		}
		if !lagGuestRoutes[routeKey] {
			route = router.RefuseLagGuestCredentials(route)
		}
		if access != routeAccessPublic {
			route = router.RefuseIntendedNetworkMismatch(route)
		}
		applied = append(applied, route)
	}
	return applied
}
