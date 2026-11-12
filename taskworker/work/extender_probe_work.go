package work

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Extender uptime probes (connect/EXTENDER.md C3).
//
// Every five minutes each active address is dialed over the required paths it
// was accepted on -- its tcp carrier, or its webrtc carrier through the
// signaling relay, or both, webrtc beside tcp once tcp has failed or been slow
// to pass -- with a fresh challenge (C2a; connect/EXTENDER.md S8). That is the whole liveness test: an address that
// cannot sign bytes chosen now is not serving, whatever its record says. The
// dial is the same one a client makes, so what passes here is what a client
// can use.
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
// A failure counts only when the prober could have passed the probe. Each pass
// is judged one family at a time before any of its failures is written: when
// the failures span the family's networks, or the prober's own control target
// fails over the family, the failures are the prober's and count against no
// one (extender_probe_common_mode.go, extender_probe_control.go). Successes
// always count.
//
// An address the probes deactivated is probed again for its return, and
// restored the first time it answers, its extender with it, so a revocation the
// extender recovers from does not wait on the extender's own next activation
// (extender_probe_recovery.go).
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

	// The end of a pass kept for writing its failures, which wait on the
	// pass's verdict: no probe starts later than this before the pass's max
	// time. An address left unprobed is the oldest probed of the next pass.
	ExtenderProbeRecordReserve = 2 * time.Minute

	// How long an address accepted on both required paths gives tcp before
	// webrtc is probed beside it. A tcp probe that answers passes well within
	// it, so a healthy address sends its extender's device no offer each
	// pass, and one whose tcp port stopped answering, which waits out the
	// budget, still leaves webrtc the activation's whole budget.
	ExtenderProbeWebRtcHeadStart = ExtenderProbeAddressTimeout - controller.ExtenderActivateProbeTimeout
)

// probeExtenderAddress probes one address over the required paths it was
// accepted on (C2a, C3) and requires a challenge signature under the
// extender's published key: the tcp carrier when the address lists it, the
// webrtc carrier when it lists that (connect/EXTENDER.md S8), and, when it
// lists both, tcp first and webrtc beside it once tcp has failed or not passed
// within its head start, the address live while either answers. So an address
// accepted on webrtc is never judged by a public tcp port it may not have, a
// tcp port that stopped answering does not starve the webrtc probe of the
// address budget, and a tcp port that answers costs no offer. An address that
// lists neither is judged by tcp, as every address was before webrtc.
//
// It is a variable so a test can drive the task's accounting -- the failure
// budget, the deactivation, the revocation -- deterministically, without
// standing up an extender for every case and without waiting on real dials;
// the two paths beneath it and the head start's timer are variables so a test
// can prove which path an address is judged by, and when. Production never
// replaces them; the real dials are proved against an in-process extender by
// the activation tests, which run the same connect probes.
var probeExtenderAddress = func(
	ctx context.Context,
	target *model.NetworkExtenderProbeTarget,
	serverName string,
	destinationHost string,
) error {
	listsTcp := slices.Contains(target.Carriers, connect.ExtenderCarrierTcp)
	listsWebRtc := slices.Contains(target.Carriers, connect.ExtenderCarrierWebRtc)
	probeTcp := probeExtenderTcpAddress
	probeWebRtc := probeExtenderWebRtcAddress
	switch {
	case listsWebRtc && !listsTcp:
		return probeExtenderPath(func() error {
			return probeWebRtc(ctx, target, destinationHost)
		})
	case !listsWebRtc:
		return probeExtenderPath(func() error {
			return probeTcp(ctx, target, serverName, destinationHost)
		})
	}

	// both, a staggered race: the first to pass ends the other, and the call
	// returns once both have ended, so no probe outlives the address's place
	// in the pass's concurrency
	pathCtx, pathCancel := context.WithCancel(ctx)
	defer pathCancel()
	results := make(chan error, 2)
	running := 0
	start := func(probe func() error) {
		running += 1
		go func() {
			results <- probeExtenderPath(probe)
		}()
	}
	webRtcStarted := false
	startWebRtc := func() {
		if webRtcStarted || ctx.Err() != nil {
			return
		}
		webRtcStarted = true
		start(func() error {
			return probeWebRtc(pathCtx, target, destinationHost)
		})
	}
	start(func() error {
		return probeTcp(pathCtx, target, serverName, destinationHost)
	})
	headStart, stopHeadStart := extenderProbeWebRtcHeadStart()
	defer stopHeadStart()
	passed := false
	errs := []error{}
	for 0 < running {
		select {
		case err := <-results:
			running -= 1
			switch {
			case passed:
				// the canceled path, joined
			case err == nil:
				passed = true
				pathCancel()
			default:
				errs = append(errs, err)
				startWebRtc()
			}
		case <-headStart:
			headStart = nil
			if !passed {
				startWebRtc()
			}
		}
	}
	if passed {
		return nil
	}
	return errors.Join(errs...)
}

