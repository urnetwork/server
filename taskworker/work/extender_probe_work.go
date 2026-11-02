package work

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Extender uptime probes (connect/EXTENDER.md C3).
//
// Every five minutes each active address is dialed over its tcp carrier with a
// fresh challenge. That is the whole liveness test: an address that cannot sign
// bytes chosen now is not serving, whatever its record says. The dial is the
// same one a client makes, so what passes here is what a client can use.
//
// Removal is on consecutive failures, not on any failure and not on a sum. Six
// in a row -- half an hour of being unreachable -- deactivates the address, and
// a single success anywhere in that window clears the count. A home connection
// that drops for a minute keeps its place; one that is gone loses it.
//
// Losing the last address revokes the extender, which is the only way a record
// is withdrawn before it expires. The revocation is signed and queued in the
// same transaction that deactivates the address, so the directory cannot learn
// the extender is gone without also getting the proof.
//
// The probe is also the operator's half of the per-country blocked state (R4,
// connect/EXTENDER-UPDATE2.md §1.3): a record is blocked only while every
// active address answers it, and a failed probe sets aside the block reports
// made before it, since those describe the outage rather than a block
// (model.DiscountNetworkExtenderBlockReports).
//
// The task never fails on probe failures. They are what it went to find out;
// returning them as an error would stop the chain re-arming and the whole
// directory would silently stop being checked.

const (
	// How often the whole active set is probed.
	ExtenderProbeTimeout = 5 * time.Minute

	// One address's budget. Generous, because a slow extender is still a
	// working one, and the concurrency below is what bounds the pass.
	ExtenderProbeAddressTimeout = 15 * time.Second

	// Addresses probed at once. The work is network wait, not cpu, and the
	// bound exists so a large directory does not open thousands of sockets
	// from one process at once.
	ExtenderProbeConcurrency = 16

	// Consecutive failures that deactivate an address.
	ExtenderMaxConsecutiveProbeFailures = 6

	// How long one pass may run before its context ends. A pass takes about
	// half a minute while most addresses answer, but each address that does not
	// answer holds a worker for ExtenderProbeAddressTimeout, so an outage across
	// 430 addresses takes about seven minutes, which the task default of two
	// would cut off every time.
	ExtenderProbeMaxTime = 10 * time.Minute
)

// probeExtenderAddress dials one address's tcp carrier and requires a
// challenge signature under the extender's published key.
//
// It is a variable so a test can drive the task's accounting -- the failure
// budget, the deactivation, the revocation -- deterministically, without
// standing up an extender for every case and without waiting on real dials.
// Production never replaces it; the real dial is proved against an in-process
// extender by the activation tests, which run the same connect probe.
var probeExtenderAddress = func(
	ctx context.Context,
	target *model.NetworkExtenderProbeTarget,
	serverName string,
	destinationHost string,
) error {
	// the dial, with the camouflage key the extender's records carry, is the
	// controller's, so it is proved against an in-process extender in both
	// phases (phase 14b)
	return controller.ProbeExtenderUptime(ctx, target, serverName, destinationHost)
}

type ExtenderProbeArgs struct {
}

type ExtenderProbeResult struct {
	// counts only; which extender failed is in the log, and a task result is
	// not a place anything should be reading the directory back out of
	Probed      int `json:"probed"`
	Succeeded   int `json:"succeeded"`
	Failed      int `json:"failed"`
	Deactivated int `json:"deactivated"`
	Revoked     int `json:"revoked"`
	// block reports a failed probe set aside, since they describe an outage
	// rather than a block (connect/EXTENDER-UPDATE2.md §1.3)
	DiscountedBlockReports int `json:"discounted_block_reports,omitempty"`
}

func ScheduleExtenderProbe(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleExtenderProbeAt(clientSession, tx, server.NowUtc().Add(ExtenderProbeTimeout))
}

func scheduleExtenderProbeAt(
	clientSession *session.ClientSession,
	tx server.PgTx,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		ExtenderProbe,
		&ExtenderProbeArgs{},
		clientSession,
		task.RunOnce("extender_probe"),
		task.RunAt(runAt),
		task.MaxTime(ExtenderProbeMaxTime),
	)
}

