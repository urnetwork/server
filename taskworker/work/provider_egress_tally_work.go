// A durable task chain owns each tally stream. A finishing transaction commits
// the increments, an applied receipt, and its next cursor together; Redis
// cleanup occurs only when that committed successor starts.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

var egressTallyRollups = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_egress_tally_rollups_total",
	Help: "Finite auxiliary tally rollup page reads by read, failed or repair outcome; SQL commitment is recorded by the durable task receipt.",
}, []string{"outcome"})

func init() { prometheus.MustRegister(egressTallyRollups) }

type RollupProviderEgressTalliesArgs struct {
	Shard      int    `json:"shard"`
	Generation string `json:"generation"`
	Cursor     string `json:"cursor"`
	Initialize bool   `json:"initialize"`
	RepairFrom string `json:"repair_from,omitempty"`
}

type RollupProviderEgressTalliesResult struct {
	RepairGeneration string                          `json:"repair_generation,omitempty"`
	TaskId           server.Id                       `json:"task_id"`
	NextCursor       string                          `json:"next_cursor"`
	Batch            *model.ProviderEgressTallyBatch `json:"batch"`
	Applied          bool                            `json:"applied"`
}

// A retained key always keeps its original generation/cursor. A missing task
// creates a different generation, which retained Redis metadata will refuse;
// boot can never silently replay an old stream from zero.
func ScheduleRollupProviderEgressTallies(clientSession *session.ClientSession, tx server.PgTx) {
	for shard := range model.ProviderEgressTallyShardCount {
		args := &RollupProviderEgressTalliesArgs{Shard: shard, Generation: server.NewId().String(), Cursor: "0-0", Initialize: true}
		scheduleProviderEgressTallyOwner(tx, clientSession, args, server.NowUtc())
	}
}

// The insertion result is part of the checkpoint protocol, not a best-effort
// scheduling hint. A conflicting successor during Post aborts all increments.
func scheduleProviderEgressTallyOwner(tx server.PgTx, clientSession *session.ClientSession, args *RollupProviderEgressTalliesArgs, at time.Time) bool {
	scheduled, _ := task.ScheduleTaskInTxIfAbsent(tx, RollupProviderEgressTallies, args, clientSession,
		task.RunOnce("rollup_provider_egress_tallies_v1", args.Shard), task.RunAt(at), task.MaxTime(3*time.Minute))
	return scheduled
}

// Source reads and coalescing finish before the SQL handback owns tally rows.
// MaxTime isolates the task from generic batches; its actual I/O is bounded.
func RollupProviderEgressTallies(args *RollupProviderEgressTalliesArgs, clientSession *session.ClientSession) (*RollupProviderEgressTalliesResult, error) {
	if args == nil {
		return nil, fmt.Errorf("missing tally rollup owner")
	}
	identity, ok := task.ExecutionIdentityFromContext(clientSession.Ctx)
	if !ok {
		return nil, fmt.Errorf("tally rollup requires a durable task owner")
	}
	page, err := model.ReadProviderEgressTallyPage(clientSession.Ctx, args.Shard, args.Generation, args.Cursor, args.Initialize, args.RepairFrom)
	if err != nil {
		if !args.Initialize && model.IsProviderEgressTallyContinuityLoss(err) {
			// First commit a new owner generation. Only its successor may
			// discard the unusable prefix; an ambiguous read never does.
			egressTallyRollups.WithLabelValues("repair").Inc()
			return &RollupProviderEgressTalliesResult{TaskId: identity.TaskId, NextCursor: args.Cursor, Batch: &model.ProviderEgressTallyBatch{}, RepairGeneration: server.NewId().String()}, nil
		}
		egressTallyRollups.WithLabelValues("failed").Inc()
		return nil, err
	}
	batch, err := model.PrepareProviderEgressTallyBatch(page.Records)
	if err != nil {
		egressTallyRollups.WithLabelValues("failed").Inc()
		return nil, err
	}
	egressTallyRollups.WithLabelValues("read").Inc()
	return &RollupProviderEgressTalliesResult{TaskId: identity.TaskId, NextCursor: page.NextCursor, Batch: batch}, nil
}

