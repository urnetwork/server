package api

// §7.5's roles at the routes (R-TEAM-2, D-TEAM-3, I-6; TEAM-M6-03): every
// route route_authz.go classifies Network or AppAdmin names the permission it
// asks of a team member's role (model.RolePermission). A team member's
// credential, a network or client token naming a member, an API key whose
// creator is a member, or an admin token of a member, reaches such a route only
// where its role, cut by its scopes, holds the permission; otherwise 403
// RolePermissionRefused, before the handler runs (router.RequireRolePermission).
//
// A route the table does not map asks RolePermissionOwner: owner-only, failing
// closed (D-TEAM-3), and TestEveryAdminRouteHasAPermission fails until it is
// mapped. A personal network's owner carries no member_id and passes, as do a
// personal network's API keys within their scopes; the strict_exp allowlist
// still refuses a sidecar's token on every route here, and the client token
// gate every client token on a Network route.
//
// The gate decides on the credential's claims as sent, before the handler
// authenticates it: a refusal answers 403 at once, and a credential the gate
// passes is still authenticated by the handler, so a forged claim gains
// nothing. Only a member credential's role is read from Postgres, mirrored from
// the config repo (model.MemberRole).

import (
	"net/http"
	"strings"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/apikey"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/session"
)

// The permission each Network and AppAdmin route asks, by "METHOD pattern".
var routePermissionByRoute = map[string]model.RolePermission{
	// the owner of record's own account: its sign-ins, seedphrases and codes
	"POST /auth/add-auth":              model.RolePermissionAccount,
	"POST /auth/remove-auth":           model.RolePermissionAccount,
	"POST /auth/generate-seedphrase":   model.RolePermissionAccount,
	"POST /auth/regenerate-seedphrase": model.RolePermissionAccount,
	"POST /auth/code-create":           model.RolePermissionAccount,
	"POST /test/balance-drain":         model.RolePermissionAccount,
	"POST /test/balance-restore":       model.RolePermissionAccount,

	// the Owner's: deletion and the network's sessions
	"POST /auth/network-delete": model.RolePermissionOwner,
	"POST /network/idp":         model.RolePermissionOwner,
	// where the team's sign-ins route, and where its admin events are sent
	"POST /network/domain-add":                model.RolePermissionOwner,
	"POST /network/domain-verify":             model.RolePermissionOwner,
	"POST /network/domain-remove":             model.RolePermissionOwner,
	"POST /network/audit-webhook":             model.RolePermissionOwner,
	"GET /network/sessions":                   model.RolePermissionOwner,
	"POST /network/revoke-session":            model.RolePermissionOwner,
	"POST /network/revoke-other-sessions":     model.RolePermissionOwner,
	"GET /network/session-operations/([^/]+)": model.RolePermissionOwner,

	// the caller's own credential, account and preferences
	"POST /auth/network-refresh":        model.RolePermissionSelf,
	"GET /network/user":                 model.RolePermissionSelf,
	"POST /preferences/set-preferences": model.RolePermissionSelf,
	"POST /network/team-create":         model.RolePermissionSelf,

	// members, roles and invitations
	"GET /network/members": model.RolePermissionMembers,

	// the network's own settings
	"POST /account/change-name":               model.RolePermissionAdmin,
	"POST /network/user/update":               model.RolePermissionAdmin,
	"POST /account/claim-name":                model.RolePermissionAdmin,
	"POST /network/emoji":                     model.RolePermissionAdmin,
	"POST /network/ranking-visibility":        model.RolePermissionAdmin,
	"POST /network/points-ranking-visibility": model.RolePermissionAdmin,
	"POST /account/set-referral":              model.RolePermissionAdmin,
	"GET /account/unlink-referral-network":    model.RolePermissionAdmin,

	// admissions: clients, devices and proxies
	"POST /network/register-client-v1":      model.RolePermissionDevices,
	"POST /network/remove-clients":          model.RolePermissionDevices,
	"POST /network/client-data-cap":         model.RolePermissionDevices,
	"POST /network/proxy/rotate-exit":       model.RolePermissionDevices,
	"GET /device/associations":              model.RolePermissionDevices,
	"POST /device/add":                      model.RolePermissionDevices,
	"POST /device/confirm-share":            model.RolePermissionDevices,
	"POST /device/create-share-code":        model.RolePermissionDevices,
	"POST /device/remove-association":       model.RolePermissionDevices,
	"POST /device/set-association-name":     model.RolePermissionDevices,
	"GET /device/share-code/([^/]+)/qr.png": model.RolePermissionDevices,
	"POST /device/share-status":             model.RolePermissionDevices,

	// policy and network-wide rules
	"POST /network/block-location":   model.RolePermissionPolicy,
	"POST /network/unblock-location": model.RolePermissionPolicy,
	"POST /network/client-acl-group": model.RolePermissionPolicy,

	// keys and OAuth clients
	"GET /account/api-keys":         model.RolePermissionKeys,
	"POST /account/api-key":         model.RolePermissionKeys,
	"POST /account/api-key/remove":  model.RolePermissionKeys,
	"GET /network/auth-keys":        model.RolePermissionKeys,
	"POST /network/auth-keys":       model.RolePermissionKeys,
	"POST /network/auth-key-revoke": model.RolePermissionKeys,
	"POST /oauth/authorize":         model.RolePermissionKeys,
	"POST /oauth/consent":           model.RolePermissionKeys,

	// billing, payouts and wallets
	"GET /account/balance-codes":          model.RolePermissionBilling,
	"GET /account/payments":               model.RolePermissionBilling,
	"GET /account/payout-wallet":          model.RolePermissionBilling,
	"POST /account/payout-wallet":         model.RolePermissionBilling,
	"POST /account/wallet":                model.RolePermissionBilling,
	"GET /account/wallets":                model.RolePermissionBilling,
	"POST /account/wallets/remove":        model.RolePermissionBilling,
	"POST /account/wallets/verify-seeker": model.RolePermissionBilling,
	"POST /stripe/customer-portal":        model.RolePermissionBilling,
	"POST /network/team-checkout":         model.RolePermissionBilling,
	"GET /subscription/details":           model.RolePermissionBilling,
	"POST /subscription/cancel":           model.RolePermissionBilling,
	"POST /subscription/resume":           model.RolePermissionBilling,
	"GET /wallet/balance":                 model.RolePermissionBilling,
	"POST /wallet/circle-init":            model.RolePermissionBilling,
	"POST /wallet/circle-transfer-out":    model.RolePermissionBilling,
	"POST /sn/fleet/authorization":        model.RolePermissionBilling,
	"GET /sn/fleet/claim":                 model.RolePermissionBilling,
	"POST /sn/fleet/hotkey-signatures":    model.RolePermissionBilling,
	"POST /sn/fleet/top-up":               model.RolePermissionBilling,
	"POST /sn/wallet/hotkey-consent":      model.RolePermissionBilling,
	"POST /sn/wallet/hotkey-delegation":   model.RolePermissionBilling,
	"POST /sn/wallet/network-consent":     model.RolePermissionBilling,

	// reading settings, clients and the network's earnings
	"GET /network/clients":                  model.RolePermissionRead,
	"GET /network/admin-events":             model.RolePermissionRead,
	"GET /network/client-data-caps":         model.RolePermissionRead,
	"GET /network/embed":                    model.RolePermissionRead,
	"GET /network/proxies":                  model.RolePermissionRead,
	"GET /stats/providers":                  model.RolePermissionRead,
	"POST /stats/provider-last-90":          model.RolePermissionRead,
	"POST /stats/provider-last-n":           model.RolePermissionRead,
	"POST /stats/providers-last-n":          model.RolePermissionRead,
	"POST /stats/providers-overview-last-n": model.RolePermissionRead,
	"GET /stats/providers-overview-last-90": model.RolePermissionRead,
}

