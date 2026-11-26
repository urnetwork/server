package model

// The rev/ and hold/ stores and the authorization log's sequencer of a process that runs api and taskworker together,
// as server/cli/all's does (§19.4's writers that share a process; owner decisions 785, 788, amended by QD-1420), each
// service on its own block's MinIO user (testClusterUser): every rev/ write goes through api's signing store and the
// sweep through taskworker's sweep store, the worker's hold/ writer is current whenever it is installed and api's only
// where it is not, and api's sequencer and the worker's each keep a slot of their own, each rule holding whichever is
// installed first and whichever is removed first. So every DELETE runs under the worker's user, as api's deletes
// nothing. Before the places, each was one process-wide pointer, so the worker's install replaced api's: every rev/
// write failed as unsigned, the issuer's tick handoffs handed off again, and the worker's removal cleared what api
// installed. Before QD-1420 the sweep read api's store, and the later of two hold/ writers stood in for both, so the
// sweep, and the hold pass after api's mount came later than taskworker's start, sent their DELETEs as api's user,
// which refuses them. No Postgres.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// What the store answers a request its user's policy does not allow.
var errTestAccessDenied = errors.New("AccessDenied: access denied")

// The first metadata cluster as one block's MinIO user reaches it, over the one bucket every block's user shares
// (§19.4's own credentials; xops' templates/file-clusters/policy-api.json.j2 and policy-taskworker.json.j2): api's user
// reads and writes proj/, projrows/, rev/ and hold/, lists under them and reads sentinel/_, and deletes nothing, while
// taskworker's reaches every object. The store answers a request the policy refuses with AccessDenied, which this
// reports as server's FileCluster reads it: a read or a LIST failed, a PUT or a DELETE an unknown outcome
// (fileClusterPutError, fileClusterDeleteError). Counts the PUTs and DELETEs the user sent.
type testClusterUser struct {
	sidecar.BlobCluster
	// the prefixes the user reads, writes and lists under; nil for every key
	prefixes []string
	// whether the user deletes
	deletes bool

	stateLock sync.Mutex
	// PUTs the policy let through, by key prefix up to its first slash
	prefixPutCounts map[string]int
	// DELETEs the policy let through, and those it refused
	deleteCount        int
	refusedDeleteCount int
}

// api's user on cluster (§19.4's api row).
func newTestApiClusterUser(cluster sidecar.BlobCluster) *testClusterUser {
	return &testClusterUser{
		BlobCluster:     cluster,
		prefixes:        []string{"proj/", "projrows/", "rev/", "hold/"},
		prefixPutCounts: map[string]int{},
	}
}

// taskworker's user on cluster (§19.4's taskworker row).
func newTestWorkerClusterUser(cluster sidecar.BlobCluster) *testClusterUser {
	return &testClusterUser{BlobCluster: cluster, deletes: true, prefixPutCounts: map[string]int{}}
}

// Whether the policy lets the user write key, or list under it.
func (self *testClusterUser) reaches(key string) bool {
	if self.prefixes == nil {
		return true
	}
	for _, prefix := range self.prefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// Whether the policy lets the user read key.
func (self *testClusterUser) reads(key string) bool {
	return self.reaches(key) || key == "sentinel/_"
}

// A PUT the policy let through, counted, or its refusal.
func (self *testClusterUser) put(key string) error {
	if !self.reaches(key) {
		return &sidecar.BlobUnknownError{Err: errTestAccessDenied}
	}
	prefix, _, _ := strings.Cut(key, "/")
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.prefixPutCounts[prefix+"/"] += 1
	}()
	return nil
}

func (self *testClusterUser) PutIfNoneMatch(ctx context.Context, key string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	if err := self.put(key); err != nil {
		return "", err
	}
	return self.BlobCluster.PutIfNoneMatch(ctx, key, body, options)
}

func (self *testClusterUser) PutIfMatch(ctx context.Context, key string, etag string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	if err := self.put(key); err != nil {
		return "", err
	}
	return self.BlobCluster.PutIfMatch(ctx, key, etag, body, options)
}

