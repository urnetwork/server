package controller

// api's own hold/ writer (installApiLegalHolds; QD-1399), which the renew_hold staff kind writes through: in a process
// that serves api's routes and runs no taskworker, as production's api does, a renewal of a hold the worker placed
// rewrites its object and spends api's write budget; api's writes and the worker's to one key, each conditioned on the
// object its own writer read, never replace one another unread; and in all's process, which installs both writers,
// the worker's is the process's in either order and removing one leaves the other (QD-1420). The first three run on
// MG5's tables, in a build with sidecar_pending, over FakeBlobs first clusters; the last needs neither.

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Turns holds on and has placement and renewal read clock. Answers what puts both back.
func pushTestHoldsOn(clock *sidecartest.FakeClock) (pop func()) {
	popConfig := server.Testing_PushAbuseConfig("holds_enabled: true\nprotected_names: {roots: [root.example], platform: [platform.example]}\n")
	restoreNow := Testing_SetLegalHoldNow(clock.Now)
	return func() {
		restoreNow()
		popConfig()
	}
}

// Places a preserve_2703f hold on the network's case at the hold clock's now as taskworker's process places one: what
// its LegalHoldPlacementTask runs (placeLegalHoldLocally), through store, its own hold/ writer, with its soft-state
// source over softState, both installed for the placement alone, so the process goes on with neither, as api's.
func placeTestHoldAsTheWorker(t testing.TB, ctx context.Context, store *model.AbuseHoldStore, softState *sidecar.ServiceHost, networkId server.Id, caseId server.Id) *model.LegalHold {
	t.Helper()
	removeStore := model.InstallWorkerAbuseHoldStore(store)
	model.SetAbuseHoldSoftStateSource(model.NewAbuseHoldSoftStateSource(softState, nil))
	result, err := placeLegalHoldLocally(ctx, &model.LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f})
	model.SetAbuseHoldSoftStateSource(nil)
	removeStore()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Inserted || !result.Hold.PlaceTime.Equal(testHoldDay0) || !result.Hold.ExpireTime.Equal(testHoldDay(90)) {
		t.Fatalf("the worker placed %+v", result.Hold)
	}
	return result.Hold
}

// The hold's object as reader reads it, failing the test when there is none.
func readTestHoldObject(t testing.TB, ctx context.Context, reader *model.AbuseHoldStore, networkId server.Id, caseId server.Id) *model.AbuseHoldObject {
	t.Helper()
	object, found, err := reader.Read(ctx, networkId, caseId)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the hold has no object")
	}
	return object
}

// Whether object holds the hold placed at day 0 with the expiry expireTime.
func testHoldObjectExpires(object *model.AbuseHoldObject, expireTime time.Time) bool {
	return object.Record != nil && object.Record.PlaceTime.Equal(testHoldDay0) && object.Record.ExpireTime.Equal(expireTime)
}

// renew_hold's body for the network's case.
func testRenewHoldBody(networkId server.Id, caseId server.Id) []byte {
	return []byte(`{"kind":"renew_hold","case_id":"` + caseId.String() + `","network_id":"` + networkId.String() + `"}`)
}

