package model

// §19.10's Postgres restore at api (TEAM-M3-02; D-OPS-14, D-OPS-18, I-15,
// FS-41, FS-42): the steps that run, in order and each idempotent, before
// api's public routes, the token exchange and CreateContract reopen, which
// OPS-M3-03's sidecarctl restore drives (CONTRACTS' TEAM-M3-02 row).
//
//	heads    AdvanceAclHeadsAfterRestore: each network's proj/ entries past its
//	         restored network_acl_head are verified as §7.9's readers verify
//	         them (the sequencer's rows-only log), and the head advances to the
//	         verified tip with has_document, so a first policy document filed
//	         after the dumps holds (§8.17). A head the store's log does not pass
//	         through is reported and left, never moved down or across
//	effects  ReapplyRemovalEffects: §7.10's effects for every entry in the
//	         retained log whose admin event the restored rows do not hold, in
//	         position order, idempotent (config_log_effects.go): the entries
//	         after the older dump's snapshot, which owe their deactivations,
//	         identity revocations, member removals and rev/ objects again
//	rev      RecreateRecentRevocations: rev/ objects for every client and sidecar
//	         the restored rows record as removed less than rev/'s 20 minutes
//	         ago, since a site loss took the objects and a service token issued
//	         before them may still live (§7.8's life row)
//
// None of these reads a request or writes a log entry.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// What the heads step did for one network.
type ConfigLogRestoreHead struct {
	NetworkId server.Id
	// the restored head's position and the one it stands at now
	From uint64
	To   uint64
	// why the head did not move, nil when it moved or had nowhere to go
	Err error
}

// The heads step (see the file header), over the process's sequencer.
func AdvanceAclHeadsAfterRestore(ctx context.Context, sequencer *ConfigLogSequencer) ([]*ConfigLogRestoreHead, error) {
	heads, err := GetNetworkAclHeads(ctx)
	if err != nil {
		return nil, err
	}
	networkIds := make([]server.Id, 0, len(heads))
	for networkId := range heads {
		networkIds = append(networkIds, networkId)
	}
	slices.SortFunc(networkIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	outcomes := []*ConfigLogRestoreHead{}
	for _, networkId := range networkIds {
		head := heads[networkId]
		outcome := &ConfigLogRestoreHead{NetworkId: networkId, From: head.Version, To: head.Version}
		outcomes = append(outcomes, outcome)
		pin, hasDocument, err := sequencer.verifiedTip(ctx, networkId, &bridge.AuthPin{Version: head.Version, Chain: head.Chain})
		if err != nil {
			outcome.Err = err
			continue
		}
		if pin == nil || pin.GetVersion() <= head.Version {
			continue
		}
		if _, err := AdvanceNetworkAclHead(ctx, networkId, pin.GetVersion(), pin.GetChain(), hasDocument); err != nil {
			outcome.Err = err
			continue
		}
		outcome.To = pin.GetVersion()
	}
	return outcomes, nil
}

// The restored head is not on the store's log.
var ErrConfigLogRestoreOffChain = errors.New("the restored network_acl_head is not on the store's log")

// The verified tip of the network's log in the store and whether its rows hold
// a document, nil with no log; ErrConfigLogRestoreOffChain when the log does
// not pass through head where it could tell.
func (self *ConfigLogSequencer) verifiedTip(ctx context.Context, networkId server.Id, head *bridge.AuthPin) (*bridge.AuthPin, bool, error) {
	network := self.network(networkId)
	network.stateLock.Lock()
	defer network.stateLock.Unlock()
	// load afresh: the store, never this memory, says what the log is
	network.log, network.signedGenesis = nil, nil
	clear(network.basePins)
	network.bases = nil
	if err := network.loadWithLock(ctx, self, nil); err != nil {
		return nil, false, err
	}
	if network.log == nil {
		return nil, false, nil
	}
	state := network.log.State()
	pin := state.Pin()
	if head.GetVersion() <= pin.GetVersion() && state.RetentionPoint() <= head.GetVersion() && !state.OnChain(head) {
		return nil, false, fmt.Errorf("%w: at %d", ErrConfigLogRestoreOffChain, head.GetVersion())
	}
	hasDocument := false
	if acl, err := state.Acl(); err == nil {
		hasDocument = acl.HasDocument()
	}
	return pin, hasDocument, nil
}

// The effects step (see the file header) for one network: a walk of its whole
// retained log from the start, whatever this process applied before, until no
// entry is left. Answers how many entries' effects it applied.
func ReapplyRemovalEffects(ctx context.Context, sequencer *ConfigLogSequencer, networkId server.Id, settings *ConfigLogEffectsSettings) (int, error) {
	if settings == nil {
		settings = DefaultConfigLogEffectsSettings()
	}
	network := sequencer.network(networkId)
	func() {
		network.stateLock.Lock()
		defer network.stateLock.Unlock()
		network.effectsThrough = 0
	}()
	appliedCount := 0
	for {
		result, err := ApplyConfigLogEffects(ctx, sequencer, networkId, settings)
		if err != nil {
			return appliedCount, err
		}
		appliedCount += result.AppliedCount
		if !result.More {
			return appliedCount, nil
		}
	}
}

// The rev step (see the file header): owes the rev/ objects of every client
// deactivated and every sidecar identity revoked since now less rev/'s
// DeleteAfter, each at its own removal time. Answers how many it owed.
func RecreateRecentRevocations(ctx context.Context, now time.Time, settings *SidecarRevSettings) (int, error) {
	if !server.SidecarMg1Linked() {
		return 0, nil
	}
	if settings == nil {
		settings = DefaultSidecarRevSettings()
	}
	since := now.Add(-settings.DeleteAfter)
	revocations := []*SidecarRevocation{}
	if r := server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, err := tx.Query(
				ctx,
				`
					SELECT network_id, client_id, deactivate_time
					FROM network_client
					WHERE active = false AND deactivate_time IS NOT NULL AND $1 <= deactivate_time
					ORDER BY client_id
				`,
				since,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var networkId, clientId server.Id
					var deactivateTime time.Time
					server.Raise(result.Scan(&networkId, &clientId, &deactivateTime))
					revocations = append(revocations, &SidecarRevocation{
						NetworkId: networkId,
						Kind:      sidecar.RevocationKindClient,
						SubjectId: clientId,
						NotBefore: SidecarRevocationNotBefore(deactivateTime, settings),
						Reason:    sidecar.RevocationReasonDeactivated,
					})
				}
			})
			result, err = tx.Query(
				ctx,
				`
					SELECT network_id, sidecar_id, revoke_time
					FROM sidecar_identity
					WHERE revoke_time IS NOT NULL AND $1 <= revoke_time
					ORDER BY sidecar_id
				`,
				since,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var networkId, sidecarId server.Id
					var revokeTime time.Time
					server.Raise(result.Scan(&networkId, &sidecarId, &revokeTime))
					revocations = append(revocations, &SidecarRevocation{
						NetworkId: networkId,
						Kind:      sidecar.RevocationKindSidecar,
						SubjectId: sidecarId,
						NotBefore: SidecarRevocationNotBefore(revokeTime, settings),
						Reason:    sidecar.RevocationReasonRevoked,
					})
				}
			})
			WriteRevocations(ctx, tx, revocations)
		})
	}); r != nil {
		return 0, fmt.Errorf("recent revocations: %v", r)
	}
	return len(revocations), nil
}

