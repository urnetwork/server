package model

// §11.13 prices each metadata write by its own cost row, and api's P0 writers spend each write that took effect at
// that row (api_write_spend.go): a create at the create row, an If-Match PUT over a standing object at the overwrite
// row, and a DELETE at the delete row. Each test runs on T2's fake Blobs behind evsservice.BudgetedBlobs, as
// api_budget_spend_on_create_test.go's do, so a PUT or DELETE whose ctx no budget passed is refused:
//
//   - under the table a default budget prices with, which api's and taskworker's installs pass, a rev/ create spends
//     §11.13's create row (~9 KiB) and a raise, an If-Match PUT over the object, its overwrite row (~14 KiB);
//   - a hold's Rewrite of its standing object spends the overwrite row, and the pass's put-back of a missing one the
//     create row;
//   - a hold's Delete spends the delete row (~3.5 KiB), carrying P0 so a budgeted cluster takes it, which one with no
//     budget it refuses; a Delete of a key already gone sends no DELETE and spends nothing;
//   - a hold's Delete whose outcome was unknown spends once across its retry: landed, its answer lost, or never, the
//     read after it failing too or not;
//   - rev/'s expiry sweep and the authorization log's retention spend each DELETE at the delete row, as P0 of kinds
//     revocation and log, a sweep's DELETE whose answer was lost after it landed included;
//   - a write kept unsettled and the write of another row that settles it spend the dearer of their two rows, never
//     less than the write that took effect.
//
// All but the last fail on the code before this change, which priced every write at the create row and sent each
// DELETE with no P0, spending nothing for it.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// A cluster over the world's that counts the DELETEs sent to each key.
type testApiBudgetDeleteCluster struct {
	sidecar.BlobCluster

	stateLock       sync.Mutex
	deleteKeyCounts map[string]int
}

func newTestApiBudgetDeleteCluster(cluster sidecar.BlobCluster) *testApiBudgetDeleteCluster {
	return &testApiBudgetDeleteCluster{BlobCluster: cluster, deleteKeyCounts: map[string]int{}}
}

// Counts the DELETE, then sends it.
func (self *testApiBudgetDeleteCluster) Delete(ctx context.Context, key string) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.deleteKeyCounts[key] += 1
	}()
	return self.BlobCluster.Delete(ctx, key)
}

// How many DELETEs were sent to key.
func (self *testApiBudgetDeleteCluster) deleteCount(key string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.deleteKeyCounts[key]
}

// A ctx a fixture's write on the world's budgeted cluster carries: P0 from a budget of its own, which no test reads.
func (self *testApiBudgetWorld) fixtureContext(kind sidecar.WriteP0Kind) context.Context {
	return sidecartest.NewFakeWriteBudget(nil).IssueP0(self.t.Context(), self.cluster.ClusterId(), kind, 0)
}

// Whether key holds an object.
func (self *testApiBudgetWorld) exists(key string) bool {
	self.t.Helper()
	_, err := self.cluster.Head(self.t.Context(), key)
	if errors.Is(err, sidecar.ErrBlobNotFound) {
		return false
	}
	if err != nil {
		self.t.Fatal(err)
	}
	return true
}

