//go:build sidecar_pending

package sidecarrestore

// The ops step and §19.10's deleted-network path on private Postgres and T2's fake blobs (OPS-M3-03): a log whose
// network the restored rows lack is listed with its repos; until the deleted state's signer is settled nothing is
// marked or queued; with one, each repo is marked deleted{the incident id} once, the deletion of its log is owed and
// queued, and the job deletes proj/ and projrows/ only after the last shred, leaving a restored network's log alone.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A repo of the world's log, placed and active on the first cluster as evs's create leaves it.
func (self *testLogWorld) placedRepo(blobs *sidecartest.FakeBlobs) sidecar.Id {
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
	repoBytes, err := sidecar.MarshalEvsRepoObject(&protocol.EvsRepoObject{
		RepoId:    repoId.Bytes(),
		Kind:      protocol.RepoKind_REPO_KIND_GENERAL,
		State:     protocol.EvsRepoState_EVS_REPO_ACTIVE,
		Format:    sidecar.EvsRepoFormat1,
		NetworkId: sidecar.Id(self.networkId).Bytes(),
	})
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

// Moves repo/ one transition on, as the shred's phases do.
func testRepoTransition(t testing.TB, ctx context.Context, blobs *sidecartest.FakeBlobs, repoId sidecar.Id, state protocol.EvsRepoState) {
	t.Helper()
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	object, etag := testRepoObject(t, ctx, blobs, repoId)
	next := &protocol.EvsRepoObject{
		RepoId: object.GetRepoId(), Kind: object.GetKind(), State: state, Format: object.GetFormat(),
		NetworkId: object.GetNetworkId(), OwnerMemberId: object.GetOwnerMemberId(), DeletionId: object.GetDeletionId(),
		StateMs: object.GetStateMs() + 1,
	}
	if err := sidecar.CheckEvsRepoTransition(object, next); err != nil {
		t.Fatal(err)
	}
	nextBytes, err := sidecar.MarshalEvsRepoObject(next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.PutIfMatch(ctx, sidecar.EvsRepoObjectKey(repoId), etag, nextBytes, nil); err != nil {
		t.Fatal(err)
	}
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

// A restored network's log and an orphan's, each with a placed repo.
type testOrphanWorld struct {
	blobs    *sidecartest.FakeBlobs
	restored *testLogWorld
	orphan   *testLogWorld
	kept     sidecar.Id
	lost     sidecar.Id
	tasks    *sidecartest.FakeTasks
	now      time.Time
}

func newTestOrphanWorld(t testing.TB, ctx context.Context) *testOrphanWorld {
	t.Helper()
	blobs := testFakeBlobs(t)
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	restoredId, restoredOwner := server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, restoredId, "restore-kept", restoredOwner)
	restored := newTestLogWorld(t, ctx, first, restoredId, restoredOwner)
	orphan := newTestLogWorld(t, ctx, first, server.NewId(), server.NewId())
	self := &testOrphanWorld{
		blobs:    blobs,
		restored: restored,
		orphan:   orphan,
		tasks:    sidecartest.NewFakeTasks(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil),
		now:      server.NowUtc(),
	}
	self.kept = restored.placedRepo(blobs)
	self.lost = orphan.placedRepo(blobs)
	return self
}

// The orphan path's deps, with the signer settings gives.
func (self *testOrphanWorld) deps(settings *OrphanLogSettings) *OrphanLogDeps {
	first, _ := self.blobs.FirstCluster()
	sequencerSettings := model.DefaultConfigLogSettings()
	sequencerSettings.Clock = self.orphan.world.Clock()
	return &OrphanLogDeps{
		Blobs:     self.blobs,
		Sequencer: model.NewConfigLogSequencer(first, sequencerSettings, nil),
		Tasks:     self.tasks,
		Now:       func() time.Time { return self.now },
		Settings:  settings,
	}
}

func TestOpsStepRefusesADatabaseTheScriptDidNotMark(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		self := newTestOrphanWorld(t, ctx)
		incident := testIncident(t, IncidentKindPostgres)
		err := RunOpsStep(ctx, self.deps(nil), &StepRun{Incident: incident, Out: &bytes.Buffer{}})
		connect.AssertEqual(t, errors.Is(err, ErrIncidentMarker), true)
		connect.AssertEqual(t, len(incident.RowsOfKind(ListKindOrphanLog)), 0)
	})
}

