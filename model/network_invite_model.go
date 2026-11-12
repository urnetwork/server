package model

// §7.5's invitations (TEAM-M6-04; R-TEAM-3, D-TEAM-16, I-4; §2.8 api-3): a
// team's Owner or Admin invites a person by email, and the person joins as a
// member once signed in with that email.
//
//	create   POST /network/member-invite {email, role, expire_days <= 30}, a
//	         network credential of an Owner or Admin of a team on the team
//	         tier, or a one-command admin token whose use the invite's
//	         transaction spends: network_invite keeps the email, the role, the
//	         expiry and only the SHA-256 of the invite's secret. The answer
//	         alone holds ur-invite-<invite_id>-<secret>, the bearer; the
//	         inviting device appends the link's client-only fragment (sidecar's
//	         config_invite.go), which no request carries. New invites owe the
//	         network authority notice and an admin event
//	accept   POST /network/member-invite-accept {invite_bearer}, the invitee's
//	         network credential, never a client's, an API key or an admin
//	         token: one of the person's verified emails is the invite's, the
//	         invite is unexpired, and the person holds no membership of the
//	         team. The membership starts as member: roles are signed in the
//	         config repo and mirrored from it, never set from a request
//	         (TEAM-M6-03), so the invite's role is answered for the device that
//	         admits the person's first device, which signs it. The invite is
//	         spent in the same transaction, and owes an admin event
//	switch   POST /auth/network-switch {network_id} (network_invite_controller.go)
//
// A refusal never repeats the bearer or the invite's email. A device's join is
// MACed with the fragment's one-time secret, which only devices hold; api never
// sees it, and refuses no device here: an ephemeral or read_only first device
// carries no invite's consent (§8.5's invited clause, QD-072), which every
// reader judges.
//
// Nothing here reads or writes network_invite unless the binary links MG6.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Tunables of invitations.
type NetworkInviteSettings struct {
	// The longest an invite lives, in days: 30 (§7.5's expire_days <= 30). An
	// invite lives at least one day.
	MaxExpireDays int
}

func DefaultNetworkInviteSettings() *NetworkInviteSettings {
	return &NetworkInviteSettings{
		MaxExpireDays: 30,
	}
}

// The bearer's format values: ur-invite-<invite_id>-<secret>, each part
// lowercase RFC 4648 base32 with no padding, as an auth key's bearer is written.
const (
	networkInviteBearerPrefix = "ur-invite-"
	// the secret's random bytes
	networkInviteSecretByteCount = 32
)

var networkInviteEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// The admin event words of invitations.
const (
	AdminEventTargetMemberInvite       = "member_invite"
	AdminEventActionMemberInviteCreate = "member_invite.create"
)

// The scope an admin token needs to invite: §7.8's OAuth client scopes hold
// none narrower for members, so only all.
const networkInviteAdminScope = "all"

// Refusals, fixed text that names neither the bearer nor the email.
const (
	networkInviteRefusedMessage       = "Only the team's Owners and Admins may invite members."
	networkInviteEmailMessage         = "The invite needs an email address."
	networkInviteRoleMessage          = "The invite needs one of the seven member roles."
	networkInviteInvalidMessage       = "The invite is not valid."
	networkInviteExpiredMessage       = "The invite expired."
	networkInviteOtherEmailMessage    = "The invite is for another email address."
	networkInviteAlreadyMemberMessage = "You are already a member of this team."
	networkInviteCredentialMessage    = "An invite is accepted with a person's own sign-in."
)

func init() {
	// a one-command admin token reaches the route, and its use is spent in the
	// invite's transaction (session.RegisterAdminCommandRoute)
	session.RegisterAdminCommandRoute(http.MethodPost, "/network/member-invite")
}

// What invitations depend on apart from the database, so tests can replace it.
type networkInvites struct {
	settings *NetworkInviteSettings
	now      func() time.Time
	tierOf   func(ctx context.Context, networkId server.Id) NetworkTier
}

func newNetworkInvites() *networkInvites {
	return &networkInvites{
		settings: DefaultNetworkInviteSettings(),
		now:      server.NowUtc,
		tierOf:   NetworkTierOf,
	}
}

// The bearer a person holds for an invite.
func networkInviteBearer(inviteId server.Id, secret []byte) string {
	return networkInviteBearerPrefix + networkInviteEncoding.EncodeToString(inviteId.Bytes()) + "-" + networkInviteEncoding.EncodeToString(secret)
}

