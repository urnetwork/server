//go:build sidecar_pending

package sidecarrestore

// The ops step and §19.10's deleted-network path as QD-813 settled it, on private Postgres and T2's fakes (OPS-M3-03):
// a log whose network the restored rows lack is listed with each repo shredding a week and ten minutes after the
// incident, repo/ left active and the job queued for then; before that time the job changes nothing; after it each
// active or deleted repo goes shredding{the incident} with no signer and its shred is queued under evs's own once
// key; rows that came back end the job with nothing changed; evs's own shred runs each repo to its tombstone, and
// only after the last does the job delete proj/ and projrows/; a rerun of the step or the job changes nothing more,
// and a repo the path cannot take holds its log.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A repo of the world's log, placed on the first cluster with repo/ in state as evs's create and lifecycle leave it:
// a deleted repo names a RepoDelete of its own, at stateTime, and its signer, and a shredding or shredded one a
// deletion of its own.
func (self *testLogWorld) placedRepoIn(blobs *sidecartest.FakeBlobs, state protocol.EvsRepoState, stateTime time.Time) sidecar.Id {
	self.t.Helper()
	before, _, err := self.sequencer.RestoreRepoIds(self.ctx, self.networkId)
	if err != nil {
		self.t.Fatal(err)
	}
	self.post(self.world.RepoCreate(protocol.RepoKind_REPO_KIND_GENERAL))
	after, _, err := self.sequencer.RestoreRepoIds(self.ctx, self.networkId)
	if err != nil {
		self.t.Fatal(err)
	}
	var repoId sidecar.Id
	for _, id := range after {
		if !containsId(before, id) {
			repoId = id
		}
	}
	if repoId == (sidecar.Id{}) {
		self.t.Fatal("the RepoCreate named no new repo")
	}
	first, err := blobs.FirstCluster()
	if err != nil {
		self.t.Fatal(err)
	}
	placeBytes, err := (&sidecar.EvsRepoPlace{ClusterId: first.ClusterId(), CreatorSidecarId: sidecar.NewId()}).Marshal()
	if err != nil {
		self.t.Fatal(err)
	}
	if _, err := first.PutIfNoneMatch(self.ctx, sidecar.EvsPlaceKey(repoId), placeBytes, nil); err != nil {
		self.t.Fatal(err)
	}
	object := &protocol.EvsRepoObject{
		RepoId:    repoId.Bytes(),
		Kind:      protocol.RepoKind_REPO_KIND_GENERAL,
		State:     state,
		Format:    sidecar.EvsRepoFormat1,
		NetworkId: sidecar.Id(self.networkId).Bytes(),
	}
	switch state {
	case protocol.EvsRepoState_EVS_REPO_DELETED:
		object.DeletionId = sidecar.NewId().Bytes()
		object.StateSigner = sidecar.NewId().Bytes()
		object.StateMs = stateTime.UnixMilli()
	case protocol.EvsRepoState_EVS_REPO_SHREDDING, protocol.EvsRepoState_EVS_REPO_SHREDDED:
		object.DeletionId = sidecar.NewId().Bytes()
		object.StateMs = stateTime.UnixMilli()
	}
	repoBytes, err := sidecar.MarshalEvsRepoObject(object)
	if err != nil {
		self.t.Fatal(err)
	}
	if _, err := first.PutIfNoneMatch(self.ctx, sidecar.EvsRepoObjectKey(repoId), repoBytes, nil); err != nil {
		self.t.Fatal(err)
	}
	return repoId
}

// Whether ids holds id.
func containsId(ids []sidecar.Id, id sidecar.Id) bool {
	for _, held := range ids {
		if held == id {
			return true
		}
	}
	return false
}