// Under the table a budget built with evsservice's default settings prices with, the one api's install passes, a
// rev/ create spends §11.13's create row and a raise, an If-Match PUT over the object, its overwrite row: on 4 KiB
// blocks ~9 KiB and ~14 KiB, where the create-only price spent the raise at ~9 KiB.
func TestSidecarRevRaiseSpendsTheOverwriteRow(t *testing.T) {
	world := newTestApiBudgetWorld(t)
	ctx := t.Context()
	settings := DefaultSidecarRevSettings()
	settings.Clock = world.clock
	writer := NewSidecarRevStore(world.cluster, testRevSign, settings)
	budget := sidecartest.NewFakeWriteBudget(nil)
	writer.SetWriteBudget(budget, nil)
	prices := evsservice.DefaultWriteBudgetSettings().PriceTable
	price := func(operation evsservice.WriteOperationKind, key string) int64 {
		info, err := world.cluster.Head(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		priced, err := prices.Price(evsservice.WriteOperation{Kind: operation, ObjectByteCount: info.ByteCount, Count: 1})
		if err != nil {
			t.Fatal(err)
		}
		return priced
	}
	revocation := newTestRevocation(world.clock.Now())
	key := revocation.key()

	lower := *revocation
	lower.NotBefore = revocation.NotBefore.Add(-time.Minute)
	createdCount := testApiBudgetRevWriteCount("created")
	if err := writer.Write(ctx, &lower); err != nil {
		t.Fatal(err)
	}
	if count := testApiBudgetRevWriteCount("created"); count != createdCount+1 {
		t.Fatalf("the first write counted %v creates, want 1", count-createdCount)
	}
	createPrice := price(evsservice.WriteOperationCreate, key)
	if spent := world.spent(sidecar.WriteP0KindRevocation, budget); spent != createPrice {
		t.Errorf("the create spent %d, and §11.13's create row prices it at %d", spent, createPrice)
	}

	raisedCount := testApiBudgetRevWriteCount("raised")
	if err := writer.Write(ctx, revocation); err != nil {
		t.Fatal(err)
	}
	if count := testApiBudgetRevWriteCount("raised"); count != raisedCount+1 {
		t.Fatalf("the second write counted %v raises, want 1", count-raisedCount)
	}
	raisePrice := price(evsservice.WriteOperationOverwrite, key)
	if raisePrice <= price(evsservice.WriteOperationCreate, key) {
		t.Fatalf("the overwrite row prices the raise at %d, no more than the create row", raisePrice)
	}
	if spent := world.spent(sidecar.WriteP0KindRevocation, budget); spent != createPrice+raisePrice {
		t.Errorf("the create and the raise spent %d, and §11.13's rows price them at %d and %d", spent, createPrice, raisePrice)
	}
	if count := world.cluster.putCount(key); count != 2 {
		t.Errorf("%d PUTs, want the create's and the raise's", count)
	}
}

// A hold's Rewrite of its standing object, a renewal, is an If-Match PUT and spends the overwrite row, and the pass's
// put-back of an object gone from a live row's key is a create and spends the create row.
func TestAbuseHoldRewriteSpendsTheOverwriteRow(t *testing.T) {
	world := newTestApiBudgetWorld(t)
	ctx := t.Context()
	store, budget := world.holdStore()
	record := testApiBudgetHoldRecord(world.clock.Now())
	key := sidecar.HoldObjectKey(record.NetworkId, record.CaseId)

	if _, err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	spent := world.objectPrice(key, evsservice.WriteOperationCreate)
	if got := world.spent(sidecar.WriteP0KindLifecycle, budget); got != spent {
		t.Errorf("the create spent %d, and its row prices it at %d", got, spent)
	}

	// the renewal §2703(f)(2) allows, keeping place_time
	renewed := *record
	renewed.ExpireTime = LegalHoldRenewedDeadline(record.PlaceTime)
	if err := store.Rewrite(ctx, &renewed); err != nil {
		t.Fatal(err)
	}
	spent += world.objectPrice(key, evsservice.WriteOperationOverwrite)
	if got := world.spent(sidecar.WriteP0KindLifecycle, budget); got != spent {
		t.Errorf("the create and the renewal spent %d, and their rows price them at %d", got, spent)
	}

	// the object gone, as a site loss leaves a live row's: the pass puts it back
	if err := world.cluster.Delete(world.fixtureContext(sidecar.WriteP0KindLifecycle), key); err != nil {
		t.Fatal(err)
	}
	if err := store.Rewrite(ctx, &renewed); err != nil {
		t.Fatal(err)
	}
	spent += world.objectPrice(key, evsservice.WriteOperationCreate)
	if got := world.spent(sidecar.WriteP0KindLifecycle, budget); got != spent {
		t.Errorf("the create, the renewal and the put-back spent %d, and their rows price them at %d", got, spent)
	}
	if count := world.cluster.putCount(key); count != 3 {
		t.Errorf("%d PUTs, want the create's, the renewal's and the put-back's", count)
	}
}

// A hold's Delete carries P0, so a budgeted cluster takes it, and spends §11.13's delete row once; a writer with no
// budget is refused there before anything is sent; and a Delete of a key already gone, which the store would answer
// as it answers one that deleted, sends no DELETE and spends nothing.
func TestAbuseHoldDeleteSpendsTheDeleteRow(t *testing.T) {
	world := newTestApiBudgetWorld(t)
	ctx := t.Context()
	deletes := newTestApiBudgetDeleteCluster(world.cluster)
	store := NewAbuseHoldStore(deletes)
	budget := sidecartest.NewFakeWriteBudget(nil)
	store.SetWriteBudget(budget, testApiBudgetPrices())
	record := testApiBudgetHoldRecord(world.clock.Now())
	key := sidecar.HoldObjectKey(record.NetworkId, record.CaseId)
	networkId, caseId := server.Id(record.NetworkId), server.Id(record.CaseId)

	if _, err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	createPrice := world.objectPrice(key, evsservice.WriteOperationCreate)

	unbudgeted := NewAbuseHoldStore(world.cluster)
	if err := unbudgeted.Delete(ctx, networkId, caseId); !errors.Is(err, evsservice.ErrWriteUnbudgeted) {
		t.Errorf("a Delete with no budget on a budgeted cluster answered %v", err)
	}
	if !world.exists(key) {
		t.Fatal("a refused Delete deleted the object")
	}

	if err := store.Delete(ctx, networkId, caseId); err != nil {
		t.Fatal(err)
	}
	if world.exists(key) {
		t.Error("the object stands after its Delete")
	}
	deletePrice := testApiBudgetPrice(evsservice.WriteOperationDelete, 0)
	if spent := world.spent(sidecar.WriteP0KindLifecycle, budget); spent != createPrice+deletePrice {
		t.Errorf("the create and the Delete spent %d, and their rows price them at %d and %d", spent, createPrice, deletePrice)
	}

	if err := store.Delete(ctx, networkId, caseId); err != nil {
		t.Errorf("a Delete of a key already gone: %v", err)
	}
	if count := deletes.deleteCount(key); count != 1 {
		t.Errorf("%d DELETEs, want the one that deleted", count)
	}
	if spent := world.spent(sidecar.WriteP0KindLifecycle, budget); spent != createPrice+deletePrice {
		t.Errorf("a Delete of a key already gone spent %d", spent-createPrice-deletePrice)
	}
}

// A hold's Delete whose outcome was unknown reads its key: gone, it landed, its answer lost, and answers nil; the
// object it meant standing, it never landed. Either way it and its retry spend one delete row, and one whose read
// failed too is spent as issued, so its retry, which finds the key gone or sends the DELETE again, spends nothing
// more.
func TestAbuseHoldDeleteWithAnUnknownOutcomeSpendsOnce(t *testing.T) {
	type testCase struct {
		name   string
		result sidecartest.FaultResult
		// whether the read after the DELETE fails too
		readFails bool
		// whether the first Delete answers nil, its read finding the key gone
		firstDeleted bool
		// the DELETEs sent across the Delete and its retry
		deleteCount int
	}
	testCases := []testCase{
		{name: "a lost answer", result: sidecartest.ApplyThenDrop, firstDeleted: true, deleteCount: 1},
		{name: "a dead request", result: sidecartest.FailBeforeApply, deleteCount: 2},
		{name: "a lost answer and a failed read", result: sidecartest.ApplyThenDrop, readFails: true, deleteCount: 1},
		{name: "a dead request and a failed read", result: sidecartest.FailBeforeApply, readFails: true, deleteCount: 2},
	}
	for _, testCase := range testCases {
		world := newTestApiBudgetWorld(t)
		ctx := t.Context()
		deletes := newTestApiBudgetDeleteCluster(world.cluster)
		store := NewAbuseHoldStore(deletes)
		budget := sidecartest.NewFakeWriteBudget(nil)
		store.SetWriteBudget(budget, testApiBudgetPrices())
		record := testApiBudgetHoldRecord(world.clock.Now())
		key := sidecar.HoldObjectKey(record.NetworkId, record.CaseId)
		networkId, caseId := server.Id(record.NetworkId), server.Id(record.CaseId)
		if _, err := store.Create(ctx, record); err != nil {
			t.Fatal(err)
		}
		createPrice := world.objectPrice(key, evsservice.WriteOperationCreate)

		world.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpDelete, KeyPrefix: key, Result: testCase.result})
		if testCase.readFails {
			// the Delete's read before its DELETE passes, and the one after it fails
			world.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpGet, KeyPrefix: key, Nth: 2, Result: sidecartest.FailBeforeApply})
		}
		err := store.Delete(ctx, networkId, caseId)
		world.checkFaultsFired(testCase.name)
		if testCase.firstDeleted {
			if err != nil {
				t.Errorf("%s: a Delete whose read found the key gone answered %v", testCase.name, err)
			}
		} else {
			var unknown *sidecar.BlobUnknownError
			if !errors.As(err, &unknown) {
				t.Errorf("%s: a Delete whose outcome stayed unknown answered %v", testCase.name, err)
			}
		}

		if err := store.Delete(ctx, networkId, caseId); err != nil {
			t.Errorf("%s: the retry: %v", testCase.name, err)
			continue
		}
		if world.exists(key) {
			t.Errorf("%s: the object stands after the retry", testCase.name)
		}
		if count := deletes.deleteCount(key); count != testCase.deleteCount {
			t.Errorf("%s: %d DELETEs, want %d", testCase.name, count, testCase.deleteCount)
		}
		deletePrice := testApiBudgetPrice(evsservice.WriteOperationDelete, 0)
		if spent := world.spent(sidecar.WriteP0KindLifecycle, budget); spent != createPrice+deletePrice {
			t.Errorf("%s: the create, the Delete and its retry spent %d, and the create and one delete price at %d", testCase.name, spent, createPrice+deletePrice)
		}
	}
}