func (self *testClusterUser) PutStream(ctx context.Context, key string, body io.Reader, byteCount int64, checksumSha256 []byte) (string, error) {
	if err := self.put(key); err != nil {
		return "", err
	}
	return self.BlobCluster.PutStream(ctx, key, body, byteCount, checksumSha256)
}

func (self *testClusterUser) Get(ctx context.Context, key string) (*sidecar.BlobObject, error) {
	if !self.reads(key) {
		return nil, fmt.Errorf("blob read failed: %w", errTestAccessDenied)
	}
	return self.BlobCluster.Get(ctx, key)
}

func (self *testClusterUser) GetRange(ctx context.Context, key string, offset int64, byteCount int64) (*sidecar.BlobObject, error) {
	if !self.reads(key) {
		return nil, fmt.Errorf("blob read failed: %w", errTestAccessDenied)
	}
	return self.BlobCluster.GetRange(ctx, key, offset, byteCount)
}

func (self *testClusterUser) Head(ctx context.Context, key string) (*sidecar.BlobInfo, error) {
	if !self.reads(key) {
		return nil, fmt.Errorf("blob read failed: %w", errTestAccessDenied)
	}
	return self.BlobCluster.Head(ctx, key)
}

func (self *testClusterUser) List(ctx context.Context, keyPrefix string, startAfter string, limit int) ([]*sidecar.BlobInfo, bool, error) {
	if !self.reaches(keyPrefix) {
		return nil, false, fmt.Errorf("blob list failed: %w", errTestAccessDenied)
	}
	return self.BlobCluster.List(ctx, keyPrefix, startAfter, limit)
}

func (self *testClusterUser) Delete(ctx context.Context, key string) error {
	refused := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.deletes {
			self.refusedDeleteCount += 1
			return true
		}
		self.deleteCount += 1
		return false
	}()
	if refused {
		return &sidecar.BlobUnknownError{Err: errTestAccessDenied}
	}
	return self.BlobCluster.Delete(ctx, key)
}

// The DELETEs the user sent: those the policy let through, and those it refused.
func (self *testClusterUser) deleteCounts() (deleted int, refused int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.deleteCount, self.refusedDeleteCount
}

// The PUTs the policy let through under prefix, such as "hold/".
func (self *testClusterUser) putCount(prefix string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.prefixPutCounts[prefix]
}

// The stores rev/'s writes and its sweep go through now, nil for none.
func testCurrentRevStores() (write *SidecarRevStore, sweep *SidecarRevStore) {
	write, _ = currentSidecarRevWriteStore()
	sweep, _ = currentSidecarRevSweepStore()
	return write, sweep
}

// Every write goes through api's signing store and the sweep through the worker's sweep store, in every order of
// install and removal, each falling back to the other only while its own is not installed, and each removal takes out
// its own store alone.
func TestRevStoresOfOneProcessKeepTheirRoles(t *testing.T) {
	SetSidecarRevStore(nil)
	defer SetSidecarRevStore(nil)
	s := newTestRevStore(t)
	issuer := s.store
	sweep := NewSidecarRevStore(s.cluster, nil, nil)
	requireStores := func(what string, wantWrite *SidecarRevStore, wantSweep *SidecarRevStore) {
		t.Helper()
		if write, swept := testCurrentRevStores(); write != wantWrite || swept != wantSweep {
			t.Fatalf("%s: writes go through %p and the sweep through %p, want %p and %p", what, write, swept, wantWrite, wantSweep)
		}
	}

	// api's first, as all's main installs api's parts before taskworker starts, and removed last, as all's one undo
	// runs once every service has drained
	removeIssuer := InstallSidecarRevStore(issuer)
	requireStores("api's alone, as in api's process", issuer, issuer)
	removeSweep := InstallSidecarRevStore(sweep)
	requireStores("the worker's installed beside api's", issuer, sweep)
	removeSweep()
	requireStores("the worker's removed", issuer, issuer)
	removeIssuer()
	requireStores("both removed", nil, nil)

	// api's removed first: the worker's sweeps on, and a write reaches it, which refuses it as unsigned
	removeIssuer = InstallSidecarRevStore(issuer)
	removeSweep = InstallSidecarRevStore(sweep)
	removeIssuer()
	requireStores("api's removed", sweep, sweep)
	removeSweep()
	requireStores("both removed", nil, nil)

	// the worker's first, as when api's mount comes after taskworker's start
	removeSweep = InstallSidecarRevStore(sweep)
	requireStores("the worker's alone, as in taskworker's process", sweep, sweep)
	removeIssuer = InstallSidecarRevStore(issuer)
	requireStores("api's installed later", issuer, sweep)
	removeSweep()
	requireStores("the worker's removed", issuer, issuer)
	removeIssuer()
	requireStores("both removed", nil, nil)

	// a removal of a store another install has since replaced takes out nothing
	other := NewSidecarRevStore(s.cluster, testRevSign, nil)
	removeIssuer = InstallSidecarRevStore(issuer)
	removeOther := InstallSidecarRevStore(other)
	removeIssuer()
	requireStores("a stale removal of api's", other, other)
	otherSweep := NewSidecarRevStore(s.cluster, nil, nil)
	removeSweep = InstallSidecarRevStore(sweep)
	removeOtherSweep := InstallSidecarRevStore(otherSweep)
	removeSweep()
	requireStores("a stale removal of the worker's", other, otherSweep)
	removeOther()
	removeOtherSweep()

	// SetSidecarRevStore installs one store alone, and nil removes both
	InstallSidecarRevStore(sweep)
	SetSidecarRevStore(issuer)
	requireStores("SetSidecarRevStore's alone", issuer, issuer)
	SetSidecarRevStore(nil)
	requireStores("SetSidecarRevStore(nil)", nil, nil)
}

