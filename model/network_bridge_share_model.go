package model

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// §17.9's cross-network shares as api holds them (§17.16, §2.8 api-9):
// network_bridge_share, one row per share. The sharing network X files the row
// with its targets table at its invite, and the invited network Y binds itself
// at accept with its alias table and the accept status. Both tables are sealed
// under K, which the invite code derives and the server, holding only the code's
// hash, cannot, so the row holds no prefix, address or key in the clear (I-2):
// api stores, versions and serves bytes. A sidecar of either network reads both
// tables and replaces its own network's at a higher version only, so a replay of
// an older table is refused.
//
// POST /acl/invite and POST /acl/accept-invite are ACL-M6-03's, and their
// handlers call CreateNetworkBridgeShareInTx and AcceptNetworkBridgeShareInTx in
// their own transactions. A share's revocation removes its row
// (RemoveNetworkBridgeShare), and a network's deletion every row naming it on
// either side. The table belongs to MG6 (server.SidecarMg6Linked): with it
// unlinked, every function here refuses and touches nothing.
//
// An outbound share is X's: §21.7 gives a network on probation or restricted
// none (C-20). That standing is ABUSE's to read (model.AbuseStandingLimits), so
// X's filing and X's replacement ask a check that refuses until ABUSE installs
// it (SetNetworkBridgeShareOutboundCheck): a share fails closed rather than
// skipping §21.7.

// network_bridge_share.accept_status (§17.9's status row).
type NetworkBridgeShareAcceptStatus string

const (
	NetworkBridgeShareAcceptStatusActive          NetworkBridgeShareAcceptStatus = "active"
	NetworkBridgeShareAcceptStatusRefusedTooLarge NetworkBridgeShareAcceptStatus = "refused_too_large"
	NetworkBridgeShareAcceptStatusRefusedNoSpace  NetworkBridgeShareAcceptStatus = "refused_no_space"
)

// Whether the status is one §17.9 names.
func (self NetworkBridgeShareAcceptStatus) valid() bool {
	switch self {
	case NetworkBridgeShareAcceptStatusActive,
		NetworkBridgeShareAcceptStatusRefusedTooLarge,
		NetworkBridgeShareAcceptStatusRefusedNoSpace:
		return true
	default:
		return false
	}
}

// Tunables of the share rows.
type NetworkBridgeShareSettings struct {
	// The largest sealed table either side stores: §17.9's targets and aliases,
	// each padded to a multiple of 256 B, with room for a share of many hosts.
	TableMaxByteCount int
}

func DefaultNetworkBridgeShareSettings() *NetworkBridgeShareSettings {
	return &NetworkBridgeShareSettings{
		TableMaxByteCount: 256 * 1024,
	}
}

var (
	// MG6 is not linked: no share table exists yet.
	ErrNetworkBridgeShareUnavailable = errors.New("Bridge shares are not available yet.")
	// No such share, or none the caller's network is a side of.
	ErrNetworkBridgeShareNotFound = errors.New("No such bridge share.")
	// A table at or below the version already stored, a replay included.
	ErrNetworkBridgeShareVersion = errors.New("A bridge share table's version must rise.")
	// An empty or oversized table, a version below 1, an unknown status, a
	// second accept, or a network accepting its own share.
	ErrNetworkBridgeShareInvalid = errors.New("Invalid bridge share.")
	// §21.7: the sharing network's standing gives it no outbound share, or its
	// standing could not be read.
	ErrNetworkBridgeShareOutboundRefused = errors.New("This network may not share outbound.")
)

// Whether a network may share outbound now, by its §21.7 standing. Safe for
// concurrent use.
type NetworkBridgeShareOutboundCheck func(ctx context.Context, networkId server.Id) (allowed bool, err error)

var networkBridgeShareOutboundCheckLock sync.Mutex
var networkBridgeShareOutboundCheck NetworkBridgeShareOutboundCheck

// Installs the standing check X's filings ask, which ABUSE supplies from
// model.AbuseStandingLimits. Until one is installed every outbound filing is
// refused.
func SetNetworkBridgeShareOutboundCheck(check NetworkBridgeShareOutboundCheck) {
	networkBridgeShareOutboundCheckLock.Lock()
	defer networkBridgeShareOutboundCheckLock.Unlock()
	networkBridgeShareOutboundCheck = check
}

// Installs a check for a test and returns what restores the one before.
func Testing_SetNetworkBridgeShareOutboundCheck(check NetworkBridgeShareOutboundCheck) func() {
	networkBridgeShareOutboundCheckLock.Lock()
	defer networkBridgeShareOutboundCheckLock.Unlock()
	previous := networkBridgeShareOutboundCheck
	networkBridgeShareOutboundCheck = check
	return func() {
		SetNetworkBridgeShareOutboundCheck(previous)
	}
}