func TestOrphanLogsHoldEveryRepoUntilTheSignerIsSettled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		self := newTestOrphanWorld(t, ctx)
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())

		connect.AssertEqual(t, RunOpsStep(ctx, self.deps(nil), &StepRun{Incident: incident, Out: &bytes.Buffer{}}), nil)
		// its config repo, which no test placed, and the repo it created
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanLog, self.orphan.networkId), "2 repos")
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanLog, self.restored.networkId), "")
		held := testListDetail(t, incident, ListKindOrphanRepo, server.Id(self.lost))
		connect.AssertEqual(t, strings.HasPrefix(held, orphanRepoHeldPrefix) && strings.Contains(held, "ops-m3-03-orphan-deleted-signer"), true)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanLogHeld, self.orphan.networkId) != "", true)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanLogDeleteOwed, self.orphan.networkId), "")
		object, _ := testRepoObject(t, ctx, self.blobs, self.lost)
		connect.AssertEqual(t, object.GetState(), protocol.EvsRepoState_EVS_REPO_ACTIVE)
		connect.AssertEqual(t, len(self.tasks.AllRequests()), 0)
	})
}

func TestOrphanLogsMarkReposAndDeleteTheLogWithTheLastShred(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		self := newTestOrphanWorld(t, ctx)
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		signer := sidecar.NewId()
		settings := DefaultOrphanLogSettings()
		settings.DeletedSigner = func(networkId server.Id, repoId sidecar.Id) (sidecar.Id, error) { return signer, nil }

		for range 2 {
			connect.AssertEqual(t, RunOpsStep(ctx, self.deps(settings), &StepRun{Incident: incident, Out: &bytes.Buffer{}}), nil)
		}
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanRepo, server.Id(self.lost)), orphanRepoMarked)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanLogDeleteOwed, self.orphan.networkId) != "", true)
		object, _ := testRepoObject(t, ctx, self.blobs, self.lost)
		connect.AssertEqual(t, object.GetState(), protocol.EvsRepoState_EVS_REPO_DELETED)
		connect.AssertEqual(t, object.GetDeletionId(), sidecar.Id(incident.IncidentId()).Bytes())
		connect.AssertEqual(t, object.GetStateMs(), self.now.UnixMilli())
		kept, _ := testRepoObject(t, ctx, self.blobs, self.kept)
		connect.AssertEqual(t, kept.GetState(), protocol.EvsRepoState_EVS_REPO_ACTIVE)

		// one job, once-keyed by incident and network, first due after §11.12's start
		pending := self.tasks.Pending()
		connect.AssertEqual(t, len(pending), 1)
		connect.AssertEqual(t, pending[0].Name, OrphanLogJobName)
		connect.AssertEqual(t, pending[0].RunAt.Equal(self.now.Add(settings.FirstCheckAfter)), true)
		args := pending[0].Args

		orphanEntries := sidecar.ConfigEntryKeyPrefix(sidecar.Id(self.orphan.networkId))
		orphanRows := "projrows/" + sidecar.Id(self.orphan.networkId).String() + "/"
		restoredEntries := sidecar.ConfigEntryKeyPrefix(sidecar.Id(self.restored.networkId))
		connect.AssertEqual(t, 0 < testKeyCount(t, ctx, self.blobs, orphanEntries), true)
		restoredCount := testKeyCount(t, ctx, self.blobs, restoredEntries)

		// a run before the shred keeps the log and looks again later
		jobTasks := sidecartest.NewFakeTasks(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil)
		jobDeps := self.deps(settings)
		jobDeps.Tasks = jobTasks
		connect.AssertEqual(t, RunOrphanLogJob(ctx, jobDeps, args), nil)
		connect.AssertEqual(t, 0 < testKeyCount(t, ctx, self.blobs, orphanEntries), true)
		connect.AssertEqual(t, len(jobTasks.Pending()), 1)
		connect.AssertEqual(t, jobTasks.Pending()[0].RunAt.Equal(self.now.Add(settings.RecheckInterval)), true)

		testRepoTransition(t, ctx, self.blobs, self.lost, protocol.EvsRepoState_EVS_REPO_SHREDDING)
		connect.AssertEqual(t, RunOrphanLogJob(ctx, jobDeps, args), nil)
		connect.AssertEqual(t, 0 < testKeyCount(t, ctx, self.blobs, orphanEntries), true)

		testRepoTransition(t, ctx, self.blobs, self.lost, protocol.EvsRepoState_EVS_REPO_SHREDDED)
		for range 2 {
			connect.AssertEqual(t, RunOrphanLogJob(ctx, jobDeps, args), nil)
		}
		connect.AssertEqual(t, testKeyCount(t, ctx, self.blobs, orphanEntries), 0)
		connect.AssertEqual(t, testKeyCount(t, ctx, self.blobs, orphanRows), 0)
		connect.AssertEqual(t, testKeyCount(t, ctx, self.blobs, restoredEntries), restoredCount)
	})
}
