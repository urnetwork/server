//go:build sidecar_pending

package model

// taskworker's pass over the token projection (sidecar_push_token_slots.go; §18.7's steps 5 and 6,
// SDK-M8-11), on Postgres with MG8 and T2's FakeRedis with the push twins, under a cluster of masters
// that the test restarts and moves slots between, on a fake clock. FS-36's and FS-120's full races on
// T3's 3-master cluster are SDK-M8-12's.

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/pushservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// Every slot has a sentinel, the least n whose tag hashes to it, and no two slots share one.
func TestSentinelForEverySlot(t *testing.T) {
	maxN := 0
	seen := map[string]bool{}
	for slot := range server.RedisSlotCount {
		key := PushTokenSentinelKey(slot)
		if server.RedisHashSlot(key) != slot || sidecartest.RedisClusterSlot(key) != slot {
			t.Fatalf("slot %d: %s hashes to %d", slot, key, server.RedisHashSlot(key))
		}
		if seen[key] {
			t.Fatalf("%s twice", key)
		}
		seen[key] = true
		maxN = max(maxN, PushTokenSentinelNumber(slot))
	}
	// least: no smaller n reaches the slot
	firstNs := map[int]int{}
	for n := 0; n <= maxN; n += 1 {
		slot := server.RedisHashSlot("{tokgen_" + strconv.Itoa(n) + "}")
		if _, ok := firstNs[slot]; !ok {
			firstNs[slot] = n
		}
	}
	for slot := range server.RedisSlotCount {
		if firstNs[slot] != PushTokenSentinelNumber(slot) {
			t.Fatalf("slot %d: sentinel %d, least %d", slot, PushTokenSentinelNumber(slot), firstNs[slot])
		}
	}
	t.Logf("the largest sentinel number is %d", maxN)
}

// T2's cluster for the pass: masters with run_ids and the slots each serves, over one FakeRedis whose
// clock is every master's TIME.
type testPushTokenCluster struct {
	redis   *sidecartest.FakeRedis
	clock   *sidecartest.FakeClock
	masters []*PushTokenMaster
	// slots listed by SlotTimeAndKeys, in order
	listed []int
	// slots whose listing fails, with the error
	failSlots map[int]error
}

func newTestPushTokenCluster(t testing.TB, start time.Time) *testPushTokenCluster {
	t.Helper()
	clock := sidecartest.NewFakeClock(start)
	redis := sidecartest.NewFakeRedis(context.Background(), clock, nil, nil, nil)
	t.Cleanup(redis.Close)
	sidecartest.RegisterPushScripts(redis)
	return &testPushTokenCluster{redis: redis, clock: clock}
}

func (self *testPushTokenCluster) Masters(ctx context.Context) ([]*PushTokenMaster, error) {
	masters := []*PushTokenMaster{}
	for _, master := range self.masters {
		copied := *master
		copied.Slots = slices.Clone(master.Slots)
		masters = append(masters, &copied)
	}
	return masters, nil
}

func (self *testPushTokenCluster) master(address string) *PushTokenMaster {
	for _, master := range self.masters {
		if master.Address == address {
			return master
		}
	}
	return nil
}

