// Required owners retain individual immutable identities while sharing one
// bounded insertion statement in the caller's existing transaction.
package task

import (
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A required allocation cannot be merged with another run-once producer.
type RequiredTaskBatchItem[T any] struct {
	Args    T
	RunOnce *RunOnceOption
}

// The caller must drain the batch and roll back on error. Exact returned IDs
// and run-once keys prove every required owner was inserted; a conflicting row
// is never overwritten or accepted as a replacement for the new payload.
func QueueRequiredTasksInBatch[T any, R any](tx server.PgTx, batch server.PgBatch, taskFunction TaskFunction[T, R], items []RequiredTaskBatchItem[T], clientSession *session.ClientSession, opts ...any) {
	requireTaskPublicationBackend(tx, opts)
	if len(items) < 1 || len(items) > 64 {
		panic("required task batch must contain one through sixty-four owners")
	}
	ids := make([]server.Id, len(items))
	arguments := make([]string, len(items))
	hashes := make([][]byte, len(items))
	ports := make([]int, len(items))
	identities := make([]*string, len(items))
	runAts := make([]time.Time, len(items))
	keys := make([]string, len(items))
	priorities := make([]int, len(items))
	maxTimes := make([]int, len(items))
	expected := map[server.Id]string{}
	keySet := map[string]bool{}
	var functionName string
	for index, item := range items {
		if item.RunOnce == nil {
			panic("required task batch owner has no run-once key")
		}
		prepared := prepareTask(taskFunction, item.Args, clientSession, append(opts, item.RunOnce)...)
		functionName = prepared.functionName
		key := *prepared.runOnceKey
		if keySet[key] {
			panic("required task batch repeats an immutable owner")
		}
		if _, present := expected[prepared.taskId]; present {
			panic("required task batch repeats a task identity")
		}
		keySet[key] = true
		expected[prepared.taskId] = key
		ids[index], arguments[index], hashes[index], ports[index] = prepared.taskId, string(prepared.argsJson), prepared.clientAddressHash, prepared.clientAddressPort
		identities[index], runAts[index], keys[index] = prepared.byJwtJson, prepared.runAt, key
		priorities[index], maxTimes[index] = prepared.priority, prepared.maxTimeSeconds
	}
	batch.Queue(`INSERT INTO pending_task (
        task_id,function_name,args_json,client_address,client_address_hash,
        client_address_port,client_by_jwt_json,run_at,run_once_key,run_priority,
        run_max_time_seconds,claim_time,release_time)
        SELECT owner.task_id,$2,owner.args_json,'',owner.client_hash,owner.client_port,
            owner.client_identity,owner.run_at,owner.run_once_key,owner.run_priority,owner.max_time,$11,$11
        FROM unnest($1::uuid[],$3::text[],$4::bytea[],$5::int[],$6::text[],
            $7::timestamp[],$8::text[],$9::int[],$10::int[])
            AS owner(task_id,args_json,client_hash,client_port,client_identity,run_at,run_once_key,run_priority,max_time)
        ON CONFLICT (run_once_key) DO NOTHING RETURNING task_id,run_once_key`,
		ids, functionName, arguments, hashes, ports, identities, runAts, keys, priorities, maxTimes, time.Time{}).Query(func(rows pgx.Rows) error {
		seen := map[server.Id]bool{}
		for rows.Next() {
			var id server.Id
			var key string
			if err := rows.Scan(&id, &key); err != nil {
				return err
			}
			if expectedKey, present := expected[id]; !present || expectedKey != key || seen[id] {
				return fmt.Errorf("required task batch returned an unexpected owner")
			}
			seen[id] = true
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(seen) != len(expected) {
			return fmt.Errorf("required task batch lost an immutable owner")
		}
		server.AddTxCommitCount(tx, &taskSubmittedCounter, uint64(len(seen)))
		return nil
	})
}