// What budget spent on cluster as P0 of kind, failing the test on any violation of its rules.
func testProcessStoreSpent(t testing.TB, budget *sidecartest.FakeWriteBudget, cluster sidecar.BlobCluster, kind sidecar.WriteP0Kind) int64 {
	t.Helper()
	if violations := budget.Violations(); len(violations) != 0 {
		t.Errorf("the budget's violations: %v", violations)
	}
	return budget.P0ByteCount(cluster.ClusterId(), kind)
}

// Installs both of api's and the worker's, api's first when apiFirst, and answers what removes both.
func testInstallBoth(apiFirst bool, installApi func() func(), installWorker func() func()) (remove func()) {
	removes := []func(){}
	if apiFirst {
		removes = append(removes, installApi(), installWorker())
	} else {
		removes = append(removes, installWorker(), installApi())
	}
	return func() {
		for _, removeOne := range removes {
			removeOne()
		}
	}
}

// With each service's store on its own block's MinIO user over one bucket, in either order of install, a removal's
// write task lands its signed object through api's store, spending api's budget at §11.13's create row, and the sweep
// 20 minutes on deletes it through the worker's sweep store, spending the worker's at the delete row and none of api's:
// api's user is sent no DELETE. A sweep through api's store, which the sweep read before QD-1420, is refused at its
// DELETE, and the object stands.
func TestRevWritesGoThroughApisAndTheSweepThroughTheWorkers(t *testing.T) {
	SetSidecarRevStore(nil)
	defer SetSidecarRevStore(nil)
	ctx := context.Background()
	clientSession := session.NewLocalClientSession(ctx, "", nil)
	defer clientSession.Cancel()
	// a store of s's cluster as user, on its own budget, at the clock s keeps
	newStore := func(s *testRevStore, user *testClusterUser, sign func([]byte) ([]byte, error)) (*SidecarRevStore, *sidecartest.FakeWriteBudget) {
		settings := DefaultSidecarRevSettings()
		settings.Clock = s.clock
		store := NewSidecarRevStore(user, sign, settings)
		budget := sidecartest.NewFakeWriteBudget(nil)
		store.SetWriteBudget(budget, testApiBudgetPrices())
		return store, budget
	}

	for _, apiFirst := range []bool{true, false} {
		s := newTestRevStore(t)
		apiUser := newTestApiClusterUser(s.cluster)
		workerUser := newTestWorkerClusterUser(s.cluster)
		api, apiBudget := newStore(s, apiUser, testRevSign)
		sweep, sweepBudget := newStore(s, workerUser, nil)
		removeBoth := testInstallBoth(apiFirst, func() func() { return InstallSidecarRevStore(api) }, func() func() { return InstallSidecarRevStore(sweep) })

		revocation := newTestRevocation(s.clock.Now())
		if _, err := WriteSidecarRevTask(&WriteSidecarRevArgs{Revocations: []*SidecarRevocation{revocation}}, clientSession); err != nil {
			t.Fatalf("api first %t: the write task: %v", apiFirst, err)
		}
		if s.held(t, revocation) == nil {
			t.Fatalf("api first %t: the write task did not publish the signed object", apiFirst)
		}
		info, err := s.cluster.Head(ctx, revocation.key())
		if err != nil {
			t.Fatal(err)
		}
		created := testProcessStoreSpent(t, apiBudget, s.cluster, sidecar.WriteP0KindRevocation)
		if want := testApiBudgetPrice(evsservice.WriteOperationCreate, info.ByteCount); created != want || apiUser.putCount("rev/") != 1 {
			t.Fatalf("api first %t: api's create spent %d in %d PUTs, and §11.13's create row prices it at %d", apiFirst, created, apiUser.putCount("rev/"), want)
		}

		s.clock.Advance(DefaultSidecarRevSettings().DeleteAfter + time.Minute)
		deletedCount, err := SweepSidecarRevocations(ctx)
		if err != nil || deletedCount != 1 {
			t.Fatalf("api first %t: the sweep deleted %d: %v", apiFirst, deletedCount, err)
		}
		if s.held(t, revocation) != nil {
			t.Fatalf("api first %t: the sweep left the expired object", apiFirst)
		}
		if got, want := testProcessStoreSpent(t, sweepBudget, s.cluster, sidecar.WriteP0KindRevocation), testApiBudgetPrice(evsservice.WriteOperationDelete, 0); got != want {
			t.Errorf("api first %t: the sweep spent %d of the worker's budget, and §11.13's delete row prices it at %d", apiFirst, got, want)
		}
		if got := testProcessStoreSpent(t, apiBudget, s.cluster, sidecar.WriteP0KindRevocation); got != created {
			t.Errorf("api first %t: api's budget spent %d after the sweep and %d before it", apiFirst, got, created)
		}
		if deleted, refused := apiUser.deleteCounts(); deleted != 0 || refused != 0 {
			t.Errorf("api first %t: api's user was sent %d DELETEs, %d refused", apiFirst, deleted+refused, refused)
		}
		if deleted, _ := workerUser.deleteCounts(); deleted != 1 {
			t.Errorf("api first %t: the worker's user sent %d DELETEs", apiFirst, deleted)
		}
		removeBoth()
	}

	// the sweep through api's store, as the sweep read it before: api's user refuses the DELETE, and the object stands
	s := newTestRevStore(t)
	apiUser := newTestApiClusterUser(s.cluster)
	api, _ := newStore(s, apiUser, testRevSign)
	revocation := newTestRevocation(s.clock.Now())
	if err := api.Write(ctx, revocation); err != nil {
		t.Fatal(err)
	}
	s.clock.Advance(DefaultSidecarRevSettings().DeleteAfter + time.Minute)
	if deletedCount, err := api.Sweep(ctx); err == nil || deletedCount != 0 {
		t.Fatalf("a sweep as api's user deleted %d: %v", deletedCount, err)
	}
	if s.held(t, revocation) == nil {
		t.Fatal("api's user deleted the object")
	}
	if _, refused := apiUser.deleteCounts(); refused != 1 {
		t.Fatalf("api's user refused %d DELETEs", refused)
	}
}