// repo/ as the first cluster holds it.
func testRepoObject(t testing.TB, ctx context.Context, blobs *sidecartest.FakeBlobs, repoId sidecar.Id) (*protocol.EvsRepoObject, string) {
	t.Helper()
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	object, etag, err := sidecar.ReadEvsRepoObject(ctx, first, repoId)
	if err != nil {
		t.Fatal(err)
	}
	return object, etag
}

// The keys under prefix on the first cluster.
func testKeyCount(t testing.TB, ctx context.Context, blobs *sidecartest.FakeBlobs, prefix string) int {
	t.Helper()
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	infos, _, err := first.List(ctx, prefix, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(infos)
}

// A restored network's log with an active repo, and an orphan's with three: active, deleted under a RepoDelete of its
// own a day before the loss, and shredding under a deletion of its own, all on one fake host whose clock times the
// step, the job and evs's shred.
type testOrphanWorld struct {
	t     testing.TB
	ctx   context.Context
	host  *sidecartest.FakeHost
	clock *sidecartest.FakeClock
	// the plans the host's fakes take
	faults        *sidecartest.FaultQueue
	restored      *testLogWorld
	orphan        *testLogWorld
	kept          sidecar.Id
	lostActive    sidecar.Id
	lostDeleted   sidecar.Id
	lostShredding sidecar.Id
}

func newTestOrphanWorld(t testing.TB, ctx context.Context) *testOrphanWorld {
	t.Helper()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	faults := sidecartest.NewFaultQueue()
	host := sidecartest.NewFakeHost(ctx, clock, faults, nil, nil)
	first, err := host.Blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	restoredId, restoredOwner := server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, restoredId, "restore-kept", restoredOwner)
	self := &testOrphanWorld{
		t:        t,
		ctx:      ctx,
		host:     host,
		clock:    clock,
		faults:   faults,
		restored: newTestLogWorld(t, ctx, first, restoredId, restoredOwner),
		orphan:   newTestLogWorld(t, ctx, first, server.NewId(), server.NewId()),
	}
	self.kept = self.restored.placedRepoIn(host.Blobs, protocol.EvsRepoState_EVS_REPO_ACTIVE, clock.Now())
	self.lostActive = self.orphan.placedRepoIn(host.Blobs, protocol.EvsRepoState_EVS_REPO_ACTIVE, clock.Now())
	self.lostDeleted = self.orphan.placedRepoIn(host.Blobs, protocol.EvsRepoState_EVS_REPO_DELETED, clock.Now().Add(-24*time.Hour))
	self.lostShredding = self.orphan.placedRepoIn(host.Blobs, protocol.EvsRepoState_EVS_REPO_SHREDDING, clock.Now().Add(-time.Hour))
	return self
}

// The ops step's deps, as sidecarctl's: its blobs held to a write budget it does not have, so any write to a
// metadata cluster would fail the step.
func (self *testOrphanWorld) stepDeps() *OrphanLogDeps {
	first, _ := self.host.Blobs.FirstCluster()
	sequencerSettings := model.DefaultConfigLogSettings()
	sequencerSettings.Clock = self.orphan.world.Clock()
	return &OrphanLogDeps{
		Blobs:     evsservice.NewBudgetedBlobs(self.host.Blobs),
		Sequencer: model.NewConfigLogSequencer(first, sequencerSettings, nil),
		Tasks:     self.host.Tasks,
		Now:       self.clock.Now,
	}
}

// The job's deps, as taskworker's: every metadata write it makes must carry its class from the host's budget.
func (self *testOrphanWorld) jobDeps() *OrphanLogDeps {
	return &OrphanLogDeps{
		Blobs:  evsservice.NewBudgetedBlobs(self.host.Blobs),
		Tasks:  self.host.Tasks,
		Budget: self.host.Budget,
		Now:    self.clock.Now,
	}
}

// The ops step under incident, now on the world's clock.
func (self *testOrphanWorld) runOpsStep(incident *Incident) {
	self.t.Helper()
	if err := RunOpsStep(self.ctx, self.stepDeps(), &StepRun{Incident: incident, Out: &bytes.Buffer{}}); err != nil {
		self.t.Fatal(err)
	}
}

