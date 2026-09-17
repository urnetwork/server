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

// The extender publish tick (connect/EXTENDER.md C4).
//
// Every ten minutes a few active extenders get a freshly signed record on the
// publish queue, oldest publish first. Nothing is published in bulk and there
// is no full sync anywhere (D6): a joining node gets a bounded sample and then
// learns the rest from this drip as it arrives, which is what keeps the whole
// directory off any single message and off any single node's memory.
//
// The drip is also the expiry mechanism. A record is valid for fourteen days
// and the rotation is sized to come back round within seven, so an extender
// that stays up is always republished with more than half its validity left,
// and one the operator stops publishing simply expires out of every directory
// without needing a revocation.
//
// The tick is also where the geo dns sets are refreshed (C5), in
// publishExtenderDns of extender_dns_publish.go. The dns sample belongs on
// this tick rather than on one of its own because it and the drip describe the
// same directory, and two cadences would let them disagree about which
// extenders are live.

const (
	// How often the drip runs.
	ExtenderPublishTimeout = 10 * time.Minute

	// The smallest batch, which is what a directory of any ordinary size uses.
	ExtenderPublishMinBatchSize = 8

	// Ticks in the rotation window: seven days at one tick every ten minutes.
	// The batch is sized so the whole active set is republished within it, so
	// no record reaches its fourteen day expiry while its extender is up.
	ExtenderPublishRotationTickCount = 7 * 24 * 60 / 10
)

// extenderPublishBatchSize is how many extenders one tick republishes.
//
// The floor is what a small directory uses, and above about eight thousand
// active extenders -- the most the floor can rotate within the window -- the
// batch grows with the population instead. Rounding is upward on purpose: a
// batch one short of the requirement lets the rotation drift past the window
// and, eventually, past a record's expiry.
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
	Published int `json:"published"`
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

	for _, extenderId := range model.GetNetworkExtenderIdsForPublish(
		clientSession.Ctx,
		result.BatchSize,
	) {
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
		"[extenderpublish]published %d of %d active extenders (batch %d)\n",
		result.Published,
		result.Active,
		result.BatchSize,
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