// api's process and taskworker's each keep what they did alone: in api's, writes and the sweep both read api's
// store, and in taskworker's, the sweep deletes through the worker's while a write task finds no signer and keeps
// its revocations owed, writing nothing.
func TestRevStoreOfOneServiceKeepsItsBehavior(t *testing.T) {
	SetSidecarRevStore(nil)
	defer SetSidecarRevStore(nil)
	ctx := context.Background()
	clientSession := session.NewLocalClientSession(ctx, "", nil)
	defer clientSession.Cancel()

	// api's process
	s := newTestRevStore(t)
	removeApi := InstallSidecarRevStore(s.store)
	revocation := newTestRevocation(s.clock.Now())
	if _, err := WriteSidecarRevTask(&WriteSidecarRevArgs{Revocations: []*SidecarRevocation{revocation}}, clientSession); err != nil || s.held(t, revocation) == nil {
		t.Fatalf("api's process did not publish: %v", err)
	}
	s.clock.Advance(DefaultSidecarRevSettings().DeleteAfter + time.Minute)
	if deletedCount, err := SweepSidecarRevocations(ctx); err != nil || deletedCount != 1 {
		t.Fatalf("api's store, alone, swept %d: %v", deletedCount, err)
	}
	removeApi()

	// taskworker's process
	s = newTestRevStore(t)
	workerUser := newTestWorkerClusterUser(s.cluster)
	settings := DefaultSidecarRevSettings()
	settings.Clock = s.clock
	removeSweep := InstallSidecarRevStore(NewSidecarRevStore(workerUser, nil, settings))
	defer removeSweep()
	revocation = newTestRevocation(s.clock.Now())
	if _, err := WriteSidecarRevTask(&WriteSidecarRevArgs{Revocations: []*SidecarRevocation{revocation}}, clientSession); err == nil {
		t.Fatal("a write task in taskworker's process landed with no signer")
	}
	if s.held(t, revocation) != nil || workerUser.putCount("rev/") != 0 {
		t.Fatal("taskworker's process wrote a revocation")
	}
	// an expired object the issuer left
	issued := NewSidecarRevStore(s.cluster, testRevSign, settings)
	if err := issued.Write(ctx, revocation); err != nil {
		t.Fatal(err)
	}
	s.clock.Advance(DefaultSidecarRevSettings().DeleteAfter + time.Minute)
	if deletedCount, err := SweepSidecarRevocations(ctx); err != nil || deletedCount != 1 {
		t.Fatalf("taskworker's store swept %d: %v", deletedCount, err)
	}
	if deleted, _ := workerUser.deleteCounts(); deleted != 1 {
		t.Fatalf("the worker's user sent %d DELETEs", deleted)
	}
}

