package work

// §19.10's labels step (DNS-M5-16; §12.5, D-DNS-10, I-14, FS-41, FS-42): after a Postgres restore and the dns
// restore step, dns_ct_replay re-reads CT for the root's names logged since the sidecar dump's time less 7 days,
// through every CT log client the process installs (SetDnsCtLogs: Cert Spotter's and crt.sh's), and hands their
// union to model.ReplayDnsCtNames, which inserts each label dns_label lacks as retiring with no network and records
// each leaf's SHA-256 under the incident. A replay must be whole to hold I-14, so a log that fails, or a process
// with none installed, fails the run, and the task retries until every log answers; the 90-day tombstones run in
// dns_maintenance (dns_work.go).
//
// The restore's frame (OPS-M3-03, §19.10) schedules one replay a root and incident, once, through
// ScheduleDnsCtReplay.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A CT log's view the replay reads (§12.5): every certificate logged since since that names a name under root,
// each name with its leaf's SHA-256. Cert Spotter's and crt.sh's clients each implement it. Safe for concurrent use.
type DnsCtLog interface {
	// The log's name, for the run's log line.
	Name() string
	Names(ctx context.Context, root string, since time.Time) ([]*model.DnsCtName, error)
}

// The process's CT log clients.
var dnsCtLogs = struct {
	stateLock sync.Mutex
	logs      []DnsCtLog
}{}

// Installs the CT log clients every replay reads, replacing any before. None leaves every replay failing.
func SetDnsCtLogs(logs ...DnsCtLog) {
	dnsCtLogs.stateLock.Lock()
	defer dnsCtLogs.stateLock.Unlock()
	dnsCtLogs.logs = append([]DnsCtLog{}, logs...)
}

func currentDnsCtLogs() []DnsCtLog {
	dnsCtLogs.stateLock.Lock()
	defer dnsCtLogs.stateLock.Unlock()
	return append([]DnsCtLog{}, dnsCtLogs.logs...)
}

// The replay's tunables.
type DnsCtReplaySettings struct {
	// How far before the sidecar dump's time the replay reads CT: §19.10's 7 days, which outlasts a log's maximum
	// merge delay and an issuance the dump missed by being in flight.
	DumpTimeMargin time.Duration
	// A failed run's retry delay.
	RetryInterval time.Duration
	// Each run's task max time.
	MaxTime time.Duration
}

func DefaultDnsCtReplaySettings() *DnsCtReplaySettings {
	return &DnsCtReplaySettings{
		DumpTimeMargin: 7 * 24 * time.Hour,
		RetryInterval:  5 * time.Minute,
		MaxTime:        30 * time.Minute,
	}
}

// No CT log client is installed, so no replay can be whole.
var ErrDnsCtNoLogs = errors.New("dns ct replay: no ct log client is installed")

type DnsCtReplayArgs struct {
	Root       string    `json:"root"`
	IncidentId server.Id `json:"incident_id"`
	// The sidecar dump's time (§19.10), less DumpTimeMargin the replay reads from.
	DumpTime time.Time `json:"dump_time"`
}

type DnsCtReplayResult struct {
	NameCount     int  `json:"name_count"`
	InsertedCount int  `json:"inserted_count"`
	Failed        bool `json:"failed"`
}

// Schedules root's replay for incident, once an incident and root.
func ScheduleDnsCtReplay(clientSession *session.ClientSession, tx server.PgTx, args *DnsCtReplayArgs) {
	if !server.SidecarMg5Linked() {
		return
	}
	scheduleDnsCtReplay(clientSession, tx, args, server.NowUtc())
}

func scheduleDnsCtReplay(clientSession *session.ClientSession, tx server.PgTx, args *DnsCtReplayArgs, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		DnsCtReplay,
		args,
		clientSession,
		task.RunOnce("dns_ct_replay", args.Root, args.IncidentId),
		task.RunAt(runAt),
		task.MaxTime(DefaultDnsCtReplaySettings().MaxTime),
	)
}

// Reads every installed log since the dump's time less the margin and replays their union. A log that fails fails
// the run, which DnsCtReplayPost reschedules.
func DnsCtReplay(args *DnsCtReplayArgs, clientSession *session.ClientSession) (*DnsCtReplayResult, error) {
	if !server.SidecarMg5Linked() {
		return &DnsCtReplayResult{}, nil
	}
	return runDnsCtReplay(clientSession.Ctx, args, currentDnsCtLogs(), server.NowUtc())
}

func runDnsCtReplay(ctx context.Context, args *DnsCtReplayArgs, logs []DnsCtLog, now time.Time) (*DnsCtReplayResult, error) {
	result := &DnsCtReplayResult{}
	if len(logs) == 0 {
		glog.Errorf("[dns]ct replay of incident %s waits: %v\n", args.IncidentId, ErrDnsCtNoLogs)
		result.Failed = true
		return result, nil
	}
	since := args.DumpTime.Add(-DefaultDnsCtReplaySettings().DumpTimeMargin)
	names := []*model.DnsCtName{}
	for _, log := range logs {
		logNames, err := log.Names(ctx, args.Root, since)
		if err != nil {
			glog.Warningf("[dns]ct replay of incident %s: %s failed, the run retries: %v\n", args.IncidentId, log.Name(), err)
			result.Failed = true
			return result, nil
		}
		names = append(names, logNames...)
	}
	result.NameCount = len(names)
	insertedCount, err := model.ReplayDnsCtNames(ctx, args.Root, args.IncidentId, names, now)
	result.InsertedCount = insertedCount
	if err != nil {
		glog.Warningf("[dns]ct replay of incident %s, the run retries: %v\n", args.IncidentId, err)
		result.Failed = true
	}
	return result, nil
}

// Reschedules a run that did not finish.
func DnsCtReplayPost(
	args *DnsCtReplayArgs,
	result *DnsCtReplayResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if result.Failed {
		scheduleDnsCtReplay(clientSession, tx, args, server.NowUtc().Add(DefaultDnsCtReplaySettings().RetryInterval))
	}
	return nil
}
