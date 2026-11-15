package model

// §7.6's SCIM queue (TEAM-M6-07; R-TEAM-4, D-TEAM-25, Q-TEAM-3, I-4; §2.8 api-3): what a team's IdP told the
// server through SCIM that only an admin device can carry out, one network_scim_change row each, kept at most
// ChangeRetention (7 days) from its create_time:
//
//	member_revoke  the IdP deactivated or deleted a member: the server denied it at once
//	               (DenyNetworkMemberInTx), and its MemberRevoke waits for an admin device, or for the
//	               indexer ServiceSettings.scim_signer names when the member is outside the AdminSet
//	               (D-TEAM-25). member_id names it; detail holds its email for the console
//	user_create    the IdP provisioned a person who is no member: the server adds no member from
//	               unsigned SCIM data, so an admin device invites the email (§7.5's invitations).
//	               detail holds userName, emails, externalId and displayName as the IdP sent them
//	group          the IdP created, changed, replaced or deleted a group: groups live in the config repo
//	               (owner decision 46, §7.7's group:<name>@<domain>), so an admin device applies it.
//	               detail holds the operation, the group's id, its displayName and the members it adds,
//	               removes or sets, by SCIM user id
//
// Roles never sync from SCIM (§7.6), and nothing here adds a reader: the server holds the queue, the console lists
// it, and only a signed statement changes the config (owner decisions 5, 46). Reads leave out rows past retention,
// so a row lives at most 7 days whether or not the sweep has run (RemoveExpiredNetworkScimChanges).
//
// Two routes read the queue. GET /network/scim-changes is the console's and ur admin's: the team's Owners and Admins,
// every change. GET /sidecar/scim-changes is the indexer's that ServiceSettings.scim_signer names (QD-829), a
// sidecar-classified route on §7.4's strict_exp allowlist: api answers it only to the client of the sidecar the newest
// projection's ProjectionSettings.scim_signer names, standing as a MemberRevoke it signs must find it, judged by the
// rows api judges statements by (ConfigLogSequencer.CaughtUpState), and only with the queued member_revoke changes of
// the members those rows hold outside the AdminSet, in the first route's form; any other client token is refused 403,
// as is every network credential. The indexer acts on this unsigned word, so a breached server can have it remove
// members outside the AdminSet: a denial only, since no reader is added (I-4), on a designation that is the owner's
// choice (owner decision 150).
//
// Nothing here reads or writes network_scim_change unless the binary links MG6.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Tunables of the SCIM queue.
type NetworkScimSettings struct {
	// How long a queued change lives from its create_time: 7 days (§7.6's "each up to 7 days for an admin device",
	// §2.8 api-3's "SCIM changes at most 7 d"). Reads leave out older rows.
	ChangeRetention time.Duration
	// The most changes GET /network/scim-changes and GET /sidecar/scim-changes answer at a time: 1,000, a team's
	// member list's page. A smaller limit the indexer asks is kept.
	ChangeReadLimit int
}

func DefaultNetworkScimSettings() *NetworkScimSettings {
	return &NetworkScimSettings{
		ChangeRetention: 7 * 24 * time.Hour,
		ChangeReadLimit: 1000,
	}
}

// What a queued change asks an admin device to do.
type NetworkScimChangeKind string

const (
	NetworkScimChangeKindMemberRevoke NetworkScimChangeKind = "member_revoke"
	NetworkScimChangeKindUserCreate   NetworkScimChangeKind = "user_create"
	NetworkScimChangeKindGroup        NetworkScimChangeKind = "group"
)

// The admin event words of SCIM.
const (
	// the team's IdP, by its SCIM bearer
	AdminEventActorScim              = "scim"
	AdminEventTargetScimChange       = "scim_change"
	AdminEventActionScimDeactivate   = "scim.deactivate"
	AdminEventActionScimUserCreate   = "scim.user_create"
	AdminEventActionScimUserWithdraw = "scim.user_withdraw"
	AdminEventActionScimGroup        = "scim.group"
)

