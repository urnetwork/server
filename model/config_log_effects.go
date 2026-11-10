package model

// §7.10's removal at api, driven from the log (TEAM-M2-16; R-TEAM-11,
// R-TEAM-12, D-TEAM-10, I-15): what api does once a verified statement or tick
// has landed, never because a request asked. A removal's denial is unsigned and
// takes effect only because a statement that verified at its own position
// landed, so a statement refused at its position, or a request that never
// landed, changes nothing here.
//
//	task      one run-once task a network (ApplyConfigLogEffectsTask), which
//	          the sequencer schedules before an append's first PUT, two
//	          minutes on, so a process killed between its append and the rest
//	          still owes the effects, and again after the entry lands, to run
//	          at once (ConfigLogSettings.Effects)
//	read      the run catches this process's verifier up to the store's tip
//	          and walks the retained entries after what it last applied, each
//	          with the verdict the verifier gave it at its own position, and
//	          compares S(n-1) with S(n) for the kinds that can carry an effect
//	effects   per entry, in one transaction:
//	            a sidecar whose row the entry revoked (a SidecarRevoke, a
//	            MemberRevoke's devices, a completed proposal of either, a
//	            tick's reap): its bound clients deactivated through the
//	            deactivation path, which writes their revocation records, so
//	            connect closes their transports at its next authorization
//	            recheck, within the 90 s lease (session/authorization_lease.go),
//	            and owes their rev/ objects; then §7.10's chain on the sidecar,
//	            its identity revoked (model.SetSidecarIdentityRevoked's step)
//	            and its own rev/ object
//	            a member whose row the entry removed: §7.10's chain on the
//	            member, its rev/ object, and its network_member row, where a
//	            team has one
//	            a member whose role the entry set (an AdminSet, a MemberAdmit,
//	            or a completed proposal of either): network_member's role,
//	            mirrored from the log (TEAM-M6-03)
//	          and one admin event naming the entry (§7.11)
//	sponsors  each run that met a SponsorSet or a member's removal, and a
//	          process's first run for a network, sets every tagged client's
//	          network_client_owner.sponsor_user_id to its sidecar's sponsor at
//	          the tip, the member a SponsorSet named or its admitting member,
//	          or NULL once that member is gone and autogroup:admin stands in
//	          (§7.7, owner decision 83)
//
// Every effect is idempotent, and an entry's effects run once: its admin
// event carries the entry's own id as its request id
// (configLogEffectRequestId), the run skips an entry whose event exists, and
// the event commits with the effects, so a crash repeats nothing and §19.10's
// restore, which brings back the admin events of the older dump, re-applies
// exactly the entries after it. Each process keeps how far it applied in
// memory only: a fresh process walks the whole retained run again and finds
// each applied entry's event.
//
// Not here, and listed by TEAM-M2-16's draft: the notice a new Delegation or an
// AdminSet change owes Owners and Admins (§7.8), Suspend's rev/ object, and an
// entry whose states this process's verifier no longer holds (before its
// previous base), which is logged and passed.
//
// A run holds the network's verifier lock only to read the log and its states,
// never across a Postgres transaction.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Tunables of the effects task.
type ConfigLogEffectsSettings struct {
	// How long after an append's intent the task runs, when the append's own
	// wake never comes: 2 min, past a slow PUT's outcome, so the run finds the
	// entry the intent was for.
	IntentDelay time.Duration
	// A run's max time.
	MaxTime time.Duration
	// How long a run that could not reach the log or Postgres waits before it
	// tries again.
	RetryDelay time.Duration
	// The most entries one run walks; the next run starts at once after them.
	EntryMaxCount int
}

func DefaultConfigLogEffectsSettings() *ConfigLogEffectsSettings {
	return &ConfigLogEffectsSettings{
		IntentDelay:   2 * time.Minute,
		MaxTime:       5 * time.Minute,
		RetryDelay:    30 * time.Second,
		EntryMaxCount: 1024,
	}
}

// The prefix of every admin event action an entry's effects record, the rest
// the entry's kind: "tick", or a statement's type as auth.proto names it,
// lowercase, without SIGNED_.
const AdminEventActionConfigPrefix = "config."