// A process that serves api's routes and runs no taskworker, as production's api does, renews through the renew_hold
// staff kind a hold the worker placed: InstallApiRoutes installs api's own hold/ writer on its first cluster
// (installApiLegalHolds), whose If-Match rewrite of the object spends api's write budget at §11.13's overwrite row, as
// P0 of kind lifecycle, and nothing of the worker's. The cluster evsservice.BudgetedBlobs wraps refuses any write no
// budget passed as P0. With no writer, as api's process ran before QD-1399, the same renewal answers
// ErrLegalHoldStoreUnavailable and writes nothing.
func TestApiRoutesRenewALegalHoldThroughApisOwnWriter(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		holdClock := sidecartest.NewFakeClock(testHoldDay0)
		defer pushTestHoldsOn(holdClock)()
		// the host's clock stands still, so api's budget reports what it spent as it closes
		fakeHost := sidecartest.NewFakeHost(ctx, nil, nil, nil, nil)
		blobs := evsservice.NewBudgetedBlobs(fakeHost.Blobs)
		first, err := blobs.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		reader := model.NewAbuseHoldStore(first)
		prices := evsservice.DefaultWriteBudgetSettings().PriceTable
		price := func(operation evsservice.WriteOperationKind, body []byte) int64 {
			t.Helper()
			priced, err := prices.Price(evsservice.WriteOperation{Kind: operation, ObjectByteCount: int64(len(body)), Count: 1})
			if err != nil {
				t.Fatal(err)
			}
			return priced
		}

		// the worker places the hold through its own writer, on its own budget
		workerBudget := sidecartest.NewFakeWriteBudget(nil)
		workerStore := model.NewAbuseHoldStore(first)
		workerStore.SetWriteBudget(workerBudget, nil)
		networkId, caseId := newTestHoldCase(t, ctx)
		placeTestHoldAsTheWorker(t, ctx, workerStore, &sidecar.ServiceHost{Blobs: blobs, Clock: fakeHost.Clock, Redis: fakeHost.Redis}, networkId, caseId)
		placedObject := readTestHoldObject(t, ctx, reader, networkId, caseId)
		workerSpent := workerBudget.P0ByteCount(first.ClusterId(), sidecar.WriteP0KindLifecycle)
		if want := price(evsservice.WriteOperationCreate, placedObject.Body); workerSpent != want {
			t.Fatalf("the worker's placement spent %d, and §11.13's create row prices it at %d", workerSpent, want)
		}

		legal := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleLegal}}
		holdClock.AdvanceTo(testHoldDay(80))

		// no hold/ writer in the process: refused before anything is written
		if _, err := AbuseStaffAction(ctx, legal, testRenewHoldBody(networkId, caseId)); !errors.Is(err, model.ErrLegalHoldStoreUnavailable) {
			t.Fatalf("a renewal with no hold/ writer: %v", err)
		}
		if hold := model.GetLegalHold(ctx, networkId, caseId); hold == nil || hold.RenewTime != nil || !hold.ExpireTime.Equal(testHoldDay(90)) {
			t.Fatalf("a refused renewal changed the row: %+v", hold)
		}
		if object := readTestHoldObject(t, ctx, reader, networkId, caseId); !bytes.Equal(object.Body, placedObject.Body) {
			t.Fatalf("a refused renewal wrote the object: %s", object.Body)
		}

		// api's routes as api's main installs them, no taskworker beside them, on the same first cluster
		undoInstalls := InstallApiRoutes(ctx, fakeHost.ServiceHost(), &ApiRoutesInstallDeps{
			Clusters: func(ctx context.Context) (sidecar.BlobStore, func(), error) {
				return blobs, func() {}, nil
			},
		})
		undone := false
		defer func() {
			if !undone {
				undoInstalls()
			}
		}()
		if _, err := model.CurrentAbuseHoldStore(); err != nil {
			t.Fatalf("api's routes installed no hold/ writer: %v", err)
		}
		renewed, err := AbuseStaffAction(ctx, legal, testRenewHoldBody(networkId, caseId))
		if err != nil {
			t.Fatalf("api's renewal: %v", err)
		}
		view := renewed.Result.(*AbuseStaffHoldResult).Hold
		if view.PlaceTimeMs != testHoldDay0.UnixMilli() || view.RenewTimeMs != testHoldDay(80).UnixMilli() || view.ExpireTimeMs != testHoldDay(180).UnixMilli() {
			t.Fatalf("renewed %+v", view)
		}
		renewedObject := readTestHoldObject(t, ctx, reader, networkId, caseId)
		if !testHoldObjectExpires(renewedObject, testHoldDay(180)) {
			t.Fatalf("the renewed object: %s", renewedObject.Body)
		}
		renewedOwed := 0
		for _, owed := range testOwedLegalHoldRecords(t, ctx) {
			if owed.Event == LegalHoldEventRenewed && owed.CaseId == caseId && owed.ExpireTimeMs == testHoldDay(180).UnixMilli() {
				renewedOwed += 1
			}
		}
		if renewedOwed != 1 {
			t.Fatalf("the legal-request mailbox is owed %d renewed records", renewedOwed)
		}

		// api's budget reports what it spent as it closes, with the routes' installs
		undone = true
		undoInstalls()
		if _, err := model.CurrentAbuseHoldStore(); !errors.Is(err, model.ErrLegalHoldStoreUnavailable) {
			t.Fatalf("api's hold/ writer outlived its routes: %v", err)
		}
		// evsservice's issued counter (writeBudgetIssuedMetricName), by cluster, class and kind
		apiSpent := int64(fakeHost.Metrics.Value("urnetwork_evs_budget_issued_bytes_total", first.ClusterId(), "p0", string(sidecar.WriteP0KindLifecycle)))
		if want := price(evsservice.WriteOperationOverwrite, renewedObject.Body); apiSpent != want {
			t.Errorf("api's budget spent %d on the renewal, and §11.13's overwrite row prices it at %d", apiSpent, want)
		}
		if got := workerBudget.P0ByteCount(first.ClusterId(), sidecar.WriteP0KindLifecycle); got != workerSpent {
			t.Errorf("the renewal spent the worker's budget: %d after the placement's %d", got, workerSpent)
		}
		if violations := workerBudget.Violations(); len(violations) != 0 {
			t.Errorf("the worker's budget's violations: %v", violations)
		}
	})
}