// A queued change as network_scim_change holds it.
type NetworkScimChange struct {
	ChangeId  server.Id             `json:"change_id"`
	NetworkId server.Id             `json:"network_id"`
	MemberId  *server.Id            `json:"member_id,omitempty"`
	Kind      NetworkScimChangeKind `json:"kind"`
	// the change as the server normalized it, a NetworkScim*Detail by kind
	Detail     json.RawMessage `json:"detail,omitempty"`
	CreateTime time.Time       `json:"create_time"`
}

// A member_revoke change's detail.
type NetworkScimMemberRevokeDetail struct {
	// the member's email, for the console's list of removals waiting for a signature
	Email *string `json:"email,omitempty"`
	// the member's role when the server denied it: an AdminSet role's removal needs k of n
	Role NetworkMemberRole `json:"role,omitempty"`
}

// A user_create change's detail, as the IdP sent it.
type NetworkScimUserCreateDetail struct {
	UserName    string   `json:"user_name"`
	Emails      []string `json:"emails,omitempty"`
	ExternalId  string   `json:"external_id,omitempty"`
	DisplayName string   `json:"display_name,omitempty"`
}

// A group operation.
type NetworkScimGroupOp string

const (
	NetworkScimGroupOpCreate  NetworkScimGroupOp = "create"
	NetworkScimGroupOpPatch   NetworkScimGroupOp = "patch"
	NetworkScimGroupOpReplace NetworkScimGroupOp = "replace"
	NetworkScimGroupOpDelete  NetworkScimGroupOp = "delete"
)

// A group change's detail. Members are SCIM user ids: a member_id, or a queued user_create's change_id.
type NetworkScimGroupDetail struct {
	Op          NetworkScimGroupOp `json:"op"`
	GroupId     server.Id          `json:"group_id"`
	DisplayName *string            `json:"display_name,omitempty"`
	// create and replace: the group's members
	MemberIds []server.Id `json:"member_ids,omitempty"`
	// patch: the members added and removed
	AddMemberIds    []server.Id `json:"add_member_ids,omitempty"`
	RemoveMemberIds []server.Id `json:"remove_member_ids,omitempty"`
}

