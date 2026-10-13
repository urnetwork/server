// A run-once row's args may be replaced only between claims, under the same
// claim generation that fences every handback.
package task

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// The outcome of one guarded replacement attempt.
type RunOnceArgsReplacement int

const (
	// No row is pending under the key.
	RunOnceArgsAbsent RunOnceArgsReplacement = iota
	// A lease holds the row. Its run in flight keeps its args and handback.
	RunOnceArgsLeased
	// The current args were kept.
	RunOnceArgsKept
	// The row holds the replacement under a new claim generation.
	RunOnceArgsReplaced
	// The row changed after it was read, so nothing was written.
	RunOnceArgsChanged
)

// Replaces the args of the row pending under runOnce, only while no lease holds
// it, in the caller's read committed transaction. replace receives the current
// args and returns their replacement, or false to keep them; it must not perform
// I/O. The write compares the task, claim generation and args it read, so a
// concurrent claim either sees the replacement or turns the write into a no-op.
// The write also advances the claim generation: the handback, lease refresh or
// checkpoint of a worker whose lease lapsed no longer matches and is refused.
// Run-once scheduling keeps its merge; no other writer changes behavior.
func ReplaceUnleasedRunOnceArgsInTx(
	ctx context.Context,
	tx server.PgTx,
	runOnce *RunOnceOption,
	now time.Time,
	replace func(argsJson string) (string, bool),
) RunOnceArgsReplacement {
	if runOnce == nil {
		panic(errors.New("run-once args replacement requires a run-once key"))
	}
	now = now.UTC()
	var taskId server.Id
	var argsJson string
	var claimGeneration int64
	var leased bool
	err := tx.QueryRow(
		ctx,
		`
			SELECT task_id, args_json, claim_generation, $2 < release_time
			FROM pending_task
			WHERE run_once_key = $1
		`,
		runOnce.String(),
		now,
	).Scan(&taskId, &argsJson, &claimGeneration, &leased)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunOnceArgsAbsent
	}
	server.Raise(err)
	if leased {
		return RunOnceArgsLeased
	}
	replacement, ok := replace(argsJson)
	if !ok {
		return RunOnceArgsKept
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE pending_task
			SET
				args_json = $4,
				claim_generation = claim_generation + 1
			WHERE
				task_id = $1 AND
				claim_generation = $2 AND
				args_json = $3 AND
				release_time <= $5
		`,
		taskId,
		claimGeneration,
		argsJson,
		replacement,
		now,
	))
	if tag.RowsAffected() != 1 {
		return RunOnceArgsChanged
	}
	return RunOnceArgsReplaced
}
