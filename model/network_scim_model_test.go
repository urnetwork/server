//go:build sidecar_pending

package model

// §7.6's SCIM queue (network_scim_model.go; TEAM-M6-07), which needs MG6: who reads GET /network/scim-changes, its
// pages, the tier, a one-command admin token read once, and the queue's 7 days, with the team tier stubbed on.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A team with an Owner and a Member, each with a credential, and three queued changes.
type scimChangesFixture struct {
	ctx           context.Context
	teamNetworkId server.Id
	owner         *session.ClientSession
	member        *session.ClientSession
	memberUserId  server.Id
	now           time.Time
	changes       *networkScimChanges
}

func newScimChangesFixture(t testing.TB) *scimChangesFixture {
	t.Helper()
	ctx := context.Background()
	_, restore := Testing_InMemoryNetworkNameSearch(ctx)
	t.Cleanup(restore)
	_, ownerUserId, _, personal := testTeamPersonalNetwork(t, ctx, "scim-queue-owner")
	created := createTestTeam(t, testNetworkTeams(server.NowUtc), personal, testTeamName("scim-queue-team"))
	self := &scimChangesFixture{ctx: ctx, teamNetworkId: *created.NetworkId, now: server.NowUtc()}
	ownerMemberId := *created.MemberId
	self.owner = session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: ownerUserId, MemberId: &ownerMemberId})
	self.memberUserId = server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		member, err := AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{NetworkId: self.teamNetworkId, UserId: self.memberUserId, Role: NetworkMemberRoleMember})
		server.Raise(err)
		memberId := member.MemberId
		self.member = session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: self.memberUserId, MemberId: &memberId})
		for _, kind := range []NetworkScimChangeKind{NetworkScimChangeKindMemberRevoke, NetworkScimChangeKindUserCreate, NetworkScimChangeKindGroup} {
			_, err := AddNetworkScimChangeInTx(ctx, tx, self.teamNetworkId, nil, kind, map[string]string{"kind": string(kind)}, self.now)
			server.Raise(err)
		}
	})
	self.changes = &networkScimChanges{
		settings: DefaultNetworkScimSettings(),
		now:      func() time.Time { return self.now },
		tierOf:   func(ctx context.Context, networkId server.Id) NetworkTier { return NetworkTierTeam },
	}
	return self
}

// The Owner reads the queue, a page at a time; a Member, a client token and an API key are refused; and a network
// outside the team tier reads nothing.
func TestScimChangesReadByOwnersAndAdmins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newScimChangesFixture(t)
		result, err := self.changes.get(&GetNetworkScimChangesArgs{}, self.owner)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(result.Changes), 3)
		connect.AssertEqual(t, result.More, false)

		self.changes.settings = &NetworkScimSettings{ChangeRetention: 7 * 24 * time.Hour, ChangeReadLimit: 2}
		first, err := self.changes.get(&GetNetworkScimChangesArgs{}, self.owner)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(first.Changes), 2)
		connect.AssertEqual(t, first.More, true)
		after := first.Changes[1].ChangeId
		second, err := self.changes.get(&GetNetworkScimChangesArgs{After: &after}, self.owner)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(second.Changes), 1)
		connect.AssertEqual(t, second.More, false)

		if _, err := self.changes.get(&GetNetworkScimChangesArgs{}, self.member); err == nil {
			t.Error("a Member read the queue")
		}
		clientId := server.NewId()
		client := session.Testing_CreateClientSession(self.ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: self.memberUserId, ClientId: &clientId})
		if _, err := self.changes.get(&GetNetworkScimChangesArgs{}, client); err == nil {
			t.Error("a client token read the queue")
		}
		apiKey := session.Testing_CreateClientSession(self.ctx, self.owner.ByJwt)
		apiKey.ApiKeyAuthenticated = true
		if _, err := self.changes.get(&GetNetworkScimChangesArgs{}, apiKey); err == nil {
			t.Error("an API key read the queue")
		}
		self.changes.tierOf = func(ctx context.Context, networkId server.Id) NetworkTier { return NetworkTierFree }
		if _, err := self.changes.get(&GetNetworkScimChangesArgs{}, self.owner); err == nil {
			t.Error("a network outside the team tier read the queue")
		}
	})
}

// A one-command admin token with the all scope reads once: its replay answers the first read; a token without all is
// refused.
func TestScimChangesByAdminTokenOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newScimChangesFixture(t)
		tokenSession := func(scopes []string, jti string) *session.ClientSession {
			clientSession := session.Testing_CreateClientSession(self.ctx, self.owner.ByJwt)
			clientSession.UnsignedIdentity = true
			clientSession.AdminCommand = &session.AdminCommandUse{
				NetworkId: self.teamNetworkId, MemberId: *self.owner.ByJwt.MemberId, Jti: jti, Cmd: "scim-changes-test-cmd",
				Scopes: scopes, ExpireTime: self.now.Add(time.Hour),
			}
			return clientSession
		}
		if _, err := self.changes.get(&GetNetworkScimChangesArgs{}, tokenSession([]string{"auth_keys"}, server.NewId().String())); err == nil {
			t.Error("a token without all read the queue")
		}
		jti := server.NewId().String()
		first, err := self.changes.get(&GetNetworkScimChangesArgs{}, tokenSession([]string{"all"}, jti))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(first.Changes), 3)
		server.Tx(self.ctx, func(tx server.PgTx) {
			_, err := AddNetworkScimChangeInTx(self.ctx, tx, self.teamNetworkId, nil, NetworkScimChangeKindGroup, nil, self.now)
			server.Raise(err)
		})
		replay, err := self.changes.get(&GetNetworkScimChangesArgs{}, tokenSession([]string{"all"}, jti))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(replay.Changes), 3)
	})
}

// A change lasts ChangeRetention: past it reads leave it out, and the sweep deletes it.
func TestScimChangesExpireAtRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newScimChangesFixture(t)
		self.now = self.now.Add(DefaultNetworkScimSettings().ChangeRetention + time.Second)
		result, err := self.changes.get(&GetNetworkScimChangesArgs{}, self.owner)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(result.Changes), 0)
		removed := RemoveExpiredNetworkScimChanges(self.ctx, self.now.Add(-DefaultNetworkScimSettings().ChangeRetention))
		connect.AssertEqual(t, 3 <= removed, true)
	})
}