// The head start's timer, a channel that receives once it has elapsed and a
// stop. It is a variable so a test can elapse it, or never, without waiting.
var extenderProbeWebRtcHeadStart = func() (<-chan time.Time, func()) {
	timer := time.NewTimer(ExtenderProbeWebRtcHeadStart)
	return timer.C, func() {
		timer.Stop()
	}
}

// One path's probe, a panic in which fails that path alone rather than the
// pass, which runs it from a goroutine of its own.
func probeExtenderPath(probe func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[extender]uptime probe panicked: %s\n", server.ErrorJson(r, debug.Stack()))
			err = fmt.Errorf("the probe failed: %v", r)
		}
	}()
	return probe()
}

// The tcp challenge of one address (C3).
var probeExtenderTcpAddress = func(
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

// The webrtc challenge of one address (C2a, C3), through the operator's
// signaling relay to the extender's client.
var probeExtenderWebRtcAddress = func(
	ctx context.Context,
	target *model.NetworkExtenderProbeTarget,
	destinationHost string,
) error {
	return controller.ProbeExtenderWebRtcUptime(ctx, target, destinationHost)
}

// The reads and writes of one pass, and its diagnostic line. The accounting
// itself -- the failure budget, the deactivation, the revocation, the restore
// -- is the model's, and the model's own tests prove it against the database;
// this is the seam a test runs the pass's decisions through with no database
// at all. Production never replaces it.
type extenderProbeStore interface {
	// every active address, oldest probe first
	activeTargets(ctx context.Context) []*model.NetworkExtenderProbeTarget
	// applies one address's result (model.RecordNetworkExtenderProbeResult)
	recordResult(
		ctx context.Context,
		target *model.NetworkExtenderProbeTarget,
		success bool,
		probeTime time.Time,
		maxConsecutiveProbeFailures int,
		signRevocation model.NetworkExtenderRevocationSigner,
	) *model.NetworkExtenderProbeOutcome
	// sets aside the block reports an outage made (model.DiscountNetworkExtenderBlockReports)
	discountBlockReports(ctx context.Context, extenderId server.Id, now time.Time) int64
	// the temporary diagnostic line of one failed probe (extender_probe_diagnostics.go)
	logFailure(
		ctx context.Context,
		target *model.NetworkExtenderProbeTarget,
		serverName string,
		probeErr error,
		now time.Time,
	)
	// every address the probes deactivated that last worked at or after
	// lastWorkedSince, oldest probe first (model.GetNetworkExtenderRecoveryProbeTargets)
	recoveryTargets(
		ctx context.Context,
		lastWorkedSince time.Time,
		minConsecutiveProbeFailures int,
	) []*model.NetworkExtenderRecoveryProbeTarget
	// applies one probe for an address's return (model.RecordNetworkExtenderRecoveryProbeResult)
	recordRecovery(
		ctx context.Context,
		target *model.NetworkExtenderRecoveryProbeTarget,
		success bool,
		probeTime time.Time,
		lastWorkedSince time.Time,
		minConsecutiveProbeFailures int,
		signRecord model.NetworkExtenderRecordSigner,
	) *model.NetworkExtenderRecoveryOutcome
}

// The store of a pass. A variable for the reason probeExtenderAddress is one.
var newExtenderProbeStore = func() extenderProbeStore {
	return modelExtenderProbeStore{}
}

// The clock of a pass, for the same reason.
var extenderProbeNow = server.NowUtc

// The production store: the model's reads and writes.
type modelExtenderProbeStore struct{}

func (modelExtenderProbeStore) activeTargets(ctx context.Context) []*model.NetworkExtenderProbeTarget {
	return model.GetActiveNetworkExtenderProbeTargets(ctx)
}

func (modelExtenderProbeStore) recordResult(
	ctx context.Context,
	target *model.NetworkExtenderProbeTarget,
	success bool,
	probeTime time.Time,
	maxConsecutiveProbeFailures int,
	signRevocation model.NetworkExtenderRevocationSigner,
) *model.NetworkExtenderProbeOutcome {
	return model.RecordNetworkExtenderProbeResult(
		ctx,
		target.ExtenderId,
		target.IpVersion,
		success,
		probeTime,
		maxConsecutiveProbeFailures,
		signRevocation,
	)
}

func (modelExtenderProbeStore) discountBlockReports(ctx context.Context, extenderId server.Id, now time.Time) int64 {
	return model.DiscountNetworkExtenderBlockReports(ctx, extenderId, now)
}

func (modelExtenderProbeStore) logFailure(
	ctx context.Context,
	target *model.NetworkExtenderProbeTarget,
	serverName string,
	probeErr error,
	now time.Time,
) {
	logExtenderProbeFailure(ctx, target, serverName, probeErr, now)
}

func (modelExtenderProbeStore) recoveryTargets(
	ctx context.Context,
	lastWorkedSince time.Time,
	minConsecutiveProbeFailures int,
) []*model.NetworkExtenderRecoveryProbeTarget {
	return model.GetNetworkExtenderRecoveryProbeTargets(ctx, lastWorkedSince, minConsecutiveProbeFailures)
}

func (modelExtenderProbeStore) recordRecovery(
	ctx context.Context,
	target *model.NetworkExtenderRecoveryProbeTarget,
	success bool,
	probeTime time.Time,
	lastWorkedSince time.Time,
	minConsecutiveProbeFailures int,
	signRecord model.NetworkExtenderRecordSigner,
) *model.NetworkExtenderRecoveryOutcome {
	return model.RecordNetworkExtenderRecoveryProbeResult(
		ctx,
		target.ExtenderId,
		target.IpVersion,
		target.Ip,
		success,
		probeTime,
		lastWorkedSince,
		minConsecutiveProbeFailures,
		signRecord,
	)
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
	// failures of a family whose pass was held, which counted against no one
	Held int `json:"held,omitempty"`
	// the held families and the verdict that held each, such as "ipv6 common_mode"
	HeldFamilies []string `json:"held_families,omitempty"`
	// deactivated addresses probed for their return, the ones restored, and the
	// revoked extenders the restores brought back
	RecoveryProbed int `json:"recovery_probed,omitempty"`
	Restored       int `json:"restored,omitempty"`
	Unrevoked      int `json:"unrevoked,omitempty"`
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

// ExtenderProbe checks every active extender address once (C3), and the
// recently deactivated ones for their return.
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

	pass := &extenderProbePass{
		ctx:             clientSession.Ctx,
		store:           newExtenderProbeStore(),
		config:          config,
		rootPrivateKey:  rootPrivateKey,
		destinationHost: destinationHost,
		result:          result,
		judged:          map[int][]extenderProbeJudgedResult{},
	}
	pass.run()
	return result, nil
}

// One pass.
type extenderProbePass struct {
	ctx             context.Context
	store           extenderProbeStore
	config          *controller.ExtenderConfig
	rootPrivateKey  ed25519.PrivateKey
	destinationHost string

	stateLock sync.Mutex
	result    *ExtenderProbeResult
	// the active addresses' results by family, which the verdict reads
	judged map[int][]extenderProbeJudgedResult
	// the failed probes, which wait on their family's verdict
	failures         []*model.NetworkExtenderProbeTarget
	recoveryFailures []*model.NetworkExtenderRecoveryProbeTarget
}

func (self *extenderProbePass) run() {
	passTime := extenderProbeNow()
	lastWorkedSince := passTime.Add(-ExtenderProbeRecoveryWindow)
	targets := self.store.activeTargets(self.ctx)
	recoveryTargets := extenderProbeDueRecoveryTargets(
		self.store.recoveryTargets(self.ctx, lastWorkedSince, ExtenderMaxConsecutiveProbeFailures),
		passTime,
	)
	self.result.Probed = len(targets)
	self.result.RecoveryProbed = len(recoveryTargets)
	if len(targets) == 0 && len(recoveryTargets) == 0 {
		return
	}

	// the control brackets the probes of each family the pass judges
	ipVersions := []int{}
	for _, target := range targets {
		if !slices.Contains(ipVersions, target.IpVersion) {
			ipVersions = append(ipVersions, target.IpVersion)
		}
	}
	slices.Sort(ipVersions)
	controlBefore := self.checkControl(ipVersions)
	self.probe(targets, recoveryTargets, lastWorkedSince)
	controlAfter := self.checkControl(ipVersions)

	judgements := map[int]*extenderProbeFamilyJudgement{}
	for _, ipVersion := range ipVersions {
		controlFailed := false
		controlNote := ""
		for _, check := range []*extenderProbeControlCheck{controlBefore[ipVersion], controlAfter[ipVersion]} {
			controlFailed = controlFailed || check.holds
			if check.result == extenderProbeControlFailed && controlNote == "" {
				controlNote = "; the control target failed"
				if check.detail != "" {
					controlNote += ": " + check.detail
				}
			}
		}
		judgement := judgeExtenderProbeFamily(ipVersion, self.judged[ipVersion], controlFailed)
		judgements[ipVersion] = judgement
		extenderProbePassesCounter.WithLabelValues(extenderProbeIpVersionLabel(ipVersion), judgement.verdict).Inc()
		switch {
		case judgement.held():
			self.result.HeldFamilies = append(self.result.HeldFamilies, fmt.Sprintf("ipv%d %s", ipVersion, judgement.verdict))
			// alert class: the prober, not the extenders, failed this family's pass
			glog.Errorf(
				"[extenderprobe]ipv%d pass held (%s): %d of %d addresses failed across %d of %d networks; none of its failures counts%s\n",
				ipVersion,
				judgement.verdict,
				judgement.failedAddressCount,
				judgement.addressCount,
				judgement.failedNetworkCount,
				judgement.networkCount,
				controlNote,
			)
		case controlFailed:
			glog.Warningf(
				"[extenderprobe]ipv%d pass counted%s, but %d of %d addresses answered, so the failure is the control's own\n",
				ipVersion,
				controlNote,
				judgement.addressCount-judgement.failedAddressCount,
				judgement.addressCount,
			)
		case controlNote != "":
			// a control that has not answered from this process cannot speak
			// for the prober, and holds nothing
			glog.Warningf(
				"[extenderprobe]ipv%d pass counted%s, and it has not answered in this process, so it holds nothing\n",
				ipVersion,
				controlNote,
			)
		}
	}

	self.record(judgements, lastWorkedSince)

	glog.Infof(
		"[extenderprobe]probed %d addresses: %d ok, %d failed, %d deactivated, %d revoked, %d block reports discounted; %d failures held; %d probed for their return, %d restored, %d extenders back\n",
		self.result.Probed,
		self.result.Succeeded,
		self.result.Failed,
		self.result.Deactivated,
		self.result.Revoked,
		self.result.DiscountedBlockReports,
		self.result.Held,
		self.result.RecoveryProbed,
		self.result.Restored,
		self.result.Unrevoked,
	)
}

// Checks the prober's control target over each family, and counts what it
// found.
func (self *extenderProbePass) checkControl(ipVersions []int) map[int]*extenderProbeControlCheck {
	checks := extenderProbeControls.check(self.ctx, self.destinationHost, ipVersions)
	for _, ipVersion := range ipVersions {
		extenderProbeControlCounter.WithLabelValues(extenderProbeIpVersionLabel(ipVersion), checks[ipVersion].result).Inc()
	}
	return checks
}

// Runs work on one of ExtenderProbeConcurrency workers.
func runExtenderProbeWorkers[T any](items []T, work func(item T)) {
	workers := make(chan struct{}, ExtenderProbeConcurrency)
	wait := sync.WaitGroup{}
	for _, item := range items {
		wait.Add(1)
		workers <- struct{}{}
		go func() {
			defer func() {
				<-workers
				wait.Done()
			}()
			// A raise that reaches the top of a goroutine takes the process
			// with it, and the writes raise once the pass's context has ended:
			// the task's max time, or a drain. It ends this address instead, and
			// the pass returns with the task.
			server.HandleError(func() {
				work(item)
			})
		}()
	}
	wait.Wait()
}

// The probes of the pass: the active addresses, then the ones probed for their
// return. A success is written at once, since it counts whatever the verdict;
// a failure waits for its family's verdict.
func (self *extenderProbePass) probe(
	targets []*model.NetworkExtenderProbeTarget,
	recoveryTargets []*model.NetworkExtenderRecoveryProbeTarget,
	lastWorkedSince time.Time,
) {
	probeCtx, probeCancel := context.WithTimeout(self.ctx, ExtenderProbeMaxTime-ExtenderProbeRecordReserve)
	defer probeCancel()

	type probeItem struct {
		target   *model.NetworkExtenderProbeTarget
		recovery *model.NetworkExtenderRecoveryProbeTarget
	}
	items := []probeItem{}
	for _, target := range targets {
		items = append(items, probeItem{target: target})
	}
	for _, recoveryTarget := range recoveryTargets {
		items = append(items, probeItem{target: &recoveryTarget.NetworkExtenderProbeTarget, recovery: recoveryTarget})
	}

	runExtenderProbeWorkers(items, func(item probeItem) {
		if probeCtx.Err() != nil {
			// past the pass's last start, or the pass ended
			return
		}
		serverName, err := self.config.ProbeServerName()
		if err != nil {
			glog.Errorf("[extenderprobe]no probe name: %s\n", err)
			return
		}
		addressCtx, addressCancel := context.WithTimeout(
			self.ctx,
			ExtenderProbeAddressTimeout,
		)
		defer addressCancel()

		// TEMPORARY (ExtenderProbeFailureDiagnostics): the same dial, watched,
		// and how a failure failed, at most once an hour for each address
		addressCtx, probeSocket := withExtenderProbeSocket(addressCtx)
		probeErr := probeExtenderAddress(addressCtx, item.target, serverName, self.destinationHost)
		if probeErr != nil && self.ctx.Err() != nil {
			// the pass ended under the probe, which says nothing of the extender
			return
		}

		if item.recovery != nil {
			self.probedRecovery(item.recovery, probeErr == nil, lastWorkedSince)
			return
		}

		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.judged[item.target.IpVersion] = append(self.judged[item.target.IpVersion], extenderProbeJudgedResult{
				networkId: item.target.NetworkId,
				failed:    probeErr != nil,
			})
		}()
		if probeErr == nil {
			self.store.recordResult(
				self.ctx,
				item.target,
				true,
				extenderProbeNow(),
				ExtenderMaxConsecutiveProbeFailures,
				self.signRevocation,
			)
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.result.Succeeded += 1
			extenderProbeResultsCounter.WithLabelValues(
				extenderProbeIpVersionLabel(item.target.IpVersion),
				extenderProbeSetActive,
				extenderProbeResultOk,
			).Inc()
			return
		}

		if ExtenderProbeFailureDiagnostics {
			self.store.logFailure(self.ctx, item.target, serverName, probeSocket.observed(probeErr), extenderProbeNow())
		}
		// one line per unreachable address is a lot during an outage; the
		// count of the pass is the unconditional signal
		if glog.V(1) {
			glog.Infof(
				"[extenderprobe]extender %s ipv%d did not answer: %s\n",
				item.target.ExtenderId,
				item.target.IpVersion,
				extenderProbeErrorText(item.target, probeErr),
			)
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.failures = append(self.failures, item.target)
	})
}

// A probe for one address's return. A success restores it at once; a failure
// waits for the verdict, like any other.
func (self *extenderProbePass) probedRecovery(
	target *model.NetworkExtenderRecoveryProbeTarget,
	success bool,
	lastWorkedSince time.Time,
) {
	if !success {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.recoveryFailures = append(self.recoveryFailures, target)
		return
	}
	outcome := self.store.recordRecovery(
		self.ctx,
		target,
		true,
		extenderProbeNow(),
		lastWorkedSince,
		ExtenderMaxConsecutiveProbeFailures,
		self.signRecord,
	)
	ipVersionLabel := extenderProbeIpVersionLabel(target.IpVersion)
	extenderProbeResultsCounter.WithLabelValues(ipVersionLabel, extenderProbeSetRecovery, extenderProbeResultOk).Inc()
	if !outcome.AddressRestored {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.result.Restored += 1
	if outcome.ExtenderRestored {
		self.result.Unrevoked += 1
		extenderProbeRestoresCounter.WithLabelValues(ipVersionLabel, extenderProbeRestoreExtender).Inc()
		extenderProbeRestoreRevokedSeconds.Observe(outcome.RevokedDuration.Seconds())
		glog.Infof(
			"[extenderprobe]extender %s ipv%d answered again and is restored, %s after its revocation\n",
			target.ExtenderId,
			target.IpVersion,
			outcome.RevokedDuration.Round(time.Second),
		)
	} else {
		extenderProbeRestoresCounter.WithLabelValues(ipVersionLabel, extenderProbeRestoreAddress).Inc()
		glog.Infof(
			"[extenderprobe]extender %s ipv%d answered again and is restored\n",
			target.ExtenderId,
			target.IpVersion,
		)
	}
}

// Writes the failures of the families whose verdict counts them, and counts
// the rest as held. A family with no verdict probed no active address, so its
// probes for a return count.
func (self *extenderProbePass) record(
	judgements map[int]*extenderProbeFamilyJudgement,
	lastWorkedSince time.Time,
) {
	held := func(ipVersion int) bool {
		judgement, ok := judgements[ipVersion]
		return ok && judgement.held()
	}

	runExtenderProbeWorkers(self.failures, func(target *model.NetworkExtenderProbeTarget) {
		ipVersionLabel := extenderProbeIpVersionLabel(target.IpVersion)
		if held(target.IpVersion) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.result.Held += 1
			extenderProbeResultsCounter.WithLabelValues(ipVersionLabel, extenderProbeSetActive, extenderProbeResultHeld).Inc()
			return
		}
		outcome := self.store.recordResult(
			self.ctx,
			target,
			false,
			extenderProbeNow(),
			ExtenderMaxConsecutiveProbeFailures,
			self.signRevocation,
		)
		// A failed probe is an outage. The block reports made before it
		// describe that outage, not a block, so none of them counts again once
		// the extender answers (connect/EXTENDER-UPDATE2.md §1.3). Set aside
		// after the result commits, so every report the failure did not refuse
		// is already visible.
		discountedCount := self.store.discountBlockReports(self.ctx, target.ExtenderId, extenderProbeNow())

		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.result.Failed += 1
		self.result.DiscountedBlockReports += int(discountedCount)
		extenderProbeResultsCounter.WithLabelValues(ipVersionLabel, extenderProbeSetActive, extenderProbeResultFailed).Inc()
		if outcome.AddressDeactivated {
			self.result.Deactivated += 1
			extenderProbeDeactivationsCounter.WithLabelValues(ipVersionLabel).Inc()
			glog.Errorf(
				"[extenderprobe]extender %s ipv%d deactivated after %d consecutive failures\n",
				target.ExtenderId,
				target.IpVersion,
				ExtenderMaxConsecutiveProbeFailures,
			)
		}
		if outcome.ExtenderRevoked {
			self.result.Revoked += 1
			extenderProbeRevocationsCounter.Inc()
			glog.Errorf(
				"[extenderprobe]extender %s revoked, it has no active address left\n",
				target.ExtenderId,
			)
		}
	})

	runExtenderProbeWorkers(self.recoveryFailures, func(target *model.NetworkExtenderRecoveryProbeTarget) {
		ipVersionLabel := extenderProbeIpVersionLabel(target.IpVersion)
		if held(target.IpVersion) {
			// the probe for its return comes round again at the next pass
			extenderProbeResultsCounter.WithLabelValues(ipVersionLabel, extenderProbeSetRecovery, extenderProbeResultHeld).Inc()
			return
		}
		self.store.recordRecovery(
			self.ctx,
			target,
			false,
			extenderProbeNow(),
			lastWorkedSince,
			ExtenderMaxConsecutiveProbeFailures,
			self.signRecord,
		)
		extenderProbeResultsCounter.WithLabelValues(ipVersionLabel, extenderProbeSetRecovery, extenderProbeResultFailed).Inc()
	})
}

// Signs the revocation of an extender that lost its last address.
func (self *extenderProbePass) signRevocation(extender *model.NetworkExtender, issueTime time.Time) ([]byte, error) {
	_, message, err := controller.SignExtenderRevocation(
		self.config,
		self.rootPrivateKey,
		extender,
		issueTime,
	)
	return message, err
}

// Signs the record of a restored extender.
func (self *extenderProbePass) signRecord(
	extender *model.NetworkExtender,
	addresses []*model.NetworkExtenderAddress,
	issueTime time.Time,
) ([]byte, error) {
	_, message, err := controller.SignExtenderRecord(
		self.config,
		self.rootPrivateKey,
		extender,
		addresses,
		issueTime,
	)
	return message, err
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
