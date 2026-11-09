package taskworker

import (
	"context"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
)

// Two drives whose counters only rise by hand.
type testEvsDriveCounters struct {
	stateLock sync.Mutex
	written   uint64
}

func (self *testEvsDriveCounters) ReadDriveCounters(ctx context.Context, clusterId string) (*evsservice.DriveCounterReading, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return &evsservice.DriveCounterReading{Drives: []*evsservice.DriveCounter{
		{DriveId: "0-0-0", WrittenByteCount: self.written},
		{DriveId: "0-0-1", WrittenByteCount: self.written / 2},
	}}, nil
}

func (self *testEvsDriveCounters) write(byteCount uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.written += byteCount
}

// T3 for the loop's Redis half: the lease script, the publish and the saved state on the private test store
// through server's adapter, a fake clock stepping the loops and fake drives under them. Two loops share a
// cluster: one holds the lease and the other publishes nothing; once the holder's Run ends, its lease is
// released and the other resumes its state exactly.
func TestEvsBudgetLoopsShareALeaseOnTheTestStore(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		ctx := t.Context()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		blobs := sidecartest.NewFakeBlobs(clock, nil, nil, nil)
		host := &sidecar.ServiceHost{
			Redis:   sidecarhost.NewRedis(nil),
			Blobs:   blobs,
			Clock:   clock,
			Metrics: sidecartest.NewFakeMetrics(nil),
		}
		counters := &testEvsDriveCounters{}
		settings := evsservice.DefaultBudgetLoopSettings()

		runCtx, cancelRun := context.WithCancel(ctx)
		var lifetime sync.WaitGroup
		loops := runEvsBudgetLoops(runCtx, &lifetime, host, counters, settings)
		if len(loops.budgetLoops) != 1 || len(loops.hddLoops) != 1 {
			t.Fatalf("%d budget and %d hdd loops for one metadata and one segment cluster", len(loops.budgetLoops), len(loops.hddLoops))
		}
		first := loops.budgetLoops[0]
		// each Run steps at once and then waits on the fake clock
		if err := clock.AwaitTimers(ctx, 2); err != nil {
			t.Fatal(err)
		}
		if !first.Holding() || !loops.hddLoops[0].Holding() {
			t.Fatal("a running loop does not hold its lease")
		}

		second, err := evsservice.NewBudgetLoop(host, "meta-1", counters, nil, settings)
		if err != nil {
			t.Fatal(err)
		}
		if second.Step(ctx) != nil {
			t.Fatal("a second loop published beside the holder")
		}
		reports, err := evsservice.ReadWriteBudgetReports(ctx, host.Redis, "meta-1")
		if err != nil || len(reports) != 0 {
			t.Fatalf("reports %v %v", reports, err)
		}
		state, err := evsservice.LoadGovernorState(ctx, host.Redis, "meta-1")
		if err != nil || state == nil {
			t.Fatalf("no saved state on the store: %v", err)
		}

		// the holder's Run ends and releases the lease, so the second takes it at once and resumes the state
		cancelRun()
		lifetime.Wait()
		clock.Advance(settings.Budget.Interval)
		counters.write(300)
		allowance := second.Step(ctx)
		if allowance == nil {
			t.Fatal("the second loop did not take the released lease")
		}
		if allowance.Seq != state.Seq+1 || allowance.MeasuredByteCount != 300 {
			t.Fatalf("seq %d after %d, measured %d", allowance.Seq, state.Seq, allowance.MeasuredByteCount)
		}
		if want := min(state.BucketByteCount+allowance.DepthByteCount/720-300, allowance.DepthByteCount); allowance.BucketByteCount < want-1 || want+1 < allowance.BucketByteCount {
			t.Fatalf("B %d resumed from %d, want about %d", allowance.BucketByteCount, state.BucketByteCount, want)
		}
	})
}

// The adapter carries server's reading over unchanged.
func TestEvsDriveCounterSourceCarriesTheReading(t *testing.T) {
	info := &server.FileClusterCounterReading{
		RawUsageByteCount:    1,
		RawCapacityByteCount: 2,
		UsedInodeCount:       3,
		FreeInodeCount:       4,
		Drives: []*server.FileClusterDriveCounters{
			{DriveId: "0-0-0", WrittenByteCount: 5, WritesCompleted: 6, ReadsCompleted: 7, Healing: true},
			{DriveId: "0-0-1", Missing: true},
		},
	}
	converted := convertEvsDriveCounterReading(info)
	if !converted.UsageKnown || converted.RawUsageByteCount != 1 || converted.RawCapacityByteCount != 2 || converted.UsedInodeCount != 3 || converted.FreeInodeCount != 4 {
		t.Fatalf("usage %+v", converted)
	}
	if drive := converted.Drives[0]; drive.DriveId != "0-0-0" || drive.WrittenByteCount != 5 || drive.WritesCompleted != 6 || drive.ReadsCompleted != 7 || !drive.Healing || drive.Missing {
		t.Fatalf("drive %+v", drive)
	}
	if drive := converted.Drives[1]; !drive.Missing {
		t.Fatalf("drive %+v", drive)
	}
}
