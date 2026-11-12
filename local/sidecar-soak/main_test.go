package main

// The soak on T2, stepped on a fake clock so each run repeats exactly: a planted goroutine leak, a planted Redis key
// leak and planted orphan segments each fail their signal, a clean short run reads back on what it reads and is no
// evidence, an orphan count GC could not make whole reads open, each signal's comparison on its own, and the stack
// target refused rather than run on fakes.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/load"
)

// A short soak on clock: phases waited through by moving it, and each service's arrivals run one after another,
// the clock moved by a gap between them.
func fakeSoakSettings(clock *sidecartest.FakeClock) *soakSettings {
	settings := shortSoakSettings()
	settings.Duration = 2 * time.Minute
	settings.Warmup = 30 * time.Second
	settings.SampleCount = 3
	settings.SampleInterval = 10 * time.Second
	settings.Subscribers = 20
	settings.Clock = clock
	settings.wait = func(ctx context.Context, wait time.Duration) error {
		clock.Advance(wait)
		return nil
	}
	settings.pace = func(ctx context.Context, service *soakService, rate float64, duration time.Duration) *load.Report {
		count := int64(rate * duration.Seconds())
		gap := time.Duration(float64(time.Second) / rate)
		report := &load.Report{
			Name: service.Name, OfferedPerSecond: rate, Scheduled: count,
			Settings: map[string]string{}, CommitSet: map[string]string{}, FailureKinds: map[string]int64{},
		}
		for range count {
			err := service.Workload(ctx, service.arrivals)
			service.arrivals += 1
			report.Arrivals += 1
			if err != nil {
				report.Failed += 1
				report.FailureKinds[err.Error()] += 1
			} else {
				report.Succeeded += 1
			}
			clock.Advance(gap)
		}
		return report
	}
	return settings
}

// The t2 target with planted services beside its mix, or in its place when alone.
type plantedSoakTarget struct {
	*t2SoakTarget
	mix []*soakService
}