// What one landed entry does at api, from its verdict and the states around
// it.
type configLogEntryEffect struct {
	n      uint64
	chain  [sha256.Size]byte
	timeMs int64
	// the admin event's action
	action string
	// the statement's signer when a sidecar signed it
	signerSidecarId *server.Id
	// sidecars whose rows the entry revoked, and why their rev/ objects say
	revokedSidecarIds []server.Id
	reason            sidecar.RevocationReason
	// members whose rows the entry removed
	removedMemberIds []server.Id
	// members whose role the entry set, with the role
	memberRoles map[server.Id]bridge.MemberRole
	// whether the tip's sponsors may have moved, a SponsorSet's or a removed
	// member's agents'
	sponsorsMoved bool
}

// Whether the entry does anything here.
func (self *configLogEntryEffect) empty() bool {
	return len(self.revokedSidecarIds) == 0 && len(self.removedMemberIds) == 0 && len(self.memberRoles) == 0 && !self.sponsorsMoved
}

// The admin event's request id for an entry's effects: the first 16 bytes of
// SHA-256 over a domain and the entry's (n, A_n), so it names that entry alone
// and a second run finds its event.
func configLogEffectRequestId(networkId server.Id, n uint64, chain [sha256.Size]byte) server.Id {
	h := sha256.New()
	h.Write([]byte("urnetwork config log effect v1"))
	h.Write(networkId.Bytes())
	var position [8]byte
	for i := range 8 {
		position[i] = byte(n >> (56 - 8*i))
	}
	h.Write(position[:])
	h.Write(chain[:])
	var requestId server.Id
	copy(requestId[:], h.Sum(nil))
	return requestId
}

// The statement types whose landing can carry an effect here: a removal, a
// role, a sponsor, or a proposal that completes one.
var configLogEffectTypes = map[bridge.SignedType]bool{
	bridge.SignedType_SIGNED_SIDECAR_REVOKE:     true,
	bridge.SignedType_SIGNED_MEMBER_REVOKE:      true,
	bridge.SignedType_SIGNED_ADMIN_SET:          true,
	bridge.SignedType_SIGNED_MEMBER_ADMIT:       true,
	bridge.SignedType_SIGNED_SPONSOR_SET:        true,
	bridge.SignedType_SIGNED_AUTHORITY_PROPOSAL: true,
	bridge.SignedType_SIGNED_AUTHORITY_COSIGN:   true,
}

// One run's read of a network's log: the effects of the entries after what this
// process applied, at most limit entries, and where it stands.
type configLogEffectsRead struct {
	effects []*configLogEntryEffect
	// the newest position walked
	through uint64
	// more entries wait past through
	more bool
	// the tip's sponsors, by tagged sidecar, nil for a removed member's
	sponsors map[server.Id]*server.Id
	// whether sponsors should be reconciled this run
	reconcileSponsors bool
	// the sponsors' members, which the tip's rows name, for their users
	sponsorMemberIds []server.Id
}

// Applies the store's entries past this instance's tip to its verifier, so a
// read sees what other api instances appended.
func (self *configLogNetwork) catchUpWithLock(ctx context.Context, sequencer *ConfigLogSequencer) error {
	networkId := sidecar.Id(self.networkId)
	positions, _, err := sequencer.listPositions(ctx, self.networkId, self.log.Pin().GetVersion(), 1<<30)
	if err != nil {
		return err
	}
	for _, n := range positions {
		entryBytes, found, err := configLogGet(ctx, sequencer.cluster, sidecar.ConfigEntryKey(networkId, n))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("entry %d listed and gone", n)
		}
		if err := self.applyWithLock(ctx, sequencer, entryBytes); err != nil {
			return err
		}
	}
	return nil
}