// Queues a change in the caller's transaction at now, and answers it. A binary that does not link MG6 refuses.
func AddNetworkScimChangeInTx(ctx context.Context, tx server.PgTx, networkId server.Id, memberId *server.Id, kind NetworkScimChangeKind, detail any, now time.Time) (*NetworkScimChange, error) {
	if !server.SidecarMg6Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkTeamsUnavailableMessage)
	}
	change := &NetworkScimChange{
		ChangeId:   server.NewId(),
		NetworkId:  networkId,
		MemberId:   memberId,
		Kind:       kind,
		CreateTime: now.UTC().Truncate(time.Microsecond),
	}
	if detail != nil {
		detailJson, err := json.Marshal(detail)
		if err != nil {
			return nil, err
		}
		change.Detail = detailJson
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_scim_change (
				change_id,
				network_id,
				member_id,
				kind,
				detail,
				create_time
			)
			VALUES ($1, $2, $3, $4, $5, $6)
		`,
		change.ChangeId,
		change.NetworkId,
		change.MemberId,
		string(change.Kind),
		[]byte(change.Detail),
		change.CreateTime,
	))
	return change, nil
}

const networkScimChangeColumns = `
	change_id,
	network_id,
	member_id,
	kind,
	detail,
	create_time
`

func scanNetworkScimChange(result server.PgResult) *NetworkScimChange {
	change := &NetworkScimChange{}
	var kind string
	var detail []byte
	server.Raise(result.Scan(
		&change.ChangeId,
		&change.NetworkId,
		&change.MemberId,
		&kind,
		&detail,
		&change.CreateTime,
	))
	change.Kind = NetworkScimChangeKind(kind)
	if detail != nil {
		change.Detail = json.RawMessage(detail)
	}
	return change
}

// The network's queued changes of the kinds named, all kinds for none, created at or after minCreateTime, in
// change_id order, in tx.
func GetNetworkScimChangesInTx(ctx context.Context, tx server.PgTx, networkId server.Id, minCreateTime time.Time, kinds ...NetworkScimChangeKind) []*NetworkScimChange {
	changes := []*NetworkScimChange{}
	if !server.SidecarMg6Linked() {
		return changes
	}
	kindTexts := []string{}
	for _, kind := range kinds {
		kindTexts = append(kindTexts, string(kind))
	}
	result, err := tx.Query(
		ctx,
		`
			SELECT `+networkScimChangeColumns+`
			FROM network_scim_change
			WHERE network_id = $1 AND $2 <= create_time AND (cardinality($3::varchar[]) = 0 OR kind = ANY($3::varchar[]))
			ORDER BY change_id
		`,
		networkId,
		minCreateTime.UTC(),
		kindTexts,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			changes = append(changes, scanNetworkScimChange(result))
		}
	})
	return changes
}

// The network's queued change of changeId, created at or after minCreateTime, nil for none, in tx.
func GetNetworkScimChangeInTx(ctx context.Context, tx server.PgTx, networkId server.Id, changeId server.Id, minCreateTime time.Time) (change *NetworkScimChange) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	result, err := tx.Query(
		ctx,
		`
			SELECT `+networkScimChangeColumns+`
			FROM network_scim_change
			WHERE network_id = $1 AND change_id = $2 AND $3 <= create_time
		`,
		networkId,
		changeId,
		minCreateTime.UTC(),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			change = scanNetworkScimChange(result)
		}
	})
	return
}

// Removes a queued change in tx, false for none.
func RemoveNetworkScimChangeInTx(ctx context.Context, tx server.PgTx, networkId server.Id, changeId server.Id) bool {
	if !server.SidecarMg6Linked() {
		return false
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`DELETE FROM network_scim_change WHERE network_id = $1 AND change_id = $2`,
		networkId,
		changeId,
	))
	return tag.RowsAffected() == 1
}

// Deletes the changes created before minCreateTime, everywhere: what a sweep runs with now less ChangeRetention. A
// no-op without MG6.
func RemoveExpiredNetworkScimChanges(ctx context.Context, minCreateTime time.Time) (removedCount int64) {
	if !server.SidecarMg6Linked() {
		return 0
	}
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM network_scim_change WHERE create_time < $1`,
			minCreateTime.UTC(),
		))
		removedCount = tag.RowsAffected()
	})
	return
}

type GetNetworkScimChangesArgs struct {
	// the change_id to read after, in change_id order; nil from the first
	After *server.Id
}

type GetNetworkScimChangesResult struct {
	Changes []*NetworkScimChange `json:"changes"`
	// changes follow the last one: read again after it
	More bool `json:"more,omitempty"`
}

// The scope an admin token needs to read the queue: §7.8's OAuth client scopes hold none narrower for members, so
// only all.
const networkScimChangesAdminScope = "all"

const networkScimChangesRefusedMessage = "Only the team's Owners and Admins may read its SCIM changes."

// What the queue's routes depend on apart from the database, so tests can replace it.
type networkScimChanges struct {
	settings *NetworkScimSettings
	now      func() time.Time
	tierOf   func(ctx context.Context, networkId server.Id) NetworkTier
	// the sidecar whose current client clientId is, false for none (GET /sidecar/scim-changes)
	clientSidecar func(ctx context.Context, networkId server.Id, clientId server.Id) (server.Id, bool)
	// the network's verified state at the log's tip, nil before base 1 (GET /sidecar/scim-changes)
	tipState func(ctx context.Context, networkId server.Id) (*sidecar.ConfigState, error)
}

func newNetworkScimChanges() *networkScimChanges {
	return &networkScimChanges{
		settings:      DefaultNetworkScimSettings(),
		now:           server.NowUtc,
		tierOf:        NetworkTierOf,
		clientSidecar: currentClientSidecar,
		tipState:      configTipState,
	}
}

