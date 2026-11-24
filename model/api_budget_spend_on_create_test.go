package model

// api's rev/ and hold/ writers spend the process's write budget once the store shows a write took effect
// (api_write_spend.go), as the authorization log's sequencer spends its objects (config_log_budget_test.go). Each test
// runs on T2's fake Blobs behind evsservice.BudgetedBlobs, so a PUT whose ctx no budget passed is refused, with the
// PUTs sent counted by key, and holds each writer's budget to the writes the store shows took effect, priced as
// testApiBudgetPrices prices them, each write at its own cost row:
//
//   - a rev/ write racing another writer spends nothing for the PUT the racer's object refused 412: over an object at
//     or past its not_before it ends current, and over one below it raises it, spending that raise once;
//   - a rev/ PUT that never landed, dead before the store took it or refused 412 over nothing, is sent again and
//     spends once, a create and a raise alike;
//   - a rev/ PUT whose answer was lost after it landed spends once and counts as the create or raise it was, not as
//     an object already current;
//   - a rev/ PUT whose outcome stayed unknown, the read after it failing too, spends once across the retry that
//     settles it, landed or not, at the same time or later, and counts as created once;
//   - a hold's create whose answer was lost spends once across its retry, which the create's own object refuses 412,
//     as does one whose read failed too, landed or not, and one dead before the store took it;
//   - a hold's Rewrite refused If-Match, or dead before the store took it, spends nothing, as does the create of a
//     missing object refused 412 or dead; each spends once for its retry that lands, and one whose answer was lost
//     after it landed spends once;
//   - the settings this change adds hold: past UnsettledWriteCount an unsettled PUT is spent and not remembered, and
//     a rev/ writer with no object bound refuses every write.
//
// Each but the last two fails on the code before this change, which issued every PUT's price to the budget before
// sending it and counted a rev/ create whose answer was lost as current.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// The cost rows the tests price writes at: §11.13's three rows as a table holds them, each its own fixed part, odd so
// no few writes of one row sum to another's, and a part for each byte, so a total tells how many writes of each row
// were spent and of what.
func testApiBudgetPrices() *evsservice.PriceTable {
	return &evsservice.PriceTable{
		BlockByteCount:       1,
		SmallObjectByteCount: 0,
		CreateByteCount:      9001,
		OverwriteByteCount:   14009,
		DeleteByteCount:      3511,
		ShardCount:           1,
	}
}

// What one write of operation on an object of objectByteCount bytes prices at under testApiBudgetPrices.
func testApiBudgetPrice(operation evsservice.WriteOperationKind, objectByteCount int64) int64 {
	price, err := testApiBudgetPrices().Price(evsservice.WriteOperation{Kind: operation, ObjectByteCount: objectByteCount, Count: 1})
	if err != nil {
		panic(err)
	}
	return price
}

// A first cluster that counts the PUTs sent to each key.
type testApiBudgetCluster struct {
	sidecar.BlobCluster

	stateLock    sync.Mutex
	putKeyCounts map[string]int
}

func (self *testApiBudgetCluster) countPut(key string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.putKeyCounts[key] += 1
}

// Counts the PUT, then sends it.
func (self *testApiBudgetCluster) PutIfNoneMatch(ctx context.Context, key string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	self.countPut(key)
	return self.BlobCluster.PutIfNoneMatch(ctx, key, body, options)
}

// Counts the PUT, then sends it.
func (self *testApiBudgetCluster) PutIfMatch(ctx context.Context, key string, etag string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	self.countPut(key)
	return self.BlobCluster.PutIfMatch(ctx, key, etag, body, options)
}

// How many PUTs were sent to key.
func (self *testApiBudgetCluster) putCount(key string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.putKeyCounts[key]
}

// T2's fake first cluster behind BudgetedBlobs, its PUTs counted, its clock, and the queue its faults are armed on.
type testApiBudgetWorld struct {
	t       *testing.T
	clock   *sidecartest.FakeClock
	faults  *sidecartest.FaultQueue
	cluster *testApiBudgetCluster
}

