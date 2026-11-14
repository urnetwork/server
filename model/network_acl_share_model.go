package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// §8.11's cross-network shares at api (ACL-M6-03; §2.8 api-9, §8.17's
// network_acl_share): the invites that bind a share's two networks, and the
// contract rule that settles a covered pair as FriendsAndFamily.
//
// An invite is X's: POST /acl/invite files the SHA-256 of a 256-bit code the
// inviting sidecar drew, never the code, with a use count (`ur bridge invite
// --count`) and a window of at most 7 days, and in the same transaction the
// share's row (network_bridge_share) with X's targets sealed under K, which the
// code derives and api, holding only its hash, cannot (§17.9's share seal).
//
// Accepting is Y's, in two calls to POST /acl/accept-invite, since Y must open
// the target table and plan its aliases under the share's id before it can file
// them (DESIGN-DETAILS 12.2.7; question acl-m6-03-accept-invite-claim):
//
//	claim     the code's hash alone: one use taken, and Y bound to a share as
//	          its invited network, the invite's own row for its first claim and
//	          a new row holding the same target table for each later one; the
//	          answer is the share, X's sealed targets with it. A claim Y repeats
//	          answers the share it holds and takes no use. The invite's row goes
//	          when its last use is taken, and is refused and goes once its
//	          window closes.
//	complete  the share's id with Y's alias table sealed under K and the accept
//	          status §17.9's preflight reached: stored once, for the share Y
//	          claimed; a retry of the same bytes answers as the first did, and
//	          anything later is BRIDGE's PUT at a higher version.
//
// Neither call grants anything. Each network activates the share by a
// GrantActivate in its own log, which an admin approves (owner decision 74),
// and the contract gate reads only those projections (AclShareBetween), never
// these rows, which go long before a share ends.
//
// Every route refuses a bearer that is no sidecar of its network, and
// acl_network_shares refuses a network whose plan does not include shares,
// X's at its invite and Y's at its claim (§20.4). The table belongs to MG6
// (server.SidecarMg6Linked): with it unlinked every function here refuses and
// touches nothing.

// Tunables of the invites.
type NetworkAclShareSettings struct {
	// The longest an invite stays open, and the window of one that names none:
	// §8.11's 7 days. A window past it is refused.
	WindowMax time.Duration
	// The most networks one code may bind (`--count`). A reading: §8.11 names
	// no bound, and each use may add a share row as large as its target table.
	UseMaxCount int
	// The rows one sweep of closed windows removes in a transaction.
	SweepBatchSize int
	// How long one sweep may run before it yields to the next.
	SweepRunBudget time.Duration
}

func DefaultNetworkAclShareSettings() *NetworkAclShareSettings {
	return &NetworkAclShareSettings{
		WindowMax:      7 * 24 * time.Hour,
		UseMaxCount:    1000,
		SweepBatchSize: 10000,
		SweepRunBudget: 30 * time.Second,
	}
}

// The limit key of §20.4's tier row that includes shares.
const networkAclShareLimitKey = "acl_network_shares"

var (
	// MG6 is not linked: no invite table exists yet.
	ErrNetworkAclShareUnavailable = errors.New("Network shares are not available yet.")
	// A code hash that is not 32 bytes, a use count or window out of range, a
	// code already filed, or a network accepting its own invite.
	ErrNetworkAclShareInvalid = errors.New("Invalid network share invite.")
	// No such invite: never filed, its uses run out, or its window closed.
	ErrNetworkAclShareNotFound = errors.New("No such network share invite.")
	// The network's plan does not include network shares (§20.4).
	ErrNetworkAclShareNotIncluded = errors.New("This network's plan does not include network shares.")
	// The share is accepted already, with other tables than the ones posted.
	ErrNetworkAclShareAccepted = errors.New("This network share is accepted already.")
)

// One invite's row.
type NetworkAclInvite struct {
	// SHA-256 of the invite's code.
	CodeHash []byte
	// X, the inviting network.
	NetworkId server.Id
	// The share the invite filed, with X's target table: the first claim's.
	ShareId server.Id
	// The uses left.
	UseCount   int
	CreateTime time.Time
	ExpireTime time.Time
}

// ErrNetworkAclShareNotIncluded when sidecar limits are enforced and the
// network's plan leaves acl_network_shares off.
func checkNetworkAclShareIncluded(ctx context.Context, networkId server.Id) error {
	if !Pro().EnforceSidecarLimits {
		return nil
	}
	if NetworkSidecarLimits(ctx, networkId).Limit(networkAclShareLimitKey) == 0 {
		return ErrNetworkAclShareNotIncluded
	}
	return nil
}

