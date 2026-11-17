package work

// Pay with Gamma's task (mmm/BTCPAY.md §18): one chain, RunOnce
// "gamma_biller", runs the biller and the watcher every few seconds, so one
// process owns the billing key's nonce. A thin shell: the work is in
// controller/gamma_biller.go. While gamma.yml is not present the chain backs
// off, doubling up to cryptoPayIdleMaxDelay, so a vault fix is picked up
// within minutes without a restart.

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A run is abandoned after this long.
const gammaBillerTaskMaxTime = 10 * time.Minute

type GammaBillerArgs struct {
	// the runs in a row that found Gamma not configured
	Idle int `json:"idle,omitempty"`
}

type GammaBillerResult = controller.GammaBillerResult

// ScheduleGammaBiller starts the biller's chain.
func ScheduleGammaBiller(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleGammaBiller(clientSession, tx, &GammaBillerArgs{}, server.NowUtc())
}

func scheduleGammaBiller(clientSession *session.ClientSession, tx server.PgTx, args *GammaBillerArgs, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		GammaBiller,
		args,
		clientSession,
		task.RunOnce("gamma_biller"),
		task.RunAt(runAt),
		task.MaxTime(gammaBillerTaskMaxTime),
	)
}

// GammaBiller is one run of the biller and the watcher.
func GammaBiller(args *GammaBillerArgs, clientSession *session.ClientSession) (*GammaBillerResult, error) {
	return controller.RunGammaBiller(clientSession)
}

// GammaBillerPost schedules the next run: after the run interval, or later
// while Gamma is not configured.
func GammaBillerPost(
	args *GammaBillerArgs,
	result *GammaBillerResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	next := &GammaBillerArgs{}
	delay := 12 * time.Second
	if result != nil && 0 < result.Interval {
		delay = result.Interval
	}
	if result == nil || !result.Configured {
		next.Idle = args.Idle + 1
		delay = cryptoPayIdleDelay(delay, next.Idle)
	}
	scheduleGammaBiller(clientSession, tx, next, server.NowUtc().Add(delay))
	return nil
}