func newTestApiBudgetWorld(t *testing.T) *testApiBudgetWorld {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	faults := sidecartest.NewFaultQueue()
	blobs := sidecartest.NewFakeBlobs(clock, faults, nil, sidecartest.DefaultFakeBlobsSettings())
	budgeted, err := evsservice.NewBudgetedBlobs(blobs).FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	return &testApiBudgetWorld{
		t:       t,
		clock:   clock,
		faults:  faults,
		cluster: &testApiBudgetCluster{BlobCluster: budgeted, putKeyCounts: map[string]int{}},
	}
}

// A rev/ writer on the world's cluster and clock, as one api instance runs it, signing as the rev tests' stand-in
// does, and spending a budget of its own.
func (self *testApiBudgetWorld) revStore() (*SidecarRevStore, *sidecartest.FakeWriteBudget) {
	settings := DefaultSidecarRevSettings()
	settings.Clock = self.clock
	store := NewSidecarRevStore(self.cluster, testRevSign, settings)
	budget := sidecartest.NewFakeWriteBudget(nil)
	store.SetWriteBudget(budget, testApiBudgetPrices())
	return store, budget
}

// A hold/ writer on the world's cluster, spending a budget of its own.
func (self *testApiBudgetWorld) holdStore() (*AbuseHoldStore, *sidecartest.FakeWriteBudget) {
	store := NewAbuseHoldStore(self.cluster)
	budget := sidecartest.NewFakeWriteBudget(nil)
	store.SetWriteBudget(budget, testApiBudgetPrices())
	return store, budget
}

// Arms a fault on the world's store.
func (self *testApiBudgetWorld) fault(plan *sidecartest.FaultPlan) {
	self.t.Helper()
	if _, err := self.faults.Add(plan); err != nil {
		self.t.Fatal(err)
	}
}

// Fails the test unless every armed fault fired.
func (self *testApiBudgetWorld) checkFaultsFired(step string) {
	self.t.Helper()
	if unfired := self.faults.Unfired(); len(unfired) != 0 {
		self.t.Errorf("%s: faults that never fired: %v", step, unfired)
	}
}

// What a write of operation of the object at key, as it stands, prices at: the P0 one such write of it spends. A HEAD
// is a GET to the fault queue, so this is read only once the GET faults armed fired.
func (self *testApiBudgetWorld) objectPrice(key string, operation evsservice.WriteOperationKind) int64 {
	self.t.Helper()
	info, err := self.cluster.Head(self.t.Context(), key)
	if err != nil {
		self.t.Fatal(err)
	}
	return testApiBudgetPrice(operation, info.ByteCount)
}

// The P0 of kind the budgets spent on the world's cluster, summed, failing the test on any violation of the
// interface one saw.
func (self *testApiBudgetWorld) spent(kind sidecar.WriteP0Kind, budgets ...*sidecartest.FakeWriteBudget) int64 {
	self.t.Helper()
	spent := int64(0)
	for _, budget := range budgets {
		spent += budget.P0ByteCount(self.cluster.ClusterId(), kind)
		if violations := budget.Violations(); len(violations) != 0 {
			self.t.Errorf("the budget's violations: %v", violations)
		}
	}
	return spent
}

// The not_before the rev/ object of revocation's subject holds, the zero time for none.
func (self *testApiBudgetWorld) revNotBefore(revocation *SidecarRevocation) time.Time {
	self.t.Helper()
	record, err := sidecar.ReadRevocation(self.t.Context(), self.cluster, testRevVerify,
		sidecar.Id(revocation.NetworkId), revocation.Kind, sidecar.Id(revocation.SubjectId))
	if err != nil {
		self.t.Fatal(err)
	}
	if record == nil {
		return time.Time{}
	}
	return record.NotBefore
}

// How many writes the rev/ writers counted as outcome so far.
func testApiBudgetRevWriteCount(outcome string) float64 {
	return testutil.ToFloat64(sidecarRevWriteCounter.WithLabelValues(outcome))
}

