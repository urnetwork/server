package work

import (
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// The extender publish tick (connect/EXTENDER.md C4; connect/GEOMAP.md §2.8).
//
// Every ten minutes a few active extenders get a freshly signed record on the
// publish queue, oldest publish first. Nothing is published in bulk and there
// is no full sync anywhere (D6): a joining node gets a bounded sample and then
// learns the rest from this drip as it arrives, which is what keeps the whole
// directory off any single message and off any single node's memory.
//
// The drip is also the expiry mechanism. A record is valid for a day
// (controller.ExtenderRecordExpireTimeout, D19) and the batch is sized to
// rotate the whole active set within ExtenderPublishRotationTimeout, half of
// that, so a record reaches every client with at least half its life left and
// an extender that stays up is never seen expired by a connected client. The
// half is the same proportion the seven of fourteen days had; a rotation of
// the full day would put every client of an extender into the expired tier at
// once for the length of a tick.
//
// The batch alone cannot promise that: a tick that ran late, a taskworker
// that was down, or a population that grew between ticks leaves extenders
// behind the rotation. So every tick also releases every extender whose
// latest record is already older than the rotation, however many there are,
// and the rotation is a bound rather than an average. An extender the
// operator stops publishing -- revoked, or gone from every probe -- is
// selected by neither and simply expires out of every directory within a day,
// without needing a revocation.
//
// The tick is also where the geo dns sets are refreshed (C5), in
// publishExtenderDns of extender_dns_publish.go. The dns sample belongs on
// this tick rather than on one of its own because it and the drip describe the
// same directory, and two cadences would let them disagree about which
// extenders are live. The TXT records it carries are signed fresh on every
// tick, as the bootstrap sample of an activation is, so neither is ever older
// than the directory state it describes.

const (
	// How often the drip runs.
	ExtenderPublishTimeout = 10 * time.Minute

	// The smallest batch, which is what a directory of any ordinary size uses.
	ExtenderPublishMinBatchSize = 8

	// How long one full rotation of the active set may take (D19): half the
	// record ttl. An extender whose latest record is older than this is
	// released on the next tick whatever the batch.
	ExtenderPublishRotationTimeout = 12 * time.Hour

	// Ticks in the rotation window, which the batch is sized to cover.
	ExtenderPublishRotationTickCount = int(ExtenderPublishRotationTimeout / ExtenderPublishTimeout)
)

// How many extenders one tick republishes on rotation, before the stale ones
// the tick releases besides.
//
// The floor is what a small directory uses, and above the most the floor can
// rotate within the window -- 576 active extenders at eight per tick over the
// 72 ticks of twelve hours -- the batch grows with the population instead.
// Rounding is upward on purpose: a batch one short of the requirement lets the
// rotation drift past the window, and every extender it leaves behind is then
// released at once as stale rather than spread over the ticks.
func extenderPublishBatchSize(activeCount int) int {
	if activeCount <= 0 {
		return ExtenderPublishMinBatchSize
	}
	required := (activeCount + ExtenderPublishRotationTickCount - 1) / ExtenderPublishRotationTickCount
	return max(ExtenderPublishMinBatchSize, required)
}

type ExtenderPublishArgs struct {
}

type ExtenderPublishResult struct {
	// counts only; the records themselves are in the publish queue
	Active    int `json:"active"`
	BatchSize int `json:"batch_size"`
	// the batch plus every stale extender beyond it
	Selected  int `json:"selected"`
	Published int `json:"published"`
}

// The latest record time an extender may have at `now` and still wait for its
// turn in the rotation. Anything older has less than half its life left and
// goes on this tick.
func extenderPublishStaleBefore(now time.Time) time.Time {
	return now.Add(-ExtenderPublishRotationTimeout)
}

func ScheduleExtenderPublish(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleExtenderPublishAt(clientSession, tx, server.NowUtc().Add(ExtenderPublishTimeout))
}

func scheduleExtenderPublishAt(
	clientSession *session.ClientSession,
	tx server.PgTx,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		ExtenderPublish,
		&ExtenderPublishArgs{},
		clientSession,
		task.RunOnce("extender_publish"),
		task.RunAt(runAt),
	)
}

// ExtenderPublish signs and queues the next batch of records (C4).
//
// It returns nil when an individual extender could not be published, for the
// same reason the probe task does: an extender that went inactive between
// being selected and being signed is an expected race with the probe tick, not
// a fault, and a task that failed on it would stop the chain and stall the
// rotation for every extender.
func ExtenderPublish(
	_ *ExtenderPublishArgs,
	clientSession *session.ClientSession,
) (*ExtenderPublishResult, error) {
	result := &ExtenderPublishResult{}

	config, err := controller.EnvExtenderConfig()
	if err != nil {
		// no extender network in this deployment; the chain still re-arms
		return result, nil
	}
	rootPrivateKey, err := config.RootPrivateKey()
	if err != nil {
		glog.Errorf("[extenderpublish]no root key, the drip is paused: %s\n", err)
		return result, nil
	}

	result.Active = model.CountActiveNetworkExtenders(clientSession.Ctx)
	result.BatchSize = extenderPublishBatchSize(result.Active)

	extenderIds := model.GetNetworkExtenderIdsForPublish(
		clientSession.Ctx,
		result.BatchSize,
		extenderPublishStaleBefore(server.NowUtc()),
	)
	result.Selected = len(extenderIds)
	for _, extenderId := range extenderIds {
		published := model.PublishNetworkExtenderRecord(
			clientSession.Ctx,
			extenderId,
			func(
				extender *model.NetworkExtender,
				addresses []*model.NetworkExtenderAddress,
				issueTime time.Time,
			) ([]byte, error) {
				_, message, err := controller.SignExtenderRecord(
					config,
					rootPrivateKey,
					extender,
					addresses,
					issueTime,
				)
				return message, err
			},
		)
		if published {
			result.Published += 1
		}
	}

	if err := publishExtenderDns(clientSession.Ctx, config); err != nil {
		// the dns sets and the drip are independent publishers of the same
		// directory; one failing must not cost the other its tick
		glog.Errorf("[extenderpublish]dns publish failed: %s\n", err)
	}

	glog.Infof(
		"[extenderpublish]published %d of %d active extenders (batch %d, selected %d)\n",
		result.Published,
		result.Active,
		result.BatchSize,
		result.Selected,
	)
	return result, nil
}

func ExtenderPublishPost(
	_ *ExtenderPublishArgs,
	_ *ExtenderPublishResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	scheduleExtenderPublishAt(clientSession, tx, server.NowUtc().Add(ExtenderPublishTimeout))
	return nil
}