// X's invite: its row and, in the same transaction, the share's with X's sealed
// targets (CreateNetworkBridgeShareInTx, which also asks §21.7's standing). A
// window of 0 takes WindowMax. A code hash already filed is refused, so a code is
// never reused.
func CreateNetworkAclInvite(
	ctx context.Context,
	networkId server.Id,
	codeHash []byte,
	useCount int,
	window time.Duration,
	targetTable []byte,
	targetTableVersion int64,
	now time.Time,
) (*NetworkAclInvite, error) {
	if !server.SidecarMg6Linked() {
		return nil, ErrNetworkAclShareUnavailable
	}
	settings := DefaultNetworkAclShareSettings()
	if window == 0 {
		window = settings.WindowMax
	}
	if len(codeHash) != sha256.Size || useCount < 1 || settings.UseMaxCount < useCount || window < 0 || settings.WindowMax < window {
		return nil, ErrNetworkAclShareInvalid
	}
	if err := checkNetworkAclShareIncluded(ctx, networkId); err != nil {
		return nil, err
	}
	// what the share's own insert refuses, refused before the transaction
	if err := checkNetworkBridgeShareTable(targetTable, targetTableVersion); err != nil {
		return nil, err
	}
	if err := checkNetworkBridgeShareOutbound(ctx, networkId); err != nil {
		return nil, err
	}
	invite := &NetworkAclInvite{
		CodeHash:   bytes.Clone(codeHash),
		NetworkId:  networkId,
		ShareId:    server.NewId(),
		UseCount:   useCount,
		CreateTime: now.UTC(),
		ExpireTime: now.UTC().Add(window),
	}
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_acl_share (
					code_hash,
					network_id,
					share_id,
					use_count,
					create_time,
					expire_time
				)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (code_hash) DO NOTHING
			`,
			invite.CodeHash,
			invite.NetworkId,
			invite.ShareId,
			invite.UseCount,
			invite.CreateTime,
			invite.ExpireTime,
		))
		if tag.RowsAffected() != 1 {
			// a code filed before: nothing is written
			returnErr = ErrNetworkAclShareInvalid
			return
		}
		// the share's insert writes only once it passes its checks, so a refusal
		// leaves the invite's row alone to take back, and the commit writes
		// nothing
		if err := CreateNetworkBridgeShareInTx(ctx, tx, invite.ShareId, networkId, targetTable, targetTableVersion); err != nil {
			removeNetworkAclInviteInTx(ctx, tx, invite.CodeHash)
			returnErr = err
			return
		}
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return invite, nil
}

// The invite's row, or nil.
func getNetworkAclInvite(ctx context.Context, conn server.PgCanQuery, codeHash []byte, forUpdate bool) *NetworkAclInvite {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	var invite *NetworkAclInvite
	result, err := conn.Query(
		ctx,
		`
			SELECT
				network_id,
				share_id,
				use_count,
				create_time,
				expire_time
			FROM network_acl_share
			WHERE code_hash = $1`+lock,
		codeHash,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			invite = &NetworkAclInvite{CodeHash: bytes.Clone(codeHash)}
			server.Raise(result.Scan(
				&invite.NetworkId,
				&invite.ShareId,
				&invite.UseCount,
				&invite.CreateTime,
				&invite.ExpireTime,
			))
		}
	})
	return invite
}

// The invite's row, or nil. For tests and for a sidecar that filed it.
func GetNetworkAclInvite(ctx context.Context, codeHash []byte) *NetworkAclInvite {
	if !server.SidecarMg6Linked() {
		return nil
	}
	var invite *NetworkAclInvite
	server.Db(ctx, func(conn server.PgConn) {
		invite = getNetworkAclInvite(ctx, conn, codeHash, false)
	})
	return invite
}

// Removes the invite's row in tx.
func removeNetworkAclInviteInTx(ctx context.Context, tx server.PgTx, codeHash []byte) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM network_acl_share
			WHERE code_hash = $1
		`,
		codeHash,
	))
}

