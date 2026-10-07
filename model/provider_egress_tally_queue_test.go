// Synthetic queue controls cover finite admission, immutable prefixes and
// bootstrap/loss refusal. Native fixtures never contact a production service.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Opens a fixture shard, simulating the committed initialization successor.
func testingOpenTallyShard(t testing.TB, ctx context.Context, shard int, generation string) {
	t.Helper()
	if _, err := ReadProviderEgressTallyPage(ctx, shard, generation, "0-0", true); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProviderEgressTallyPage(ctx, shard, generation, "0-0", false); err != nil {
		t.Fatal(err)
	}
}

// The page planner preserves each counter's meaning while amortizing repeated
// shared place/site updates. Duplicate site names and canaries stay independent.
func TestProviderEgressTallyBatchCoalescesExactCounters(t *testing.T) {
	day := time.Date(2026, 10, 6, 23, 0, 0, 0, time.FixedZone("synthetic", -3600))
	record := ProviderEgressTallyRecord{MeasuredAt: day, Run: ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: " ZZ ", Region: " Synthetic "}, Healthy: true}, Loads: []ProviderEgressSiteLoad{
		{Name: "same.example", Ok: true, Healthy: true}, {Name: " same.example ", Healthy: true},
		{Name: "canary.example", Canary: true, Ok: true}, {Name: "ignored.example", Canary: true},
		{Name: ""}, {Name: strings.Repeat("x", 129)},
	}}
	records := make([]ProviderEgressTallyRecord, 256)
	for i := range records {
		records[i] = record
	}
	batch, err := PrepareProviderEgressTallyBatch(records)
	if err != nil || len(batch.Places) != 1 || len(batch.Sites) != 3 {
		t.Fatalf("coalesced shape=%+v err=%v", batch, err)
	}
	p := batch.Places[0]
	if p.Runs != 256 || p.HealthyRuns != 256 || p.EchoFailures != 0 || p.Place.CountryCode != "zz" || p.Place.Region != "Synthetic" || !p.Day.Equal(time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("place counters=%+v", p)
	}
	s := batch.Sites[2]
	if s.Name != "same.example" || s.Loads != 512 || s.Failures != 256 || s.HealthyLoads != 512 || s.HealthyFailures != 256 || s.CanaryLoads != 0 {
		t.Fatalf("site counters=%+v", s)
	}
	if batch.Sites[0].CanaryLoads != 256 || batch.Sites[0].CanaryPasses != 256 || batch.Sites[0].Loads != 0 || batch.Sites[1].CanaryPasses != 0 {
		t.Fatal("canary evidence changed scoring counters")
	}
	if _, err := PrepareProviderEgressTallyBatch(append(records, record)); err == nil {
		t.Fatal("unbounded record page accepted")
	}
}

// Decimal sequence parsing must not collapse precision or accept ambiguous
// encodings at the durable replay boundary.
func TestProviderEgressTallyCursorValidation(t *testing.T) {
	for _, row := range []struct {
		a, b  string
		order int
	}{
		{a: "9-0", b: "10-0", order: -1}, {a: "10-999", b: "11-0", order: -1}, {a: "18446744073709551615-0", b: "18446744073709551614-9", order: 1}, {a: "1-2", b: "1-2", order: 0},
	} {
		got, err := CompareProviderEgressTallyCursor(row.a, row.b)
		if err != nil || got != row.order {
			t.Fatalf("cursor order=%d err=%v", got, err)
		}
	}
	for _, bad := range []string{"", "1", "01-0", "1-00", "-1-0", "1--1", "18446744073709551616-0"} {
		if _, err := CompareProviderEgressTallyCursor(bad, "0-0"); err == nil {
			t.Fatalf("ambiguous cursor accepted: %q", bad)
		}
	}
}

