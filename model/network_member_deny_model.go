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
//	         longer holds (TEAM-M6-02, TEAM-M6-03); an API key names its
//	         creator by user, so a re-join's new role reaches it again
//	         (questions/member-removal-ends-keys-api-keys-rejoin.md)
//	clients  its untagged clients are deactivated through the deactivation
//	         path, which runs §7.10's chain for each, tokens, keys, listeners,
//	         publishes and certificates, and owes their rev/ objects; its tagged
//	         agents stay, keeping their tags' grants (owner decision 83)
//	keys     every auth key the member's user made in the network is deleted
//	         (QD-1211): a key names its creator by user, never by membership,
//	         so a key that outlived the removal would enroll again once the
//	         same person re-joined under a new member_id, against §7.5's "a
//	         re-invite never revives an old token". The removal owes the auth
//	         key notice once, listing every key it ended, and an auth_key.revoke
//	         admin event for each, naming the removal's actor and request, as a
//	         revoke does. Deleting a pre-approved key stops its bearer only: a
//	         copy of its bundle still admits devices until its Delegation ends
//	         (D-TEAM-31), which the removal's answer says (BundleStillAdmits)
//	rev/     §7.10's chain on the member itself owes rev/<network>/member/
//	         <member_id> (TEAM-M1-07), which voids its service tokens at evs and
//	         gkm within their 15 minutes
//	event    the removal's own admin event, member.remove or scim.deactivate,
//	         recorded last
//
// A key made while its member is being removed is ended too. Its creation
// reads the creator's membership under a share lock in its own transaction
// (createAuthKeyAt), so a creation that reaches the row after the denial
// locked it meets the deleted row and makes nothing, and the denial's lock
// waits for a creation that locked the row first. Under read committed the
// denial's delete then sees that key. Under repeatable read, server.Tx's
// default, a creation that commits after the denial's snapshot is not in what
// the delete sees, but it appended its auth_key.create event to the network's
// admin event chain, and the denial's own events, which it always appends
// after the delete, meet that event as a serialization failure
// (network_admin_event_model.go), so server.Tx runs the denial again, and the
// rerun deletes the key.
//
// It changes no reader. The config repo still admits the member's devices and
// members still encrypt to them until an admin device, or the indexer
// ServiceSettings.scim_signer names for a member outside the AdminSet, signs
// the MemberRevoke, which is the removal (owner decisions 5, 59). An admin
// device finds the removals still owed as the config's members that the server
// no longer lists (sidecar's ConfigState.UnsignedMemberRemovals), and SCIM's
// queue names its own (network_scim_model.go). When the MemberRevoke lands,
// config_log_effects.go ends the member again, which is idempotent, its keys
// included (removeConfigMemberInTx), so a MemberRevoke an admin device signed
// with no denial before it ends them too.
//
// Nothing here reads or writes network_member unless the binary links MG6, nor
// network_auth_key unless it links MG1.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
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
	// the removal's own admin event of the network: member.remove from POST
	// /network/member-remove, scim.deactivate from SCIM. The denial records it
	// last, after an auth_key.revoke event for each auth key it ends, each
	// naming this event's actor and request. A denial without one is refused.
	Event *AdminEvent
}

// What a denial ended.
type NetworkMemberDenied struct {
	// the membership as it was
	Member *NetworkMember
	// the untagged clients it deactivated, descendants included by the
	// deactivation path
	DeactivatedClientCount int64
	// the auth keys the member's user made in the network, which the denial
	// deleted, in auth_key_id order, without their secrets
	RevokedAuthKeys []*NetworkAuthKey
}

// Whether a key the denial ended was pre-approved, so that a copy of its
// bundle still admits devices until its Delegation ends (D-TEAM-31): what the
// removal's answer says, as a revoke's does.
func (self *NetworkMemberDenied) BundleStillAdmits() bool {
	return slices.ContainsFunc(self.RevokedAuthKeys, func(authKey *NetworkAuthKey) bool {
		return authKey.PreApproved
	})
}

// Refused: no membership of that member_id in the network.
var ErrNetworkMemberNotFound = errors.New("No such member of the team.")