// The effects of the entries after this process's watermark, read under the
// network's lock from its verifier, its states and its kept verdicts.
func (self *ConfigLogSequencer) readConfigLogEffects(ctx context.Context, networkId server.Id, limit int) (*configLogEffectsRead, error) {
	network := self.network(networkId)
	network.stateLock.Lock()
	defer network.stateLock.Unlock()
	if err := network.loadWithLock(ctx, self, nil); err != nil {
		return nil, err
	}
	read := &configLogEffectsRead{}
	if network.log == nil {
		return read, nil
	}
	if err := network.catchUpWithLock(ctx, self); err != nil {
		return nil, err
	}
	read.through = network.effectsThrough
	read.reconcileSponsors = network.effectsThrough == 0
	for _, entryBytes := range network.log.Retained().Entries {
		entry := &bridge.AuthEntry{}
		if err := proto.Unmarshal(entryBytes, entry); err != nil {
			return nil, err
		}
		if entry.GetN() <= network.effectsThrough {
			continue
		}
		if limit <= len(read.effects) {
			read.more = true
			break
		}
		effect, err := network.entryEffectWithLock(ctx, self, entry, entryBytes)
		if err != nil {
			return nil, err
		}
		if effect != nil {
			read.effects = append(read.effects, effect)
			if effect.sponsorsMoved {
				read.reconcileSponsors = true
			}
		}
		read.through = entry.GetN()
	}
	if read.reconcileSponsors {
		state := network.log.State()
		read.sponsors = map[server.Id]*server.Id{}
		for _, sidecarId := range state.SidecarIds() {
			row := state.SidecarRow(sidecarId)
			if row == nil || len(row.GetTagIds()) == 0 {
				continue
			}
			if memberId, ok := state.Sponsor(sidecarId); ok {
				sponsorMemberId := server.Id(memberId)
				read.sponsors[server.Id(sidecarId)] = &sponsorMemberId
				read.sponsorMemberIds = append(read.sponsorMemberIds, sponsorMemberId)
			} else {
				read.sponsors[server.Id(sidecarId)] = nil
			}
		}
	}
	return read, nil
}

// Raises this process's watermark to through, after a run applied every
// effect up to it.
func (self *ConfigLogSequencer) configLogEffectsApplied(networkId server.Id, through uint64) {
	network := self.network(networkId)
	network.stateLock.Lock()
	defer network.stateLock.Unlock()
	network.effectsThrough = max(network.effectsThrough, through)
}

// One entry's effect, from its kept verdict and the states before and after it,
// nil when it does nothing here: a base, a refused statement, a type that
// carries no effect, or a position whose states this verifier no longer holds.
func (self *configLogNetwork) entryEffectWithLock(ctx context.Context, sequencer *ConfigLogSequencer, entry *bridge.AuthEntry, entryBytes []byte) (*configLogEntryEffect, error) {
	effect := &configLogEntryEffect{
		n:      entry.GetN(),
		timeMs: entry.GetTimeMs(),
		reason: sidecar.RevocationReasonRemoved,
	}
	switch entry.GetKind() {
	case bridge.AuthEntryKindStatement:
		signed := &bridge.Signed{}
		if proto.Unmarshal(entry.GetBody(), signed) != nil || !configLogEffectTypes[bridge.SignedType(signed.GetType())] {
			return nil, nil
		}
		if len(signed.GetSignerId()) == len(sidecar.Id{}) && len(signed.GetKeyId()) == 0 {
			signerSidecarId, err := server.IdFromBytes(signed.GetSignerId())
			if err == nil {
				effect.signerSidecarId = &signerSidecarId
			}
		}
		effect.action = AdminEventActionConfigPrefix + configLogSignedTypeWord(bridge.SignedType(signed.GetType()))
	case bridge.AuthEntryKindTick:
		tick := &bridge.ApiTick{}
		if proto.Unmarshal(entry.GetBody(), tick) != nil || len(tick.GetReapedSidecarIds()) == 0 {
			return nil, nil
		}
		effect.action = AdminEventActionConfigPrefix + "tick"
		// a reap deactivated the clients already; the rest follows it
		effect.reason = sidecar.RevocationReasonDeactivated
	default:
		return nil, nil
	}

	// the verdict the verifier gave the entry at its own position
	verdict, err := self.log.Apply(entryBytes, sequencer.rowsSource(ctx))
	if err != nil {
		return nil, err
	}
	if entry.GetKind() == bridge.AuthEntryKindStatement && verdict.SignedType == 0 && verdict.Accepted() {
		// a position before what the verifier keeps: no verdict to go by
		glog.Warningf("[config]%s effects pass entry %d: its verdict is not kept\n", self.networkId, entry.GetN())
		return nil, nil
	}
	if !verdict.Accepted() {
		return nil, nil
	}
	chain, err := bridge.AuthChainDigest(entry.GetN(), entry.GetPrevChain(), entry.GetKind(), entry.GetTimeMs(), entry.GetBody())
	if err != nil {
		return nil, err
	}
	effect.chain = chain
	after, err := self.log.StateAt(&bridge.AuthPin{Version: entry.GetN(), Chain: chain[:]})
	if err != nil {
		glog.Warningf("[config]%s effects pass entry %d: %v\n", self.networkId, entry.GetN(), err)
		return nil, nil
	}
	before, err := self.log.StateAt(&bridge.AuthPin{Version: entry.GetN() - 1, Chain: entry.GetPrevChain()})
	if err != nil {
		glog.Warningf("[config]%s effects pass entry %d: %v\n", self.networkId, entry.GetN(), err)
		return nil, nil
	}

	for _, sidecarId := range after.SidecarIds() {
		if row := after.SidecarRow(sidecarId); row != nil && row.GetRevokedAt() == entry.GetN() {
			effect.revokedSidecarIds = append(effect.revokedSidecarIds, server.Id(sidecarId))
		}
	}
	effect.memberRoles = map[server.Id]bridge.MemberRole{}
	for _, memberId := range before.MemberIds() {
		if after.Member(memberId) == nil {
			effect.removedMemberIds = append(effect.removedMemberIds, server.Id(memberId))
			effect.sponsorsMoved = true
		}
	}
	for _, memberId := range after.MemberIds() {
		role := after.Member(memberId).GetRole()
		if previous := before.Member(memberId); previous == nil || previous.GetRole() != role {
			effect.memberRoles[server.Id(memberId)] = role
		}
	}
	if verdict.SignedType == bridge.SignedType_SIGNED_SPONSOR_SET {
		effect.sponsorsMoved = true
	}
	if effect.empty() {
		return nil, nil
	}
	return effect, nil
}