// ErrNetworkBridgeShareOutboundRefused unless the installed check allows
// networkId to share outbound.
func checkNetworkBridgeShareOutbound(ctx context.Context, networkId server.Id) error {
	networkBridgeShareOutboundCheckLock.Lock()
	check := networkBridgeShareOutboundCheck
	networkBridgeShareOutboundCheckLock.Unlock()
	if check == nil {
		return ErrNetworkBridgeShareOutboundRefused
	}
	allowed, err := check(ctx, networkId)
	if err != nil || !allowed {
		return ErrNetworkBridgeShareOutboundRefused
	}
	return nil
}

// One share's row.
type NetworkBridgeShare struct {
	ShareId server.Id
	// X: the network whose targets are shared.
	NetworkId server.Id
	// Y, once it accepts; nil before.
	InvitedNetworkId *server.Id
	// X's targets, sealed under K.
	TargetTable        []byte
	TargetTableVersion int64
	// Y's aliases, sealed under K; nil before Y accepts.
	AliasTable        []byte
	AliasTableVersion int64
	// Empty before Y accepts.
	AcceptStatus NetworkBridgeShareAcceptStatus
}

// A sealed table and its version as a side files them.
func checkNetworkBridgeShareTable(table []byte, version int64) error {
	if len(table) == 0 || DefaultNetworkBridgeShareSettings().TableMaxByteCount < len(table) || version < 1 {
		return ErrNetworkBridgeShareInvalid
	}
	return nil
}

// X's invite (ACL-M6-03's POST /acl/invite), in its transaction: the share's row
// with X's sealed targets table at version. A share id that exists is refused.
func CreateNetworkBridgeShareInTx(
	ctx context.Context,
	tx server.PgTx,
	shareId server.Id,
	networkId server.Id,
	targetTable []byte,
	version int64,
) error {
	if !server.SidecarMg6Linked() {
		return ErrNetworkBridgeShareUnavailable
	}
	if err := checkNetworkBridgeShareTable(targetTable, version); err != nil {
		return err
	}
	if err := checkNetworkBridgeShareOutbound(ctx, networkId); err != nil {
		return err
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_bridge_share (
				share_id,
				network_id,
				target_table,
				target_table_version
			)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (share_id) DO NOTHING
		`,
		shareId,
		networkId,
		targetTable,
		version,
	))
	if tag.RowsAffected() != 1 {
		return ErrNetworkBridgeShareInvalid
	}
	return nil
}

// Y's accept (ACL-M6-03's POST /acl/accept-invite), in its transaction: binds
// invitedNetworkId once, with Y's sealed alias table at version and the accept
// status §17.9's preflight reached. A share already accepted, or X accepting its
// own, is refused.
func AcceptNetworkBridgeShareInTx(
	ctx context.Context,
	tx server.PgTx,
	shareId server.Id,
	invitedNetworkId server.Id,
	aliasTable []byte,
	version int64,
	status NetworkBridgeShareAcceptStatus,
) error {
	if !server.SidecarMg6Linked() {
		return ErrNetworkBridgeShareUnavailable
	}
	if err := checkNetworkBridgeShareTable(aliasTable, version); err != nil {
		return err
	}
	if !status.valid() {
		return ErrNetworkBridgeShareInvalid
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE network_bridge_share
			SET
				invited_network_id = $2,
				alias_table = $3,
				alias_table_version = $4,
				accept_status = $5
			WHERE
				share_id = $1 AND
				invited_network_id IS NULL AND
				network_id <> $2
		`,
		shareId,
		invitedNetworkId,
		aliasTable,
		version,
		string(status),
	))
	if tag.RowsAffected() != 1 {
		return ErrNetworkBridgeShareInvalid
	}
	return nil
}