// Y's claim (see the file header): one use of a live invite taken and Y bound
// to a share, or the share Y holds of it already with no use taken. The answer
// is the share's row, X's sealed targets in it.
func ClaimNetworkAclInvite(ctx context.Context, invitedNetworkId server.Id, codeHash []byte, now time.Time) (*NetworkBridgeShare, error) {
	if !server.SidecarMg6Linked() {
		return nil, ErrNetworkAclShareUnavailable
	}
	if len(codeHash) != sha256.Size {
		return nil, ErrNetworkAclShareInvalid
	}
	// the checks that read another store run before the transaction, on the
	// invite as it reads now, and the transaction reads it again
	invite := GetNetworkAclInvite(ctx, codeHash)
	if invite == nil {
		return nil, ErrNetworkAclShareNotFound
	}
	if invite.NetworkId == invitedNetworkId {
		return nil, ErrNetworkAclShareInvalid
	}
	if err := checkNetworkAclShareIncluded(ctx, invitedNetworkId); err != nil {
		return nil, err
	}
	// a share is X's outbound share wherever Y binds it: §21.7's standing again
	if err := checkNetworkBridgeShareOutbound(ctx, invite.NetworkId); err != nil {
		return nil, err
	}

	var shareId server.Id
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		invite := getNetworkAclInvite(ctx, tx, codeHash, true)
		if invite == nil {
			returnErr = ErrNetworkAclShareNotFound
			return
		}
		if !now.Before(invite.ExpireTime) {
			removeNetworkAclInviteInTx(ctx, tx, codeHash)
			returnErr = ErrNetworkAclShareNotFound
			return
		}
		if invite.NetworkId == invitedNetworkId {
			returnErr = ErrNetworkAclShareInvalid
			return
		}
		var template *NetworkBridgeShare
		result, err := tx.Query(
			ctx,
			`
				SELECT
					invited_network_id,
					target_table,
					target_table_version
				FROM network_bridge_share
				WHERE share_id = $1
				FOR UPDATE
			`,
			invite.ShareId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				template = &NetworkBridgeShare{ShareId: invite.ShareId, NetworkId: invite.NetworkId}
				server.Raise(result.Scan(&template.InvitedNetworkId, &template.TargetTable, &template.TargetTableVersion))
			}
		})
		if template == nil {
			// X revoked the invite's share: nothing is left to bind
			removeNetworkAclInviteInTx(ctx, tx, codeHash)
			returnErr = ErrNetworkAclShareNotFound
			return
		}
		if template.InvitedNetworkId != nil && *template.InvitedNetworkId == invitedNetworkId {
			// a repeat of Y's first claim
			shareId = template.ShareId
			return
		}
		// a repeat of a later claim: the share of the invite's targets Y holds
		var held *server.Id
		result, err = tx.Query(
			ctx,
			`
				SELECT share_id
				FROM network_bridge_share
				WHERE
					network_id = $1 AND
					invited_network_id = $2 AND
					target_table = $3
				LIMIT 1
			`,
			invite.NetworkId,
			invitedNetworkId,
			template.TargetTable,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var heldShareId server.Id
				server.Raise(result.Scan(&heldShareId))
				held = &heldShareId
			}
		})
		if held != nil {
			shareId = *held
			return
		}
		if invite.UseCount < 1 {
			removeNetworkAclInviteInTx(ctx, tx, codeHash)
			returnErr = ErrNetworkAclShareNotFound
			return
		}
		if template.InvitedNetworkId == nil {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_bridge_share
					SET invited_network_id = $2
					WHERE
						share_id = $1 AND
						invited_network_id IS NULL
				`,
				template.ShareId,
				invitedNetworkId,
			))
			shareId = template.ShareId
		} else {
			// another network holds the first share: Y's is a new row of the
			// same target table, which X sealed for no one network (§17.9)
			shareId = server.NewId()
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_bridge_share (
						share_id,
						network_id,
						invited_network_id,
						target_table,
						target_table_version
					)
					VALUES ($1, $2, $3, $4, $5)
				`,
				shareId,
				invite.NetworkId,
				invitedNetworkId,
				template.TargetTable,
				template.TargetTableVersion,
			))
		}
		if invite.UseCount <= 1 {
			removeNetworkAclInviteInTx(ctx, tx, codeHash)
		} else {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_acl_share
					SET use_count = use_count - 1
					WHERE code_hash = $1
				`,
				codeHash,
			))
		}
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return GetNetworkBridgeShare(ctx, shareId)
}

// Y's completion (see the file header): its alias table and the accept status,
// stored once for the share Y claimed.
func CompleteNetworkAclInvite(
	ctx context.Context,
	invitedNetworkId server.Id,
	shareId server.Id,
	aliasTable []byte,
	aliasTableVersion int64,
	status NetworkBridgeShareAcceptStatus,
) (*NetworkBridgeShare, error) {
	if !server.SidecarMg6Linked() {
		return nil, ErrNetworkAclShareUnavailable
	}
	if err := checkNetworkBridgeShareTable(aliasTable, aliasTableVersion); err != nil {
		return nil, err
	}
	if !status.valid() {
		return nil, ErrNetworkBridgeShareInvalid
	}
	var stored bool
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_bridge_share
				SET
					alias_table = $3,
					alias_table_version = $4,
					accept_status = $5
				WHERE
					share_id = $1 AND
					invited_network_id = $2 AND
					accept_status IS NULL AND
					alias_table_version < $4
			`,
			shareId,
			invitedNetworkId,
			aliasTable,
			aliasTableVersion,
			string(status),
		))
		stored = tag.RowsAffected() == 1
	})
	share, err := GetNetworkBridgeShare(ctx, shareId)
	if err != nil {
		return nil, err
	}
	if share.InvitedNetworkId == nil || *share.InvitedNetworkId != invitedNetworkId {
		return nil, ErrNetworkBridgeShareNotFound
	}
	if stored {
		return share, nil
	}
	// a retry of the completion that stored answers as it did
	if share.AcceptStatus == status && share.AliasTableVersion == aliasTableVersion && bytes.Equal(share.AliasTable, aliasTable) {
		return share, nil
	}
	return nil, ErrNetworkAclShareAccepted
}