// A SignedType's word in an admin event action: SIGNED_SIDECAR_REVOKE's is
// sidecar_revoke.
func configLogSignedTypeWord(signedType bridge.SignedType) string {
	name := signedType.String()
	word := []byte{}
	if len(name) < len("SIGNED_") || name[:len("SIGNED_")] != "SIGNED_" {
		return "statement"
	}
	for _, c := range []byte(name[len("SIGNED_"):]) {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		word = append(word, c)
	}
	return string(word)
}

// Whether an earlier run applied the entry's effects: its admin event exists.
func configLogEffectAppliedInTx(ctx context.Context, tx server.PgTx, networkId server.Id, requestId server.Id) bool {
	var applied bool
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT EXISTS (
				SELECT 1
				FROM network_admin_event
				WHERE network_id = $1 AND request_id = $2
			)
		`,
		networkId,
		requestId,
	).Scan(&applied))
	return applied
}

// Ends a sidecar the log revoked, in tx: its bound active clients deactivated
// for reason through the deactivation path, then §7.10's chain on the sidecar
// at the entry's time.
func removeConfigSidecarInTx(ctx context.Context, tx server.PgTx, networkId server.Id, sidecarId server.Id, reason sidecar.RevocationReason, at time.Time) error {
	clientIds := []server.Id{}
	result, err := tx.Query(
		ctx,
		`
			SELECT sidecar_identity_binding.client_id
			FROM sidecar_identity_binding
			INNER JOIN sidecar_identity ON sidecar_identity.sidecar_id = sidecar_identity_binding.sidecar_id
			INNER JOIN network_client ON network_client.client_id = sidecar_identity_binding.client_id
			WHERE
				sidecar_identity_binding.sidecar_id = $1 AND
				sidecar_identity.network_id = $2 AND
				network_client.network_id = $2 AND
				network_client.active
			ORDER BY sidecar_identity_binding.client_id
		`,
		sidecarId,
		networkId,
	)
	if err != nil {
		return err
	}
	for result.Next() {
		var clientId server.Id
		if err := result.Scan(&clientId); err != nil {
			result.Close()
			return err
		}
		clientIds = append(clientIds, clientId)
	}
	result.Close()
	if err := result.Err(); err != nil {
		return err
	}
	if 0 < len(clientIds) {
		if _, err := deactivateNetworkClientsInTx(withRemovalReason(ctx, reason), tx, clientIds, networkId); err != nil {
			return err
		}
	}
	return ApplyRemovalEffectsInTx(ctx, tx, &Removal{
		Subjects: []*RemovalSubject{{NetworkId: networkId, Kind: sidecar.RevocationKindSidecar, SubjectId: sidecarId}},
		Reason:   reason,
		Time:     at,
	})
}

// Ends a member the log removed, in tx: §7.10's chain on the member at the
// entry's time, and its network_member row where a team has one. Its devices
// are the entry's revoked sidecars, ended beside it.
func removeConfigMemberInTx(ctx context.Context, tx server.PgTx, networkId server.Id, memberId server.Id, at time.Time) error {
	if err := ApplyRemovalEffectsInTx(ctx, tx, &Removal{
		Subjects: []*RemovalSubject{{NetworkId: networkId, Kind: sidecar.RevocationKindMember, SubjectId: memberId}},
		Reason:   sidecar.RevocationReasonRemoved,
		Time:     at,
	}); err != nil {
		return err
	}
	if !server.SidecarMg6Linked() {
		return nil
	}
	_, err := tx.Exec(
		ctx,
		`DELETE FROM network_member WHERE network_id = $1 AND member_id = $2`,
		networkId,
		memberId,
	)
	return err
}

// Mirrors the roles the log set into network_member (TEAM-M6-03): a member
// whose row a team holds takes the log's role; a member the server holds no row
// for, such as a personal network's owner, has nothing to mirror.
func mirrorConfigMemberRolesInTx(ctx context.Context, tx server.PgTx, networkId server.Id, memberRoles map[server.Id]bridge.MemberRole) error {
	if len(memberRoles) == 0 || !server.SidecarMg6Linked() {
		return nil
	}
	memberIds := make([]server.Id, 0, len(memberRoles))
	for memberId := range memberRoles {
		memberIds = append(memberIds, memberId)
	}
	slices.SortFunc(memberIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	for _, memberId := range memberIds {
		role, ok := NetworkMemberRoleOfBridge(memberRoles[memberId])
		if !ok {
			// a role this build does not know mirrors nothing, so no route
			// opens on it
			continue
		}
		if _, err := tx.Exec(
			ctx,
			`
				UPDATE network_member
				SET role = $3
				WHERE network_id = $1 AND member_id = $2 AND role <> $3
			`,
			networkId,
			memberId,
			string(role),
		); err != nil {
			return err
		}
	}
	return nil
}

// Applies one entry's effects in tx and records its admin event, once: an
// entry whose event exists is skipped. Reports whether this call applied it.
func applyConfigLogEntryEffectInTx(ctx context.Context, tx server.PgTx, networkId server.Id, effect *configLogEntryEffect) (bool, error) {
	requestId := configLogEffectRequestId(networkId, effect.n, effect.chain)
	if configLogEffectAppliedInTx(ctx, tx, networkId, requestId) {
		return false, nil
	}
	at := time.UnixMilli(effect.timeMs).UTC()
	for _, sidecarId := range effect.revokedSidecarIds {
		if err := removeConfigSidecarInTx(ctx, tx, networkId, sidecarId, effect.reason, at); err != nil {
			return false, err
		}
	}
	for _, memberId := range effect.removedMemberIds {
		if err := removeConfigMemberInTx(ctx, tx, networkId, memberId, at); err != nil {
			return false, err
		}
	}
	if err := mirrorConfigMemberRolesInTx(ctx, tx, networkId, effect.memberRoles); err != nil {
		return false, err
	}
	event := &AdminEvent{
		NetworkId:  networkId,
		ActorKind:  AdminEventActorSystem,
		Action:     effect.action,
		TargetKind: AdminEventTargetNetwork,
		TargetId:   &networkId,
		Result:     AdminEventResultOk,
		RequestId:  &requestId,
	}
	if effect.signerSidecarId != nil {
		signerSidecarId := *effect.signerSidecarId
		event.ActorKind, event.ActorId = AdminEventActorSidecar, &signerSidecarId
	}
	switch {
	case 0 < len(effect.removedMemberIds):
		memberId := effect.removedMemberIds[0]
		event.TargetKind, event.TargetId = AdminEventTargetMember, &memberId
	case 0 < len(effect.revokedSidecarIds):
		sidecarId := effect.revokedSidecarIds[0]
		event.TargetKind, event.TargetId = AdminEventTargetSidecar, &sidecarId
	}
	AddAdminEventInTx(ctx, tx, event)
	return true, nil
}

// The users of the members memberIds name in networkId: a team member's row's
// user, and in a network with no team a member_id is its user's id (§7.5). A
// member no row names in a team has none.
func configMemberUserIdsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, memberIds []server.Id) (map[server.Id]server.Id, error) {
	userIds := map[server.Id]server.Id{}
	if len(memberIds) == 0 {
		return userIds, nil
	}
	team := false
	if server.SidecarMg6Linked() {
		if err := tx.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM network_team WHERE network_id = $1)`,
			networkId,
		).Scan(&team); err != nil {
			return nil, err
		}
	}
	if !team {
		for _, memberId := range memberIds {
			userIds[memberId] = memberId
		}
		return userIds, nil
	}
	result, err := tx.Query(
		ctx,
		`
			SELECT member_id, user_id
			FROM network_member
			WHERE network_id = $1 AND member_id = ANY($2)
		`,
		networkId,
		memberIds,
	)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	for result.Next() {
		var memberId, userId server.Id
		if err := result.Scan(&memberId, &userId); err != nil {
			return nil, err
		}
		userIds[memberId] = userId
	}
	return userIds, result.Err()
}