// taskworker's hold/ writer is current whenever it is installed, in either order of install, and api's only while no
// worker's is; each removal takes out its own writer alone, and SetAbuseHoldStore installs one writer alone.
func TestHoldWritersOfOneProcessKeepTheWorkersCurrent(t *testing.T) {
	SetAbuseHoldStore(nil)
	defer SetAbuseHoldStore(nil)
	blobs := sidecartest.NewFakeBlobs(nil, nil, nil, nil)
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	api := NewAbuseHoldStore(newTestApiClusterUser(first))
	worker := NewAbuseHoldStore(newTestWorkerClusterUser(first))
	requireCurrent := func(what string, want *AbuseHoldStore) {
		t.Helper()
		store, err := CurrentAbuseHoldStore()
		if store != want {
			t.Fatalf("%s: the current writer is %p, want %p", what, store, want)
		}
		if want == nil && !errors.Is(err, ErrLegalHoldStoreUnavailable) {
			t.Fatalf("%s: no writer answers %v", what, err)
		}
	}

	// api's first, as all's main installs api's parts before taskworker starts
	removeApi := InstallAbuseHoldStore(api)
	requireCurrent("api's alone, as in api's process", api)
	removeWorker := InstallWorkerAbuseHoldStore(worker)
	requireCurrent("the worker's installed later", worker)
	removeApi()
	requireCurrent("api's removed", worker)
	removeWorker()
	requireCurrent("both removed", nil)

	// the worker's first, as when api's mount comes after taskworker's start: it stays current
	removeWorker = InstallWorkerAbuseHoldStore(worker)
	requireCurrent("the worker's alone, as in taskworker's process", worker)
	removeApi = InstallAbuseHoldStore(api)
	requireCurrent("api's installed later", worker)
	removeWorker()
	requireCurrent("the worker's removed", api)
	removeApi()
	requireCurrent("both removed", nil)

	// a removal of a writer another install has since replaced takes out nothing
	otherWorker := NewAbuseHoldStore(newTestWorkerClusterUser(first))
	removeWorker = InstallWorkerAbuseHoldStore(worker)
	removeOtherWorker := InstallWorkerAbuseHoldStore(otherWorker)
	removeWorker()
	requireCurrent("a stale removal of the worker's", otherWorker)
	removeOtherWorker()
	otherApi := NewAbuseHoldStore(newTestApiClusterUser(first))
	removeApi = InstallAbuseHoldStore(api)
	removeOtherApi := InstallAbuseHoldStore(otherApi)
	removeApi()
	requireCurrent("a stale removal of api's", otherApi)
	removeOtherApi()

	// SetAbuseHoldStore installs one writer alone, and nil removes both
	InstallAbuseHoldStore(api)
	InstallWorkerAbuseHoldStore(worker)
	SetAbuseHoldStore(otherApi)
	requireCurrent("SetAbuseHoldStore's alone", otherApi)
	SetAbuseHoldStore(nil)
	requireCurrent("SetAbuseHoldStore(nil)", nil)
}