// Ends a member at the server in the caller's transaction (the file header):
// its untagged clients deactivated, every auth key its user made in the
// network deleted, §7.10's chain on the member, its network_member row
// deleted, and the removal's own admin event recorded. ErrNetworkMemberNotFound
// when the network holds no such membership, which a removal racing this one
// also answers, after its commit. A binary that does not link MG6 refuses.
func DenyNetworkMemberInTx(ctx context.Context, tx server.PgTx, denial *NetworkMemberDenial) (*NetworkMemberDenied, error) {
	if !server.SidecarMg6Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkTeamsUnavailableMessage)
	}
	if denial.Time.IsZero() {
		return nil, errors.New("a member's denial with no time")
	}
	if denial.Event == nil || denial.Event.NetworkId != denial.NetworkId {
		return nil, errors.New("a member's denial with no admin event of its network")
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
	denied.RevokedAuthKeys = endNetworkMemberAuthKeysInTx(ctx, tx, denial.NetworkId, member.UserId, denial.Event, denial.Time)
	if err := ApplyRemovalEffectsInTx(ctx, tx, &Removal{
		Subjects: []*RemovalSubject{{NetworkId: denial.NetworkId, Kind: sidecar.RevocationKindMember, SubjectId: denial.MemberId}},
		Reason:   sidecar.RevocationReasonRemoved,
		Time:     denial.Time,
	}); err != nil {
		return nil, err
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`DELETE FROM network_member WHERE member_id = $1 AND network_id = $2`,
		denial.MemberId,
		denial.NetworkId,
	))
	AddAdminEventInTx(ctx, tx, denial.Event)
	return denied, nil
}

// Ends the auth keys creatorUserId made in networkId, in tx, the removal's
// own transaction (the file header's keys row): every such row is deleted,
// expired or not, and the removal owes one auth key notice listing each key as
// revoked at the removal's time, and an auth_key.revoke admin event for each,
// with removalEvent's actor and request. Answers the keys it deleted, in
// auth_key_id order; none in a binary that does not link MG1, which holds no
// key. The caller holds the membership's row locked, so an enrollment's spend,
// which reads that row first, waits for the removal or fails it.
func endNetworkMemberAuthKeysInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	creatorUserId server.Id,
	removalEvent *AdminEvent,
	at time.Time,
) []*NetworkAuthKey {
	revoked := []*NetworkAuthKey{}
	if !server.SidecarMg1Linked() {
		return revoked
	}
	result, err := tx.Query(
		ctx,
		`
			DELETE FROM network_auth_key
			WHERE network_id = $1 AND creator_user_id = $2
			RETURNING
				auth_key_id,
				network_id,
				creator_user_id,
				tag_ids,
				create_time,
				expire_time,
				max_uses,
				use_count,
				ephemeral,
				pre_approved
		`,
		networkId,
		creatorUserId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			authKey := &NetworkAuthKey{}
			server.Raise(result.Scan(
				&authKey.AuthKeyId,
				&authKey.NetworkId,
				&authKey.CreatorUserId,
				&authKey.TagIds,
				&authKey.CreateTime,
				&authKey.ExpireTime,
				&authKey.MaxUses,
				&authKey.UseCount,
				&authKey.Ephemeral,
				&authKey.PreApproved,
			))
			revoked = append(revoked, authKey)
		}
	})
	if len(revoked) == 0 {
		return revoked
	}
	slices.SortFunc(revoked, func(a *NetworkAuthKey, b *NetworkAuthKey) int {
		return a.AuthKeyId.Cmp(b.AuthKeyId)
	})
	changes := make([]*SecurityAuthKeyChange, 0, len(revoked))
	for _, authKey := range revoked {
		changes = append(changes, newSecurityAuthKeyChange(ctx, SecurityAuthKeyRevoked, authKey, at))
	}
	oweSecurityAuthKeyNoticeInTx(ctx, tx, networkId, changes...)
	for _, authKey := range revoked {
		authKeyId := authKey.AuthKeyId
		AddAdminEventInTx(ctx, tx, &AdminEvent{
			NetworkId:  networkId,
			ActorKind:  removalEvent.ActorKind,
			ActorId:    removalEvent.ActorId,
			Action:     AdminEventActionAuthKeyRevoke,
			TargetKind: AdminEventTargetAuthKey,
			TargetId:   &authKeyId,
			Result:     AdminEventResultOk,
			RequestId:  removalEvent.RequestId,
		})
	}
	return revoked
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
