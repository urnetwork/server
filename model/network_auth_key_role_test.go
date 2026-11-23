//go:build sidecar_pending

package model

// §7.5's roles at auth key creation (authKeyReachOf; R-TEAM-2, D-TEAM-3), on Postgres with MG1 and MG6: every role
// makes a key for its own devices, untagged and not pre-approved, and only a role holding the keys permission makes
// any tagged or pre-approved one, a Network admin's keys for owned tags reaching only its own tags
// (network_auth_key_reach_test.go), each credential's scopes cutting its role.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A session of the team member's network credential, its member_id in it.
func testAuthKeyMemberSession(ctx context.Context, networkId server.Id, member *NetworkMember) *session.ClientSession {
	memberId := member.MemberId
	return session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: member.UserId, MemberId: &memberId})
}

// The network's keys whose creator is creatorUserId.
func countTestAuthKeysOf(t testing.TB, ctx context.Context, networkId server.Id, creatorUserId server.Id) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COUNT(*) FROM network_auth_key WHERE network_id = $1 AND creator_user_id = $2`,
			networkId,
			creatorUserId,
		).Scan(&count))
	})
	return
}

// Whether err is the 403 of a role refusal carrying message.
func authKeyRoleRefusal(err error, message string) bool {
	return err != nil && err.Error() == fmt.Sprintf("%d %s", http.StatusForbidden, message)
}

// Each of §7.5's seven roles makes a key for its own devices, untagged and not pre-approved, an ephemeral reusable one
// included, its creator the member's user. Only an Owner and an Admin make any tagged or pre-approved key: a Network
// admin makes a tagged one only for a tag it owns, and none here, where the network's log names no tag's owner, so it
// is refused 403 for owned tags; every other role, a Member among them, is refused 403 for its own devices alone; and a
// refusal creates nothing.
func TestEachRoleMakesAuthKeysForItsOwnDevices(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, _ := newTestSignInTeam(t, ctx, false)
		now := server.NowUtc()
		tagId := strings.Repeat("cd", 16)
		// the network's log has no base yet, so no tag has an owner
		previousTagOwnerState := authKeyTagOwnerState
		t.Cleanup(func() { authKeyTagOwnerState = previousTagOwnerState })
		authKeyTagOwnerState = func(ctx context.Context, networkId server.Id) (*sidecar.ConfigState, error) {
			return nil, nil
		}
		holdsKeys := map[NetworkMemberRole]bool{
			NetworkMemberRoleOwner:        true,
			NetworkMemberRoleAdmin:        true,
			NetworkMemberRoleNetworkAdmin: false,
			NetworkMemberRoleItAdmin:      false,
			NetworkMemberRoleBillingAdmin: false,
			NetworkMemberRoleAuditor:      false,
			NetworkMemberRoleMember:       false,
		}
		for role, keys := range holdsKeys {
			member := addTestSignInMember(t, ctx, teamId, server.NewId(), role, false)
			clientSession := testAuthKeyMemberSession(ctx, teamId, member)
			reach := authKeyReachOf(clientSession)
			if (reach == authKeyReachAny) != keys || reach == authKeyReachNone || (reach == authKeyReachOwnedTags) != (role == NetworkMemberRoleNetworkAdmin) {
				t.Errorf("a %s's reach is %d", role, reach)
			}
			refusedMessage := authKeyOwnDevicesRefusedMessage
			if role == NetworkMemberRoleNetworkAdmin {
				refusedMessage = authKeyOwnedTagsRefusedMessage
			}

			own := createTestAuthKey(t, clientSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, now)
			createTestAuthKey(t, clientSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 5, Ephemeral: true}, now)
			authKey, err := consumeTestAuthKey(ctx, own.AuthKey, now)
			if err != nil || authKey.CreatorUserId != member.UserId || len(authKey.TagIds) != 0 || authKey.PreApproved {
				t.Fatalf("a %s's own key spent as %+v (%v)", role, authKey, err)
			}

			for _, args := range []*CreateAuthKeyArgs{
				{DurationSeconds: 3600, MaxUses: 1, Tags: []string{tagId}},
				{DurationSeconds: 3600, MaxUses: 1, PreApproved: true},
				{DurationSeconds: 3600, MaxUses: 1, Tags: []string{tagId}, PreApproved: true, Ephemeral: true},
			} {
				result, err := createAuthKeyAt(args, clientSession, DefaultNetworkAuthKeySettings(), now)
				if keys {
					if err != nil || result.Error != nil || result.AuthKey == "" {
						t.Errorf("a %s's key %+v: %v %+v", role, args, err, result)
					}
					continue
				}
				if !authKeyRoleRefusal(err, refusedMessage) || result != nil {
					t.Errorf("a %s's key %+v answered %v %+v, not %q", role, args, err, result, refusedMessage)
				}
			}
			want := 2
			if keys {
				want = 5
			}
			if count := countTestAuthKeysOf(t, ctx, teamId, member.UserId); count != want {
				t.Errorf("a %s holds %d keys, want %d", role, count, want)
			}
		}
	})
}

// A credential whose member cannot be read makes no key, a pending member's and a member's of another team, and is
// refused 403 with nothing created. A credential's scopes cut its role: a Member's auth_keys admin token, its API keys
// (all included, which reaches no more than the role) and its OAuth client token make a key for its own devices alone;
// an Admin's auth_keys or legacy API key any key, and its devices:core API key none. A personal network's owner is asked
// only its scopes.
func TestAuthKeyReachOfEachCredential(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, _ := newTestSignInTeam(t, ctx, false)
		otherTeamId, _ := newTestSignInTeam(t, ctx, false)
		member := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleMember, false)
		admin := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleAdmin, false)
		pending := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleAdmin, true)
		elsewhere := addTestSignInMember(t, ctx, otherTeamId, server.NewId(), NetworkMemberRoleOwner, false)
		personalUserId, personalNetworkId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, personalNetworkId, "personal-"+personalNetworkId.String(), personalUserId)

		withApiKey := func(clientSession *session.ClientSession, scopes []string) *session.ClientSession {
			apiKeyId := server.NewId()
			clientSession.ApiKeyAuthenticated = true
			clientSession.ApiKeyId = &apiKeyId
			clientSession.ApiKeyScopes = scopes
			return clientSession
		}
		withAdminCommand := func(clientSession *session.ClientSession, scopes []string) *session.ClientSession {
			clientSession.UnsignedIdentity = true
			clientSession.AdminCommand = &session.AdminCommandUse{
				NetworkId:  clientSession.ByJwt.NetworkId,
				MemberId:   *clientSession.ByJwt.MemberId,
				Jti:        server.NewId().String(),
				Cmd:        "synthetic-cmd-digest",
				Scopes:     scopes,
				ExpireTime: server.NowUtc().Add(time.Hour),
			}
			return clientSession
		}
		withOauthClient := func(clientSession *session.ClientSession, scopes []string) *session.ClientSession {
			clientSession.UnsignedIdentity = true
			clientSession.OauthClient = &session.OauthClientUse{
				NetworkId:     clientSession.ByJwt.NetworkId,
				MemberId:      *clientSession.ByJwt.MemberId,
				OauthClientId: server.NewId(),
				Scopes:        scopes,
				ExpireTime:    server.NowUtc().Add(time.Hour),
			}
			return clientSession
		}
		personal := func() *session.ClientSession {
			return session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: personalNetworkId, UserId: personalUserId})
		}
		// a credential naming the team and a membership the team does not hold
		elsewhereSession := testAuthKeyMemberSession(ctx, teamId, elsewhere)

		cases := []struct {
			name          string
			clientSession *session.ClientSession
			reach         authKeyReach
		}{
			{"a Member's token", testAuthKeyMemberSession(ctx, teamId, member), authKeyReachOwnDevices},
			{"a Member's auth_keys admin token", withAdminCommand(testAuthKeyMemberSession(ctx, teamId, member), []string{ApiKeyScopeAuthKeys}), authKeyReachOwnDevices},
			{"a Member's auth_keys API key", withApiKey(testAuthKeyMemberSession(ctx, teamId, member), []string{ApiKeyScopeAuthKeys}), authKeyReachOwnDevices},
			{"a Member's all API key", withApiKey(testAuthKeyMemberSession(ctx, teamId, member), []string{ApiKeyScopeAll}), authKeyReachOwnDevices},
			{"a Member's auth_keys OAuth client token", withOauthClient(testAuthKeyMemberSession(ctx, teamId, member), []string{ApiKeyScopeAuthKeys}), authKeyReachOwnDevices},
			{"a Member's devices:core API key", withApiKey(testAuthKeyMemberSession(ctx, teamId, member), []string{ApiKeyScopeDevicesCore}), authKeyReachNone},
			{"an Admin's auth_keys API key", withApiKey(testAuthKeyMemberSession(ctx, teamId, admin), []string{ApiKeyScopeAuthKeys}), authKeyReachAny},
			{"an Admin's legacy API key", withApiKey(testAuthKeyMemberSession(ctx, teamId, admin), nil), authKeyReachAny},
			{"an Admin's devices:core API key", withApiKey(testAuthKeyMemberSession(ctx, teamId, admin), []string{ApiKeyScopeDevicesCore}), authKeyReachNone},
			{"an Admin's auth_keys admin token", withAdminCommand(testAuthKeyMemberSession(ctx, teamId, admin), []string{ApiKeyScopeAuthKeys}), authKeyReachAny},
			{"an Admin's admin token naming no scope", withAdminCommand(testAuthKeyMemberSession(ctx, teamId, admin), nil), authKeyReachNone},
			{"a pending member's token", testAuthKeyMemberSession(ctx, teamId, pending), authKeyReachNone},
			{"another team's member", elsewhereSession, authKeyReachNone},
			{"a personal owner's token", personal(), authKeyReachAny},
			{"a personal owner's auth_keys API key", withApiKey(personal(), []string{ApiKeyScopeAuthKeys}), authKeyReachAny},
			{"a personal owner's policy:read API key", withApiKey(personal(), []string{ApiKeyScopePolicyRead}), authKeyReachNone},
		}
		for _, c := range cases {
			if reach := authKeyReachOf(c.clientSession); reach != c.reach {
				t.Errorf("%s reaches %d, want %d", c.name, reach, c.reach)
			}
		}

		// a credential that reaches no key is refused before anything runs
		now := server.NowUtc()
		for _, clientSession := range []*session.ClientSession{
			testAuthKeyMemberSession(ctx, teamId, pending),
			elsewhereSession,
			withApiKey(testAuthKeyMemberSession(ctx, teamId, admin), []string{ApiKeyScopeDevicesCore}),
		} {
			result, err := createAuthKeyAt(&CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, clientSession, DefaultNetworkAuthKeySettings(), now)
			if !authKeyRoleRefusal(err, authKeyRoleRefusedMessage) || result != nil {
				t.Errorf("a credential reaching no key answered %v %+v", err, result)
			}
		}
		for _, userId := range []server.Id{pending.UserId, elsewhere.UserId, admin.UserId} {
			if count := countTestAuthKeysOf(t, ctx, teamId, userId); count != 0 {
				t.Errorf("a refused credential's user %s holds %d keys", userId, count)
			}
		}
	})
}
