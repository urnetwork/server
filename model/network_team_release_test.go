//go:build !sidecar_pending

package model

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A release leg until MG6's Gn-out (FS-54's untagged case): no Team table
// exists, both team routes answer that teams are not available, and nothing
// reaches the missing tables.
func TestTeamRoutesAreUnavailableWithoutMg6(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if server.SidecarMg6Linked() {
			t.Fatal("a release build links a pending group")
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, table := range []string{"network_team", "network_member"} {
				var found *string
				server.Raise(conn.QueryRow(ctx, `SELECT to_regclass($1)::text`, table).Scan(&found))
				if found != nil {
					t.Fatalf("%s exists without MG6", table)
				}
			}
		})
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: server.NewId(), UserId: server.NewId()})
		if result, err := CreateNetworkTeam(&CreateNetworkTeamArgs{NetworkName: "release-leg-team"}, clientSession); err == nil || !strings.HasPrefix(err.Error(), "503 ") || result != nil {
			t.Fatalf("team-create: %v %+v", err, result)
		}
		if result, err := GetNetworkMembers(&GetNetworkMembersArgs{}, clientSession); err == nil || !strings.HasPrefix(err.Error(), "503 ") || result != nil {
			t.Fatalf("members: %v %+v", err, result)
		}
		if GetNetworkTeam(ctx, clientSession.ByJwt.NetworkId) != nil || GetNetworkMember(ctx, server.NewId()) != nil ||
			GetNetworkMemberOfUser(ctx, clientSession.ByJwt.NetworkId, clientSession.ByJwt.UserId) != nil {
			t.Fatal("a read found a team without MG6")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			if _, err := AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{NetworkId: server.NewId(), UserId: server.NewId(), Role: NetworkMemberRoleOwner}); err == nil {
				t.Error("a membership was added without MG6")
			}
			// a deletion's hooks touch no missing table
			removeNetworkTeamInTx(ctx, tx, server.NewId())
			removeNetworkMembershipsOfUserInTx(ctx, tx, server.NewId())
		})
	})
}