// rev/'s expiry sweep spends each DELETE at the delete row, as P0 of kind revocation, once its object's last write is
// DeleteAfter old, and nothing before; a DELETE whose answer was lost after it landed, which the HEAD after it shows
// gone, counts as deleted and spends once.
func TestSidecarRevSweepSpendsTheDeleteRow(t *testing.T) {
	world := newTestApiBudgetWorld(t)
	ctx := t.Context()
	writer, _ := world.revStore()
	settings := DefaultSidecarRevSettings()
	settings.Clock = world.clock
	sweeper := NewSidecarRevStore(world.cluster, nil, settings)
	budget := sidecartest.NewFakeWriteBudget(nil)
	sweeper.SetWriteBudget(budget, testApiBudgetPrices())
	deletePrice := testApiBudgetPrice(evsservice.WriteOperationDelete, 0)

	first := newTestRevocation(world.clock.Now())
	if err := writer.Write(ctx, first); err != nil {
		t.Fatal(err)
	}
	if deletedCount, err := sweeper.Sweep(ctx); err != nil || deletedCount != 0 {
		t.Fatalf("a sweep before DeleteAfter deleted %d: %v", deletedCount, err)
	}
	if spent := world.spent(sidecar.WriteP0KindRevocation, budget); spent != 0 {
		t.Errorf("a sweep that deleted nothing spent %d", spent)
	}
	world.clock.Advance(settings.DeleteAfter)
	if deletedCount, err := sweeper.Sweep(ctx); err != nil || deletedCount != 1 {
		t.Fatalf("the sweep deleted %d: %v", deletedCount, err)
	}
	if world.exists(first.key()) {
		t.Error("the expired object stands after the sweep")
	}
	if spent := world.spent(sidecar.WriteP0KindRevocation, budget); spent != deletePrice {
		t.Errorf("the sweep's one DELETE spent %d, and the delete row prices it at %d", spent, deletePrice)
	}

	second := newTestRevocation(world.clock.Now())
	if err := writer.Write(ctx, second); err != nil {
		t.Fatal(err)
	}
	world.clock.Advance(settings.DeleteAfter)
	world.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpDelete, KeyPrefix: second.key(), Result: sidecartest.ApplyThenDrop})
	if deletedCount, err := sweeper.Sweep(ctx); err != nil || deletedCount != 1 {
		t.Fatalf("the sweep whose DELETE's answer was lost deleted %d: %v", deletedCount, err)
	}
	world.checkFaultsFired("the lost answer")
	if spent := world.spent(sidecar.WriteP0KindRevocation, budget); spent != 2*deletePrice {
		t.Errorf("the sweeps' two DELETEs spent %d, and the delete row prices them at %d", spent, 2*deletePrice)
	}
}