// The share's row, or ErrNetworkBridgeShareNotFound.
func GetNetworkBridgeShare(ctx context.Context, shareId server.Id) (*NetworkBridgeShare, error) {
	if !server.SidecarMg6Linked() {
		return nil, ErrNetworkBridgeShareUnavailable
	}
	var share *NetworkBridgeShare
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					share_id,
					network_id,
					invited_network_id,
					target_table,
					target_table_version,
					alias_table,
					alias_table_version,
					accept_status
				FROM network_bridge_share
				WHERE share_id = $1
			`,
			shareId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				share = &NetworkBridgeShare{}
				var acceptStatus *string
				server.Raise(result.Scan(
					&share.ShareId,
					&share.NetworkId,
					&share.InvitedNetworkId,
					&share.TargetTable,
					&share.TargetTableVersion,
					&share.AliasTable,
					&share.AliasTableVersion,
					&acceptStatus,
				))
				if acceptStatus != nil {
					share.AcceptStatus = NetworkBridgeShareAcceptStatus(*acceptStatus)
				}
			}
		})
	})
	if share == nil {
		return nil, ErrNetworkBridgeShareNotFound
	}
	return share, nil
}

// Replaces the table of callerNetworkId's side, X's targets or Y's aliases, at
// a version above the stored one; the other side's table is untouched. Y may
// carry a new accept status with its aliases, and X carries none. A network
// that is neither side gets ErrNetworkBridgeShareNotFound, so the share's
// existence is not shown to it.
func PutNetworkBridgeShare(
	ctx context.Context,
	shareId server.Id,
	callerNetworkId server.Id,
	table []byte,
	version int64,
	status NetworkBridgeShareAcceptStatus,
) error {
	if !server.SidecarMg6Linked() {
		return ErrNetworkBridgeShareUnavailable
	}
	if err := checkNetworkBridgeShareTable(table, version); err != nil {
		return err
	}
	if status != "" && !status.valid() {
		return ErrNetworkBridgeShareInvalid
	}
	share, err := GetNetworkBridgeShare(ctx, shareId)
	if err != nil {
		return err
	}
	sharing := share.NetworkId == callerNetworkId
	invited := share.InvitedNetworkId != nil && *share.InvitedNetworkId == callerNetworkId
	switch {
	case sharing:
		if status != "" {
			return ErrNetworkBridgeShareInvalid
		}
		if err := checkNetworkBridgeShareOutbound(ctx, callerNetworkId); err != nil {
			return err
		}
	case !invited:
		return ErrNetworkBridgeShareNotFound
	}
	var replaced bool
	server.Tx(ctx, func(tx server.PgTx) {
		if sharing {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_bridge_share
					SET
						target_table = $3,
						target_table_version = $4
					WHERE
						share_id = $1 AND
						network_id = $2 AND
						target_table_version < $4
				`,
				shareId,
				callerNetworkId,
				table,
				version,
			))
			replaced = tag.RowsAffected() == 1
		} else {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_bridge_share
					SET
						alias_table = $3,
						alias_table_version = $4,
						accept_status = COALESCE($5, accept_status)
					WHERE
						share_id = $1 AND
						invited_network_id = $2 AND
						alias_table_version < $4
				`,
				shareId,
				callerNetworkId,
				table,
				version,
				func() *string {
					if status == "" {
						return nil
					}
					value := string(status)
					return &value
				}(),
			))
			replaced = tag.RowsAffected() == 1
		}
	})
	if !replaced {
		return ErrNetworkBridgeShareVersion
	}
	return nil
}

// A share's revocation: removes its row. Reports whether there was one.
func RemoveNetworkBridgeShare(ctx context.Context, shareId server.Id) (removed bool) {
	if !server.SidecarMg6Linked() {
		return false
	}
	server.Tx(ctx, func(tx server.PgTx) {
		removed = RemoveNetworkBridgeShareInTx(ctx, tx, shareId)
	})
	return
}

// RemoveNetworkBridgeShare in the caller's transaction, as a revocation's
// handler runs it beside the grant it revokes.
func RemoveNetworkBridgeShareInTx(ctx context.Context, tx server.PgTx, shareId server.Id) bool {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM network_bridge_share
			WHERE share_id = $1
		`,
		shareId,
	))
	return tag.RowsAffected() == 1
}

