package controller

// The certs worker's command for a delegated DNS-01 challenge (QD-1165; QD-175, QD-027; §12.8, §13.8; DESIGN-DETAILS
// 17.2.9): what POST and GET /tls/custom-challenge hand over, run on taskworker's queue, where certs' DNS client and its
// internal-certs bearer are. One command a receipt, keyed by the receipt's opaque id alone, so no task row names a
// domain, a name or a digest (I-13); a capture or a readiness ask queues it in its own transaction.
//
// Each run takes at most WorkStepMaxCount steps of model.TlsCustomStore.ChallengeWorkStep, each one transaction that
// settles an unheard dispatch from the zone journal, rechecks the claim and the name's hourly window, and answers at
// most one DNS call, made outside any transaction: the write, through CertsDns.WriteAcmeChallenge with the one digest,
// or the read, CertsDns.Ready of the serial the write took. What the call answered is recorded in a transaction of its
// own (RecordChallengeWrite, RecordChallengeReadiness). The command is bounded in steps, in run time (WorkMaxTime) and
// in dispatches (DispatchMaxCount), and ends with the receipt's retention at the latest.
//
// The DNS write takes no idempotency key: POST /dns/internal/acme-challenge carries the name and the values alone, and
// each write takes a new serial. A lost acknowledgement, an answer that never came, a 503 or a run stopped mid-call,
// leaves the dispatch unheard, and the next step settles it from the zone journal under the zone's lock: a commit of
// the digest at the name above the root's serial the dispatch recorded is the write, adopted with its serial and not
// charged again. With none, the step waits until SettleAfter past the dispatch, by a run the post queues at that time,
// before it owes the write again (model's tls_custom_challenge_model.go).
//
// What it runs on is installed per process (SetTlsCustomChallengeWorker), as the chains' is: taskworker's certs
// install sets it while certs reaches the dns api. A process with nothing installed leaves every receipt as it is,
// and the next capture or readiness ask queues the command again.

