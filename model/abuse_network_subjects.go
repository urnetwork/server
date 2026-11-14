package model

// What a §21 action reaches inside one network, for ABUSE-M5-06's effects
// (controller/abuse_effects.go): its subjects, whose open contracts a
// suspension closes one subject at a time (§8.6, CloseAclSubjectContracts); the
// sidecar clients a standing change hints; its suspended row in
// network_abuse_state; and the other actions that still hold a unit a lift
// would free.
//
// A §21 suspension is server state feeding §8.6's hard deny, never a signed
// Suspend (I-4, plan-ABUSE-suspension-effects): nothing here appends to the
// authorization log or writes rev/. The suspended row names the case of a
// suspension that stands, and ends with the last one's lift. Rows exist only
// for restricted and suspended
// (plan-ABUSE-small-readings #3); the restricted rows a kill or a halt sets,
// and every read of standing, are ABUSE-M5-07's.
//
// Nothing here reads or writes MG5's tables unless the binary links the group
// (server.SidecarMg5Linked).

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// network_abuse_state's stored states (plan-ABUSE-small-readings #3).
const (
	networkAbuseStateRestricted = "restricted"
	networkAbuseStateSuspended  = "suspended"
)

// Up to limit of the network's subjects past after, in id order, and whether
// more follow. A subject is a sidecar (sidecar_identity, every status, since a
// revoked sidecar's contract may still be open) or a client no sidecar binding
// names and whose stored subject is itself or not yet set:
// CloseAclSubjectContracts takes either, and reaches a sidecar's bound clients
// of every generation and every window client whose stored subject is one of
// its clients, so each client of the network is covered by exactly one subject.
// The zero id starts at the first.
func AbuseNetworkSubjects(
	ctx context.Context,
	networkId server.Id,
	after server.Id,
	limit int,
) (subjectIds []server.Id, more bool) {
	subjectIds = []server.Id{}
	if limit <= 0 {
		return
	}
	sidecars := `SELECT NULL::uuid AS subject_id WHERE false`
	unbound := ``
	if server.SidecarMg1Linked() {
		sidecars = `
			SELECT sidecar_identity.sidecar_id AS subject_id
			FROM sidecar_identity
			WHERE sidecar_identity.network_id = $1
		`
		unbound = `
			AND NOT EXISTS (
				SELECT 1
				FROM sidecar_identity_binding
				WHERE sidecar_identity_binding.client_id = network_client.client_id
			)
		`
	}
	acting := ``
	if server.SidecarMg2Linked() {
		// a client is its own subject when the column says so, or says nothing
		// yet (D-ACL-3's backfill): a child not yet backfilled is then closed
		// as itself, which CloseAclSubjectContracts' stored-subject read would
		// otherwise miss
		acting = `
			AND (
				network_client.acl_subject_client_id IS NULL OR
				network_client.acl_subject_client_id = network_client.client_id
			)
		`
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT subject_id
				FROM (
					`+sidecars+`
					UNION
					SELECT network_client.client_id AS subject_id
					FROM network_client
					WHERE network_client.network_id = $1
					`+unbound+`
					`+acting+`
				) subject
				WHERE $2 < subject_id
				ORDER BY subject_id
				LIMIT $3
			`,
			networkId,
			after,
			limit+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var subjectId server.Id
				server.Raise(result.Scan(&subjectId))
				subjectIds = append(subjectIds, subjectId)
			}
		})
	})
	if limit < len(subjectIds) {
		subjectIds = subjectIds[:limit]
		more = true
	}
	return
}

// The clients a network's standing change hints on the core topic (§21.16):
// its active sidecar clients, which hold strict_exp tokens (§7.4), at most
// limit of them; a client past it learns at its hourly resync. Nothing in a
// binary that does not link MG0.
func AbuseNetworkHintClientIds(ctx context.Context, networkId server.Id, limit int) (clientIds []server.Id) {
	clientIds = []server.Id{}
	if !server.SidecarMg0Linked() || limit <= 0 {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_client.client_id
				FROM network_client
				INNER JOIN network_client_owner ON network_client_owner.client_id = network_client.client_id
				WHERE
					network_client.network_id = $1 AND
					network_client.active = true AND
					network_client_owner.strict_exp
				ORDER BY network_client.client_id
				LIMIT $2
			`,
			networkId,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})
	return
}

