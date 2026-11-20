// Tests for `sidecarctl evs observe-report` (owner decision 513, QD-987). The drop half runs on sidecartest's fake
// clusters: drop tables and jobs' snap-0 files written through evsservice's own encoders, repos placed through
// place/ on either metadata cluster, and job times read from the objects' LastModified under the fake clock. The
// purge half answers the command's query from a recorded counter, each process a series of its own. The report
// prints each soak's days, counts and unmet thresholds and the run exits 0 met or not; a counter that fell reads as a
// restart of its series; a job counts toward D-TEST-18's share only with both booleans; an oracle refusal is read
// from the entry or, once the entry left, the void record; a table on a cluster place/ does not name is left out and
// counted; and whatever does not read exits 1 with nothing printed.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server/monitor"
)

// The env the tests name.
const evsObserveReportTestEnv = "test"

// A 16-byte id from a label.
func evsObserveReportTestId(label string) sidecar.Id {
	digest := sha256.Sum256([]byte("evs observe-report test " + label))
	return sidecar.Id(digest[:16])
}

// One process's counter as the monitor holds it: from start it rises by cleanPerDay and retainedPerDay each day, and
// from restart, when set, it counts again from zero. It answers nothing before start or after end, when set.
type evsObserveReportTestSeries struct {
	instance       string
	start          time.Time
	end            time.Time
	restart        time.Time
	cleanPerDay    int64
	retainedPerDay int64
}

// The series' readings at a time, clean then retained, and whether it answers there.
func (self *evsObserveReportTestSeries) at(at time.Time) (int64, int64, bool) {
	if at.Before(self.start) || !self.end.IsZero() && self.end.Before(at) {
		return 0, 0, false
	}
	since := self.start
	if !self.restart.IsZero() && !at.Before(self.restart) {
		since = self.restart
	}
	days := int64(at.Sub(since) / (24 * time.Hour))
	return days * self.cleanPerDay, days * self.retainedPerDay, true
}

// One repo of the world: where its table sits and what the table holds.
type evsObserveReportTestRepo struct {
	repoId    sidecar.Id
	clusterId string
	table     *evsservice.DropTableState
}

// Fake clusters with two metadata clusters, a recorded counter, and what a run asked of both.
type evsObserveReportWorld struct {
	t       *testing.T
	ctx     context.Context
	clock   *sidecartest.FakeClock
	host    *sidecartest.FakeHost
	segment sidecar.BlobCluster
	// a paid write's context per metadata cluster
	clusterPaidCtxs map[string]context.Context
	repos           []*evsObserveReportTestRepo
	series          []*evsObserveReportTestSeries
	idCount         int
	// what a run's settings counted and asked
	openCount  int
	closeCount int
	queries    []string
	queryTimes []time.Time
	// answers the queries when set, in place of the series
	query evsObserveReportQuery
}

// An empty world.
func newEvsObserveReportWorld(t *testing.T) *evsObserveReportWorld {
	ctx := t.Context()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	hostSettings := sidecartest.DefaultFakeHostSettings()
	hostSettings.Blobs.MetadataClusterIds = []string{"meta-1", "meta-2"}
	host := sidecartest.NewFakeHost(ctx, clock, nil, nil, hostSettings)
	segment, err := host.Blobs.SegmentCluster()
	if err != nil {
		t.Fatal(err)
	}
	world := &evsObserveReportWorld{t: t, ctx: ctx, clock: clock, host: host, segment: segment, clusterPaidCtxs: map[string]context.Context{}}
	networkId := [16]byte(evsObserveReportTestId("network"))
	for _, clusterId := range hostSettings.Blobs.MetadataClusterIds {
		grant, err := host.Budget.AdmitP1(ctx, clusterId, networkId, sidecar.WriteCost{ImmediateByteCount: 1})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(grant.Close)
		world.clusterPaidCtxs[clusterId] = grant.Context()
	}
	return world
}

// The next id of a kind.
func (self *evsObserveReportWorld) id(kind string) sidecar.Id {
	self.idCount += 1
	return evsObserveReportTestId(fmt.Sprintf("%s %d", kind, self.idCount))
}