// With each service's hold/ writer on its own block's MinIO user over one bucket, in either order of install, an ended
// hold's DELETE through the current writer, the one AbuseHoldPass ends holds through, takes its object away under the
// worker's user, spending the worker's budget at §11.13's delete row and none of api's, and api's user is sent no
// DELETE. Through api's writer, which a later install made current before QD-1420, as when api's mount came after
// taskworker's start, the DELETE is refused and the object stands. A process with one writer alone, api's or the
// worker's, writes through it.
func TestHoldDeletesGoThroughTheWorkersWriterBesideApis(t *testing.T) {
	SetAbuseHoldStore(nil)
	defer SetAbuseHoldStore(nil)
	ctx := context.Background()
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(testLegalHoldDay0), nil, nil, nil)
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	apiUser := newTestApiClusterUser(first)
	workerUser := newTestWorkerClusterUser(first)
	api := NewAbuseHoldStore(apiUser)
	apiBudget := sidecartest.NewFakeWriteBudget(nil)
	api.SetWriteBudget(apiBudget, testApiBudgetPrices())
	worker := NewAbuseHoldStore(workerUser)
	workerBudget := sidecartest.NewFakeWriteBudget(nil)
	worker.SetWriteBudget(workerBudget, testApiBudgetPrices())
	spent := func(budget *sidecartest.FakeWriteBudget) int64 {
		t.Helper()
		return testProcessStoreSpent(t, budget, first, sidecar.WriteP0KindLifecycle)
	}
	// a hold's object, as the worker's placement writes one
	place := func() *sidecar.HoldRecord {
		t.Helper()
		record := testApiBudgetHoldRecord(testLegalHoldDay0)
		if _, err := worker.Create(ctx, record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	held := func(record *sidecar.HoldRecord) *AbuseHoldObject {
		t.Helper()
		object, found, err := NewAbuseHoldStore(first).Read(ctx, server.Id(record.NetworkId), server.Id(record.CaseId))
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			return nil
		}
		return object
	}
	deletePrice := testApiBudgetPrice(evsservice.WriteOperationDelete, 0)

	for _, apiFirst := range []bool{true, false} {
		removeBoth := testInstallBoth(apiFirst, func() func() { return InstallAbuseHoldStore(api) }, func() func() { return InstallWorkerAbuseHoldStore(worker) })
		record := place()
		store, err := CurrentAbuseHoldStore()
		if err != nil || store != worker {
			t.Fatalf("api first %t: the current writer is not the worker's: %v", apiFirst, err)
		}
		workerSpent, apiSpent := spent(workerBudget), spent(apiBudget)
		if err := store.Delete(ctx, server.Id(record.NetworkId), server.Id(record.CaseId)); err != nil {
			t.Fatalf("api first %t: the ended hold's DELETE: %v", apiFirst, err)
		}
		if held(record) != nil {
			t.Fatalf("api first %t: the ended hold's object stands", apiFirst)
		}
		if got := spent(workerBudget) - workerSpent; got != deletePrice {
			t.Errorf("api first %t: the DELETE spent %d of the worker's budget, and §11.13's delete row prices it at %d", apiFirst, got, deletePrice)
		}
		if got := spent(apiBudget); got != apiSpent {
			t.Errorf("api first %t: the DELETE spent %d of api's budget", apiFirst, got-apiSpent)
		}
		removeBoth()
	}
	if deleted, refused := apiUser.deleteCounts(); deleted != 0 || refused != 0 {
		t.Fatalf("api's user was sent %d DELETEs, %d refused", deleted+refused, refused)
	}

	// api's writer, current before QD-1420 after a later install: its user refuses the DELETE, and the object stands
	record := place()
	if err := api.Delete(ctx, server.Id(record.NetworkId), server.Id(record.CaseId)); err == nil {
		t.Fatal("api's user deleted an ended hold's object")
	}
	if held(record) == nil {
		t.Fatal("the object went under api's user")
	}
	if _, refused := apiUser.deleteCounts(); refused != 1 {
		t.Fatalf("api's user refused %d DELETEs", refused)
	}

	// api's process, its writer alone, renews through it, If-Match the object it read
	removeApi := InstallAbuseHoldStore(api)
	store, err := CurrentAbuseHoldStore()
	if err != nil || store != api {
		t.Fatalf("api's writer alone is not current: %v", err)
	}
	renewed := *record
	renewed.ExpireTime = LegalHoldRenewedDeadline(record.PlaceTime)
	apiSpent := spent(apiBudget)
	if err := store.Rewrite(ctx, &renewed); err != nil {
		t.Fatalf("api's renewal: %v", err)
	}
	object := held(record)
	if object == nil || object.Record == nil || !object.Record.ExpireTime.Equal(renewed.ExpireTime) {
		t.Fatalf("api's renewal did not land: %+v", object)
	}
	if got, want := spent(apiBudget)-apiSpent, testApiBudgetPrice(evsservice.WriteOperationOverwrite, int64(len(object.Body))); got != want {
		t.Errorf("api's renewal spent %d, and §11.13's overwrite row prices it at %d", got, want)
	}
	removeApi()

	// taskworker's process, its writer alone, ends the hold through it
	removeWorker := InstallWorkerAbuseHoldStore(worker)
	defer removeWorker()
	store, err = CurrentAbuseHoldStore()
	if err != nil || store != worker {
		t.Fatalf("the worker's writer alone is not current: %v", err)
	}
	if err := store.Delete(ctx, server.Id(record.NetworkId), server.Id(record.CaseId)); err != nil || held(record) != nil {
		t.Fatalf("the worker's writer alone did not end the hold: %v", err)
	}
}

