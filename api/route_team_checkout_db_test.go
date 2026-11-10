//go:build sidecar_pending

package api

// The role gate at POST /network/team-checkout (QUOTA-M6-02), on Postgres with
// MG6: who reaches the handler.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// The team's Owner and a Billing admin reach team-checkout; every other role, a
// Member included, is refused 403 RolePermissionRefused before the handler; and
// a client token is refused on this Network route.
func TestTeamCheckoutAtTheRoleGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		const routeKey = "POST /network/team-checkout"
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
		for _, role := range []model.NetworkMemberRole{
			model.NetworkMemberRoleOwner,
			model.NetworkMemberRoleAdmin,
			model.NetworkMemberRoleNetworkAdmin,
			model.NetworkMemberRoleItAdmin,
			model.NetworkMemberRoleBillingAdmin,
			model.NetworkMemberRoleAuditor,
			model.NetworkMemberRoleMember,
		} {
			reached := false
			route := testRoleGatedRoute(t, routeKey, &reached)
			w := testRoleServe(route, http.MethodPost, "/network/team-checkout", memberToken(role))
			want := role == model.NetworkMemberRoleOwner || role == model.NetworkMemberRoleBillingAdmin
			if reached != want {
				t.Errorf("the %s reaches team-checkout: %t, want %t", role, reached, want)
			}
			if !reached && (w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), router.RolePermissionRefusedMessage)) {
				t.Errorf("team-checkout for the %s: %d %s", role, w.Code, w.Body.String())
			}
		}

		if access := routeAccessByRoute[routeKey]; access != routeAccessNetwork {
			t.Fatalf("team-checkout is classed %v, want the network credential's", access)
		}
		if permission := routePermissionFor(routeKey); permission != model.RolePermissionBilling {
			t.Fatalf("team-checkout asks %s, want billing", permission)
		}
	})
}
