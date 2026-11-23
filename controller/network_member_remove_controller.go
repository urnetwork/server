package controller

// §7.10's member removal at api (TEAM-M6-11; R-TEAM-11, D-TEAM-10, I-15,
// FS-87): POST /network/member-remove {member_id} denies a team's member at
// once and leaves its removal owed to an admin device's signature.
//
//	caller   a network credential of an Owner or Admin of a team on the team
//	         tier, or a one-command admin token with the all scope whose use
//	         the removal's transaction spends; never a client token or an API
//	         key. A member of the AdminSet (§7.5's first four roles) is
//	         removed only by an Owner, since the admin set is the Owner's
//	         (§7.5's table), and the team keeps at least one Owner
//	deny     model.DenyNetworkMemberInTx in one transaction: the membership's
//	         row, its untagged clients through the deactivation path, every
//	         auth key the member's user made in the team (QD-1211), each with
//	         its auth_key.revoke admin event and all of them in one auth key
//	         notice, and the member's rev/ object; the removal's admin event,
//	         member.remove; the network authority notice when the member was
//	         in the AdminSet
//	answer   the member, whether its MemberRevoke needs k of n, how many
//	         clients and auth keys the denial ended, and signature_owed, always
//	         set: the server's denial is not the removal (owner decisions 5,
//	         59). Until an admin device signs the MemberRevoke, an authority
//	         statement for an AdminSet member, with its presence
//	         countersignature where §7.9's presence row asks, members still
//	         encrypt to the member's devices; `ur admin members remove` signs
//	         it beside this call. When an ended key was pre-approved,
//	         bundle_still_admits and its note say that a copy of its bundle
//	         still admits devices until its Delegation ends, as a key's revoke
//	         answers (D-TEAM-31)
//
// A refusal names no member it did not find.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The scope an admin token needs to remove a member: §7.8's OAuth client scopes
// hold none narrower for members, so only all.
const networkMemberRemoveAdminScope = "all"

// Refusals, fixed text.
const (
	networkMemberRemoveRefusedMessage    = "Only the team's Owners and Admins may remove members."
	networkMemberRemoveAdminSetMessage   = "Only the team's Owners may remove a member of the admin set."
	networkMemberRemoveLastOwnerMessage  = "The team's last Owner cannot be removed."
	networkMemberRemoveScopeMessage      = "The admin token does not carry the all scope."
	networkMemberRemoveUnavailableReason = "Teams are not available."
)

// What a removal that ended a pre-approved auth key answers (D-TEAM-31), as
// the key's revoke does.
const networkMemberRemoveBundleNote = "Removing the member revoked the auth keys it made, which stops their bearers only. A copy of a pre-approved key's bundle still admits devices until its Delegation ends: run `ur admin delegations revoke` on an admin device to end it."

func init() {
	// a one-command admin token reaches the route, and its use is spent in the
	// removal's transaction (session.RegisterAdminCommandRoute)
	session.RegisterAdminCommandRoute(http.MethodPost, "/network/member-remove")
}

type RemoveNetworkMemberArgs struct {
	MemberId server.Id `json:"member_id"`
}

type RemoveNetworkMemberError struct {
	Message string `json:"message"`
}

type RemoveNetworkMemberResult struct {
	MemberId *server.Id `json:"member_id,omitempty"`
	// the member was in the AdminSet: its MemberRevoke is an authority
	// statement, k of n
	AdminSet bool `json:"admin_set,omitempty"`
	// the member's untagged clients the denial deactivated
	DeactivatedClientCount int64 `json:"deactivated_client_count,omitempty"`
	// the auth keys the member's user made in the team, which the denial
	// revoked: their bearers enroll nothing more
	RevokedAuthKeyCount int64 `json:"revoked_auth_key_count,omitempty"`
	// a revoked key was pre-approved: a copy of its bundle keeps admitting
	// devices until its Delegation ends (D-TEAM-31)
	BundleStillAdmits bool `json:"bundle_still_admits,omitempty"`
	// what to do about such a bundle
	Note string `json:"note,omitempty"`
	// the removal is cryptographic only once an admin device signs the
	// member's MemberRevoke; set on every removal
	SignatureOwed bool                      `json:"signature_owed,omitempty"`
	Error         *RemoveNetworkMemberError `json:"error,omitempty"`
}

// What removals depend on apart from the database, so tests can replace it.
type networkMemberRemovals struct {
	now         func() time.Time
	requireTier func(ctx context.Context, networkId server.Id) error
}

func newNetworkMemberRemovals() *networkMemberRemovals {
	return &networkMemberRemovals{
		now:         server.NowUtc,
		requireTier: model.RequireTeamTier,
	}
}

