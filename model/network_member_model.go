package model

// §7.5's members (R-TEAM-2, Q-TEAM-7; §2.8 api-3): the people of a team network,
// one network_member row each, holding only what sign-in and roles need: the
// member's user, an email and a role, with §7.6's pending JIT state and
// reauthentication deadline. Groups, tags, device names and admissions are never
// here; they live in the config repo (owner decision 46).
//
// member_id is new per join. It rides in tokens and rev/ objects in place of the
// user id a personal network uses, so a removal and a re-invite never revive a
// token minted for the earlier membership: the earlier member_id names no row.
// A user holds at most one live membership in a network, and a removal deletes
// the row.
//
// Roles are §7.5's seven. They are signed in the config repo (AdminSet for the
// first four, MemberAdmit for the rest) and the server mirrors them from verified
// log entries, never from a request (TEAM-M6-03). The one role set from a request
// is the first Owner, which POST /network/team-create writes before the team's
// config repo exists (network_team_model.go).
//
// Nothing here reads or writes network_member unless the binary links MG6
// (server.SidecarMg6Linked).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A member's role (§7.5), as network_member.role stores it: the lowercase name of
// its bridge.MemberRole without the MEMBER_ROLE_ prefix.
type NetworkMemberRole string

const (
	// all, including deletion, the IdP and the admin set
	NetworkMemberRoleOwner NetworkMemberRole = "owner"
	// members, roles, admissions; policy, tags, groups, DNS, edge, versioning
	// rules; keys, OAuth clients; read
	NetworkMemberRoleAdmin NetworkMemberRole = "admin"
	// policy, tags, groups, DNS, edge, versioning rules; keys for owned tags; read
	NetworkMemberRoleNetworkAdmin NetworkMemberRole = "network_admin"
	// admissions; untagged keys; read
	NetworkMemberRoleItAdmin NetworkMemberRole = "it_admin"
	// billing, payouts; read
	NetworkMemberRoleBillingAdmin NetworkMemberRole = "billing_admin"
	// read: settings, clients, audit
	NetworkMemberRoleAuditor NetworkMemberRole = "auditor"
	// own devices
	NetworkMemberRoleMember NetworkMemberRole = "member"
)

// Each role's bridge.MemberRole, the config repo's form.
var networkMemberRoleBridgeRoles = map[NetworkMemberRole]bridge.MemberRole{
	NetworkMemberRoleOwner:        bridge.MemberRole_MEMBER_ROLE_OWNER,
	NetworkMemberRoleAdmin:        bridge.MemberRole_MEMBER_ROLE_ADMIN,
	NetworkMemberRoleNetworkAdmin: bridge.MemberRole_MEMBER_ROLE_NETWORK_ADMIN,
	NetworkMemberRoleItAdmin:      bridge.MemberRole_MEMBER_ROLE_IT_ADMIN,
	NetworkMemberRoleBillingAdmin: bridge.MemberRole_MEMBER_ROLE_BILLING_ADMIN,
	NetworkMemberRoleAuditor:      bridge.MemberRole_MEMBER_ROLE_AUDITOR,
	NetworkMemberRoleMember:       bridge.MemberRole_MEMBER_ROLE_MEMBER,
}

// Whether the role is one of the seven.
func (self NetworkMemberRole) Valid() bool {
	_, ok := networkMemberRoleBridgeRoles[self]
	return ok
}

// The config repo's form of the role, MEMBER_ROLE_UNSPECIFIED for no valid role.
func (self NetworkMemberRole) BridgeRole() bridge.MemberRole {
	return networkMemberRoleBridgeRoles[self]
}

// The role of a bridge.MemberRole, false for MEMBER_ROLE_UNSPECIFIED or a value
// this build does not know.
func NetworkMemberRoleOfBridge(role bridge.MemberRole) (NetworkMemberRole, bool) {
	for memberRole, bridgeRole := range networkMemberRoleBridgeRoles {
		if bridgeRole == role {
			return memberRole, true
		}
	}
	return "", false
}

// Whether the role signs config records under AdminSet (§7.5's first four).
func (self NetworkMemberRole) AdminSet() bool {
	switch self {
	case NetworkMemberRoleOwner, NetworkMemberRoleAdmin, NetworkMemberRoleNetworkAdmin, NetworkMemberRoleItAdmin:
		return true
	default:
		return false
	}
}

// The width of network_member.email and network_invite.email, user_auth's.
const networkMemberEmailMaxLength = 256

// The answer of a member route while the binary does not link MG6.
const networkTeamsUnavailableMessage = "Teams are not available."