// Initialization is durable before admission. Retained metadata fences a
// replacement owner, and losing metadata after readiness never resets to zero.
func TestProviderEgressTallyQueueBootstrapFencesGeneration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		run := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic"}}
		shard := ProviderEgressTallyShard(run.Place)
		if _, err := ReadProviderEgressTallyPage(ctx, shard, "synthetic-owner", "0-0", true); err != nil {
			t.Fatal(err)
		}
		if err := NewProviderEgressTallyWriter().Record(ctx, time.Now(), run, nil); err == nil {
			t.Fatal("handoff preceded durable initialization")
		}
		if _, err := ReadProviderEgressTallyPage(ctx, shard, "synthetic-owner", "0-0", false); err != nil {
			t.Fatal(err)
		}
		if err := NewProviderEgressTallyWriter().Record(ctx, time.Now(), run, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadProviderEgressTallyPage(ctx, shard, "synthetic-replacement", "0-0", true); err == nil {
			t.Fatal("replacement owner replayed retained records from zero")
		}
		server.Redis(ctx, func(r server.RedisClient) { server.Raise(r.Del(ctx, providerEgressTallyKeys(shard)...).Err()) })
		if _, err := ReadProviderEgressTallyPage(ctx, shard, "synthetic-owner", "0-0", false); err == nil {
			t.Fatal("lost initialized queue silently restarted at zero")
		}
	})
}

// A reread of an uncommitted prefix is identical. New arrivals survive later
// cleanup, which deletes only the prefix named by the durable cursor.
func TestProviderEgressTallyQueueRetainsUnacknowledgedPrefix(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		run := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic"}}
		shard := ProviderEgressTallyShard(run.Place)
		generation := "synthetic-owner"
		testingOpenTallyShard(t, ctx, shard, generation)
		writer := NewProviderEgressTallyWriter()
		for range 3 {
			if err := writer.Record(ctx, time.Now(), run, nil); err != nil {
				t.Fatal(err)
			}
		}
		first, err := ReadProviderEgressTallyPage(ctx, shard, generation, "0-0", false)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := ReadProviderEgressTallyPage(ctx, shard, generation, "0-0", false)
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Records) != 3 || !reflect.DeepEqual(first, replay) {
			t.Fatal("unacknowledged prefix changed or vanished")
		}
		if err := writer.Record(ctx, time.Now(), run, nil); err != nil {
			t.Fatal(err)
		}
		next, err := ReadProviderEgressTallyPage(ctx, shard, generation, first.NextCursor, false)
		if err != nil || len(next.Records) != 1 {
			t.Fatalf("concurrent append lost: page=%+v err=%v", next, err)
		}
		last, err := ReadProviderEgressTallyPage(ctx, shard, generation, next.NextCursor, false)
		if err != nil || len(last.Records) != 0 {
			t.Fatalf("acknowledged prefix reappeared: %+v %v", last, err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			value, err := r.HGet(ctx, providerEgressTallyKeys(shard)[0], "bytes").Int64()
			server.Raise(err)
			if value != 0 {
				t.Fatalf("cleaned queue bytes=%d", value)
			}
		})
	})
}

// A finite byte cap refuses the complete run and marks evidence loss. There is
// no fall back to the tally table; the next healthy handoff preserves the mark.
func TestProviderEgressTallyQueueRefusalMarksProjectionLoss(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		run := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic"}}
		shard := ProviderEgressTallyShard(run.Place)
		testingOpenTallyShard(t, ctx, shard, "synthetic-owner")
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.HSet(ctx, providerEgressTallyKeys(shard)[0], "bytes", providerEgressTallyQueueBytes).Err())
		})
		writer := NewProviderEgressTallyWriter()
		if err := writer.Record(ctx, time.Now(), run, nil); err == nil {
			t.Fatal("full queue accepted a run")
		}
		server.Redis(ctx, func(r server.RedisClient) {
			count, err := r.XLen(ctx, providerEgressTallyKeys(shard)[1]).Result()
			server.Raise(err)
			losses, err := r.ZCard(ctx, providerEgressTallyKeys(shard)[2]).Result()
			server.Raise(err)
			if count != 0 || losses != 1 {
				t.Fatal("refusal partially queued a run or hid loss")
			}
			server.Raise(r.HSet(ctx, providerEgressTallyKeys(shard)[0], "bytes", 0).Err())
		})
		if err := writer.Record(ctx, time.Now(), run, nil); err != nil {
			t.Fatal(err)
		}
		if rows := GetProviderEgressPlaceTallies(ctx, time.Time{}); len(rows) != 0 {
			t.Fatal("producer silently fell back to PostgreSQL")
		}
		page, err := ReadProviderEgressTallyPage(ctx, shard, "synthetic-owner", "0-0", false)
		if err != nil || len(page.Records) != 1 {
			t.Fatalf("whole accepted run not retained: %+v %v", page, err)
		}
	})
}