// POST /network/member-remove (see the file header).
func RemoveNetworkMember(args *RemoveNetworkMemberArgs, clientSession *session.ClientSession) (*RemoveNetworkMemberResult, error) {
	return newNetworkMemberRemovals().remove(args, clientSession)
}

func (self *networkMemberRemovals) remove(args *RemoveNetworkMemberArgs, clientSession *session.ClientSession) (*RemoveNetworkMemberResult, error) {
	if !server.SidecarMg6Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkMemberRemoveUnavailableReason)
	}
	ctx := clientSession.Ctx
	byJwt := clientSession.ByJwt
	if byJwt == nil || byJwt.ClientId != nil || clientSession.ApiKeyAuthenticated {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkMemberRemoveRefusedMessage)
	}
	adminCommand := clientSession.AdminCommand
	if adminCommand != nil && !slices.Contains(adminCommand.Scopes, networkMemberRemoveAdminScope) {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkMemberRemoveScopeMessage)
	}
	if clientSession.UnsignedIdentity && adminCommand == nil {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkMemberRemoveRefusedMessage)
	}
	if err := self.requireTier(ctx, byJwt.NetworkId); err != nil {
		return nil, err
	}
	now := self.now().UTC().Truncate(time.Microsecond)

	var answer *RemoveNetworkMemberResult
	var refusedErr error
	server.Tx(ctx, func(tx server.PgTx) {
		answer, refusedErr = nil, nil
		// the caller first, so a refused caller writes nothing, its token's jti
		// included
		caller := model.GetNetworkMemberOfUserInTx(ctx, tx, byJwt.NetworkId, byJwt.UserId)
		if caller == nil || caller.Pending || !caller.Role.Permits(model.RolePermissionMembers) {
			refusedErr = fmt.Errorf("%d %s", http.StatusForbidden, networkMemberRemoveRefusedMessage)
			return
		}
		if adminCommand != nil {
			storedResult, first, err := model.ClaimAdminTokenUseInTx(ctx, tx, adminCommand, now)
			server.Raise(err)
			if !first {
				// a replay of the token: the first use's answer, and nothing runs
				replayed := &RemoveNetworkMemberResult{}
				server.Raise(json.Unmarshal(storedResult, replayed))
				answer = replayed
				return
			}
		}
		refuse := func(message string) {
			answer = &RemoveNetworkMemberResult{Error: &RemoveNetworkMemberError{Message: message}}
		}
		target := model.GetNetworkMemberInTx(ctx, tx, args.MemberId)
		switch {
		case target == nil || target.NetworkId != byJwt.NetworkId:
			refuse(model.ErrNetworkMemberNotFound.Error())
		case target.Role.AdminSet() && !caller.Role.Permits(model.RolePermissionOwner):
			refuse(networkMemberRemoveAdminSetMessage)
		case target.Role == model.NetworkMemberRoleOwner && !target.Pending && model.CountNetworkOwnersInTx(ctx, tx, byJwt.NetworkId) <= 1:
			refuse(networkMemberRemoveLastOwnerMessage)
		}
		if answer == nil {
			// the removal's event, which the denial records after its keys'
			memberId := args.MemberId
			denied, err := model.DenyNetworkMemberInTx(ctx, tx, &model.NetworkMemberDenial{
				NetworkId: byJwt.NetworkId,
				MemberId:  args.MemberId,
				Time:      now,
				Event:     model.NewSessionAdminEvent(clientSession, model.AdminEventActionMemberRemove, model.AdminEventTargetMember, &memberId),
			})
			switch {
			case errors.Is(err, model.ErrNetworkMemberNotFound):
				refuse(model.ErrNetworkMemberNotFound.Error())
			case err != nil:
				server.Raise(err)
			default:
				adminSet := denied.Member.Role.AdminSet()
				if adminSet {
					model.OweNetworkMemberDeniedNoticeInTx(ctx, tx, byJwt.NetworkId, now)
				}
				answer = &RemoveNetworkMemberResult{
					MemberId:               &memberId,
					AdminSet:               adminSet,
					DeactivatedClientCount: denied.DeactivatedClientCount,
					RevokedAuthKeyCount:    int64(len(denied.RevokedAuthKeys)),
					SignatureOwed:          true,
				}
				if denied.BundleStillAdmits() {
					answer.BundleStillAdmits = true
					answer.Note = networkMemberRemoveBundleNote
				}
			}
		}
		if adminCommand != nil {
			// a refusal is the command's answer too, which a retry repeats
			resultJson, err := json.Marshal(answer)
			server.Raise(err)
			server.Raise(model.StoreAdminTokenUseResultInTx(ctx, tx, adminCommand.Jti, resultJson))
		}
	})
	if refusedErr != nil {
		return nil, refusedErr
	}
	return answer, nil
}
