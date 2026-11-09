package taskworker

// §19.6's budget loop per metadata cluster (EVSB-M2-07) and hdd loop per segment cluster (EVSB-M2-08): each
// taskworker process starts an evsservice.BudgetLoop for every metadata cluster its vault file names and an
// evsservice.HddLoop for its segment cluster, and each loop holds its cluster's lease in Redis, so however
// many taskworker processes run, one steps each cluster and another takes over within the lease's ttl. They
// start inside startTaskworkerRuntime and join its lifetime group, ending with its context, the lease
// released. The hdd loop's capacity comes from sidecar.yml's file_clusters entry, through the host's gates.
//
// They start only where vault holds file-clusters-taskworker.yml: a deployment with no file clusters, today's
// production among them, starts none, and a file that does not load starts none and says so. The loops read no
// Postgres and touch Redis only in {nvb_<set>}.

import (
	"context"
	"sync"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
)

// The vault file's service: file-clusters-taskworker.yml.
const evsBudgetFileClustersService = "taskworker"

// What startEvsBudgetLoops runs: a test replaces it.
var startEvsBudgetLoops = startEvsBudgetLoopsFromVault

// Loads the file clusters and starts a loop per metadata cluster on lifetime, each ending with ctx. Returns
// once they are started; a vault without the file starts nothing.
func startEvsBudgetLoopsFromVault(ctx context.Context, lifetime *sync.WaitGroup) {
	clusters, err := server.LoadFileClusters(ctx, evsBudgetFileClustersService, nil)
	if err != nil {
		glog.Infof("[evsbudget]no file clusters, so no budget loop runs: %v\n", err)
		return
	}
	lifetime.Go(func() {
		<-ctx.Done()
		clusters.Close()
	})
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{Blobs: clusters})
	runEvsBudgetLoops(ctx, lifetime, host, newEvsDriveCounterSource(server.NewFileClusterCounters(clusters, nil)), nil)
}

// The loops runEvsBudgetLoops started.
type evsBudgetLoops struct {
	budgetLoops []*evsservice.BudgetLoop
	hddLoops    []*evsservice.HddLoop
}

// A budget loop per metadata cluster of host.Blobs and an hdd loop for its segment cluster, each run on
// lifetime until ctx ends. settings may be nil for the defaults.
func runEvsBudgetLoops(ctx context.Context, lifetime *sync.WaitGroup, host *sidecar.ServiceHost, counters evsservice.DriveCounterSource, settings *evsservice.BudgetLoopSettings) *evsBudgetLoops {
	loops := &evsBudgetLoops{}
	for _, clusterId := range host.Blobs.MetadataClusterIds() {
		loop, err := evsservice.NewBudgetLoop(host, clusterId, counters, evsBudgetLogger{}, settings)
		if err != nil {
			glog.Errorf("[evsbudget]%s: no budget loop: %v\n", clusterId, err)
			continue
		}
		loops.budgetLoops = append(loops.budgetLoops, loop)
		lifetime.Go(func() {
			server.HandleError(func() {
				loop.Run(ctx)
			})
		})
	}
	if segmentCluster, err := host.Blobs.SegmentCluster(); err == nil {
		loop, err := evsservice.NewHddLoop(host, segmentCluster.ClusterId(), counters, evsBudgetLogger{}, nil)
		if err != nil {
			glog.Errorf("[evsbudget]%s: no hdd loop: %v\n", segmentCluster.ClusterId(), err)
		} else {
			loops.hddLoops = append(loops.hddLoops, loop)
			lifetime.Go(func() {
				server.HandleError(func() {
					loop.Run(ctx)
				})
			})
		}
	}
	return loops
}

// server.FileClusterCounters as the loop's source, so server's root links no engine.
type evsDriveCounterSource struct {
	counters *server.FileClusterCounters
}

func newEvsDriveCounterSource(counters *server.FileClusterCounters) *evsDriveCounterSource {
	return &evsDriveCounterSource{counters: counters}
}

func (self *evsDriveCounterSource) ReadDriveCounters(ctx context.Context, clusterId string) (*evsservice.DriveCounterReading, error) {
	reading, err := self.counters.ReadDriveCounters(ctx, clusterId)
	if err != nil {
		return nil, err
	}
	return convertEvsDriveCounterReading(reading), nil
}

// server's reading as the loop's, admin info's use known since a reading needs fresh admin info.
func convertEvsDriveCounterReading(reading *server.FileClusterCounterReading) *evsservice.DriveCounterReading {
	converted := &evsservice.DriveCounterReading{
		UsageKnown:           true,
		RawUsageByteCount:    reading.RawUsageByteCount,
		RawCapacityByteCount: reading.RawCapacityByteCount,
		UsedInodeCount:       reading.UsedInodeCount,
		FreeInodeCount:       reading.FreeInodeCount,
	}
	for _, drive := range reading.Drives {
		converted.Drives = append(converted.Drives, &evsservice.DriveCounter{
			DriveId:          drive.DriveId,
			WrittenByteCount: drive.WrittenByteCount,
			WritesCompleted:  drive.WritesCompleted,
			ReadsCompleted:   drive.ReadsCompleted,
			Healing:          drive.Healing,
			Missing:          drive.Missing,
		})
	}
	return converted
}

// glog as the loops' sidecar.Logger.
type evsBudgetLogger struct{}

func (self evsBudgetLogger) Info(args ...any) {
	glog.Info(args...)
}

func (self evsBudgetLogger) Infof(format string, args ...any) {
	glog.Infof(format, args...)
}

func (self evsBudgetLogger) Warningf(format string, args ...any) {
	glog.Warningf(format, args...)
}

func (self evsBudgetLogger) Errorf(format string, args ...any) {
	glog.Errorf(format, args...)
}

func (self evsBudgetLogger) V(level int32) sidecar.Verbose {
	return evsBudgetVerbose(glog.V(glog.Level(level)))
}

// glog's verbose level as the loops' sidecar.Verbose.
type evsBudgetVerbose glog.Verbose

func (self evsBudgetVerbose) Enabled() bool {
	return bool(self)
}

func (self evsBudgetVerbose) Info(args ...any) {
	glog.Verbose(self).Info(args...)
}

func (self evsBudgetVerbose) Infof(format string, args ...any) {
	glog.Verbose(self).Infof(format, args...)
}
