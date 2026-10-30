// One Redis read answers a tagged credential's state check (owner decision,
// 2026-10-26: Redis becomes the authority; REVOKE-FINAL.md §14). SQL and the
// session operation journal remain the durable record that a missing
// projection falls back to, so losing Redis data can only cost a query.
//
// A record is a hash keyed by the credential's network and one of
// revocationRecordShardCount shards of its lineage: the root client of a client
// credential, or the session of a network credential. Every client of a
// lineage therefore lives in one hash, a check is one HMGET on one slot, and a
// network with many lineages spreads over many slots. Network-wide events
// write every shard of the network, a bounded fan-out with no index to lose.
//
// Fields:
//   - n:user, the network's admin user id, and n:cred, the user's
//     credential_change_time in Unix microseconds. Both come from a SQL
//     fallback; n:cred only ever rises. n:del marks a deleted network.
//   - c:<client>, "a <device> <source client>" for an active client.
//   - r:<root>, "a" for an active root client.
//   - s:<session>, "a" for a session the network has not revoked.
//
// A positive projection is written only after the SQL fallback found the
// credential valid, from a read at most revocationRecordFreshness old, and
// expires after revocationRecordPositiveTtl. A revocation writes the tombstone
// "x" before its SQL change commits, and no projection overwrites a tombstone
// while it lives (revocationRecordTombstoneTtl). After the tombstone expires,
// the absent field sends the next check to SQL, which already refuses. A
// missing field is never read as "no revocation".
package session

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

const (
	revocationRecordShardCount = 64
	// a positive projection is rebuilt from SQL at least this often
	revocationRecordPositiveTtl = time.Hour
	// outlives every in-flight fallback, whose projection the freshness bound
	// already refuses after a minute
	revocationRecordTombstoneTtl = 10 * time.Minute
	// longer than every field's own lifetime; only ever extended
	revocationRecordKeyTtl = 25 * time.Hour
	// the oldest SQL read a projection may be built from, by Redis time
	revocationRecordFreshness = time.Minute
	// fields per tombstone script call
	revocationRecordFieldBatch = 512
)

const (
	revocationFieldUser       = "n:user"
	revocationFieldCredential = "n:cred"
	revocationFieldDeleted    = "n:del"
	revocationTombstone       = "x"
	revocationActive          = "a"
	revocationAbsent          = "-"
)

func revocationClientField(clientId server.Id) string   { return "c:" + clientId.String() }
func revocationRootField(rootId server.Id) string       { return "r:" + rootId.String() }
func revocationSessionField(sessionId server.Id) string { return "s:" + sessionId.String() }

func revocationRecordShard(lineageId server.Id) int {
	hash := fnv.New32a()
	hash.Write(lineageId[:])
	return int(hash.Sum32() % revocationRecordShardCount)
}

func revocationRecordShardKey(networkId server.Id, shard int) string {
	return fmt.Sprintf("{rv_%s_%d}", networkId, shard)
}

func revocationRecordKey(networkId server.Id, lineageId server.Id) string {
	return revocationRecordShardKey(networkId, revocationRecordShard(lineageId))
}

// Every shard key of a network, for network-wide writes.
func revocationRecordNetworkKeys(networkId server.Id) []string {
	keys := make([]string, revocationRecordShardCount)
	for shard := range revocationRecordShardCount {
		keys[shard] = revocationRecordShardKey(networkId, shard)
	}
	return keys
}

// Readers trust the record only once vault auth.yml enables it, after every
// writer is deployed. Writers always write, so the record is complete from the
// first enabled read.
var revocationRecordOverride atomic.Pointer[bool]
var vaultRevocationRecordEnabled = sync.OnceValue(func() bool {
	return loadAuthSettingBool("revocation_record_enabled")
})

func RevocationRecordEnabled() bool {
	if value := revocationRecordOverride.Load(); value != nil {
		return *value
	}
	return vaultRevocationRecordEnabled()
}