// Sets every tagged client of the network's sidecars to its sidecar's sponsor
// at the tip, in tx: sponsors by tagged sidecar, nil where autogroup:admin
// stands in. Only rows that differ are written.
func reconcileConfigSponsorsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, sponsors map[server.Id]*server.Id, sponsorMemberIds []server.Id) error {
	if len(sponsors) == 0 || !server.SidecarMg1Linked() {
		return nil
	}
	userIds, err := configMemberUserIdsInTx(ctx, tx, networkId, sponsorMemberIds)
	if err != nil {
		return err
	}
	sponsoredSidecarIds := []server.Id{}
	sponsorUserIds := []server.Id{}
	unsponsoredSidecarIds := []server.Id{}
	for sidecarId, memberId := range sponsors {
		if memberId == nil {
			unsponsoredSidecarIds = append(unsponsoredSidecarIds, sidecarId)
			continue
		}
		userId, ok := userIds[*memberId]
		if !ok {
			unsponsoredSidecarIds = append(unsponsoredSidecarIds, sidecarId)
			continue
		}
		sponsoredSidecarIds = append(sponsoredSidecarIds, sidecarId)
		sponsorUserIds = append(sponsorUserIds, userId)
	}
	if 0 < len(sponsoredSidecarIds) {
		if _, err := tx.Exec(
			ctx,
			`
				UPDATE network_client_owner
				SET sponsor_user_id = sponsor.sponsor_user_id
				FROM
					unnest($2::uuid[], $3::uuid[]) AS sponsor (sidecar_id, sponsor_user_id),
					sidecar_identity_binding,
					sidecar_identity
				WHERE
					sidecar_identity_binding.sidecar_id = sponsor.sidecar_id AND
					sidecar_identity.sidecar_id = sponsor.sidecar_id AND
					sidecar_identity.network_id = $1 AND
					network_client_owner.client_id = sidecar_identity_binding.client_id AND
					network_client_owner.tagged AND
					network_client_owner.sponsor_user_id IS DISTINCT FROM sponsor.sponsor_user_id
			`,
			networkId,
			sponsoredSidecarIds,
			sponsorUserIds,
		); err != nil {
			return err
		}
	}
	if 0 < len(unsponsoredSidecarIds) {
		if _, err := tx.Exec(
			ctx,
			`
				UPDATE network_client_owner
				SET sponsor_user_id = NULL
				FROM sidecar_identity_binding, sidecar_identity
				WHERE
					sidecar_identity_binding.sidecar_id = ANY($2) AND
					sidecar_identity.sidecar_id = sidecar_identity_binding.sidecar_id AND
					sidecar_identity.network_id = $1 AND
					network_client_owner.client_id = sidecar_identity_binding.client_id AND
					network_client_owner.tagged AND
					network_client_owner.sponsor_user_id IS NOT NULL
			`,
			networkId,
			unsponsoredSidecarIds,
		); err != nil {
			return err
		}
	}
	return nil
}