// The generic Post retry catches plain panics, unlike initial finalization.
// Keep known refusal boundaries before any successful mutation; a repeated
// callback first reads its committed applied receipt and remains inert.
func RollupProviderEgressTalliesPost(args *RollupProviderEgressTalliesArgs, result *RollupProviderEgressTalliesResult, clientSession *session.ClientSession, tx server.PgTx) error {
	ctx := clientSession.Ctx
	if args == nil || result == nil || result.TaskId == (server.Id{}) || result.Batch == nil {
		panic("invalid tally post")
	}
	order, err := model.CompareProviderEgressTallyCursor(result.NextCursor, args.Cursor)
	server.Raise(err)
	if order < 0 || args.Generation == "" || args.Shard < 0 || args.Shard >= model.ProviderEgressTallyShardCount {
		panic("invalid tally cursor advance")
	}
	var storedArgs, storedResult string
	server.Raise(tx.QueryRow(ctx, `SELECT args_json,result_json FROM finished_task WHERE task_id=$1 FOR UPDATE`, result.TaskId).Scan(&storedArgs, &storedResult))
	var priorArgs RollupProviderEgressTalliesArgs
	var priorResult RollupProviderEgressTalliesResult
	server.Raise(json.Unmarshal([]byte(storedArgs), &priorArgs))
	server.Raise(json.Unmarshal([]byte(storedResult), &priorResult))
	if priorArgs != *args || priorResult.TaskId != result.TaskId || priorResult.NextCursor != result.NextCursor || priorResult.RepairGeneration != result.RepairGeneration {
		panic("tally post receipt mismatch")
	}
	if priorResult.Applied {
		// No successor recreation here: it may already have advanced several
		// pages or been removed, neither of which authorizes reapplication.
		return nil
	}
	// Use the durable result, not potentially changed callback-owned slices.
	if priorResult.Batch == nil {
		panic("missing durable tally batch")
	}
	// A generic RunPost retry catches plain validation panics and can commit
	// prior SQL. Refuse the complete durable batch before reserving its owner.
	model.ValidateProviderEgressTallyBatch(priorResult.Batch)
	if (order == 0 && (len(priorResult.Batch.Places) != 0 || len(priorResult.Batch.Sites) != 0)) || (order > 0 && len(priorResult.Batch.Places) == 0) {
		panic("tally page did not advance with its increments")
	}
	for _, place := range priorResult.Batch.Places {
		if model.ProviderEgressTallyShard(place.Place) != args.Shard {
			panic("tally page crossed its owner shard")
		}
	}
	next := *args
	next.Cursor = result.NextCursor
	next.Initialize = false
	next.RepairFrom = ""
	if priorResult.RepairGeneration != "" {
		if args.Initialize || order != 0 || priorResult.RepairGeneration == args.Generation || len(priorResult.Batch.Places) != 0 || len(priorResult.Batch.Sites) != 0 {
			panic("invalid tally repair receipt")
		}
		next.Generation = priorResult.RepairGeneration
		next.RepairFrom = args.Generation
		next.Cursor = "0-0"
		next.Initialize = true
	}
	delay := 5 * time.Second
	if order > 0 || args.Initialize || next.Initialize {
		// Drain occupied shards promptly, with a finite owner cadence rather
		// than throttling a concentrated place to one page every five seconds.
		delay = 250 * time.Millisecond
	}
	// Serialization is another pure refusal boundary. Finish it alongside
	// batch validation before the first mutation of the durable checkpoint.
	priorResult.Applied = true
	raw, err := json.Marshal(&priorResult)
	server.Raise(err)
	if !scheduleProviderEgressTallyOwner(tx, clientSession, &next, server.NowUtc().Add(delay)) {
		panic("tally successor already exists without applied receipt")
	}
	model.AddProviderEgressTallyBatchInTx(ctx, tx, priorResult.Batch)
	server.RaisePgResult(tx.Exec(ctx, `UPDATE finished_task SET result_json=$2 WHERE task_id=$1`, result.TaskId, string(raw)))
	return nil
}

// The normal probe pass owns the writer's error carry across concurrent turns.
func newProviderEgressTallyRecorder() func(context.Context, time.Time, model.ProviderEgressRunTally, []model.ProviderEgressSiteLoad) {
	writer := model.NewProviderEgressTallyWriter()
	return func(ctx context.Context, at time.Time, run model.ProviderEgressRunTally, loads []model.ProviderEgressSiteLoad) {
		recordProviderEgressTallyWithWriter(ctx, writer, at, run, loads)
	}
}
