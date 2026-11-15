package taskworker

// EVSB-M10-03's drop jobs and dry runs among evs's jobs (evs_jobs.go; §11.13's drop history rows), and EVSB-M10-05's
// verify injected into them:
//
//   - evs.drop with no args is the daily start: it queues itself a day on, then LISTs drops/ on each metadata cluster
//     and queues each repo's run, so a lost {evsd_} hint holds a repo's drops back a day at most (§11.13's who row).
//   - evs.drop with a repo's args is that repo's run: its drop job (evsservice.DropJob), then its dry runs
//     (evsservice.DropPreviews), then its next run at the sooner time either asks for, once-keyed by the repo, so a
//     waiting job is woken at its retry_ms and then each 10 min (DESIGN-DETAILS 10.2.8).
//   - the {evsd_<repo_id>} hints evs sets at each DROP, cancel and dry run reach this process through its Redis key-event
//     subscriber (startEvsDropHints), and each queues its repo's run at once and is taken.
//
// Verify is dropverify.Verify, which evsservice cannot import, so it is injected here behind evsservice.DropVerifier
// (D-EVS-64: the oracle is written apart from mark and shipped in taskworker). Resolve and mark are evsservice's
// DropResolve and DropMark (EVSB-M10-04), and the deletes its DropDelete (EVSB-M10-06), which read §19.11's drop mode
// themselves, so observe deletes nothing whatever the job asks. The lock the start and verify read is §8.9's state in
// the process's one gate cache (C-09), which gkm's reconciler start installs (installEvsDropLocks).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/evsservice/dropverify"
)

// The drop jobs' tunables in taskworker (QD-702).
type evsDropJobSettings struct {
	// The daily start's interval: 24 h, the bound on a lost hint (§11.13's who row). Positive.
	StartInterval time.Duration
	// Hints the subscriber holds before it drops one, which the daily start then covers. Positive.
	HintQueueCount int
	// How long a hint's queue waits when the task queue does not answer, before it tries again. Positive.
	HintRetryInterval time.Duration
}

// §11.13's values. A fresh value each call.
func defaultEvsDropJobSettings() *evsDropJobSettings {
	return &evsDropJobSettings{
		StartInterval:     24 * time.Hour,
		HintQueueCount:    10000,
		HintRetryInterval: 10 * time.Second,
	}
}

// The args of one repo's run: "repo", its metadata cluster and its id, one a line.
const evsDropRepoArgs = "repo"

// Args that name no run of evs.drop.
var errEvsDropArgs = errors.New("evs.drop args invalid")

// One repo's run, once-keyed by the repo.
func evsDropRepoRequest(clusterId string, repoId sidecar.Id, runAt time.Time) *sidecar.TaskRequest {
	return &sidecar.TaskRequest{
		Name:    evsservice.EvsDropTaskName,
		OnceKey: evsservice.EvsDropTaskName + ":" + sidecar.EvsKeyId(repoId),
		Args:    []byte(strings.Join([]string{evsDropRepoArgs, clusterId, sidecar.EvsKeyId(repoId)}, "\n")),
		RunAt:   runAt,
	}
}

// The cluster and repo a run's args name.
func parseEvsDropRepoArgs(args []byte) (string, sidecar.Id, error) {
	fields := strings.Split(string(args), "\n")
	if len(fields) != 3 || fields[0] != evsDropRepoArgs || fields[1] == "" {
		return "", sidecar.Id{}, errEvsDropArgs
	}
	repoId, err := sidecar.ParseId(fields[2])
	if err != nil || sidecar.EvsKeyId(repoId) != fields[2] {
		return "", sidecar.Id{}, errEvsDropArgs
	}
	return fields[1], repoId, nil
}

func init() {
	evsJobs = append(evsJobs, &evsJob{name: evsservice.EvsDropTaskName, run: runEvsDrop, first: firstEvsDrop})
}

// The daily start's chain, from the runtime's start.
func firstEvsDrop(host *sidecar.ServiceHost) *sidecar.TaskRequest {
	return &sidecar.TaskRequest{OnceKey: evsservice.EvsDropTaskName, RunAt: host.ClockOrReal().Now()}
}

// The lock view the newest runtime's gate cache gives, nil until one is installed.
var evsDropLocksCurrent atomic.Pointer[evsservice.DropLockView]

// Makes §8.9's state in gate the lock the drop jobs read, and returns what removes it at its runtime's end, which leaves
// a newer runtime's in place.
func installEvsDropLocks(gate evsservice.EvsAclGate) func() {
	locks := evsservice.NewAclDropLocks(gate)
	evsDropLocksCurrent.Store(&locks)
	return func() {
		evsDropLocksCurrent.CompareAndSwap(&locks, nil)
	}
}

