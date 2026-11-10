//go:build sidecar_pending

package api

// The role gate at §7.6's domain routes and §7.11's audit routes (TEAM-M6-06,
// TEAM-M6-09), on Postgres with MG6: who reaches each handler.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// Auditors and above reach GET /network/admin-events and a Member is refused
// 403 RolePermissionRefused before the handler; the domain routes and the
// webhook route are the Owner's, an Admin refused; discovery is Public and
// asks no role; and the events route refuses a seq or a limit that is no
// number.
func TestAuditAndDomainRoutesAtTheRoleGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, ownerOfRecordId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, teamId, "team-"+teamId.String(), ownerOfRecordId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team(network_id) VALUES($1)`, teamId))
		})
		memberToken := func(role model.NetworkMemberRole) string {
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, server.NewId(), "member-"+userId.String(), userId)
			server.Tx(ctx, func(tx server.PgTx) {
				_, err := model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{NetworkId: teamId, UserId: userId, Role: role})
				server.Raise(err)
			})
			signIn := model.ResolveNetworkSignIn(ctx, userId, teamId, server.NowUtc())
			if signIn == nil || signIn.MemberId == nil {
				t.Fatalf("no sign-in for the %s", role)
			}
			return model.NewNetworkSignInByJwt(signIn, false).Testing_Sign()
		}
		tokens := map[model.NetworkMemberRole]string{}
		for _, role := range []model.NetworkMemberRole{
			model.NetworkMemberRoleOwner,
			model.NetworkMemberRoleAdmin,
			model.NetworkMemberRoleNetworkAdmin,
			model.NetworkMemberRoleItAdmin,
			model.NetworkMemberRoleBillingAdmin,
			model.NetworkMemberRoleAuditor,
			model.NetworkMemberRoleMember,
		} {
			tokens[role] = memberToken(role)
		}
		// whether the role's token reaches the route's handler; a refusal is
		// the role gate's 403
		reaches := func(routeKey string, role model.NetworkMemberRole) bool {
			t.Helper()
			reached := false
			route := testRoleGatedRoute(t, routeKey, &reached)
			method, path, _ := strings.Cut(routeKey, " ")
			w := testRoleServe(route, method, path, tokens[role])
			if reached {
				return true
			}
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), router.RolePermissionRefusedMessage) {
				t.Fatalf("%s for the %s: %d %s", routeKey, role, w.Code, w.Body.String())
			}
			return false
		}

		for role := range tokens {
			if got, want := reaches("GET /network/admin-events", role), role != model.NetworkMemberRoleMember; got != want {
				t.Errorf("the %s reaches the admin events: %t, want %t", role, got, want)
			}
			for _, routeKey := range []string{
				"POST /network/domain-add",
				"POST /network/domain-verify",
				"POST /network/domain-remove",
				"POST /network/audit-webhook",
			} {
				if got, want := reaches(routeKey, role), role == model.NetworkMemberRoleOwner; got != want {
					t.Errorf("the %s reaches %s: %t, want %t", role, routeKey, got, want)
				}
			}
		}

		// discovery asks no credential and no role
		reached := false
		discover := testRoleGatedRoute(t, "POST /auth/sso-discover", &reached)
		routerCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		if w := serveRouteAccess(router.NewRouter(routerCtx, []*router.Route{discover}), http.MethodPost, "/auth/sso-discover", ""); !reached {
			t.Fatalf("discovery with no credential: %d %s", w.Code, w.Body.String())
		}
		reached = false
		if testRoleServe(discover, http.MethodPost, "/auth/sso-discover", tokens[model.NetworkMemberRoleMember]); !reached {
			t.Fatal("discovery refused a Member")
		}

		// the events route's own query checks
		apiRouter := router.NewRouter(routerCtx, applyRouteAccess(Routes()))
		for _, path := range []string{"/network/admin-events?after=abc", "/network/admin-events?after=-1", "/network/admin-events?limit=0", "/network/admin-events?limit=x"} {
			if w := serveRouteAccess(apiRouter, http.MethodGet, path, "Bearer "+tokens[model.NetworkMemberRoleAuditor]); w.Code != http.StatusBadRequest {
				t.Errorf("%s answered %d %s", path, w.Code, w.Body.String())
			}
		}
	})
}