// A repo whose place/ names placeClusterId, none for "", and whose table will sit on tableClusterId.
func (self *evsObserveReportWorld) repo(placeClusterId string, tableClusterId string) *evsObserveReportTestRepo {
	self.t.Helper()
	repo := &evsObserveReportTestRepo{repoId: self.id("repo"), clusterId: tableClusterId, table: &evsservice.DropTableState{}}
	self.repos = append(self.repos, repo)
	if placeClusterId == "" {
		return repo
	}
	placeBytes, err := (&sidecar.EvsRepoPlace{ClusterId: placeClusterId, CreatorSidecarId: evsObserveReportTestId("signer")}).Marshal()
	if err != nil {
		self.t.Fatal(err)
	}
	firstCluster, err := self.host.Blobs.FirstCluster()
	if err != nil {
		self.t.Fatal(err)
	}
	if _, err := firstCluster.PutIfNoneMatch(self.clusterPaidCtxs["meta-1"], sidecar.EvsPlaceKey(repo.repoId), placeBytes, nil); err != nil {
		self.t.Fatal(err)
	}
	return repo
}

// How a job's one drop stands in its repo's table.
type evsObserveReportTestOutcome int

const (
	// observed, its entry standing
	evsObserveReportTestObserved evsObserveReportTestOutcome = iota
	// refused {verify}, its entry standing
	evsObserveReportTestRefused
	// refused {verify}, its entry gone and its void record left
	evsObserveReportTestRefusedVoid
	// still executing
	evsObserveReportTestExecuting
	// done under drop: on
	evsObserveReportTestDone
)

// A job of repo that covered one drop and wrote its snap-0 now, by the world's clock, with shape; the drop stands in
// the repo's table as outcome. Returns the job's id.
func (self *evsObserveReportWorld) job(repo *evsObserveReportTestRepo, outcome evsObserveReportTestOutcome, shape evsservice.DropListedShape) sidecar.Id {
	self.t.Helper()
	jobId := self.id("job")
	covered := &evsservice.DropCovered{
		DropId: self.id("drop"), RefId: self.id("ref"), Version: 7, PushId: self.id("push"),
		Place:  &protocol.EvsPlace{Minute: 29000000, Instance: 1, Seq: 1, Length: 512},
		SeenMs: self.clock.Now().UnixMilli(),
	}
	key := evsservice.DropFileKey(repo.repoId, jobId, evsservice.DropFileSnap, 0)
	if _, err := self.segment.PutIfNoneMatch(self.ctx, key, evsservice.EncodeDropCoveredFile(repo.repoId, jobId, []*evsservice.DropCovered{covered}, shape), nil); err != nil {
		self.t.Fatal(err)
	}
	entry := &evsservice.DropTableEntry{
		DropId: covered.DropId, RefId: covered.RefId, PushId: covered.PushId, Version: covered.Version, Place: covered.Place, SeenMs: covered.SeenMs,
	}
	switch outcome {
	case evsObserveReportTestObserved:
		entry.State = evsservice.DropStateObserved
	case evsObserveReportTestRefused, evsObserveReportTestRefusedVoid:
		entry.State, entry.EndedDay = evsservice.DropStateRefusedVerify, 20000
		repo.table.Voids = append(repo.table.Voids, &evsservice.DropVoidRecord{DropId: covered.DropId, State: evsservice.DropStateRefusedVerify})
	case evsObserveReportTestExecuting:
		entry.State, entry.Phase = evsservice.DropStateExecuting, protocol.EvsDropPhase_EVS_DROP_MARK
	case evsObserveReportTestDone:
		entry.State, entry.EndedDay = evsservice.DropStateDone, 20000
	}
	if outcome != evsObserveReportTestRefusedVoid {
		repo.table.Drops = append(repo.table.Drops, entry)
	}
	return jobId
}

// Writes an object of a job or dry run of repo on the segment cluster now, as a later file of it.
func (self *evsObserveReportWorld) jobObject(repo *evsObserveReportTestRepo, jobId sidecar.Id, name string) {
	self.t.Helper()
	if _, err := self.segment.PutIfNoneMatch(self.ctx, evsservice.DropscanKey(repo.repoId, jobId, name), []byte("a later file"), nil); err != nil {
		self.t.Fatal(err)
	}
}

// Writes every repo's table on its cluster. A test calls it once, after its jobs.
func (self *evsObserveReportWorld) writeTables() {
	self.t.Helper()
	for _, repo := range self.repos {
		encoded, err := evsservice.EncodeDropTable(repo.table)
		if err != nil {
			self.t.Fatal(err)
		}
		cluster, err := self.host.Blobs.Cluster(repo.clusterId)
		if err != nil {
			self.t.Fatal(err)
		}
		if _, err := cluster.PutIfNoneMatch(self.clusterPaidCtxs[repo.clusterId], evsservice.DropsKey(repo.repoId), encoded, nil); err != nil {
			self.t.Fatal(err)
		}
	}
}