// A preserve_2703f hold of a new network and case placed at placeTime, as placement writes its object.
func testApiBudgetHoldRecord(placeTime time.Time) *sidecar.HoldRecord {
	placeTime = placeTime.UTC().Truncate(time.Millisecond)
	return &sidecar.HoldRecord{
		NetworkId:  sidecar.NewId(),
		CaseId:     sidecar.NewId(),
		Kind:       sidecar.HoldKindPreserve2703f,
		Scope:      sidecar.HoldScopeNetwork(),
		PlaceTime:  placeTime,
		ExpireTime: placeTime.AddDate(0, 0, LegalHoldPreserveDays),
	}
}

// The bytes of record's hold object, nil for none.
func (self *testApiBudgetWorld) holdBody(store *AbuseHoldStore, record *sidecar.HoldRecord) []byte {
	self.t.Helper()
	object, found, err := store.Read(self.t.Context(), server.Id(record.NetworkId), server.Id(record.CaseId))
	if err != nil {
		self.t.Fatal(err)
	}
	if !found {
		return nil
	}
	return object.Body
}

// A rev/ write racing another writer, another api instance whose object lands between the write's read and its PUT,
// spends nothing for the PUT that object refused 412: over an object at or past its not_before the write ends
// current, and over one below it raises it, spending the raise once; a raise refused 412 by the racer's raise past it
// spends nothing either. The racer spends its own writes once each.
func TestSidecarRevWriteRacingAnotherWriterSpendsOnce(t *testing.T) {
	type testCase struct {
		name string
		// whether an object below the write's not_before stands before the race, the racer's, so the write raises it
		standing bool
		// the racer's not_before, from the write's
		racerOffset time.Duration
		// what the write counts as
		outcome string
		// how many of the write's PUTs took effect
		writeCount int
	}
	testCases := []testCase{
		{name: "a racer's create at the write's not_before", outcome: "current"},
		{name: "a racer's create past it", racerOffset: time.Minute, outcome: "current"},
		{name: "a racer's create below it", racerOffset: -time.Minute, outcome: "raised", writeCount: 1},
		{name: "a racer's raise past the write's raise", standing: true, racerOffset: time.Minute, outcome: "current"},
	}
	for _, testCase := range testCases {
		world := newTestApiBudgetWorld(t)
		ctx := t.Context()
		writer, writerBudget := world.revStore()
		racer, racerBudget := world.revStore()
		revocation := newTestRevocation(world.clock.Now())
		key := revocation.key()

		racerWriteCount := 1
		racerSpent := int64(0)
		if testCase.standing {
			standing := *revocation
			standing.NotBefore = revocation.NotBefore.Add(-time.Minute)
			if err := racer.Write(ctx, &standing); err != nil {
				t.Fatal(err)
			}
			racerWriteCount += 1
			racerSpent += world.objectPrice(key, evsservice.WriteOperationCreate)
		}
		raced := *revocation
		raced.NotBefore = revocation.NotBefore.Add(testCase.racerOffset)
		Testing_SidecarRevBeforePut = func(string) {
			Testing_SidecarRevBeforePut = nil
			if err := racer.Write(ctx, &raced); err != nil {
				t.Errorf("%s: the racer's write: %v", testCase.name, err)
			}
			// over its own standing object the racer raises, an overwrite; else it creates
			if testCase.standing {
				racerSpent += world.objectPrice(key, evsservice.WriteOperationOverwrite)
			} else {
				racerSpent += world.objectPrice(key, evsservice.WriteOperationCreate)
			}
		}
		outcomeCount := testApiBudgetRevWriteCount(testCase.outcome)
		err := writer.Write(ctx, revocation)
		Testing_SidecarRevBeforePut = nil
		if err != nil {
			t.Errorf("%s: %v", testCase.name, err)
			continue
		}

		if count := testApiBudgetRevWriteCount(testCase.outcome); count != outcomeCount+1 {
			t.Errorf("%s: the write counted %v as %s, want 1", testCase.name, count-outcomeCount, testCase.outcome)
		}
		wantNotBefore := revocation.NotBefore
		if raced.NotBefore.After(wantNotBefore) {
			wantNotBefore = raced.NotBefore
		}
		if notBefore := world.revNotBefore(revocation); !notBefore.Equal(wantNotBefore) {
			t.Errorf("%s: the object holds not_before %v, want %v", testCase.name, notBefore, wantNotBefore)
		}
		// the write's refused PUT, its raise if it raised, and the racer's
		if count := world.cluster.putCount(key); count != racerWriteCount+1+testCase.writeCount {
			t.Errorf("%s: %d PUTs, want %d", testCase.name, count, racerWriteCount+1+testCase.writeCount)
		}
		if spent := world.spent(sidecar.WriteP0KindRevocation, racerBudget); spent != racerSpent {
			t.Errorf("%s: the racer spent %d, and its writes price at %d", testCase.name, spent, racerSpent)
		}
		// the write's one write that took effect is a raise over the racer's object, an overwrite
		writerWant := int64(testCase.writeCount) * world.objectPrice(key, evsservice.WriteOperationOverwrite)
		if spent := world.spent(sidecar.WriteP0KindRevocation, writerBudget); spent != writerWant {
			t.Errorf("%s: the write spent %d for %d writes that took effect, which price at %d", testCase.name, spent, testCase.writeCount, writerWant)
		}
	}
}

