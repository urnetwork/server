//go:build sidecar_pending

package model

// TEAM's membership writers keep the Redis revocation record's memberships
// (owner decision 561, QD-1013), on Postgres with MG6: every end of a
// membership tombstones m:<member_id> in every record of its network before it
// commits, and a change whose record write fails does not commit; an IdP
// sign-in's reauth deadline move drops the membership's projection first; and a
// password reset raises the member user's cutoff u:<user_id> in every record of
// its teams.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A team and a joined, admitted member with a personal network of its own.
func testingRecordTeamMember(t testing.TB, ctx context.Context) (teamId server.Id, userId server.Id, userAuth string, member *NetworkMember) {
	t.Helper()
	teamId, _ = newTestSignInTeam(t, ctx, false)
	userId = server.NewId()
	userAuth = Testing_CreateNetwork(ctx, server.NewId(), fmt.Sprintf("member-%s", userId), userId)
	member = addTestSignInMember(t, ctx, teamId, userId, NetworkMemberRoleMember, false)
	return
}

// Writes the membership's projected m: value into every record of the network,
// as checks through each shard's lineages would project it.
func testingProjectMembership(t testing.TB, ctx context.Context, networkId server.Id, member *NetworkMember) {
	t.Helper()
	value := session.Testing_RevocationMembershipValue(member.UserId, member.ReauthDeadline)
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		for _, key := range session.Testing_RevocationRecordShardKeys(networkId) {
			if err := r.HSet(ctx, key, session.Testing_RevocationMemberField(member.MemberId), value).Err(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Fails the test unless no record of the network holds field.
func assertNetworkRevocationFieldAbsent(t testing.TB, ctx context.Context, networkId server.Id, field string) {
	t.Helper()
	for _, key := range session.Testing_RevocationRecordShardKeys(networkId) {
		value, ok, err := session.Testing_RevocationRecordField(ctx, key, field)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("record %s still holds %s = %q", key, field, value)
		}
	}
}

func isRevocationTombstone(value string) bool { return value == session.Testing_RevocationTombstone }

// A denial, by POST /network/member-remove or SCIM, tombstones the membership
// in every record of the team before it commits; one whose record write fails
// leaves the membership as it was.
func TestMemberDenialTombstonesTheMembershipBeforeCommit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, _, _, member := testingRecordTeamMember(t, ctx)
		deny := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				_, err := DenyNetworkMemberInTx(ctx, tx, &NetworkMemberDenial{NetworkId: teamId, MemberId: member.MemberId, Time: server.NowUtc()})
				server.Raise(err)
			})
		}

		restore := session.Testing_FailRevocationRecordWrites(errTestingRevocationStoreDown)
		raised := testingRaised(deny)
		restore()
		if !errors.Is(raised, errTestingRevocationStoreDown) {
			t.Fatalf("a denial without its record writes was not refused: %v", raised)
		}
		if GetNetworkMember(ctx, member.MemberId) == nil {
			t.Fatal("a denial committed without its record writes")
		}

		deny()
		if GetNetworkMember(ctx, member.MemberId) != nil {
			t.Fatal("the denial left the membership")
		}
		assertNetworkRevocationField(t, ctx, teamId, session.Testing_RevocationMemberField(member.MemberId), isRevocationTombstone)
	})
}

// A MemberRevoke's landing tombstones the membership the team still holds
// before it deletes the row, and fails without its record writes; a member the
// server holds no row for, a personal network's owner, writes nothing.
func TestConfigMemberRemovalTombstonesTheMembershipBeforeCommit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, _, _, member := testingRecordTeamMember(t, ctx)
		remove := func(networkId server.Id, memberId server.Id) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(removeConfigMemberInTx(ctx, tx, networkId, memberId, server.NowUtc()))
			})
		}

		restore := session.Testing_FailRevocationRecordWrites(errTestingRevocationStoreDown)
		raised := testingRaised(func() { remove(teamId, member.MemberId) })
		restore()
		if !errors.Is(raised, errTestingRevocationStoreDown) || GetNetworkMember(ctx, member.MemberId) == nil {
			t.Fatalf("a log removal without its record writes committed or was not refused: %v", raised)
		}

		remove(teamId, member.MemberId)
		if GetNetworkMember(ctx, member.MemberId) != nil {
			t.Fatal("the log removal left the membership")
		}
		assertNetworkRevocationField(t, ctx, teamId, session.Testing_RevocationMemberField(member.MemberId), isRevocationTombstone)

		// a personal network's member_id in the log is its owner's user id
		personalId, ownerId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, personalId, fmt.Sprintf("personal-%s", personalId), ownerId)
		remove(personalId, ownerId)
		assertNetworkRevocationFieldAbsent(t, ctx, personalId, session.Testing_RevocationMemberField(ownerId))
	})
}