// Large but valid records demonstrate that the reply is byte bounded as well
// as count bounded. The first excluded run remains intact for the next page.
func TestProviderEgressTallyQueuePageByteBound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		run := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic"}}
		shard := ProviderEgressTallyShard(run.Place)
		generation := "synthetic-owner"
		testingOpenTallyShard(t, ctx, shard, generation)
		loads := make([]ProviderEgressSiteLoad, 100)
		for i := range loads {
			loads[i] = ProviderEgressSiteLoad{Name: strings.Repeat("x", 120), Ok: true}
		}
		writer := NewProviderEgressTallyWriter()
		for range 20 {
			if err := writer.Record(ctx, time.Now(), run, loads); err != nil {
				t.Fatal(err)
			}
		}
		page, err := ReadProviderEgressTallyPage(ctx, shard, generation, "0-0", false)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Records) == 0 || len(page.Records) >= 20 {
			t.Fatal("page did not stop at its byte budget")
		}
		bytes := 0
		for _, record := range page.Records {
			raw, err := json.Marshal(record)
			server.Raise(err)
			bytes += len(raw)
		}
		if bytes > providerEgressTallyPageBytes {
			t.Fatal("page exceeded its byte budget")
		}
		next, err := ReadProviderEgressTallyPage(ctx, shard, generation, page.NextCursor, false)
		if err != nil || len(page.Records)+len(next.Records) != 20 {
			t.Fatalf("excluded suffix lost: %v", err)
		}
	})
}

// Unavailable shards stay observable without vetoing the unaffected shards.
// Losing accepted records never becomes an empty healthy page.
func TestProviderEgressTallyProjectionScopesUnavailableShards(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Now()
		retention := now.Add(-14 * 24 * time.Hour).UTC().Truncate(24 * time.Hour)
		if p := ReadProviderEgressTallyProjection(ctx, now, retention); len(p.UnknownShards) != ProviderEgressTallyShardCount || len(p.Excluded) != 0 {
			t.Fatalf("missing projections hidden or invented global exclusions: %+v", p)
		}
		for shard := range ProviderEgressTallyShardCount {
			testingOpenTallyShard(t, ctx, shard, "synthetic-owner")
		}
		if p := ReadProviderEgressTallyProjection(ctx, time.Now(), retention); len(p.UnknownShards) != 0 || len(p.Excluded) != 0 {
			t.Fatalf("ready projection=%+v", p)
		}
		if p := ReadProviderEgressTallyProjection(ctx, now.Add(2*time.Minute), retention); len(p.UnknownShards) != ProviderEgressTallyShardCount {
			t.Fatal("stale ownership hidden")
		}
		run := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic"}}
		shard := ProviderEgressTallyShard(run.Place)
		if err := NewProviderEgressTallyWriter().Record(ctx, now, run, nil); err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) { server.Raise(r.Del(ctx, providerEgressTallyKeys(shard)[1]).Err()) })
		if _, err := ReadProviderEgressTallyPage(ctx, shard, "synthetic-owner", "0-0", false); err == nil {
			t.Fatal("lost records became an empty healthy page")
		}
		p := ReadProviderEgressTallyProjection(ctx, time.Now(), retention)
		if len(p.UnknownShards) != 1 || p.UnknownShards[0] != shard || len(p.Excluded) != 0 {
			t.Fatalf("one failed shard tainted unrelated evidence: %+v", p)
		}
	})
}