// A rev/ PUT that never landed, dead before the store took it or refused 412 over nothing, as the read after it
// shows, is sent again and spent once, for the PUT that lands: a create's and a raise's alike.
func TestSidecarRevPutSentAgainAfterItNeverLandedSpendsOnce(t *testing.T) {
	type testCase struct {
		name   string
		result sidecartest.FaultResult
		// whether the write raises an object below its not_before, which it created first
		raise bool
	}
	testCases := []testCase{
		{name: "a create dead before the store took it", result: sidecartest.FailBeforeApply},
		{name: "a create refused 412 over nothing", result: sidecartest.Precondition},
		{name: "a raise dead before the store took it", result: sidecartest.FailBeforeApply, raise: true},
		{name: "a raise refused 412 over its own object", result: sidecartest.Precondition, raise: true},
	}
	for _, testCase := range testCases {
		world := newTestApiBudgetWorld(t)
		ctx := t.Context()
		writer, budget := world.revStore()
		revocation := newTestRevocation(world.clock.Now())
		key := revocation.key()

		spentBefore := int64(0)
		putCountBefore := 0
		outcome := "created"
		operation := evsservice.WriteOperationCreate
		if testCase.raise {
			standing := *revocation
			standing.NotBefore = revocation.NotBefore.Add(-time.Minute)
			if err := writer.Write(ctx, &standing); err != nil {
				t.Fatal(err)
			}
			spentBefore = world.objectPrice(key, evsservice.WriteOperationCreate)
			putCountBefore = 1
			outcome = "raised"
			// a raise is an If-Match PUT over the object: the overwrite row
			operation = evsservice.WriteOperationOverwrite
		}
		world.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: key, Result: testCase.result})
		outcomeCount := testApiBudgetRevWriteCount(outcome)
		if err := writer.Write(ctx, revocation); err != nil {
			t.Errorf("%s: %v", testCase.name, err)
			continue
		}
		world.checkFaultsFired(testCase.name)

		if count := world.cluster.putCount(key); count != putCountBefore+2 {
			t.Errorf("%s: %d PUTs for the write, want the one that never landed and the one sent again", testCase.name, count-putCountBefore)
		}
		if count := testApiBudgetRevWriteCount(outcome); count != outcomeCount+1 {
			t.Errorf("%s: the write counted %v as %s, want 1", testCase.name, count-outcomeCount, outcome)
		}
		if notBefore := world.revNotBefore(revocation); !notBefore.Equal(revocation.NotBefore) {
			t.Errorf("%s: the object holds not_before %v, want %v", testCase.name, notBefore, revocation.NotBefore)
		}
		if spent, want := world.spent(sidecar.WriteP0KindRevocation, budget), spentBefore+world.objectPrice(key, operation); spent != want {
			t.Errorf("%s: spent %d, and the writes that took effect price at %d", testCase.name, spent, want)
		}
	}
}