// An invite's id and secret from its bearer, given with or without its prefix;
// false for anything else. A decoding that does not encode back is refused.
func parseNetworkInviteBearer(text string) (inviteId server.Id, secret []byte, ok bool) {
	idText, secretText, found := strings.Cut(strings.TrimPrefix(text, networkInviteBearerPrefix), "-")
	if !found {
		return server.Id{}, nil, false
	}
	idBytes, idErr := networkInviteEncoding.DecodeString(idText)
	secretBytes, secretErr := networkInviteEncoding.DecodeString(secretText)
	if idErr != nil || secretErr != nil || len(idBytes) != len(server.Id{}) || len(secretBytes) != networkInviteSecretByteCount ||
		networkInviteEncoding.EncodeToString(idBytes) != idText || networkInviteEncoding.EncodeToString(secretBytes) != secretText {
		return server.Id{}, nil, false
	}
	inviteId, err := server.IdFromBytes(idBytes)
	if err != nil {
		return server.Id{}, nil, false
	}
	return inviteId, secretBytes, true
}

// An email as an invite stores and matches it: trimmed and lowercase, false for
// one with no @ or past the column's width.
func normalizeNetworkInviteEmail(email string) (string, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	local, domain, found := strings.Cut(email, "@")
	if !found || local == "" || domain == "" || strings.ContainsAny(email, " \t\r\n") || networkMemberEmailMaxLength < len(email) {
		return "", false
	}
	return email, true
}

type CreateNetworkMemberInviteArgs struct {
	Email string `json:"email"`
	// one of §7.5's seven, which the admitting device signs in the config repo
	Role NetworkMemberRole `json:"role"`
	// 1 to the settings' MaxExpireDays
	ExpireDays int `json:"expire_days"`
}

type CreateNetworkMemberInviteError struct {
	Message string `json:"message"`
}

// Prints without the bearer.
type CreateNetworkMemberInviteResult struct {
	InviteId *server.Id `json:"invite_id,omitempty"`
	// ur-invite-<invite_id>-<secret>, which this answer alone holds
	InviteBearer string                          `json:"invite_bearer,omitempty"`
	ExpireTime   *time.Time                      `json:"expire_time,omitempty"`
	Error        *CreateNetworkMemberInviteError `json:"error,omitempty"`
}

// Never the bearer.
func (self *CreateNetworkMemberInviteResult) String() string {
	return "CreateNetworkMemberInviteResult{}"
}

// As String, for %#v.
func (self *CreateNetworkMemberInviteResult) GoString() string {
	return self.String()
}

// POST /network/member-invite (see the file header).
func CreateNetworkMemberInvite(args *CreateNetworkMemberInviteArgs, clientSession *session.ClientSession) (*CreateNetworkMemberInviteResult, error) {
	return newNetworkInvites().create(args, clientSession)
}