// Testing_SetRevocationRecordEnabled forces the read path for tests. The
// returned pop restores the previous override.
func Testing_SetRevocationRecordEnabled(value bool) func() {
	previous := revocationRecordOverride.Swap(&value)
	return func() { revocationRecordOverride.Store(previous) }
}

// The lineage a tagged credential's record is keyed by. A client credential
// needs its root claim; a network credential needs its session. Untagged
// credentials keep the SQL state query.
func revocationRecordLineage(byJwt *ByJwt) (server.Id, bool) {
	if byJwt.ClientId != nil {
		if byJwt.RootClientId == nil {
			return server.Id{}, false
		}
		return *byJwt.RootClientId, true
	}
	if byJwt.SessionId != nil {
		return *byJwt.SessionId, true
	}
	return server.Id{}, false
}

func usesRevocationRecord(byJwt *ByJwt) bool {
	if !RevocationRecordEnabled() {
		return false
	}
	_, ok := revocationRecordLineage(byJwt)
	return ok
}

type revocationVerdict uint8

const (
	// the record cannot answer; SQL does
	revocationMiss revocationVerdict = iota
	revocationValid
	revocationNoActiveRow
	revocationRotated
	revocationSessionRevoked
)

// The fields one check reads, in a fixed order.
type revocationRecordQuery struct {
	key           string
	clientField   string
	rootField     string
	sessionField  string
	parentClient  *server.Id
	fields        []string
	clientIndex   int
	rootIndex     int
	sessionIndex  int
	credentialIdx int
}

func newRevocationRecordQuery(byJwt *ByJwt, lineage server.Id, parentClientId *server.Id) *revocationRecordQuery {
	query := &revocationRecordQuery{
		key:          revocationRecordKey(byJwt.NetworkId, lineage),
		parentClient: parentClientId,
		fields:       []string{revocationFieldUser, revocationFieldCredential, revocationFieldDeleted},
		clientIndex:  -1,
		rootIndex:    -1,
		sessionIndex: -1,
	}
	query.credentialIdx = 1
	if byJwt.ClientId != nil {
		query.clientIndex = len(query.fields)
		query.clientField = revocationClientField(*byJwt.ClientId)
		query.fields = append(query.fields, query.clientField)
		// the session branch never reads the root (REVOKE-FINAL.md §1)
		if byJwt.SessionId == nil && *byJwt.RootClientId != *byJwt.ClientId {
			query.rootIndex = len(query.fields)
			query.rootField = revocationRootField(*byJwt.RootClientId)
			query.fields = append(query.fields, query.rootField)
		}
	}
	if byJwt.SessionId != nil {
		query.sessionIndex = len(query.fields)
		query.sessionField = revocationSessionField(*byJwt.SessionId)
		query.fields = append(query.fields, query.sessionField)
	}
	return query
}