// The queue's pending requests of the job named name.
func (self *testOrphanWorld) pending(name string) []*sidecar.TaskRequest {
	requests := []*sidecar.TaskRequest{}
	for _, request := range self.host.Tasks.Pending() {
		if request.Name == name {
			requests = append(requests, request)
		}
	}
	return requests
}

// The one orphan log job queued.
func (self *testOrphanWorld) job() *sidecar.TaskRequest {
	self.t.Helper()
	jobs := self.pending(OrphanLogJobName)
	if len(jobs) != 1 {
		self.t.Fatalf("%d orphan log jobs queued", len(jobs))
	}
	return jobs[0]
}

// Each repo's repo/ ETag, which every write changes.
func (self *testOrphanWorld) repoEtags(extraRepoIds ...sidecar.Id) map[sidecar.Id]string {
	self.t.Helper()
	etags := map[sidecar.Id]string{}
	for _, repoId := range append([]sidecar.Id{self.kept, self.lostActive, self.lostDeleted, self.lostShredding}, extraRepoIds...) {
		_, etag := testRepoObject(self.t, self.ctx, self.host.Blobs, repoId)
		etags[repoId] = etag
	}
	return etags
}

// The objects of the orphan's log, its entries and its rows, and of the restored network's.
func (self *testOrphanWorld) logKeyCounts() (orphanEntryCount int, orphanRowCount int, restoredEntryCount int) {
	self.t.Helper()
	orphanEntryCount = testKeyCount(self.t, self.ctx, self.host.Blobs, sidecar.ConfigEntryKeyPrefix(sidecar.Id(self.orphan.networkId)))
	orphanRowCount = testKeyCount(self.t, self.ctx, self.host.Blobs, "projrows/"+sidecar.Id(self.orphan.networkId).String()+"/")
	restoredEntryCount = testKeyCount(self.t, self.ctx, self.host.Blobs, sidecar.ConfigEntryKeyPrefix(sidecar.Id(self.restored.networkId)))
	return
}

// Asserts the repo shows state under the incident, named by no signer, since at.
func (self *testOrphanWorld) assertShreddingUnder(incident *Incident, repoId sidecar.Id, state protocol.EvsRepoState, at time.Time) {
	self.t.Helper()
	object, _ := testRepoObject(self.t, self.ctx, self.host.Blobs, repoId)
	connect.AssertEqual(self.t, object.GetState(), state)
	connect.AssertEqual(self.t, object.GetDeletionId(), sidecar.Id(incident.IncidentId()).Bytes())
	connect.AssertEqual(self.t, len(object.GetStateSigner()), 0)
	if state == protocol.EvsRepoState_EVS_REPO_SHREDDING {
		connect.AssertEqual(self.t, object.GetStateMs(), at.UnixMilli())
	}
}

func TestOpsStepRefusesADatabaseTheScriptDidNotMark(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		self := newTestOrphanWorld(t, ctx)
		incident := testIncident(t, IncidentKindPostgres)
		err := RunOpsStep(ctx, self.stepDeps(), &StepRun{Incident: incident, Out: &bytes.Buffer{}})
		connect.AssertEqual(t, errors.Is(err, ErrIncidentMarker), true)
		connect.AssertEqual(t, len(incident.RowsOfKind(ListKindOrphanLog)), 0)
		connect.AssertEqual(t, len(self.host.Tasks.AllRequests()), 0)
	})
}

