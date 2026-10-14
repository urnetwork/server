// Native capacity coverage of the durable owner: real Redis pages, the
// production target and Post, and fixture SQL counters. Each eval is one claim
// of the owner; producer rates are counted per claim, never timed.
package work

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
)

// The shard's last accepted record id, straight from its metadata.
func (self *testingTallyOwner) tail() string {
	self.t.Helper()
	var tail string
	server.Redis(self.ctx, func(r server.RedisClient) {
		var err error
		tail, err = r.HGet(self.ctx, fmt.Sprintf("provider_egress_tally:{v1:%d}:meta", self.shard), "tail").Result()
		server.Raise(err)
	})
	return tail
}

// Admission occupancy straight from the shard's metadata and stream.
func (self *testingTallyOwner) retained() (count int64, length int64) {
	self.t.Helper()
	server.Redis(self.ctx, func(r server.RedisClient) {
		var err error
		count, err = r.HGet(self.ctx, fmt.Sprintf("provider_egress_tally:{v1:%d}:meta", self.shard), "count").Int64()
		server.Raise(err)
		length, err = r.XLen(self.ctx, fmt.Sprintf("provider_egress_tally:{v1:%d}:records", self.shard)).Result()
		server.Raise(err)
	})
	return count, length
}

// One claim commits every retained page, not one 256-record page. Before the
// fix the claim committed 256 of 1000 runs and left the rest for later claims.
func TestProviderEgressTallyRollupDrainsBacklogBeyondOnePagePerClaim(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		h := newTestingTallyOwner(t)
		defer h.close()
		h.record(1000)
		h.eval()
		h.counts(1000)
		if after, tail := h.pending(), h.tail(); after.Cursor != tail {
			t.Fatalf("committed cursor %s did not reach the end of the backlog %s", after.Cursor, tail)
		}
		// The committed successor deletes the whole drained prefix.
		h.eval()
		h.counts(1000)
		if count, length := h.retained(); count != 0 || length != 0 {
			t.Fatalf("drained prefix still occupies admission: count=%d length=%d", count, length)
		}
	})
}

// Main on 2026-10-14: about 35% of runs land on the hottest place shard and its
// owner is claimed about every 50s. Twice the 90 runs/s wave peak is then about
// 3200 runs per claim on that shard. Before the fix each claim drained 256, so
// the shard filled its 16384-record bound and refused the sixth claim's runs
// as full, as Main's hot shards did. Overflowing the old owner takes more real
// handoffs than one shard retains, so the fixture's hang guard is longer.
func TestProviderEgressTallyRollupKeepsUpWithTwiceTheWavePeak(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		h := newTestingTallyOwnerWithin(t, 10*time.Minute)
		defer h.close()
		const runsPerClaim = 3200
		const claims = 6
		writer := model.NewProviderEgressTallyWriter()
		refused := 0
		var firstRefusal error
		retainedCounts := []int64{}
		for claim := range claims {
			for range runsPerClaim {
				if err := writer.Record(h.ctx, h.day, h.run, []model.ProviderEgressSiteLoad{{Name: "synthetic.example", Ok: true, Healthy: true}}); err != nil {
					refused += 1
					if firstRefusal == nil {
						firstRefusal = fmt.Errorf("claim %d: %w", claim+1, err)
					}
				}
			}
			h.eval()
			count, length := h.retained()
			if count != length {
				t.Fatalf("claim %d admission count=%d differs from stream length=%d", claim+1, count, length)
			}
			retainedCounts = append(retainedCounts, count)
		}
		if refused != 0 {
			t.Fatalf("refused %d of %d runs at %d runs per claim; first %v", refused, claims*runsPerClaim, runsPerClaim, firstRefusal)
		}
		h.counts(claims * runsPerClaim)
		// Each claim leaves only its own committed runs, awaiting the successor.
		for claim, count := range retainedCounts {
			if count != runsPerClaim {
				t.Fatalf("claim %d left %d retained records; want %d", claim+1, count, runsPerClaim)
			}
		}
	})
}

// A multi-page drain commits once. Until the committed successor runs, Redis
// still retains the whole drained prefix, and a repeated Post stays inert.
func TestProviderEgressTallyRollupMultiPageReplayDoesNotAddTwice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		h := newTestingTallyOwner(t)
		defer h.close()
		h.record(600)
		before := h.pending()
		id := h.eval()
		h.counts(600)
		finished := task.GetFinishedTasks(h.ctx, id)[id]
		var result RollupProviderEgressTalliesResult
		server.Raise(json.Unmarshal([]byte(finished.ResultJson), &result))
		if !result.Applied || result.Batch == nil || len(result.Batch.Places) != 1 || result.Batch.Places[0].Runs != 600 {
			t.Fatalf("multi-page receipt=%+v", result)
		}
		if count, length := h.retained(); count != 600 || length != 600 {
			t.Fatalf("cleanup preceded the committed successor: count=%d length=%d", count, length)
		}
		retained, err := model.DrainProviderEgressTallies(h.ctx, h.shard, before.Generation, before.Cursor, false)
		if err != nil || retained.Records != 600 {
			t.Fatalf("lost-ACK fixture did not retain its drained prefix: %v", err)
		}
		for range 2 {
			server.Tx(h.ctx, func(tx server.PgTx) { server.Raise(RollupProviderEgressTalliesPost(&before, &result, h.session, tx)) })
		}
		h.counts(600)
		h.record(3)
		h.eval()
		h.counts(603)
		if count, length := h.retained(); count != 3 || length != 3 {
			t.Fatalf("successor did not clean the whole committed prefix: count=%d length=%d", count, length)
		}
	})
}

// A late SQL failure rolls back every page of the drain with its receipt and
// successor. Nothing is deleted, and the retry applies all pages once.
func TestProviderEgressTallyRollupMultiPageFailureRetainsEveryPage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		h := newTestingTallyOwner(t)
		defer h.close()
		h.record(600)
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
			t.Fatal("failed drain advanced the durable cursor")
		}
		if count, length := h.retained(); count != 600 || length != 600 {
			t.Fatalf("failed drain lost retained records: count=%d length=%d", count, length)
		}
		server.Tx(h.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(h.ctx, `DROP TRIGGER synthetic_rollup_failure ON provider_egress_site_tally`))
		})
		h.eval()
		h.counts(600)
	})
}
