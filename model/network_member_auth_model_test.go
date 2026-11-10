//go:build sidecar_pending

package model

// Per-network sign-in and the network switch's groundwork (TEAM-M6-02), on
// Postgres with MG6.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/apikey"
	"github.com/urnetwork/server/session"
)

// A team as team-create leaves one, written directly: the network, its
// synthetic owner of record and its network_team row.
func newTestSignInTeam(t testing.TB, ctx context.Context, ssoRequired bool) (networkId server.Id, ownerOfRecordId server.Id) {
	t.Helper()
	networkId, ownerOfRecordId = server.NewId(), server.NewId()
	Testing_CreateNetwork(ctx, networkId, "team-"+networkId.String(), ownerOfRecordId)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team(network_id, sso_required) VALUES($1, $2)`, networkId, ssoRequired))
	})
	return
}

// Adds userId to the team.
func addTestSignInMember(t testing.TB, ctx context.Context, networkId server.Id, userId server.Id, role NetworkMemberRole, pending bool) *NetworkMember {
	t.Helper()
	var member *NetworkMember
	server.Tx(ctx, func(tx server.PgTx) {
		var err error
		member, err = AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{NetworkId: networkId, UserId: userId, Role: role, Pending: pending})
		server.Raise(err)
	})
	return member
}

// A personal network's owner signs in as its owner, with no member_id; a team
// member signs in to the team with its membership; the team's owner of record
// and a pending member sign in to nothing; and a user's sign-ins list both.
func TestResolveNetworkSignIn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		userId, personalNetworkId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, personalNetworkId, "personal-"+personalNetworkId.String(), userId)
		teamId, ownerOfRecordId := newTestSignInTeam(t, ctx, true)
		member := addTestSignInMember(t, ctx, teamId, userId, NetworkMemberRoleAdmin, false)

		personal := ResolveNetworkSignIn(ctx, userId, personalNetworkId, now)
		if personal == nil || personal.MemberId != nil || personal.Role != NetworkMemberRoleOwner {
			t.Fatalf("the personal sign-in: %+v", personal)
		}
		team := ResolveNetworkSignIn(ctx, userId, teamId, now)
		if team == nil || team.MemberId == nil || *team.MemberId != member.MemberId || team.Role != NetworkMemberRoleAdmin || !team.SsoRequired {
			t.Fatalf("the team sign-in: %+v", team)
		}
		if ResolveNetworkSignIn(ctx, ownerOfRecordId, teamId, now) != nil {
			t.Fatal("the team's owner of record signed in")
		}
		if ResolveNetworkSignIn(ctx, userId, server.NewId(), now) != nil {
			t.Fatal("a sign-in to a network that does not exist")
		}
		pendingUserId := server.NewId()
		addTestSignInMember(t, ctx, teamId, pendingUserId, NetworkMemberRoleMember, true)
		if ResolveNetworkSignIn(ctx, pendingUserId, teamId, now) != nil {
			t.Fatal("a pending member signed in")
		}

		signIns := GetNetworkSignInsOfUser(ctx, userId, now)
		connect.AssertEqual(t, len(signIns), 2)
		networkIds := map[server.Id]bool{}
		for _, signIn := range signIns {
			networkIds[signIn.NetworkId] = true
		}
		connect.AssertEqual(t, networkIds[personalNetworkId] && networkIds[teamId], true)
	})
}

// The switch's groundwork mints a team credential naming the membership,
// which validates in the team; a user with no sign-in there gets none.
func TestMintNetworkSignInNamesTheMembership(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		userId, personalNetworkId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, personalNetworkId, "personal-"+personalNetworkId.String(), userId)
		teamId, _ := newTestSignInTeam(t, ctx, false)
		member := addTestSignInMember(t, ctx, teamId, userId, NetworkMemberRoleMember, false)

		signed, signIn, err := MintNetworkSignIn(ctx, userId, teamId, "network_switch", false)
		if err != nil || signIn == nil || signed == "" {
			t.Fatalf("mint: %v %+v", err, signIn)
		}
		byJwt, err := session.ParseByJwt(ctx, signed)
		if err != nil {
			t.Fatal(err)
		}
		if byJwt.NetworkId != teamId || byJwt.MemberId == nil || *byJwt.MemberId != member.MemberId || byJwt.UserId != userId {
			t.Fatalf("the credential names %s %v %s", byJwt.NetworkId, byJwt.MemberId, byJwt.UserId)
		}
		connect.AssertEqual(t, session.ValidateByJwtState(ctx, byJwt, false), nil)

		strangerId := server.NewId()
		Testing_CreateNetwork(ctx, server.NewId(), "stranger", strangerId)
		signed, signIn, err = MintNetworkSignIn(ctx, strangerId, teamId, "network_switch", false)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, signIn == nil && signed == "", true)
	})
}

// A team's API key acts as its creator's membership, never as the owner of
// record; a key whose creator holds no live membership is no key; a personal
// network's key still acts as its owner.
func TestTeamApiKeyActsAsItsCreator(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, ownerOfRecordId := newTestSignInTeam(t, ctx, false)
		userId := server.NewId()
		Testing_CreateNetwork(ctx, server.NewId(), "creator", userId)
		member := addTestSignInMember(t, ctx, teamId, userId, NetworkMemberRoleAdmin, false)
		insertKey := func(networkId server.Id, creatorUserId *server.Id) string {
			key := "urn_synthetic" + server.NewId().String()
			hash := sha256.Sum256([]byte(key))
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO account_api_key (api_key_id, network_id, api_key, name, creator_user_id) VALUES ($1, $2, $3, 'synthetic', $4)`,
					server.NewId(), networkId, hex.EncodeToString(hash[:]), creatorUserId,
				))
			})
			return key
		}

		byKey := apikey.GetNetworkByApiKey(insertKey(teamId, &userId), ctx)
		if byKey == nil || byKey.UserId != userId || byKey.MemberId == nil || *byKey.MemberId != member.MemberId {
			t.Fatalf("a member's key: %+v", byKey)
		}
		if byKey.UserId == ownerOfRecordId {
			t.Fatal("a team key acts as the owner of record")
		}
		if apikey.GetNetworkByApiKey(insertKey(teamId, nil), ctx) != nil {
			t.Fatal("a team key with no creator stands for someone")
		}
		strangerId := server.NewId()
		if apikey.GetNetworkByApiKey(insertKey(teamId, &strangerId), ctx) != nil {
			t.Fatal("a team key whose creator holds no membership stands for someone")
		}

		personalOwnerId, personalNetworkId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, personalNetworkId, "personal-key", personalOwnerId)
		personal := apikey.GetNetworkByApiKey(insertKey(personalNetworkId, nil), ctx)
		if personal == nil || personal.UserId != personalOwnerId || personal.MemberId != nil {
			t.Fatalf("a personal key: %+v", personal)
		}
	})
}
