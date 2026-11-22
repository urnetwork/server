package model

// Batched forms of the reads and writes each live connection announcement
// makes on its own every sync round (connect's transport_announce.go;
// LBALTOPT2.md R18). A connect process runs a sync round for every connection
// about every 30 seconds, and each round's read and stats write were their
// own round trips: about 1.5% of connect's CPU on 2026-11-20. Connect's
// announce batcher gathers the rounds that come due together, and these
// functions serve them with one statement or one pipeline where each
// connection had its own. Each keeps the per-connection function's semantics
// and reports each connection's own failure.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// GetProvideModes for many clients: their mirrors in one Redis pipeline, and
// each client whose mirror is missing or undecodable from postgres on its own,
// as GetProvideModes does. A client whose postgres read raises gets the raise
// as its error and leaves the others' reads alone. The results are in the
// order of clientIds, which may repeat a client.
func GetProvideModesBatch(ctx context.Context, clientIds []server.Id) (provideModes []map[ProvideMode]bool, errs []error) {
	provideModes = make([]map[ProvideMode]bool, len(clientIds))
	errs = make([]error, len(clientIds))
	if len(clientIds) == 0 {
		return
	}
	provideModesListJsons := make([]string, len(clientIds))
	server.Redis(ctx, func(r server.RedisClient) {
		// one GET a client, each on its own {pm_<clientId>} slot: a plain
		// pipeline routes each command to its node
		cmds := make([]*redis.StringCmd, len(clientIds))
		r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for i, clientId := range clientIds {
				cmds[i] = pipe.Get(ctx, provideModesKey(clientId))
			}
			return nil
		})
		for i, cmd := range cmds {
			// a failed read is a missing mirror, as in GetProvideModes
			provideModesListJsons[i], _ = cmd.Result()
		}
	})
	// a decoded mirror answers at once; each other client reads postgres on
	// its own goroutine, as each read did on its connection's, so that the
	// batch takes as long as its slowest read, not their sum. What a read
	// raises is its client's error, without the log HandleError writes.
	var fallbacks sync.WaitGroup
	for i, clientId := range clientIds {
		if decoded, ok := decodeProvideModesMirror(provideModesListJsons[i]); ok {
			provideModes[i] = decoded
			continue
		}
		fallbacks.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					if raisedErr, ok := r.(error); ok {
						errs[i] = raisedErr
					} else {
						errs[i] = fmt.Errorf("%v", r)
					}
				}
			}()
			provideModes[i] = provideModesFromMirror(ctx, clientId, provideModesListJsons[i])
		})
	}
	fallbacks.Wait()
	return
}

// One sync round's read (GetProvideKeyChanges): the client, and the start of
// the window whose provide key changes it counts.
type ProvideKeyChangesRead struct {
	ClientId server.Id
	MinTime  time.Time
}

// What GetProvideKeyChanges returns for one read, or its own error.
type ProvideKeyChanges struct {
	ChangedCount int
	ProvideModes map[ProvideMode]bool
	Err          error
}

// Each read's count in one statement: the lateral count runs per read, as
// GetProvideKeyChanges's statement runs, on the (client_id, change_time) key.
const provideKeyChangesBatchSql = `
	SELECT
		provide_read.read_index,
		read_changes.changed_count
	FROM unnest($1::uuid[], $2::timestamp[]) WITH ORDINALITY AS provide_read(client_id, min_time, read_index)
	CROSS JOIN LATERAL (
		SELECT COUNT(*) AS changed_count
		FROM provide_key_change
		WHERE
			provide_key_change.client_id = provide_read.client_id AND
			provide_read.min_time <= provide_key_change.change_time
	) read_changes
`

// GetProvideKeyChangesBatch is GetProvideKeyChanges for many reads: their
// change counts in one statement and their provide modes through
// GetProvideModesBatch. The statement raises for every read, as each read's
// own statement would in the same outage; a read whose provide modes fail
// gets its own error. The results are in the order of reads.
func GetProvideKeyChangesBatch(ctx context.Context, reads []ProvideKeyChangesRead) []ProvideKeyChanges {
	results := make([]ProvideKeyChanges, len(reads))
	if len(reads) == 0 {
		return results
	}
	clientIds := make([]server.Id, len(reads))
	minTimes := make([]time.Time, len(reads))
	for i, read := range reads {
		clientIds[i] = read.ClientId
		minTimes[i] = read.MinTime
	}
	counted := make([]bool, len(reads))
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, provideKeyChangesBatchSql, clientIds, minTimes)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var readIndex int64
				var changedCount int
				err := result.Scan(&readIndex, &changedCount)
				server.Raise(err)
				// the ordinality counts from 1
				i := int(readIndex) - 1
				if i < 0 || len(reads) <= i {
					err := fmt.Errorf("provide key change read %d is not one of %d", readIndex, len(reads))
					server.Raise(err)
				}
				results[i].ChangedCount = changedCount
				counted[i] = true
			}
		})
	})
	for i := range reads {
		if !counted[i] {
			err := fmt.Errorf("provide key change read %d of %d has no count", i+1, len(reads))
			server.Raise(err)
		}
	}
	provideModes, errs := GetProvideModesBatch(ctx, clientIds)
	for i := range reads {
		results[i].ProvideModes = provideModes[i]
		results[i].Err = errs[i]
	}
	return results
}

