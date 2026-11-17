package model

// §7.10's server half of a member's removal (TEAM-M6-11, TEAM-M6-07; R-TEAM-11,
// D-TEAM-10, D-TEAM-25, I-4, I-15): what api ends at once when a team removes a
// member, by POST /network/member-remove or SCIM's active=false, before any
// admin device signs the member's MemberRevoke. It is a denial, the one kind of
// statement a server originates (I-4):
//
//	row      the member's network_member row is deleted in the caller's
//	         transaction, so every credential naming its member_id stops at
//	         once: its network and admin tokens, and the reach of the API keys
//	         and OAuth clients it created, which is their scopes ∩ a role it no
//	         longer holds (TEAM-M6-02, TEAM-M6-03). The network's revocation
//	         record tombstones the membership first, before the transaction
//	         commits, so sockets, which reads only the record, refuses those
//	         tokens at once too (session.RevokeMemberRecords; owner decision
//	         561)
//	clients  its untagged clients are deactivated through the deactivation
//	         path, which runs §7.10's chain for each, tokens, keys, listeners,
//	         publishes and certificates, and owes their rev/ objects; its tagged
//	         agents stay, keeping their tags' grants (owner decision 83)
//	rev/     §7.10's chain on the member itself owes rev/<network>/member/
//	         <member_id> (TEAM-M1-07), which voids its service tokens at evs and
//	         gkm within their 15 minutes
//
// It changes no reader. The config repo still admits the member's devices and
// members still encrypt to them until an admin device, or the indexer
// ServiceSettings.scim_signer names for a member outside the AdminSet, signs
// the MemberRevoke, which is the removal (owner decisions 5, 59). An admin
// device finds the removals still owed as the config's members that the server
// no longer lists (sidecar's ConfigState.UnsignedMemberRemovals), and SCIM's
// queue names its own (network_scim_model.go). When the MemberRevoke lands,
// config_log_effects.go ends the member again, which is idempotent.
//
// Nothing here reads or writes network_member unless the binary links MG6.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The admin event words of a member's removal.
const (
	AdminEventActionMemberRemove = "member.remove"
)

// One member's denial.
type NetworkMemberDenial struct {
	NetworkId server.Id
	MemberId  server.Id
	// when it took effect, the not_before of the rev/ objects it owes
	Time time.Time
}

// What a denial ended.
type NetworkMemberDenied struct {
	// the membership as it was
	Member *NetworkMember
	// the untagged clients it deactivated, descendants included by the
	// deactivation path
	DeactivatedClientCount int64
}

// Refused: no membership of that member_id in the network.
var ErrNetworkMemberNotFound = errors.New("No such member of the team.")

// Ends a member at the server in the caller's transaction (the file header):
// its untagged clients deactivated, §7.10's chain on the member, and its
// network_member row deleted. ErrNetworkMemberNotFound when the network holds
// no such membership, which a removal racing this one also answers, after its
// commit. A binary that does not link MG6 refuses.
func DenyNetworkMemberInTx(ctx context.Context, tx server.PgTx, denial *NetworkMemberDenial) (*NetworkMemberDenied, error) {
	if !server.SidecarMg6Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkTeamsUnavailableMessage)
	}
	if denial.Time.IsZero() {
		return nil, errors.New("a member's denial with no time")
	}
	var member *NetworkMember
	result, err := tx.Query(
		ctx,
		`
			SELECT `+networkMemberColumns+`
			FROM network_member
			WHERE member_id = $1 AND network_id = $2
			FOR UPDATE
		`,
		denial.MemberId,
		denial.NetworkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			member = scanNetworkMember(result)
		}
	})
	if member == nil {
		return nil, ErrNetworkMemberNotFound
	}

	denied := &NetworkMemberDenied{Member: member}
	if server.SidecarMg0Linked() {
		// the member's own clients, read from the network's active clients so
		// the scan stays the network's; tagged agents are the network's, not
		// the member's, and stay (owner decision 83)
		clientIds := []server.Id{}
		result, err := tx.Query(
			ctx,
			`
				SELECT network_client.client_id
				FROM network_client
				INNER JOIN network_client_owner ON network_client_owner.client_id = network_client.client_id
				WHERE
					network_client.network_id = $1 AND
					network_client.active AND
					network_client_owner.member_id = $2 AND
					NOT network_client_owner.tagged
				ORDER BY network_client.client_id
			`,
			denial.NetworkId,
			denial.MemberId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
		if 0 < len(clientIds) {
			count, err := deactivateNetworkClientsInTx(withRemovalReason(ctx, sidecar.RevocationReasonRemoved), tx, clientIds, denial.NetworkId)
			if err != nil {
				return nil, err
			}
			denied.DeactivatedClientCount = count
		}
	}
	if err := ApplyRemovalEffectsInTx(ctx, tx, &Removal{
		Subjects: []*RemovalSubject{{NetworkId: denial.NetworkId, Kind: sidecar.RevocationKindMember, SubjectId: denial.MemberId}},
		Reason:   sidecar.RevocationReasonRemoved,
		Time:     denial.Time,
	}); err != nil {
		return nil, err
	}
	// every record of the network refuses the membership before the deletion
	// commits; a failed write fails the denial
	if err := session.RevokeMemberRecords(ctx, map[server.Id][]server.Id{denial.NetworkId: {denial.MemberId}}); err != nil {
		return nil, err
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`DELETE FROM network_member WHERE member_id = $1 AND network_id = $2`,
		denial.MemberId,
		denial.NetworkId,
	))
	return denied, nil
}

// How many live Owners the team holds, read in tx: what a removal of an Owner
// must leave at least one of. Their rows stay locked FOR UPDATE until tx ends,
// taken in member_id order, the order an account deletion locks a team's Owners
// in (account_last_owner_model.go), so a deletion, removal or role change of
// another Owner waits for tx, or tx for it, and no two of them each leave the
// other's Owner as the last. Under RepeatableRead a row the winner changed
// fails tx with a serialization failure, which server.Tx reruns.
func CountNetworkOwnersInTx(ctx context.Context, tx server.PgTx, networkId server.Id) (count int) {
	if !server.SidecarMg6Linked() {
		return 0
	}
	result, err := tx.Query(
		ctx,
		`
			/* team-owner-count-lock */
			SELECT member_id
			FROM network_member
			WHERE network_id = $1 AND role = $2 AND NOT pending
			ORDER BY member_id
			FOR UPDATE
		`,
		networkId,
		string(NetworkMemberRoleOwner),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			count += 1
		}
	})
	return
}

// Owes the network authority notice for an AdminSet member the server denied:
// who administers the network changed at the server, before the log's
// MemberRevoke, which owes it again (config_log_effects.go), the two joining
// one email inside the coalescing window. It never fails the removal.
func OweNetworkMemberDeniedNoticeInTx(ctx context.Context, tx server.PgTx, networkId server.Id, at time.Time) {
	oweSecurityNetworkAuthorityNoticeInTx(ctx, tx, networkId, &SecurityNetworkAuthorityChange{
		ChangeTime: at,
		Action:     SecurityNetworkAuthorityAdminSet,
		Via:        securityNoticeVia(ctx),
	})
}