// The series' answer to one query at a time, as Mimir's vector: a clean and a retained result a process, each with
// the labels a pushed series carries.
func (self *evsObserveReportWorld) seriesAnswer(at time.Time) []*monitor.MimirQuerySample {
	answer := []*monitor.MimirQuerySample{}
	for _, series := range self.series {
		clean, retained, ok := series.at(at)
		if !ok {
			continue
		}
		for _, result := range []struct {
			name  string
			value int64
		}{{name: "clean", value: clean}, {name: "retained", value: retained}} {
			answer = append(answer, &monitor.MimirQuerySample{
				Labels: map[string]string{"env": evsObserveReportTestEnv, "instance": series.instance, "job": "sidecar-evs", "result": result.name},
				Value:  float64(result.value),
			})
		}
	}
	return answer
}

// A run's settings on the world's clusters, clock and counter, counting each open and close and recording each
// query.
func (self *evsObserveReportWorld) settings() *evsObserveReportSettings {
	settings := defaultEvsObserveReportSettings()
	settings.now = self.clock.Now
	settings.clock = self.clock
	settings.openEnv = func(ctx context.Context, env string) (*evsObserveReportEnv, error) {
		self.openCount += 1
		if env != evsObserveReportTestEnv {
			return nil, fmt.Errorf("the world is %s, not %s", evsObserveReportTestEnv, env)
		}
		return &evsObserveReportEnv{
			query: func(ctx context.Context, query string, at time.Time, maxByteCount int) ([]*monitor.MimirQuerySample, error) {
				self.queries = append(self.queries, query)
				self.queryTimes = append(self.queryTimes, at)
				if maxByteCount != settings.queryMaxByteCount {
					return nil, fmt.Errorf("the query was bounded at %d bytes, not the settings' %d", maxByteCount, settings.queryMaxByteCount)
				}
				if self.query != nil {
					return self.query(ctx, query, at, maxByteCount)
				}
				return self.seriesAnswer(at), nil
			},
			blobs: self.host.Blobs,
			close: func() { self.closeCount += 1 },
		}, nil
	}
	return settings
}

// Runs the command line after `evs observe-report` under settings through a command set, so a test reads the exit
// code a runbook step reads. Returns the code and what went to stdout and stderr.
func runEvsObserveReportTest(settings *evsObserveReportSettings, args ...string) (int, string, string) {
	commands := newSidecarctlCommandSet()
	commands.add(&sidecarctlCommand{
		words:   "evs observe-report",
		usage:   "<env> [--days=<n>]",
		summary: "the command under test",
		run: func(args []string, out io.Writer) error {
			return runEvsObserveReportWithSettings(context.Background(), args, settings, out)
		},
	})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := commands.run(append([]string{"evs", "observe-report"}, args...), stdout, stderr)
	return code, stdout.String(), stderr.String()
}

// Fails unless printed holds each of wants.
func evsObserveReportTestWants(t *testing.T, printed string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(printed, want) {
			t.Errorf("the report does not say %q:\n%s", want, printed)
		}
	}
}

