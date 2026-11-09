//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// §7.5's members, which need MG6 (TEAM-M6-01).

// The seven roles are the config repo's MemberRole values, named as the enum
// names them, and the first four are the AdminSet's.
func TestNetworkMemberRolesAreTheConfigReposRoles(t *testing.T) {
	roles := []NetworkMemberRole{
		NetworkMemberRoleOwner,
		NetworkMemberRoleAdmin,
		NetworkMemberRoleNetworkAdmin,
		NetworkMemberRoleItAdmin,
		NetworkMemberRoleBillingAdmin,
		NetworkMemberRoleAuditor,
		NetworkMemberRoleMember,
	}
	if len(roles) != len(networkMemberRoleBridgeRoles) || len(bridge.MemberRole_name) != len(roles)+1 {
		t.Fatalf("%d roles, %d mapped, %d in the enum", len(roles), len(networkMemberRoleBridgeRoles), len(bridge.MemberRole_name))
	}
	for i, role := range roles {
		bridgeRole := role.BridgeRole()
		if bridgeRole.String() != "MEMBER_ROLE_"+strings.ToUpper(string(role)) || int32(bridgeRole) != int32(i+1) {
			t.Fatalf("%s maps to %s", role, bridgeRole)
		}
		if roleOf, ok := NetworkMemberRoleOfBridge(bridgeRole); !ok || roleOf != role || !role.Valid() {
			t.Fatalf("%s round trips to %s %t", role, roleOf, ok)
		}
		if role.AdminSet() != (i < 4) {
			t.Fatalf("%s's AdminSet is %t", role, role.AdminSet())
		}
	}
	for _, bridgeRole := range []bridge.MemberRole{bridge.MemberRole_MEMBER_ROLE_UNSPECIFIED, bridge.MemberRole(len(roles) + 1)} {
		if role, ok := NetworkMemberRoleOfBridge(bridgeRole); ok {
			t.Fatalf("%d maps to %s", bridgeRole, role)
		}
	}
	for _, role := range []NetworkMemberRole{"", "Owner", "superuser"} {
		if role.Valid() || role.BridgeRole() != bridge.MemberRole_MEMBER_ROLE_UNSPECIFIED {
			t.Fatalf("%q is a role", role)
		}
	}
}

// member_id is new per join: a second join while a membership lives is refused
// and changes nothing, and a join after a removal gets a new member_id, so the
// earlier one, which tokens and rev/ objects carry, names nothing and revives
// nothing.
func TestReinviteGetsANewMemberId(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		_, _, _, ownerSession := testTeamPersonalNetwork(t, ctx, "reinvite-owner")
		created := createTestTeam(t, testNetworkTeams(server.NowUtc), ownerSession, testTeamName("reinvite"))
		teamNetworkId := *created.NetworkId
		_, userId, _, _ := testTeamPersonalNetwork(t, ctx, "reinvite-member")

		join := func(role NetworkMemberRole) (member *NetworkMember, err error) {
			server.Tx(ctx, func(tx server.PgTx) {
				member, err = AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{
					NetworkId: teamNetworkId,
					UserId:    userId,
					Role:      role,
				})
			})
			return
		}
		first, err := join(NetworkMemberRoleMember)
		if err != nil || first.UserId != userId || first.NetworkId != teamNetworkId || first.MemberId == userId {
			t.Fatalf("the first join: %v %+v", err, first)
		}
		if again, err := join(NetworkMemberRoleAdmin); !errors.Is(err, ErrNetworkMemberExists) || again != nil {
			t.Fatalf("a second join answered %v %+v", err, again)
		}
		if stored := GetNetworkMember(ctx, first.MemberId); stored == nil || stored.Role != NetworkMemberRoleMember {
			t.Fatalf("the second join changed the membership: %+v", stored)
		}

		// the removal deletes the row; TEAM-M6-11's flow does it with its rev/
		// object and admin event
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_member WHERE member_id = $1`, first.MemberId))
		})
		second, err := join(NetworkMemberRoleMember)
		if err != nil || second.MemberId == first.MemberId {
			t.Fatalf("the re-join: %v %+v", err, second)
		}
		if GetNetworkMember(ctx, first.MemberId) != nil {
			t.Fatal("the ended membership's member_id still names a membership")
		}
		if current := GetNetworkMemberOfUser(ctx, teamNetworkId, userId); current == nil || current.MemberId != second.MemberId {
			t.Fatalf("the user's membership is %+v", current)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			if GetNetworkMemberInTx(ctx, tx, first.MemberId) != nil {
				t.Error("the ended membership's member_id names a membership in a transaction")
			}
			if current := GetNetworkMemberOfUserInTx(ctx, tx, teamNetworkId, userId); current == nil || current.MemberId != second.MemberId {
				t.Errorf("the user's membership in a transaction is %+v", current)
			}
		})

		// no join takes a role that is not one of the seven
		server.Tx(ctx, func(tx server.PgTx) {
			if _, err := AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{NetworkId: teamNetworkId, UserId: server.NewId(), Role: "superuser"}); err == nil {
				t.Error("a join took an unknown role")
			}
		})
	})
}

// The server holds a member's user, email and role, its JIT state and its
// reauthentication deadline, and nothing of groups, tags, device names or
// admissions, which live in the config repo (Q-TEAM-7, owner decision 46).
func TestNetworkMemberHoldsOnlyUsersEmailsAndRoles(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		columns := []string{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT column_name
				FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = 'network_member'
				ORDER BY column_name
			`)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var column string
					server.Raise(result.Scan(&column))
					columns = append(columns, column)
				}
			})
		})
		want := []string{"email", "member_id", "network_id", "pending", "reauth_deadline", "role", "user_id"}
		if !slices.Equal(columns, want) {
			t.Fatalf("network_member holds %v, want %v", columns, want)
		}
	})
}