// The sidecar clientId is the current client of: its binding's sidecar, an identity not revoked, and the binding its
// sidecar's newest generation, so an older generation's token is no longer that sidecar's client (§5.15). False for
// none, and for a failed lookup.
func currentClientSidecar(ctx context.Context, networkId server.Id, clientId server.Id) (server.Id, bool) {
	if _, err := GetClientBindingKey(ctx, clientId); err != nil {
		return server.Id{}, false
	}
	view := GetSidecarIdentityByClient(ctx, networkId, clientId)
	if view == nil || view.Status != SidecarIdentityStatusActive {
		return server.Id{}, false
	}
	return view.SidecarId, true
}

// The network's verified state at the log's tip through this process's sequencer, nil before base 1.
func configTipState(ctx context.Context, networkId server.Id) (*sidecar.ConfigState, error) {
	sequencer, err := CurrentConfigLogSequencer()
	if err != nil {
		return nil, err
	}
	return sequencer.CaughtUpState(ctx, networkId)
}

// GET /network/scim-changes: the team's queued SCIM changes, unexpired, in change_id order, a page at a time, for
// the console and an admin device to carry out. An Owner's or Admin's network credential, or a one-command admin
// token with the all scope; a team on the team tier. A pending member reads nothing.
func GetNetworkScimChanges(args *GetNetworkScimChangesArgs, clientSession *session.ClientSession) (*GetNetworkScimChangesResult, error) {
	return newNetworkScimChanges().get(args, clientSession)
}

func (self *networkScimChanges) get(args *GetNetworkScimChangesArgs, clientSession *session.ClientSession) (*GetNetworkScimChangesResult, error) {
	if !server.SidecarMg6Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkTeamsUnavailableMessage)
	}
	ctx := clientSession.Ctx
	byJwt := clientSession.ByJwt
	if byJwt == nil || byJwt.ClientId != nil || clientSession.ApiKeyAuthenticated {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkScimChangesRefusedMessage)
	}
	adminCommand := clientSession.AdminCommand
	if adminCommand != nil && !slices.Contains(adminCommand.Scopes, networkScimChangesAdminScope) {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, "The admin token does not carry the all scope.")
	}
	if clientSession.UnsignedIdentity && adminCommand == nil {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkScimChangesRefusedMessage)
	}
	if err := requireTeamTierOf(self.tierOf(ctx, byJwt.NetworkId)); err != nil {
		return nil, err
	}
	limit := max(self.settings.ChangeReadLimit, 1)
	minCreateTime := self.now().Add(-self.settings.ChangeRetention)
	now := self.now().UTC().Truncate(time.Microsecond)

	var answer *GetNetworkScimChangesResult
	var refusedErr error
	server.Tx(ctx, func(tx server.PgTx) {
		answer, refusedErr = nil, nil
		caller := GetNetworkMemberOfUserInTx(ctx, tx, byJwt.NetworkId, byJwt.UserId)
		if caller == nil || caller.Pending || !caller.Role.Permits(RolePermissionMembers) {
			refusedErr = fmt.Errorf("%d %s", http.StatusForbidden, networkScimChangesRefusedMessage)
			return
		}
		if adminCommand != nil {
			storedResult, first, err := ClaimAdminTokenUseInTx(ctx, tx, adminCommand, now)
			server.Raise(err)
			if !first {
				replayed := &GetNetworkScimChangesResult{}
				server.Raise(json.Unmarshal(storedResult, replayed))
				answer = replayed
				return
			}
		}
		result := &GetNetworkScimChangesResult{Changes: []*NetworkScimChange{}}
		rows, err := tx.Query(
			ctx,
			`
				SELECT `+networkScimChangeColumns+`
				FROM network_scim_change
				WHERE network_id = $1 AND $2 <= create_time AND ($3::uuid IS NULL OR $3::uuid < change_id)
				ORDER BY change_id
				LIMIT $4
			`,
			byJwt.NetworkId,
			minCreateTime.UTC(),
			args.After,
			limit+1,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				change := scanNetworkScimChange(rows)
				if limit <= len(result.Changes) {
					result.More = true
					continue
				}
				result.Changes = append(result.Changes, change)
			}
		})
		if adminCommand != nil {
			resultJson, err := json.Marshal(result)
			server.Raise(err)
			server.Raise(StoreAdminTokenUseResultInTx(ctx, tx, adminCommand.Jti, resultJson))
		}
		answer = result
	})
	if refusedErr != nil {
		return nil, refusedErr
	}
	return answer, nil
}