import (
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar/certsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The handoff's barriers (§22.8), which a T2 or T3 schedule holds: after POST's capture commits, as a run of the
// worker's command starts, before its write and before its readiness read go to the dns api, and after GET's ask
// commits. Called with the receipt's id; nil in production.
var Testing_TlsCustomChallengeBarrier func(name string, challengeId server.Id)

// The barrier names.
const (
	TlsCustomChallengeBarrierCaptured   = "controller.tls_custom_challenge.captured"
	TlsCustomChallengeBarrierRun        = "controller.tls_custom_challenge.run"
	TlsCustomChallengeBarrierWrite      = "controller.tls_custom_challenge.write"
	TlsCustomChallengeBarrierReady      = "controller.tls_custom_challenge.ready"
	TlsCustomChallengeBarrierReadyAsked = "controller.tls_custom_challenge.ready_asked"
)

func tlsCustomChallengeBarrier(name string, challengeId server.Id) {
	if barrier := Testing_TlsCustomChallengeBarrier; barrier != nil {
		barrier(name, challengeId)
	}
}

// What the command runs on. Store and Dns are required.
type TlsCustomChallengeWorkerDeps struct {
	Store *model.TlsCustomStore
	// certs' DNS client, on taskworker's internal-certs bearer (QD-027).
	Dns certsservice.CertsDns
	// nil takes model.DefaultTlsCustomChallengeSettings.
	Settings *model.TlsCustomChallengeSettings
}

func (self *TlsCustomChallengeWorkerDeps) settings() *model.TlsCustomChallengeSettings {
	if self.Settings != nil {
		return self.Settings
	}
	return model.DefaultTlsCustomChallengeSettings()
}

var tlsCustomChallengeWorkerDeps atomic.Pointer[TlsCustomChallengeWorkerDeps]

// Installs what this process's command runs on, answering what it installed, nil when it removed: the install that
// put it there removes only that (UnsetTlsCustomChallengeWorker). nil, deps missing a required field, or settings that
// do not validate remove it.
func SetTlsCustomChallengeWorker(deps *TlsCustomChallengeWorkerDeps) *TlsCustomChallengeWorkerDeps {
	if deps == nil || deps.Store == nil || deps.Dns == nil || (deps.Settings != nil && deps.Settings.Validate() != nil) {
		tlsCustomChallengeWorkerDeps.Store(nil)
		return nil
	}
	tlsCustomChallengeWorkerDeps.Store(deps)
	return deps
}

// Removes deps, if it is still what is installed.
func UnsetTlsCustomChallengeWorker(deps *TlsCustomChallengeWorkerDeps) {
	if deps != nil {
		tlsCustomChallengeWorkerDeps.CompareAndSwap(deps, nil)
	}
}

type TlsCustomChallengeWorkArgs struct {
	ChallengeId server.Id `json:"challenge_id"`
}

type TlsCustomChallengeWorkResult struct {
	// When the command runs again: an unheard dispatch's settle time, or at once with steps still owed. Zero for no
	// run owed.
	RunAt time.Time `json:"run_at,omitempty"`
	// The DNS calls the run made.
	WriteCount int `json:"write_count"`
	ReadyCount int `json:"ready_count"`
}

// The receipt's command, queued in tx at runAt, zero for now. A command already queued for the receipt keeps its
// earlier time, and one running owns one successor (task.RunOnce).
func scheduleTlsCustomChallengeWorkInTx(tx server.PgTx, clientSession *session.ClientSession, challengeId server.Id, runAt time.Time, settings *model.TlsCustomChallengeSettings) {
	options := []any{
		task.RunOnce("tls_custom_challenge", challengeId),
		task.MaxTime(settings.WorkMaxTime),
		task.Priority(task.TaskPriorityFastest),
	}
	if !runAt.IsZero() {
		options = append(options, task.RunAt(runAt))
	}
	task.ScheduleTaskInTx(tx, TlsCustomChallengeWork, &TlsCustomChallengeWorkArgs{ChallengeId: challengeId}, clientSession, options...)
}

// One run of the command (see the file header).
func TlsCustomChallengeWork(args *TlsCustomChallengeWorkArgs, clientSession *session.ClientSession) (*TlsCustomChallengeWorkResult, error) {
	result := &TlsCustomChallengeWorkResult{}
	deps := tlsCustomChallengeWorkerDeps.Load()
	if deps == nil || args == nil {
		// no DNS client here: the receipt waits for a capture or an ask to queue the command again
		return result, nil
	}
	settings := deps.settings()
	ctx := clientSession.Ctx
	tlsCustomChallengeBarrier(TlsCustomChallengeBarrierRun, args.ChallengeId)
	for range settings.WorkStepMaxCount {
		step, err := deps.Store.ChallengeWorkStep(ctx, args.ChallengeId, settings)
		if err != nil {
			return nil, err
		}
		switch {
		case step.WriteName != "":
			tlsCustomChallengeBarrier(TlsCustomChallengeBarrierWrite, args.ChallengeId)
			serial, writeErr := deps.Dns.WriteAcmeChallenge(ctx, step.WriteName, []string{step.WriteValue})
			result.WriteCount += 1
			if writeErr != nil {
				// the error names the route, never the name (I-13)
				glog.Infof("[tls]custom challenge %s write %d unanswered or refused: %v\n", args.ChallengeId, step.DispatchCount, writeErr)
			}
			if err := deps.Store.RecordChallengeWrite(ctx, args.ChallengeId, step.DispatchCount, serial, writeErr); err != nil {
				return nil, err
			}
		case step.ReadySerial != 0:
			tlsCustomChallengeBarrier(TlsCustomChallengeBarrierReady, args.ChallengeId)
			readiness, err := deps.Dns.Ready(ctx, step.ReadySerial)
			result.ReadyCount += 1
			if err != nil {
				// nothing to record: GET answers waiting, and its next ask queues another read
				glog.Infof("[tls]custom challenge %s ready read failed: %v\n", args.ChallengeId, err)
				return result, nil
			}
			if err := deps.Store.RecordChallengeReadiness(ctx, args.ChallengeId, step.ReadySerial, string(readiness)); err != nil {
				return nil, err
			}
		case !step.WaitUntil.IsZero():
			result.RunAt = step.WaitUntil
			return result, nil
		default:
			return result, nil
		}
	}
	// steps still owed: the next run takes them at once
	result.RunAt = server.NowUtc()
	return result, nil
}

// Queues the next run the result asks for.
func TlsCustomChallengeWorkPost(args *TlsCustomChallengeWorkArgs, result *TlsCustomChallengeWorkResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if args == nil || result == nil || result.RunAt.IsZero() {
		return nil
	}
	settings := model.DefaultTlsCustomChallengeSettings()
	if deps := tlsCustomChallengeWorkerDeps.Load(); deps != nil {
		settings = deps.settings()
	}
	scheduleTlsCustomChallengeWorkInTx(tx, clientSession, args.ChallengeId, result.RunAt, settings)
	return nil
}
