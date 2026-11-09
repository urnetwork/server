package model

// §7.11's admin event log (D-TEAM-11; §2.8 api-8): network_admin_event records
// each admin action the server executes on a network, in the action's own
// transaction, so an event exists exactly when its action commits.
//
// Each network has one hash chain. An event's seq is the network's previous seq
// plus one, starting at 1, and its chain_sha256 hashes the previous event's chain
// value (32 zero bytes before the first) with the event's canonical form
// (adminEventCanonical). seq and the chain are assigned under a
// transaction-scoped per-network lock, so concurrent admins of one network keep
// one chain. A transaction whose snapshot predates the lock (repeatable read)
// may read a stale head; its insert then meets the committed event and
// PostgreSQL refuses it as a serialization failure, which server.Tx reruns.
//
// An event holds opaque ids and fixed words only: who acted (a user, a client,
// an API key or the system), the action, what it acted on, the result and the
// request's id. No field can hold a client address or a name (I-13). Rows are
// kept 400 days from their event time (RemoveOldNetworkAdminEvents). Once a
// network's every event has aged out, its next event starts the chain again at
// seq 1, and each row keeps its predecessor's chain value, so the retained run
// verifies from its oldest row.
//
// Nothing here reads or writes the table unless MG1 is linked
// (server.SidecarMg1Linked).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Tunables of the admin event log.
type NetworkAdminEventSettings struct {
	// How long an event is kept after its event time. 400 days is §2.8 api-8's
	// published ttl, so a longer value breaks that promise. Non-positive keeps
	// nothing past the next sweep.
	Retention time.Duration
	// The most events one read of the log returns.
	ReadLimit int
}

func DefaultNetworkAdminEventSettings() *NetworkAdminEventSettings {
	return &NetworkAdminEventSettings{
		Retention: 400 * 24 * time.Hour,
		ReadLimit: 1000,
	}
}

// Who acted.
const (
	// the user of a network token
	AdminEventActorUser = "user"
	// a client token's client
	AdminEventActorClient = "client"
	// an API key
	AdminEventActorApiKey = "api_key"
	// the server itself, as in a taskworker job
	AdminEventActorSystem = "system"
)

// What an action acted on.
const (
	AdminEventTargetNetwork = "network"
	AdminEventTargetAuthKey = "auth_key"
	AdminEventTargetApiKey  = "api_key"
	// a team network's membership (§7.5), by member_id
	AdminEventTargetMember = "member"
)

// The actions. Each is a fixed word, never built from input.
const (
	AdminEventActionNetworkRename = "network.rename"
	AdminEventActionAuthKeyCreate = "auth_key.create"
	AdminEventActionAuthKeyRevoke = "auth_key.revoke"
	AdminEventActionApiKeyCreate  = "api_key.create"
	AdminEventActionApiKeyDelete  = "api_key.delete"
	// POST /network/team-create, in the new team's own chain
	AdminEventActionTeamCreate = "team.create"
	AdminEventActionMemberAdd  = "member.add"
)

// The results.
const (
	AdminEventResultOk = "ok"
)

// The widths of the table's text columns.
const (
	adminEventKindMaxLength   = 16
	adminEventActionMaxLength = 64
	adminEventResultMaxLength = 32
)

// The chain value before a network's first event.
var adminEventGenesisSha256 = make([]byte, sha256.Size)

// The domain of every chain hash, so no other hash in the server can pass for one.
const adminEventChainDomain = "urnetwork network_admin_event v1"

// An admin action to record. It has no field for a client address or a name.
type AdminEvent struct {
	NetworkId  server.Id
	ActorKind  string
	ActorId    *server.Id
	Action     string
	TargetKind string
	TargetId   *server.Id
	Result     string
	// the request that made the change, when a request did
	// (session.RequestIdFromContext)
	RequestId *server.Id
}

// An event as the log holds it.
type NetworkAdminEvent struct {
	AdminEvent
	Seq         int64
	EventTime   time.Time
	PrevSha256  []byte
	ChainSha256 []byte
}

// The actor of the request a session carries: the API key that authenticated
// it, else a client token's client, else the token's user.
func AdminEventActorOf(clientSession *session.ClientSession) (actorKind string, actorId *server.Id) {
	if clientSession.ApiKeyAuthenticated && clientSession.ApiKeyId != nil {
		apiKeyId := *clientSession.ApiKeyId
		return AdminEventActorApiKey, &apiKeyId
	}
	if clientSession.ByJwt == nil {
		return AdminEventActorSystem, nil
	}
	if clientSession.ByJwt.ClientId != nil {
		clientId := *clientSession.ByJwt.ClientId
		return AdminEventActorClient, &clientId
	}
	userId := clientSession.ByJwt.UserId
	return AdminEventActorUser, &userId
}