type GetSidecarScimChangesArgs struct {
	// the change_id to read after, in change_id order; nil from the first
	After *server.Id
	// the most changes to answer, at most ChangeReadLimit; 0 for ChangeReadLimit
	Limit int
}

const sidecarScimChangesRefusedMessage = "Only the indexer ServiceSettings.scim_signer names reads SCIM's queued removals."

// GET /sidecar/scim-changes (the file header; QD-829): the queued member_revoke changes of the members the network's
// newest rows hold outside the AdminSet, unexpired, in change_id order, a page at a time, in GET /network/scim-changes's
// form, for the indexer ServiceSettings.scim_signer names to sign. Only that sidecar's current client token, the sidecar
// standing; any other client token, a network credential, an API key, an admin or OAuth client token is refused 403.
// No tier is asked: a removal is a denial, which a downgraded team still signs (§20.7's downgrade row).
func GetSidecarScimChanges(args *GetSidecarScimChangesArgs, clientSession *session.ClientSession) (*GetNetworkScimChangesResult, error) {
	return newNetworkScimChanges().getForScimSigner(args, clientSession)
}

func (self *networkScimChanges) getForScimSigner(args *GetSidecarScimChangesArgs, clientSession *session.ClientSession) (*GetNetworkScimChangesResult, error) {
	if !server.SidecarMg6Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkTeamsUnavailableMessage)
	}
	if args == nil {
		args = &GetSidecarScimChangesArgs{}
	}
	refused := fmt.Errorf("%d %s", http.StatusForbidden, sidecarScimChangesRefusedMessage)
	ctx := clientSession.Ctx
	byJwt := clientSession.ByJwt
	// a client's token alone: every network credential names no client, as on every /sidecar route
	if byJwt == nil || byJwt.ClientId == nil || clientSession.ApiKeyAuthenticated || clientSession.UnsignedIdentity {
		return nil, refused
	}
	sidecarId, ok := self.clientSidecar(ctx, byJwt.NetworkId, *byJwt.ClientId)
	if !ok {
		return nil, refused
	}
	// the store's reads first, holding no Postgres connection across them
	state, err := self.tipState(ctx, byJwt.NetworkId)
	if err != nil {
		glog.Infof("[scim]%s: the log's tip did not read for the scim_signer: %s\n", byJwt.NetworkId, err)
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, "The authorization log is unavailable.")
	}
	if state == nil || !state.ScimSignerStanding(sidecar.Id(sidecarId), self.now()) {
		return nil, refused
	}
	result := &GetNetworkScimChangesResult{Changes: []*NetworkScimChange{}}
	memberIds := []server.Id{}
	for _, memberId := range state.ScimSignerMemberIds() {
		memberIds = append(memberIds, server.Id(memberId))
	}
	if len(memberIds) == 0 {
		return result, nil
	}
	limit := self.settings.ChangeReadLimit
	if 0 < args.Limit {
		limit = min(args.Limit, limit)
	}
	limit = max(limit, 1)
	minCreateTime := self.now().Add(-self.settings.ChangeRetention)
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`
				SELECT `+networkScimChangeColumns+`
				FROM network_scim_change
				WHERE
					network_id = $1 AND
					kind = $2 AND
					$3 <= create_time AND
					member_id = ANY($4::uuid[]) AND
					($5::uuid IS NULL OR $5::uuid < change_id)
				ORDER BY change_id
				LIMIT $6
			`,
			byJwt.NetworkId,
			string(NetworkScimChangeKindMemberRevoke),
			minCreateTime.UTC(),
			memberIds,
			args.After,
			limit+1,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				change := scanNetworkScimChange(rows)
				if limit <= len(result.Changes) {
					result.More = true
					continue
				}
				result.Changes = append(result.Changes, change)
			}
		})
	})
	return result, nil
}