// A first cluster that runs a hook once, after a read of key has returned and before its caller sends anything more:
// where another process's write lands between this writer's read of a hold and its write.
type testHoldHookedCluster struct {
	sidecar.BlobCluster

	stateLock sync.Mutex
	key       string
	afterGet  func()
}

// Runs afterGet once, after the next read of key returns.
func (self *testHoldHookedCluster) arm(key string, afterGet func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.key = key
	self.afterGet = afterGet
}

func (self *testHoldHookedCluster) Get(ctx context.Context, key string) (*sidecar.BlobObject, error) {
	object, err := self.BlobCluster.Get(ctx, key)
	afterGet := func() func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if key != self.key {
			return nil
		}
		afterGet := self.afterGet
		self.afterGet = nil
		return afterGet
	}()
	if afterGet != nil {
		afterGet()
	}
	return object, err
}

// What becomes of a hold's object so that the pass writes over it: lost, or holding an earlier expiry than its row's.
type testHoldObjectDamage struct {
	name string
	// whether the pass puts the object back (RestoredCount) rather than rewrites it (RewrittenCount)
	restored bool
	apply    func(t testing.TB, ctx context.Context, first sidecar.BlobCluster, hold *model.LegalHold)
}

var testHoldObjectDamages = []*testHoldObjectDamage{
	{
		name:     "a lost object",
		restored: true,
		apply: func(t testing.TB, ctx context.Context, first sidecar.BlobCluster, hold *model.LegalHold) {
			t.Helper()
			if err := first.Delete(ctx, sidecar.HoldObjectKey(sidecar.Id(hold.NetworkId), sidecar.Id(hold.CaseId))); err != nil {
				t.Fatal(err)
			}
		},
	},
	{
		name:     "an object holding an earlier expiry",
		restored: false,
		apply: func(t testing.TB, ctx context.Context, first sidecar.BlobCluster, hold *model.LegalHold) {
			t.Helper()
			record := hold.Record()
			record.ExpireTime = testHoldDay(60)
			body, err := sidecar.EncodeHoldRecord(record)
			if err != nil {
				t.Fatal(err)
			}
			key := sidecar.HoldObjectKey(record.NetworkId, record.CaseId)
			info, err := first.Head(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := first.PutIfMatch(ctx, key, info.Etag, body, nil); err != nil {
				t.Fatal(err)
			}
		},
	},
}

// Two processes' hold/ writers on one first cluster, as production runs them, for one damage of one hold: the worker's,
// which placement and AbuseHoldPass write through, and api's, installed as api's process installs it
// (installApiLegalHolds), which the renew_hold staff kind writes through. Each writes through a view of the cluster
// that can run a hook after its read of the hold (testHoldHookedCluster). A hold is placed at day 0 of the clock, as
// the worker places one; holds are on, and placement and renewal read the clock, until close.
type testHoldWriters struct {
	clock       *sidecartest.FakeClock
	first       sidecar.BlobCluster
	tasks       *sidecartest.FakeTasks
	workerFirst *testHoldHookedCluster
	workerStore *model.AbuseHoldStore
	apiFirst    *testHoldHookedCluster
	apiBudget   *sidecartest.FakeWriteBudget
	legal       *AbuseStaff
	networkId   server.Id
	caseId      server.Id
	key         string
	placed      *model.LegalHold
	closes      []func()
}

// blobs is the first cluster's store, shared by the cases of a test so the holds earlier cases left keep their
// objects, which the pass then leaves as they are.
func newTestHoldWriters(t testing.TB, ctx context.Context, blobs *sidecartest.FakeBlobs) *testHoldWriters {
	t.Helper()
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	clock := sidecartest.NewFakeClock(testHoldDay0)
	self := &testHoldWriters{
		clock:       clock,
		first:       first,
		tasks:       sidecartest.NewFakeTasks(clock, blobs.Faults(), nil),
		workerFirst: &testHoldHookedCluster{BlobCluster: first},
		apiFirst:    &testHoldHookedCluster{BlobCluster: first},
		apiBudget:   sidecartest.NewFakeWriteBudget(nil),
		legal:       &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleLegal}},
	}
	self.closes = append(self.closes, pushTestHoldsOn(clock))
	self.workerStore = model.NewAbuseHoldStore(self.workerFirst)
	self.workerStore.SetWriteBudget(sidecartest.NewFakeWriteBudget(nil), nil)
	self.networkId, self.caseId = newTestHoldCase(t, ctx)
	self.key = sidecar.HoldObjectKey(sidecar.Id(self.networkId), sidecar.Id(self.caseId))
	softState := &sidecar.ServiceHost{
		Blobs: blobs, Clock: clock, Redis: sidecartest.NewFakeRedis(ctx, clock, blobs.Faults(), nil, nil),
	}
	self.placed = placeTestHoldAsTheWorker(t, ctx, self.workerStore, softState, self.networkId, self.caseId)
	self.closes = append(self.closes, installApiLegalHolds(&sidecar.ServiceHost{Budget: self.apiBudget}, self.apiFirst))
	return self
}

