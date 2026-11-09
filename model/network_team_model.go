package model

// §7.5's team networks (R-TEAM-2, D-TEAM-2, Q-TEAM-2, Q-TEAM-7; §2.8 api-2): a
// team is a network of its own, beside its members' personal networks, which
// POST /network/team-create makes. A personal network is never converted, so
// team-create always makes a new network (owner decision 55).
//
// The new network's admin_user_id is a synthetic owner of record: a network_user
// made with the network, with no sign-in method, which never authenticates. Its
// row holds no user_auth, password, wallet or seed phrase, no sign-in table holds
// a row for it, and its credential_change_time lies past any token's creation
// time, so the state check that every api and connect credential passes refuses
// a token naming it whatever path minted one. admin_user_id never changes
// (D-TEAM-2): the refusal cache and every check keyed on it stay sound, while the
// humans, from the caller made first Owner on, are network_member rows that a
// removal can delete, so every human is removable.
//
// A team holds network_team, kept for the network's life, and the server holds
// only its users, emails and roles; groups, tags, device names and admissions
// live in the config repo (owner decision 46).
//
// §7.5's Team routes (members, invitations, IdPs, domains, SCIM, OAuth clients,
// admin events and audit webhooks) refuse a network outside the team tier with a
// QuotaRefusal whose upgrade is set (RequireTeamTier). team-create is not one: it
// makes the network that POST /network/team-checkout then buys the tier for
// (§20.5's Becoming Team).
//
// Nothing here reads or writes the Team tables unless the binary links MG6
// (server.SidecarMg6Linked); until then each route answers 503.

import (
	"context"
	"fmt"
	"net/http"
	"time"

	goaway "github.com/TwiN/go-away"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Tunables of team networks.
type NetworkTeamSettings struct {
	// The teams one user may create in CreateWindow. Each is a network with its
	// owner of record and its first Owner's membership, kept for the network's
	// life, so creation spends a budget as a name change does. Non-positive
	// creates none.
	CreateLimit int
	// The trailing window CreateLimit counts over. Attempts are kept
	// AccountActionAttemptRetention, twice the longest window that reads them,
	// so this is at most 24 hours.
	CreateWindow time.Duration
	// The most members one GET /network/members answers. Non-positive answers
	// one.
	MemberReadLimit int
}

func DefaultNetworkTeamSettings() *NetworkTeamSettings {
	return &NetworkTeamSettings{
		CreateLimit:     5,
		CreateWindow:    24 * time.Hour,
		MemberReadLimit: 1000,
	}
}

// network_user.auth_type of a team's synthetic owner of record. No sign-in path
// looks a user up by it: they look users up by a sign-in, which this user has
// none of.
const AuthTypeTeamOwnerOfRecord AuthType = "owner_of_record"

// network_user.user_name of a team's owner of record.
const teamOwnerOfRecordUserName = "owner-of-record"

// network_user.credential_change_time of a team's owner of record: after the
// creation time any token can carry, so the state check refuses every token that
// names the user (session.ValidateByJwtState). A timestamp, not infinity, which
// scans into no time.Time.
var teamOwnerOfRecordCredentialChangeTime = time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)

// QuotaRefusal.limit_key of a route only the team tier includes (§20.7's
// "members, SSO" row): no pro.yml key, since a tier, not a count, lifts it.
const TeamTierLimitKey = "team"

// The account action budget team-create spends.
const AccountActionTeamCreate = "team_create"

// What the team routes depend on apart from the database, so tests can replace
// it.
type networkTeams struct {
	settings       *NetworkTeamSettings
	renameSettings *NetworkRenameSettings
	now            func() time.Time
	tierOf         func(ctx context.Context, networkId server.Id) NetworkTier
	labelHolder    NetworkLabelHolder
	screen         func(name string) error
}

func newNetworkTeams() *networkTeams {
	return &networkTeams{
		settings:       DefaultNetworkTeamSettings(),
		renameSettings: DefaultNetworkRenameSettings(),
		now:            server.NowUtc,
		tierOf:         NetworkTierOf,
		labelHolder:    defaultNetworkLabelHolder(),
		screen:         screenRenamedNetworkName,
	}
}

// Nil when the network is on the team tier, else the refusal every Team route
// answers (§7.5): a *QuotaRefusalError naming TeamTierLimitKey with upgrade set,
// which the router answers 402 with the QuotaRefusal as its body. Reads the tier
// through NetworkTierOf's caches.
func RequireTeamTier(ctx context.Context, networkId server.Id) error {
	return requireTeamTierOf(NetworkTierOf(ctx, networkId))
}

// RequireTeamTier for a caller holding a transaction, through NetworkTierOfInTx.
func RequireTeamTierInTx(ctx context.Context, tx server.PgTx, networkId server.Id) error {
	return requireTeamTierOf(NetworkTierOfInTx(ctx, tx, networkId))
}

func (self *networkTeams) requireTeamTier(ctx context.Context, networkId server.Id) error {
	return requireTeamTierOf(self.tierOf(ctx, networkId))
}