// Decides a credential from one HMGET reply, with the SQL path's precedence:
// a revoked session first, then a missing or mismatched row, then rotation.
func (self *revocationRecordQuery) verdict(byJwt *ByJwt, values []any) (revocationVerdict, time.Time) {
	value := func(index int) (string, bool) {
		if index < 0 || len(values) <= index || values[index] == nil {
			return "", false
		}
		text, ok := values[index].(string)
		return text, ok
	}
	if session, ok := value(self.sessionIndex); ok && session == revocationTombstone {
		return revocationSessionRevoked, time.Time{}
	}
	if _, ok := value(2); ok {
		return revocationNoActiveRow, time.Time{}
	}
	user, userOk := value(0)
	credential, credentialOk := value(self.credentialIdx)
	if !userOk || !credentialOk {
		return revocationMiss, time.Time{}
	}
	if user != byJwt.UserId.String() {
		return revocationNoActiveRow, time.Time{}
	}
	credentialMicros, err := strconv.ParseInt(credential, 10, 64)
	if err != nil {
		return revocationMiss, time.Time{}
	}
	if 0 <= self.clientIndex {
		client, ok := value(self.clientIndex)
		if !ok {
			return revocationMiss, time.Time{}
		}
		if client == revocationTombstone {
			return revocationNoActiveRow, time.Time{}
		}
		parts := strings.Fields(client)
		if len(parts) != 3 || parts[0] != revocationActive {
			return revocationMiss, time.Time{}
		}
		if byJwt.DeviceId != nil && parts[1] != byJwt.DeviceId.String() {
			return revocationNoActiveRow, time.Time{}
		}
		if self.parentClient != nil && parts[2] != self.parentClient.String() {
			return revocationNoActiveRow, time.Time{}
		}
	}
	if 0 <= self.sessionIndex {
		session, ok := value(self.sessionIndex)
		if !ok || session != revocationActive {
			return revocationMiss, time.Time{}
		}
	} else if 0 <= self.rootIndex {
		root, ok := value(self.rootIndex)
		if !ok {
			return revocationMiss, time.Time{}
		}
		if root == revocationTombstone {
			return revocationNoActiveRow, time.Time{}
		}
		if root != revocationActive {
			return revocationMiss, time.Time{}
		}
	}
	credentialChangeTime := time.UnixMicro(credentialMicros).UTC()
	if byJwt.CreateTime.Before(credentialChangeTime) {
		return revocationRotated, credentialChangeTime
	}
	return revocationValid, time.Time{}
}

var revocationRecordCheckCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork", Subsystem: "jwt", Name: "state_record_checks_total",
	Help: "Tagged-credential state checks read from the Redis revocation record, by trusted caller and operation, credential shape and outcome: valid or refused by the record, a miss the SQL fallback answered, or unavailable.",
}, []string{"caller", "operation", "credential", "outcome"})

var revocationRecordWriteCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork", Subsystem: "session", Name: "revocation_record_writes_total",
	Help: "Redis revocation record writes by kind: client, session, credential and network revocations, and projections built from a SQL fallback; outcome success, failure, or stale for a projection refused by the freshness bound.",
}, []string{"kind", "outcome"})

func init() {
	prometheus.MustRegister(revocationRecordCheckCounter, revocationRecordWriteCounter)
}

func observeRevocationRecordCheck(ctx context.Context, byJwt *ByJwt, outcome string) {
	labels := stateQuerySourceLabels[stateQuerySourceFromContext(ctx)]
	credential := "account"
	if byJwt.ClientId != nil {
		credential = "client"
	}
	revocationRecordCheckCounter.WithLabelValues(labels[0], labels[1], credential, outcome).Inc()
}

func observeRevocationRecordWrite(kind string, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	revocationRecordWriteCounter.WithLabelValues(kind, outcome).Inc()
}

// The one read of a check; a test stands in for an unavailable store.
var readRevocationRecord = func(ctx context.Context, key string, fields []string) (values []any, err error) {
	err = server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		values, err = r.HMGet(ctx, key, fields...).Result()
		return err
	})
	return
}

// A test fails every revocation write with this error, to prove a revocation
// that cannot reach the record does not commit.
var revocationRecordWriteFailure atomic.Pointer[error]

// Testing_FailRevocationRecordWrites fails every revocation record write with
// err until the returned function runs.
func Testing_FailRevocationRecordWrites(err error) func() {
	previous := revocationRecordWriteFailure.Swap(&err)
	return func() { revocationRecordWriteFailure.Store(previous) }
}

func injectedRevocationRecordWriteFailure() error {
	if failure := revocationRecordWriteFailure.Load(); failure != nil {
		return *failure
	}
	return nil
}

