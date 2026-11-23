package model

// §7.5's roles at the server (R-TEAM-2, D-TEAM-3, I-6; TEAM-M6-03): what each
// of the seven roles may do on the routes that administer a network, as
// permissions api's route table names (api/route_permission.go).
//
//	Owner          all, including deletion, the IdP and the admin set
//	Admin          members, roles, admissions; policy, tags, groups, DNS, edge,
//	               versioning rules; keys, OAuth clients; read
//	Network admin  policy, tags, groups, DNS, edge, versioning rules; keys for
//	               owned tags; read
//	IT admin       admissions; untagged keys; read
//	Billing admin  billing, payouts; read
//	Auditor        read (settings, clients, audit)
//	Member         own devices
//
// No role holds RolePermissionAccount, the owner of record's own account: its
// sign-ins, seedphrases and auth codes. In a team that account is synthetic and
// never signs in (D-TEAM-2), so a member that could add a sign-in to it could
// sign in as the network itself. A personal network's owner carries no
// member_id and passes every permission.
//
// Every role holds RolePermissionOwnDevices, the Member's "own devices": every
// member is a person whose devices enroll from an auth key its own credential
// makes, its first one from its invite (§7.5) whatever role the invite names.
// The three auth key routes ask it, and the auth key model narrows a caller
// whose role, cut by its scopes, lacks RolePermissionKeys
// (network_auth_key_model.go's authKeyReachOf): it creates a key for its own
// devices, untagged and not pre-approved, so the device it enrolls is the
// caller's own, and it lists and revokes only the keys it created. Untagged
// keys (IT admin) are such keys. Keys for owned tags (Network admin) are
// RolePermissionOwnedTagKeys: beside a key for its own devices, a tagged key,
// pre-approved or not, whose every tag the network's tagOwners name the
// caller's member an owner of at the newest verified state of its log, which
// the model reads at the key's creation and again at each enrollment
// (network_client_owner_model.go).
//
// Roles are signed in the config repo, AdminSet for the first four and
// MemberAdmit for the rest, and network_member mirrors them from verified log
// entries only (config_log_effects.go's mirrorConfigMemberRolesInTx), never
// from a request; the one role a request sets is the first Owner, which
// team-create writes before the team's config repo exists.
//
// A credential's scopes cut its role further (§7.8): an API key's and an admin
// token's reach is their scopes ∩ their creator's role.

import (
	"context"

	"github.com/urnetwork/server"
)

// What a network administration route asks of its caller's role.
type RolePermission string

const (
	// the owner of record's own account; no team role holds it
	RolePermissionAccount RolePermission = "account"
	// deletion, the IdP and the admin set: the Owner's alone
	RolePermissionOwner RolePermission = "owner"
	// members, roles, invitations
	RolePermissionMembers RolePermission = "members"
	// the network's own settings: its name, its emoji, its visibility, its
	// referrer
	RolePermissionAdmin RolePermission = "admin"
	// admissions: the network's clients, devices and proxies
	RolePermissionDevices RolePermission = "devices"
	// policy, tags, groups, DNS, edge and versioning rules, and network-wide
	// blocks
	RolePermissionPolicy RolePermission = "policy"
	// auth keys, API keys and OAuth clients
	RolePermissionKeys RolePermission = "keys"
	// a tagged auth key, pre-approved or not, for tags the caller's member
	// owns under the network's tagOwners (§7.5's Network admin: keys for owned
	// tags); every role holding RolePermissionKeys holds it too
	RolePermissionOwnedTagKeys RolePermission = "owned_tag_keys"
	// an auth key for the caller's own devices: untagged and not pre-approved,
	// so the device it enrolls is the caller's (§7.5's Member: own devices),
	// and the keys the caller created, to list and revoke; every role holds it
	RolePermissionOwnDevices RolePermission = "own_devices"
	// billing, payouts and wallets
	RolePermissionBilling RolePermission = "billing"
	// reading settings, clients and audit
	RolePermissionRead RolePermission = "read"
	// the caller's own credential and account preferences
	RolePermissionSelf RolePermission = "self"
)

// Every permission, for the tables' tests.
var RolePermissions = []RolePermission{
	RolePermissionAccount,
	RolePermissionOwner,
	RolePermissionMembers,
	RolePermissionAdmin,
	RolePermissionDevices,
	RolePermissionPolicy,
	RolePermissionKeys,
	RolePermissionOwnedTagKeys,
	RolePermissionOwnDevices,
	RolePermissionBilling,
	RolePermissionRead,
	RolePermissionSelf,
}