func (self *networkInvites) create(args *CreateNetworkMemberInviteArgs, clientSession *session.ClientSession) (*CreateNetworkMemberInviteResult, error) {
	if !server.SidecarMg6Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkTeamsUnavailableMessage)
	}
	ctx := clientSession.Ctx
	byJwt := clientSession.ByJwt
	if byJwt == nil || byJwt.ClientId != nil || clientSession.ApiKeyAuthenticated {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkInviteRefusedMessage)
	}
	adminCommand := clientSession.AdminCommand
	if adminCommand != nil && !slices.Contains(adminCommand.Scopes, networkInviteAdminScope) {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, "The admin token does not carry the all scope.")
	}
	if clientSession.UnsignedIdentity && adminCommand == nil {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkInviteRefusedMessage)
	}
	if err := requireTeamTierOf(self.tierOf(ctx, byJwt.NetworkId)); err != nil {
		return nil, err
	}
	refuse := func(message string) (*CreateNetworkMemberInviteResult, error) {
		return &CreateNetworkMemberInviteResult{Error: &CreateNetworkMemberInviteError{Message: message}}, nil
	}
	email, ok := normalizeNetworkInviteEmail(args.Email)
	if !ok {
		return refuse(networkInviteEmailMessage)
	}
	if !args.Role.Valid() {
		return refuse(networkInviteRoleMessage)
	}
	maxExpireDays := max(self.settings.MaxExpireDays, 1)
	if args.ExpireDays < 1 || maxExpireDays < args.ExpireDays {
		return refuse(fmt.Sprintf("An invite lives 1 to %d days.", maxExpireDays))
	}

	secret := make([]byte, networkInviteSecretByteCount)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	secretSha256 := sha256.Sum256(secret)
	now := self.now().UTC().Truncate(time.Microsecond)
	inviteId := server.NewId()
	expireTime := now.Add(time.Duration(args.ExpireDays) * 24 * time.Hour)
	created := &CreateNetworkMemberInviteResult{
		InviteId:     &inviteId,
		InviteBearer: networkInviteBearer(inviteId, secret),
		ExpireTime:   &expireTime,
	}
	clear(secret)

	var answer *CreateNetworkMemberInviteResult
	var refusedErr error
	server.Tx(ctx, func(tx server.PgTx) {
		answer, refusedErr = nil, nil
		if adminCommand != nil {
			storedResult, first, err := ClaimAdminTokenUseInTx(ctx, tx, adminCommand, now)
			server.Raise(err)
			if !first {
				// a replay of the token: the first use's answer, and nothing runs
				replayed := &CreateNetworkMemberInviteResult{}
				server.Raise(json.Unmarshal(storedResult, replayed))
				answer = replayed
				return
			}
		}
		caller := GetNetworkMemberOfUserInTx(ctx, tx, byJwt.NetworkId, byJwt.UserId)
		if caller == nil || caller.Pending || !caller.Role.Permits(RolePermissionMembers) {
			refusedErr = fmt.Errorf("%d %s", http.StatusForbidden, networkInviteRefusedMessage)
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_invite (
					invite_id,
					network_id,
					token_sha256,
					email,
					role,
					expire_time
				)
				VALUES ($1, $2, $3, $4, $5, $6)
			`,
			inviteId,
			byJwt.NetworkId,
			secretSha256[:],
			email,
			string(args.Role),
			expireTime,
		))
		event := NewSessionAdminEvent(clientSession, AdminEventActionMemberInviteCreate, AdminEventTargetMemberInvite, &inviteId)
		AddAdminEventInTx(ctx, tx, event)
		oweSecurityNetworkAuthorityNoticeInTx(ctx, tx, byJwt.NetworkId, &SecurityNetworkAuthorityChange{
			ChangeTime: now,
			Action:     SecurityNetworkAuthorityMemberInvite,
			Via:        securityNoticeVia(ctx),
		})
		if adminCommand != nil {
			resultJson, err := json.Marshal(created)
			server.Raise(err)
			server.Raise(StoreAdminTokenUseResultInTx(ctx, tx, adminCommand.Jti, resultJson))
		}
		answer = created
	})
	if refusedErr != nil {
		return nil, refusedErr
	}
	return answer, nil
}

type AcceptNetworkMemberInviteArgs struct {
	// ur-invite-<invite_id>-<secret>, never its #fragment
	InviteBearer string `json:"invite_bearer"`
}

type AcceptNetworkMemberInviteError struct {
	Message string `json:"message"`
}

type AcceptNetworkMemberInviteResult struct {
	NetworkId *server.Id `json:"network_id,omitempty"`
	MemberId  *server.Id `json:"member_id,omitempty"`
	// the role the invite names, which the device that admits the member's
	// first device signs; the membership holds member until the config repo
	// gives it
	InvitedRole NetworkMemberRole               `json:"invited_role,omitempty"`
	Error       *AcceptNetworkMemberInviteError `json:"error,omitempty"`
}

// POST /network/member-invite-accept (see the file header).
func AcceptNetworkMemberInvite(args *AcceptNetworkMemberInviteArgs, clientSession *session.ClientSession) (*AcceptNetworkMemberInviteResult, error) {
	return newNetworkInvites().accept(args, clientSession)
}

// An invite as network_invite holds it.
type networkInviteRow struct {
	networkId   server.Id
	tokenSha256 []byte
	email       string
	role        NetworkMemberRole
	expireTime  time.Time
}

func (self *networkInvites) accept(args *AcceptNetworkMemberInviteArgs, clientSession *session.ClientSession) (*AcceptNetworkMemberInviteResult, error) {
	if !server.SidecarMg6Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkTeamsUnavailableMessage)
	}
	ctx := clientSession.Ctx
	byJwt := clientSession.ByJwt
	if byJwt == nil || byJwt.ClientId != nil || clientSession.ApiKeyAuthenticated || clientSession.UnsignedIdentity {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkInviteCredentialMessage)
	}
	refuse := func(message string) (*AcceptNetworkMemberInviteResult, error) {
		return &AcceptNetworkMemberInviteResult{Error: &AcceptNetworkMemberInviteError{Message: message}}, nil
	}
	inviteId, secret, ok := parseNetworkInviteBearer(args.InviteBearer)
	if !ok {
		return refuse(networkInviteInvalidMessage)
	}
	secretSha256 := sha256.Sum256(secret)
	clear(secret)
	now := self.now().UTC()

	var answer *AcceptNetworkMemberInviteResult
	var answerErr error
	server.Tx(ctx, func(tx server.PgTx) {
		answer, answerErr = nil, nil
		var invite *networkInviteRow
		result, err := tx.Query(
			ctx,
			`
				SELECT network_id, token_sha256, email, role, expire_time
				FROM network_invite
				WHERE invite_id = $1
				FOR UPDATE
			`,
			inviteId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				invite = &networkInviteRow{}
				var role string
				server.Raise(result.Scan(&invite.networkId, &invite.tokenSha256, &invite.email, &role, &invite.expireTime))
				invite.role = NetworkMemberRole(role)
			}
		})
		if invite == nil || subtle.ConstantTimeCompare(invite.tokenSha256, secretSha256[:]) != 1 {
			answer = &AcceptNetworkMemberInviteResult{Error: &AcceptNetworkMemberInviteError{Message: networkInviteInvalidMessage}}
			return
		}
		if !now.Before(invite.expireTime) {
			// an expired invite is gone for good
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_invite WHERE invite_id = $1`, inviteId))
			answer = &AcceptNetworkMemberInviteResult{Error: &AcceptNetworkMemberInviteError{Message: networkInviteExpiredMessage}}
			return
		}
		if !networkInviteEmailOfUserInTx(ctx, tx, byJwt.UserId, invite.email) {
			answer = &AcceptNetworkMemberInviteResult{Error: &AcceptNetworkMemberInviteError{Message: networkInviteOtherEmailMessage}}
			return
		}
		if err := requireTeamTierOf(self.tierOf(ctx, invite.networkId)); err != nil {
			answerErr = err
			return
		}
		team := GetNetworkTeamInTx(ctx, tx, invite.networkId)
		if team == nil {
			answer = &AcceptNetworkMemberInviteResult{Error: &AcceptNetworkMemberInviteError{Message: networkInviteInvalidMessage}}
			return
		}
		email := invite.email
		member, err := AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{
			NetworkId: invite.networkId,
			UserId:    byJwt.UserId,
			Email:     &email,
			Role:      NetworkMemberRoleMember,
		})
		if errors.Is(err, ErrNetworkMemberExists) {
			answer = &AcceptNetworkMemberInviteResult{Error: &AcceptNetworkMemberInviteError{Message: networkInviteAlreadyMemberMessage}}
			return
		}
		server.Raise(err)
		// spent: one membership per invite
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_invite WHERE invite_id = $1`, inviteId))

		memberId := member.MemberId
		actorId := byJwt.UserId
		event := &AdminEvent{
			NetworkId:  invite.networkId,
			ActorKind:  AdminEventActorUser,
			ActorId:    &actorId,
			Action:     AdminEventActionMemberAdd,
			TargetKind: AdminEventTargetMember,
			TargetId:   &memberId,
			Result:     AdminEventResultOk,
		}
		if requestId, ok := session.RequestIdFromContext(ctx); ok {
			event.RequestId = &requestId
		}
		AddAdminEventInTx(ctx, tx, event)

		networkId := invite.networkId
		answer = &AcceptNetworkMemberInviteResult{
			NetworkId:   &networkId,
			MemberId:    &memberId,
			InvitedRole: invite.role,
		}
	})
	if answerErr != nil {
		return nil, answerErr
	}
	return answer, nil
}

// Whether one of the user's verified sign-in emails is email, already
// normalized: a verified email password sign-in or an IdP identity's email.
func networkInviteEmailOfUserInTx(ctx context.Context, tx server.PgTx, userId server.Id, email string) bool {
	found := false
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT EXISTS (
				SELECT 1
				FROM network_user_auth_password
				WHERE user_id = $1 AND auth_type = $2 AND verified AND lower(user_auth) = $3
				UNION ALL
				SELECT 1
				FROM network_user_auth_sso
				WHERE user_id = $1 AND lower(user_auth) = $3
			)
		`,
		userId,
		string(UserAuthTypeEmail),
		email,
	).Scan(&found))
	return found
}