// Answers a tagged credential's state check from its record, or through the
// SQL fallback when the record cannot answer. A Redis failure is unavailable:
// the record is the authority, so nothing is accepted without it.
func validateByJwtStateRecord(ctx context.Context, byJwt *ByJwt, parentClientId *server.Id) error {
	if err := refuseRememberedByJwtState(ctx, byJwt, parentClientId); err != nil {
		return err
	}
	lineage, _ := revocationRecordLineage(byJwt)
	query := newRevocationRecordQuery(byJwt, lineage, parentClientId)
	values, err := readRevocationRecord(ctx, query.key, query.fields)
	if err != nil {
		observeRevocationRecordCheck(ctx, byJwt, "record_unavailable")
		return errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable)
	}
	verdict, credentialChangeTime := query.verdict(byJwt, values)
	switch verdict {
	case revocationValid:
		observeRevocationRecordCheck(ctx, byJwt, "record_valid")
		return nil
	case revocationSessionRevoked:
		observeRevocationRecordCheck(ctx, byJwt, "record_refused")
		return ErrSessionRevoked
	case revocationNoActiveRow:
		// Not remembered: a tombstone outlives a deactivation that failed to
		// commit by minutes, and a remembered refusal would hold it for the
		// refusal ttl. The record answers again for one read.
		observeRevocationRecordCheck(ctx, byJwt, "record_refused")
		return rejectNoActiveRow(byJwt)
	case revocationRotated:
		observeRevocationRecordCheck(ctx, byJwt, "record_refused")
		return rejectRotated(byJwt, credentialChangeTime)
	}
	observeRevocationRecordCheck(ctx, byJwt, "record_miss")
	return validateByJwtStateRecordFallback(ctx, byJwt, lineage, parentClientId)
}

// What a valid fallback read, for the projection.
type revocationRecordFacts struct {
	credentialChangeTime time.Time
	clientActive         bool
	deviceId             *server.Id
	sourceClientId       *server.Id
	rootMatches          bool
}

// The SQL state query decides; the record learns from a valid answer. A
// session credential first passes its network revocation marker and the
// session operation journal, which outlives the marker through every
// affected credential's horizon, so lost Redis state cannot revive it.
func validateByJwtStateRecordFallback(ctx context.Context, byJwt *ByJwt, lineage server.Id, parentClientId *server.Id) (returnErr error) {
	observed := server.NowUtc()
	if byJwt.SessionId != nil {
		if err := CheckSession(ctx, byJwt.NetworkId, *byJwt.SessionId); err != nil {
			return err
		}
	}
	key, rememberable := newStateRefusalKey(byJwt, parentClientId)
	source := stateQuerySourceFromContext(ctx)
	noActiveRow := false
	var facts *revocationRecordFacts
	server.Db(ctx, func(conn server.PgConn) {
		if byJwt.SessionId != nil {
			if returnErr = sessionJournalFence(ctx, conn, byJwt.NetworkId, *byJwt.SessionId, &byJwt.CreateTime); returnErr != nil {
				return
			}
		}
		noActiveRow, returnErr = validateByJwtStateQuery(ctx, conn, byJwt, parentClientId)
		if returnErr != nil {
			if rememberable && noActiveRow && stateRefusalClassified(*byJwt.ClientId) {
				row, level, provide := classifyStateRefusal(ctx, conn, *byJwt.ClientId)
				labels := stateQuerySourceLabels[source]
				stateRefusalClassCounter.WithLabelValues(labels[0], labels[1], row, level, provide).Inc()
			}
			return
		}
		facts = readRevocationRecordFacts(ctx, conn, byJwt, lineage)
	})
	if rememberable && noActiveRow {
		stateRefusals.remember(key, time.Now())
		stateRefusalCounters[source][stateRefusalStored].Inc()
	}
	if returnErr != nil || facts == nil {
		return
	}
	// The answer is already valid; a projection that cannot be written only
	// leaves the next check to SQL.
	projectRevocationRecord(ctx, byJwt, lineage, observed, facts)
	return nil
}

