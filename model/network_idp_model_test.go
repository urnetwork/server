//go:build sidecar_pending

package model

// §7.6's team IdPs (TEAM-M6-05), on Postgres with MG6.

import (
	"bytes"
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// An issuer is an https URL whose host is public, with no user, query or
// fragment; an address is public unless loopback, private, link-local, shared
// or unspecified.
func TestValidNetworkIdpIssuer(t *testing.T) {
	connect.AssertEqual(t, ValidNetworkIdpIssuer("https://idp.example"), nil)
	connect.AssertEqual(t, ValidNetworkIdpIssuer("https://idp.example/tenant/v2"), nil)
	for _, refused := range []string{
		"",
		"http://idp.example",
		"https://idp.example?x=1",
		"https://idp.example#frag",
		"https://user@idp.example",
		"https://127.0.0.1",
		"https://10.0.0.8",
		"https://[::1]",
		"https://169.254.169.254",
		"https://100.64.0.1",
		"ftp://idp.example",
	} {
		if ValidNetworkIdpIssuer(refused) == nil {
			t.Errorf("issuer %q passed", refused)
		}
	}
	connect.AssertEqual(t, NetworkIdpPublicAddress(netip.MustParseAddr("203.0.113.9")), true)
	connect.AssertEqual(t, NetworkIdpPublicAddress(netip.MustParseAddr("2001:db8::9")), true)
	for _, address := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.1.1", "::1", "fe80::1", "0.0.0.0", "::ffff:10.0.0.1"} {
		if NetworkIdpPublicAddress(netip.MustParseAddr(address)) {
			t.Errorf("address %s is public", address)
		}
	}
}

// The Owner's session of a team, with the tier check passed.
func newTestIdpTeam(t testing.TB, ctx context.Context) (networkId server.Id, ownerSession *session.ClientSession) {
	t.Helper()
	networkId, _ = newTestSignInTeam(t, ctx, false)
	ownerUserId := server.NewId()
	Testing_CreateNetwork(ctx, server.NewId(), "owner", ownerUserId)
	member := addTestSignInMember(t, ctx, networkId, ownerUserId, NetworkMemberRoleOwner, false)
	byJwt := session.NewByJwt(networkId, ownerUserId, "team", false, false)
	byJwt.MemberId = &member.MemberId
	ownerSession = session.NewLocalClientSession(ctx, "192.0.2.10:1", byJwt)
	t.Cleanup(ownerSession.Cancel)
	t.Cleanup(Testing_SetNetworkIdpRequireTeamTier(func(ctx context.Context, networkId server.Id) error { return nil }))
	return
}

// A client secret is sealed under the vault key, bound to its idp_id, and
// opens again; with no vault key, or for private_key_jwt, whose assertion key
// is not built, the IdP is refused; a personal network has none.
func TestCreateNetworkIdpSealsTheSecret(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, ownerSession := newTestIdpTeam(t, ctx)
		key := bytes.Repeat([]byte{7}, 32)
		restore := Testing_SetNetworkIdpSecretKey(key)
		defer restore()

		secret := "synthetic-idp-secret-4f1e"
		result, err := CreateNetworkIdp(&CreateNetworkIdpArgs{
			Issuer: "https://idp.example", ClientId: "synthetic-client", ClientAuth: NetworkIdpClientAuthClientSecret, ClientSecret: secret,
		}, ownerSession)
		if err != nil || result.Error != nil || result.IdpId == nil {
			t.Fatalf("create: %v %+v", err, result)
		}
		idp := GetNetworkIdp(ctx, *result.IdpId)
		if idp == nil || idp.NetworkId != networkId {
			t.Fatal("no IdP row")
		}
		if bytes.Contains(idp.clientSecretCiphertext, []byte(secret)) {
			t.Fatal("the secret is stored in the clear")
		}
		opened, err := idp.ClientSecret()
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, opened, secret)
		// the sealed secret opens for its own row only
		moved := *idp
		moved.IdpId = server.NewId()
		if _, err := moved.ClientSecret(); err == nil {
			t.Fatal("a sealed secret opened for another IdP")
		}

		Testing_SetNetworkIdpSecretKey(nil)
		for _, args := range []*CreateNetworkIdpArgs{
			{Issuer: "https://idp.example", ClientId: "c", ClientAuth: NetworkIdpClientAuthClientSecret, ClientSecret: secret},
			{Issuer: "https://idp.example", ClientId: "c", ClientAuth: NetworkIdpClientAuthPrivateKeyJwt},
			{Issuer: "http://idp.example", ClientId: "c", ClientAuth: NetworkIdpClientAuthPrivateKeyJwt},
			{Issuer: "https://idp.example", ClientId: "c", ClientAuth: "client_secret_basic"},
		} {
			result, err := CreateNetworkIdp(args, ownerSession)
			if err != nil || result.Error == nil {
				t.Errorf("an IdP that cannot be honored was added: %v %+v", err, result)
			}
		}

		personalUserId, personalNetworkId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, personalNetworkId, "personal", personalUserId)
		personalSession := session.NewLocalClientSession(ctx, "192.0.2.11:1", session.NewByJwt(personalNetworkId, personalUserId, "personal", false, false))
		defer personalSession.Cancel()
		Testing_SetNetworkIdpSecretKey(key)
		result, err = CreateNetworkIdp(&CreateNetworkIdpArgs{
			Issuer: "https://idp.example", ClientId: "c", ClientAuth: NetworkIdpClientAuthClientSecret, ClientSecret: secret,
		}, personalSession)
		if err != nil || result.Error == nil {
			t.Fatalf("a personal network took an IdP: %v %+v", err, result)
		}
	})
}

