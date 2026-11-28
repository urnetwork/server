// The §22.9 producer sources: the operator service's durable PostgreSQL state
// (its finalized event mirror checkpoint, chain-write intents and subnet
// tasks) read with bounded constant queries, and the chain's finalized
// balances and nonces of every scoped writer. The queries never interpolate an
// identity; rows are filtered in memory and identities are never rendered.
package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// The producer's finalized event mirror checkpoint for one deployment.
type subnetProcessMirror struct {
	present    bool
	nextBlock  uint64
	blockHash  common.Hash
	hashKnown  bool
	ageSeconds int64
}

// One nonterminal chain-write intent of the pinned deployment.
type subnetProcessIntent struct {
	from       common.Address
	nonce      uint64
	status     string
	attempts   int64
	ageSeconds int64
}

// One subnet task row.
type subnetProcessTask struct {
	function    string
	ageSeconds  int64
	errorCount  int64
	errorRaised bool
}

// The complete or explicitly partial producer view.
type subnetProcessProducer struct {
	mirror         subnetProcessMirror
	intents        []subnetProcessIntent
	intentsPartial bool
	tasks          []subnetProcessTask
	tasksPartial   bool
}

// The deployment key the producer stores: decimal chain id, colon, lowercase
// coordinator address, exactly as the server's namespace builds it.
func subnetProcessDeploymentKey(pin *SubnetReplayPin) string {
	return strconv.FormatUint(pin.chainId, 10) + ":" + strings.ToLower(pin.coordinator.Hex())
}

func subnetProcessInteger(value string) (int64, bool) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return parsed, err == nil
}

// Reads the three bounded producer views. A query or parse failure is a gap.
func readSubnetProcessProducer(ctx context.Context, env *probeEnv, pin *SubnetReplayPin, settings *SubnetReplaySettings) (*subnetProcessProducer, error) {
	producer := &subnetProcessProducer{}
	key := subnetProcessDeploymentKey(pin)
	limit := settings.ProducerMaximumRows
	rows, err := env.runner.pg(ctx, fmt.Sprintf(`
		SELECT deployment_key, high_water_block, block_hash,
			floor(extract(epoch FROM (now() AT TIME ZONE 'UTC') - update_time))::bigint
		FROM st_chain_sync
		WHERE singleton_id = 1
		ORDER BY deployment_key
		LIMIT %d`, limit+1))
	if err != nil {
		return nil, subnetProcessSourceError(ctx, err)
	}
	if limit < len(rows) {
		return nil, newSubnetReplayUnknown("producer-mirror-bound", nil)
	}
	for _, row := range rows {
		if len(row) != 4 {
			return nil, newSubnetReplayUnknown("producer-row-malformed", nil)
		}
		if row.str(0) != key {
			continue
		}
		next, nextOk := subnetProcessInteger(row.str(1))
		age, ageOk := subnetProcessInteger(row.str(3))
		if !nextOk || !ageOk || next < 0 {
			return nil, newSubnetReplayUnknown("producer-row-malformed", nil)
		}
		producer.mirror = subnetProcessMirror{present: true, nextBlock: uint64(next), ageSeconds: age}
		if hash := row.str(2); hash != "" {
			parsed, ok := subnetReplayHash(strings.ToLower(hash))
			if !ok {
				return nil, newSubnetReplayUnknown("producer-row-malformed", nil)
			}
			producer.mirror.blockHash, producer.mirror.hashKnown = parsed, true
		}
	}

	rows, err = env.runner.pg(ctx, fmt.Sprintf(`
		SELECT deployment_id, chain_id, from_address, nonce, status, attempt_count,
			floor(extract(epoch FROM (now() AT TIME ZONE 'UTC') - update_time))::bigint
		FROM st_transaction_intent
		WHERE status NOT IN ('finalized', 'failed')
		ORDER BY update_time
		LIMIT %d`, settings.MaximumIntents+1))
	if err != nil {
		return nil, subnetProcessSourceError(ctx, err)
	}
	if settings.MaximumIntents < len(rows) {
		rows = rows[:settings.MaximumIntents]
		producer.intentsPartial = true
	}
	for _, row := range rows {
		if len(row) != 7 {
			return nil, newSubnetReplayUnknown("producer-row-malformed", nil)
		}
		chainId, chainOk := subnetProcessInteger(row.str(1))
		nonce, nonceOk := subnetProcessInteger(row.str(3))
		attempts, attemptsOk := subnetProcessInteger(row.str(5))
		age, ageOk := subnetProcessInteger(row.str(6))
		if !chainOk || !nonceOk || !attemptsOk || !ageOk || nonce < 0 || !common.IsHexAddress(row.str(2)) {
			return nil, newSubnetReplayUnknown("producer-row-malformed", nil)
		}
		if row.str(0) != pin.deploymentId || uint64(chainId) != pin.chainId {
			continue
		}
		switch status := row.str(4); status {
		case "prepared", "signed", "broadcast", "mined", "uncertain":
			producer.intents = append(producer.intents, subnetProcessIntent{from: common.HexToAddress(row.str(2)), nonce: uint64(nonce), status: status, attempts: attempts, ageSeconds: age})
		default:
			return nil, newSubnetReplayUnknown("producer-intent-status-unsupported", nil)
		}
	}

	rows, err = env.runner.pg(ctx, fmt.Sprintf(`
		SELECT function_name,
			floor(extract(epoch FROM (now() AT TIME ZONE 'UTC') - run_at))::bigint,
			reschedule_error_count, reschedule_error IS NOT NULL
		FROM pending_task
		WHERE function_name LIKE '%%/work.St%%'
		ORDER BY function_name, run_at
		LIMIT %d`, limit+1))
	if err != nil {
		return nil, subnetProcessSourceError(ctx, err)
	}
	if limit < len(rows) {
		rows = rows[:limit]
		producer.tasksPartial = true
	}
	for _, row := range rows {
		age, ageOk := subnetProcessInteger(row.str(1))
		errorCount, countOk := subnetProcessInteger(row.str(2))
		if len(row) != 4 || !ageOk || !countOk || (row.str(3) != "t" && row.str(3) != "f") {
			return nil, newSubnetReplayUnknown("producer-row-malformed", nil)
		}
		name := row.str(0)
		producer.tasks = append(producer.tasks, subnetProcessTask{function: name[strings.LastIndex(name, ".")+1:], ageSeconds: age, errorCount: errorCount, errorRaised: row.str(3) == "t"})
	}
	return producer, nil
}

