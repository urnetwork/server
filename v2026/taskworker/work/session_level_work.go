// The session level sample: urnetwork_session_live_per_network, one
// observation per network with a live session set, once a period.
package work

import (
	"context"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// A pass runs at every quarter hour. Each pass reads the 32 maintenance index
// shards (every network with session state) and two counts per network, all
// reads: at a million networks, about a million pipelined ZCOUNT pairs a
// pass, about 1,100 a second spread over the cluster.
const SampleSessionLevelsPeriod = 15 * time.Minute

// A pass the budget cuts short is partial (passes_total{outcome="partial"});
// the next pass starts from the first shard again.
const sampleSessionLevelsBudget = 10 * time.Minute

type SampleSessionLevelsArgs struct{}

type SampleSessionLevelsResult struct {
	Observed int  `json:"observed"`
	Complete bool `json:"complete"`
}

// The next quarter hour after now, so passes are a fixed period apart.
func nextSampleSessionLevelsTime(now time.Time) time.Time {
	return now.Truncate(SampleSessionLevelsPeriod).Add(SampleSessionLevelsPeriod)
}

func ScheduleSampleSessionLevels(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		SampleSessionLevels,
		&SampleSessionLevelsArgs{},
		clientSession,
		task.RunOnce("sample_session_levels"),
		task.RunAt(nextSampleSessionLevelsTime(server.NowUtc())),
		task.MaxTime(sampleSessionLevelsBudget+time.Minute),
	)
}

func SampleSessionLevels(_ *SampleSessionLevelsArgs, clientSession *session.ClientSession) (*SampleSessionLevelsResult, error) {
	ctx, cancel := context.WithTimeout(clientSession.Ctx, sampleSessionLevelsBudget)
	defer cancel()
	observed, complete, err := session.SampleSessionLevels(ctx, server.NowUtc())
	if err != nil {
		glog.Infof("[session]level sample stopped after %d networks: %s\n", observed, err)
	}
	// a pass that stopped is reported, never retried within the period
	return &SampleSessionLevelsResult{Observed: observed, Complete: complete}, nil
}

func SampleSessionLevelsPost(_ *SampleSessionLevelsArgs, _ *SampleSessionLevelsResult, clientSession *session.ClientSession, tx server.PgTx) error {
	ScheduleSampleSessionLevels(clientSession, tx)
	return nil
}