// An account's deletion ends its user's memberships of other teams, each
// tombstoned in every record of its team before the deletion commits.
func TestAccountDeletionTombstonesTheUsersMemberships(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, userId, _, member := testingRecordTeamMember(t, ctx)
		otherTeamId, _ := newTestSignInTeam(t, ctx, false)
		otherMember := addTestSignInMember(t, ctx, otherTeamId, userId, NetworkMemberRoleMember, true)
		personalId := testNetworkOfAdmin(t, ctx, userId)

		if outcome := RemoveNetworkWithStoreSnapshot(ctx, personalId, &userId, nil).Outcome; outcome != RemoveNetworkRemoved {
			t.Fatal("account removal failed", outcome)
		}
		if GetNetworkMember(ctx, member.MemberId) != nil || GetNetworkMember(ctx, otherMember.MemberId) != nil {
			t.Fatal("the deletion left a membership")
		}
		assertNetworkRevocationField(t, ctx, teamId, session.Testing_RevocationMemberField(member.MemberId), isRevocationTombstone)
		assertNetworkRevocationField(t, ctx, otherTeamId, session.Testing_RevocationMemberField(otherMember.MemberId), isRevocationTombstone)
	})
}

// The network the user administers, its personal network.
func testNetworkOfAdmin(t testing.TB, ctx context.Context, userId server.Id) (networkId server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT network_id FROM network WHERE admin_user_id = $1`, userId).Scan(&networkId))
	})
	return
}

// An IdP sign-in moves a member's reauth deadline only after dropping the
// membership's projection from every record of the team, so the next check
// reads SQL; one whose record write fails moves nothing.
func TestSsoSignInDropsTheMembershipProjectionBeforeCommit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, userId, _, member := testingRecordTeamMember(t, ctx)
		idp := &NetworkIdp{IdpId: server.NewId(), NetworkId: teamId, Issuer: "https://idp.example", ClientId: "c", ClientAuth: NetworkIdpClientAuthClientSecret}
		identity := &NetworkOidcIdentity{Sub: "member-1", Email: "member@team.example", EmailVerified: true}
		settings := DefaultNetworkIdpSettings()
		// whole seconds, which the deadline's column keeps exactly
		now := server.NowUtc().Truncate(time.Second)
		resolve := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				_, err := ResolveNetworkSsoLoginInTx(ctx, tx, idp, identity, &userId, now, settings)
				server.Raise(err)
			})
		}
		testingProjectMembership(t, ctx, teamId, member)

		restore := session.Testing_FailRevocationRecordWrites(errTestingRevocationStoreDown)
		raised := testingRaised(resolve)
		restore()
		if !errors.Is(raised, errTestingRevocationStoreDown) {
			t.Fatalf("a sign-in without its record writes was not refused: %v", raised)
		}
		if moved := GetNetworkMember(ctx, member.MemberId); moved == nil || moved.ReauthDeadline != nil {
			t.Fatalf("a sign-in moved the deadline without its record writes: %+v", moved)
		}

		resolve()
		moved := GetNetworkMember(ctx, member.MemberId)
		if moved == nil || moved.ReauthDeadline == nil || !moved.ReauthDeadline.Equal(now.Add(settings.ReauthWindow)) {
			t.Fatalf("the sign-in's deadline: %+v", moved)
		}
		assertNetworkRevocationFieldAbsent(t, ctx, teamId, session.Testing_RevocationMemberField(member.MemberId))
	})
}

// A password reset raises the member user's cutoff in every record of its
// teams, beside its own network's cutoff, before the rotation commits.
func TestPasswordResetRaisesTheMemberUsersCutoff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, userId, userAuth, _ := testingRecordTeamMember(t, ctx)
		personalId := testNetworkOfAdmin(t, ctx, userId)
		actor := session.Testing_CreateClientSession(ctx, session.NewByJwt(personalId, userId, "member-reset", false, false))
		defer actor.Cancel()

		reset, err := AuthPasswordResetCreateCode(AuthPasswordResetCreateCodeArgs{UserAuth: userAuth}, actor)
		if err != nil || reset.ResetCode == nil {
			t.Fatal(reset, err)
		}
		result, err := AuthPasswordSet(AuthPasswordSetArgs{ResetCode: *reset.ResetCode, Password: "rotated-password"}, actor)
		if err != nil || result == nil || result.Error != nil {
			t.Fatal("rotation failed", result, err)
		}
		var cutoff time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT credential_change_time FROM network_user WHERE user_id = $1`, userId).Scan(&cutoff))
		})
		atLeastCutoff := func(value string) bool {
			micros, err := strconv.ParseInt(value, 10, 64)
			return err == nil && cutoff.UnixMicro() <= micros
		}
		assertNetworkRevocationField(t, ctx, teamId, session.Testing_RevocationUserField(userId), atLeastCutoff)
		assertNetworkRevocationField(t, ctx, personalId, session.Testing_RevocationCredentialField, atLeastCutoff)
		// the team's own cutoff is its owner of record's, which the reset leaves
		assertNetworkRevocationFieldAbsent(t, ctx, teamId, session.Testing_RevocationCredentialField)
	})
}