func (self *testPushTokenCluster) SlotTimeAndKeys(ctx context.Context, master *PushTokenMaster, slot int, limit int) (int64, []string, bool, error) {
	if err, failing := self.failSlots[slot]; failing {
		return 0, nil, false, err
	}
	serving := self.master(master.Address)
	if serving == nil || serving.RunId != master.RunId || !slices.Contains(serving.Slots, slot) {
		return 0, nil, false, errPushTokenSlotMoved
	}
	self.listed = append(self.listed, slot)
	keys := []string{}
	for key := range self.redis.Dump() {
		if sidecartest.RedisClusterSlot(key) == slot {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	more := limit < len(keys)
	if more {
		keys = keys[:limit]
	}
	return self.clock.Now().UnixMilli(), keys, more, nil
}

// The store side, through the fake's router.
type testPushTokenStore struct {
	*sidecartest.FakeRedis
}

func (self *testPushTokenStore) GetEach(ctx context.Context, keys []string) ([][]byte, error) {
	values := make([][]byte, len(keys))
	for index, key := range keys {
		value, found, err := self.FakeRedis.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if found {
			values[index] = value
		}
	}
	return values, nil
}

func (self *testPushTokenStore) SetPersistent(ctx context.Context, key string, value []byte) error {
	return self.FakeRedis.Set(ctx, key, value, 0)
}

func (self *testPushTokenCluster) store() pushTokenStore {
	return &testPushTokenStore{FakeRedis: self.redis}
}

// Two masters: m0 serving a few other slots, its sentinels in place, and m1 serving small, which the tests
// restart. The slots neither serves stand for the rest of a cluster, which a pass reads no sentinel of.
func (self *testPushTokenCluster) twoMasters(t testing.TB, small []int) {
	t.Helper()
	slices.Sort(small)
	rest := []int{}
	for slot := 0; len(rest) < 4; slot += 1 {
		if !slices.Contains(small, slot) {
			rest = append(rest, slot)
		}
	}
	self.masters = []*PushTokenMaster{
		{Address: "192.0.2.10:7000", RunId: "run-m0-a", Slots: rest},
		{Address: "192.0.2.11:7000", RunId: "run-m1-a", Slots: slices.Clone(small)},
	}
	ctx := context.Background()
	for _, master := range self.masters {
		for _, slot := range master.Slots {
			if err := self.redis.Set(ctx, PushTokenSentinelKey(slot), []byte(master.RunId), 0); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The master at address restarts: it comes back with a new run_id holding what keep returns true for of
// its slots' keys, nothing if keep is nil.
func (self *testPushTokenCluster) restart(address string, runId string, keep func(key string) bool) {
	master := self.master(address)
	master.RunId = runId
	ctx := context.Background()
	for key := range self.redis.Dump() {
		if slices.Contains(master.Slots, sidecartest.RedisClusterSlot(key)) && (keep == nil || !keep(key)) {
			server.Raise(self.redis.Delete(ctx, []string{key}))
		}
	}
}

// A pass on the cluster at its clock's now.
func (self *testPushTokenCluster) pass(t testing.TB, ctx context.Context, state *PushTokenPassState, settings *SidecarPushTokenPassSettings) (*PushTokenPassState, *PushTokenPassReport) {
	t.Helper()
	next, report, err := RunPushTokenPass(ctx, state, self.clock.Now(), true, self.store(), self, settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	return next, report
}

// A client id whose {scp_<C>} hashes to slot.
func testPushTokenClientInSlot(slot int) server.Id {
	for {
		clientId := server.NewId()
		if PushTokenSlot(clientId) == slot {
			return clientId
		}
	}
}

// A client id on a slot of its own, none of avoid.
func testPushTokenClientOutside(avoid []int) server.Id {
	for {
		clientId := server.NewId()
		if !slices.Contains(avoid, PushTokenSlot(clientId)) {
			return clientId
		}
	}
}

// Commits clientId's row as a POST would, without its apply, as a killed api leaves it.
func commitTestPushTokenRow(t testing.TB, ctx context.Context, clientId server.Id, platform string, token string) *SidecarPushTokenRow {
	t.Helper()
	var result *SetPushTokenResult
	server.Tx(ctx, func(tx server.PgTx) {
		result = setPushTokenInTx(ctx, tx, clientId, platform, token)
	})
	return result.Row
}

// Moves clientIds' rows' update_time back by d, as rows that changed long before a restart, which no
// sweep reaches.
func ageTestPushTokenRows(t testing.TB, ctx context.Context, clientIds []server.Id, d time.Duration) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE sidecar_push_token SET update_time = update_time - $2 * interval '1 second' WHERE client_id = ANY($1)`, clientIds, d.Seconds()))
	})
}

// Applies row on the cluster's store with t0 now.
func (self *testPushTokenCluster) apply(t testing.TB, ctx context.Context, row *SidecarPushTokenRow) pushservice.WakeTokenApplyResult {
	t.Helper()
	applied, err := pushservice.ApplyWakeToken(ctx, self.redis, row.wakeTokenRow(), self.clock.Now().UnixMilli(), DefaultSidecarPushTokenSettings().ApplyWindow)
	if err != nil {
		t.Fatal(err)
	}
	return applied
}

// What clientId's tok and tokrev hold on the fake, "" for absent.
func (self *testPushTokenCluster) tokenKeys(t testing.TB, ctx context.Context, clientId server.Id) (string, string) {
	t.Helper()
	tok, _, err := self.redis.Get(ctx, pushservice.HintTokenKey(connect.Id(clientId)))
	if err != nil {
		t.Fatal(err)
	}
	tokrev, _, err := self.redis.Get(ctx, pushservice.WakeTokenRevisionKey(connect.Id(clientId)))
	if err != nil {
		t.Fatal(err)
	}
	return string(tok), string(tokrev)
}

// Fails unless clientId's keys project row.
func (self *testPushTokenCluster) expectProjected(t testing.TB, ctx context.Context, row *SidecarPushTokenRow) {
	t.Helper()
	tok, tokrev := self.tokenKeys(t, ctx, row.ClientId)
	wantTok := ""
	if !row.Tombstone() {
		wantTok = string(pushservice.WakeTokenValue(row.Platform, row.Token))
	}
	if tok != wantTok || tokrev != strconv.FormatInt(row.Rev, 10) {
		t.Fatalf("tok %q tokrev %q, want %q at %d", tok, tokrev, wantTok, row.Rev)
	}
}

// The pass's settings for these tests: the defaults, with no sweep reaching back past the last pass's
// start, so a test's rows from before land only as it says.
func testPushTokenPassSettings() *SidecarPushTokenPassSettings {
	return DefaultSidecarPushTokenPassSettings()
}

// A master restarted from an older dump: the pass that finds its slots and the next, 60 s on, leave the
// older token in place, and the first pass at least 90 s after the finding reconciles the slot to the
// newest row and sets the sentinel to the new run_id, after which nothing is found.
func TestReconcileWaits90s(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cluster := newTestPushTokenCluster(t, time.Now())
		clientId := server.NewId()
		slot := PushTokenSlot(clientId)
		cluster.twoMasters(t, []int{slot})

		older := commitTestPushTokenRow(t, ctx, clientId, sidecar.PushTokenPlatformApns, "older-token")
		cluster.apply(t, ctx, older)
		newer := commitTestPushTokenRow(t, ctx, clientId, sidecar.PushTokenPlatformApns, "newer-token")
		cluster.apply(t, ctx, newer)
		ageTestPushTokenRows(t, ctx, []server.Id{clientId}, time.Hour)
		// m1 restarts from a dump taken while tok held the older row
		olderValue := string(pushservice.WakeTokenValue(older.Platform, older.Token))
		cluster.restart("192.0.2.11:7000", "run-m1-b", func(key string) bool { return false })
		server.Raise(cluster.redis.Set(ctx, pushservice.HintTokenKey(connect.Id(clientId)), []byte(olderValue), 0))
		server.Raise(cluster.redis.Set(ctx, pushservice.WakeTokenRevisionKey(connect.Id(clientId)), []byte(strconv.FormatInt(older.Rev, 10)), 0))
		server.Raise(cluster.redis.Set(ctx, PushTokenSentinelKey(slot), []byte("run-m1-a"), 0))

		settings := testPushTokenPassSettings()
		state, report := cluster.pass(t, ctx, &PushTokenPassState{LastCompletedStartTime: cluster.clock.Now()}, settings)
		if report.FoundSlotCount != 1 || report.ReconciledSlotCount != 0 || report.WaitingSlotCount != 1 {
			t.Fatalf("finding pass: %+v", report)
		}
		cluster.clock.Advance(60 * time.Second)
		state, report = cluster.pass(t, ctx, state, settings)
		if report.FoundSlotCount != 0 || report.ReconciledSlotCount != 0 || report.WaitingSlotCount != 1 {
			t.Fatalf("60 s on: %+v", report)
		}
		if tok, _ := cluster.tokenKeys(t, ctx, clientId); tok != olderValue {
			t.Fatalf("reconciled early: tok %q", tok)
		}
		cluster.clock.Advance(30 * time.Second)
		state, report = cluster.pass(t, ctx, state, settings)
		if report.ReconciledSlotCount != 1 || report.ReconciledRowCount != 1 || report.WaitingSlotCount != 0 {
			t.Fatalf("90 s on: %+v", report)
		}
		cluster.expectProjected(t, ctx, newer)
		if sentinel, _, _ := cluster.redis.Get(ctx, PushTokenSentinelKey(slot)); string(sentinel) != "run-m1-b" {
			t.Fatalf("sentinel %q", sentinel)
		}
		cluster.clock.Advance(60 * time.Second)
		if _, report = cluster.pass(t, ctx, state, settings); report.FoundSlotCount != 0 || report.WaitingSlotCount != 0 || report.ReconciledSlotCount != 0 {
			t.Fatalf("after: %+v", report)
		}
	})
}

// A found slot that another master takes before its 90 s waits again from the pass that sees the move,
// under the new master, while the old master's other found slots reconcile on time.
func TestSlotMovedMeanwhileWaitsAgain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cluster := newTestPushTokenCluster(t, time.Now())
		movedClientId := server.NewId()
		movedSlot := PushTokenSlot(movedClientId)
		stayingClientId := testPushTokenClientOutside([]int{movedSlot})
		stayingSlot := PushTokenSlot(stayingClientId)
		cluster.twoMasters(t, []int{movedSlot, stayingSlot})
		cluster.masters = append(cluster.masters, &PushTokenMaster{Address: "192.0.2.12:7000", RunId: "run-m2-a", Slots: []int{}})
		moved := commitTestPushTokenRow(t, ctx, movedClientId, sidecar.PushTokenPlatformFcm, "moved-token")
		staying := commitTestPushTokenRow(t, ctx, stayingClientId, sidecar.PushTokenPlatformFcm, "staying-token")
		ageTestPushTokenRows(t, ctx, []server.Id{movedClientId, stayingClientId}, time.Hour)
		cluster.restart("192.0.2.11:7000", "run-m1-b", nil)

		settings := testPushTokenPassSettings()
		state, report := cluster.pass(t, ctx, &PushTokenPassState{LastCompletedStartTime: cluster.clock.Now()}, settings)
		if report.FoundSlotCount != 2 {
			t.Fatalf("finding pass: %+v", report)
		}
		// m2 takes the slot 60 s on, empty
		cluster.clock.Advance(60 * time.Second)
		m1 := cluster.master("192.0.2.11:7000")
		m1.Slots = slices.DeleteFunc(m1.Slots, func(slot int) bool { return slot == movedSlot })
		cluster.master("192.0.2.12:7000").Slots = []int{movedSlot}
		state, report = cluster.pass(t, ctx, state, settings)
		if report.FoundSlotCount != 1 || report.ReconciledSlotCount != 0 {
			t.Fatalf("the moving pass: %+v", report)
		}
		cluster.clock.Advance(30 * time.Second)
		state, report = cluster.pass(t, ctx, state, settings)
		if report.ReconciledSlotCount != 1 || report.WaitingSlotCount != 1 {
			t.Fatalf("90 s after the finding: %+v", report)
		}
		cluster.expectProjected(t, ctx, staying)
		if tok, tokrev := cluster.tokenKeys(t, ctx, movedClientId); tok != "" || tokrev != "" {
			t.Fatalf("the moved slot reconciled early: %q %q", tok, tokrev)
		}
		cluster.clock.Advance(60 * time.Second)
		state, report = cluster.pass(t, ctx, state, settings)
		if report.ReconciledSlotCount != 1 || report.WaitingSlotCount != 0 {
			t.Fatalf("90 s after the move: %+v", report)
		}
		cluster.expectProjected(t, ctx, moved)
		if sentinel, _, _ := cluster.redis.Get(ctx, PushTokenSentinelKey(movedSlot)); string(sentinel) != "run-m2-a" {
			t.Fatalf("sentinel %q", sentinel)
		}
	})
}

// A client whose first POST lands mid-reconcile keeps its token, whether its row commits after the slot's
// keys are listed and before its rows are read, or after the rows are read and before the unlisted
// clients' keys go: keys are listed first, and only a listed client with no row loses its keys.
func TestFirstPostDuringReconcileKeepsToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cluster := newTestPushTokenCluster(t, time.Now())
		slot := PushTokenSlot(server.NewId())
		cluster.twoMasters(t, []int{slot})
		cluster.restart("192.0.2.11:7000", "run-m1-b", nil)

		landed := map[string]*SidecarPushTokenRow{}
		pushTokenReconcileTestHook = func(stage string, hookSlot int) {
			if hookSlot != slot {
				return
			}
			// a new client's first POST: its row commits, then its apply lands
			row := commitTestPushTokenRow(t, ctx, testPushTokenClientInSlot(slot), sidecar.PushTokenPlatformApns, "first-"+stage)
			if applied := cluster.apply(t, ctx, row); applied != pushservice.WakeTokenApplied {
				t.Errorf("%s: the POST's apply %s", stage, applied)
			}
			landed[stage] = row
		}
		defer func() { pushTokenReconcileTestHook = nil }()

		settings := testPushTokenPassSettings()
		state, _ := cluster.pass(t, ctx, &PushTokenPassState{LastCompletedStartTime: cluster.clock.Now()}, settings)
		cluster.clock.Advance(90 * time.Second)
		_, report := cluster.pass(t, ctx, state, settings)
		if report.ReconciledSlotCount != 1 || report.DeletedClientCount != 0 || len(landed) != 2 {
			t.Fatalf("%+v, %d posts", report, len(landed))
		}
		for _, row := range landed {
			cluster.expectProjected(t, ctx, row)
		}
	})
}

// In a reconciled slot, a listed client with no row, its row deleted since the dump the master restarted
// from, loses tok and tokrev, and a client with a row keeps its newest.
func TestClientWithNoRowDeleted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cluster := newTestPushTokenCluster(t, time.Now())
		orphanClientId := server.NewId()
		slot := PushTokenSlot(orphanClientId)
		keptClientId := testPushTokenClientInSlot(slot)
		cluster.twoMasters(t, []int{slot})
		kept := commitTestPushTokenRow(t, ctx, keptClientId, sidecar.PushTokenPlatformFcm, "kept-token")
		cluster.apply(t, ctx, kept)
		orphan := &SidecarPushTokenRow{ClientId: orphanClientId, Platform: sidecar.PushTokenPlatformFcm, Token: "deleted-row-token", Rev: 7}
		cluster.apply(t, ctx, orphan)
		cluster.restart("192.0.2.11:7000", "run-m1-b", func(key string) bool { return true })

		settings := testPushTokenPassSettings()
		state, _ := cluster.pass(t, ctx, &PushTokenPassState{LastCompletedStartTime: cluster.clock.Now()}, settings)
		cluster.clock.Advance(90 * time.Second)
		_, report := cluster.pass(t, ctx, state, settings)
		if report.ReconciledSlotCount != 1 || report.DeletedClientCount != 1 {
			t.Fatalf("%+v", report)
		}
		if tok, tokrev := cluster.tokenKeys(t, ctx, orphanClientId); tok != "" || tokrev != "" {
			t.Fatalf("the orphan kept %q %q", tok, tokrev)
		}
		cluster.expectProjected(t, ctx, kept)
	})
}

// A row whose apply was lost with a killed api lands at the next pass's sweep, with no app launch; a row
// changed before the last completed pass's start less 2 minutes is not swept again.
func TestSweepLandsLostApply(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cluster := newTestPushTokenCluster(t, time.Now())
		cluster.twoMasters(t, []int{})
		lost := commitTestPushTokenRow(t, ctx, server.NewId(), sidecar.PushTokenPlatformApns, "lost-apply")
		old := commitTestPushTokenRow(t, ctx, server.NewId(), sidecar.PushTokenPlatformApns, "old-row")
		lastStart := lost.UpdateTime.Add(30 * time.Second)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE sidecar_push_token SET update_time = $2 WHERE client_id = $1`, old.ClientId, lastStart.Add(-3*time.Minute)))
		})

		cluster.clock.AdvanceTo(lastStart.Add(60 * time.Second))
		state, report := cluster.pass(t, ctx, &PushTokenPassState{LastCompletedStartTime: lastStart}, testPushTokenPassSettings())
		if report.SweptRowCount != 1 || !report.SweepComplete || state.Sweep != nil || !state.LastCompletedStartTime.Equal(cluster.clock.Now()) {
			t.Fatalf("%+v, %+v", report, state)
		}
		cluster.expectProjected(t, ctx, lost)
		if tok, tokrev := cluster.tokenKeys(t, ctx, old.ClientId); tok != "" || tokrev != "" {
			t.Fatalf("an old row was swept: %q %q", tok, tokrev)
		}
		// with no completed pass on record, a sweep reads every row
		_, report = cluster.pass(t, ctx, &PushTokenPassState{}, testPushTokenPassSettings())
		if report.SweptRowCount != 2 {
			t.Fatalf("a first sweep: %+v", report)
		}
	})
}