// A rev/ PUT whose answer was lost after it landed, which the read after it finds, spends once and counts as the
// create or raise it was, not as an object already current.
func TestSidecarRevLostAnswerThatLandedCountsCreated(t *testing.T) {
	type testCase struct {
		name string
		// whether the write raises an object below its not_before, which it created first
		raise bool
	}
	testCases := []testCase{
		{name: "a create"},
		{name: "a raise", raise: true},
	}
	for _, testCase := range testCases {
		world := newTestApiBudgetWorld(t)
		ctx := t.Context()
		writer, budget := world.revStore()
		revocation := newTestRevocation(world.clock.Now())
		key := revocation.key()

		spentBefore := int64(0)
		putCountBefore := 0
		outcome := "created"
		operation := evsservice.WriteOperationCreate
		if testCase.raise {
			standing := *revocation
			standing.NotBefore = revocation.NotBefore.Add(-time.Minute)
			if err := writer.Write(ctx, &standing); err != nil {
				t.Fatal(err)
			}
			spentBefore = world.objectPrice(key, evsservice.WriteOperationCreate)
			putCountBefore = 1
			outcome = "raised"
			// a raise is an If-Match PUT over the object: the overwrite row
			operation = evsservice.WriteOperationOverwrite
		}
		world.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: key, Result: sidecartest.ApplyThenDrop})
		outcomeCount, currentCount := testApiBudgetRevWriteCount(outcome), testApiBudgetRevWriteCount("current")
		if err := writer.Write(ctx, revocation); err != nil {
			t.Errorf("%s: %v", testCase.name, err)
			continue
		}
		world.checkFaultsFired(testCase.name)

		if count := testApiBudgetRevWriteCount(outcome); count != outcomeCount+1 {
			t.Errorf("%s: the write counted %v as %s, want 1", testCase.name, count-outcomeCount, outcome)
		}
		if count := testApiBudgetRevWriteCount("current"); count != currentCount {
			t.Errorf("%s: the write counted %v as current, its own landed PUT's object", testCase.name, count-currentCount)
		}
		if count := world.cluster.putCount(key); count != putCountBefore+1 {
			t.Errorf("%s: %d PUTs for the write, want 1", testCase.name, count-putCountBefore)
		}
		if spent, want := world.spent(sidecar.WriteP0KindRevocation, budget), spentBefore+world.objectPrice(key, operation); spent != want {
			t.Errorf("%s: spent %d, and the writes that took effect price at %d", testCase.name, spent, want)
		}
	}
}