const removeExpiredNetworkAclInvitesSql = `
	DELETE FROM network_acl_share
	WHERE code_hash IN (
		SELECT code_hash
		FROM network_acl_share
		WHERE expire_time <= $1
		ORDER BY expire_time
		LIMIT $2
		FOR UPDATE SKIP LOCKED
	) AND expire_time <= $1
`

// Removes the invites whose window closed at or before now, soonest first, for
// at most the sweep's run budget; the next sweep takes the rest of a backlog.
// Nothing without MG6.
func RemoveExpiredNetworkAclInvites(ctx context.Context, now time.Time) (removedCount int64) {
	if !server.SidecarMg6Linked() {
		return 0
	}
	settings := DefaultNetworkAclShareSettings()
	removedCount, _ = removeRetentionBatches(
		ctx,
		RetentionTableNetworkAclShare,
		settings.SweepBatchSize,
		time.Now().Add(settings.SweepRunBudget),
		func(tx server.PgTx) int64 {
			tag := server.RaisePgResult(tx.Exec(ctx, removeExpiredNetworkAclInvitesSql, now.UTC(), settings.SweepBatchSize))
			return tag.RowsAffected()
		},
	)
	return
}

// Removes every invite networkId filed, in the network's deletion transaction.
// The shares those invites bound go with removeNetworkBridgeSharesInTx.
func removeNetworkAclSharesInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM network_acl_share
			WHERE network_id = $1
		`,
		networkId,
	))
}

// §6.10.1 item 4's rule (§8.11, D-ACL-10): a contract settles as
// FriendsAndFamily exactly when its relationship is Public, the destination
// client is a sidecar's own (network_client_owner.strict_exp; never an sdk
// device client, which advertises FriendsAndFamily whenever it provides
// Public), and §8.10's gate found a share covering both subjects in both
// networks' verified projections (sidecar.AclContractVerdict.Shared, acl_share.go):
// the dialer's network granting it network:X, and X's granting network:Y the
// destination. With no gate on there is no share, so the contract stays Public.
func AclShareBetween(details *ProvideRelationshipDetails, verdict sidecar.AclContractVerdict) bool {
	return details.Mode == ProvideModePublic && details.DestinationStrictExp && verdict.Shared()
}

// POST /acl/invite's body (§8.11, C11).
type AclInviteArgs struct {
	// SHA-256 of the invite's 256-bit code, 32 bytes; the code never reaches a
	// server.
	CodeHash []byte `json:"code_hash"`
	// How many networks may accept it (`--count`), at least 1.
	UseCount int `json:"use_count"`
	// How long it stays open, in seconds, at most §8.11's 7 days; 0 takes the
	// 7 days.
	WindowSeconds int64 `json:"window_seconds,omitempty"`
	// X's targets sealed under K, label targets, and their version, 1 at an
	// invite: the target table never changes.
	TargetTable        []byte `json:"target_table"`
	TargetTableVersion int64  `json:"target_table_version"`
}

// POST /acl/invite's answer.
type AclInviteResult struct {
	ShareId    server.Id `json:"share_id"`
	UseCount   int       `json:"use_count"`
	ExpireTime time.Time `json:"expire_time"`
}

// POST /acl/accept-invite's body: a claim names the code's hash alone, and a
// completion the share's id with Y's alias table and the accept status.
type AclAcceptInviteArgs struct {
	CodeHash []byte `json:"code_hash,omitempty"`

	ShareId *server.Id `json:"share_id,omitempty"`
	// Y's aliases sealed under K, label aliases, and their version, 1 at an
	// accept.
	AliasTable        []byte                         `json:"alias_table,omitempty"`
	AliasTableVersion int64                          `json:"alias_table_version,omitempty"`
	AcceptStatus      NetworkBridgeShareAcceptStatus `json:"accept_status,omitempty"`
}

// The status and message an invite error answers with.
func networkAclShareHttpError(err error) error {
	switch {
	case errors.Is(err, ErrNetworkAclShareUnavailable):
		return fmt.Errorf("%d %s", http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, ErrNetworkAclShareNotFound):
		return fmt.Errorf("%d %s", http.StatusNotFound, err.Error())
	case errors.Is(err, ErrNetworkAclShareInvalid):
		return fmt.Errorf("%d %s", http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNetworkAclShareNotIncluded):
		return fmt.Errorf("%d %s", http.StatusForbidden, err.Error())
	case errors.Is(err, ErrNetworkAclShareAccepted):
		return fmt.Errorf("%d %s", http.StatusConflict, err.Error())
	}
	return networkBridgeShareHttpError(err)
}

// POST /acl/invite (§8.11): X's invite, filed by a sidecar of X. Client class,
// on the strict_exp allowlist.
func AclInvite(invite *AclInviteArgs, clientSession *session.ClientSession) (*AclInviteResult, error) {
	if !server.SidecarMg6Linked() {
		return nil, networkAclShareHttpError(ErrNetworkAclShareUnavailable)
	}
	if !networkBridgeShareCallerIsSidecar(clientSession) {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, "Network share invites are a sidecar's.")
	}
	if invite == nil || invite.WindowSeconds < 0 || DefaultNetworkAclShareSettings().WindowMax/time.Second < time.Duration(invite.WindowSeconds) {
		return nil, networkAclShareHttpError(ErrNetworkAclShareInvalid)
	}
	created, err := CreateNetworkAclInvite(
		clientSession.Ctx,
		clientSession.ByJwt.NetworkId,
		invite.CodeHash,
		invite.UseCount,
		time.Duration(invite.WindowSeconds)*time.Second,
		invite.TargetTable,
		invite.TargetTableVersion,
		server.NowUtc(),
	)
	if err != nil {
		return nil, networkAclShareHttpError(err)
	}
	return &AclInviteResult{ShareId: created.ShareId, UseCount: created.UseCount, ExpireTime: created.ExpireTime}, nil
}

// POST /acl/accept-invite (§8.11): Y's claim or completion, by a sidecar of Y.
// Both answer the share as it stands, both tables in it. Client class, on the
// strict_exp allowlist.
func AclAcceptInvite(accept *AclAcceptInviteArgs, clientSession *session.ClientSession) (*NetworkBridgeShareResult, error) {
	if !server.SidecarMg6Linked() {
		return nil, networkAclShareHttpError(ErrNetworkAclShareUnavailable)
	}
	if !networkBridgeShareCallerIsSidecar(clientSession) {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, "Network share invites are a sidecar's.")
	}
	if accept == nil {
		return nil, networkAclShareHttpError(ErrNetworkAclShareInvalid)
	}
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	var share *NetworkBridgeShare
	var err error
	switch {
	case accept.ShareId == nil && len(accept.AliasTable) == 0 && accept.AcceptStatus == "":
		share, err = ClaimNetworkAclInvite(ctx, networkId, accept.CodeHash, server.NowUtc())
	case accept.ShareId != nil:
		share, err = CompleteNetworkAclInvite(ctx, networkId, *accept.ShareId, accept.AliasTable, accept.AliasTableVersion, accept.AcceptStatus)
	default:
		err = ErrNetworkAclShareInvalid
	}
	if err != nil {
		return nil, networkAclShareHttpError(err)
	}
	return share.result(), nil
}