// A row goes 24 h after its client's removal, after its tok and tokrev, whether or not the wake switch is
// on; one removed less than 24 h ago, and a live one, stay.
func TestRowDeleted24hAfterRemoval(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cluster := newTestPushTokenCluster(t, time.Now())
		cluster.twoMasters(t, []int{})
		rows := []*SidecarPushTokenRow{}
		for range 3 {
			row := commitTestPushTokenRow(t, ctx, server.NewId(), sidecar.PushTokenPlatformFcm, testPushTokenValue)
			cluster.apply(t, ctx, row)
			rows = append(rows, row)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := TombstonePushTokenInTx(ctx, tx, []server.Id{rows[0].ClientId, rows[1].ClientId})
			server.Raise(err)
		})
		now := cluster.clock.Now()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE sidecar_push_token SET removed_time = $2 WHERE client_id = $1`, rows[0].ClientId, now.Add(-25*time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE sidecar_push_token SET removed_time = $2 WHERE client_id = $1`, rows[1].ClientId, now.Add(-23*time.Hour)))
		})
		for _, removed := range rows[:2] {
			row := testPushTokenRow(t, ctx, removed.ClientId)
			cluster.apply(t, ctx, row)
		}

		_, report, err := RunPushTokenPass(ctx, &PushTokenPassState{LastCompletedStartTime: now}, now, false, cluster.store(), cluster, testPushTokenPassSettings(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if report.DeletedRowCount != 1 || report.SweptRowCount != 0 || report.FoundSlotCount != 0 {
			t.Fatalf("%+v", report)
		}
		if row := testPushTokenRow(t, ctx, rows[0].ClientId); row != nil {
			t.Fatalf("a row 25 h after its removal: %s", row)
		}
		if tok, tokrev := cluster.tokenKeys(t, ctx, rows[0].ClientId); tok != "" || tokrev != "" {
			t.Fatalf("its keys stayed: %q %q", tok, tokrev)
		}
		for _, staying := range rows[1:] {
			if row := testPushTokenRow(t, ctx, staying.ClientId); row == nil {
				t.Fatalf("%s went", staying)
			}
		}
	})
}