// A canceled run keeps its context error; every other failure is a gap whose
// private cause stays in memory.
func subnetProcessSourceError(ctx context.Context, err error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return newSubnetReplayUnknown("producer-database-unavailable", err)
}

// One scoped writer's finalized balance and nonce.
type subnetProcessWriter struct {
	address common.Address
	balance *hexutil.Big
	nonce   *hexutil.Uint64
}

// Reads every writer's balance and next nonce at the checkpoint, hash pinned.
func readSubnetProcessWriters(ctx context.Context, rpc *subnetReplayRpc, checkpoint subnetReplayEvmCheckpoint, addresses []common.Address) (map[common.Address]*subnetProcessWriter, error) {
	selector := map[string]any{"blockHash": checkpoint.hash, "requireCanonical": true}
	writers := map[common.Address]*subnetProcessWriter{}
	calls := []*subnetReplayRpcCall{}
	for _, address := range addresses {
		if _, ok := writers[address]; ok || address == (common.Address{}) {
			continue
		}
		writer := &subnetProcessWriter{address: address}
		writers[address] = writer
		calls = append(calls,
			&subnetReplayRpcCall{method: "eth_getBalance", params: []any{address, selector}, result: &writer.balance},
			&subnetReplayRpcCall{method: "eth_getTransactionCount", params: []any{address, selector}, result: &writer.nonce},
		)
	}
	if err := rpc.call(ctx, calls...); err != nil {
		return nil, err
	}
	if err := subnetReplayRpcFailed(calls...); err != nil {
		return nil, err
	}
	for _, writer := range writers {
		if writer.balance == nil || writer.nonce == nil {
			return nil, newSubnetReplayUnknown("writer-state-missing", nil)
		}
	}
	return writers, nil
}
