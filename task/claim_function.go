// Registered function lanes cannot be hidden by another function's older
// backlog. One indexed lane turn alternates with the original global queue,
// and it goes to the next lane in rotation order that has due work.
package task

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Lanes one probe examines from the rotation position. Most registered lanes
// are empty at any time; a lane turn passes up to this many in one statement.
const claimFunctionProbeLimit = 16

// A lane found without due work is passed without a seek for this long, so
// each process seeks an empty lane about once per interval however often its
// loops take lane turns. New work in that lane waits at most this long.
const claimFunctionEmptyLaneTtl = 5 * time.Second

// Each probed lane is one seek on the function index, limited to its first due
// row, so no lane reads another lane's rows however large that backlog is. The
// lane cursor's full order makes the function index the only presorted path;
// a scan of the global poll order expecting an early match would filter the
// whole due queue for an empty lane. The chosen lane's first due block bounds
// its cursor, so index entries the probe already passed are not read twice.
const claimFunctionProbeSql = `SELECT lane.position,due.available_block FROM unnest($1::text[]) WITH ORDINALITY AS lane(name,position)
 CROSS JOIN LATERAL (SELECT available_block FROM pending_task
 WHERE regexp_replace(function_name, '/v[0-9]+', '', 'g') = lane.name AND available_block <= (SELECT $2::bigint)
 ORDER BY available_block, run_priority DESC, run_max_time_seconds DESC, task_id LIMIT 1) AS due
 ORDER BY lane.position LIMIT 1`

// Rotation is shared by this worker's Run and EvalTasks callers. An isolated
// lane handed from refill to the next claim belongs only to that Run's poll.
func (self *TaskWorker) nextClaimFunction(options taskClaimOptions) string {
	if !self.settings.FairClaimFunctions {
		return ""
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.draining || self.runCtx.Err() != nil || len(self.claimFunctionNames) == 0 {
		return ""
	}
	if options.poll != nil && !options.ordinaryOnly && options.poll.isolatedFunction != "" {
		name := options.poll.isolatedFunction
		options.poll.isolatedFunction = ""
		return name
	}
	turn := self.claimFunctionTurn
	self.claimFunctionTurn++
	if turn%2 != 0 {
		return ""
	}
	name := self.claimFunctionNames[(turn/2)%uint64(len(self.claimFunctionNames))]
	canonical := self.claimTargetName(name)
	if limit := self.settings.TargetClaimLimits[canonical]; limit != 0 && limit <= self.claimTargetCounts[canonical] {
		return ""
	}
	return name
}

// Up to claimFunctionProbeLimit lanes from start in rotation order that are
// neither saturated nor recently found empty, with each lane's distance from
// start.
func (self *TaskWorker) claimFunctionWindow(start string, now time.Time) (names []string, steps []int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	first := -1
	for index, name := range self.claimFunctionNames {
		if name == start {
			first = index
		}
	}
	if first < 0 {
		return []string{start}, []int{0}
	}
	for step := range len(self.claimFunctionNames) {
		if claimFunctionProbeLimit <= len(names) {
			break
		}
		name := self.claimFunctionNames[(first+step)%len(self.claimFunctionNames)]
		canonical := self.claimTargetName(name)
		if limit := self.settings.TargetClaimLimits[canonical]; limit != 0 && limit <= self.claimTargetCounts[canonical] {
			continue
		}
		if now.Before(self.claimFunctionEmptyUntil[name]) {
			continue
		}
		names = append(names, name)
		steps = append(steps, step)
	}
	return
}

// Records lanes found without due work, and moves the shared rotation forward
// by whole lanes, keeping the alternation with ordinary turns.
func (self *TaskWorker) passClaimFunctions(empty []string, steps int, now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, name := range empty {
		self.claimFunctionEmptyUntil[name] = now.Add(claimFunctionEmptyLaneTtl)
	}
	self.claimFunctionTurn += 2 * uint64(steps)
}

// The first lane in start's window with due work, or "" when none has any,
// with the block of that lane's first due row, or zero without a probe. Lanes
// before it are recorded empty. The next lane turn starts after the chosen
// lane, or after the window.
func (self *TaskWorker) firstDueClaimFunction(ctx context.Context, tx server.PgTx, start string, nowBlock int64) (string, int64, error) {
	now := self.claimNow()
	names, steps := self.claimFunctionWindow(start, now)
	switch {
	case len(names) == 0:
		return "", 0, nil
	case len(names) == 1 && names[0] == start:
		// The lane cursor itself is this lane's only seek.
		return start, 0, nil
	}
	var position, firstBlock int64
	err := tx.QueryRow(ctx, claimFunctionProbeSql, names, nowBlock).Scan(&position, &firstBlock)
	if errors.Is(err, pgx.ErrNoRows) {
		self.passClaimFunctions(names, steps[len(steps)-1], now)
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	if position < 1 || int64(len(names)) < position {
		return "", 0, errors.New("task claim lane probe returned an unknown lane")
	}
	self.passClaimFunctions(names[:position-1], steps[position-1], now)
	return names[position-1], firstBlock, nil
}

// A lane cursor that found no due row at all records its lane empty.
func (self *TaskWorker) passEmptyClaimFunction(name string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.claimFunctionEmptyUntil[name] = self.claimNow().Add(claimFunctionEmptyLaneTtl)
}