// The event of an action the session's request made on its network.
func NewSessionAdminEvent(clientSession *session.ClientSession, action string, targetKind string, targetId *server.Id) *AdminEvent {
	actorKind, actorId := AdminEventActorOf(clientSession)
	event := &AdminEvent{
		NetworkId:  clientSession.ByJwt.NetworkId,
		ActorKind:  actorKind,
		ActorId:    actorId,
		Action:     action,
		TargetKind: targetKind,
		TargetId:   targetId,
		Result:     AdminEventResultOk,
	}
	if requestId, ok := session.RequestIdFromContext(clientSession.Ctx); ok {
		event.RequestId = &requestId
	}
	return event
}

// Panics on an event no caller should build: every word is a code constant, so
// this is a programming error, not input.
func checkAdminEvent(event *AdminEvent) {
	check := func(name string, value string, maxLength int) {
		if value == "" || maxLength < len(value) {
			panic(fmt.Errorf("admin event %s %q is empty or longer than %d", name, value, maxLength))
		}
	}
	check("actor kind", event.ActorKind, adminEventKindMaxLength)
	check("action", event.Action, adminEventActionMaxLength)
	check("target kind", event.TargetKind, adminEventKindMaxLength)
	check("result", event.Result, adminEventResultMaxLength)
}

// The bytes an event's chain value hashes after its predecessor's: the domain,
// then each field in a fixed order, strings length-prefixed and absent ids
// marked, so no two events encode alike.
func adminEventCanonical(event *NetworkAdminEvent) []byte {
	b := &bytes.Buffer{}
	writeString := func(value string) {
		binary.Write(b, binary.BigEndian, uint16(len(value)))
		b.WriteString(value)
	}
	writeId := func(id *server.Id) {
		if id == nil {
			b.WriteByte(0)
			return
		}
		b.WriteByte(1)
		b.Write(id.Bytes())
	}
	writeString(adminEventChainDomain)
	b.Write(event.NetworkId.Bytes())
	binary.Write(b, binary.BigEndian, event.Seq)
	binary.Write(b, binary.BigEndian, event.EventTime.UTC().UnixMicro())
	writeString(event.ActorKind)
	writeId(event.ActorId)
	writeString(event.Action)
	writeString(event.TargetKind)
	writeId(event.TargetId)
	writeString(event.Result)
	writeId(event.RequestId)
	return b.Bytes()
}

// SHA-256(prev || canonical(event)).
func adminEventChain(prevSha256 []byte, event *NetworkAdminEvent) []byte {
	h := sha256.New()
	h.Write(prevSha256)
	h.Write(adminEventCanonical(event))
	return h.Sum(nil)
}

// Appends the event to its network's chain in the caller's transaction and
// returns it as stored. A binary that does not link MG1 records nothing and
// returns nil. Safe under any isolation level (see the file header).
func AddAdminEventInTx(ctx context.Context, tx server.PgTx, event *AdminEvent) *NetworkAdminEvent {
	return addAdminEventInTxAt(ctx, tx, event, server.NowUtc())
}

// AddAdminEventInTx at a given now. The event time never falls below the
// previous event's, so time order and seq order agree for the sweep.
func addAdminEventInTxAt(ctx context.Context, tx server.PgTx, event *AdminEvent, now time.Time) *NetworkAdminEvent {
	if !server.SidecarMg1Linked() {
		return nil
	}
	checkAdminEvent(event)
	// the network's chain lock, held to the end of the caller's transaction
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('network_admin_event:' || $1::uuid::text, 0))`,
		event.NetworkId,
	))

	stored := &NetworkAdminEvent{
		AdminEvent: *event,
		Seq:        1,
		EventTime:  now.UTC().Truncate(time.Microsecond),
		PrevSha256: adminEventGenesisSha256,
	}
	result, err := tx.Query(
		ctx,
		`
			SELECT seq, event_time, chain_sha256
			FROM network_admin_event
			WHERE network_id = $1
			ORDER BY seq DESC
			LIMIT 1
		`,
		event.NetworkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var seq int64
			var eventTime time.Time
			var chainSha256 []byte
			server.Raise(result.Scan(&seq, &eventTime, &chainSha256))
			stored.Seq = seq + 1
			stored.PrevSha256 = chainSha256
			if stored.EventTime.Before(eventTime) {
				stored.EventTime = eventTime
			}
		}
	})
	stored.ChainSha256 = adminEventChain(stored.PrevSha256, stored)

	// A conflict here means this transaction's snapshot predates the head the
	// lock now guards. Under repeatable read PostgreSQL raises it as a
	// serialization failure (server.Tx reruns the transaction); under read
	// committed the lock rules it out.
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_admin_event (
				network_id,
				seq,
				event_time,
				actor_kind,
				actor_id,
				action,
				target_kind,
				target_id,
				result,
				request_id,
				prev_sha256,
				chain_sha256
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (network_id, seq) DO NOTHING
		`,
		stored.NetworkId,
		stored.Seq,
		stored.EventTime,
		stored.ActorKind,
		stored.ActorId,
		stored.Action,
		stored.TargetKind,
		stored.TargetId,
		stored.Result,
		stored.RequestId,
		stored.PrevSha256,
		stored.ChainSha256,
	))
	if tag.RowsAffected() != 1 {
		panic(fmt.Errorf("admin event %d of network %s is taken under the chain lock", stored.Seq, stored.NetworkId))
	}
	return stored
}