// Reads what the projection stores, after the state query found the
// credential valid. A client that is no longer active, or a root claim that
// is not the client's actual root, is not projected: deactivation tombstones
// only the actual root's shard.
func readRevocationRecordFacts(ctx context.Context, conn server.PgCanQuery, byJwt *ByJwt, lineage server.Id) *revocationRecordFacts {
	facts := &revocationRecordFacts{}
	var clientId *server.Id
	if byJwt.ClientId != nil {
		clientId = byJwt.ClientId
	}
	result, err := conn.Query(ctx, `
		SELECT
			network_user.credential_change_time,
			network_client.client_id IS NOT NULL,
			network_client.device_id,
			network_client.source_client_id
		FROM network_user
		INNER JOIN network ON
			network.admin_user_id = network_user.user_id AND
			network.network_id = $2
		LEFT JOIN network_client ON
			network_client.client_id = $3 AND
			network_client.network_id = network.network_id AND
			network_client.active
		WHERE network_user.user_id = $1
	`, byJwt.UserId, byJwt.NetworkId, clientId)
	found := false
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&facts.credentialChangeTime, &facts.clientActive, &facts.deviceId, &facts.sourceClientId))
			found = true
		}
	})
	if !found {
		return nil
	}
	if byJwt.ClientId != nil && facts.clientActive {
		root, err := ResolveClientRootInTx(ctx, conn, byJwt.NetworkId, *byJwt.ClientId)
		facts.rootMatches = err == nil && root == lineage
	}
	return facts
}

const revocationRecordProjectLua = `
local t=redis.call('TIME')
local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)
if now-tonumber(ARGV[1])>tonumber(ARGV[2]) then return 'stale' end
if redis.call('HEXISTS',KEYS[1],'n:del')==1 then return 'deleted' end
if redis.call('HEXISTS',KEYS[1],'n:user')==0 then redis.call('HSET',KEYS[1],'n:user',ARGV[3]) end
local credential=tonumber(redis.call('HGET',KEYS[1],'n:cred')) or -1
if tonumber(ARGV[4])>credential then redis.call('HSET',KEYS[1],'n:cred',ARGV[4]) end
for i=7,#ARGV,2 do
 if redis.call('HGET',KEYS[1],ARGV[i])~='x' then
  redis.call('HSET',KEYS[1],ARGV[i],ARGV[i+1])
  redis.call('HPEXPIRE',KEYS[1],tonumber(ARGV[5]),'FIELDS',1,ARGV[i])
 end
end
local ttl=redis.call('PTTL',KEYS[1])
if ttl<tonumber(ARGV[6]) then redis.call('PEXPIRE',KEYS[1],tonumber(ARGV[6])) end
return 'ok'
`

func revocationIdOrAbsent(id *server.Id) string {
	if id == nil {
		return revocationAbsent
	}
	return id.String()
}

// Projects a valid fallback answer into the credential's record.
func projectRevocationRecord(ctx context.Context, byJwt *ByJwt, lineage server.Id, observed time.Time, facts *revocationRecordFacts) {
	positiveTtl := revocationRecordPositiveTtl - time.Duration(rand.Int64N(int64(revocationRecordPositiveTtl/10)))
	args := []any{
		observed.UnixMilli(),
		revocationRecordFreshness.Milliseconds(),
		byJwt.UserId.String(),
		facts.credentialChangeTime.UnixMicro(),
		positiveTtl.Milliseconds(),
		revocationRecordKeyTtl.Milliseconds(),
	}
	if byJwt.ClientId != nil && facts.clientActive && facts.rootMatches {
		args = append(args, revocationClientField(*byJwt.ClientId), fmt.Sprintf("%s %s %s", revocationActive, revocationIdOrAbsent(facts.deviceId), revocationIdOrAbsent(facts.sourceClientId)))
		if byJwt.SessionId == nil && lineage != *byJwt.ClientId {
			// the state query found this root active in the network
			args = append(args, revocationRootField(lineage), revocationActive)
		}
	}
	if byJwt.SessionId != nil {
		args = append(args, revocationSessionField(*byJwt.SessionId), revocationActive)
	}
	var outcome string
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		outcome, err = r.Eval(ctx, revocationRecordProjectLua, []string{revocationRecordKey(byJwt.NetworkId, lineage)}, args...).Text()
		return err
	})
	if err == nil && outcome == "stale" {
		revocationRecordWriteCounter.WithLabelValues("projection", "stale").Inc()
		return
	}
	observeRevocationRecordWrite("projection", err)
}

