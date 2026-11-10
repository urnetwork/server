package model

// §7.5's one person in several networks (R-TEAM-2, R-TEAM-3, D-TEAM-2, I-6;
// TEAM-M6-02): a credential names one network, bound when it is minted
// (server/IDP.md §2), and who the user is there depends on that network:
//
//	owner    a personal network's admin_user_id, the user that made it; its
//	         credentials carry no member_id, as every credential minted
//	         before teams does, so personal networks keep today's behavior
//	member   a team's network_member row: its credentials carry the row's
//	         member_id, which the state check reads (session's
//	         validateMemberByJwtStateQuery), never admin_user_id, which in a
//	         team is a synthetic owner of record that never signs in
//
// A JIT pending member (§7.6) and a member past its reauth_deadline sign in to
// nothing. An SSO-required team's sign-in rule is the caller's to apply, from
// SsoRequired: POST /auth/network-switch (TEAM-M6-04) refuses a session that
// did not come through the team's IdP.

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A user's way into one network.
type NetworkSignIn struct {
	NetworkId   server.Id
	NetworkName string
	UserId      server.Id
	// the membership a team credential names; nil for a personal network's
	// owner
	MemberId *server.Id
	// the member's role, mirrored from the config repo (TEAM-M6-03); a
	// personal network's owner is its Owner
	Role NetworkMemberRole
	// the team refuses every sign-in but its IdP's (§7.6)
	SsoRequired bool
}

// The user's sign-in to networkId at now, nil when it has none there: the
// owner of a personal network, or a live member of a team, not JIT pending and
// inside its reauth_deadline. A team's owner of record never signs in.
func ResolveNetworkSignIn(ctx context.Context, userId server.Id, networkId server.Id, now time.Time) (signIn *NetworkSignIn) {
	team := false
	server.Db(ctx, func(conn server.PgConn) {
		teamColumn := "false"
		ssoColumn := "false"
		if server.SidecarMg6Linked() {
			teamColumn = "network_team.network_id IS NOT NULL"
			ssoColumn = "COALESCE(network_team.sso_required, false)"
		}
		teamJoin := ""
		if server.SidecarMg6Linked() {
			teamJoin = "LEFT JOIN network_team ON network_team.network_id = network.network_id"
		}
		var adminUserId *server.Id
		var networkName string
		var ssoRequired bool
		found := false
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network.admin_user_id,
					network.network_name,
					`+teamColumn+`,
					`+ssoColumn+`
				FROM network
				`+teamJoin+`
				WHERE network.network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&adminUserId, &networkName, &team, &ssoRequired))
				found = true
			}
		})
		if !found {
			return
		}
		if !team {
			if adminUserId != nil && *adminUserId == userId {
				signIn = &NetworkSignIn{
					NetworkId:   networkId,
					NetworkName: networkName,
					UserId:      userId,
					Role:        NetworkMemberRoleOwner,
				}
			}
			return
		}
		member := getNetworkMemberOfUserWithQuery(ctx, conn, networkId, userId)
		if member == nil || member.Pending || (member.ReauthDeadline != nil && !now.Before(*member.ReauthDeadline)) {
			return
		}
		memberId := member.MemberId
		signIn = &NetworkSignIn{
			NetworkId:   networkId,
			NetworkName: networkName,
			UserId:      userId,
			MemberId:    &memberId,
			Role:        member.Role,
			SsoRequired: ssoRequired,
		}
	})
	return
}

// Every network the user may sign in to at now: its own personal network and
// each team it holds a live membership of, in network id order.
func GetNetworkSignInsOfUser(ctx context.Context, userId server.Id, now time.Time) []*NetworkSignIn {
	networkIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT network_id FROM network WHERE admin_user_id = $1`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})
		if !server.SidecarMg6Linked() {
			return
		}
		result, err = conn.Query(
			ctx,
			`SELECT network_id FROM network_member WHERE user_id = $1 ORDER BY network_id`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})
	})
	signIns := []*NetworkSignIn{}
	seen := map[server.Id]bool{}
	for _, networkId := range networkIds {
		if seen[networkId] {
			continue
		}
		seen[networkId] = true
		if signIn := ResolveNetworkSignIn(ctx, userId, networkId, now); signIn != nil {
			signIns = append(signIns, signIn)
		}
	}
	return signIns
}

// The credential a sign-in mints: a network credential for its network, naming
// its membership in a team. POST /auth/network-switch (TEAM-M6-04) signs it
// through session.MintNetworkSession, whose state check reads that membership
// again in the mint's transaction.
func NewNetworkSignInByJwt(signIn *NetworkSignIn, pro bool) *session.ByJwt {
	byJwt := session.NewByJwt(signIn.NetworkId, signIn.UserId, signIn.NetworkName, false, pro)
	if signIn.MemberId != nil {
		memberId := *signIn.MemberId
		byJwt.MemberId = &memberId
	}
	return byJwt
}

// Refused: the team signs in only through its IdP (§7.6).
var ErrNetworkSsoRequired = errors.New("This team signs in only through its IdP.")

// Mints a network credential for the user's sign-in to networkId, of kind (the
// session row's), or nil when the user has none there. viaIdp says the sign-in
// came through the network's own IdP (network_sso_controller.go); an
// SSO-required team refuses every other with ErrNetworkSsoRequired (§7.6).
func MintNetworkSignIn(ctx context.Context, userId server.Id, networkId server.Id, kind string, viaIdp bool) (string, *NetworkSignIn, error) {
	signIn := ResolveNetworkSignIn(ctx, userId, networkId, server.NowUtc())
	if signIn == nil {
		return "", nil, nil
	}
	if signIn.SsoRequired && !viaIdp {
		return "", nil, ErrNetworkSsoRequired
	}
	signed, err := session.MintNetworkSession(ctx, NewNetworkSignInByJwt(signIn, IsProFresh(ctx, &networkId)), kind)
	if err != nil {
		return "", nil, err
	}
	return signed, signIn, nil
}
