// A recurring run-once chain that finds nothing to do backs off to a long
// delay and leaves a wake mark in Redis. A producer that adds work clears the
// mark after its work commits and, when this producer was the one to clear it,
// kicks the chain back to now. While the chain is active there is no mark, so
// a producer pays one Redis round trip and writes nothing to the queue; a
// back-off costs at most one kick however many producers arrive, since only
// one of them removes the mark.
//
// A kick, like KickTasks, moves only the run_at of a pending row. A claimed row
// keeps waiting on its release_time, and its completion replaces it with
// whatever successor its post schedules, so a kick that lands on a running
// chain is lost. The mark is therefore set only after the idle run's finishing
// transaction has committed its backed-off successor, and the chain looks for
// work again after setting it. A producer clears the mark only after its own
// commit, so one of the two always sees the other: either the chain's second
// look finds the work and wakes the chain itself, or the producer finds the
// mark and kicks a successor that already exists and is not yet claimed.
//
// Producers run in request processes, so the kick is a pooled transaction that
// takes the chain's queue key only if it is free, never a direct maintenance
// session or a wait. A busy key kicks nothing and needs nothing: its holder is
// claiming the successor, which then reads the producer's committed work, or
// finishing or refreshing a run, which either reads that work or ends idle and
// takes its own second look, or the startup scheduler, which pulls the chain
// to its active cadence.
//
// Losing the mark (a failed Redis write, a crash after the commit, a producer
// from an older build) loses only the kick: the chain still runs at its long
// delay. All marks share one hash tag, so a producer clears several in one
// pipeline.
package task

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Tunables of idle chain wake marks.
type IdleWakeSettings struct {
	// How long a mark lives after the idle run that set it. It must outlast the
	// owner's idle delay plus one run, so that a backed-off chain's mark does
	// not lapse before its next idle run sets it again. A lapsed mark loses
	// only the kick; the chain still runs at its idle delay.
	MarkTtl time.Duration
	// Bounds one mark, clear or wake, including the kick's queue transaction.
	Timeout time.Duration
}

// Four 30-second idle delays per mark. A wake is a Redis round trip and at
// most one short queue transaction per kicked chain.
func DefaultIdleWakeSettings() *IdleWakeSettings {
	return &IdleWakeSettings{
		MarkTtl: 2 * time.Minute,
		Timeout: 5 * time.Second,
	}
}

// One count per run-once key and operation: "marked" and "resumed" for a
// chain's own mark; for a producer's wake "active" (no mark), "kicked", "busy"
// (its queue key held) or "due" (no future run_at to pull in); and "error".
var taskIdleWakeCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "task",
		Name:      "idle_wake_total",
		Help:      "Idle run-once chain marks set and cleared, and producer wakes that kicked a backed-off chain or left it active, busy or already due",
	},
	[]string{"outcome"},
)

func init() {
	prometheus.MustRegister(taskIdleWakeCounter)
}

// Keyed by the raw run-once key, the form KickTasks accepts. The shared hash
// tag keeps every mark in one slot.
func idleWakeMarkKey(runOnceKey string) string {
	return fmt.Sprintf("task_idle_wake:v1:{task_idle_wake}:%s", runOnceKey)
}

// Records that the chain under runOnceKey has backed off. Call it only after
// the idle run's finishing transaction committed the backed-off successor,
// then look for work again and call WakeIdleRunOnce if there is any.
func MarkIdleRunOnce(ctx context.Context, runOnceKey string, settings *IdleWakeSettings) error {
	bounded, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	err := server.RedisWithDeadline(bounded, func(r server.RedisClient) error {
		return r.Set(bounded, idleWakeMarkKey(runOnceKey), "1", settings.MarkTtl).Err()
	}, server.OptRedisNoPreflight())
	observeIdleWake("mark", "marked", err)
	return err
}