func requireTeamTierOf(tier NetworkTier) error {
	if tier == NetworkTierTeam {
		return nil
	}
	return NewQuotaRefusalError(&sidecar.QuotaRefusal{
		LimitKey: TeamTierLimitKey,
		Upgrade:  true,
	})
}

// A team as network_team holds it.
type NetworkTeam struct {
	NetworkId server.Id
	// §7.6: the team refuses every sign-in but its IdP's
	SsoRequired bool
}

// The network's team, nil for a network team-create did not make or while the
// binary does not link MG6.
func GetNetworkTeam(ctx context.Context, networkId server.Id) (team *NetworkTeam) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		team = getNetworkTeamWithQuery(ctx, conn, networkId)
	})
	return
}

// GetNetworkTeam in the caller's transaction.
func GetNetworkTeamInTx(ctx context.Context, tx server.PgTx, networkId server.Id) *NetworkTeam {
	if !server.SidecarMg6Linked() {
		return nil
	}
	return getNetworkTeamWithQuery(ctx, tx, networkId)
}

func getNetworkTeamWithQuery(ctx context.Context, query server.PgCanQuery, networkId server.Id) (team *NetworkTeam) {
	result, err := query.Query(
		ctx,
		`SELECT network_id, sso_required FROM network_team WHERE network_id = $1`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			team = &NetworkTeam{}
			server.Raise(result.Scan(&team.NetworkId, &team.SsoRequired))
		}
	})
	return
}

type CreateNetworkTeamArgs struct {
	NetworkName string `json:"network_name"`
}

// Why team-create refused.
const (
	// the name fails ValidateNetworkName or §12.5's label rules
	CreateNetworkTeamRefusalInvalid = "invalid"
	// another network, a reclaim tombstone or dns_label holds the name, or
	// another network's name is within the near distance
	CreateNetworkTeamRefusalNotAvailable = "not_available"
	// the caller's creation budget is spent
	CreateNetworkTeamRefusalRateLimited = "rate_limited"
)

type CreateNetworkTeamError struct {
	// one of CreateNetworkTeamRefusal*
	Code    string `json:"code"`
	Message string `json:"message"`
}

type CreateNetworkTeamResult struct {
	// the team's network
	NetworkId   *server.Id `json:"network_id,omitempty"`
	NetworkName string     `json:"network_name,omitempty"`
	// the caller's membership, its first Owner
	MemberId *server.Id              `json:"member_id,omitempty"`
	Role     NetworkMemberRole       `json:"role,omitempty"`
	Error    *CreateNetworkTeamError `json:"error,omitempty"`
}

// The refusal of team-create to a credential that is no person's.
const createNetworkTeamClientMessage = "A client credential cannot create a team."

// POST /network/team-create: makes a team network named network_name, with a
// synthetic owner of record as its admin_user_id and the caller its first Owner,
// in one transaction with its network_team row, two admin events in the new
// network's chain and one attempt of the caller's creation budget. The name
// meets a rename's rules (network_rename_model.go). The caller's own network is
// unchanged. No token for the team is minted here: a member's token in the team
// is TEAM-M6-02's and POST /auth/network-switch TEAM-M6-04's.
func CreateNetworkTeam(args *CreateNetworkTeamArgs, clientSession *session.ClientSession) (*CreateNetworkTeamResult, error) {
	return newNetworkTeams().create(args, clientSession)
}

