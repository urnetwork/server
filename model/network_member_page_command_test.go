//go:build sidecar_pending

package model

// QD-1162 with MG6: a one-command admin token reads a later page of GET /network/members by the after its command's
// body names, each page its own command and use, a replay of a page's command answering that page as first read; the
// account session's query cursor reads the same pages.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestMembersPageCommandReadsPastItsAfter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		_, ownerUserId, _, personal := testTeamPersonalNetwork(t, ctx, "members-page-owner")
		teams := testNetworkTeams(server.NowUtc)
		teams.tierOf = func(ctx context.Context, networkId server.Id) NetworkTier { return NetworkTierTeam }
		teams.settings.MemberReadLimit = 1
		created := createTestTeam(t, teams, personal, testTeamName("members-page-team"))
		ownerMemberId := *created.MemberId
		server.Tx(ctx, func(tx server.PgTx) {
			for range 2 {
				_, err := AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{NetworkId: *created.NetworkId, UserId: server.NewId(), Role: NetworkMemberRoleMember})
				server.Raise(err)
			}
		})
		tokenSession := func(jti string) *session.ClientSession {
			clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: *created.NetworkId, UserId: ownerUserId, MemberId: &ownerMemberId})
			clientSession.UnsignedIdentity = true
			clientSession.AdminCommand = &session.AdminCommandUse{
				NetworkId: *created.NetworkId, MemberId: ownerMemberId, Jti: jti, Cmd: "members-page-cmd-" + jti,
				Scopes: []string{ApiKeyScopeAll}, ExpireTime: server.NowUtc().Add(time.Hour),
			}
			return clientSession
		}
		read := func(body string, jti string) *GetNetworkMembersResult {
			t.Helper()
			args, err := NetworkMembersCommandArgs([]byte(body))
			connect.AssertEqual(t, err, nil)
			result, err := teams.getMembers(args, tokenSession(jti))
			connect.AssertEqual(t, err, nil)
			return result
		}

		pages := []server.Id{}
		body := ""
		for range 3 {
			page := read(body, server.NewId().String())
			connect.AssertEqual(t, len(page.Members), 1)
			pages = append(pages, page.Members[0].MemberId)
			body = `{"after":"` + page.Members[0].MemberId.String() + `"}`
		}
		if !slices.IsSortedFunc(pages, func(a server.Id, b server.Id) int { return a.Cmp(b) }) || pages[0] == pages[1] || pages[1] == pages[2] {
			t.Fatalf("pages %v", pages)
		}
		last := read(body, server.NewId().String())
		connect.AssertEqual(t, len(last.Members), 0)
		connect.AssertEqual(t, last.More, false)

		// a replay of the second page's command answers the second page, as first read
		jti := server.NewId().String()
		second := `{"after":"` + pages[0].String() + `"}`
		connect.AssertEqual(t, read(second, jti).Members[0].MemberId, pages[1])
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{NetworkId: *created.NetworkId, UserId: server.NewId(), Role: NetworkMemberRoleMember})
			server.Raise(err)
		})
		connect.AssertEqual(t, read(second, jti).Members[0].MemberId, pages[1])

		// an account session's query cursor reads the same page
		account := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: *created.NetworkId, UserId: ownerUserId, MemberId: &ownerMemberId})
		byQuery, err := teams.getMembers(&GetNetworkMembersArgs{After: &pages[0]}, account)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, byQuery.Members[0].MemberId, pages[1])
	})
}