// Whether err is the 503 a process with no sequencer answers.
func testConfigLogUnavailable(err error) bool {
	configLogError := &ConfigLogError{}
	return errors.As(err, &configLogError) && configLogError.Status == http.StatusServiceUnavailable
}

// The issuer slot holds api's sequencer alone and the worker slot taskworker's, which falls back to api's where the
// process holds none; each install's removal takes out its own alone.
func TestWorkerSequencerTakesASlotBesideApis(t *testing.T) {
	SetConfigLogSequencer(nil)
	defer SetConfigLogSequencer(nil)
	cluster := testConfigLogFakeCluster(t)
	api := NewConfigLogSequencer(cluster, DefaultConfigLogSettings(), nil)
	workerSettings := DefaultConfigLogSettings()
	workerSettings.QueueTicksAtIssuer = true
	worker := NewConfigLogSequencer(cluster, workerSettings, nil)
	current := func() (*ConfigLogSequencer, *ConfigLogSequencer) {
		t.Helper()
		issuer, issuerErr := CurrentConfigLogSequencer()
		if issuerErr != nil && !testConfigLogUnavailable(issuerErr) {
			t.Fatalf("api's slot answered %v", issuerErr)
		}
		forWorker, workerErr := CurrentWorkerConfigLogSequencer()
		if workerErr != nil && !testConfigLogUnavailable(workerErr) {
			t.Fatalf("the worker's slot answered %v", workerErr)
		}
		return issuer, forWorker
	}

	if issuer, forWorker := current(); issuer != nil || forWorker != nil {
		t.Fatal("a sequencer stands before any install")
	}
	// api's alone, as in api's process or all's whose vault holds api's clusters alone
	SetConfigLogSequencer(api)
	if issuer, forWorker := current(); issuer != api || forWorker != api {
		t.Fatal("the worker's duties do not run on api's sequencer where the process holds no worker's")
	}
	// the worker's beside it, as all's process installs taskworker's after api's parts
	removeWorker := InstallWorkerConfigLogSequencer(worker)
	if issuer, forWorker := current(); issuer != api || forWorker != worker {
		t.Fatal("the worker's sequencer took api's slot, or api's the worker's")
	}
	// api's removed: the worker's keeps its slot, and api's routes answer 503
	SetConfigLogSequencer(nil)
	if issuer, forWorker := current(); issuer != nil || forWorker != worker {
		t.Fatal("removing api's sequencer took away the worker's")
	}
	SetConfigLogSequencer(api)
	// a removal of a worker's sequencer another install has since replaced takes out nothing
	again := NewConfigLogSequencer(cluster, workerSettings, nil)
	removeAgain := InstallWorkerConfigLogSequencer(again)
	removeWorker()
	if issuer, forWorker := current(); issuer != api || forWorker != again {
		t.Fatal("a stale removal took away the worker's sequencer installed since")
	}
	removeAgain()
	if issuer, forWorker := current(); issuer != api || forWorker != api {
		t.Fatal("the worker's removal took away api's sequencer")
	}
}