// Sets the network suspended under its case (§21.7): the row every gate's hard
// deny reads through ABUSE-M5-07. A row already there, a restricted one
// included, becomes this suspension, since suspended beats every other state.
// A second call for the same case changes nothing but update_time.
func SetNetworkAbuseSuspended(ctx context.Context, networkId server.Id, caseId server.Id, now time.Time) error {
	if !server.SidecarMg5Linked() {
		return ErrAbuseUnlinked
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_abuse_state (
					network_id,
					state,
					case_id,
					until_time,
					update_time
				) VALUES ($1, $2, $3, NULL, $4)
				ON CONFLICT (network_id) DO UPDATE
				SET
					state = EXCLUDED.state,
					case_id = EXCLUDED.case_id,
					until_time = NULL,
					update_time = EXCLUDED.update_time
			`,
			networkId,
			networkAbuseStateSuspended,
			caseId,
			now.UTC(),
		))
	})
	return nil
}

// Ends the network's suspension, as the lift of its last standing suspension
// does: the caller has checked that no other suspension of the network stands
// (GetNetworkStandingAbuseActions). Only a suspended row changes, and a missing
// row is nothing to end. restrictedUntil, when set and still ahead, leaves the
// network restricted until then under the row's case, since a suspension
// inside §21.7's window keeps it restricted unless review lifted it as wrong
// (plan-ABUSE-small-readings #4); nil removes the row. ended is whether a row
// changed.
func EndNetworkAbuseSuspended(
	ctx context.Context,
	networkId server.Id,
	restrictedUntil *time.Time,
	now time.Time,
) (ended bool, returnErr error) {
	if !server.SidecarMg5Linked() {
		return false, ErrAbuseUnlinked
	}
	now = now.UTC()
	server.Tx(ctx, func(tx server.PgTx) {
		if restrictedUntil != nil && now.Before(*restrictedUntil) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_abuse_state
					SET
						state = $2,
						until_time = $3,
						update_time = $4
					WHERE
						network_abuse_state.network_id = $1 AND
						network_abuse_state.state = $5
				`,
				networkId,
				networkAbuseStateRestricted,
				restrictedUntil.UTC(),
				now,
				networkAbuseStateSuspended,
			))
			ended = tag.RowsAffected() == 1
			return
		}
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_abuse_state
				WHERE
					network_abuse_state.network_id = $1 AND
					network_abuse_state.state = $2
			`,
			networkId,
			networkAbuseStateSuspended,
		))
		ended = tag.RowsAffected() == 1
	})
	return
}

// Whether network_abuse_state holds the network suspended, and under which
// case. ABUSE-M5-07's GetNetworkAbuseStanding is the read every gate uses; this
// one is the effects' own, to say what a lift left.
func GetNetworkAbuseSuspended(ctx context.Context, networkId server.Id) (caseId server.Id, suspended bool) {
	if !server.SidecarMg5Linked() {
		return server.Id{}, false
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_abuse_state.case_id
				FROM network_abuse_state
				WHERE network_abuse_state.network_id = $1 AND network_abuse_state.state = $2
			`,
			networkId,
			networkAbuseStateSuspended,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&caseId))
				suspended = true
			}
		})
	})
	return
}

// The network's actions that stand at now, oldest first: every action but a
// lift that no review lifted and whose own until, if it has one, is still
// ahead. What a lift reads, so it frees only the units no other standing action
// holds. Nothing in a binary that does not link MG5.
func GetNetworkStandingAbuseActions(ctx context.Context, networkId server.Id, now time.Time) (actions []*AbuseAction) {
	actions = []*AbuseAction{}
	if !server.SidecarMg5Linked() {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+abuseActionColumns+`
				FROM abuse_action
				WHERE
					abuse_action.network_id = $1 AND
					abuse_action.kind <> $2 AND
					abuse_action.review_state NOT IN ($3, $4) AND
					(abuse_action.until_time IS NULL OR $5 < abuse_action.until_time)
				ORDER BY abuse_action.create_time, abuse_action.action_id
			`,
			networkId,
			string(AbuseActionKindLift),
			string(AbuseReviewStateLifted),
			string(AbuseReviewStateLiftedWrong),
			now.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				actions = append(actions, scanAbuseAction(result))
			}
		})
	})
	return
}
