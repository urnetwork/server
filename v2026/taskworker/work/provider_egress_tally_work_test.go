// Native task finalization, real Redis pages and fixture SQL counters exercise
// the additive commit boundary. No elapsed throughput threshold is a verdict.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// One isolated fixture owner uses the production target, Post and task worker.
type testingTallyOwner struct {
	t       testing.TB
	ctx     context.Context
	cancel  context.CancelFunc
	session *session.ClientSession
	worker  *task.TaskWorker
	shard   int
	run     model.ProviderEgressRunTally
	day     time.Time
}

func newTestingTallyOwner(t testing.TB) *testingTallyOwner {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	h := &testingTallyOwner{t: t, ctx: ctx, cancel: cancel, day: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), run: model.ProviderEgressRunTally{Place: model.ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic rollup"}, Healthy: true}}
	h.shard = model.ProviderEgressTallyShard(h.run.Place)
	h.session = session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	h.worker = task.NewTaskWorkerWithDefaults(ctx)
	h.worker.AddTargets(task.NewTaskTargetWithPost(RollupProviderEgressTallies, RollupProviderEgressTalliesPost))

	server.Tx(ctx, func(tx server.PgTx) {
		if !scheduleProviderEgressTallyOwner(tx, h.session, &RollupProviderEgressTalliesArgs{Shard: h.shard, Generation: server.NewId().String(), Cursor: "0-0", Initialize: true}, server.NowUtc()) {
			t.Fatal("fixture owner already existed")
		}
	})
	h.eval()
	h.eval()
	return h
}

// Ends only this fixture owner's lifetime before its database is torn down.
func (self *testingTallyOwner) close() {
	self.worker.Close()
	self.session.Cancel()
	self.cancel()
}

// Making the scheduled successor due advances fixture time without sleeps.
func (self *testingTallyOwner) wake() {
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE run_once_key=$1`, task.RunOnce("rollup_provider_egress_tallies_v1", self.shard).String(), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)))
	})
}

func (self *testingTallyOwner) eval() server.Id {
	self.t.Helper()
	self.wake()
	finished, retried, posts, err := self.worker.EvalTasks(1)
	if err != nil || len(finished) != 1 || len(retried) != 0 || len(posts) != 0 {
		self.t.Fatalf("rollup handback finished=%d retry=%d post=%d err=%v", len(finished), len(retried), len(posts), err)
	}
	return finished[0]
}

func (self *testingTallyOwner) record(n int) {
	self.t.Helper()
	writer := model.NewProviderEgressTallyWriter()
	for range n {
		if err := writer.Record(self.ctx, self.day, self.run, []model.ProviderEgressSiteLoad{{Name: "synthetic.example", Ok: true, Healthy: true}}); err != nil {
			self.t.Fatal(err)
		}
	}
}

func (self *testingTallyOwner) pending() RollupProviderEgressTalliesArgs {
	var raw string
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT args_json FROM pending_task WHERE run_once_key=$1`, task.RunOnce("rollup_provider_egress_tallies_v1", self.shard).String()).Scan(&raw))
	})
	var args RollupProviderEgressTalliesArgs
	server.Raise(json.Unmarshal([]byte(raw), &args))
	return args
}

func (self *testingTallyOwner) counts(want int) {
	self.t.Helper()
	places := model.GetProviderEgressPlaceTallies(self.ctx, self.day)
	sites := model.GetProviderEgressSiteTallies(self.ctx, self.day)
	if want == 0 {
		if len(places) != 0 || len(sites) != 0 {
			self.t.Fatal("uncommitted page changed SQL counters")
		}
		return
	}
	if len(places) != 1 || places[0].RunCount != want || places[0].HealthyRunCount != want || len(sites) != 1 || sites[0].LoadCount != want || sites[0].HealthyLoadCount != want {
		self.t.Fatalf("whole page counters places=%+v sites=%+v want=%d", places, sites, want)
	}
}