func TestOrphanLogsListEachRepoShreddingAWeekOnAndLeaveItActive(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		self := newTestOrphanWorld(t, ctx)
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		before := self.repoEtags()
		entryCount, rowCount, restoredCount := self.logKeyCounts()
		shredAt := self.clock.Now().Add(7*24*time.Hour + 10*time.Minute)

		self.runOpsStep(incident)
		// a rerun an hour on lists every row as the first did and asks for the same job
		self.clock.Advance(time.Hour)
		self.runOpsStep(incident)

		// its config repo, which no test placed, and the three it created
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanLog, self.orphan.networkId), "4 repos")
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanLog, self.restored.networkId), "")
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanShredAt, incident.IncidentId()), shredAt.Format(time.RFC3339Nano))
		shreddingAt := orphanRepoShreddingAtPrefix + shredAt.Format(time.RFC3339Nano)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanRepo, server.Id(self.lostActive)), shreddingAt)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanRepo, server.Id(self.lostDeleted)), shreddingAt)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanRepo, server.Id(self.lostShredding)), orphanRepoPast)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanRepo, server.Id(self.kept)), "")
		notPlacedCount := 0
		for _, row := range incident.RowsOfKind(ListKindOrphanRepo) {
			if row.Detail == orphanRepoNotPlaced {
				notPlacedCount += 1
			}
		}
		connect.AssertEqual(t, notPlacedCount, 1)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanLogDeleteOwed, self.orphan.networkId) != "", true)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanLogHeld, self.orphan.networkId), "")

		// nothing of the log or its repos changed: repo/ stays active
		connect.AssertEqual(t, self.repoEtags(), before)
		object, _ := testRepoObject(t, ctx, self.host.Blobs, self.lostActive)
		connect.AssertEqual(t, object.GetState(), protocol.EvsRepoState_EVS_REPO_ACTIVE)
		afterEntryCount, afterRowCount, afterRestoredCount := self.logKeyCounts()
		connect.AssertEqual(t, []int{afterEntryCount, afterRowCount, afterRestoredCount}, []int{entryCount, rowCount, restoredCount})

		// one job, once-keyed by incident and network, due a week and ten minutes after the incident: the rerun's
		// request coalesced with it
		job := self.job()
		connect.AssertEqual(t, job.RunAt.Equal(shredAt), true)
		offeredCount := 0
		for _, request := range self.host.Tasks.AllRequests() {
			if request.Name == OrphanLogJobName {
				offeredCount += 1
				connect.AssertEqual(t, request.OnceKey, job.OnceKey)
				connect.AssertEqual(t, request.Args, job.Args)
			}
		}
		connect.AssertEqual(t, offeredCount, 2)
		connect.AssertEqual(t, len(self.pending(evsservice.EvsShredTaskName)), 0)
	})
}

func TestOrphanLogJobChangesNothingBeforeAWeek(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		self := newTestOrphanWorld(t, ctx)
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		self.runOpsStep(incident)
		job := self.job()
		before := self.repoEtags()
		entryCount, rowCount, restoredCount := self.logKeyCounts()
		connect.AssertEqual(t, 0 < entryCount && 0 < rowCount, true)

		// a run the queue gives a minute early
		self.clock.AdvanceTo(job.RunAt.Add(-time.Minute))
		connect.AssertEqual(t, RunOrphanLogJob(ctx, self.jobDeps(), job.Args), nil)

		connect.AssertEqual(t, self.repoEtags(), before)
		afterEntryCount, afterRowCount, afterRestoredCount := self.logKeyCounts()
		connect.AssertEqual(t, []int{afterEntryCount, afterRowCount, afterRestoredCount}, []int{entryCount, rowCount, restoredCount})
		connect.AssertEqual(t, len(self.pending(evsservice.EvsShredTaskName)), 0)
		connect.AssertEqual(t, self.host.Budget.P0ByteCount("meta-1", sidecar.WriteP0KindLifecycle), int64(0))
		// it asked to run again at its time
		requests := self.host.Tasks.AllRequests()
		last := requests[len(requests)-1]
		connect.AssertEqual(t, last.Name, OrphanLogJobName)
		connect.AssertEqual(t, last.RunAt.Equal(job.RunAt), true)
		connect.AssertEqual(t, self.job().RunAt.Equal(job.RunAt), true)
	})
}