// The lock view current now, a nil interface while none is installed, which starts nothing.
func evsDropLocks() evsservice.DropLockView {
	if locks := evsDropLocksCurrent.Load(); locks != nil {
		return *locks
	}
	return nil
}

// dropverify.Verify behind evsservice.DropVerifier.
type evsDropVerifier struct{}

func (self evsDropVerifier) Verify(ctx context.Context, request *evsservice.DropVerifyRequest) (*evsservice.DropVerifyResult, error) {
	drops := make([]*dropverify.Drop, 0, len(request.Drops))
	for _, drop := range request.Drops {
		drops = append(drops, &dropverify.Drop{DropId: drop.DropId, RefId: drop.RefId, Version: drop.Version})
	}
	result, err := dropverify.Verify(ctx, &dropverify.Request{
		Cluster: request.Cluster, SegmentCluster: request.SegmentCluster, RepoId: request.RepoId, JobId: request.JobId,
		Round: request.Round, Marked: request.Marked, Since: request.Since, Drops: drops,
		Executed: request.Liveness.Executed, LockStanding: request.LockStanding, Pace: request.Pace,
	})
	if err != nil {
		return nil, err
	}
	lateMarks := make([]evsservice.DropScanId, 0, len(result.LateMarks))
	for _, id := range result.LateMarks {
		lateMarks = append(lateMarks, evsservice.DropScanId{Id: id.Id, Kind: evsservice.DropIdKind(id.Kind)})
	}
	return &evsservice.DropVerifyResult{
		Snapshot: result.Snapshot, Disagree: result.Disagree, Detail: result.Detail, LateMarks: lateMarks,
		Cancelled: result.Cancelled, Locked: result.Locked, ScanByteCount: result.ScanByteCount,
	}, nil
}

// The seams one run's job and dry runs take: resolve, mark and the deletes, verify injected, and the lock from the gate
// cache. A seam that cannot be built stays nil, which starts no job.
func evsDropJobDeps(host *sidecar.ServiceHost) (*evsservice.DropJobDeps, error) {
	format2 := evsservice.NewFormat2Gate(host)
	resolver, err := evsservice.NewDropResolve(host, &evsservice.DropResolveDeps{Log: evsBudgetLogger{}})
	if err != nil {
		return nil, err
	}
	marker, err := evsservice.NewDropMark(host, &evsservice.DropMarkDeps{Log: evsBudgetLogger{}})
	if err != nil {
		return nil, err
	}
	deleter, err := evsservice.NewDropDelete(host, &evsservice.DropDeleteDeps{Format2: format2, Log: evsBudgetLogger{}})
	if err != nil {
		return nil, err
	}
	return &evsservice.DropJobDeps{
		Resolver: resolver,
		Marker:   marker,
		Verifier: evsDropVerifier{},
		Deleter:  deleter,
		Locks:    evsDropLocks(),
		Format2:  format2,
		Log:      evsBudgetLogger{},
	}, nil
}

// One run: the daily start, or one repo's.
func runEvsDrop(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	if len(args) == 0 {
		return runEvsDropStart(ctx, host)
	}
	clusterId, repoId, err := parseEvsDropRepoArgs(args)
	if err != nil {
		glog.Errorf("[evsjobs]%s with args that name no repo: dropped\n", evsservice.EvsDropTaskName)
		return nil
	}
	deps, err := evsDropJobDeps(host)
	if err != nil {
		return err
	}
	job, err := evsservice.NewDropJob(host, deps)
	if err != nil {
		return err
	}
	result, err := job.Run(ctx, clusterId, repoId)
	if err != nil {
		return err
	}
	previews, err := evsservice.NewDropPreviews(host, &evsservice.DropPreviewDeps{Resolver: deps.Resolver, Marker: deps.Marker, Log: evsBudgetLogger{}})
	if err != nil {
		return err
	}
	previewResult, err := previews.Run(ctx, clusterId, repoId)
	if err != nil {
		return err
	}
	if glog.V(1) {
		glog.Infof("[evsjobs]drop run of repo %s: waiting %q, round %d, ended %t; dry run state %s\n",
			sidecar.EvsKeyId(repoId), result.Waiting, result.Round, result.Ended, previewResult.State)
	}
	next := result.Next
	if !previewResult.Next.IsZero() && (next.IsZero() || previewResult.Next.Before(next)) {
		next = previewResult.Next
	}
	if next.IsZero() {
		return nil
	}
	return host.Tasks.Schedule(ctx, evsDropRepoRequest(clusterId, repoId, next))
}

