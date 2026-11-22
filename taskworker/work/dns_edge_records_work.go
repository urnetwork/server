package work

// §12.7's hourly pass over the edge grants (owner decision 335; plan-DNS-edge-record-trigger, QD-130): each run walks
// the networks with a log through model.ReconcileDnsEdgeRecords, which reads each network's published names from the
// process's published-names source (QD-770) and writes its zone's * and @ edge flags where they differ, so a grant
// that expired loses its records and a sync a crash lost after an entry's PUT is made. A run that stops at its bound
// hands its cursor to the next, which runs at once; a run that cannot read the grants (no source yet, or a source
// that fails) writes nothing past what it read and is tried again RetryInterval on. A binary that does not link MG2
// and MG5 schedules nothing, and a process serving no root reads nothing.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Cadences of the pass.
type DnsEdgeRecordsWorkSettings struct {
	// How often a pass starts: §12.7's hour, so an expired grant's records go within an hour of its end.
	Interval time.Duration
	// How soon a run that could not read the grants is tried again.
	RetryInterval time.Duration
	// Each run's task max time.
	MaxTime time.Duration
	// The walk's batches and each run's bound.
	Reconcile *model.DnsEdgeRecordsReconcileSettings
}

func DefaultDnsEdgeRecordsWorkSettings() *DnsEdgeRecordsWorkSettings {
	return &DnsEdgeRecordsWorkSettings{
		Interval:      time.Hour,
		RetryInterval: 10 * time.Minute,
		MaxTime:       30 * time.Minute,
		Reconcile:     model.DefaultDnsEdgeRecordsReconcileSettings(),
	}
}

type DnsEdgeRecordsArgs struct {
	// Where the run starts; nil for the first network.
	AfterNetworkId *server.Id `json:"after_network_id,omitempty"`
}

type DnsEdgeRecordsResult struct {
	model.DnsEdgeRecordsReconcileResult
}

// Schedules the pass's first run an interval from now. A binary that does not link MG2 and MG5 schedules nothing.
func ScheduleDnsEdgeRecords(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleDnsEdgeRecords(clientSession, tx, &DnsEdgeRecordsArgs{}, server.NowUtc().Add(DefaultDnsEdgeRecordsWorkSettings().Interval))
}

func scheduleDnsEdgeRecords(clientSession *session.ClientSession, tx server.PgTx, args *DnsEdgeRecordsArgs, runAt time.Time) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		DnsEdgeRecords,
		args,
		clientSession,
		task.RunOnce("dns_edge_records"),
		task.RunAt(runAt),
		task.MaxTime(DefaultDnsEdgeRecordsWorkSettings().MaxTime),
	)
}

// One run of the pass at real time.
func DnsEdgeRecords(
	dnsEdgeRecords *DnsEdgeRecordsArgs,
	clientSession *session.ClientSession,
) (*DnsEdgeRecordsResult, error) {
	settings := DefaultDnsEdgeRecordsWorkSettings()
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return &DnsEdgeRecordsResult{}, nil
	}
	result, err := model.ReconcileDnsEdgeRecords(clientSession.Ctx, server.NowUtc(), dnsEdgeRecords.AfterNetworkId, settings.Reconcile)
	if err != nil {
		return nil, task.WithRetryDelay(err, settings.RetryInterval)
	}
	if (0 < result.WriteCount || 0 < result.FailedCount) && glog.V(1) {
		glog.Infof("[dns]edge records pass: %d networks read, %d zones written, %d waiting, %d failed\n", result.NetworkCount, result.WriteCount, result.WaitingCount, result.FailedCount)
	}
	return &DnsEdgeRecordsResult{DnsEdgeRecordsReconcileResult: *result}, nil
}

// Schedules the next run: at once from this run's cursor when it stopped short, else an interval from now.
func DnsEdgeRecordsPost(
	dnsEdgeRecords *DnsEdgeRecordsArgs,
	dnsEdgeRecordsResult *DnsEdgeRecordsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if dnsEdgeRecordsResult.AfterNetworkId != nil {
		scheduleDnsEdgeRecords(clientSession, tx, &DnsEdgeRecordsArgs{AfterNetworkId: dnsEdgeRecordsResult.AfterNetworkId}, server.NowUtc())
		return nil
	}
	ScheduleDnsEdgeRecords(clientSession, tx)
	return nil
}