// A failed handoff remembers its own day/place, even when a later success is
// for a different place on the same shard. No loss is attributed to that place.
func TestProviderEgressTallyCanceledHandoffCarriesExactLoss(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Now()
		lostRun := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic lost"}}
		shard := ProviderEgressTallyShard(lostRun.Place)
		healthyRun := lostRun
		found := false
		for i := range 1024 {
			healthyRun.Place.Region = fmt.Sprintf("Synthetic healthy %d", i)
			if ProviderEgressTallyShard(healthyRun.Place) == shard {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("synthetic second place missing")
		}
		testingOpenTallyShard(t, ctx, shard, "synthetic-owner")
		writer := NewProviderEgressTallyWriter()
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := writer.Record(canceled, now, lostRun, nil); err == nil {
			t.Fatal("canceled handoff accepted")
		}
		if err := writer.Record(ctx, now, healthyRun, nil); err != nil {
			t.Fatal(err)
		}
		p := ReadProviderEgressTallyProjection(ctx, time.Now(), now.Add(-24*time.Hour))
		want := ProviderEgressTallyExclusion{Day: now.UTC().Format(time.DateOnly), CountryCode: lostRun.Place.CountryCode, Region: lostRun.Place.Region}
		if len(p.Excluded) != 1 || p.Excluded[0] != want {
			t.Fatalf("wrong loss scope: %+v", p.Excluded)
		}
		page, err := ReadProviderEgressTallyPage(ctx, shard, "synthetic-owner", "0-0", false)
		if err != nil || len(page.Records) != 1 || page.Records[0].Run.Place != healthyRun.Place {
			t.Fatalf("canceled evidence replayed or healthy handoff lost: %+v %v", page, err)
		}
	})
}

// Both refresh projections remove only the known incomplete day/place before
// summing. A later day at that place and another place's same day still count.
func TestProviderEgressTallyLossExcludesOnlyAffectedDayAndPlace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		day := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
		first := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic first"}, Healthy: true}
		second := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic second"}, Healthy: true}
		loads := []ProviderEgressSiteLoad{{Name: "synthetic.example", Ok: true, Healthy: true}}
		AddProviderEgressRunTally(ctx, day, first, loads)
		AddProviderEgressRunTally(ctx, day, second, loads)
		AddProviderEgressRunTally(ctx, day.Add(24*time.Hour), first, loads)
		excluded := ProviderEgressTallyExclusion{Day: day.Format(time.DateOnly), CountryCode: first.Place.CountryCode, Region: first.Place.Region}
		rows := GetProviderEgressSiteTallies(ctx, day, excluded)
		if len(rows) != 2 || rows[0].LoadCount != 1 || rows[1].LoadCount != 1 {
			t.Fatalf("unaffected place/day removed: %+v", rows)
		}
		totals := GetProviderEgressSiteDayTotals(ctx, day, []string{"synthetic.example"}, excluded)
		if len(totals) != 2 || totals[0].LoadCount != 1 || totals[1].LoadCount != 1 {
			t.Fatalf("probation used incomplete rows or lost unaffected rows: %+v", totals)
		}
	})
}

