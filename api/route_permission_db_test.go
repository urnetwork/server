//go:build sidecar_pending

package api

// The role gate through the routes the API serves (TEAM-M6-03), on Postgres
// with MG6: a member's token reaches an admin route only with the route's
// permission, and a personal network's owner reaches every one.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// The gated route of routeKey, its handler replaced by one that marks it was
// reached.
func testRoleGatedRoute(t testing.TB, routeKey string, reached *bool) *router.Route {
	t.Helper()
	for _, route := range applyRouteAccess(Routes()) {
		if routeAccessKey(route) == routeKey {
			return router.Testing_WithHandler(route, func(w http.ResponseWriter, r *http.Request) {
				*reached = true
				w.WriteHeader(http.StatusOK)
			})
		}
	}
	t.Fatalf("no route %s", routeKey)
	return nil
}

// Serves one request through the route with the bearer.
func testRoleServe(route *router.Route, method string, path string, bearer string) *httptest.ResponseRecorder {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return serveRouteAccess(router.NewRouter(ctx, []*router.Route{route}), method, path, "Bearer "+bearer)
}

// A team member's token reaches GET /network/members with members' permission
// (an Admin) and is refused 403 RolePermissionRefused without it (an Auditor),
// the handler never reached; no member reaches the owner of record's account;
// a personal network's owner reaches both.
func TestRoleGateAtTheRoutes(t *testing.T) {
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
			var member *model.NetworkMember
			server.Tx(ctx, func(tx server.PgTx) {
				var err error
				member, err = model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{NetworkId: teamId, UserId: userId, Role: role})
				server.Raise(err)
			})
			signIn := model.ResolveNetworkSignIn(ctx, userId, teamId, server.NowUtc())
			if signIn == nil || signIn.MemberId == nil || *signIn.MemberId != member.MemberId {
				t.Fatalf("no sign-in for the %s", role)
			}
			return model.NewNetworkSignInByJwt(signIn, false).Testing_Sign()
		}
		adminToken := memberToken(model.NetworkMemberRoleAdmin)
		auditorToken := memberToken(model.NetworkMemberRoleAuditor)
		ownerToken := memberToken(model.NetworkMemberRoleOwner)

		reached := false
		members := testRoleGatedRoute(t, "GET /network/members", &reached)
		w := testRoleServe(members, http.MethodGet, "/network/members", adminToken)
		if w.Code != http.StatusOK || !reached {
			t.Fatalf("an Admin on the members route: %d %s", w.Code, w.Body.String())
		}
		reached = false
		w = testRoleServe(members, http.MethodGet, "/network/members", auditorToken)
		if w.Code != http.StatusForbidden || reached || !strings.Contains(w.Body.String(), router.RolePermissionRefusedMessage) {
			t.Fatalf("an Auditor on the members route: %d %s, reached %t", w.Code, w.Body.String(), reached)
		}

		// no member, an Owner included, reaches the owner of record's account
		reached = false
		addAuth := testRoleGatedRoute(t, "POST /auth/add-auth", &reached)
		w = testRoleServe(addAuth, http.MethodPost, "/auth/add-auth", ownerToken)
		if w.Code != http.StatusForbidden || reached {
			t.Fatalf("an Owner on the account's sign-ins: %d %s, reached %t", w.Code, w.Body.String(), reached)
		}

		// a personal network's owner carries no member_id and is not asked
		personalUserId, personalNetworkId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, personalNetworkId, "personal-"+personalNetworkId.String(), personalUserId)
		personalSignIn := model.ResolveNetworkSignIn(ctx, personalUserId, personalNetworkId, server.NowUtc())
		personalToken := model.NewNetworkSignInByJwt(personalSignIn, false).Testing_Sign()
		reached = false
		w = testRoleServe(addAuth, http.MethodPost, "/auth/add-auth", personalToken)
		if !reached {
			t.Fatalf("a personal owner was refused by the role gate: %d %s", w.Code, w.Body.String())
		}
	})
}