// The daily start: its next run first, then each repo with a table on each metadata cluster.
func runEvsDropStart(ctx context.Context, host *sidecar.ServiceHost) error {
	settings := defaultEvsDropJobSettings()
	now := host.ClockOrReal().Now()
	if err := host.Tasks.Schedule(ctx, &sidecar.TaskRequest{
		Name: evsservice.EvsDropTaskName, OnceKey: evsservice.EvsDropTaskName, RunAt: now.Add(settings.StartInterval),
	}); err != nil {
		return err
	}
	dropSettings := evsservice.DefaultDropSettings()
	for _, clusterId := range host.Blobs.MetadataClusterIds() {
		cluster, err := host.Blobs.Cluster(clusterId)
		if err != nil {
			return err
		}
		startAfter := ""
		for {
			objects, more, err := cluster.List(ctx, "drops/", startAfter, dropSettings.ListLimit)
			if err != nil {
				return err
			}
			for _, object := range objects {
				repoText, ok := strings.CutSuffix(strings.TrimPrefix(object.Key, "drops/"), "/_")
				if !ok {
					continue
				}
				repoId, err := sidecar.ParseId(repoText)
				if err != nil || evsservice.DropsKey(repoId) != object.Key {
					continue
				}
				if err := host.Tasks.Schedule(ctx, evsDropRepoRequest(clusterId, repoId, now)); err != nil {
					return err
				}
			}
			if !more || len(objects) == 0 {
				break
			}
			startAfter = objects[len(objects)-1].Key
		}
	}
	return nil
}

// What startEvsDropHints runs: a test replaces it.
var startEvsDropHints = startEvsDropHintsOnHost

// Watches the {evsd_} hints on the process's evs host, which startEvsJobs built, until ctx ends. A process with no
// host or no Redis watches nothing, and the daily start covers its repos.
func startEvsDropHintsOnHost(ctx context.Context, lifetime *sync.WaitGroup) {
	host := evsJobHost.Load()
	if host == nil || host.Redis == nil || host.Tasks == nil {
		glog.Infof("[evsjobs]no evs host, so no drop hint is watched\n")
		return
	}
	watcher, err := newEvsDropHintWatcher(host, defaultEvsDropJobSettings())
	if err != nil {
		glog.Errorf("[evsjobs]drop hints are not watched: %v\n", err)
		return
	}
	lifetime.Go(func() {
		watcher.run(ctx)
	})
}

// The hints' subscriber: the callback notes a key and returns, and run queues each repo's run and takes its hint.
type evsDropHintWatcher struct {
	host     *sidecar.ServiceHost
	settings *evsDropJobSettings
	keys     chan string
	remove   func()
}

// A watcher on host's shared subscriber.
func newEvsDropHintWatcher(host *sidecar.ServiceHost, settings *evsDropJobSettings) (*evsDropHintWatcher, error) {
	self := &evsDropHintWatcher{host: host, settings: settings, keys: make(chan string, settings.HintQueueCount)}
	remove, err := host.Redis.AddKeyEventCallback("{evsd_*}", self.noted)
	if err != nil {
		return nil, err
	}
	self.remove = remove
	return self, nil
}

// On the subscriber's goroutine: notes the key, never blocking; one past the queue's bound waits for the daily start.
func (self *evsDropHintWatcher) noted(event sidecar.RedisKeyEvent) {
	if event.Resync || event.Key == "" {
		return
	}
	select {
	case self.keys <- event.Key:
	default:
	}
}

// Queues each noted repo's run and takes its hint, until ctx ends.
func (self *evsDropHintWatcher) run(ctx context.Context) {
	defer self.remove()
	clock := self.host.ClockOrReal()
	for {
		var key string
		select {
		case <-ctx.Done():
			return
		case key = <-self.keys:
		}
		if err := self.take(ctx, key); err != nil {
			glog.Errorf("[evsjobs]drop hint %s not taken: %v\n", key, err)
			select {
			case <-ctx.Done():
				return
			case <-clock.After(self.settings.HintRetryInterval):
			}
		}
	}
}

// Queues the repo's run its hint names and takes the hint; a key that is no repo's hint, or a hint already taken, is
// passed over.
func (self *evsDropHintWatcher) take(ctx context.Context, key string) error {
	repoId, ok := evsservice.DropHintRepoId(key)
	if !ok {
		return nil
	}
	clusterId, found, err := self.host.Redis.Get(ctx, key)
	if err != nil {
		return err
	}
	if !found || len(clusterId) == 0 {
		return nil
	}
	if err := self.host.Tasks.Schedule(ctx, evsDropRepoRequest(string(clusterId), repoId, self.host.ClockOrReal().Now())); err != nil {
		return fmt.Errorf("repo %s's run: %w", sidecar.EvsKeyId(repoId), err)
	}
	_, err = self.host.Redis.CompareAndDelete(ctx, key, clusterId)
	return err
}