// One hundred accepted records produce one actual place upsert. A committed
// but Redis-unacknowledged page and repeated Post cannot apply them twice.
func TestProviderEgressTallyRollupCommittedReplayDoesNotAddTwice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		h := newTestingTallyOwner(t)
		defer h.close()
		server.Tx(h.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(h.ctx, `CREATE SEQUENCE synthetic_rollup_place_attempts;
		CREATE FUNCTION synthetic_rollup_place_attempt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM nextval('synthetic_rollup_place_attempts');RETURN NEW;END $$;
		CREATE TRIGGER synthetic_rollup_place_attempt BEFORE INSERT ON provider_egress_place_tally FOR EACH ROW EXECUTE FUNCTION synthetic_rollup_place_attempt()`))
		})
		h.record(100)
		before := h.pending()
		id := h.eval()
		h.counts(100)
		finished := task.GetFinishedTasks(h.ctx, id)[id]
		var result RollupProviderEgressTalliesResult
		server.Raise(json.Unmarshal([]byte(finished.ResultJson), &result))
		if !result.Applied {
			t.Fatal("SQL commit lacks its applied receipt")
		}
		// Cleanup has not run: the original prefix still exists at the old
		// cursor, reproducing a lost post-commit Redis acknowledgment.
		retained, err := model.ReadProviderEgressTallyPage(h.ctx, h.shard, before.Generation, before.Cursor, false)
		if err != nil || len(retained.Records) != 100 {
			t.Fatalf("lost-ACK fixture did not retain its page: %v", err)
		}
		for range 2 {
			server.Tx(h.ctx, func(tx server.PgTx) { server.Raise(RollupProviderEgressTalliesPost(&before, &result, h.session, tx)) })
		}
		h.counts(100)
		var attempts int64
		server.Db(h.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(h.ctx, `SELECT last_value FROM synthetic_rollup_place_attempts`).Scan(&attempts))
		})
		if attempts != 1 {
			t.Fatalf("100 records/repeated Post acquired place row %d times; want1", attempts)
		}
		h.record(3)
		h.eval()
		h.counts(103)
		// Even after a newer cursor committed, an older receipt is inert.
		server.Tx(h.ctx, func(tx server.PgTx) { server.Raise(RollupProviderEgressTalliesPost(&before, &result, h.session, tx)) })
		h.counts(103)
	})
}

// A late site failure rolls back increments, receipt and successor together.
// The original task then retries the unchanged Redis page and applies it once.
func TestProviderEgressTallyRollupFailureRetainsCursorAndPage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		h := newTestingTallyOwner(t)
		defer h.close()
		h.record(7)
		before := h.pending()
		server.Tx(h.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(h.ctx, `CREATE FUNCTION synthetic_rollup_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic tally site failure' USING ERRCODE='P0001';END $$;
		CREATE TRIGGER synthetic_rollup_failure BEFORE INSERT ON provider_egress_site_tally FOR EACH ROW EXECUTE FUNCTION synthetic_rollup_failure()`))
		})
		h.wake()
		failure := server.HandleError(func() { _, _, _, err := h.worker.EvalTasks(1); server.Raise(err) })
		if failure == nil {
			t.Fatal("late site failure did not abort handback")
		}
		h.counts(0)
		if after := h.pending(); after != before {
			t.Fatal("failed page advanced the durable cursor")
		}
		page, err := model.ReadProviderEgressTallyPage(h.ctx, h.shard, before.Generation, before.Cursor, false)
		if err != nil || len(page.Records) != 7 {
			t.Fatalf("failed page lost its retained records: %v", err)
		}
		server.Tx(h.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(h.ctx, `DROP TRIGGER synthetic_rollup_failure ON provider_egress_site_tally`))
		})
		h.eval()
		h.counts(7)
	})
}

// Startup is idempotent for a retained owner. Removing its durable cursor does
// not grant a fresh task permission to replay still-retained Redis records.
func TestProviderEgressTallyRollupBootstrapCannotResetRetainedCursor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		h := newTestingTallyOwner(t)
		defer h.close()
		h.record(2)
		h.eval()
		before := h.pending()
		server.Tx(h.ctx, func(tx server.PgTx) { ScheduleRollupProviderEgressTallies(h.session, tx) })
		if after := h.pending(); after != before {
			t.Fatal("startup changed retained generation/cursor")
		}
		server.Tx(h.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(h.ctx, `DELETE FROM pending_task WHERE run_once_key=$1`, task.RunOnce("rollup_provider_egress_tallies_v1", h.shard).String()))
			ScheduleRollupProviderEgressTallies(h.session, tx)
		})
		replacement := h.pending()
		if replacement.Generation == before.Generation {
			t.Fatal("lost durable cursor reused its generation")
		}
		if _, err := model.ReadProviderEgressTallyPage(h.ctx, h.shard, replacement.Generation, replacement.Cursor, replacement.Initialize); err == nil {
			t.Fatal("fresh task replayed retained records")
		}
		h.counts(2)
	})
}