func TestOrphanLogJobMovesEachRepoToShreddingAndQueuesItsShred(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		self := newTestOrphanWorld(t, ctx)
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		self.runOpsStep(incident)
		job := self.job()
		_, shreddingEtag := testRepoObject(t, ctx, self.host.Blobs, self.lostShredding)
		_, keptEtag := testRepoObject(t, ctx, self.host.Blobs, self.kept)

		self.clock.AdvanceTo(job.RunAt)
		connect.AssertEqual(t, RunOrphanLogJob(ctx, self.jobDeps(), job.Args), nil)

		// the active repo and the one still deleted go shredding under the incident, with no signer
		self.assertShreddingUnder(incident, self.lostActive, protocol.EvsRepoState_EVS_REPO_SHREDDING, job.RunAt)
		self.assertShreddingUnder(incident, self.lostDeleted, protocol.EvsRepoState_EVS_REPO_SHREDDING, job.RunAt)
		// another deletion's shredding repo and the restored network's repo are left as they were
		_, etag := testRepoObject(t, ctx, self.host.Blobs, self.lostShredding)
		connect.AssertEqual(t, etag, shreddingEtag)
		_, etag = testRepoObject(t, ctx, self.host.Blobs, self.kept)
		connect.AssertEqual(t, etag, keptEtag)
		// the moves were P0 lifecycle on the repos' cluster, as DELETE's are
		connect.AssertEqual(t, 0 < self.host.Budget.P0ByteCount("meta-1", sidecar.WriteP0KindLifecycle), true)

		// each moved repo's first shred pass, due now; none for the other deletion's repo, whose own shred runs
		shreds := self.pending(evsservice.EvsShredTaskName)
		connect.AssertEqual(t, len(shreds), 2)
		shreddingOnceKeys := map[string]bool{}
		for _, shred := range shreds {
			repoId, deletionId, final, err := evsservice.ParseEvsShredArgs(shred.Args)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, repoId == self.lostActive || repoId == self.lostDeleted, true)
			connect.AssertEqual(t, deletionId, sidecar.Id(incident.IncidentId()))
			connect.AssertEqual(t, final, false)
			connect.AssertEqual(t, shred.RunAt.Equal(job.RunAt), true)
			shreddingOnceKeys[shred.OnceKey] = true
		}
		// the job asks to look again an hour on, which the step's request, still pending since this run was not
		// the queue's, holds at its sooner time
		requests := self.host.Tasks.AllRequests()
		last := requests[len(requests)-1]
		connect.AssertEqual(t, last.Name, OrphanLogJobName)
		connect.AssertEqual(t, last.RunAt.Equal(job.RunAt.Add(time.Hour)), true)

		// a rerun, as after a crash between a move and its shred's request, moves nothing again and asks for each
		// moved repo's shred again, which coalesces with the one queued
		etags := self.repoEtags()
		offeredCount := len(self.host.Tasks.AllRequests())
		connect.AssertEqual(t, RunOrphanLogJob(ctx, self.jobDeps(), job.Args), nil)
		connect.AssertEqual(t, self.repoEtags(), etags)
		reoffered := map[string]bool{}
		for _, request := range self.host.Tasks.AllRequests()[offeredCount:] {
			if request.Name == evsservice.EvsShredTaskName {
				reoffered[request.OnceKey] = true
			}
		}
		connect.AssertEqual(t, reoffered, shreddingOnceKeys)
		connect.AssertEqual(t, len(self.pending(evsservice.EvsShredTaskName)), 2)

		// evs's reconciler queues every shredding repo's shred under evs's own once key: the job's coalesce with
		// it, and the other deletion's repo gets its own
		offeredCount = len(self.host.Tasks.AllRequests())
		counts, err := evsservice.RunReconcile(ctx, self.host.ServiceHost(), &evsservice.EvsReconcileDeps{})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, counts.Queued, 3)
		connect.AssertEqual(t, len(self.pending(evsservice.EvsShredTaskName)), 3)
		for _, request := range self.host.Tasks.AllRequests()[offeredCount:] {
			repoId, _, _, err := evsservice.ParseEvsShredArgs(request.Args)
			connect.AssertEqual(t, err, nil)
			if repoId == self.lostActive || repoId == self.lostDeleted {
				connect.AssertEqual(t, shreddingOnceKeys[request.OnceKey], true)
			}
		}
	})
}