func newPlantedSoakTarget(t *testing.T, settings *soakSettings, alone bool, planted func(target *t2SoakTarget) *soakService) *plantedSoakTarget {
	t.Helper()
	target, err := newT2SoakTarget(t.Context(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(target.Close)
	mix := target.Mix()
	if alone {
		mix = nil
	}
	if planted != nil {
		mix = append(mix, planted(target))
	}
	return &plantedSoakTarget{t2SoakTarget: target, mix: mix}
}

// A repo of its own on the target's first metadata cluster, placed and active as evs creates one, in a network of
// its own, for planted objects the world's commits never touch: its id and its network's.
func plantRepo(t *testing.T, target *t2SoakTarget) (sidecar.Id, sidecar.Id) {
	t.Helper()
	repoId, networkId := sidecar.NewId(), sidecar.NewId()
	first, err := target.fakes.Blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	placeBytes, err := (&sidecar.EvsRepoPlace{ClusterId: first.ClusterId(), CreatorSidecarId: sidecar.NewId()}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.PutIfNoneMatch(t.Context(), sidecar.EvsPlaceKey(repoId), placeBytes, nil); err != nil {
		t.Fatal(err)
	}
	repoBytes, err := proto.Marshal(&protocol.EvsRepoObject{
		RepoId: repoId.Bytes(), Kind: protocol.RepoKind_REPO_KIND_GENERAL, State: protocol.EvsRepoState_EVS_REPO_ACTIVE,
		Format: sidecar.EvsRepoFormat1, NetworkId: networkId.Bytes(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.PutIfNoneMatch(t.Context(), sidecar.EvsRepoObjectKey(repoId), repoBytes, nil); err != nil {
		t.Fatal(err)
	}
	return repoId, networkId
}

// Each arrival uploads two segments to repoId: one whose push never commits, an orphan once the horizon passes it,
// and one a parcel's pidx/ entry introduces, as a committed and evacuated push leaves it, which is no orphan.
func plantedUploads(target *t2SoakTarget, repoId sidecar.Id) *soakService {
	refId := sidecar.NewId()
	return &soakService{Name: "planted-uploads", CapacityPerSecond: 10, CapacitySource: "a test's", Workload: func(ctx context.Context, arrival int64) error {
		segmentCluster, err := target.fakes.Blobs.SegmentCluster()
		if err != nil {
			return err
		}
		first, err := target.fakes.Blobs.FirstCluster()
		if err != nil {
			return err
		}
		orphanBody := []byte(fmt.Sprintf("orphan %d", arrival))
		if _, err := segmentCluster.PutIfNoneMatch(ctx, evsservice.SegmentKey(repoId, sha256.Sum256(orphanBody)), orphanBody, nil); err != nil {
			return err
		}
		adoptedBody := []byte(fmt.Sprintf("adopted %d", arrival))
		adoptedId := sha256.Sum256(adoptedBody)
		if _, err := segmentCluster.PutIfNoneMatch(ctx, evsservice.SegmentKey(repoId, adoptedId), adoptedBody, nil); err != nil {
			return err
		}
		version := uint64(arrival) + 1
		containerId := sidecar.NewId()
		entryBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(&protocol.EvsPidxEntry{
			ContainerKind: protocol.EvsContainerKind_EVS_CONTAINER_PARCEL,
			ContainerId:   containerId.Bytes(),
			FirstVersion:  version,
			LastVersion:   version,
			Records: []*protocol.EvsPidxRecord{{
				Version: version, Type: protocol.EvsBatchRecordType_EVS_BATCH_PUSH, Offset: 0, Length: 200,
				Place: &protocol.EvsPlace{Minute: 1, Instance: 2, Seq: 3, Offset: sidecar.EvsBatchHeaderByteCount, Length: 200},
			}},
			SegmentIds: [][]byte{adoptedId[:]},
		})
		if err != nil {
			return err
		}
		_, err = first.PutIfNoneMatch(ctx, evsservice.PidxKey(repoId, refId, version, containerId), entryBytes, nil)
		return err
	}}
}

func (self *plantedSoakTarget) Mix() []*soakService {
	return self.mix
}

func verdictOf(t *testing.T, report *soakReport, signal soakSignal) *soakVerdict {
	t.Helper()
	for _, verdict := range report.Verdicts {
		if verdict.Signal == signal {
			return verdict
		}
	}
	t.Fatalf("no verdict for %s: %+v", signal, report)
	return nil
}

// A service that leaves one goroutine behind every arrival fails the goroutine signal, and the run.
func TestSoakFailsAPlantedGoroutineLeak(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	settings := fakeSoakSettings(clock)
	leaked := make(chan struct{})
	t.Cleanup(func() { close(leaked) })
	target := newPlantedSoakTarget(t, settings, false, func(target *t2SoakTarget) *soakService {
		return &soakService{Name: "planted-goroutines", CapacityPerSecond: 10, CapacitySource: "a test's", Workload: func(ctx context.Context, arrival int64) error {
			go func() { <-leaked }()
			return nil
		}}
	})
	report := runSoak(t.Context(), settings, target)
	if verdict := verdictOf(t, report, soakSignalGoroutines); verdict.Outcome != soakLeaked {
		t.Fatalf("a goroutine a load arrival left behind read %s: %+v", verdict.Outcome, verdict)
	}
	if report.Passed {
		t.Fatal("a leaking soak passed")
	}
}

// A service that writes a key with no ttl every arrival fails the Redis key signal, and the run.
func TestSoakFailsAPlantedKeyLeak(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	settings := fakeSoakSettings(clock)
	target := newPlantedSoakTarget(t, settings, false, func(target *t2SoakTarget) *soakService {
		return &soakService{Name: "planted-keys", CapacityPerSecond: 10, CapacitySource: "a test's", Workload: func(ctx context.Context, arrival int64) error {
			return target.fakes.Redis.Set(ctx, fmt.Sprintf("{soak-planted}%d", arrival), []byte("kept"), 0)
		}}
	})
	report := runSoak(t.Context(), settings, target)
	if verdict := verdictOf(t, report, soakSignalRedisKeys); verdict.Outcome != soakLeaked {
		t.Fatalf("keys a load left with no ttl read %s: %+v", verdict.Outcome, verdict)
	}
	if verdict := verdictOf(t, report, soakSignalGoroutines); verdict.Outcome != soakBack {
		t.Fatalf("goroutines in a key leak's run read %s: %+v", verdict.Outcome, verdict)
	}
	if report.Passed {
		t.Fatal("a leaking soak passed")
	}
}

// The evs mix alone: goroutines and keys read back, the heap open on t2, orphans back by GC's dry count, every
// commit's hints delivered, the run passes, and a short run on fakes is no evidence, its orphan count reaching
// nothing the load wrote.
func TestSoakCleanShortRunPassesAsNoEvidence(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	settings := fakeSoakSettings(clock)
	target := newPlantedSoakTarget(t, settings, false, nil)
	report := runSoak(t.Context(), settings, target)
	if !report.Passed {
		t.Fatalf("a clean soak failed: %+v", report.Verdicts)
	}
	for signal, want := range map[soakSignal]soakOutcome{
		soakSignalGoroutines: soakBack, soakSignalRedisKeys: soakBack, soakSignalHeap: soakOpen, soakSignalOrphans: soakBack,
	} {
		if verdict := verdictOf(t, report, signal); verdict.Outcome != want {
			t.Errorf("%s read %s, want %s: %+v", signal, verdict.Outcome, want, verdict)
		}
	}
	if len(report.Load) != 1 || report.Load[0].Arrivals == 0 || report.Load[0].Failed != 0 {
		t.Fatalf("the load phase: %+v", report.Load)
	}
	if !report.Smoke {
		t.Fatal("a short run is no smoke")
	}
	if report.orphanCoverage() == "" || !slices.ContainsFunc(report.Notes, func(note string) bool { return strings.HasPrefix(note, "orphan_objects compares only") }) {
		t.Fatalf("a short run's orphan count reached the load: %+v, notes %q", report.End, report.Notes)
	}
	err := report.gateEvidence()
	if err == nil || !strings.Contains(err.Error(), "a smoke that proves nothing") || !strings.Contains(err.Error(), "orphan_objects judged nothing the load wrote") {
		t.Fatalf("a short run on fakes reached a gate: %v", err)
	}
}

// Orphan GC's dry count over the planted uploads, with each quiet past the 49 h a segment takes to pass the horizon:
// the warm-up's orphans stand at the baseline, the load's join them by the end, the segments a pidx/ entry
// introduces are never counted, the signal leaks, and the end's count reaches the load. Nothing is deleted.
func TestSoakFailsAPlantedOrphanLeak(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	settings := fakeSoakSettings(clock)
	settings.Quiet = 50 * time.Hour
	target := newPlantedSoakTarget(t, settings, true, func(target *t2SoakTarget) *soakService {
		repoId, _ := plantRepo(t, target)
		return plantedUploads(target, repoId)
	})
	report := runSoak(t.Context(), settings, target)
	if len(report.Warmup) != 1 || len(report.Load) != 1 || report.Load[0].Failed != 0 {
		t.Fatalf("the phases: %+v, %+v", report.Warmup, report.Load)
	}
	warmup, loaded := report.Warmup[0].Arrivals, report.Load[0].Arrivals
	verdict := verdictOf(t, report, soakSignalOrphans)
	if verdict.Outcome != soakLeaked || verdict.Baseline != warmup || verdict.End != warmup+loaded {
		t.Fatalf("orphans %+v, after %d warm-up and %d load arrivals", verdict, warmup, loaded)
	}
	if report.Passed || report.orphanCoverage() != "" {
		t.Fatalf("passed %v, coverage %q", report.Passed, report.orphanCoverage())
	}
	segmentCluster, err := target.fakes.Blobs.SegmentCluster()
	if err != nil {
		t.Fatal(err)
	}
	if deletes := target.fakes.Blobs.RequestCount(segmentCluster.ClusterId(), sidecartest.FaultOpDelete); deletes != 0 {
		t.Fatalf("the counts issued %d DELETEs", deletes)
	}
}

// A count GC could not make whole, here with a hold on a repo's network, reads the orphan signal open with why,
// never back: what GC would not judge is no count of zero.
func TestSoakReadsOrphansOpenWhenGcWouldNotJudgeARepo(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	settings := fakeSoakSettings(clock)
	target := newPlantedSoakTarget(t, settings, false, nil)
	_, networkId := plantRepo(t, target.t2SoakTarget)
	err := sidecartest.PlaceHold(t.Context(), target.fakes.Blobs, &sidecar.HoldRecord{
		NetworkId: networkId, CaseId: sidecar.NewId(), Kind: sidecar.HoldKindPreserve2703f,
		Scope: sidecar.HoldScopeNetwork(), PlaceTime: clock.Now(), ExpireTime: clock.Now().Add(90 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	report := runSoak(t.Context(), settings, target)
	verdict := verdictOf(t, report, soakSignalOrphans)
	if verdict.Outcome != soakOpen || !strings.Contains(verdict.Why, "held") || report.Baseline.Orphans != -1 || report.End.Orphans != -1 {
		t.Fatalf("orphans %+v", verdict)
	}
	if err := report.gateEvidence(); err == nil || !strings.Contains(err.Error(), "orphan_objects read open") {
		t.Fatalf("a run whose count was not whole reached a gate: %v", err)
	}
}

// Each signal on its own: within its tolerance back, past it leaked, unread or open on its target open.
func TestCompareSoakReadsEachSignal(t *testing.T) {
	reading := func(heap uint64, goroutines int, keys int64, orphans int64) *soakReading {
		samples := []load.Sample{}
		for range 3 {
			samples = append(samples, load.Sample{HeapAllocBytes: heap, Goroutines: goroutines, RedisKeys: keys})
		}
		return &soakReading{Samples: samples, Orphans: orphans}
	}
	tolerances := defaultSoakTolerances()
	baseline := reading(100*1024*1024, 50, 40, 3)
	cases := []struct {
		end  *soakReading
		open map[soakSignal]string
		want map[soakSignal]soakOutcome
	}{
		{reading(100*1024*1024+26*1024*1024, 58, 56, 3), nil,
			map[soakSignal]soakOutcome{soakSignalHeap: soakBack, soakSignalGoroutines: soakBack, soakSignalRedisKeys: soakBack, soakSignalOrphans: soakBack}},
		{reading(100*1024*1024+27*1024*1024, 59, 57, 4), nil,
			map[soakSignal]soakOutcome{soakSignalHeap: soakLeaked, soakSignalGoroutines: soakLeaked, soakSignalRedisKeys: soakLeaked, soakSignalOrphans: soakLeaked}},
		{reading(1, 1, -1, -1), map[soakSignal]string{soakSignalHeap: "fakes"},
			map[soakSignal]soakOutcome{soakSignalHeap: soakOpen, soakSignalGoroutines: soakBack, soakSignalRedisKeys: soakOpen, soakSignalOrphans: soakOpen}},
	}
	for i, each := range cases {
		for _, verdict := range compareSoak(baseline, each.end, tolerances, each.open) {
			if verdict.Outcome != each.want[verdict.Signal] {
				t.Errorf("case %d: %s read %s, want %s: %+v", i, verdict.Signal, verdict.Outcome, each.want[verdict.Signal], verdict)
			}
		}
	}
}

// The stack target is not built, so the program refuses it and exits 2 rather than run on fakes.
func TestSoakRefusesTheUnbuiltStackTarget(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if code := run(t.Context(), []string{"-short", "-target", "stack"}, stdout, stderr); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "the stack target is not built") {
		t.Fatalf("stderr %q", stderr.String())
	}
}