// Each part of a pass stops at its bound and the next pass resumes where it stopped: the sweep at its row
// count, by its cursor, completing only when a page comes up short, its start then the last completed;
// the reconciles at their slot count; the deletions at their row count.
func TestPassWorkBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cluster := newTestPushTokenCluster(t, time.Now())
		dueSlots := []int{}
		clientIds := []server.Id{}
		for range 3 {
			clientId := testPushTokenClientOutside(dueSlots)
			clientIds = append(clientIds, clientId)
			dueSlots = append(dueSlots, PushTokenSlot(clientId))
		}
		cluster.twoMasters(t, dueSlots)
		for _, clientId := range clientIds {
			commitTestPushTokenRow(t, ctx, clientId, sidecar.PushTokenPlatformApns, testPushTokenValue)
		}
		for range 2 {
			commitTestPushTokenRow(t, ctx, server.NewId(), sidecar.PushTokenPlatformApns, testPushTokenValue)
		}
		cluster.restart("192.0.2.11:7000", "run-m1-b", nil)

		settings := testPushTokenPassSettings()
		settings.SweepRowCount = 2
		settings.SweepPageCount = 2
		settings.ReconcileSlotCount = 1
		firstStart := cluster.clock.Now()
		state, report := cluster.pass(t, ctx, &PushTokenPassState{}, settings)
		if report.SweptRowCount != 2 || report.SweepComplete || state.Sweep == nil || !report.More || report.FoundSlotCount != 3 {
			t.Fatalf("pass 1: %+v", report)
		}
		cluster.clock.Advance(60 * time.Second)
		state, report = cluster.pass(t, ctx, state, settings)
		if report.SweptRowCount != 2 || report.SweepComplete {
			t.Fatalf("pass 2: %+v", report)
		}
		cluster.clock.Advance(30 * time.Second)
		state, report = cluster.pass(t, ctx, state, settings)
		if report.SweptRowCount != 1 || !report.SweepComplete || state.Sweep != nil || !state.LastCompletedStartTime.Equal(firstStart) {
			t.Fatalf("pass 3: %+v %+v", report, state)
		}
		if report.ReconciledSlotCount != 1 || report.WaitingSlotCount != 2 || !report.More {
			t.Fatalf("pass 3's reconciles: %+v", report)
		}
		for pass := 4; pass <= 5; pass += 1 {
			cluster.clock.Advance(10 * time.Second)
			state, report = cluster.pass(t, ctx, state, settings)
			if report.ReconciledSlotCount != 1 {
				t.Fatalf("pass %d: %+v", pass, report)
			}
		}
		if report.WaitingSlotCount != 0 || !slices.Equal(cluster.listed, slices.Sorted(slices.Values(dueSlots))) {
			t.Fatalf("listed %v of %v: %+v", cluster.listed, dueSlots, report)
		}

		// the deletions: two rows removed long ago, one a pass
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := TombstonePushTokenInTx(ctx, tx, clientIds[:2])
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE sidecar_push_token SET removed_time = $2 WHERE client_id = ANY($1)`, clientIds[:2], cluster.clock.Now().Add(-48*time.Hour)))
		})
		settings.DeleteRowCount = 1
		for pass := range 2 {
			if _, report = cluster.pass(t, ctx, state, settings); report.DeletedRowCount != 1 {
				t.Fatalf("deletion pass %d: %+v", pass, report)
			}
		}
	})
}

// The pass on the test stack's Redis, one node: the node is one master serving every slot, its run_id
// read by INFO; a pass finds every slot whose sentinel is absent, and the next, ReconcileWait on, lists
// the low slots by SCAN and reconciles them, so a stale token there goes and the sentinels name the node.
func TestPushTokenPassOnTheTestRedis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		store, topology := NewPushTokenServerRedis(ctx)
		masters, err := topology.Masters(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(masters) != 1 || masters[0].RunId == "" || len(masters[0].Slots) != server.RedisSlotCount {
			t.Fatalf("masters %+v", masters)
		}
		lowSlots := 64
		clientId := server.NewId()
		for lowSlots <= PushTokenSlot(clientId) {
			clientId = server.NewId()
		}
		orphan := &SidecarPushTokenRow{ClientId: clientId, Platform: sidecar.PushTokenPlatformApns, Token: "orphan-token", Rev: 3}
		t0, err := pushservice.ReadWakeTokenTime(ctx, store, connect.Id(clientId))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pushservice.ApplyWakeToken(ctx, store, orphan.wakeTokenRow(), t0, time.Minute); err != nil {
			t.Fatal(err)
		}
		for slot := range lowSlots {
			server.Raise(store.Delete(ctx, []string{PushTokenSentinelKey(slot)}))
		}

		settings := DefaultSidecarPushTokenPassSettings()
		settings.ReconcileSlotCount = lowSlots
		start := time.Now()
		state, report, err := RunPushTokenPass(ctx, &PushTokenPassState{LastCompletedStartTime: start}, start, true, store, topology, settings, nil)
		if err != nil {
			t.Fatal(err)
		}
		if report.FoundSlotCount < lowSlots {
			t.Fatalf("finding pass: %+v", report)
		}
		_, report, err = RunPushTokenPass(ctx, state, start.Add(settings.ReconcileWait), true, store, NewPushTokenRedisTopology(redisClientOf(ctx)), settings, nil)
		if err != nil {
			t.Fatal(err)
		}
		if report.ReconciledSlotCount != lowSlots || report.DeletedClientCount < 1 {
			t.Fatalf("reconciling pass: %+v", report)
		}
		values, err := store.GetEach(ctx, []string{pushservice.HintTokenKey(connect.Id(clientId)), PushTokenSentinelKey(PushTokenSlot(clientId))})
		if err != nil {
			t.Fatal(err)
		}
		if values[0] != nil || string(values[1]) != masters[0].RunId {
			t.Fatalf("tok %q, sentinel %q", values[0], values[1])
		}
	})
}

// server's Redis client.
func redisClientOf(ctx context.Context) server.RedisClient {
	var client server.RedisClient
	server.Redis(ctx, func(c server.RedisClient) {
		client = c
	})
	return client
}

// A slot whose reconcile fails leaves the rest of its pass's progress in the state the pass returns: the
// slot it reconciled is no longer found, the slot it newly found is, and the failed slot waits for the next
// pass.
func TestReconcileProgressSurvivesAFailedSlot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cluster := newTestPushTokenCluster(t, time.Now())
		lowSlots := []int{0, 1, 2, 3, 4, 5, 6, 7}
		failingSlot := PushTokenSlot(testPushTokenClientOutside(lowSlots))
		reconciledSlot := PushTokenSlot(testPushTokenClientOutside(append(slices.Clone(lowSlots), failingSlot)))
		lateSlot := PushTokenSlot(testPushTokenClientOutside(append(slices.Clone(lowSlots), failingSlot, reconciledSlot)))
		cluster.twoMasters(t, []int{failingSlot, reconciledSlot})
		cluster.restart("192.0.2.11:7000", "run-m1-b", nil)

		settings := testPushTokenPassSettings()
		state, report := cluster.pass(t, ctx, &PushTokenPassState{LastCompletedStartTime: cluster.clock.Now()}, settings)
		if report.FoundSlotCount != 2 {
			t.Fatalf("finding pass: %+v", report)
		}
		// 90 s on m1 also serves lateSlot, with no sentinel, and failingSlot's listing fails
		cluster.clock.Advance(90 * time.Second)
		m1 := cluster.master("192.0.2.11:7000")
		m1.Slots = append(m1.Slots, lateSlot)
		cluster.failSlots = map[int]error{failingSlot: errors.New("a listing that fails")}
		next, report, err := RunPushTokenPass(ctx, state, cluster.clock.Now(), true, cluster.store(), cluster, settings, nil)
		if err == nil {
			t.Fatal("the failed slot was no error")
		}
		if report.ReconciledSlotCount != 1 || report.FoundSlotCount != 1 || report.WaitingSlotCount != 2 {
			t.Fatalf("%+v", report)
		}
		found := map[int]bool{}
		for _, group := range next.Found {
			for _, slot := range group.Slots {
				found[slot] = true
			}
		}
		if len(found) != 2 || !found[failingSlot] || !found[lateSlot] {
			t.Fatalf("found %v, want %d and %d", found, failingSlot, lateSlot)
		}
	})
}