// Removes api's writer and puts holds' switch and clock back.
func (self *testHoldWriters) close() {
	for _, closeOne := range self.closes {
		defer closeOne()
	}
}

// The worker's pass at the clock's now.
func (self *testHoldWriters) pass(ctx context.Context) *AbuseHoldPassResult {
	return RunAbuseHoldPass(ctx, &AbuseHoldPassDeps{Store: self.workerStore, Tasks: self.tasks, Now: self.clock.Now})
}

// The hold's object, read on the shared cluster.
func (self *testHoldWriters) object(t testing.TB, ctx context.Context) *model.AbuseHoldObject {
	t.Helper()
	return readTestHoldObject(t, ctx, model.NewAbuseHoldStore(self.first), self.networkId, self.caseId)
}

// The worker's pass decides on its read of a hold's object, and api's renewal lands before the pass writes: the pass's
// write, If-Match the object that read found or If-None-Match for none (model.AbuseHoldStore.RewriteUnless), is
// refused, the pass counts the hold failed, and the renewal's object stands; the next pass finds the object as the
// row says and writes nothing. The pass used to decide on one read and write on another, its own Rewrite's, which
// found the renewal and replaced it with the row's older expiry. For each damage that has the pass write.
func TestHoldPassLeavesARenewalThatLandedAfterItsRead(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(testHoldDay0), nil, nil, nil)
		for _, damage := range testHoldObjectDamages {
			func() {
				writers := newTestHoldWriters(t, ctx, blobs)
				defer writers.close()
				damage.apply(t, ctx, writers.first, writers.placed)
				writers.clock.AdvanceTo(testHoldDay(80))

				var renewed *AbuseStaffResult
				var renewErr error
				writers.workerFirst.arm(writers.key, func() {
					renewed, renewErr = AbuseStaffAction(ctx, writers.legal, testRenewHoldBody(writers.networkId, writers.caseId))
				})
				result := writers.pass(ctx)
				if renewed == nil || renewErr != nil {
					t.Fatalf("%s: api's renewal after the pass's read: %v", damage.name, renewErr)
				}
				if result.FailedCount != 1 || result.RestoredCount != 0 || result.RewrittenCount != 0 {
					t.Fatalf("%s: the pass wrote over the renewal, or did not leave the hold to the next pass: %+v", damage.name, result)
				}
				if object := writers.object(t, ctx); !testHoldObjectExpires(object, testHoldDay(180)) {
					t.Fatalf("%s: the renewal's object did not stand: %s", damage.name, object.Body)
				}
				if hold := model.GetLegalHold(ctx, writers.networkId, writers.caseId); hold == nil || hold.RenewTime == nil || !hold.ExpireTime.Equal(testHoldDay(180)) {
					t.Fatalf("%s: the renewal's row: %+v", damage.name, hold)
				}

				if result := writers.pass(ctx); result.FailedCount != 0 || result.RestoredCount != 0 || result.RewrittenCount != 0 {
					t.Fatalf("%s: the next pass: %+v", damage.name, result)
				}
				if object := writers.object(t, ctx); !testHoldObjectExpires(object, testHoldDay(180)) {
					t.Fatalf("%s: the next pass changed the renewal's object: %s", damage.name, object.Body)
				}
			}()
		}
	})
}

