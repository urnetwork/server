package work

import (
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The public seed chain (connect/EXTENDER-UPDATE2.md §2): every ten minutes,
// one publisher pass (extender_seed_publish.go) and one check of every mirror
// (extender_seed_mirror_check.go).
//
// The chain is always scheduled and does nothing while the seed block of
// `extender.yml` is off, which is the default; it reports only that state
// (extender_seed_metrics.go). The run-once key keeps one pass at a time across
// the fleet, and a pass never fails its task for a publish or a mirror that
// failed: the outcome is counted and the chain re-arms, as the drip's does.

const (
	// How often the chain runs.
	ExtenderSeedTimeout = 10 * time.Minute

	// The longest one pass may run: the store reads and write, eight record
	// reads and signatures, and the mirror fetches at their timeouts.
	ExtenderSeedMaxTime = 5 * time.Minute
)

type ExtenderSeedArgs struct {
}

// Counts and codes only; no record, address or key.
type ExtenderSeedResult struct {
	Enabled        bool   `json:"enabled"`
	Outcome        string `json:"outcome,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Sequence       uint64 `json:"sequence,omitempty"`
	Records        int    `json:"records,omitempty"`
	Revocations    int    `json:"revocations,omitempty"`
	MirrorRoutes   int    `json:"mirror_routes,omitempty"`
	MirrorRoutesUp int    `json:"mirror_routes_up,omitempty"`
}

func ScheduleExtenderSeed(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleExtenderSeedAt(clientSession, tx, server.NowUtc().Add(ExtenderSeedTimeout))
}

func scheduleExtenderSeedAt(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		ExtenderSeed,
		&ExtenderSeedArgs{},
		clientSession,
		task.RunOnce("extender_seed"),
		task.RunAt(runAt),
		task.MaxTime(ExtenderSeedMaxTime),
	)
}

// ExtenderSeed is one pass of the seed chain.
func ExtenderSeed(
	_ *ExtenderSeedArgs,
	clientSession *session.ClientSession,
) (*ExtenderSeedResult, error) {
	result := &ExtenderSeedResult{}
	now := server.NowUtc()

	config, err := controller.EnvExtenderConfig()
	if err != nil || !config.Seed.Enabled {
		extenderSeedObservations.observeEnabled(false, now)
		return result, nil
	}
	result.Enabled = true
	extenderSeedObservations.observeEnabled(true, now)

	runExtenderSeedPass(clientSession, config, now, result)
	return result, nil
}

// The publish and the mirror check of one enabled pass.
func runExtenderSeedPass(
	clientSession *session.ClientSession,
	config *controller.ExtenderConfig,
	now time.Time,
	result *ExtenderSeedResult,
) {
	ctx := clientSession.Ctx

	store, ok := server.LoadBlobStore()
	if !ok {
		result.Outcome = extenderSeedOutcomeStore
	} else if publisher, err := newExtenderSeedPublisher(config, store); err != nil {
		glog.Errorf("[extenderseed]the extender configuration cannot sign a seed bundle: %s\n", err)
		result.Outcome = extenderSeedOutcomeConfig
	} else {
		published := publisher.publish(ctx, now)
		result.Outcome = published.outcome
		result.Reason = published.reason
		if published.stored != nil {
			result.Sequence = published.stored.Sequence
			result.Records = len(published.stored.Records)
			result.Revocations = len(published.stored.Revocations)
		}
		extenderSeedObservations.observePublished(published.stored, now)
		if published.outcome == extenderSeedOutcomePublished {
			glog.Infof(
				"[extenderseed]published sequence %d with %d records and %d revocations (%s)\n",
				result.Sequence,
				result.Records,
				result.Revocations,
				published.reason,
			)
		}
	}
	extenderSeedPublishesCounter.WithLabelValues(result.Outcome).Inc()

	routes, err := config.Seed.MirrorRoutes()
	if err != nil {
		glog.Errorf("[extenderseed]the seed mirrors are not checked: %s\n", err)
		return
	}
	rootKeys, err := connect.NewExtenderRootKeySetFromHex(config.RootPublicKeys()...)
	if err != nil {
		glog.Errorf("[extenderseed]the seed mirrors are not checked: the root keys do not parse\n")
		return
	}
	networkHosts := append([]string{config.NetworkHost}, config.NetworkHosts...)
	for _, mirrorResult := range checkExtenderSeedMirrors(ctx, routes, rootKeys, networkHosts, now) {
		result.MirrorRoutes += 1
		if mirrorResult.observation.up {
			result.MirrorRoutesUp += 1
		}
	}
}

func ExtenderSeedPost(
	_ *ExtenderSeedArgs,
	_ *ExtenderSeedResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleExtenderSeed(clientSession, tx)
	return nil
}
