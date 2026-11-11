//go:build sidecar_pending

package api

// GET /network/usage at the role gate (QUOTA-M2-12), on Postgres: the read roles reach its handler and a
// Member is refused 403 RolePermissionRefused before it.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

func TestNetworkUsageRouteAtTheRoleGate(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, ownerOfRecordId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, teamId, "team-"+teamId.String(), ownerOfRecordId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team(network_id) VALUES($1)`, teamId))
		})
		for _, role := range []model.NetworkMemberRole{
			model.NetworkMemberRoleOwner,
			model.NetworkMemberRoleAdmin,
			model.NetworkMemberRoleBillingAdmin,
			model.NetworkMemberRoleAuditor,
			model.NetworkMemberRoleMember,
		} {
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
			token := model.NewNetworkSignInByJwt(signIn, false).Testing_Sign()
			reached := false
			route := testRoleGatedRoute(t, "GET /network/usage", &reached)
			w := testRoleServe(route, http.MethodGet, "/network/usage", token)
			if want := role != model.NetworkMemberRoleMember; reached != want {
				t.Errorf("the %s reaches the usage: %t, want %t (%d %s)", role, reached, want, w.Code, w.Body.String())
			}
			if !reached && (w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), router.RolePermissionRefusedMessage)) {
				t.Errorf("the %s's refusal: %d %s", role, w.Code, w.Body.String())
			}
		}
	})
}