// A move whose If-Match loses, as to another write, and whose reread then fails ends the run with the reread's error,
// the repo unmoved, and the queue's retry moves it. The reread's error once fell into the loop body's own err and was
// lost, and the next try read a nil repo/.
func TestOrphanLogJobEndsWithTheRereadsErrorAfterALostMove(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		self := newTestOrphanWorld(t, ctx)
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		self.runOpsStep(incident)
		job := self.job()
		self.clock.AdvanceTo(job.RunAt)

		// the job's first read of the repo and its move's are the key's first two GETs
		repoKey := sidecar.EvsRepoObjectKey(self.lostActive)
		for _, plan := range []*sidecartest.FaultPlan{
			{Op: sidecartest.FaultOpPut, KeyPrefix: repoKey, Result: sidecartest.Precondition},
			{Op: sidecartest.FaultOpGet, KeyPrefix: repoKey, Nth: 3, Result: sidecartest.FailBeforeApply},
		} {
			if _, err := self.faults.Add(plan); err != nil {
				t.Fatal(err)
			}
		}
		connect.AssertEqual(t, RunOrphanLogJob(ctx, self.jobDeps(), job.Args) != nil, true)
		connect.AssertEqual(t, len(self.faults.Unfired()), 0)
		object, _ := testRepoObject(t, ctx, self.host.Blobs, self.lostActive)
		connect.AssertEqual(t, object.GetState(), protocol.EvsRepoState_EVS_REPO_ACTIVE)

		connect.AssertEqual(t, RunOrphanLogJob(ctx, self.jobDeps(), job.Args), nil)
		self.assertShreddingUnder(incident, self.lostActive, protocol.EvsRepoState_EVS_REPO_SHREDDING, job.RunAt)
		self.assertShreddingUnder(incident, self.lostDeleted, protocol.EvsRepoState_EVS_REPO_SHREDDING, job.RunAt)
	})
}

func TestOrphanLogJobEndsWhenTheNetworksRowsCameBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		self := newTestOrphanWorld(t, ctx)
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		self.runOpsStep(incident)
		job := self.job()
		// the network's rows come back within the week, as a later restore could bring them
		model.Testing_CreateNetwork(ctx, self.orphan.networkId, "restore-returned", self.orphan.ownerUserId)
		before := self.repoEtags()
		entryCount, rowCount, restoredCount := self.logKeyCounts()
		offeredCount := len(self.host.Tasks.AllRequests())

		self.clock.AdvanceTo(job.RunAt)
		connect.AssertEqual(t, RunOrphanLogJob(ctx, self.jobDeps(), job.Args), nil)

		// nothing moved, nothing deleted, and nothing queued: no shred and no next run
		connect.AssertEqual(t, self.repoEtags(), before)
		afterEntryCount, afterRowCount, afterRestoredCount := self.logKeyCounts()
		connect.AssertEqual(t, []int{afterEntryCount, afterRowCount, afterRestoredCount}, []int{entryCount, rowCount, restoredCount})
		connect.AssertEqual(t, len(self.host.Tasks.AllRequests()), offeredCount)
		connect.AssertEqual(t, self.host.Budget.P0ByteCount("meta-1", sidecar.WriteP0KindLifecycle), int64(0))
	})
}