const revocationRecordTombstoneLua = `
for i=3,#ARGV do redis.call('HSET',KEYS[1],ARGV[i],'x') end
if #ARGV>=3 then redis.call('HPEXPIRE',KEYS[1],tonumber(ARGV[1]),'FIELDS',#ARGV-2,unpack(ARGV,3)) end
local ttl=redis.call('PTTL',KEYS[1])
if ttl<tonumber(ARGV[2]) then redis.call('PEXPIRE',KEYS[1],tonumber(ARGV[2])) end
return #ARGV-2
`

const revocationRecordNetworkLua = `
if ARGV[1]=='del' then
 redis.call('HSET',KEYS[1],'n:del','1')
else
 local credential=tonumber(redis.call('HGET',KEYS[1],'n:cred')) or -1
 if tonumber(ARGV[2])>credential then redis.call('HSET',KEYS[1],'n:cred',ARGV[2]) end
end
local ttl=redis.call('PTTL',KEYS[1])
if ttl<tonumber(ARGV[3]) then redis.call('PEXPIRE',KEYS[1],tonumber(ARGV[3])) end
return 1
`

// Writes tombstones for the given fields of each key, every call before any
// result is trusted. One failure fails the whole write.
func writeRevocationTombstones(ctx context.Context, keyFields map[string][]string) error {
	if len(keyFields) == 0 {
		return nil
	}
	if err := injectedRevocationRecordWriteFailure(); err != nil {
		return err
	}
	return server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		commands, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for key, fields := range keyFields {
				for start := 0; start < len(fields); start += revocationRecordFieldBatch {
					end := min(len(fields), start+revocationRecordFieldBatch)
					args := []any{revocationRecordTombstoneTtl.Milliseconds(), revocationRecordKeyTtl.Milliseconds()}
					for _, field := range fields[start:end] {
						args = append(args, field)
					}
					pipe.Eval(ctx, revocationRecordTombstoneLua, []string{key}, args...)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, command := range commands {
			if err := command.Err(); err != nil {
				return err
			}
		}
		return nil
	})
}

func writeRevocationNetworkFields(ctx context.Context, networkIds []server.Id, kind string, value string) error {
	if len(networkIds) == 0 {
		return nil
	}
	if err := injectedRevocationRecordWriteFailure(); err != nil {
		return err
	}
	return server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		commands, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, networkId := range networkIds {
				for _, key := range revocationRecordNetworkKeys(networkId) {
					pipe.Eval(ctx, revocationRecordNetworkLua, []string{key}, kind, value, revocationRecordKeyTtl.Milliseconds())
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, command := range commands {
			if err := command.Err(); err != nil {
				return err
			}
		}
		return nil
	})
}

// The actual root of each client, by one bounded recursive read. A client
// whose chain does not reach a top-level row has no root, and so no credential
// whose record could project it.
func resolveClientRootsInTx(ctx context.Context, conn server.PgCanQuery, clientIds []server.Id) (map[server.Id]server.Id, error) {
	roots := map[server.Id]server.Id{}
	if len(clientIds) == 0 {
		return roots, nil
	}
	rows, err := conn.Query(ctx, `
		WITH RECURSIVE chain AS (
			SELECT client_id AS origin_id, client_id, source_client_id, 1 AS depth
			FROM network_client
			WHERE client_id = ANY($1)
			UNION ALL
			SELECT chain.origin_id, parent.client_id, parent.source_client_id, chain.depth + 1
			FROM chain
			INNER JOIN network_client parent ON parent.client_id = chain.source_client_id
			WHERE chain.depth < $2
		)
		SELECT origin_id, client_id FROM chain WHERE source_client_id IS NULL
	`, clientIds, ClientAncestryDepthLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var originId, rootId server.Id
		if err := rows.Scan(&originId, &rootId); err != nil {
			return nil, err
		}
		roots[originId] = rootId
	}
	return roots, rows.Err()
}

