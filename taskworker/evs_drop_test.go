package taskworker

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
)

// The drop job's daily start queues itself a day on and a run for each repo with a table, and a repo's run with no
// resolve or mark built starts nothing and asks to run again (EVSB-M10-04 lands those seams).
func TestEvsDropStartQueuesEachRepoWithATable(t *testing.T) {
	var drop *evsJob
	for _, job := range evsJobs {
		if job.name == evsservice.EvsDropTaskName {
			drop = job
		}
	}
	if drop == nil || drop.first == nil {
		t.Fatal("no evs.drop job, or one with no chain")
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fake := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	host := fake.ServiceHost()
	repoId := sidecar.Id{1, 2, 3}
	table, err := evsservice.NewDropTable(host, evsservice.NewBudgetedBlobs(host.Blobs), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := fake.Budget.AdmitP1(t.Context(), "meta-1", [16]byte{9}, sidecar.WriteCost{ImmediateByteCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer grant.Close()
	commit := &evsservice.DropCommit{
		ClusterId: "meta-1", RepoId: repoId, RefId: sidecar.Id{4}, PushId: sidecar.Id{5}, Version: 2,
		Place: &protocol.EvsPlace{Minute: evsservice.BatchMinute(clock.Now()), Length: 100},
		Body:  &protocol.EvsDropHistory{DropId: make([]byte, 16), Action: 1, NotBeforeMs: clock.Now().UnixMilli()},
	}
	if err := table.Append(grant.Context(), commit, clock.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := drop.run(t.Context(), host, nil); err != nil {
		t.Fatal(err)
	}
	var repoRun *sidecar.TaskRequest
	var nextStart *sidecar.TaskRequest
	for _, request := range fake.Tasks.Pending() {
		switch request.OnceKey {
		case evsservice.EvsDropTaskName:
			nextStart = request
		case evsservice.EvsDropTaskName + ":" + sidecar.EvsKeyId(repoId):
			repoRun = request
		}
	}
	if nextStart == nil || !nextStart.RunAt.Equal(clock.Now().Add(24*time.Hour)) || repoRun == nil {
		t.Fatalf("the start queued %v and %v", nextStart, repoRun)
	}
	clusterId, gotRepoId, err := parseEvsDropRepoArgs(repoRun.Args)
	if err != nil || clusterId != "meta-1" || gotRepoId != repoId {
		t.Fatalf("a repo run's args: %q %v", clusterId, err)
	}
	// the drop is not startable for 25 min; the run asks to come back then
	if err := drop.run(t.Context(), host, repoRun.Args); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseEvsDropRepoArgs([]byte("repo\nmeta-1\nnot an id")); err == nil {
		t.Fatal("args naming no repo parsed")
	}
}

// The injected verify answers through dropverify: a job with no candidates and one ref agrees, its V the head's.
func TestEvsDropVerifierIsDropverify(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fake := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	cluster, _ := fake.Blobs.Cluster("meta-1")
	segment, _ := fake.Blobs.SegmentCluster()
	result, err := evsDropVerifier{}.Verify(t.Context(), &evsservice.DropVerifyRequest{
		Cluster: cluster, SegmentCluster: segment, RepoId: sidecar.Id{1}, JobId: sidecar.Id{2}, Round: 1,
		Liveness:     &evsservice.DropLiveness{Dead: map[sidecar.Id]bool{}, Known: map[sidecar.Id]bool{}},
		LockStanding: func(ctx context.Context) (bool, error) { return true, nil },
	})
	if err != nil || result.Disagree || !result.Locked || len(result.Snapshot) != 0 {
		t.Fatalf("an empty repo's verify: %+v %v", result, err)
	}
}

// A hint queues its repo's run at once and is taken; a key that is no repo's hint is passed over.
func TestEvsDropHintQueuesItsRepoAndIsTaken(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fake := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	host := fake.ServiceHost()
	watcher := &evsDropHintWatcher{host: host, settings: defaultEvsDropJobSettings(), keys: make(chan string, 1), remove: func() {}}
	repoId := sidecar.Id{7}
	if err := fake.Redis.Set(t.Context(), evsservice.DropHintKey(repoId), []byte("meta-1"), 0); err != nil {
		t.Fatal(err)
	}
	if err := watcher.take(t.Context(), evsservice.DropHintKey(repoId)); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := fake.Redis.Get(t.Context(), evsservice.DropHintKey(repoId)); found {
		t.Fatal("a queued hint was not taken")
	}
	pending := fake.Tasks.Pending()
	if len(pending) != 1 || pending[0].OnceKey != evsservice.EvsDropTaskName+":"+sidecar.EvsKeyId(repoId) || !pending[0].RunAt.Equal(clock.Now()) {
		t.Fatalf("pending %v", pending)
	}
	if err := watcher.take(t.Context(), "{evsd_seg-1}"); err != nil || len(fake.Tasks.Pending()) != 1 {
		t.Fatal("the cluster lease key queued a run")
	}
	watcher.noted(sidecar.RedisKeyEvent{Resync: true})
	watcher.noted(sidecar.RedisKeyEvent{Key: evsservice.DropHintKey(repoId)})
	watcher.noted(sidecar.RedisKeyEvent{Key: evsservice.DropHintKey(repoId)})
	if len(watcher.keys) != 1 {
		t.Fatal("the subscriber's callback blocked or kept a resync")
	}
}