// What one run did.
type ConfigLogEffectsResult struct {
	// entries whose effects this run applied
	AppliedCount int `json:"applied_count"`
	// the newest position the run walked
	Through uint64 `json:"through"`
	// more entries wait for the next run
	More bool `json:"more,omitempty"`
}

// One run of the network's effects through sequencer, the process's: every
// entry past what this process applied, then the sponsors where they may have
// moved. An error leaves the watermark where it was, so the next run starts
// again at the entry that failed.
func ApplyConfigLogEffects(ctx context.Context, sequencer *ConfigLogSequencer, networkId server.Id, settings *ConfigLogEffectsSettings) (*ConfigLogEffectsResult, error) {
	result := &ConfigLogEffectsResult{}
	if !server.SidecarMg1Linked() {
		return result, nil
	}
	read, err := sequencer.readConfigLogEffects(ctx, networkId, settings.EntryMaxCount)
	if err != nil {
		return nil, err
	}
	for _, effect := range read.effects {
		var applied bool
		if r := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				var err error
				applied, err = applyConfigLogEntryEffectInTx(ctx, tx, networkId, effect)
				server.Raise(err)
			})
		}); r != nil {
			return nil, fmt.Errorf("effects of entry %d: %v", effect.n, r)
		}
		if applied {
			result.AppliedCount += 1
		}
		sequencer.configLogEffectsApplied(networkId, effect.n)
	}
	if read.reconcileSponsors {
		if r := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(reconcileConfigSponsorsInTx(ctx, tx, networkId, read.sponsors, read.sponsorMemberIds))
			})
		}); r != nil {
			return nil, fmt.Errorf("sponsors: %v", r)
		}
	}
	sequencer.configLogEffectsApplied(networkId, read.through)
	result.Through, result.More = read.through, read.more
	return result, nil
}

