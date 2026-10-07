// The generic durable Post retry owns a different transaction wrapper from
// initial finalization. Exercise that entrypoint, including a refused receipt.
package work

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Return an authentic finished receipt whose first Post was deferred before
// making any changes. The actual retry then uses the production callback.
func deferTestingTallyPost(h *testingTallyOwner) (RollupProviderEgressTalliesArgs, server.Id) {
	h.t.Helper()
	before := h.pending()
	h.worker.AddTargets(task.NewTaskTargetWithPost(RollupProviderEgressTallies,
		func(*RollupProviderEgressTalliesArgs, *RollupProviderEgressTalliesResult, *session.ClientSession, server.PgTx) error {
			return fmt.Errorf("synthetic defer before tally mutation")
		}))
	defer h.worker.AddTargets(task.NewTaskTargetWithPost(RollupProviderEgressTallies, RollupProviderEgressTalliesPost))
	h.wake()
	finished, retried, posts, err := h.worker.EvalTasks(1)
	if err != nil || len(finished) != 0 || len(retried) != 0 || len(posts) != 1 {
		h.t.Fatalf("deferred Post fixture finished=%d retried=%d posts=%d err=%v", len(finished), len(retried), len(posts), err)
	}
	h.counts(0)
	return before, posts[0]
}

// A plain validation panic is caught by the generic RunPost retry wrapper.
// Refusal must occur before reserving a successor that could delete the page.
func TestProviderEgressTallyRunPostRefusalRetainsCursorAndPage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		h := newTestingTallyOwner(t)
		defer h.close()
		h.record(2)
		before, id := deferTestingTallyPost(h)
		finished := task.GetFinishedTasks(h.ctx, id)[id]
		var result RollupProviderEgressTalliesResult
		server.Raise(json.Unmarshal([]byte(finished.ResultJson), &result))
		if result.Applied || result.Batch == nil || len(result.Batch.Sites) != 1 {
			t.Fatal("deferred fixture lacks its exact unapplied page")
		}
		result.Batch.Sites[0].Name = ""
		raw, err := json.Marshal(&result)
		server.Raise(err)
		server.Tx(h.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(h.ctx, `UPDATE finished_task SET result_json=$2 WHERE task_id=$1`, id, string(raw)))
		})
		got, err := h.worker.RunPost(&task.RunPostArgs{TaskId: id}, h.session)
		if got != nil || err == nil || !strings.Contains(err.Error(), "invalid tally site delta") {
			t.Fatalf("unexpected durable Post refusal: result=%+v err=%v", got, err)
		}
		h.counts(0)
		var successors int
		server.Db(h.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(h.ctx, `SELECT count(*) FROM pending_task WHERE run_once_key=$1`, task.RunOnce("rollup_provider_egress_tallies_v1", h.shard).String()).Scan(&successors))
		})
		if successors != 0 {
			t.Error("refused durable Post committed a successor that advances the unapplied cursor")
		}
		stored := task.GetFinishedTasks(h.ctx, id)[id]
		var after RollupProviderEgressTalliesResult
		server.Raise(json.Unmarshal([]byte(stored.ResultJson), &after))
		if after.Applied || stored.ResultJson != string(raw) {
			t.Fatal("refused durable Post changed its unapplied receipt")
		}
		page, err := model.ReadProviderEgressTallyPage(h.ctx, h.shard, before.Generation, before.Cursor, false)
		if err != nil || len(page.Records) != 2 {
			t.Fatalf("refused durable Post lost its retained page: %v", err)
		}
	})
}

// A valid deferred callback uses the same native RunPost entrypoint, commits
// once, and stays inert while Redis still retains the unacknowledged prefix.
func TestProviderEgressTallyRunPostCommittedReplayDoesNotAddTwice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		h := newTestingTallyOwner(t)
		defer h.close()
		h.record(2)
		before, id := deferTestingTallyPost(h)
		for range 2 {
			got, err := h.worker.RunPost(&task.RunPostArgs{TaskId: id}, h.session)
			if err != nil || got == nil {
				t.Fatalf("valid durable Post did not complete: result=%+v err=%v", got, err)
			}
			h.counts(2)
		}
		after := h.pending()
		if after.Generation != before.Generation || after.Cursor == before.Cursor || after.Initialize {
			t.Fatalf("durable Post did not preserve its advancing owner: %+v", after)
		}
		finished := task.GetFinishedTasks(h.ctx, id)[id]
		var result RollupProviderEgressTalliesResult
		server.Raise(json.Unmarshal([]byte(finished.ResultJson), &result))
		if !result.Applied || result.NextCursor != after.Cursor {
			t.Fatal("durable Post lacks its committed applied receipt")
		}
		page, err := model.ReadProviderEgressTallyPage(h.ctx, h.shard, before.Generation, before.Cursor, false)
		if err != nil || len(page.Records) != 2 {
			t.Fatalf("lost-cleanup-ACK fixture did not retain its page: %v", err)
		}
	})
}