// The permission routeKey asks: its table entry, and the Owner's for a route
// the table does not map.
func routePermissionFor(routeKey string) model.RolePermission {
	if permission, ok := routePermissionByRoute[routeKey]; ok {
		return permission
	}
	return model.RolePermissionOwner
}

var rolePermissionRefusalCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "auth",
		Name:      "role_permission_refusals_total",
		Help:      "Calls the role gate refuses (§7.5, D-TEAM-3), by route template and the permission the route asks",
	},
	[]string{"route", "permission"},
)

func init() {
	prometheus.MustRegister(rolePermissionRefusalCounter)
}

// What the gate reads of a request's credential: whose role, if a member's,
// and the scopes that cut it.
type roleCredential struct {
	networkId server.Id
	// nil for a personal network's owner
	memberId *server.Id
	// nil for no scopes of the credential's own
	scopes []string
}

// The role credential of the request, false for none the gate asks about: no
// credential, one that does not parse, or a personal network's token. Claims
// are read as sent; the handler authenticates them (the file header).
func roleCredentialOf(r *http.Request) (*roleCredential, bool) {
	authorization := r.Header.Get("Authorization")
	ctx := r.Context()
	switch {
	case strings.HasPrefix(authorization, "Bearer urn_"):
		byKey := apikey.GetNetworkByApiKey(authorization[len("Bearer "):], ctx)
		if byKey == nil {
			return nil, false
		}
		return &roleCredential{networkId: byKey.NetworkId, memberId: byKey.MemberId, scopes: byKey.Scopes}, true
	case strings.HasPrefix(authorization, "Bearer "):
		byJwt, err := session.ParseByJwtUnverified(ctx, authorization[len("Bearer "):])
		if err != nil || byJwt.MemberId == nil {
			return nil, false
		}
		memberId := *byJwt.MemberId
		return &roleCredential{networkId: byJwt.NetworkId, memberId: &memberId}, true
	case strings.HasPrefix(authorization, "DPoP "):
		claims := &oauth.AdminTokenClaims{}
		if _, _, err := gojwt.NewParser().ParseUnverified(authorization[len("DPoP "):], claims); err != nil {
			return nil, false
		}
		scopes := strings.Fields(claims.Scope)
		if !model.NetworkIsTeam(ctx, claims.NetworkId) {
			// a personal network's admin token names its owner's user
			return &roleCredential{networkId: claims.NetworkId, scopes: scopes}, true
		}
		memberId := claims.MemberId
		return &roleCredential{networkId: claims.NetworkId, memberId: &memberId, scopes: scopes}, true
	}
	return nil, false
}

// Whether the role gate refuses the request on routeKey: a member credential
// whose role, cut by its scopes, lacks the route's permission, or a scoped
// credential of a personal network whose scopes lack it. A member whose role
// cannot be read is refused.
func refuseRolePermission(r *http.Request, routeKey string) bool {
	credential, ok := roleCredentialOf(r)
	if !ok {
		return false
	}
	permission := routePermissionFor(routeKey)
	refused := false
	if credential.memberId == nil {
		// a personal network's owner, within its credential's scopes
		refused = !model.ScopesPermit(credential.scopes, permission)
	} else {
		role, ok := model.MemberRole(r.Context(), credential.networkId, *credential.memberId)
		refused = !ok || !model.RoleAndScopesPermit(role, credential.scopes, permission)
	}
	if refused {
		rolePermissionRefusalCounter.WithLabelValues(routeKey, string(permission)).Inc()
		if glog.V(1) {
			glog.Infof("[auth][%s]role refused on %s, which asks %s\n", credential.networkId, routeKey, permission)
		}
	}
	return refused
}