func (self *networkTeams) create(args *CreateNetworkTeamArgs, clientSession *session.ClientSession) (*CreateNetworkTeamResult, error) {
	if !server.SidecarMg6Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkTeamsUnavailableMessage)
	}
	ctx := clientSession.Ctx
	byJwt := clientSession.ByJwt
	// the router refuses client tokens on this Network route; this holds without it
	if byJwt == nil || byJwt.ClientId != nil {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, createNetworkTeamClientMessage)
	}
	userId := byJwt.UserId
	refuse := func(code string, message string) (*CreateNetworkTeamResult, error) {
		return &CreateNetworkTeamResult{
			Error: &CreateNetworkTeamError{
				Code:    code,
				Message: message,
			},
		}, nil
	}

	name, err := ValidateNetworkName(args.NetworkName)
	if err != nil {
		return refuse(CreateNetworkTeamRefusalInvalid, err.Error())
	}
	if err := self.screen(name); err != nil {
		return refuse(CreateNetworkTeamRefusalInvalid, err.Error())
	}
	// a name within the near distance of any network's is refused, as at a rename
	if 0 < len(networkNameSearch().AroundIds(ctx, name, self.renameSettings.NearDistance)) {
		return refuse(CreateNetworkTeamRefusalNotAvailable, renameNetworkNotAvailableMessage)
	}
	containsProfanity := goaway.IsProfane(name)

	var networkId server.Id
	var member *NetworkMember
	refusal := ""
	var retryAfter time.Duration
	var posts []server.PostFunction
	taken := NetworkNameTx(ctx, func(tx server.PgTx) {
		// a rerun starts over
		refusal = ""
		retryAfter = 0
		posts = nil
		member = nil
		now := self.now()

		// availability before the budget, so a taken name costs no attempt
		if self.nameTakenInTx(ctx, tx, name, now) {
			refusal = CreateNetworkTeamRefusalNotAvailable
			return
		}
		allow, wait := readAccountActionBudgetInTx(ctx, tx, userId, AccountActionTeamCreate, self.settings.CreateLimit, self.settings.CreateWindow, now)
		if !allow {
			refusal = CreateNetworkTeamRefusalRateLimited
			retryAfter = wait
			return
		}
		insertAccountActionAttemptInTx(ctx, tx, userId, AccountActionTeamCreate, now)

		networkId = server.NewId()
		ownerOfRecordUserId := server.NewId()
		// the owner of record: no user_auth, password, wallet or seed phrase, and
		// no sign-in table row
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_user (user_id, user_name, auth_type, credential_change_time)
				VALUES ($1, $2, $3, $4)
			`,
			ownerOfRecordUserId,
			teamOwnerOfRecordUserName,
			AuthTypeTeamOwnerOfRecord,
			teamOwnerOfRecordCredentialChangeTime,
		))
		RaiseNetworkNameWrite(tx.Exec(
			ctx,
			`
				INSERT INTO network (network_id, network_name, admin_user_id, contains_profanity)
				VALUES ($1, $2, $3, $4)
			`,
			networkId,
			name,
			ownerOfRecordUserId,
			containsProfanity,
		))
		CreateNetworkReferralCodeInTx(ctx, tx, networkId)
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_team (network_id) VALUES ($1)`,
			networkId,
		))

		var err error
		member, err = AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{
			NetworkId: networkId,
			UserId:    userId,
			Email:     networkMemberEmailOfUserInTx(ctx, tx, userId),
			Role:      NetworkMemberRoleOwner,
		})
		server.Raise(err)

		teamEvent := NewSessionAdminEvent(clientSession, AdminEventActionTeamCreate, AdminEventTargetNetwork, &networkId)
		teamEvent.NetworkId = networkId
		addAdminEventInTxAt(ctx, tx, teamEvent, now)
		memberEvent := NewSessionAdminEvent(clientSession, AdminEventActionMemberAdd, AdminEventTargetMember, &member.MemberId)
		memberEvent.NetworkId = networkId
		addAdminEventInTxAt(ctx, tx, memberEvent, now)

		posts = append(posts, IndexNetworkNameInTx(ctx, tx, networkId, name))
	})
	switch {
	case taken || refusal == CreateNetworkTeamRefusalNotAvailable:
		return refuse(CreateNetworkTeamRefusalNotAvailable, renameNetworkNotAvailableMessage)
	case refusal == CreateNetworkTeamRefusalRateLimited:
		return refuse(CreateNetworkTeamRefusalRateLimited, maxAccountActionAttemptsRetryError(AccountActionTeamCreate, self.settings.CreateWindow, retryAfter).Error())
	}
	// the team committed
	server.RunPosts(ctx, posts...)
	// the networks statistics replay created and deleted events, and a team is a
	// network; the deletion writes its event as any network's does
	auditNetworkEvent := NewAuditNetworkEvent(AuditEventTypeNetworkCreated)
	auditNetworkEvent.NetworkId = networkId
	AddAuditEvent(ctx, auditNetworkEvent)

	memberId := member.MemberId
	return &CreateNetworkTeamResult{
		NetworkId:   &networkId,
		NetworkName: name,
		MemberId:    &memberId,
		Role:        member.Role,
	}, nil
}

// Whether a network create may not take the name at now: a network holds it, a
// reclaim tombstone holds it, or dns_label holds it.
func (self *networkTeams) nameTakenInTx(ctx context.Context, tx server.PgTx, name string, now time.Time) bool {
	return networkNameHeldInTx(ctx, tx, name) ||
		networkNameReclaimHeldInTx(ctx, tx, name, now) ||
		self.labelHolder.HoldsNameInTx(ctx, tx, name)
}

// A team network's Team rows go with it, in the caller's transaction: its
// network_team row, members, invitations, IdPs with their identities, domains,
// OAuth clients, SCIM changes and webhooks. Its owner of record goes as every
// network's admin user does. A no-op while the binary does not link MG6.
func removeNetworkTeamInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	if !server.SidecarMg6Linked() {
		return
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM network_idp_identity
			USING network_idp
			WHERE network_idp.network_id = $1 AND network_idp_identity.idp_id = network_idp.idp_id
		`,
		networkId,
	))
	for _, table := range []string{
		"network_idp",
		"network_member",
		"network_invite",
		"network_domain",
		"network_oauth_client",
		"network_scim_change",
		"network_audit_webhook",
		"network_team",
	} {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM `+table+` WHERE network_id = $1`, networkId))
	}
}