func init() {
	// a one-command admin token reads the queue once (`ur admin scim-changes`), its use spent in the read's
	// transaction (session.RegisterAdminCommandRoute)
	session.RegisterAdminCommandRoute(http.MethodGet, "/network/scim-changes")
}

// How many members the team holds, pending ones included, in tx: what SCIM's user list counts.
func CountNetworkMembersInTx(ctx context.Context, tx server.PgTx, networkId server.Id) (count int) {
	if !server.SidecarMg6Linked() {
		return 0
	}
	server.Raise(tx.QueryRow(ctx, `SELECT COUNT(*) FROM network_member WHERE network_id = $1`, networkId).Scan(&count))
	return
}

// The team's members in member_id order from offset, at most limit, in tx: a page of SCIM's user list.
func GetNetworkMembersPageInTx(ctx context.Context, tx server.PgTx, networkId server.Id, offset int, limit int) []*NetworkMember {
	members := []*NetworkMember{}
	if !server.SidecarMg6Linked() || limit <= 0 {
		return members
	}
	result, err := tx.Query(
		ctx,
		`
			SELECT `+networkMemberColumns+`
			FROM network_member
			WHERE network_id = $1
			ORDER BY member_id
			OFFSET $2
			LIMIT $3
		`,
		networkId,
		max(offset, 0),
		limit,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			members = append(members, scanNetworkMember(result))
		}
	})
	return members
}

// The team's members whose email is email, compared lowercase, in tx: SCIM's userName and emails filters.
func GetNetworkMembersByEmailInTx(ctx context.Context, tx server.PgTx, networkId server.Id, email string) []*NetworkMember {
	members := []*NetworkMember{}
	if !server.SidecarMg6Linked() || email == "" {
		return members
	}
	result, err := tx.Query(
		ctx,
		`
			SELECT `+networkMemberColumns+`
			FROM network_member
			WHERE network_id = $1 AND lower(email) = lower($2)
			ORDER BY member_id
		`,
		networkId,
		email,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			members = append(members, scanNetworkMember(result))
		}
	})
	return members
}

// GET /network/scim-changes at a clock and tier a test gives.
func Testing_GetNetworkScimChanges(args *GetNetworkScimChangesArgs, clientSession *session.ClientSession, now func() time.Time, tierOf func(ctx context.Context, networkId server.Id) NetworkTier) (*GetNetworkScimChangesResult, error) {
	return (&networkScimChanges{settings: DefaultNetworkScimSettings(), now: now, tierOf: tierOf}).get(args, clientSession)
}

// GET /sidecar/scim-changes at a clock, a client's sidecar and a log state a test gives; nil for either of the last two
// reads api's own.
func Testing_GetSidecarScimChanges(
	args *GetSidecarScimChangesArgs,
	clientSession *session.ClientSession,
	now func() time.Time,
	clientSidecar func(ctx context.Context, networkId server.Id, clientId server.Id) (server.Id, bool),
	tipState func(ctx context.Context, networkId server.Id) (*sidecar.ConfigState, error),
) (*GetNetworkScimChangesResult, error) {
	changes := newNetworkScimChanges()
	changes.now = now
	if clientSidecar != nil {
		changes.clientSidecar = clientSidecar
	}
	if tipState != nil {
		changes.tipState = tipState
	}
	return changes.getForScimSigner(args, clientSession)
}