// An identity links once: again to the same user changes nothing, to another
// user is refused.
func TestLinkIdpIdentityOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		idpId, userId, otherUserId := server.NewId(), server.NewId(), server.NewId()
		connect.AssertEqual(t, LinkIdpIdentity(ctx, idpId, "sub-1", userId), nil)
		connect.AssertEqual(t, LinkIdpIdentity(ctx, idpId, "sub-1", userId), nil)
		connect.AssertEqual(t, LinkIdpIdentity(ctx, idpId, "sub-1", otherUserId), ErrIdpIdentityLinkedElsewhere)
		// another IdP's same sub is another identity
		connect.AssertEqual(t, LinkIdpIdentity(ctx, server.NewId(), "sub-1", otherUserId), nil)
	})
}

// §22.7's JIT: an identity with no linked user, whatever email it asserts,
// links to no one and makes no member; once the user's own sign-in began the
// flow it links, and the first sign-in makes a pending member whose
// reauth_deadline is a window on; a sub of another IdP is another identity.
func TestSsoLoginLinksOnlyAfterTheUsersOwnSignIn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, _ := newTestSignInTeam(t, ctx, false)
		idp := &NetworkIdp{IdpId: server.NewId(), NetworkId: teamId, Issuer: "https://idp.example", ClientId: "c", ClientAuth: NetworkIdpClientAuthClientSecret}
		passwordUserId := server.NewId()
		Testing_CreateNetwork(ctx, server.NewId(), "password-account", passwordUserId)
		identity := &NetworkOidcIdentity{Sub: "employee-7", Email: "employee@team.example", EmailVerified: true}
		settings := DefaultNetworkIdpSettings()
		now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
		resolve := func(idp *NetworkIdp, linkingUserId *server.Id) (login *NetworkSsoLogin, err error) {
			server.Tx(ctx, func(tx server.PgTx) {
				login, err = ResolveNetworkSsoLoginInTx(ctx, tx, idp, identity, linkingUserId, now, settings)
			})
			return
		}

		if _, err := resolve(idp, nil); err != ErrNetworkSsoLinkRequired {
			t.Fatalf("an unlinked identity signed in: %v", err)
		}
		if GetNetworkMemberOfUser(ctx, teamId, passwordUserId) != nil {
			t.Fatal("an email made a member")
		}

		login, err := resolve(idp, &passwordUserId)
		if err != nil || !login.Linked || !login.Pending || login.UserId != passwordUserId || login.NetworkId != teamId {
			t.Fatalf("the user's own sign-in: %v %+v", err, login)
		}
		member := GetNetworkMemberOfUser(ctx, teamId, passwordUserId)
		if member == nil || !member.Pending || member.ReauthDeadline == nil || !member.ReauthDeadline.Equal(now.Add(settings.ReauthWindow)) {
			t.Fatalf("the JIT member: %+v", member)
		}
		// a pending member signs in to nothing
		if ResolveNetworkSignIn(ctx, passwordUserId, teamId, now) != nil {
			t.Fatal("a pending member signed in")
		}

		// the identity is linked now: no linking user needed
		login, err = resolve(idp, nil)
		if err != nil || login.Linked || login.UserId != passwordUserId {
			t.Fatalf("a linked identity: %v %+v", err, login)
		}

		// another IdP's same sub links to no one
		otherIdp := &NetworkIdp{IdpId: server.NewId(), NetworkId: teamId, Issuer: "https://other.example", ClientId: "c", ClientAuth: NetworkIdpClientAuthClientSecret}
		if _, err := resolve(otherIdp, nil); err != ErrNetworkSsoLinkRequired {
			t.Fatalf("another IdP's sub reached the user: %v", err)
		}
	})
}

// reauth_deadline bounds access: a member signs in before it and not at it,
// on a fixed clock.
func TestReauthDeadlineBoundsAccess(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, _ := newTestSignInTeam(t, ctx, false)
		userId := server.NewId()
		Testing_CreateNetwork(ctx, server.NewId(), "member", userId)
		member := addTestSignInMember(t, ctx, teamId, userId, NetworkMemberRoleMember, false)
		deadline := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_member SET reauth_deadline = $2 WHERE member_id = $1`, member.MemberId, deadline))
		})
		if ResolveNetworkSignIn(ctx, userId, teamId, deadline.Add(-time.Second)) == nil {
			t.Fatal("a member was refused before its deadline")
		}
		if ResolveNetworkSignIn(ctx, userId, teamId, deadline) != nil {
			t.Fatal("a member signed in at its deadline")
		}
	})
}

// An SSO-required team refuses every sign-in but its IdP's.
func TestSsoRequiredTeamRefusesOtherSignIns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, _ := newTestSignInTeam(t, ctx, true)
		userId := server.NewId()
		Testing_CreateNetwork(ctx, server.NewId(), "member", userId)
		addTestSignInMember(t, ctx, teamId, userId, NetworkMemberRoleMember, false)
		if _, _, err := MintNetworkSignIn(ctx, userId, teamId, "network_switch", false); err != ErrNetworkSsoRequired {
			t.Fatalf("a password session switched into an SSO-required team: %v", err)
		}
		signed, signIn, err := MintNetworkSignIn(ctx, userId, teamId, "sso", true)
		if err != nil || signIn == nil || signed == "" {
			t.Fatalf("the IdP's sign-in: %v", err)
		}
	})
}