// api's renewal reads a hold's object, and the worker's pass puts the object back before api writes: the renewal's
// write, If-Match the object it read or If-None-Match for none, is refused, so the renewal answers the 412 with its row
// unrenewed, nothing spent and nothing owed, and the pass's object stands; a retry renews over it, spending api's
// budget at §11.13's overwrite row. For each damage that has the pass write.
func TestApisRenewalLeavesAPutBackThatLandedAfterItsRead(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(testHoldDay0), nil, nil, nil)
		for _, damage := range testHoldObjectDamages {
			func() {
				writers := newTestHoldWriters(t, ctx, blobs)
				defer writers.close()
				damage.apply(t, ctx, writers.first, writers.placed)
				writers.clock.AdvanceTo(testHoldDay(80))

				var result *AbuseHoldPassResult
				writers.apiFirst.arm(writers.key, func() {
					result = writers.pass(ctx)
				})
				_, err := AbuseStaffAction(ctx, writers.legal, testRenewHoldBody(writers.networkId, writers.caseId))
				if result == nil {
					t.Fatalf("%s: the pass did not run after api's read", damage.name)
				}
				if result.FailedCount != 0 || result.RestoredCount != btoi(damage.restored) || result.RewrittenCount != btoi(!damage.restored) {
					t.Fatalf("%s: the pass after api's read: %+v", damage.name, result)
				}
				if !errors.Is(err, sidecar.ErrBlobPrecondition) {
					t.Fatalf("%s: api's renewal over the pass's object: %v", damage.name, err)
				}
				if object := writers.object(t, ctx); !testHoldObjectExpires(object, testHoldDay(90)) {
					t.Fatalf("%s: the pass's object did not stand: %s", damage.name, object.Body)
				}
				if hold := model.GetLegalHold(ctx, writers.networkId, writers.caseId); hold == nil || hold.RenewTime != nil || !hold.ExpireTime.Equal(testHoldDay(90)) {
					t.Fatalf("%s: a refused renewal changed the row: %+v", damage.name, hold)
				}
				for _, owed := range testOwedLegalHoldRecords(t, ctx) {
					if owed.Event == LegalHoldEventRenewed && owed.CaseId == writers.caseId {
						t.Fatalf("%s: a refused renewal owes its record", damage.name)
					}
				}
				if spent := writers.apiBudget.P0ByteCount(writers.first.ClusterId(), sidecar.WriteP0KindLifecycle); spent != 0 {
					t.Fatalf("%s: api's refused write spent %d", damage.name, spent)
				}

				// the retry reads the pass's object and renews over it
				renewed, err := AbuseStaffAction(ctx, writers.legal, testRenewHoldBody(writers.networkId, writers.caseId))
				if err != nil || renewed.Result.(*AbuseStaffHoldResult).Hold.ExpireTimeMs != testHoldDay(180).UnixMilli() {
					t.Fatalf("%s: the retried renewal: %+v, %v", damage.name, renewed, err)
				}
				object := writers.object(t, ctx)
				if !testHoldObjectExpires(object, testHoldDay(180)) {
					t.Fatalf("%s: the retried renewal's object: %s", damage.name, object.Body)
				}
				want, err := evsservice.DefaultWriteBudgetSettings().PriceTable.Price(evsservice.WriteOperation{
					Kind: evsservice.WriteOperationOverwrite, ObjectByteCount: int64(len(object.Body)), Count: 1,
				})
				if err != nil {
					t.Fatal(err)
				}
				if spent := writers.apiBudget.P0ByteCount(writers.first.ClusterId(), sidecar.WriteP0KindLifecycle); spent != want {
					t.Fatalf("%s: api's renewal spent %d, and §11.13's overwrite row prices it at %d", damage.name, spent, want)
				}
			}()
		}
	})
}