// The production handoff succeeds while SQL explicitly refuses any direct
// tally write. This excludes a false GREEN obtained by merely losing a tally
// because the Redis queue was uninitialized in the original publication RED.
func TestProviderEgressTallyProductionHandoffRetainsEvidenceWithoutSql(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		h := newTestingTallyOwner(t)
		defer h.close()
		args := h.pending()
		server.Tx(h.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(h.ctx, `CREATE FUNCTION synthetic_tally_tripwire() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'unexpected synchronous tally SQL' USING ERRCODE='P0001';END $$;
		CREATE TRIGGER synthetic_tally_tripwire BEFORE INSERT ON provider_egress_place_tally FOR EACH ROW EXECUTE FUNCTION synthetic_tally_tripwire()`))
		})
		recordProviderEgressRunTally(h.ctx, h.day, h.run, []model.ProviderEgressSiteLoad{{Name: "synthetic.example", Ok: true, Healthy: true}})
		page, err := model.ReadProviderEgressTallyPage(h.ctx, h.shard, args.Generation, args.Cursor, false)
		if err != nil || len(page.Records) != 1 {
			t.Fatalf("production handoff lost the accepted evidence: %v", err)
		}
		h.counts(0)
		server.Tx(h.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(h.ctx, `DROP TRIGGER synthetic_tally_tripwire ON provider_egress_place_tally`))
		})
		h.eval()
		h.counts(1)
	})
}

// Explicit continuity loss is repaired only after a new generation is durable.
// The lost uncommitted suffix is observed, never reconstructed or double-added;
// a later healthy handoff resumes without waiting for an operator or refresh.
func TestProviderEgressTallyRollupRepairsLostContinuityAfterCheckpoint(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		h := newTestingTallyOwner(t)
		defer h.close()
		h.record(2)
		h.eval()
		h.counts(2)
		before := h.pending()
		h.record(3)
		server.Redis(h.ctx, func(r server.RedisClient) {
			server.Raise(r.Del(h.ctx, fmt.Sprintf("provider_egress_tally:{v1:%d}:records", h.shard)).Err())
		})
		repairId := h.eval()
		repair := h.pending()
		if repair.Generation == before.Generation || repair.RepairFrom != before.Generation || repair.Cursor != "0-0" || !repair.Initialize {
			t.Fatalf("missing durable repair intent: %+v", repair)
		}
		server.Redis(h.ctx, func(r server.RedisClient) {
			generation, err := r.HGet(h.ctx, fmt.Sprintf("provider_egress_tally:{v1:%d}:meta", h.shard), "generation").Result()
			server.Raise(err)
			if generation != before.Generation {
				t.Fatal("queue reset preceded its durable repair intent")
			}
		})
		h.counts(2)
		h.eval() // installs the new empty generation; admission still closed
		if err := model.NewProviderEgressTallyWriter().Record(h.ctx, h.day, h.run, nil); err == nil {
			t.Fatal("repair admitted before durable initialization completed")
		}
		h.eval() // committed non-initializing successor opens admission
		h.record(1)
		h.eval()
		h.counts(3)
		if _, err := model.ReadProviderEgressTallyPage(h.ctx, h.shard, before.Generation, before.Cursor, false); err == nil {
			t.Fatal("old generation survived repair")
		}
		finished := task.GetFinishedTasks(h.ctx, repairId)[repairId]
		var result RollupProviderEgressTalliesResult
		server.Raise(json.Unmarshal([]byte(finished.ResultJson), &result))
		server.Tx(h.ctx, func(tx server.PgTx) { server.Raise(RollupProviderEgressTalliesPost(&before, &result, h.session, tx)) })
		h.counts(3)
		if next := h.pending(); next.Generation != repair.Generation || next.Cursor == "0-0" {
			t.Fatal("repeated repair Post reset the live successor")
		}
	})
}

// A successor conflict cannot be accepted after an additive page. The exact
// finished receipt is seeded from a real owner, then made unapplied solely to
// drive this defensive branch; the already-existing successor must stay intact.
func TestProviderEgressTallyRollupConflictingSuccessorCannotAdd(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		h := newTestingTallyOwner(t)
		defer h.close()
		h.record(4)
		before := h.pending()
		id := h.eval()
		finished := task.GetFinishedTasks(h.ctx, id)[id]
		var result RollupProviderEgressTalliesResult
		server.Raise(json.Unmarshal([]byte(finished.ResultJson), &result))
		result.Applied = false
		raw, err := json.Marshal(&result)
		server.Raise(err)
		server.Tx(h.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(h.ctx, `UPDATE finished_task SET result_json=$2 WHERE task_id=$1`, id, string(raw)))
		})
		pending := h.pending()
		failure := server.HandleError(func() {
			server.Tx(h.ctx, func(tx server.PgTx) { server.Raise(RollupProviderEgressTalliesPost(&before, &result, h.session, tx)) }, server.OptNoRetry())
		})
		if failure == nil {
			t.Fatal("conflicting successor accepted as a new checkpoint")
		}
		h.counts(4)
		if after := h.pending(); after != pending {
			t.Fatal("conflicting Post changed the existing successor")
		}
	})
}