// Removes every share naming networkId on either side, in the network's
// deletion transaction.
func removeNetworkBridgeSharesInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM network_bridge_share
			WHERE network_id = $1 OR invited_network_id = $1
		`,
		networkId,
	))
}

// GET and PUT /network/bridge/share/<share_id>'s answer: both tables, base64 in
// JSON, as stored.
type NetworkBridgeShareResult struct {
	ShareId            server.Id                      `json:"share_id"`
	NetworkId          server.Id                      `json:"network_id"`
	InvitedNetworkId   *server.Id                     `json:"invited_network_id,omitempty"`
	TargetTable        []byte                         `json:"target_table"`
	TargetTableVersion int64                          `json:"target_table_version"`
	AliasTable         []byte                         `json:"alias_table,omitempty"`
	AliasTableVersion  int64                          `json:"alias_table_version"`
	AcceptStatus       NetworkBridgeShareAcceptStatus `json:"accept_status,omitempty"`
}

// PUT /network/bridge/share/<share_id>'s body: the caller's own side's sealed
// table and its version, and for the invited side a new accept status or none.
type PutNetworkBridgeShareArgs struct {
	Table        []byte                         `json:"table"`
	Version      int64                          `json:"version"`
	AcceptStatus NetworkBridgeShareAcceptStatus `json:"accept_status,omitempty"`
}

// The status and message a share error answers with. A share that is not the
// caller's network's to read answers as one that does not exist.
func networkBridgeShareHttpError(err error) error {
	switch {
	case errors.Is(err, ErrNetworkBridgeShareUnavailable):
		return fmt.Errorf("%d %s", http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, ErrNetworkBridgeShareNotFound):
		return fmt.Errorf("%d %s", http.StatusNotFound, err.Error())
	case errors.Is(err, ErrNetworkBridgeShareVersion):
		return fmt.Errorf("%d %s", http.StatusConflict, err.Error())
	case errors.Is(err, ErrNetworkBridgeShareInvalid):
		return fmt.Errorf("%d %s", http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNetworkBridgeShareOutboundRefused):
		return fmt.Errorf("%d %s", http.StatusForbidden, err.Error())
	default:
		return err
	}
}

// Whether the bearer is a sidecar of its network: an active identity its client
// is bound to (§5.15). §17.16 serves these routes to sidecars alone.
func networkBridgeShareCallerIsSidecar(clientSession *session.ClientSession) bool {
	if clientSession.ByJwt.ClientId == nil {
		return false
	}
	view := GetSidecarIdentityByClient(clientSession.Ctx, clientSession.ByJwt.NetworkId, *clientSession.ByJwt.ClientId)
	return view != nil && view.Status == SidecarIdentityStatusActive
}

// The row as a sidecar of networkId may read it: a share its network is a side
// of, else ErrNetworkBridgeShareNotFound.
func getNetworkBridgeShareForNetwork(ctx context.Context, shareId server.Id, networkId server.Id) (*NetworkBridgeShare, error) {
	share, err := GetNetworkBridgeShare(ctx, shareId)
	if err != nil {
		return nil, err
	}
	if share.NetworkId != networkId && (share.InvitedNetworkId == nil || *share.InvitedNetworkId != networkId) {
		return nil, ErrNetworkBridgeShareNotFound
	}
	return share, nil
}

// The share's row as GET and PUT answer it.
func (self *NetworkBridgeShare) result() *NetworkBridgeShareResult {
	return &NetworkBridgeShareResult{
		ShareId:            self.ShareId,
		NetworkId:          self.NetworkId,
		InvitedNetworkId:   self.InvitedNetworkId,
		TargetTable:        self.TargetTable,
		TargetTableVersion: self.TargetTableVersion,
		AliasTable:         self.AliasTable,
		AliasTableVersion:  self.AliasTableVersion,
		AcceptStatus:       self.AcceptStatus,
	}
}

// GET /network/bridge/share/<share_id> (§17.16): both tables, to a sidecar of
// either network. Client class, on the strict_exp allowlist.
func GetNetworkBridgeShareForSidecar(shareId server.Id, clientSession *session.ClientSession) (*NetworkBridgeShareResult, error) {
	if !server.SidecarMg6Linked() {
		return nil, networkBridgeShareHttpError(ErrNetworkBridgeShareUnavailable)
	}
	if !networkBridgeShareCallerIsSidecar(clientSession) {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, "Bridge shares are a sidecar's.")
	}
	share, err := getNetworkBridgeShareForNetwork(clientSession.Ctx, shareId, clientSession.ByJwt.NetworkId)
	if err != nil {
		return nil, networkBridgeShareHttpError(err)
	}
	return share.result(), nil
}

// PUT /network/bridge/share/<share_id> (§17.16): replaces the caller's own
// network's table at a higher version, a lower or equal one refused with 409,
// and answers both tables as they now stand. Client class, on the strict_exp
// allowlist.
func PutNetworkBridgeShareForSidecar(
	shareId server.Id,
	put *PutNetworkBridgeShareArgs,
	clientSession *session.ClientSession,
) (*NetworkBridgeShareResult, error) {
	if !server.SidecarMg6Linked() {
		return nil, networkBridgeShareHttpError(ErrNetworkBridgeShareUnavailable)
	}
	if !networkBridgeShareCallerIsSidecar(clientSession) {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, "Bridge shares are a sidecar's.")
	}
	if put == nil {
		return nil, networkBridgeShareHttpError(ErrNetworkBridgeShareInvalid)
	}
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	err := PutNetworkBridgeShare(ctx, shareId, networkId, put.Table, put.Version, put.AcceptStatus)
	if err != nil {
		return nil, networkBridgeShareHttpError(err)
	}
	share, err := getNetworkBridgeShareForNetwork(ctx, shareId, networkId)
	if err != nil {
		return nil, networkBridgeShareHttpError(err)
	}
	return share.result(), nil
}