// A rev/ PUT whose outcome stayed unknown, the read after it failing too, is spent as issued, once: the write fails,
// and its retry, which finds the PUT's object or, the PUT having never landed, creates its own, at the same time or
// later, spends nothing more, and counts the create once.
func TestSidecarRevUnsettledPutSpendsOnceAcrossItsRetry(t *testing.T) {
	type testCase struct {
		name   string
		result sidecartest.FaultResult
		// whether the clock moves before the retry, so the object it would write carries another time
		advance bool
		// the PUTs sent across the write and its retry
		putCount int
	}
	testCases := []testCase{
		{name: "a lost answer, retried at once", result: sidecartest.ApplyThenDrop, putCount: 1},
		{name: "a lost answer, retried later", result: sidecartest.ApplyThenDrop, advance: true, putCount: 1},
		{name: "a dead request, retried at once", result: sidecartest.FailBeforeApply, putCount: 2},
		{name: "a dead request, retried later", result: sidecartest.FailBeforeApply, advance: true, putCount: 2},
	}
	for _, testCase := range testCases {
		world := newTestApiBudgetWorld(t)
		ctx := t.Context()
		writer, budget := world.revStore()
		revocation := newTestRevocation(world.clock.Now())
		key := revocation.key()

		world.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: key, Result: testCase.result})
		// the write's read before its PUT passes, and the one after it fails
		world.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpGet, KeyPrefix: key, Nth: 2, Result: sidecartest.FailBeforeApply})
		createdCount, failedCount := testApiBudgetRevWriteCount("created"), testApiBudgetRevWriteCount("failed")
		if err := writer.Write(ctx, revocation); err == nil {
			t.Errorf("%s: a write whose outcome stayed unknown answered nil", testCase.name)
			continue
		}
		world.checkFaultsFired(testCase.name)
		unsettledSpent := world.spent(sidecar.WriteP0KindRevocation, budget)

		if testCase.advance {
			world.clock.Advance(time.Second)
		}
		if err := writer.Write(ctx, revocation); err != nil {
			t.Errorf("%s: the retry: %v", testCase.name, err)
			continue
		}
		if count := world.cluster.putCount(key); count != testCase.putCount {
			t.Errorf("%s: %d PUTs, want %d", testCase.name, count, testCase.putCount)
		}
		if count := testApiBudgetRevWriteCount("created"); count != createdCount+1 {
			t.Errorf("%s: the write and its retry counted %v creates, want 1", testCase.name, count-createdCount)
		}
		if count := testApiBudgetRevWriteCount("failed"); count != failedCount+1 {
			t.Errorf("%s: the write and its retry counted %v failures, want 1", testCase.name, count-failedCount)
		}
		price := world.objectPrice(key, evsservice.WriteOperationCreate)
		if unsettledSpent != price {
			t.Errorf("%s: the write whose outcome stayed unknown spent %d, and its object prices at %d", testCase.name, unsettledSpent, price)
		}
		if spent := world.spent(sidecar.WriteP0KindRevocation, budget); spent != price {
			t.Errorf("%s: the write and its retry spent %d, and the one object prices at %d", testCase.name, spent, price)
		}
	}
}

// A hold's create whose answer was lost spends once across its retry: the create reads its key, finds its own
// object and answers its record, and the retry, refused 412 by that object, spends nothing more. One whose read
// failed too is spent as issued, once, and its retry, refused 412 by the object if the create landed and creating it
// if not, spends nothing more; one dead before the store took it, as the read after it shows, spends nothing, and its
// retry creates the object once.
func TestAbuseHoldCreateRetriedAfterALostAnswerSpendsOnce(t *testing.T) {
	type testCase struct {
		name      string
		putResult sidecartest.FaultResult
		// whether the read after the PUT fails too
		readFails bool
		// whether the first create answers its record, as one that found its own object does
		firstLanded bool
	}
	testCases := []testCase{
		{name: "a lost answer", putResult: sidecartest.ApplyThenDrop, firstLanded: true},
		{name: "a lost answer and a failed read", putResult: sidecartest.ApplyThenDrop, readFails: true},
		{name: "a dead request", putResult: sidecartest.FailBeforeApply},
		{name: "a dead request and a failed read", putResult: sidecartest.FailBeforeApply, readFails: true},
	}
	for _, testCase := range testCases {
		world := newTestApiBudgetWorld(t)
		ctx := t.Context()
		store, budget := world.holdStore()
		record := testApiBudgetHoldRecord(world.clock.Now())
		key := sidecar.HoldObjectKey(record.NetworkId, record.CaseId)
		body, err := sidecar.EncodeHoldRecord(record)
		if err != nil {
			t.Fatal(err)
		}

		world.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: key, Result: testCase.putResult})
		if testCase.readFails {
			world.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpGet, KeyPrefix: key, Result: sidecartest.FailBeforeApply})
		}
		stored, err := store.Create(ctx, record)
		if testCase.firstLanded {
			if err != nil || !stored.PlaceTime.Equal(record.PlaceTime) {
				t.Errorf("%s: a create that found its own object answered %+v, %v", testCase.name, stored, err)
			}
		} else if err == nil {
			t.Errorf("%s: a create whose outcome was not known answered %+v", testCase.name, stored)
		}
		world.checkFaultsFired(testCase.name)

		stored, err = store.Create(ctx, record)
		if err != nil || !stored.PlaceTime.Equal(record.PlaceTime) {
			t.Errorf("%s: the retry answered %+v, %v", testCase.name, stored, err)
			continue
		}
		if held := world.holdBody(store, record); string(held) != string(body) {
			t.Errorf("%s: the object holds %s, want %s", testCase.name, held, body)
		}
		if count := world.cluster.putCount(key); count != 2 {
			t.Errorf("%s: %d PUTs, want the create's and its retry's", testCase.name, count)
		}
		if spent, price := world.spent(sidecar.WriteP0KindLifecycle, budget), world.objectPrice(key, evsservice.WriteOperationCreate); spent != price {
			t.Errorf("%s: the create and its retry spent %d, and the one object prices at %d", testCase.name, spent, price)
		}
	}
}