type ApplyConfigLogEffectsArgs struct {
	NetworkId server.Id `json:"network_id"`
}

type ApplyConfigLogEffectsResult struct {
	ConfigLogEffectsResult
}

// The sequencer's Effects hook: schedules the network's effects task, at once,
// or with intent IntentDelay on. Best effort: a schedule that fails is logged,
// and the next append's, or the next run's, finds what this one was for.
func ScheduleConfigLogEffects(ctx context.Context, networkId server.Id, intent bool) {
	settings := DefaultConfigLogEffectsSettings()
	runAt := server.NowUtc()
	if intent {
		runAt = runAt.Add(settings.IntentDelay)
	}
	if r := server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			owner := session.NewLocalClientSession(ctx, "", nil)
			defer owner.Cancel()
			scheduleConfigLogEffectsInTx(tx, owner, networkId, runAt, settings)
		})
	}); r != nil {
		glog.Warningf("[config]%s effects task not scheduled: %v\n", networkId, r)
	}
}

func scheduleConfigLogEffectsInTx(tx server.PgTx, owner *session.ClientSession, networkId server.Id, runAt time.Time, settings *ConfigLogEffectsSettings) {
	task.ScheduleTaskInTx(
		tx,
		ApplyConfigLogEffectsTask,
		&ApplyConfigLogEffectsArgs{NetworkId: networkId},
		owner,
		task.RunOnce("config_log_effects", networkId),
		task.RunAt(runAt),
		task.MaxTime(settings.MaxTime),
	)
}

// One run of the network's effects through this process's sequencer. Without
// one it retries RetryDelay on.
func ApplyConfigLogEffectsTask(
	applyConfigLogEffects *ApplyConfigLogEffectsArgs,
	clientSession *session.ClientSession,
) (*ApplyConfigLogEffectsResult, error) {
	settings := DefaultConfigLogEffectsSettings()
	sequencer, err := CurrentConfigLogSequencer()
	if err != nil {
		return nil, task.WithRetryDelay(err, settings.RetryDelay)
	}
	result, err := ApplyConfigLogEffects(clientSession.Ctx, sequencer, applyConfigLogEffects.NetworkId, settings)
	if err != nil {
		return nil, task.WithRetryDelay(err, settings.RetryDelay)
	}
	if 0 < result.AppliedCount && glog.V(1) {
		glog.Infof("[config]%s applied the effects of %d entries through %d\n", applyConfigLogEffects.NetworkId, result.AppliedCount, result.Through)
	}
	return &ApplyConfigLogEffectsResult{ConfigLogEffectsResult: *result}, nil
}

// Runs again at once when the run left entries behind.
func ApplyConfigLogEffectsTaskPost(
	applyConfigLogEffects *ApplyConfigLogEffectsArgs,
	applyConfigLogEffectsResult *ApplyConfigLogEffectsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if applyConfigLogEffectsResult.More {
		scheduleConfigLogEffectsInTx(tx, clientSession, applyConfigLogEffects.NetworkId, server.NowUtc(), DefaultConfigLogEffectsSettings())
	}
	return nil
}