// Whether the restored rows list the sidecar under its network's Owners and
// Admins, what §7.8's re-seed mode mints admin scopes to (§19.10's authority
// row): its identity is active in the network, its newest binding's client is
// untagged and not ephemeral, and that client's owner is the personal
// network's owner or, in a team, an Owner's or Admin's live membership.
// Answers the member it belongs to: the owner's user in a personal network, the
// membership in a team.
func ConfigReseedAdminSidecar(ctx context.Context, networkId server.Id, sidecarId server.Id) (memberId *server.Id, admin bool, returnErr error) {
	if !server.SidecarMg1Linked() {
		return nil, false, nil
	}
	if r := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			var adminUserId *server.Id
			var ownerUserId *server.Id
			found := false
			result, err := conn.Query(
				ctx,
				`
					SELECT network.admin_user_id, network_client_owner.owner_user_id
					FROM sidecar_identity
					INNER JOIN sidecar_identity_binding ON
						sidecar_identity_binding.sidecar_id = sidecar_identity.sidecar_id AND
						sidecar_identity_binding.generation = (
							SELECT MAX(newest.generation)
							FROM sidecar_identity_binding newest
							WHERE newest.sidecar_id = sidecar_identity.sidecar_id
						)
					INNER JOIN network_client_owner ON network_client_owner.client_id = sidecar_identity_binding.client_id
					INNER JOIN network ON network.network_id = sidecar_identity.network_id
					WHERE
						sidecar_identity.sidecar_id = $1 AND
						sidecar_identity.network_id = $2 AND
						sidecar_identity.status = $3 AND
						NOT network_client_owner.tagged AND
						NOT network_client_owner.ephemeral
				`,
				sidecarId,
				networkId,
				SidecarIdentityStatusActive,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&adminUserId, &ownerUserId))
					found = true
				}
			})
			if !found || ownerUserId == nil {
				return
			}
			team := false
			if server.SidecarMg6Linked() {
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM network_team WHERE network_id = $1)`, networkId).Scan(&team))
			}
			if !team {
				if adminUserId != nil && *adminUserId == *ownerUserId {
					owner := *ownerUserId
					memberId, admin = &owner, true
				}
				return
			}
			member := getNetworkMemberOfUserWithQuery(ctx, conn, networkId, *ownerUserId)
			if member != nil && !member.Pending && (member.Role == NetworkMemberRoleOwner || member.Role == NetworkMemberRoleAdmin) {
				id := member.MemberId
				memberId, admin = &id, true
			}
		})
	}); r != nil {
		return nil, false, fmt.Errorf("re-seed admin: %v", r)
	}
	return
}