// With taskworker's sequencer, which queues ticks at the issuer, in the worker's slot and api's in its own, the tick
// and activation tasks hand their runs to the issuer and the issuer's handoffs append through api's, never handing
// off again; here at a network whose log is empty, so the handoffs append nothing. Before, the worker's sequencer
// replaced api's, and each handoff handed off again.
func TestTickHandoffsReadTheirOwnSlots(t *testing.T) {
	SetConfigLogSequencer(nil)
	defer SetConfigLogSequencer(nil)
	cluster := testConfigLogFakeCluster(t)
	SetConfigLogSequencer(NewConfigLogSequencer(cluster, DefaultConfigLogSettings(), nil))
	workerSettings := DefaultConfigLogSettings()
	workerSettings.QueueTicksAtIssuer = true
	defer InstallWorkerConfigLogSequencer(NewConfigLogSequencer(cluster, workerSettings, nil))()
	clientSession := session.NewLocalClientSession(context.Background(), "", nil)
	defer clientSession.Cancel()
	networkId := server.NewId()

	tickArgs := &AppendConfigLogTickArgs{NetworkId: networkId}
	tick, err := AppendConfigLogTickTask(tickArgs, clientSession)
	if err != nil || !tick.PublishAtIssuer {
		t.Fatalf("the worker's tick task: %+v, %v", tick, err)
	}
	published, err := PublishConfigLogTickTask(tickArgs, clientSession)
	if err != nil || published.PublishAtIssuer || published.N != nil || published.More {
		t.Fatalf("the issuer's tick handoff: %+v, %v", published, err)
	}

	activationArgs := &AppendConfigLogActivationArgs{NetworkId: networkId, ProposalId: make([]byte, 32)}
	activation, err := AppendConfigLogActivationTask(activationArgs, clientSession)
	if err != nil || !activation.PublishAtIssuer {
		t.Fatalf("the worker's activation task: %+v, %v", activation, err)
	}
	publishedActivation, err := PublishConfigLogActivationTask(activationArgs, clientSession)
	if err != nil || publishedActivation.PublishAtIssuer || publishedActivation.N != nil || publishedActivation.RunAt != nil {
		t.Fatalf("the issuer's activation handoff: %+v, %v", publishedActivation, err)
	}

	// with no sequencer of api's, the handoffs wait rather than run on the worker's
	SetConfigLogSequencer(nil)
	if result, err := PublishConfigLogTickTask(tickArgs, clientSession); err == nil {
		t.Fatalf("a tick handoff with no api sequencer: %+v", result)
	}
	if result, err := PublishConfigLogActivationTask(activationArgs, clientSession); err == nil {
		t.Fatalf("an activation handoff with no api sequencer: %+v", result)
	}
}