// The authorization log's retention spends each DELETE at the delete row, as P0 of kind log: entries below the
// retention point, entry 0 kept.
func TestConfigLogRetentionSpendsTheDeleteRow(t *testing.T) {
	world := newTestApiBudgetWorld(t)
	ctx := t.Context()
	networkId := sidecar.NewId()
	fixtureCtx := world.fixtureContext(sidecar.WriteP0KindLog)
	for n := uint64(0); n <= 4; n += 1 {
		if _, err := world.cluster.PutIfNoneMatch(fixtureCtx, sidecar.ConfigEntryKey(networkId, n), []byte("entry"), nil); err != nil {
			t.Fatal(err)
		}
	}
	spend := newApiWriteSpend(world.cluster, sidecar.WriteP0KindLog, 0)
	budget := sidecartest.NewFakeWriteBudget(nil)
	spend.setBudget(budget, testApiBudgetPrices())

	result, err := retainConfigLogBelow(ctx, spend, networkId, 3, nil, world.clock.Now(), DefaultConfigLogRetentionSettings())
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedCount != 2 {
		t.Fatalf("retention deleted %d entries, want 1 and 2", result.DeletedCount)
	}
	for n, want := range []bool{true, false, false, true, true} {
		if exists := world.exists(sidecar.ConfigEntryKey(networkId, uint64(n))); exists != want {
			t.Errorf("entry %d stands %t, want %t", n, exists, want)
		}
	}
	if spent, want := world.spent(sidecar.WriteP0KindLog, budget), 2*testApiBudgetPrice(evsservice.WriteOperationDelete, 0); spent != want {
		t.Errorf("retention's two DELETEs spent %d, and the delete row prices them at %d", spent, want)
	}
}