// Diagnostic loss storage and owner-local carry are both finite. Excess loss
// scopes report unknown coverage without turning into an unrelated veto.
func TestProviderEgressTallyLossStateHasFiniteBounds(t *testing.T) {
	writer := NewProviderEgressTallyWriter()
	day := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	for i := range providerEgressTallyLossCarry + 8 {
		writer.rememberLoss(ProviderEgressTallyExclusion{Day: day.Format(time.DateOnly), CountryCode: "zz", Region: fmt.Sprintf("Synthetic %d", i)})
	}
	if len(writer.lostScopeKVs) != providerEgressTallyLossCarry {
		t.Fatal("unbounded owner carry")
	}
	overflow := false
	for _, present := range writer.overflowShards {
		overflow = overflow || present
	}
	if !overflow {
		t.Fatal("owner loss overflow disappeared")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Now()
		run := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic overflow"}}
		shard := ProviderEgressTallyShard(run.Place)
		testingOpenTallyShard(t, ctx, shard, "synthetic-owner")
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Eval(ctx, `for i=1,tonumber(ARGV[1]) do redis.call('ZADD',KEYS[1],ARGV[2],'synthetic-'..i) end return 1`, providerEgressTallyKeys(shard)[2:], providerEgressTallyLossLength, now.Unix()).Err())
			server.Raise(r.HSet(ctx, providerEgressTallyKeys(shard)[0], "bytes", providerEgressTallyQueueBytes).Err())
		})
		if err := NewProviderEgressTallyWriter().Record(ctx, now, run, nil); err == nil {
			t.Fatal("full queue accepted")
		}
		server.Redis(ctx, func(r server.RedisClient) {
			count, err := r.ZCard(ctx, providerEgressTallyKeys(shard)[2]).Result()
			server.Raise(err)
			observed, err := r.HGet(ctx, providerEgressTallyKeys(shard)[0], "unscoped_loss_at").Int64()
			server.Raise(err)
			if count != providerEgressTallyLossLength || observed < now.Unix() {
				t.Fatal("unbounded loss state or hidden overflow")
			}
		})
	})
}

// Neither an ambiguous transport response nor a different owner generation
// permits repair to erase a retained page.
func TestProviderEgressTallyRepairRequiresExplicitContinuityLoss(t *testing.T) {
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded, errors.New("tally generation mismatch"), errors.New("tally cursor exceeds retained tail"), errors.New("connection reset by peer")} {
		if IsProviderEgressTallyContinuityLoss(err) {
			t.Fatalf("ambiguous error authorizes reset: %v", err)
		}
	}
	for _, err := range []error{errors.New("tally continuity missing"), errors.New("ERR tally records missing"), errors.New("tally record count mismatch"), errors.New("tally stream continuity mismatch")} {
		if !IsProviderEgressTallyContinuityLoss(err) {
			t.Fatalf("explicit continuity loss missed: %v", err)
		}
	}
}

// One pass writer accepts independent concurrent turns without duplicating
// retained records or losing its canceled-turn scope under concurrent carries.
func TestProviderEgressTallyConcurrentHandoffsPreserveOwnership(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Now()
		run := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic concurrency"}}
		shard := ProviderEgressTallyShard(run.Place)
		testingOpenTallyShard(t, ctx, shard, "synthetic-owner")
		writer := NewProviderEgressTallyWriter()
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		start := make(chan struct{})
		results := make(chan error, 32)
		var workers sync.WaitGroup
		for i := range 32 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				callCtx := ctx
				if i%2 == 0 {
					callCtx = canceled
				}
				err := writer.Record(callCtx, now, run, nil)
				if (err == nil) != (i%2 != 0) {
					results <- fmt.Errorf("unexpected concurrent admission %d: %v", i, err)
				} else {
					results <- nil
				}
			}()
		}
		close(start)
		workers.Wait()
		for range 32 {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Record(ctx, now, run, nil); err != nil {
			t.Fatal(err)
		}
		page, err := ReadProviderEgressTallyPage(ctx, shard, "synthetic-owner", "0-0", false)
		if err != nil || len(page.Records) != 17 {
			t.Fatalf("concurrent queue duplicated or lost records: %+v %v", page, err)
		}
		p := ReadProviderEgressTallyProjection(ctx, time.Now(), now.Add(-24*time.Hour))
		if len(p.Excluded) != 1 || p.Excluded[0].Region != run.Place.Region {
			t.Fatalf("concurrent loss carry disappeared: %+v", p)
		}
	})
}