// The network's events after afterSeq, in seq order, at most limit (the
// settings' ReadLimit when non-positive).
func GetNetworkAdminEvents(ctx context.Context, networkId server.Id, afterSeq int64, limit int) []*NetworkAdminEvent {
	if limit <= 0 {
		limit = DefaultNetworkAdminEventSettings().ReadLimit
	}
	events := []*NetworkAdminEvent{}
	if !server.SidecarMg1Linked() {
		return events
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_id,
					seq,
					event_time,
					actor_kind,
					actor_id,
					action,
					target_kind,
					target_id,
					result,
					request_id,
					prev_sha256,
					chain_sha256
				FROM network_admin_event
				WHERE network_id = $1 AND $2 < seq
				ORDER BY seq
				LIMIT $3
			`,
			networkId,
			afterSeq,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				event := &NetworkAdminEvent{}
				server.Raise(result.Scan(
					&event.NetworkId,
					&event.Seq,
					&event.EventTime,
					&event.ActorKind,
					&event.ActorId,
					&event.Action,
					&event.TargetKind,
					&event.TargetId,
					&event.Result,
					&event.RequestId,
					&event.PrevSha256,
					&event.ChainSha256,
				))
				events = append(events, event)
			}
		})
	})
	return events
}

var errAdminEventChainBroken = errors.New("admin event chain broken")

// Checks a run of one network's events in seq order: consecutive seqs, each
// event's predecessor hash equal to the previous event's chain value, and each
// chain value recomputed from its event. The run's first event anchors on its own
// stored predecessor hash, which is the genesis value when its seq is 1.
func verifyAdminEventChain(events []*NetworkAdminEvent) error {
	for i, event := range events {
		if event.Seq == 1 && !bytes.Equal(event.PrevSha256, adminEventGenesisSha256) {
			return fmt.Errorf("%w: event 1 does not start from the genesis value", errAdminEventChainBroken)
		}
		if 0 < i {
			previous := events[i-1]
			if event.NetworkId != previous.NetworkId {
				return fmt.Errorf("%w: event %d is another network's", errAdminEventChainBroken, event.Seq)
			}
			if event.Seq != previous.Seq+1 {
				return fmt.Errorf("%w: event %d follows %d", errAdminEventChainBroken, event.Seq, previous.Seq)
			}
			if !bytes.Equal(event.PrevSha256, previous.ChainSha256) {
				return fmt.Errorf("%w: event %d does not chain to %d", errAdminEventChainBroken, event.Seq, previous.Seq)
			}
		}
		if !bytes.Equal(adminEventChain(event.PrevSha256, event), event.ChainSha256) {
			return fmt.Errorf("%w: event %d does not hash to its chain value", errAdminEventChainBroken, event.Seq)
		}
	}
	return nil
}

// Verifies the network's whole retained log, reading it a page at a time.
func VerifyNetworkAdminEventChain(ctx context.Context, networkId server.Id) error {
	limit := DefaultNetworkAdminEventSettings().ReadLimit
	var previous *NetworkAdminEvent
	afterSeq := int64(0)
	for {
		events := GetNetworkAdminEvents(ctx, networkId, afterSeq, limit)
		if len(events) == 0 {
			return nil
		}
		run := events
		if previous != nil {
			run = append([]*NetworkAdminEvent{previous}, events...)
		}
		if err := verifyAdminEventChain(run); err != nil {
			return err
		}
		previous = events[len(events)-1]
		afterSeq = previous.Seq
		if len(events) < limit {
			return nil
		}
	}
}

// Deletes the events whose event time is before minTime, in bounded batches.
const removeOldNetworkAdminEventsSql = `
	DELETE FROM network_admin_event
	WHERE (network_id, seq) IN (
		SELECT network_id, seq
		FROM network_admin_event
		WHERE event_time < $1
		ORDER BY event_time
		LIMIT $2
		FOR UPDATE SKIP LOCKED
	) AND event_time < $1
`

// Removes the events older than minTime, oldest first, for at most the
// retention run budget; the next run takes the rest of a backlog.
func RemoveOldNetworkAdminEvents(ctx context.Context, minTime time.Time) (removedCount int64) {
	if !server.SidecarMg1Linked() {
		return 0
	}
	removedCount, _ = removeRetentionBatches(
		ctx,
		RetentionTableNetworkAdminEvent,
		retentionBatchSize,
		time.Now().Add(retentionRunBudget),
		func(tx server.PgTx) int64 {
			tag := server.RaisePgResult(tx.Exec(ctx, removeOldNetworkAdminEventsSql, minTime.UTC(), retentionBatchSize))
			return tag.RowsAffected()
		},
	)
	return
}