// §7.5's table as permissions.
var networkMemberRolePermissions = map[NetworkMemberRole]map[RolePermission]bool{
	NetworkMemberRoleOwner: {
		RolePermissionOwner:        true,
		RolePermissionMembers:      true,
		RolePermissionAdmin:        true,
		RolePermissionDevices:      true,
		RolePermissionPolicy:       true,
		RolePermissionKeys:         true,
		RolePermissionOwnedTagKeys: true,
		RolePermissionOwnDevices:   true,
		RolePermissionBilling:      true,
		RolePermissionRead:         true,
		RolePermissionSelf:         true,
	},
	NetworkMemberRoleAdmin: {
		RolePermissionMembers:      true,
		RolePermissionAdmin:        true,
		RolePermissionDevices:      true,
		RolePermissionPolicy:       true,
		RolePermissionKeys:         true,
		RolePermissionOwnedTagKeys: true,
		RolePermissionOwnDevices:   true,
		RolePermissionRead:         true,
		RolePermissionSelf:         true,
	},
	NetworkMemberRoleNetworkAdmin: {
		RolePermissionPolicy:       true,
		RolePermissionOwnedTagKeys: true,
		RolePermissionOwnDevices:   true,
		RolePermissionRead:         true,
		RolePermissionSelf:         true,
	},
	NetworkMemberRoleItAdmin: {
		RolePermissionDevices:    true,
		RolePermissionOwnDevices: true,
		RolePermissionRead:       true,
		RolePermissionSelf:       true,
	},
	NetworkMemberRoleBillingAdmin: {
		RolePermissionBilling:    true,
		RolePermissionOwnDevices: true,
		RolePermissionRead:       true,
		RolePermissionSelf:       true,
	},
	NetworkMemberRoleAuditor: {
		RolePermissionOwnDevices: true,
		RolePermissionRead:       true,
		RolePermissionSelf:       true,
	},
	NetworkMemberRoleMember: {
		RolePermissionOwnDevices: true,
		RolePermissionSelf:       true,
	},
}

// Whether the role holds the permission. A role this build does not know holds
// none.
func (self NetworkMemberRole) Permits(permission RolePermission) bool {
	return networkMemberRolePermissions[self][permission]
}

// The permissions each of §7.8's scopes reaches, within its creator's role.
// all reaches everything the role does. Only the scopes that reach
// RolePermissionKeys reach RolePermissionOwnDevices and
// RolePermissionOwnedTagKeys, so moving the auth key routes from the one to
// the others changed no scoped credential's reach there.
var apiKeyScopePermissions = map[string]map[RolePermission]bool{
	ApiKeyScopeDevicesCore: {
		RolePermissionDevices: true,
		RolePermissionRead:    true,
		RolePermissionSelf:    true,
	},
	ApiKeyScopeAuthKeys: {
		RolePermissionKeys:         true,
		RolePermissionOwnedTagKeys: true,
		RolePermissionOwnDevices:   true,
		RolePermissionSelf:         true,
	},
	ApiKeyScopePolicyRead: {
		RolePermissionRead: true,
	},
	ApiKeyScopeLogsRead: {
		RolePermissionRead: true,
	},
}

// Whether scopes reach the permission: nil is a credential with no scopes of
// its own, such as a token or a legacy API key, which reaches what its role
// does; else one of the scopes must, all reaching everything. A scope this
// build does not know reaches nothing.
func ScopesPermit(scopes []string, permission RolePermission) bool {
	if scopes == nil {
		return true
	}
	for _, scope := range scopes {
		if scope == ApiKeyScopeAll || apiKeyScopePermissions[scope][permission] {
			return true
		}
	}
	return false
}

// Whether a credential of the role, carrying scopes, may act where the
// permission is asked: the role's permissions ∩ its scopes'.
func RoleAndScopesPermit(role NetworkMemberRole, scopes []string, permission RolePermission) bool {
	return role.Permits(permission) && ScopesPermit(scopes, permission)
}

// The member's role in networkId as network_member mirrors it, false when the
// network holds no such membership or it is JIT pending (§7.6), which reaches
// nothing, or the binary does not link MG6.
func MemberRole(ctx context.Context, networkId server.Id, memberId server.Id) (role NetworkMemberRole, ok bool) {
	if !server.SidecarMg6Linked() {
		return "", false
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT role
				FROM network_member
				WHERE network_id = $1 AND member_id = $2 AND NOT pending
			`,
			networkId,
			memberId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var roleText string
				server.Raise(result.Scan(&roleText))
				role = NetworkMemberRole(roleText)
				ok = role.Valid()
			}
		})
	})
	return
}

// Whether the network is a team: a network_team row, which only team-create
// writes. False in a binary that does not link MG6.
func NetworkIsTeam(ctx context.Context, networkId server.Id) (team bool) {
	if !server.SidecarMg6Linked() {
		return false
	}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM network_team WHERE network_id = $1)`,
			networkId,
		).Scan(&team))
	})
	return
}