// Removes the mark of a chain that is running at its active cadence again, so
// that producers stop kicking it.
func ClearIdleRunOnce(ctx context.Context, runOnceKey string, settings *IdleWakeSettings) error {
	bounded, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	cleared := false
	err := server.RedisWithDeadline(bounded, func(r server.RedisClient) error {
		removed, err := r.Del(bounded, idleWakeMarkKey(runOnceKey)).Result()
		cleared = removed == 1
		return err
	}, server.OptRedisNoPreflight())
	if cleared || err != nil {
		observeIdleWake("clear", "resumed", err)
	}
	return err
}

// Called after the producer's work committed. Clears the marks of the chains
// under runOnceKeys and kicks each chain whose mark this call removed; a chain
// without a mark is active and is not touched, and a held queue key or a row
// already due is left as it is. Returns the keys whose row moved. A Redis
// failure kicks nothing, so a Redis outage cannot turn every producer into a
// queue write; those chains wait out their idle delay instead.
func WakeIdleRunOnce(ctx context.Context, settings *IdleWakeSettings, runOnceKeys ...string) (kickedKeys []string, returnErr error) {
	if len(runOnceKeys) == 0 {
		return nil, nil
	}
	keys := slices.Clone(runOnceKeys)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	bounded, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	clearedKeys := []string{}
	returnErr = server.RedisWithDeadline(bounded, func(r server.RedisClient) error {
		results := make([]*redis.IntCmd, len(keys))
		_, err := r.Pipelined(bounded, func(pipe redis.Pipeliner) error {
			for i, key := range keys {
				results[i] = pipe.Del(bounded, idleWakeMarkKey(key))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for i, result := range results {
			if result.Val() == 1 {
				clearedKeys = append(clearedKeys, keys[i])
			}
		}
		return nil
	}, server.OptRedisNoPreflight())
	if returnErr != nil {
		observeIdleWake("wake", "", returnErr)
		return nil, returnErr
	}
	taskIdleWakeCounter.WithLabelValues("active").Add(float64(len(keys) - len(clearedKeys)))
	for _, key := range clearedKeys {
		// The removed mark belonged to this call alone; a failed kick leaves
		// the chain at its idle delay.
		server.HandleError(func() {
			kickedCount, admitted := tryKickTasks(bounded, key)
			switch {
			case !admitted:
				observeIdleWake("kick", "busy", nil)
			case kickedCount == 0:
				observeIdleWake("kick", "due", nil)
			default:
				kickedKeys = append(kickedKeys, key)
				observeIdleWake("kick", "kicked", nil)
			}
		}, func(err error) {
			returnErr = errors.Join(returnErr, err)
			observeIdleWake("kick", "", err)
		})
	}
	return
}

// Pulls a pending row under the raw run-once key to now, unless its run_at is
// already due, in a pooled read committed transaction that takes the key's
// queue ownership only if no other writer holds it. Returns the rows moved and
// whether the key was free.
func tryKickTasks(ctx context.Context, runOnceKey string) (kickedCount int64, admitted bool) {
	jsonKey, keys := kickTaskOwnershipKeys(runOnceKey)
	server.Tx(ctx, func(tx server.PgTx) {
		kickedCount = 0
		var err error
		admitted, err = server.TryTxOwnership(ctx, tx, keys)
		server.Raise(err)
		if !admitted {
			return
		}
		tag := server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2
			WHERE run_once_key IN ($1,$3) AND $2<run_at`, runOnceKey, server.NowUtc(), jsonKey))
		kickedCount = tag.RowsAffected()
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}

// Counts the outcome of one operation, or an error in its place. These errors
// repeat once per producer during a Redis or database outage, so their detail
// is verbose only.
func observeIdleWake(operation string, outcome string, err error) {
	if err != nil {
		taskIdleWakeCounter.WithLabelValues("error").Inc()
		if glog.V(1) {
			glog.Infof("[task]idle wake %s failed: %v\n", operation, err)
		}
		return
	}
	if outcome != "" {
		taskIdleWakeCounter.WithLabelValues(outcome).Inc()
	}
}