// A hold's Rewrite refused If-Match spends nothing, as does one dead before the store took it, which the object it
// was conditioned on, still standing, shows, and the create of a missing object refused 412 over nothing or dead;
// each, retried, spends once, for the write that lands. A Rewrite whose answer was lost after it landed reads its own
// bytes, answers nil and spends once.
func TestAbuseHoldRewriteRefusedIfMatchSpendsNothing(t *testing.T) {
	type testCase struct {
		name   string
		result sidecartest.FaultResult
		// whether no object stands, so the Rewrite creates it
		missing bool
		// whether the faulted write took effect
		landed bool
	}
	testCases := []testCase{
		{name: "refused If-Match", result: sidecartest.Precondition},
		{name: "dead before the store took it", result: sidecartest.FailBeforeApply},
		{name: "landed, its answer lost", result: sidecartest.ApplyThenDrop, landed: true},
		{name: "a missing object's create refused 412 over nothing", result: sidecartest.Precondition, missing: true},
		{name: "a missing object's create dead before the store took it", result: sidecartest.FailBeforeApply, missing: true},
	}
	for _, testCase := range testCases {
		world := newTestApiBudgetWorld(t)
		ctx := t.Context()
		store, budget := world.holdStore()
		record := testApiBudgetHoldRecord(world.clock.Now())
		key := sidecar.HoldObjectKey(record.NetworkId, record.CaseId)

		spentBefore := int64(0)
		var before []byte
		// a missing object's put-back is a create, and a rewrite of a standing one an If-Match overwrite
		operation := evsservice.WriteOperationCreate
		if !testCase.missing {
			if _, err := store.Create(ctx, record); err != nil {
				t.Fatal(err)
			}
			spentBefore = world.objectPrice(key, evsservice.WriteOperationCreate)
			before = world.holdBody(store, record)
			operation = evsservice.WriteOperationOverwrite
		}
		// the renewal §2703(f)(2) allows, keeping place_time
		renewed := *record
		renewed.ExpireTime = LegalHoldRenewedDeadline(record.PlaceTime)
		renewedBody, err := sidecar.EncodeHoldRecord(&renewed)
		if err != nil {
			t.Fatal(err)
		}

		world.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: key, Result: testCase.result})
		err = store.Rewrite(ctx, &renewed)
		world.checkFaultsFired(testCase.name)
		if testCase.landed {
			if err != nil {
				t.Errorf("%s: a rewrite that found its own bytes answered %v", testCase.name, err)
			}
		} else {
			if err == nil {
				t.Errorf("%s: a write that never landed answered nil", testCase.name)
				continue
			}
			if testCase.result == sidecartest.Precondition && !errors.Is(err, sidecar.ErrBlobPrecondition) {
				t.Errorf("%s: answered %v, not a 412", testCase.name, err)
			}
			if held := world.holdBody(store, record); string(held) != string(before) {
				t.Errorf("%s: the object holds %s after a write that never landed, want %s", testCase.name, held, before)
			}
			if spent := world.spent(sidecar.WriteP0KindLifecycle, budget); spent != spentBefore {
				t.Errorf("%s: spent %d for a write that never landed", testCase.name, spent-spentBefore)
			}
			if err := store.Rewrite(ctx, &renewed); err != nil {
				t.Errorf("%s: the retry: %v", testCase.name, err)
				continue
			}
		}

		if held := world.holdBody(store, record); string(held) != string(renewedBody) {
			t.Errorf("%s: the object holds %s, want %s", testCase.name, held, renewedBody)
		}
		if spent, want := world.spent(sidecar.WriteP0KindLifecycle, budget), spentBefore+world.objectPrice(key, operation); spent != want {
			t.Errorf("%s: spent %d, and the writes that took effect price at %d", testCase.name, spent, want)
		}
	}
}