// A member as network_member holds it.
type NetworkMember struct {
	MemberId  server.Id
	NetworkId server.Id
	UserId    server.Id
	// the member's sign-in email, nil when the user has none
	Email *string
	Role  NetworkMemberRole
	// §7.6's JIT state: an IdP login made the row and no admin device has
	// admitted the member yet, so it reaches nothing
	Pending bool
	// §7.6: without SCIM, the member's access ends here unless the IdP signs
	// it in again; nil when no IdP bounds it
	ReauthDeadline *time.Time
}

// Refused: the user already holds a membership in the network.
var ErrNetworkMemberExists = errors.New("The user is already a member of the network.")

type AddNetworkMemberArgs struct {
	NetworkId server.Id
	UserId    server.Id
	Email     *string
	Role      NetworkMemberRole
	Pending   bool
}

// Adds a membership in the caller's transaction, with a member_id new to this
// join, and returns it. A user that already holds a membership in the network is
// refused with ErrNetworkMemberExists and nothing is written, so the earlier
// member_id stays the only one. A binary that does not link MG6 refuses.
func AddNetworkMemberInTx(ctx context.Context, tx server.PgTx, args *AddNetworkMemberArgs) (*NetworkMember, error) {
	if !server.SidecarMg6Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkTeamsUnavailableMessage)
	}
	if !args.Role.Valid() {
		return nil, fmt.Errorf("%q is not a member role", args.Role)
	}
	email := args.Email
	if email != nil && (*email == "" || networkMemberEmailMaxLength < len(*email)) {
		email = nil
	}
	member := &NetworkMember{
		MemberId:  server.NewId(),
		NetworkId: args.NetworkId,
		UserId:    args.UserId,
		Email:     email,
		Role:      args.Role,
		Pending:   args.Pending,
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_member (
				member_id,
				network_id,
				user_id,
				email,
				role,
				pending
			)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (network_id, user_id) DO NOTHING
		`,
		member.MemberId,
		member.NetworkId,
		member.UserId,
		member.Email,
		string(member.Role),
		member.Pending,
	))
	if tag.RowsAffected() != 1 {
		return nil, ErrNetworkMemberExists
	}
	return member, nil
}

const networkMemberColumns = `
	member_id,
	network_id,
	user_id,
	email,
	role,
	pending,
	reauth_deadline
`

// Scans a row of networkMemberColumns.
func scanNetworkMember(result server.PgResult) *NetworkMember {
	member := &NetworkMember{}
	var role string
	server.Raise(result.Scan(
		&member.MemberId,
		&member.NetworkId,
		&member.UserId,
		&member.Email,
		&role,
		&member.Pending,
		&member.ReauthDeadline,
	))
	member.Role = NetworkMemberRole(role)
	return member
}

// The membership member_id names, nil when none does: a member_id of an ended
// membership names nothing, whatever the user joined since. Nil while the binary
// does not link MG6.
func GetNetworkMember(ctx context.Context, memberId server.Id) (member *NetworkMember) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		member = getNetworkMemberWithQuery(ctx, conn, memberId)
	})
	return
}

// GetNetworkMember in the caller's transaction.
func GetNetworkMemberInTx(ctx context.Context, tx server.PgTx, memberId server.Id) *NetworkMember {
	if !server.SidecarMg6Linked() {
		return nil
	}
	return getNetworkMemberWithQuery(ctx, tx, memberId)
}

func getNetworkMemberWithQuery(ctx context.Context, query server.PgCanQuery, memberId server.Id) (member *NetworkMember) {
	result, err := query.Query(
		ctx,
		`SELECT `+networkMemberColumns+` FROM network_member WHERE member_id = $1`,
		memberId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			member = scanNetworkMember(result)
		}
	})
	return
}

// The user's membership in the network, nil when it holds none or the binary
// does not link MG6.
func GetNetworkMemberOfUser(ctx context.Context, networkId server.Id, userId server.Id) (member *NetworkMember) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		member = getNetworkMemberOfUserWithQuery(ctx, conn, networkId, userId)
	})
	return
}

// GetNetworkMemberOfUser in the caller's transaction.
func GetNetworkMemberOfUserInTx(ctx context.Context, tx server.PgTx, networkId server.Id, userId server.Id) *NetworkMember {
	if !server.SidecarMg6Linked() {
		return nil
	}
	return getNetworkMemberOfUserWithQuery(ctx, tx, networkId, userId)
}

func getNetworkMemberOfUserWithQuery(ctx context.Context, query server.PgCanQuery, networkId server.Id, userId server.Id) (member *NetworkMember) {
	result, err := query.Query(
		ctx,
		`SELECT `+networkMemberColumns+` FROM network_member WHERE network_id = $1 AND user_id = $2`,
		networkId,
		userId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			member = scanNetworkMember(result)
		}
	})
	return
}

// The email a new membership of the user records: its oldest verified email
// sign-in, else the email of its oldest Apple or Google sign-in, else none. A
// phone, a wallet or a seed phrase is no email.
func networkMemberEmailOfUserInTx(ctx context.Context, tx server.PgTx, userId server.Id) (email *string) {
	result, err := tx.Query(
		ctx,
		`
			SELECT user_auth FROM (
				SELECT user_auth, 0 AS source, create_time
				FROM network_user_auth_password
				WHERE user_id = $1 AND auth_type = $2 AND verified
				UNION ALL
				SELECT user_auth, 1 AS source, create_time
				FROM network_user_auth_sso
				WHERE user_id = $1
			) emails
			ORDER BY source, create_time, user_auth
			LIMIT 1
		`,
		userId,
		string(UserAuthTypeEmail),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&email))
		}
	})
	if email != nil && (!strings.Contains(*email, "@") || networkMemberEmailMaxLength < len(*email)) {
		return nil
	}
	return
}

// The members of the user's network go with the user, in the caller's
// transaction: its memberships and its IdP identities, as §2.8 api-1 and api-3
// keep them for the account's life. The teams keep their synthetic owners of
// record, so each stays sound (D-TEAM-2). A no-op while the binary does not link
// MG6.
func removeNetworkMembershipsOfUserInTx(ctx context.Context, tx server.PgTx, userId server.Id) {
	if !server.SidecarMg6Linked() {
		return
	}
	server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_member WHERE user_id = $1`, userId))
	server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_idp_identity WHERE user_id = $1`, userId))
}

type GetNetworkMembersArgs struct {
	// the member_id to read after, in member_id order; nil from the first
	After *server.Id
}

// A member as GET /network/members shows it: §2.8 api-3's user, email and role,
// with the JIT state and the reauthentication deadline.
type NetworkMemberResult struct {
	MemberId       server.Id         `json:"member_id"`
	UserId         server.Id         `json:"user_id"`
	Email          *string           `json:"email,omitempty"`
	Role           NetworkMemberRole `json:"role"`
	Pending        bool              `json:"pending,omitempty"`
	ReauthDeadline *time.Time        `json:"reauth_deadline,omitempty"`
}

type GetNetworkMembersResult struct {
	Members []*NetworkMemberResult `json:"members"`
	// members follow the last one: read again after it
	More bool `json:"more,omitempty"`
}

// The refusal of a member route to a caller that may not use it.
const networkMembersRefusedMessage = "Only the team's Owner may list its members."

// GET /network/members: the members of the session's network, a team on the team
// tier, in member_id order, at most the settings' MemberReadLimit at a time.
//
// The team's Owner reads them. TEAM-M6-03's routePermissionByRoute widens this to
// each role that reads; until then the route is unmapped, which D-TEAM-3 makes
// owner-only. A pending member reads nothing.
func GetNetworkMembers(args *GetNetworkMembersArgs, clientSession *session.ClientSession) (*GetNetworkMembersResult, error) {
	return newNetworkTeams().getMembers(args, clientSession)
}

func (self *networkTeams) getMembers(args *GetNetworkMembersArgs, clientSession *session.ClientSession) (*GetNetworkMembersResult, error) {
	if !server.SidecarMg6Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkTeamsUnavailableMessage)
	}
	ctx := clientSession.Ctx
	byJwt := clientSession.ByJwt
	if byJwt == nil || byJwt.ClientId != nil {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkMembersRefusedMessage)
	}
	if err := self.requireTeamTier(ctx, byJwt.NetworkId); err != nil {
		return nil, err
	}

	limit := max(self.settings.MemberReadLimit, 1)
	result := &GetNetworkMembersResult{
		Members: []*NetworkMemberResult{},
	}
	refused := false
	server.Db(ctx, func(conn server.PgConn) {
		caller := getNetworkMemberOfUserWithQuery(ctx, conn, byJwt.NetworkId, byJwt.UserId)
		if caller == nil || caller.Pending || caller.Role != NetworkMemberRoleOwner {
			refused = true
			return
		}
		rows, err := conn.Query(
			ctx,
			`
				SELECT `+networkMemberColumns+`
				FROM network_member
				WHERE network_id = $1 AND ($2::uuid IS NULL OR $2::uuid < member_id)
				ORDER BY member_id
				LIMIT $3
			`,
			byJwt.NetworkId,
			args.After,
			limit+1,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				member := scanNetworkMember(rows)
				if limit <= len(result.Members) {
					result.More = true
					continue
				}
				result.Members = append(result.Members, &NetworkMemberResult{
					MemberId:       member.MemberId,
					UserId:         member.UserId,
					Email:          member.Email,
					Role:           member.Role,
					Pending:        member.Pending,
					ReauthDeadline: member.ReauthDeadline,
				})
			}
		})
	})
	if refused {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkMembersRefusedMessage)
	}
	return result, nil
}