func addClientTombstones(keyFields map[string][]string, networkId server.Id, clientId server.Id, rootId server.Id) {
	key := revocationRecordKey(networkId, rootId)
	keyFields[key] = append(keyFields[key], revocationClientField(clientId))
	if clientId == rootId {
		keyFields[key] = append(keyFields[key], revocationRootField(rootId))
	}
}

// Tombstones clients the caller is deactivating in tx, before it commits. The
// caller holds the clients' row locks. A failure must fail the transaction: a
// committed deactivation without its tombstone could leave a projection
// accepting the client.
func RevokeClientRecordsInTx(ctx context.Context, tx server.PgCanQuery, networkId server.Id, clientIds []server.Id) error {
	clientNetworkIds := make(map[server.Id]server.Id, len(clientIds))
	for _, clientId := range clientIds {
		clientNetworkIds[clientId] = networkId
	}
	return RevokeNetworkClientRecordsInTx(ctx, tx, clientNetworkIds)
}

// RevokeClientRecordsInTx for clients of many networks, by client id: one root
// read and one pipelined write for the whole batch.
func RevokeNetworkClientRecordsInTx(ctx context.Context, tx server.PgCanQuery, clientNetworkIds map[server.Id]server.Id) (returnErr error) {
	if len(clientNetworkIds) == 0 {
		return nil
	}
	defer func() { observeRevocationRecordWrite("client", returnErr) }()
	clientIds := make([]server.Id, 0, len(clientNetworkIds))
	for clientId := range clientNetworkIds {
		clientIds = append(clientIds, clientId)
	}
	roots, err := resolveClientRootsInTx(ctx, tx, clientIds)
	if err != nil {
		return err
	}
	keyFields := map[string][]string{}
	for clientId, networkId := range clientNetworkIds {
		if rootId, ok := roots[clientId]; ok {
			addClientTombstones(keyFields, networkId, clientId, rootId)
		}
	}
	return writeRevocationTombstones(ctx, keyFields)
}

// A client row the caller deleted in tx, with the parent it had.
type RetiredClient struct {
	ClientId       server.Id
	NetworkId      server.Id
	SourceClientId *server.Id
}

// Tombstones clients the caller deleted in tx, before it commits. Their rows
// are gone from tx's view, so each root is found from the first ancestor not
// deleted in the same batch, or is the client itself.
func RevokeRetiredClientRecordsInTx(ctx context.Context, tx server.PgCanQuery, retired []RetiredClient) (returnErr error) {
	if len(retired) == 0 {
		return nil
	}
	defer func() { observeRevocationRecordWrite("client", returnErr) }()
	sources := map[server.Id]*server.Id{}
	for _, client := range retired {
		sources[client.ClientId] = client.SourceClientId
	}
	// the nearest ancestor that still has a row, or nil for a deleted top level
	surviving := map[server.Id]*server.Id{}
	lookups := []server.Id{}
	for _, client := range retired {
		current := client.ClientId
		var survivor *server.Id
		for depth := 0; depth < ClientAncestryDepthLimit; depth++ {
			source, deleted := sources[current]
			if !deleted {
				id := current
				survivor = &id
				break
			}
			if source == nil {
				break
			}
			current = *source
		}
		surviving[client.ClientId] = survivor
		if survivor != nil {
			lookups = append(lookups, *survivor)
		}
	}
	roots, err := resolveClientRootsInTx(ctx, tx, lookups)
	if err != nil {
		return err
	}
	keyFields := map[string][]string{}
	for _, client := range retired {
		survivor := surviving[client.ClientId]
		if survivor == nil {
			// a deleted top-level row and its deleted descendants
			rootId := client.ClientId
			for depth := 0; depth < ClientAncestryDepthLimit; depth++ {
				source := sources[rootId]
				if source == nil {
					break
				}
				rootId = *source
			}
			addClientTombstones(keyFields, client.NetworkId, client.ClientId, rootId)
			continue
		}
		if rootId, ok := roots[*survivor]; ok {
			addClientTombstones(keyFields, client.NetworkId, client.ClientId, rootId)
		}
	}
	return writeRevocationTombstones(ctx, keyFields)
}