// Past UnsettledWriteCount a rev/ PUT whose outcome stayed unknown is spent and not remembered: with none
// remembered, the retry that creates the object the PUT never landed spends again, one object over, as the setting
// says.
func TestSidecarRevUnsettledPutPastItsCountSpendsAgain(t *testing.T) {
	world := newTestApiBudgetWorld(t)
	ctx := t.Context()
	settings := DefaultSidecarRevSettings()
	settings.Clock = world.clock
	settings.UnsettledWriteCount = 0
	writer := NewSidecarRevStore(world.cluster, testRevSign, settings)
	budget := sidecartest.NewFakeWriteBudget(nil)
	writer.SetWriteBudget(budget, testApiBudgetPrices())
	revocation := newTestRevocation(world.clock.Now())
	key := revocation.key()

	world.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: key, Result: sidecartest.FailBeforeApply})
	world.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpGet, KeyPrefix: key, Nth: 2, Result: sidecartest.FailBeforeApply})
	if err := writer.Write(ctx, revocation); err == nil {
		t.Fatal("a write whose outcome stayed unknown answered nil")
	}
	world.checkFaultsFired("the unsettled write")
	if err := writer.Write(ctx, revocation); err != nil {
		t.Fatal(err)
	}
	if spent, price := world.spent(sidecar.WriteP0KindRevocation, budget), world.objectPrice(key, evsservice.WriteOperationCreate); spent != 2*price {
		t.Errorf("spent %d with no unsettled PUT remembered, want the unsettled PUT's and the retry's, %d", spent, 2*price)
	}
}

// A rev/ writer whose settings hold no object bound refuses every write, sending and spending nothing, since a write
// that cannot read the standing object could lower its not_before.
func TestSidecarRevWriteRefusesWithoutAnObjectBound(t *testing.T) {
	world := newTestApiBudgetWorld(t)
	ctx := t.Context()
	revocation := newTestRevocation(world.clock.Now())
	key := revocation.key()
	standing, _ := world.revStore()
	if err := standing.Write(ctx, revocation); err != nil {
		t.Fatal(err)
	}

	settings := DefaultSidecarRevSettings()
	settings.Clock = world.clock
	settings.ObjectMaxByteCount = 0
	writer := NewSidecarRevStore(world.cluster, testRevSign, settings)
	budget := sidecartest.NewFakeWriteBudget(nil)
	writer.SetWriteBudget(budget, testApiBudgetPrices())
	lower := *revocation
	lower.NotBefore = revocation.NotBefore.Add(-time.Minute)
	if err := writer.Write(ctx, &lower); err == nil {
		t.Fatal("a writer with no object bound wrote")
	}
	if count := world.cluster.putCount(key); count != 1 {
		t.Errorf("%d PUTs, want the standing write's alone", count)
	}
	if notBefore := world.revNotBefore(revocation); !notBefore.Equal(revocation.NotBefore) {
		t.Errorf("the object holds not_before %v, want %v", notBefore, revocation.NotBefore)
	}
	if spent := world.spent(sidecar.WriteP0KindRevocation, budget); spent != 0 {
		t.Errorf("the refused writer spent %d", spent)
	}
}