// A write kept unsettled is spent at its own row, and the write of the key that settles it spends only what its own
// row prices past that: the dearer of the two rows in all, never less than the write that took effect, which by the
// settle rule is at most one of the two.
func TestApiWriteSpendSettlesAcrossRowsAtTheDearer(t *testing.T) {
	type testCase struct {
		name string
		// the kept write's row and the settling write's
		kept     evsservice.WriteOperationKind
		settling evsservice.WriteOperationKind
	}
	testCases := []testCase{
		{name: "a create settled by an overwrite", kept: evsservice.WriteOperationCreate, settling: evsservice.WriteOperationOverwrite},
		{name: "an overwrite settled by a create", kept: evsservice.WriteOperationOverwrite, settling: evsservice.WriteOperationCreate},
		{name: "a delete settled by an overwrite", kept: evsservice.WriteOperationDelete, settling: evsservice.WriteOperationOverwrite},
		{name: "an overwrite settled by a delete", kept: evsservice.WriteOperationOverwrite, settling: evsservice.WriteOperationDelete},
		{name: "a create settled by a create", kept: evsservice.WriteOperationCreate, settling: evsservice.WriteOperationCreate},
	}
	body := []byte("an object of the key")
	write := func(operation evsservice.WriteOperationKind) (*apiWrite, int64) {
		if operation == evsservice.WriteOperationDelete {
			return newApiDelete("etag"), testApiBudgetPrice(operation, 0)
		}
		return newApiWrite(operation, body, "", func(found bool, body []byte, etag string) bool { return !found }), testApiBudgetPrice(operation, int64(len(body)))
	}
	for _, testCase := range testCases {
		world := newTestApiBudgetWorld(t)
		ctx := t.Context()
		spend := newApiWriteSpend(world.cluster, sidecar.WriteP0KindLifecycle, 1)
		budget := sidecartest.NewFakeWriteBudget(nil)
		spend.setBudget(budget, testApiBudgetPrices())
		key := "hold/" + testCase.name

		kept, keptPrice := write(testCase.kept)
		spend.spendUnsettled(ctx, key, kept)
		if spent := world.spent(sidecar.WriteP0KindLifecycle, budget); spent != keptPrice {
			t.Errorf("%s: the unsettled write spent %d, and its row prices it at %d", testCase.name, spent, keptPrice)
		}
		settling, settlingPrice := write(testCase.settling)
		spend.spendWritten(ctx, key, settling)
		if spent, want := world.spent(sidecar.WriteP0KindLifecycle, budget), max(keptPrice, settlingPrice); spent != want {
			t.Errorf("%s: the two writes spent %d, want the dearer row, %d", testCase.name, spent, want)
		}
		// settled: a later write of the key spends its own row in full
		spend.spendWritten(ctx, key, settling)
		if spent, want := world.spent(sidecar.WriteP0KindLifecycle, budget), max(keptPrice, settlingPrice)+settlingPrice; spent != want {
			t.Errorf("%s: a write after the settling one brought the spend to %d, want %d", testCase.name, spent, want)
		}
	}
}