// Tombstones revoked sessions in every shard of the network, before the
// revocation is reported. A session's credentials can name any root, and a
// network credential's record is keyed by the session itself.
func RevokeSessionRecords(ctx context.Context, networkId server.Id, sessionIds []server.Id) (returnErr error) {
	if len(sessionIds) == 0 {
		return nil
	}
	defer func() { observeRevocationRecordWrite("session", returnErr) }()
	fields := make([]string, 0, len(sessionIds))
	for _, sessionId := range sessionIds {
		fields = append(fields, revocationSessionField(sessionId))
	}
	keyFields := map[string][]string{}
	for _, key := range revocationRecordNetworkKeys(networkId) {
		keyFields[key] = fields
	}
	return writeRevocationTombstones(ctx, keyFields)
}

// Raises the credential cutoff of every network the user administers, in tx,
// before the credential change commits. Credentials created before the cutoff
// are refused by every record of those networks.
func RotateCredentialRecordsInTx(ctx context.Context, tx server.PgCanQuery, userId server.Id, cutoff time.Time) (returnErr error) {
	defer func() { observeRevocationRecordWrite("credential", returnErr) }()
	rows, err := tx.Query(ctx, `SELECT network_id FROM network WHERE admin_user_id = $1`, userId)
	if err != nil {
		return err
	}
	networkIds := []server.Id{}
	for rows.Next() {
		var networkId server.Id
		if err := rows.Scan(&networkId); err != nil {
			rows.Close()
			return err
		}
		networkIds = append(networkIds, networkId)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return writeRevocationNetworkFields(ctx, networkIds, "cred", strconv.FormatInt(cutoff.UnixMicro(), 10))
}

// Marks every record of a network deleted, before the deletion commits.
func RetireNetworkRecords(ctx context.Context, networkId server.Id) (returnErr error) {
	defer func() { observeRevocationRecordWrite("network", returnErr) }()
	return writeRevocationNetworkFields(ctx, []server.Id{networkId}, "del", "1")
}

// Testing_RevocationRecordField reads one field of a record key, for tests
// outside the package that check what a revocation wrote.
func Testing_RevocationRecordField(ctx context.Context, key string, field string) (value string, ok bool, returnErr error) {
	returnErr = server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		value, err = r.HGet(ctx, key, field).Result()
		if errors.Is(err, server.RedisNil) {
			return nil
		}
		ok = err == nil
		return err
	})
	return
}

// Testing_RevocationRecordKey is the record key of a lineage: a client
// credential's root, or a network credential's session.
func Testing_RevocationRecordKey(networkId server.Id, lineageId server.Id) string {
	return revocationRecordKey(networkId, lineageId)
}

// Testing_RevocationRecordShardKeys is every record key of a network.
func Testing_RevocationRecordShardKeys(networkId server.Id) []string {
	return revocationRecordNetworkKeys(networkId)
}

func Testing_RevocationClientField(clientId server.Id) string { return revocationClientField(clientId) }
func Testing_RevocationRootField(rootId server.Id) string     { return revocationRootField(rootId) }
func Testing_RevocationSessionField(sessionId server.Id) string {
	return revocationSessionField(sessionId)
}

const (
	Testing_RevocationCredentialField = revocationFieldCredential
	Testing_RevocationDeletedField    = revocationFieldDeleted
	Testing_RevocationTombstone       = revocationTombstone
)