// 1 for true, 0 for false.
func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}

// all's process runs api and taskworker, each installing its own hold/ writer (installApiLegalHolds through
// model.InstallAbuseHoldStore, and taskworker's installEvsJobWriters through model.InstallWorkerAbuseHoldStore): the
// worker's is the process's whenever it is installed, in either order, since the pass deletes through it and api's
// MinIO user deletes nothing (QD-1420), api's only where the worker's is not, and removing either leaves the other, so
// neither part's end takes away the writer the other installed.
func TestTheWorkersHoldWriterStaysCurrentBesideApis(t *testing.T) {
	blobs := sidecartest.NewFakeBlobs(nil, nil, nil, nil)
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	model.SetAbuseHoldStore(nil)
	defer model.SetAbuseHoldStore(nil)
	host := &sidecar.ServiceHost{Budget: sidecartest.NewFakeWriteBudget(nil)}
	current := func() *model.AbuseHoldStore {
		store, _ := model.CurrentAbuseHoldStore()
		return store
	}

	// api's first, as all's main installs api's parts before taskworker starts
	removeApi := installApiLegalHolds(host, first)
	apiStore := current()
	workerStore := model.NewAbuseHoldStore(first)
	removeWorker := model.InstallWorkerAbuseHoldStore(workerStore)
	if apiStore == nil || current() != workerStore {
		t.Fatal("the worker's writer, installed later, is not the process's")
	}
	removeApi()
	if current() != workerStore {
		t.Fatal("removing api's writer took away the worker's")
	}
	removeWorker()
	if current() != nil {
		t.Fatal("a writer stands with both removed")
	}

	// the worker's first, as when api's mount comes after taskworker's start: it stays the process's
	removeWorker = model.InstallWorkerAbuseHoldStore(workerStore)
	removeApi = installApiLegalHolds(host, first)
	if current() != workerStore {
		t.Fatal("api's writer, installed later, took the worker's place")
	}
	removeWorker()
	apiStore = current()
	if apiStore == nil || apiStore == workerStore {
		t.Fatal("removing the worker's writer took away api's")
	}
	removeApi()
	if current() != nil {
		t.Fatal("a writer stands with both removed")
	}
}
