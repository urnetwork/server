//go:build sidecar_pending

package model

// GET /network/members by a one-command admin token (TEAM-M6-08's `ur admin members`), which needs MG6: the token
// reads the first page once, a replay answering the first read; a token without the all scope reads nothing.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestMembersReadByAdminTokenOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		_, ownerUserId, _, personal := testTeamPersonalNetwork(t, ctx, "members-token-owner")
		teams := testNetworkTeams(server.NowUtc)
		teams.tierOf = func(ctx context.Context, networkId server.Id) NetworkTier { return NetworkTierTeam }
		created := createTestTeam(t, teams, personal, testTeamName("members-token-team"))
		ownerMemberId := *created.MemberId
		tokenSession := func(scopes []string, jti string) *session.ClientSession {
			clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: *created.NetworkId, UserId: ownerUserId, MemberId: &ownerMemberId})
			clientSession.UnsignedIdentity = true
			clientSession.AdminCommand = &session.AdminCommandUse{
				NetworkId: *created.NetworkId, MemberId: ownerMemberId, Jti: jti, Cmd: "members-test-cmd", Scopes: scopes, ExpireTime: server.NowUtc().Add(time.Hour),
			}
			return clientSession
		}
		if _, err := teams.getMembers(&GetNetworkMembersArgs{}, tokenSession([]string{ApiKeyScopeAuthKeys}, server.NewId().String())); err == nil {
			t.Error("a token without all read the members")
		}
		jti := server.NewId().String()
		first, err := teams.getMembers(&GetNetworkMembersArgs{}, tokenSession([]string{ApiKeyScopeAll}, jti))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(first.Members), 1)
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{NetworkId: *created.NetworkId, UserId: server.NewId(), Role: NetworkMemberRoleMember})
			server.Raise(err)
		})
		replay, err := teams.getMembers(&GetNetworkMembersArgs{}, tokenSession([]string{ApiKeyScopeAll}, jti))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(replay.Members), 1)
	})
}