// ExtenderProbe checks every active extender address once (C3).
//
// It returns nil even when probes failed, and even when every probe failed.
// A failing probe is this job's data, not its error: a task that returned one
// would be rescheduled on a backoff with its Post skipped, so one unreachable
// extender would slow and eventually strand the liveness check for all of
// them -- and an unchecked directory publishes dead addresses forever, which
// is exactly the failure this job exists to prevent.
func ExtenderProbe(
	_ *ExtenderProbeArgs,
	clientSession *session.ClientSession,
) (*ExtenderProbeResult, error) {
	result := &ExtenderProbeResult{}

	config, err := controller.EnvExtenderConfig()
	if err != nil {
		// no extender network in this deployment; the chain still re-arms so
		// configuring one later needs no restart
		return result, nil
	}
	rootPrivateKey, err := config.RootPrivateKey()
	if err != nil {
		// without the root key a lost address could be deactivated but never
		// revoked, which would leave the directory holding a record the
		// operator can no longer withdraw. Probe nothing instead.
		glog.Errorf("[extenderprobe]no root key, the uptime probes are paused: %s\n", err)
		return result, nil
	}
	destinationHost, err := config.ApiHost()
	if err != nil {
		glog.Errorf("[extenderprobe]no api host, the uptime probes are paused: %s\n", err)
		return result, nil
	}

	targets := model.GetActiveNetworkExtenderProbeTargets(clientSession.Ctx)
	result.Probed = len(targets)
	if len(targets) == 0 {
		return result, nil
	}

	stateLock := sync.Mutex{}
	workers := make(chan struct{}, ExtenderProbeConcurrency)
	wait := sync.WaitGroup{}
	for _, target := range targets {
		wait.Add(1)
		workers <- struct{}{}
		go func() {
			defer func() {
				<-workers
				wait.Done()
			}()

			// A raise that reaches the top of a goroutine takes the process with
			// it, and the writes below raise once the pass's context has ended:
			// the task's max time, or a drain. It ends this address instead, and
			// the pass returns with the task.
			server.HandleError(func() {
				serverName, err := config.ProbeServerName()
				if err != nil {
					glog.Errorf("[extenderprobe]no probe name: %s\n", err)
					return
				}
				probeCtx, probeCancel := context.WithTimeout(
					clientSession.Ctx,
					ExtenderProbeAddressTimeout,
				)
				defer probeCancel()

				// TEMPORARY (ExtenderProbeFailureDiagnostics): the same dial, watched,
				// and how a failure failed, at most once an hour for each address
				probeCtx, probeSocket := withExtenderProbeSocket(probeCtx)
				probeErr := probeExtenderAddress(probeCtx, target, serverName, destinationHost)
				if probeErr != nil && ExtenderProbeFailureDiagnostics {
					logExtenderProbeFailure(clientSession.Ctx, target, serverName, probeSocket.observed(probeErr), server.NowUtc())
				}
				outcome := model.RecordNetworkExtenderProbeResult(
					clientSession.Ctx,
					target.ExtenderId,
					target.IpVersion,
					probeErr == nil,
					server.NowUtc(),
					ExtenderMaxConsecutiveProbeFailures,
					func(extender *model.NetworkExtender, issueTime time.Time) ([]byte, error) {
						_, message, err := controller.SignExtenderRevocation(
							config,
							rootPrivateKey,
							extender,
							issueTime,
						)
						return message, err
					},
				)
				// A failed probe is an outage. The block reports made before it
				// describe that outage, not a block, so none of them counts
				// again once the extender answers (connect/EXTENDER-UPDATE2.md
				// §1.3). Set aside after the result commits, so every report the
				// failure did not refuse is already visible.
				discountedCount := int64(0)
				if probeErr != nil {
					discountedCount = model.DiscountNetworkExtenderBlockReports(
						clientSession.Ctx,
						target.ExtenderId,
						server.NowUtc(),
					)
				}

				stateLock.Lock()
				defer stateLock.Unlock()
				result.DiscountedBlockReports += int(discountedCount)
				if probeErr == nil {
					result.Succeeded += 1
				} else {
					result.Failed += 1
					// one line per unreachable address is a lot during an outage;
					// the count below is the unconditional signal
					if glog.V(1) {
						glog.Infof(
							"[extenderprobe]extender %s ipv%d did not answer: %s\n",
							target.ExtenderId,
							target.IpVersion,
							extenderProbeErrorText(target, probeErr),
						)
					}
				}
				if outcome.AddressDeactivated {
					result.Deactivated += 1
					glog.Errorf(
						"[extenderprobe]extender %s ipv%d deactivated after %d consecutive failures\n",
						target.ExtenderId,
						target.IpVersion,
						ExtenderMaxConsecutiveProbeFailures,
					)
				}
				if outcome.ExtenderRevoked {
					result.Revoked += 1
					glog.Errorf(
						"[extenderprobe]extender %s revoked, it has no active address left\n",
						target.ExtenderId,
					)
				}
			})
		}()
	}
	wait.Wait()

	glog.Infof(
		"[extenderprobe]probed %d addresses: %d ok, %d failed, %d deactivated, %d revoked, %d block reports discounted\n",
		result.Probed,
		result.Succeeded,
		result.Failed,
		result.Deactivated,
		result.Revoked,
		result.DiscountedBlockReports,
	)
	return result, nil
}

// The text of a failed probe for a log line. A gated address must never reach a
// log (connect/EXTENDER.md R1), so the lines here name an extender by its id and
// family, and this takes the address out of what a failed dial says, which is
// where it would come back: "dial tcp <address>:443: i/o timeout".
func extenderProbeErrorText(target *model.NetworkExtenderProbeTarget, probeErr error) string {
	text := probeErr.Error()
	for _, address := range []string{target.Ip.String(), target.Ip.Unmap().String()} {
		text = strings.ReplaceAll(text, address, "<address>")
	}
	return text
}

func ExtenderProbePost(
	_ *ExtenderProbeArgs,
	_ *ExtenderProbeResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	scheduleExtenderProbeAt(clientSession, tx, server.NowUtc().Add(ExtenderProbeTimeout))
	return nil
}
