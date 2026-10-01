// Ready-registration controls use genuine signed wires and local immutable
// stores. Only scheduling boundaries are observed; no hook supplies authority.
package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Each synchronous member reports after releasing its complete-operation slot.
type stClientKeyRegistrationPublicationResultTest struct {
	index int
	err   error
}

// A finite completed-authority fixture exercises only publication ownership.
// The separate actual controller root below performs the real authority/Sql work.
type stClientKeyRegistrationPublicationQueueTest struct {
	fixture  *stClientKeyPublicationFixtureTest
	parent   *stClientKeyRegistrationCohorts
	members  []*stClientKeyRegistrationReservation
	ctx      context.Context
	cancel   context.CancelFunc
	admitted chan struct{}
	release  chan struct{}
	open     sync.Once
	results  chan stClientKeyRegistrationPublicationResultTest
	workers  sync.WaitGroup
}

// Publication membership is fixed before the real worker takes its first lock.
func newStClientKeyRegistrationPublicationQueueTest(t *testing.T, clients int) *stClientKeyRegistrationPublicationQueueTest {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	owner := &stClientKeyRegistrationPublicationQueueTest{
		fixture: newStClientKeyPublicationFixtureTest(t, clients),
		parent:  newStClientKeyRegistrationCohorts(0), ctx: ctx, cancel: cancel,
		admitted: make(chan struct{}, clients), release: make(chan struct{}),
		results: make(chan stClientKeyRegistrationPublicationResultTest, clients),
	}
	owner.parent.slots = clients
	cohort := &stClientKeyRegistrationCohort{parent: owner.parent, finished: true}
	for index := 0; index < clients; index++ {
		owner.members = append(owner.members, &stClientKeyRegistrationReservation{cohort: cohort})
	}
	owner.parent.beforePublicationForTest = func(context.Context) {
		select {
		case <-owner.ctx.Done():
		case <-owner.release:
		}
	}
	owner.parent.afterPublicationAdmissionForTest = func() { owner.admitted <- struct{}{} }
	t.Cleanup(func() {
		cancel()
		owner.open.Do(func() { close(owner.release) })
		owner.workers.Wait()
	})
	return owner
}

// The caller may provide independently owned deadlines, stores and wire faults.
func (self *stClientKeyRegistrationPublicationQueueTest) start(contexts []context.Context, stores []server.BlobStore, records []model.StClientKeyHistoryRecord) {
	self.workers.Add(len(self.members))
	for index, member := range self.members {
		go func() {
			defer self.workers.Done()
			err := member.publish(contexts[index], stores[index], records[index].EvidenceBytes, records[index].EvidenceHash)
			member.release()
			self.results <- stClientKeyRegistrationPublicationResultTest{index: index, err: err}
		}()
	}
}

// Separate byte owners let the test mutate a caller after actual admission.
func (self *stClientKeyRegistrationPublicationQueueTest) inputs() ([]context.Context, []server.BlobStore, []model.StClientKeyHistoryRecord) {
	var contexts []context.Context
	var stores []server.BlobStore
	var records []model.StClientKeyHistoryRecord
	for _, history := range self.fixture.histories {
		contexts = append(contexts, self.ctx)
		stores = append(stores, self.fixture.store)
		record := history[0]
		record.EvidenceBytes = bytes.Clone(record.EvidenceBytes)
		records = append(records, record)
	}
	return contexts, stores, records
}

// A premature result is a deterministic failure, not an elapsed-time assertion.
func (self *stClientKeyRegistrationPublicationQueueTest) awaitReady(t testing.TB) {
	t.Helper()
	for range self.members {
		select {
		case <-self.admitted:
		case result := <-self.results:
			t.Fatalf("registration bypassed its ready publication owner: member=%d error=%v", result.index, result.err)
		case <-self.ctx.Done():
			t.Fatal(self.ctx.Err())
		}
	}
}