func TestOrphanLogJobDeletesTheLogAfterTheLastShredAndNotBefore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		self := newTestOrphanWorld(t, ctx)
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		// the queue runs the job as taskworker does, and evs's own shred, whose gkm step finds no group objects in
		// this world
		runErrors := []error{}
		jobDeps := self.jobDeps()
		self.host.Tasks.Register(OrphanLogJobName, func(ctx context.Context, args []byte) error {
			err := RunOrphanLogJob(ctx, jobDeps, args)
			if err != nil {
				runErrors = append(runErrors, err)
			}
			return err
		})
		host := self.host.ServiceHost()
		self.host.Tasks.Register(evsservice.EvsShredTaskName, func(ctx context.Context, args []byte) error {
			_, err := evsservice.RunShred(ctx, host, &evsservice.EvsShredDeps{
				ShredGroup: func(ctx context.Context, cluster sidecar.BlobCluster, repoId sidecar.Id, final bool) error {
					return nil
				},
			}, args)
			if err != nil {
				runErrors = append(runErrors, err)
			}
			return err
		})
		runDue := func() {
			t.Helper()
			if _, err := self.host.Tasks.RunDue(ctx); err != nil {
				t.Fatal(err)
			}
			connect.AssertEqual(t, runErrors, []error{})
		}
		assertLog := func(kept bool) {
			t.Helper()
			entryCount, rowCount, restoredCount := self.logKeyCounts()
			connect.AssertEqual(t, 0 < entryCount, kept)
			connect.AssertEqual(t, 0 < rowCount, kept)
			connect.AssertEqual(t, 0 < restoredCount, true)
		}

		self.runOpsStep(incident)
		shredAt := self.job().RunAt
		_, _, restoredCount := self.logKeyCounts()
		assertLog(true)

		// the week out: the job moves the repos it may and queues their shreds, which wait out their first 20 s
		self.clock.AdvanceTo(shredAt)
		runDue()
		self.assertShreddingUnder(incident, self.lostActive, protocol.EvsRepoState_EVS_REPO_SHREDDING, shredAt)
		self.assertShreddingUnder(incident, self.lostDeleted, protocol.EvsRepoState_EVS_REPO_SHREDDING, shredAt)
		assertLog(true)

		// their first passes, then their final passes ten minutes on, to the tombstone
		self.clock.Advance(20 * time.Second)
		runDue()
		assertLog(true)
		self.clock.Advance(10 * time.Minute)
		runDue()
		self.assertShreddingUnder(incident, self.lostActive, protocol.EvsRepoState_EVS_REPO_SHREDDED, shredAt)
		self.assertShreddingUnder(incident, self.lostDeleted, protocol.EvsRepoState_EVS_REPO_SHREDDED, shredAt)
		assertLog(true)

		// the job's next look: the other deletion's repo is still shredding, so the log stays, looked at again later
		self.clock.AdvanceTo(shredAt.Add(time.Hour))
		runDue()
		assertLog(true)
		connect.AssertEqual(t, self.job().RunAt.Equal(shredAt.Add(2*time.Hour)), true)

		// evs's reconciler queues that repo's own shred, which runs to its tombstone
		_, err := evsservice.RunReconcile(ctx, host, &evsservice.EvsReconcileDeps{})
		connect.AssertEqual(t, err, nil)
		runDue()
		self.clock.Advance(10 * time.Minute)
		runDue()
		object, _ := testRepoObject(t, ctx, self.host.Blobs, self.lostShredding)
		connect.AssertEqual(t, object.GetState(), protocol.EvsRepoState_EVS_REPO_SHREDDED)
		assertLog(true)

		// the job's look after the last shred: the log goes, the restored network's stays, and the job ends
		self.clock.AdvanceTo(shredAt.Add(2 * time.Hour))
		runDue()
		assertLog(false)
		_, _, afterRestoredCount := self.logKeyCounts()
		connect.AssertEqual(t, afterRestoredCount, restoredCount)
		connect.AssertEqual(t, len(self.pending(OrphanLogJobName)), 0)
		connect.AssertEqual(t, 0 < self.host.Budget.P0ByteCount("meta-1", sidecar.WriteP0KindLog), true)
		kept, _ := testRepoObject(t, ctx, self.host.Blobs, self.kept)
		connect.AssertEqual(t, kept.GetState(), protocol.EvsRepoState_EVS_REPO_ACTIVE)

		// a rerun changes nothing more
		etags := self.repoEtags()
		offeredCount := len(self.host.Tasks.AllRequests())
		var args []byte
		for _, request := range self.host.Tasks.AllRequests() {
			if request.Name == OrphanLogJobName {
				args = request.Args
			}
		}
		connect.AssertEqual(t, RunOrphanLogJob(ctx, jobDeps, args), nil)
		connect.AssertEqual(t, self.repoEtags(), etags)
		connect.AssertEqual(t, len(self.host.Tasks.AllRequests()), offeredCount)
		assertLog(false)
	})
}

