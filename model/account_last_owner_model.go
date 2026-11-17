package model

// §7.5's last Owner (owner decision 329; QD-941): an account deletion is
// refused while its user is the last Owner of any team, so every team stays
// administrable at the server (inviting, roles, buying, cancelling, deleting)
// through a person holding the Owner role. The refusal names the team and what
// to do first, make another member Owner or delete the team; nothing is deleted
// on it, and the user deletes freely once the team is handed on or gone. An
// Owner lost any other way (a restore that dropped the member row, a member
// removed by SCIM or by the log) comes back through the team's own authority: a
// signed AdminSet naming another member Owner reaches the role mirror
// (config_log_effects.go). The server never originates a role.
//
// A live Owner is a network_member row whose role is owner and which is not
// pending, as the member removal's refusal counts them (CountNetworkOwnersInTx):
// a JIT pending member reaches nothing, so its role administers nothing.
//
//	check  GetLastOwnerTeam, read before any store step of POST
//	       /auth/network-delete, so a refusal leaves every subscription as it
//	       was (controller/network_remove_providers.go)
//	lock   lockLastOwnerTeamInTx, in RemoveNetworkWithStoreSnapshot's
//	       transaction before anything is deleted: the user's memberships, and
//	       the live Owners of each team the user is a live Owner of, locked FOR
//	       UPDATE in (network_id, member_id) order and held to the commit
//
// The two paths that refuse to leave a team without an Owner lock the team's
// rows they read in member_id order: this deletion, across teams in network_id
// order, and the member removal, whose count locks the team's live Owners before
// its refusal is decided (CountNetworkOwnersInTx). The role mirror updates an
// entry's members in member_id order too, and SCIM's deactivation locks the one
// row it ends. So of two deletions by a team's two Owners the second waits on
// the first's locks and, once the first commits, finds itself the last Owner and
// refuses; a member removal, a deactivation or a role change racing a deletion
// waits for it, or it for them. The deletion runs read committed, so after a
// wait it reads a row as committed: one deleted meanwhile is gone, and one whose
// role changed is read with its new role. A team in which the user became a live
// Owner after the first read below is judged by the user's row alone, which can
// only refuse a deletion that its retry then allows. A path that takes these
// rows in another order, such as a team's own deletion or a log entry removing
// several members, can deadlock with a deletion; PostgreSQL breaks it and
// server.Tx reruns the loser. The team's own authority, SCIM or the log, may
// still end its last Owner; the AdminSet above is the recovery.
//
// Nothing here reads network_member unless the binary links MG6
// (server.SidecarMg6Linked): without it no team exists, and the deletion runs as
// it did before teams.

import (
	"context"

	"github.com/urnetwork/server"
)

// A team an account deletion would leave with no live Owner.
type LastOwnerTeam struct {
	NetworkId   server.Id
	NetworkName string
}

// The first team, in network_id order, whose only live Owner is the user, read
// outside any transaction (the file header's check); nil when there is none or
// the binary does not link MG6. networkId, the network the deletion removes, is
// never the answer: a team that goes with the deletion needs no Owner.
func GetLastOwnerTeam(ctx context.Context, userId server.Id, networkId server.Id) (team *LastOwnerTeam) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		team = lastOwnerTeamWithQuery(ctx, conn, []server.Id{userId}, networkId, false)
	})
	return
}

// GetLastOwnerTeam for the users of userIds in the deletion's transaction, which
// must run read committed: every row read stays locked until tx ends (the file
// header's lock).
func lockLastOwnerTeamInTx(ctx context.Context, tx server.PgTx, userIds []server.Id, networkId server.Id) *LastOwnerTeam {
	if !server.SidecarMg6Linked() {
		return nil
	}
	return lastOwnerTeamWithQuery(ctx, tx, userIds, networkId, true)
}

// The first team, in network_id order, where a user of userIds is a live Owner
// and no other user is, outside networkId. With lock, the users' memberships and
// the live Owners of their teams are read FOR UPDATE in (network_id, member_id)
// order, so query must be a transaction that holds them to its end.
func lastOwnerTeamWithQuery(
	ctx context.Context,
	query server.PgCanQuery,
	userIds []server.Id,
	networkId server.Id,
	lock bool,
) *LastOwnerTeam {
	// the teams the users are live Owners of, as this read finds them; the
	// locked read below judges each by its rows as committed
	ownerNetworkIds := []server.Id{}
	result, err := query.Query(
		ctx,
		`
			SELECT DISTINCT network_id
			FROM network_member
			WHERE
				user_id = ANY($1) AND
				role = $2 AND
				NOT pending AND
				network_id <> $3
			ORDER BY network_id
		`,
		userIds,
		string(NetworkMemberRoleOwner),
		networkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var ownerNetworkId server.Id
			server.Raise(result.Scan(&ownerNetworkId))
			ownerNetworkIds = append(ownerNetworkIds, ownerNetworkId)
		}
	})
	if !lock && len(ownerNetworkIds) == 0 {
		return nil
	}

	// the users' own memberships are locked too, whatever their role, so no
	// role change makes one of them a team's Owner between this check and the
	// deletion of their rows
	lockClause := ""
	if lock {
		lockClause = "FOR UPDATE OF network_member"
	}
	userIdSet := map[server.Id]bool{}
	for _, userId := range userIds {
		userIdSet[userId] = true
	}
	// a team's live Owners, split into the users' and anyone else's
	type teamOwners struct {
		networkName string
		userOwner   bool
		otherOwner  bool
	}
	teamNetworkIds := []server.Id{}
	teamNetworkIdOwners := map[server.Id]*teamOwners{}
	result, err = query.Query(
		ctx,
		`
			/* account-delete-team-owner-lock */
			SELECT
				network_member.network_id,
				network.network_name,
				network_member.user_id,
				network_member.role,
				network_member.pending
			FROM network_member
			INNER JOIN network ON network.network_id = network_member.network_id
			WHERE
				network_member.network_id <> $3 AND (
					network_member.user_id = ANY($1) OR (
						network_member.network_id = ANY($4) AND
						network_member.role = $2 AND
						NOT network_member.pending
					)
				)
			ORDER BY network_member.network_id, network_member.member_id
		`+lockClause,
		userIds,
		string(NetworkMemberRoleOwner),
		networkId,
		ownerNetworkIds,
	)
	// every row is read before deciding, so a locked read locks them all
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var memberNetworkId server.Id
			var networkName string
			var memberUserId server.Id
			var role string
			var pending bool
			server.Raise(result.Scan(
				&memberNetworkId,
				&networkName,
				&memberUserId,
				&role,
				&pending,
			))
			owners, ok := teamNetworkIdOwners[memberNetworkId]
			if !ok {
				owners = &teamOwners{networkName: networkName}
				teamNetworkIdOwners[memberNetworkId] = owners
				teamNetworkIds = append(teamNetworkIds, memberNetworkId)
			}
			if NetworkMemberRole(role) != NetworkMemberRoleOwner || pending {
				continue
			}
			if userIdSet[memberUserId] {
				owners.userOwner = true
			} else {
				owners.otherOwner = true
			}
		}
	})
	for _, teamNetworkId := range teamNetworkIds {
		if owners := teamNetworkIdOwners[teamNetworkId]; owners.userOwner && !owners.otherOwner {
			return &LastOwnerTeam{
				NetworkId:   teamNetworkId,
				NetworkName: owners.networkName,
			}
		}
	}
	return nil
}