// Both soaks met on the thresholds D-TEST-12 and D-TEST-18 give: 31 days of the counter at 1,000 clean a day in one
// process, and 200 observed jobs over ten repos on both metadata clusters, the first 31 days back, half of them with
// a rename and a cross-ref copy. The run asks one query a day over its 60 days, each the counter's daily maximum per
// series in the env, prints both soaks as met with their counts, and exits 0 having written nothing.
func TestEvsObserveReportPrintsBothSoaksMet(t *testing.T) {
	world := newEvsObserveReportWorld(t)
	start := world.clock.Now()
	world.series = []*evsObserveReportTestSeries{{instance: "evs-1", start: start, cleanPerDay: 1000}}
	for i := range 10 {
		clusterId := []string{"meta-1", "meta-2"}[i%2]
		repo := world.repo(clusterId, clusterId)
		for j := range 20 {
			shape := evsservice.DropListedShape{Renames: j < 12, CrossRefCopies: j < 10}
			world.job(repo, evsObserveReportTestObserved, shape)
			world.clock.Advance(time.Minute)
		}
	}
	world.writeTables()
	world.clock.AdvanceTo(start.Add(31 * 24 * time.Hour))
	before := world.host.Blobs.Dump()

	code, stdout, stderr := runEvsObserveReportTest(world.settings(), evsObserveReportTestEnv)
	if code != 0 || stderr != "" {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	evsObserveReportTestWants(t, stdout,
		"observe soaks of test at 2026-02-02T03:04:05Z\n",
		"read urnetwork_evs_versionless_observe_total at 32 of 61 daily samples over 60 days, and 200 drop jobs of 10 repos with a drop table\n",
		"purge observe (D-TEST-12): met: 31.0 days, 31000 candidates checked, 0 retained\n",
		"drop observe (D-TEST-18): met: 31.0 days, 200 jobs, 100 on repos with renames and cross-ref copies (120 with renames, 100 with cross-ref copies), 0 refused by the oracle\n",
		"this only read: format2.purge: on and format2.drop: on stay an operator's act in test's sidecar.yml\n",
	)
	if strings.Contains(stdout, "not met") || strings.Contains(stdout, "left out") {
		t.Errorf("a met report over repos all placed printed:\n%s", stdout)
	}
	if len(world.queries) != 61 || world.openCount != 1 || world.closeCount != 1 {
		t.Fatalf("asked %d queries, opened %d and closed %d", len(world.queries), world.openCount, world.closeCount)
	}
	for i, query := range world.queries {
		if query != `max_over_time(urnetwork_evs_versionless_observe_total{env="test"}[86400s])` {
			t.Fatalf("query %d is %s", i, query)
		}
		if want := world.clock.Now().Add(-time.Duration(60-i) * 24 * time.Hour); !world.queryTimes[i].Equal(want) {
			t.Fatalf("query %d was evaluated at %s, want %s", i, world.queryTimes[i], want)
		}
	}
	after := world.host.Blobs.Dump()
	if len(after) != len(before) {
		t.Fatalf("the run left %d objects of %d", len(after), len(before))
	}
	for key, value := range before {
		if !bytes.Equal(after[key], value) {
			t.Fatalf("the run changed %s", key)
		}
	}
	// no id of a repo, job or drop, and no label of a series, reaches the report (I-13)
	for _, repo := range world.repos {
		if strings.Contains(stdout, sidecar.EvsKeyId(repo.repoId)) {
			t.Fatalf("the report names a repo:\n%s", stdout)
		}
	}
	if strings.Contains(stdout, "evs-1") || strings.Contains(stdout, "sidecar-evs") {
		t.Fatalf("the report names a series:\n%s", stdout)
	}
}

// Short, thin soaks: ten days of the counter at 500 a day and five observed jobs, one of them with both booleans,
// two with a rename alone and one with a copy alone. The report states every threshold not met, with its days and
// counts, and the run still exits 0, since reading a soak is not turning a mode on.
func TestEvsObserveReportStatesEachUnmetThresholdAndExitsZero(t *testing.T) {
	world := newEvsObserveReportWorld(t)
	start := world.clock.Now()
	world.series = []*evsObserveReportTestSeries{{instance: "evs-1", start: start, cleanPerDay: 500}}
	repo := world.repo("meta-2", "meta-2")
	for _, shape := range []evsservice.DropListedShape{
		{Renames: true, CrossRefCopies: true}, {Renames: true}, {Renames: true}, {CrossRefCopies: true}, {},
	} {
		world.job(repo, evsObserveReportTestObserved, shape)
	}
	world.writeTables()
	world.clock.AdvanceTo(start.Add(10 * 24 * time.Hour))

	code, stdout, stderr := runEvsObserveReportTest(world.settings(), evsObserveReportTestEnv)
	if code != 0 || stderr != "" {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	evsObserveReportTestWants(t, stdout,
		"at 11 of 61 daily samples over 60 days, and 5 drop jobs of 1 repos with a drop table\n",
		"purge observe (D-TEST-12): not met: 10.0 days, 5000 candidates checked, 0 retained\n",
		"  10.0 of 14 days\n",
		"  5000 of 10000 candidates checked\n",
		"drop observe (D-TEST-18): not met: 10.0 days, 5 jobs, 1 on repos with renames and cross-ref copies (3 with renames, 2 with cross-ref copies), 0 refused by the oracle\n",
		"  10.0 of 30 days\n",
		"  5 of 200 jobs\n",
		"  1 jobs on repos with renames and cross-ref copies, under 50%\n",
	)
}

// An env with no observe check counted and no drop table reads as two soaks that have not begun, and exits 0.
func TestEvsObserveReportOnAnEnvWithNothingObserved(t *testing.T) {
	world := newEvsObserveReportWorld(t)
	code, stdout, stderr := runEvsObserveReportTest(world.settings(), evsObserveReportTestEnv, "--days=3")
	if code != 0 || stderr != "" {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	evsObserveReportTestWants(t, stdout,
		"at 0 of 4 daily samples over 3 days, and 0 drop jobs of 0 repos with a drop table\n",
		"purge observe (D-TEST-12): not met: 0.0 days, 0 candidates checked, 0 retained\n",
		"  no observe check has been counted\n",
		"drop observe (D-TEST-18): not met: 0.0 days, 0 jobs,",
		"  no observed drop job\n",
	)
	if len(world.queries) != 4 {
		t.Fatalf("--days=3 asked %d queries", len(world.queries))
	}
}

// A counter that fell reads as a restart of its series, and each process's series is read alone. One process counted
// 4,000 a day for two days, restarted an hour later, and counts at the same rate since; a second ran the first day
// and a half at 600 a day and ended; a third started a day ago and holds 2 retained candidates. Summed, the second's
// end would have hidden 600 of the first's rise, and the third's start part of the first's fall.
func TestEvsObserveReportReadsACounterThatFellAsARestart(t *testing.T) {
	world := newEvsObserveReportWorld(t)
	start := world.clock.Now()
	day := 24 * time.Hour
	world.series = []*evsObserveReportTestSeries{
		{instance: "evs-1", start: start, restart: start.Add(2*day + time.Hour), cleanPerDay: 4000},
		{instance: "evs-2", start: start, end: start.Add(day + 12*time.Hour), cleanPerDay: 600},
		{instance: "evs-3", start: start.Add(3 * day), cleanPerDay: 300, retainedPerDay: 2},
	}
	world.clock.AdvanceTo(start.Add(4 * day))
	// evs-1 reads 0, 4000, 8000, then 0 and 4000 since its restart an hour past day two at the same rate
	code, stdout, stderr := runEvsObserveReportTest(world.settings(), evsObserveReportTestEnv, "--days=10")
	if code != 0 || stderr != "" {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	// evs-1: 8000 before its restart and 4000 after; evs-2: 600; evs-3: 300 clean and 2 retained
	evsObserveReportTestWants(t, stdout,
		"at 5 of 11 daily samples over 10 days",
		"purge observe (D-TEST-12): not met: 4.0 days, 12902 candidates checked, 2 retained\n",
		"  4.0 of 14 days\n",
		"  2 candidates a retained version names\n",
	)
	if strings.Contains(stdout, "of 10000 candidates checked") {
		t.Errorf("12,902 candidates read as under the threshold:\n%s", stdout)
	}
}

// D-TEST-18's no-refusal threshold and what is a job: a job whose drop the oracle refused fails the soak whether its
// entry still stands or only its void record does; a job still executing, one done under drop: on and a dry run's
// files, which hold no snap-0, are no observed job; and a job's time is its newest object's, so a later file of an
// observed job moves it.
func TestEvsObserveReportCountsOracleRefusalsAndOnlyObservedJobs(t *testing.T) {
	world := newEvsObserveReportWorld(t)
	start := world.clock.Now()
	day := 24 * time.Hour
	repo := world.repo("meta-1", "meta-1")
	both := evsservice.DropListedShape{Renames: true, CrossRefCopies: true}
	observedJobId := world.job(repo, evsObserveReportTestObserved, both)
	// its first cand file, as old as its snap-0 and listed before it
	world.jobObject(repo, observedJobId, "cand-0000000000")
	world.clock.Advance(2 * day)
	// the observed job's del file, written two days after its snap-0
	world.jobObject(repo, observedJobId, "del-0000000000")
	world.job(repo, evsObserveReportTestRefused, both)
	world.job(repo, evsObserveReportTestExecuting, both)
	world.job(repo, evsObserveReportTestDone, both)
	// a dry run: an ask and a res file and no snap-0
	previewId := world.id("preview")
	world.jobObject(repo, previewId, "ask-0000000000")
	world.jobObject(repo, previewId, "res-0000000000")
	other := world.repo("meta-2", "meta-2")
	world.job(other, evsObserveReportTestRefusedVoid, evsservice.DropListedShape{})
	world.writeTables()
	world.clock.AdvanceTo(start.Add(5 * day))

	code, stdout, stderr := runEvsObserveReportTest(world.settings(), evsObserveReportTestEnv, "--days=1")
	if code != 0 || stderr != "" {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	evsObserveReportTestWants(t, stdout,
		"and 5 drop jobs of 2 repos with a drop table\n",
		"drop observe (D-TEST-18): not met: 3.0 days, 1 jobs, 1 on repos with renames and cross-ref copies (1 with renames, 1 with cross-ref copies), 2 refused by the oracle\n",
		"  2 jobs the drop oracle refused\n",
	)
}

// Owner decision 643: held entries take a table past TableByteCount, outside its bound, and the report reads it as
// evs does, counting its observed job.
func TestEvsObserveReportReadsATableHeldEntriesTakePastItsBound(t *testing.T) {
	world := newEvsObserveReportWorld(t)
	repo := world.repo("meta-1", "meta-1")
	world.job(repo, evsObserveReportTestObserved, evsservice.DropListedShape{Renames: true, CrossRefCopies: true})
	place := &protocol.EvsPlace{Minute: 29000000, Instance: 1, Seq: 2, Length: 512}
	tableByteCount := evsservice.DefaultDropSettings().TableByteCount
	for i := 0; i <= tableByteCount/128; i += 1 {
		repo.table.Drops = append(repo.table.Drops, &evsservice.DropTableEntry{
			DropId: world.id("held drop"), RefId: world.id("ref"), Place: place, State: evsservice.DropStateHeld, EndedDay: 20000,
		})
	}
	if encoded, err := evsservice.EncodeDropTable(repo.table); err != nil || len(encoded) <= tableByteCount {
		t.Fatalf("a table of %d bytes does not pass %d: %v", len(encoded), tableByteCount, err)
	}
	world.writeTables()
	world.clock.Advance(24 * time.Hour)

	code, stdout, stderr := runEvsObserveReportTest(world.settings(), evsObserveReportTestEnv, "--days=1")
	if code != 0 || stderr != "" {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	evsObserveReportTestWants(t, stdout, "and 1 drop jobs of 1 repos with a drop table\n")
}

// A drop table is read on the cluster its repo's place/ names. One on another cluster, and one of a repo place/ does
// not name, are left out of the soak and counted in a line of the report; an object under drops/ that is no table's
// key is passed over.
func TestEvsObserveReportReadsATableOnlyWherePlaceNamesIt(t *testing.T) {
	world := newEvsObserveReportWorld(t)
	both := evsservice.DropListedShape{Renames: true, CrossRefCopies: true}
	placed := world.repo("meta-2", "meta-2")
	world.job(placed, evsObserveReportTestObserved, both)
	// place/ names meta-2 and a table sits on meta-1, with a refusal that must not be counted
	elsewhere := world.repo("meta-2", "meta-1")
	world.job(elsewhere, evsObserveReportTestRefused, both)
	unplaced := world.repo("", "meta-1")
	world.job(unplaced, evsObserveReportTestObserved, both)
	world.writeTables()
	cluster, err := world.host.Blobs.Cluster("meta-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"drops/not-a-repo/_", "drops/" + sidecar.EvsKeyId(placed.repoId) + "/other"} {
		if _, err := cluster.PutIfNoneMatch(world.clusterPaidCtxs["meta-1"], key, []byte("no table"), nil); err != nil {
			t.Fatal(err)
		}
	}
	world.clock.Advance(24 * time.Hour)

	code, stdout, stderr := runEvsObserveReportTest(world.settings(), evsObserveReportTestEnv, "--days=1")
	if code != 0 || stderr != "" {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	evsObserveReportTestWants(t, stdout,
		"and 1 drop jobs of 1 repos with a drop table\n",
		"left out 2 drop tables on a cluster their repo's place/ does not name\n",
		"1.0 days, 1 jobs, 1 on repos with renames and cross-ref copies (1 with renames, 1 with cross-ref copies), 0 refused by the oracle\n",
	)
}

// Whatever a soak rests on that does not read exits 1 with its cause on stderr and no report: the env's open, a
// query, an answer with a result the counter does not have or a reading that is no count, a table or a snap-0 that
// fails its trailer, and a run past its bound. What was opened is closed.
func TestEvsObserveReportPrintsNothingWhenASourceDoesNotRead(t *testing.T) {
	both := evsservice.DropListedShape{Renames: true, CrossRefCopies: true}
	for _, testCase := range []struct {
		name    string
		arrange func(world *evsObserveReportWorld, settings *evsObserveReportSettings)
		wants   []string
		// the env is never opened
		unopened bool
	}{
		{name: "the env does not open", arrange: func(world *evsObserveReportWorld, settings *evsObserveReportSettings) {
			settings.openEnv = func(ctx context.Context, env string) (*evsObserveReportEnv, error) {
				return nil, errors.New("the vault file is missing")
			}
		}, wants: []string{"test did not open, so nothing was read", "the vault file is missing"}, unopened: true},
		{name: "the env opens as nothing", arrange: func(world *evsObserveReportWorld, settings *evsObserveReportSettings) {
			settings.openEnv = func(ctx context.Context, env string) (*evsObserveReportEnv, error) {
				return &evsObserveReportEnv{}, nil
			}
		}, wants: []string{"it opened as no env"}, unopened: true},
		{name: "a query fails", arrange: func(world *evsObserveReportWorld, settings *evsObserveReportSettings) {
			world.query = func(ctx context.Context, query string, at time.Time, maxByteCount int) ([]*monitor.MimirQuerySample, error) {
				// the third of --days=2's three, the one at the run's time
				if len(world.queries) == 3 {
					return nil, errors.New("Mimir query HTTP 503")
				}
				return nil, nil
			}
		}, wants: []string{"the observe counter did not read at 2026-01-02T03:04:05Z", "Mimir query HTTP 503"}},
		{name: "a result the counter does not have", arrange: func(world *evsObserveReportWorld, settings *evsObserveReportSettings) {
			world.query = func(ctx context.Context, query string, at time.Time, maxByteCount int) ([]*monitor.MimirQuerySample, error) {
				return []*monitor.MimirQuerySample{{Labels: map[string]string{"instance": "evs-1", "result": "kept"}, Value: 3}}, nil
			}
		}, wants: []string{"does not read", `carries result "kept"`}},
		{name: "a reading that is no count", arrange: func(world *evsObserveReportWorld, settings *evsObserveReportSettings) {
			world.query = func(ctx context.Context, query string, at time.Time, maxByteCount int) ([]*monitor.MimirQuerySample, error) {
				return []*monitor.MimirQuerySample{{Labels: map[string]string{"instance": "evs-1", "result": "clean"}, Value: 2.5}}, nil
			}
		}, wants: []string{"reads 2.5, which is no count"}},
		{name: "one series twice", arrange: func(world *evsObserveReportWorld, settings *evsObserveReportSettings) {
			world.query = func(ctx context.Context, query string, at time.Time, maxByteCount int) ([]*monitor.MimirQuerySample, error) {
				return []*monitor.MimirQuerySample{
					{Labels: map[string]string{"instance": "evs-1", "result": "clean"}, Value: 2},
					{Labels: map[string]string{"result": "clean", "instance": "evs-1", "__name__": "urnetwork_evs_versionless_observe_total"}, Value: 3},
				}, nil
			}
		}, wants: []string{"holds one series of urnetwork_evs_versionless_observe_total twice"}},
		{name: "a table that fails its trailer", arrange: func(world *evsObserveReportWorld, settings *evsObserveReportSettings) {
			repo := world.repo("meta-2", "meta-2")
			world.job(repo, evsObserveReportTestObserved, both)
			encoded, err := evsservice.EncodeDropTable(repo.table)
			if err != nil {
				world.t.Fatal(err)
			}
			encoded[len(encoded)-1] ^= 1
			cluster, err := world.host.Blobs.Cluster("meta-2")
			if err != nil {
				world.t.Fatal(err)
			}
			if _, err := cluster.PutIfNoneMatch(world.clusterPaidCtxs["meta-2"], evsservice.DropsKey(repo.repoId), encoded, nil); err != nil {
				world.t.Fatal(err)
			}
			world.repos = nil
		}, wants: []string{"drop jobs do not read", "its drop table", evsservice.ErrDropTableInvalid.Error()}},
		{name: "a snap-0 that fails its trailer", arrange: func(world *evsObserveReportWorld, settings *evsObserveReportSettings) {
			repo := world.repo("meta-1", "meta-1")
			world.job(repo, evsObserveReportTestObserved, both)
			jobId := world.id("job")
			encoded := evsservice.EncodeDropCoveredFile(repo.repoId, jobId, nil, both)
			encoded[len(encoded)-1] ^= 1
			key := evsservice.DropFileKey(repo.repoId, jobId, evsservice.DropFileSnap, 0)
			if _, err := world.segment.PutIfNoneMatch(world.ctx, key, encoded, nil); err != nil {
				world.t.Fatal(err)
			}
			world.writeTables()
		}, wants: []string{"drop jobs do not read", "'s snap-0", evsservice.ErrDropFileInvalid.Error()}},
		{name: "a run past its bound", arrange: func(world *evsObserveReportWorld, settings *evsObserveReportSettings) {
			settings.timeout = 0
		}, wants: []string{"no report of test: the run's 0s bound passed"}, unopened: true},
	} {
		world := newEvsObserveReportWorld(t)
		settings := world.settings()
		testCase.arrange(world, settings)
		code, stdout, stderr := runEvsObserveReportTest(settings, evsObserveReportTestEnv, "--days=2")
		if code != 1 || stdout != "" {
			t.Fatalf("%s: exited %d and printed %q: %s", testCase.name, code, stdout, stderr)
		}
		for _, want := range testCase.wants {
			if !strings.Contains(stderr, want) {
				t.Errorf("%s: stderr %q, want %q in it", testCase.name, stderr, want)
			}
		}
		if testCase.unopened && (world.openCount != 0 || world.closeCount != 0) {
			t.Errorf("%s: opened %d and closed %d", testCase.name, world.openCount, world.closeCount)
		}
		if !testCase.unopened && (world.openCount != 1 || world.closeCount != 1) {
			t.Errorf("%s: opened %d and closed %d, want one of each", testCase.name, world.openCount, world.closeCount)
		}
	}
}

// A command line the command cannot take exits 2 before anything is opened: no env or two, a name that is no env's,
// days outside 1 to the settings' most, and a flag it does not have.
func TestEvsObserveReportRefusesCommandLinesItCannotTake(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"test", "main"},
		{"../main"},
		{"te st"},
		{"test", "--days=0"},
		{"test", "--days=-3"},
		{"test", "--days=" + strconv.Itoa(defaultEvsObserveReportSettings().sampleMaxDays+1)},
		{"test", "--days=many"},
		{"test", "--set-on"},
	} {
		world := newEvsObserveReportWorld(t)
		code, stdout, stderr := runEvsObserveReportTest(world.settings(), args...)
		if code != 2 || stdout != "" || stderr == "" {
			t.Errorf("%q: exited %d, printed %q and %q", args, code, stdout, stderr)
		}
		if world.openCount != 0 {
			t.Errorf("%q: opened the env", args)
		}
	}
}

// The binary as built reaches one env, the one its WARP_ENV names: asked for another, or with no WARP_ENV, it exits 1
// before it loads the monitor's settings or opens a cluster, naming both envs.
func TestEvsObserveReportRefusesAnEnvThisProcessDoesNotRunAs(t *testing.T) {
	for _, testCase := range []struct {
		processEnv string
		want       string
	}{
		{processEnv: "canary", want: "WARP_ENV=canary and was asked for test"},
		{processEnv: "", want: "WARP_ENV is not set"},
	} {
		t.Setenv("WARP_ENV", testCase.processEnv)
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		if code := sidecarctlCommands.run([]string{"evs", "observe-report", "test"}, stdout, stderr); code != 1 {
			t.Fatalf("WARP_ENV=%q: exited %d, want 1: %s", testCase.processEnv, code, stderr.String())
		}
		for _, want := range []string{"nothing was read", testCase.want} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("WARP_ENV=%q: printed %q to stderr, want %q in it", testCase.processEnv, stderr.String(), want)
			}
		}
		if stdout.Len() != 0 {
			t.Errorf("WARP_ENV=%q: printed %q", testCase.processEnv, stdout.String())
		}
	}
}

// The binary's own registry holds evs observe-report beside evs purge-turn-on, under the usage its doc gives, with
// D-TEST-12's and D-TEST-18's thresholds and the drop job's own bounds as its defaults.
func TestSidecarctlRegistersEvsObserveReport(t *testing.T) {
	command, args := sidecarctlCommands.find([]string{"evs", "observe-report", "test", "--days=30"})
	if command == nil || command.words != "evs observe-report" || command.usage != "<env> [--days=<n>]" || !slices.Equal(args, []string{"test", "--days=30"}) {
		t.Fatalf("found %v with %v, want evs observe-report with its arguments", command, args)
	}
	if other, _ := sidecarctlCommands.find([]string{"evs", "purge-turn-on", "test"}); other == nil || other == command {
		t.Fatal("evs purge-turn-on is not registered beside it")
	}
	settings := defaultEvsObserveReportSettings()
	if settings.soakSettings.PurgeSoakDays != 14 || settings.soakSettings.PurgeSoakCandidates != 10000 ||
		settings.soakSettings.DropSoakDays != 30 || settings.soakSettings.DropSoakJobs != 200 || settings.soakSettings.DropSoakRenameShare != 0.5 {
		t.Fatalf("the default thresholds are %+v", settings.soakSettings)
	}
	if settings.sampleDays < 2*settings.soakSettings.DropSoakDays || settings.sampleInterval != 24*time.Hour || settings.sampleMaxDays < settings.sampleDays {
		t.Fatalf("the default samples are %d days, at most %d, %v apart", settings.sampleDays, settings.sampleMaxDays, settings.sampleInterval)
	}
	if err := settings.dropSettings.Validate(); err != nil || settings.listLimit != settings.dropSettings.ListLimit {
		t.Fatalf("the default drop settings: %v, list limit %d", err, settings.listLimit)
	}
}