func TestOrphanLogsHoldALogNamingAKeyingRepo(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		self := newTestOrphanWorld(t, ctx)
		keying := self.orphan.placedRepoIn(self.host.Blobs, protocol.EvsRepoState_EVS_REPO_KEYING, self.clock.Now())
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		before := self.repoEtags(keying)

		self.runOpsStep(incident)

		// no transition takes a keying repo to shredding, so the whole log is the operator's: no job, no time
		left := orphanRepoOtherPrefix + protocol.EvsRepoState_EVS_REPO_KEYING.String()
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanLog, self.orphan.networkId), "5 repos")
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanRepo, server.Id(keying)), left)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanRepo, server.Id(self.lostActive)), orphanRepoKept)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanRepo, server.Id(self.lostDeleted)), orphanRepoKept)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanRepo, server.Id(self.lostShredding)), orphanRepoPast)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanLogHeld, self.orphan.networkId), left)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanLogDeleteOwed, self.orphan.networkId), "")
		connect.AssertEqual(t, len(incident.RowsOfKind(ListKindOrphanShredAt)), 0)
		connect.AssertEqual(t, len(self.host.Tasks.AllRequests()), 0)

		// a job that finds such a repo, however it was queued, moves none of the log's repos
		args, err := json.Marshal(&orphanLogJobArgs{
			IncidentId: incident.IncidentId(),
			NetworkId:  self.orphan.networkId,
			RepoIds:    []server.Id{server.Id(self.lostActive), server.Id(keying)},
			ShredAt:    self.clock.Now(),
		})
		connect.AssertEqual(t, err, nil)
		err = RunOrphanLogJob(ctx, self.jobDeps(), args)
		connect.AssertEqual(t, errors.Is(err, ErrOrphanRepoLeft), true)
		connect.AssertEqual(t, self.repoEtags(keying), before)
		connect.AssertEqual(t, len(self.host.Tasks.AllRequests()), 0)
	})
}

// Args that name no orphaned log or no time are refused before anything is read, so a run of them touches nothing.
func TestOrphanLogJobRefusesArgsThatNameNoLogOrTime(t *testing.T) {
	ctx := t.Context()
	noTime, err := json.Marshal(&orphanLogJobArgs{IncidentId: server.NewId(), NetworkId: server.NewId()})
	connect.AssertEqual(t, err, nil)
	noNetwork, err := json.Marshal(&orphanLogJobArgs{IncidentId: server.NewId(), ShredAt: time.Now()})
	connect.AssertEqual(t, err, nil)
	for _, args := range [][]byte{[]byte("no args"), noTime, noNetwork} {
		err := RunOrphanLogJob(ctx, &OrphanLogDeps{}, args)
		if !errors.Is(err, ErrOrphanLogJobArgs) {
			t.Errorf("args %q: %v", args, err)
		}
	}
}