// One connection's sync round stats (RecordClientReliabilityStatsRange).
type ClientReliabilityStatsRecord struct {
	NetworkId         server.Id
	ClientId          server.Id
	ClientAddressHash [32]byte
	StatsStartTime    time.Time
	StatsEndTime      time.Time
	Stats             ClientReliabilityStats
}

// Adds the counter pairs of ARGV, field then count, to the block hash KEYS[1]
// and sets its backstop ttl to the last ARGV, in seconds: one record's or
// several records' increments and the ttl in one atomic step, as the
// per-record transaction applied them.
const clientReliabilityStatsBatchScript = `
for i = 1, #ARGV - 1, 2 do
  redis.call('HINCRBY', KEYS[1], ARGV[i], ARGV[i + 1])
end
redis.call('EXPIRE', KEYS[1], ARGV[#ARGV])
return 0`

// The most counter fields one script of a batch adds, so that a batch holds
// its node for about as long as a few of the per-record transactions did. A
// record's fields for one block hash are never split between scripts.
const clientReliabilityStatsBatchScriptMaxFieldCount = 256

// RecordClientReliabilityStatsRangeBatch is RecordClientReliabilityStatsRange
// for many records in one pipeline: each block hash's increments from all the
// records, with its ttl, in one script, which keeps each record's increments
// to a hash atomic as its own transaction did. Every script names one key, so
// the pipeline routes each to its node. Best-effort, as each record's write
// is: a Redis error is swallowed, and a raise is logged.
func RecordClientReliabilityStatsRangeBatch(ctx context.Context, records []ClientReliabilityStatsRecord) {
	server.HandleError(func() {
		// each block hash's scripts, each script's counter pairs
		type statsKeyWrite struct {
			scriptArgs [][]any
		}
		statsKeyWrites := map[string]*statsKeyWrite{}
		// in the order the records first wrote them
		statsKeys := []string{}
		for _, record := range records {
			startBlockNumber, endBlockNumber, ok := clientReliabilityStatsBlockRange(record.ClientId, record.StatsStartTime, record.StatsEndTime)
			if !ok {
				continue
			}
			counters := record.Stats.counters()
			fieldPrefix := clientReliabilityStatsFieldPrefix(record.NetworkId, record.ClientId, record.ClientAddressHash)
			shard := clientReliabilityStatsShard(record.ClientId)
			recordArgs := []any{}
			for i, count := range counters {
				if count != 0 {
					recordArgs = append(recordArgs, fieldPrefix+string([]byte{byte(i)}), count)
				}
			}
			for blockNumber := startBlockNumber; blockNumber <= endBlockNumber; blockNumber += 1 {
				statsKey := clientReliabilityStatsKey(blockNumber, shard)
				keyWrite := statsKeyWrites[statsKey]
				if keyWrite == nil {
					keyWrite = &statsKeyWrite{
						scriptArgs: [][]any{{}},
					}
					statsKeyWrites[statsKey] = keyWrite
					statsKeys = append(statsKeys, statsKey)
				}
				last := len(keyWrite.scriptArgs) - 1
				if 2*clientReliabilityStatsBatchScriptMaxFieldCount < len(keyWrite.scriptArgs[last])+len(recordArgs) {
					keyWrite.scriptArgs = append(keyWrite.scriptArgs, []any{})
					last += 1
				}
				keyWrite.scriptArgs[last] = append(keyWrite.scriptArgs[last], recordArgs...)
			}
		}
		if len(statsKeys) == 0 {
			return
		}
		ttlSeconds := int64(clientReliabilityStatsRedisTtl / time.Second)
		server.Redis(ctx, func(r server.RedisClient) {
			r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for _, statsKey := range statsKeys {
					for _, args := range statsKeyWrites[statsKey].scriptArgs {
						// backstop ttl only — the rollup normally deletes
						// the block hash shortly after the block closes
						pipe.Eval(ctx, clientReliabilityStatsBatchScript, []string{statsKey}, append(args, ttlSeconds)...)
					}
				}
				return nil
			})
		})
	})
}