// The production refresh excludes a known incomplete place/day while judging
// unaffected evidence. Unknown peer shards never impose a fleet-wide veto.
func TestProviderEgressTallyLossAllowsUnaffectedRefresh(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		h := newTestingTallyOwner(t)
		defer h.close()
		pop := server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(testRefreshCatalog+testRefreshCatalogSize))
		defer pop()
		now := server.NowUtc()
		day := now.UTC().Truncate(24 * time.Hour)
		writer := model.NewProviderEgressTallyWriter()
		canceled, cancel := context.WithCancel(h.ctx)
		cancel()
		if err := writer.Record(canceled, now, h.run, nil); err == nil {
			t.Fatal("canceled loss fixture admitted")
		}
		if err := writer.Record(h.ctx, now, h.run, nil); err != nil {
			t.Fatal(err)
		}
		server.Tx(h.ctx, func(tx server.PgTx) {
			for _, row := range []struct {
				name, country, region string
				loads, failures       int
			}{
				{name: "synthetic-retiring-news", country: "zz", region: "Synthetic unaffected", loads: 250, failures: 150},
				{name: "synthetic-steady-news", country: "zz", region: "Synthetic unaffected", loads: 5000, failures: 0},
				{name: "synthetic-steady-news", country: h.run.Place.CountryCode, region: h.run.Place.Region, loads: 10000, failures: 10000},
			} {
				server.RaisePgResult(tx.Exec(h.ctx, `INSERT INTO provider_egress_site_tally(tally_day,name,country_code,region,load_count,failure_count,healthy_load_count,healthy_failure_count,update_time) VALUES($1,$2,$3,$4,$5,$6,$5,$6,$7)`, day, row.name, row.country, row.region, row.loads, row.failures, now))
			}
		})
		result, err := refreshEgressDestinations(h.ctx, now, func(context.Context, egresshealth.Destination, egresshealth.RequestProfile, int, time.Duration) error {
			return nil
		})
		if err != nil || result.Skipped != "" || result.TallyExcludedPlaceDays != 1 || result.TallyUnknownShards != model.ProviderEgressTallyShardCount-1 || result.Retired != 1 {
			t.Fatalf("unaffected refresh vetoed or tainted: result=%+v err=%v", result, err)
		}
		for _, row := range model.GetProviderEgressDestinations(h.ctx) {
			if row.Name == "synthetic-retiring-news" && row.Active {
				t.Fatal("unaffected retirement did not progress")
			}
			if row.Name == "synthetic-steady-news" && !row.Active {
				t.Fatal("known incomplete place retired its site")
			}
		}
	})
}

// A nonempty additive result cannot checkpoint the unchanged cursor, even if
// its receipt is otherwise authentic. That would replay the same page forever.
func TestProviderEgressTallyRollupRefusesUnadvancedAdditivePage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		h := newTestingTallyOwner(t)
		defer h.close()
		h.record(2)
		before := h.pending()
		id := h.eval()
		finished := task.GetFinishedTasks(h.ctx, id)[id]
		var result RollupProviderEgressTalliesResult
		server.Raise(json.Unmarshal([]byte(finished.ResultJson), &result))
		result.Applied = false
		result.NextCursor = before.Cursor
		raw, err := json.Marshal(&result)
		server.Raise(err)
		server.Tx(h.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(h.ctx, `UPDATE finished_task SET result_json=$2 WHERE task_id=$1`, id, string(raw)))
		})
		failure := server.HandleError(func() {
			server.Tx(h.ctx, func(tx server.PgTx) { server.Raise(RollupProviderEgressTalliesPost(&before, &result, h.session, tx)) }, server.OptNoRetry())
		})
		if failure == nil || !strings.Contains(fmt.Sprint(failure), "tally page did not advance") {
			t.Fatalf("wrong unchanged-cursor refusal: %v", failure)
		}
		h.counts(2)
	})
}