// Results are indexed independently of scheduler order and every worker is joined.
func (self *stClientKeyRegistrationPublicationQueueTest) finish(t testing.TB) []error {
	t.Helper()
	result := make([]error, len(self.members))
	seen := make([]bool, len(self.members))
	for range self.members {
		select {
		case next := <-self.results:
			if next.index < 0 || next.index >= len(result) || seen[next.index] {
				t.Fatal("duplicate or foreign publication result", next.index)
			}
			seen[next.index], result[next.index] = true, next.err
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	self.workers.Wait()
	stClientKeyRegistrationAssertPublicationIdle(t, self.parent)
	return result
}

// No success or refusal may retain a slot, pending wire or active worker.
func stClientKeyRegistrationAssertPublicationIdle(t testing.TB, parent *stClientKeyRegistrationCohorts) {
	t.Helper()
	parent.stateLock.Lock()
	defer parent.stateLock.Unlock()
	if parent.slots != 0 || len(parent.publications) != 0 || parent.publicationRunning {
		t.Fatal("publication retained a complete-member slot or queue owner", parent.slots, len(parent.publications), parent.publicationRunning)
	}
}

// Context values identify callers, not authority or accepted publication results.
type stClientKeyRegistrationPublicationContextKeyTest struct{}

// Sixty-five already-ready clients require two real quota owners. Both routes
// still create/read/close on each fresh write and exact collided retry.
func TestStClientKeyRegistrationCohortPublicationUsesBoundedReadyWindows(t *testing.T) {
	owner := newStClientKeyRegistrationPublicationQueueTest(t, stClientKeyRegistrationPublicationBatch+1)
	contexts, stores, records := owner.inputs()
	var deadlines []time.Time
	for index := range contexts {
		member, cancel := context.WithTimeout(context.WithValue(owner.ctx, stClientKeyRegistrationPublicationContextKeyTest{}, index), stCallTimeout)
		t.Cleanup(cancel)
		contexts[index] = member
		deadline, _ := member.Deadline()
		deadlines = append(deadlines, deadline)
	}
	foreignStore := server.NewLocalBlobStore(t.TempDir(), "synthetic-foreign-window")
	foreign, err := server.BeginLocalBlobWriteBatch(owner.ctx, []server.BlobStore{foreignStore}, 1)
	if err != nil || foreign == nil {
		t.Fatal("foreign comparison owner", err)
	}
	t.Cleanup(func() { _ = foreign.Close() })
	var windows []*server.LocalBlobWriteBatch
	var counts []int
	var current *server.LocalBlobWriteBatch
	var observedErr error
	owner.parent.afterPublicationWindowForTest = func(batch *server.LocalBlobWriteBatch, count int) {
		current = batch
		windows, counts = append(windows, batch), append(counts, count)
	}
	owner.parent.afterPublicationForTest = func() {
		contexts := owner.fixture.store.contexts
		for _, ctx := range contexts[len(contexts)-2:] {
			index, ok := ctx.Value(stClientKeyRegistrationPublicationContextKeyTest{}).(int)
			deadline, bounded := ctx.Deadline()
			if !ok || index < 0 || index >= len(deadlines) || !bounded || deadline != deadlines[index] {
				observedErr = errors.New("actual publication lost its member values or deadline")
				continue
			}
			if _, err := current.ContextFor(ctx); err != nil {
				observedErr = errors.Join(observedErr, err)
			}
			if _, err := foreign.ContextFor(ctx); err == nil {
				observedErr = errors.New("actual write context bypassed the live quota window")
			}
		}
	}
	owner.start(contexts, stores, records)
	owner.awaitReady(t)
	// Actual admission already cloned this caller-owned wire. Its later mutation
	// must not alter either signed route or be silently repaired in the caller.
	records[0].EvidenceBytes[0] ^= 1
	mutated := bytes.Clone(records[0].EvidenceBytes)
	owner.open.Do(func() { close(owner.release) })
	for index, err := range owner.finish(t) {
		if err != nil {
			t.Fatal("ready member", index, err)
		}
	}
	if observedErr != nil || len(windows) != 2 || windows[0] == windows[1] || counts[0] != 64 || counts[1] != 1 {
		t.Fatal("actual ready windows changed ownership or bounds", counts, observedErr)
	}
	if !bytes.Equal(records[0].EvidenceBytes, mutated) {
		t.Fatal("publisher changed borrowed caller bytes")
	}
	if len(owner.fixture.store.contexts) != 2*len(records) || owner.fixture.store.gets != 2*len(records) || owner.fixture.store.closes != owner.fixture.store.gets {
		t.Fatal("finite publication omitted an immutable attempt or exact readback")
	}
	objects, err := owner.fixture.store.List(t.Context(), "")
	if err != nil || len(objects) != 2*len(records) {
		t.Fatal("fresh immutable object census", len(objects), err)
	}
	// A later completed authority reservation must acquire new live quota owners,
	// and collisions still perform both independent exact readbacks.
	owner.parent.slots = 1
	retry := &stClientKeyRegistrationReservation{cohort: &stClientKeyRegistrationCohort{parent: owner.parent, finished: true}}
	record := owner.fixture.histories[0][0]
	err = retry.publish(contexts[0], owner.fixture.store, record.EvidenceBytes, record.EvidenceHash)
	retry.release()
	stClientKeyRegistrationAssertPublicationIdle(t, owner.parent)
	if err != nil || len(windows) != 3 || windows[2] == windows[0] || windows[2] == windows[1] || counts[2] != 1 || observedErr != nil {
		t.Fatal("collision reused a completed quota owner", err, counts, observedErr)
	}
	if len(owner.fixture.store.contexts) != 2*(len(records)+1) || owner.fixture.store.gets != 2*(len(records)+1) || owner.fixture.store.closes != owner.fixture.store.gets {
		t.Fatal("collision omitted actual immutable attempts or readbacks")
	}
}

// A canceled ready member and a typed publication failure each leave a live
// sibling's signature, actual writes and final accounting independently valid.
func TestStClientKeyRegistrationCohortPublicationPreservesLiveSibling(t *testing.T) {
	for _, fault := range []string{"cancel", "retained-hash"} {
		owner := newStClientKeyRegistrationPublicationQueueTest(t, 2)
		contexts, stores, records := owner.inputs()
		memberCtx, cancel := context.WithCancel(owner.ctx)
		contexts[0] = memberCtx
		t.Cleanup(cancel)
		if fault == "retained-hash" {
			records[0].EvidenceHash = "sha256:" + strings.Repeat("00", 32)
		}
		owner.start(contexts, stores, records)
		owner.awaitReady(t)
		if fault == "cancel" {
			cancel()
		}
		owner.open.Do(func() { close(owner.release) })
		results := owner.finish(t)
		if results[0] == nil || results[1] != nil || fault == "cancel" && !errors.Is(results[0], context.Canceled) {
			t.Fatal("one member changed its sibling's publication result", fault, results)
		}
		if len(owner.fixture.store.contexts) != 2 || owner.fixture.store.gets != 2 || owner.fixture.store.closes != 2 {
			t.Fatal("failed member skipped or duplicated its sibling's original routes", fault)
		}
		objects, err := owner.fixture.store.List(t.Context(), "")
		if err != nil || len(objects) != 2 {
			t.Fatal("sibling object census", len(objects), err)
		}
	}
}

// A wrapper observes only source declaration, without replacing the backing
// store or the actual operating-system root lock.
type stClientKeyRegistrationPublicationSourceTest struct {
	server.BlobStore
	source   server.BlobStore
	declared chan struct{}
}

// The exact declared root is the same root used by promoted Put/Get methods.
func (self *stClientKeyRegistrationPublicationSourceTest) LocalBlobBatchSource() server.BlobStore {
	self.declared <- struct{}{}
	return self.source
}

// All members can leave while another real owner still holds the root. The
// queue must join cancellation without waiting for that independent lease to end.
func TestStClientKeyRegistrationCohortPublicationAllCancellationReleasesWaitingWindow(t *testing.T) {
	owner := newStClientKeyRegistrationPublicationQueueTest(t, 2)
	holder, err := server.BeginLocalBlobWriteBatch(t.Context(), []server.BlobStore{owner.fixture.store}, 1)
	if err != nil || holder == nil {
		t.Fatal("independent root owner", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	contexts, stores, records := owner.inputs()
	var cancels []context.CancelFunc
	declared := make(chan struct{}, len(contexts))
	wrapper := &stClientKeyRegistrationPublicationSourceTest{BlobStore: owner.fixture.store, source: owner.fixture.store.BlobStore, declared: declared}
	for index := range contexts {
		ctx, cancel := context.WithCancel(owner.ctx)
		contexts[index] = ctx
		cancels = append(cancels, cancel)
		stores[index] = wrapper
	}
	owner.start(contexts, stores, records)
	owner.awaitReady(t)
	owner.open.Do(func() { close(owner.release) })
	for range contexts {
		select {
		case <-declared:
		case result := <-owner.results:
			t.Fatal("held root was bypassed", result.err)
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	for _, cancel := range cancels {
		cancel()
	}
	for _, err := range owner.finish(t) {
		if !errors.Is(err, context.Canceled) {
			t.Fatal("all-canceled root acquisition did not join", err)
		}
	}
	if len(owner.fixture.store.contexts) != 0 || owner.fixture.store.gets != 0 {
		t.Fatal("canceled window wrote through an independently held root")
	}
	if err := holder.Close(); err != nil {
		t.Fatal("member cancellation reached the unrelated holder", err)
	}
}

// Final quota reconciliation is a group result, even after every member's
// genuine immutable writes and exact signed readbacks succeeded.
func TestStClientKeyRegistrationCohortPublicationSharesFinalCensusFailure(t *testing.T) {
	owner := newStClientKeyRegistrationPublicationQueueTest(t, 2)
	contexts, stores, records := owner.inputs()
	published := 0
	var injectionErr error
	owner.parent.afterPublicationForTest = func() {
		published++
		if published == len(records) {
			injectionErr = os.WriteFile(filepath.Join(owner.fixture.root, "synthetic-unaccounted.json"), []byte("x"), 0o600)
		}
	}
	owner.start(contexts, stores, records)
	owner.awaitReady(t)
	owner.open.Do(func() { close(owner.release) })
	results := owner.finish(t)
	if injectionErr != nil || published != 2 {
		t.Fatal("final census fault did not follow all real publications", published, injectionErr)
	}
	for _, err := range results {
		if err == nil || !strings.Contains(err.Error(), "unaccounted bytes") {
			t.Fatal("a member escaped final reconciliation", err)
		}
	}
	if len(owner.fixture.store.contexts) != 4 || owner.fixture.store.gets != 4 || owner.fixture.store.closes != 4 {
		t.Fatal("final failure omitted a real signed route or readback")
	}
}

// A panic cannot strand pending/active byte owners or reservation slots. An
// active failed window does not erase a later already-ready independent window.
func TestStClientKeyRegistrationCohortPublicationJoinsPanickedWorkers(t *testing.T) {
	for _, fault := range []string{"pending", "active"} {
		owner := newStClientKeyRegistrationPublicationQueueTest(t, 65)
		contexts, stores, records := owner.inputs()
		before := owner.parent.beforePublicationForTest
		windows := 0
		if fault == "pending" {
			owner.parent.beforePublicationForTest = func(ctx context.Context) {
				before(ctx)
				panic("synthetic pending publication panic")
			}
		} else {
			owner.parent.afterPublicationWindowForTest = func(*server.LocalBlobWriteBatch, int) {
				windows++
				if windows == 1 {
					panic("synthetic active publication panic")
				}
			}
		}
		owner.start(contexts, stores, records)
		owner.awaitReady(t)
		owner.open.Do(func() { close(owner.release) })
		results := owner.finish(t)
		successes, failures := 0, 0
		for _, err := range results {
			if err == nil {
				successes++
			} else if strings.Contains(err.Error(), "panicked") {
				failures++
			} else {
				t.Fatal("panic ownership changed", err)
			}
		}
		if fault == "pending" && (successes != 0 || failures != 65 || len(owner.fixture.store.contexts) != 0) ||
			fault == "active" && (successes != 1 || failures != 64 || windows != 2 || len(owner.fixture.store.contexts) != 2) {
			t.Fatal("panic stranded members or erased a following independent window", fault, successes, failures, windows)
		}
		owner.parent.beforePublicationForTest, owner.parent.afterPublicationWindowForTest = nil, nil
		owner.parent.afterPublicationAdmissionForTest = nil
		owner.parent.slots = 1
		retry := &stClientKeyRegistrationReservation{cohort: &stClientKeyRegistrationCohort{parent: owner.parent, finished: true}}
		record := owner.fixture.histories[0][0]
		err := retry.publish(owner.ctx, owner.fixture.store, record.EvidenceBytes, record.EvidenceHash)
		retry.release()
		if err != nil {
			t.Fatal("panicked worker retained storage or queue ownership", fault, err)
		}
		stClientKeyRegistrationAssertPublicationIdle(t, owner.parent)
	}
}

// This adapter intentionally does not advertise a local batch source.
type stClientKeyRegistrationPublicationUnknownTest struct{ server.BlobStore }

// Completed reservations remain mandatory, and unknown backends retain the
// original typed publication/readback path without joining the local queue.
func TestStClientKeyRegistrationCohortPublicationRejectsUnownedAndKeepsUnknownPath(t *testing.T) {
	fixture := newStClientKeyPublicationFixtureTest(t, 1)
	record := fixture.histories[0][0]
	for _, fault := range []string{"waiting", "released", "claimed", "unfinished", "authority-error", "no-slot"} {
		parent := newStClientKeyRegistrationCohorts(0)
		parent.slots = 1
		cohort := &stClientKeyRegistrationCohort{parent: parent, finished: true}
		member := &stClientKeyRegistrationReservation{cohort: cohort}
		switch fault {
		case "waiting":
			member.waiting = true
		case "released":
			member.released = true
		case "claimed":
			member.publicationClaimed = true
		case "unfinished":
			cohort.finished = false
		case "authority-error":
			cohort.err = errors.New("synthetic authority refusal")
		case "no-slot":
			parent.slots = 0
		}
		slots := parent.slots
		if err := member.publish(t.Context(), fixture.store, record.EvidenceBytes, record.EvidenceHash); err == nil {
			t.Fatal("publication accepted an unowned reservation", fault)
		}
		if parent.slots != slots || len(parent.publications) != 0 || parent.publicationRunning || len(fixture.store.contexts) != 0 {
			t.Fatal("refusal changed queue ownership or reached storage", fault)
		}
	}
	parent := newStClientKeyRegistrationCohorts(0)
	parent.slots = 1
	parent.beforePublicationForTest = func(context.Context) { panic("unknown backend entered local worker") }
	parent.afterPublicationAdmissionForTest = func() { panic("unknown backend queued local work") }
	member := &stClientKeyRegistrationReservation{cohort: &stClientKeyRegistrationCohort{parent: parent, finished: true}}
	err := member.publish(t.Context(), &stClientKeyRegistrationPublicationUnknownTest{BlobStore: fixture.store}, record.EvidenceBytes, record.EvidenceHash)
	member.release()
	if err != nil || len(fixture.store.contexts) != 2 || fixture.store.gets != 2 || fixture.store.closes != 2 {
		t.Fatal("unknown backend lost original signed publication", err)
	}
	stClientKeyRegistrationAssertPublicationIdle(t, parent)
}

// This is the actual authenticated controller, not a copied publication call.
// Both Sql rows are ready before any quota window; final reconciliation must
// fail both responses even after all four real immutable routes were read back.
func TestStClientKeyRegistrationCohortPublicationActualControllerOwnsFinalFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, credential, cfg, cohorts := newStClientKeyRegistrationFixture(tb, 1, 0)
		endpoint := stClientKeyRegistrationConnectEndpoint(tb)
		ctx, cancel := context.WithCancel(tb.Context())
		var workers sync.WaitGroup
		tb.Cleanup(func() { cancel(); workers.Wait() })
		clientId, deviceId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(tb.Context(), credential.NetworkId, deviceId, clientId, "synthetic-publication-sibling", "test")
		sibling := credential.Client(deviceId, clientId)
		authorityReady, seal := make(chan struct{}, 2), make(chan struct{})
		publicationReady, release := make(chan struct{}, 2), make(chan struct{})
		cohorts.afterAdmissionForTest = func() { authorityReady <- struct{}{} }
		cohorts.beforeSealForTest = func(ctx context.Context) {
			select {
			case <-ctx.Done():
			case <-seal:
			}
		}
		cohorts.afterPublicationAdmissionForTest = func() { publicationReady <- struct{}{} }
		cohorts.beforePublicationForTest = func(ctx context.Context) {
			select {
			case <-ctx.Done():
			case <-release:
			}
		}
		blobConfig, present := server.LoadBlobStoreConfig()
		if !present || blobConfig == nil {
			tb.Fatal("actual local blob configuration is absent")
		}
		published, windows := 0, 0
		var injectionErr error
		cohorts.afterPublicationWindowForTest = func(_ *server.LocalBlobWriteBatch, ready int) {
			windows++
			if ready != 2 {
				injectionErr = fmt.Errorf("actual ready window has %d members, want 2", ready)
			}
		}
		cohorts.afterPublicationForTest = func() {
			published++
			if published == 2 {
				injectionErr = errors.Join(injectionErr, os.WriteFile(filepath.Join(blobConfig.LocalPath, "synthetic-unaccounted.json"), []byte("x"), 0o600))
			}
		}
		key := bytes.Repeat([]byte{9}, 32)
		results := make(chan error, 2)
		workers.Add(2)
		for _, token := range []string{credential.Sign(), sibling.Sign()} {
			go func() {
				defer workers.Done()
				results <- stClientKeyRegistrationRequest(ctx, endpoint.URL, token, key)
			}()
		}
		stClientKeyRegistrationAwait(tb, authorityReady, results)
		stClientKeyRegistrationAwait(tb, authorityReady, results)
		close(seal)
		stClientKeyRegistrationAwait(tb, publicationReady, results)
		stClientKeyRegistrationAwait(tb, publicationReady, results)
		// Admission follows committed Sql and precedes the first filesystem
		// quota lock; this query is not performed under any publication window.
		for _, id := range []server.Id{*credential.ClientId, *sibling.ClientId} {
			history, err := model.LoadStClientKeyHistory(tb.Context(), fixture.snapshot().domain, id, model.MaxStClientKeyHistoryRegistrations, model.MaxStClientKeyHistoryBytes)
			if err != nil || len(history) != 1 {
				tb.Fatal("publication queued before its actual signed Sql result", len(history), err)
			}
		}
		store, ok := server.LoadBlobStore()
		if !ok {
			tb.Fatal("actual local publication store disappeared")
		}
		objects, err := store.List(tb.Context(), "")
		if err != nil || len(objects) != 0 {
			tb.Fatal("publication acquired storage before the ready-only drain", len(objects), err)
		}
		close(release)
		for index := 0; index < 2; index++ {
			if err := <-results; err == nil || !strings.Contains(err.Error(), "unaccounted bytes") {
				tb.Fatal("actual controller bypassed ready publication or final reconciliation", err)
			}
		}
		workers.Wait()
		if injectionErr != nil || published != 2 || windows != 1 {
			tb.Fatal("actual final failure did not exercise one complete publication window", published, windows, injectionErr)
		}
		stClientKeyRegistrationAssertPublicationIdle(tb, cohorts)
		stClientKeyRegistrationAssertStored(tb, fixture, cfg, credential, key, fixture.snapshot().boundary)
		stClientKeyRegistrationAssertStored(tb, fixture, cfg, sibling, key, fixture.snapshot().boundary)
		// Default, unobserved retry acquires fresh authority and a fresh quota
		// owner while retaining both exact original signed immutable winners.
		cohorts.beforeSealForTest, cohorts.afterAdmissionForTest = nil, nil
		cohorts.beforePublicationForTest, cohorts.afterPublicationAdmissionForTest = nil, nil
		cohorts.afterPublicationWindowForTest, cohorts.afterPublicationForTest = nil, nil
		if err := stClientKeyRegistrationRequest(ctx, endpoint.URL, credential.Sign(), key); err != nil {
			tb.Fatal("actual final failure retained an unusable publication owner", err)
		}
		stClientKeyRegistrationAssertPublicationIdle(tb, cohorts)
		stClientKeyRegistrationAssertStored(tb, fixture, cfg, credential, key, fixture.snapshot().boundary)
	})
}
