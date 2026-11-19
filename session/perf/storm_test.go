package perf

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The credentials the background and steady checks draw from: networks with
// a sign-in each, and clients created half with an API key (own session) and
// half with the sign-in (inherited), every one checked once so the record
// holds its projection.
type workingSet struct {
	networks []*workingNetwork
	clients  []*credential
	signIns  []*credential
}

type workingNetwork struct {
	network *perfNetwork
	signIn  *credential
	// API key clients, which own a session: parents of hosted children
	apiKeyClients []*credential
}

func (self *harness) checkWorkingSet() *workingSet {
	if self.working != nil {
		return self.working
	}
	profile := self.config.profile
	set := &workingSet{networks: make([]*workingNetwork, profile.checkNetworks)}
	runParallel(profile.checkNetworks, 16, func(index int) {
		network := self.newNetwork("work")
		signIn, err := self.signIn(network)
		self.must(err, "working set sign-in")
		set.networks[index] = &workingNetwork{network: network, signIn: signIn}
	})
	var lock sync.Mutex
	runParallel(profile.checkNetworks*profile.checkClientsPerNetwork, 16, func(index int) {
		working := set.networks[index%profile.checkNetworks]
		apiKey := index%2 == 0
		var clientSession *session.ClientSession
		if apiKey {
			clientSession = self.apiKeySession(working.network)
		} else {
			clientSession = self.credentialSession(working.signIn)
		}
		defer clientSession.Cancel()
		credential, _, _, err := self.authClient(clientSession, false)
		self.must(err, "working set client")
		lock.Lock()
		defer lock.Unlock()
		set.clients = append(set.clients, credential)
		if apiKey {
			working.apiKeyClients = append(working.apiKeyClients, credential)
		}
	})
	for _, working := range set.networks {
		set.signIns = append(set.signIns, working.signIn)
	}
	all := append(append([]*credential{}, set.clients...), set.signIns...)
	runParallel(len(all), 16, func(index int) {
		self.must(self.leaseCheck(all[index].claims), "warm the working set")
	})
	self.working = set
	self.logf("working set: %d networks, %d clients, %d sign-ins", len(set.networks), len(set.clients), len(set.signIns))
	return set
}

// Background checks of the working set at rate, tagged by whether the
// check's record key lives on node.
func (self *harness) startTaggedChecks(name string, rate float64, node string) *runningLoads {
	working := self.checkWorkingSet()
	all := append(append([]*credential{}, working.clients...), working.signIns...)
	tags := make([]string, len(all))
	for index, credential := range all {
		tags[index] = "other-node"
		if self.nodeOf(self.recordKey(credential.claims)) == node {
			tags[index] = "storm-node"
		}
	}
	return startRecordedLoads(self.ctx, []*load{{
		name:    name,
		rate:    rate,
		workers: 64,
		op: func(ctx context.Context) {
			index := rand.IntN(len(all))
			start := time.Now()
			err := self.leaseCheck(all[index].claims)
			self.ops.observeTagged(name, tags[index], start, time.Since(start), classify(err), err)
		},
	}}, self.ops)
}

type stormWindow struct {
	Name  string     `json:"name"`
	From  time.Time  `json:"from"`
	To    time.Time  `json:"to"`
	Storm *opSummary `json:"storm_node"`
	Other *opSummary `json:"other_node"`
}

func (self *harness) windows(name string, op string, marks []time.Time, labels []string) []*stormWindow {
	windows := []*stormWindow{}
	storm, other := "storm-node", "other-node"
	for index := 0; index+1 < len(marks); index++ {
		windows = append(windows, &stormWindow{
			Name:  labels[index],
			From:  marks[index],
			To:    marks[index+1],
			Storm: self.ops.summary(op, marks[index], marks[index+1], &storm),
			Other: self.ops.summary(op, marks[index], marks[index+1], &other),
		})
	}
	return windows
}

func (self *harness) ensureLarge() *largeNetwork {
	if self.large == nil {
		self.capacityLarge()
	}
	return self.large
}

// Sign out other sessions on the large network while background checks run,
// then the client cleanup the maintenance task would run for it.
func (self *harness) runStormPhase() {
	large := self.ensureLarge()
	live := self.liveSessionCount(large.network.id)
	stormNode := self.nodeOf(self.networkSessionKey(large.network.id))
	rate := self.config.profile.rates["lease"] / 2
	self.checkWorkingSet()
	background := self.startTaggedChecks("storm.check", rate, stormNode)
	marks := []time.Time{time.Now()}
	time.Sleep(5 * time.Second)

	before, err := self.admin.snapshot(self.ctx)
	self.must(err, "snapshot")
	self.must(self.admin.slowlogReset(self.ctx), "slowlog reset")
	actor := self.credentialSession(large.actor)
	marks = append(marks, time.Now())
	start := time.Now()
	result, err := session.RevokeOtherNetworkSessions(&session.RevokeOtherSessionsArgs{OperationId: server.NewId()}, actor)
	revokeMs := float64(time.Since(start).Microseconds()) / 1000
	marks = append(marks, time.Now())
	actor.Cancel()
	self.ops.observe(fmt.Sprintf("storm.revoke.others@%d", live), start, err)
	self.must(err, "revoke others on the large network")
	afterRevoke, err := self.admin.snapshot(self.ctx)
	self.must(err, "snapshot")
	self.recordSlowScripts(fmt.Sprintf("storm.revoke.others@%d", live))
	time.Sleep(2 * time.Second)

	// the session cleanup: every client bound to a revoked session
	marks = append(marks, time.Now())
	cleanupStart := time.Now()
	batches := 0
	for {
		batchStart := time.Now()
		complete, err := model.CleanupSessionOperation(self.ctx, session.SessionCleanupOperation{
			NetworkId:        large.network.id,
			OperationId:      result.OperationId,
			TargetSessionIds: result.TargetSessionIds,
		})
		self.ops.observe("storm.cleanup.batch", batchStart, err)
		self.must(err, "session cleanup")
		batches++
		if complete || batches > 1000 {
			break
		}
	}
	cleanupMs := float64(time.Since(cleanupStart).Microseconds()) / 1000
	afterCleanup, err := self.admin.snapshot(self.ctx)
	self.must(err, "snapshot")
	marks = append(marks, time.Now())
	time.Sleep(3 * time.Second)
	marks = append(marks, time.Now())
	loads := background.finish()

	storm := &stormReport{
		Name:            "sign out other sessions",
		LiveSessions:    int(live),
		Synthetic:       large.synthetic,
		OpMs:            revokeMs,
		Revoked:         result.RevokedCount,
		CleanupMs:       cleanupMs,
		CleanupBatches:  batches,
		StormNode:       stormNode,
		RevokeCost:      self.costSince(afterRevoke, before),
		CleanupCost:     self.costSince(afterCleanup, afterRevoke),
		Windows:         self.windows("storm", "storm.check", marks, []string{"before", "revoke", "pause", "cleanup", "after"}),
		BackgroundLoads: loads,
	}
	storm.RecordBytesAfter = self.networkRecordBytes(large.network.id)
	bytes, _ := self.networkSessionBytes(large.network.id)
	storm.SessionBytesAfter = bytes

	// every revoked client is refused at its next check
	refused, accepted := self.probe(large.clients, 1000)
	storm.ProbeRefused, storm.ProbeAccepted = refused, accepted
	self.report.Storms = append(self.report.Storms, storm)
	self.report.addOutcome(fmt.Sprintf("revoked hosted clients' credentials after sign out others (%d live)", live), "refused", refused, accepted)
	self.logf("revoke others at %d live: %.1f ms, %d revoked, cleanup %.1f ms in %d batches", live, revokeMs, result.RevokedCount, cleanupMs, batches)
}

// Checks up to count of the credentials; returns how many were refused and
// how many accepted.
func (self *harness) probe(credentials []*credential, count int) (int, int) {
	sample := credentials
	if count < len(sample) {
		sample = make([]*credential, count)
		for index := range sample {
			sample[index] = credentials[rand.IntN(len(credentials))]
		}
	}
	var refused, accepted int
	var lock sync.Mutex
	runParallel(len(sample), 16, func(index int) {
		err := self.leaseCheck(sample[index].claims)
		lock.Lock()
		defer lock.Unlock()
		if err == nil {
			accepted++
		} else {
			refused++
		}
	})
	return refused, accepted
}

// The operator's sign-out-all (bringyourctl network sign-out-all), the same
// retirement a password reset journals, on a second large network with
// background checks. A hosted proxy is re-minted afterwards, as its device
// does on reopen: decision 4 keeps hosted proxies working through a reset.
func (self *harness) runSignOutPhase() {
	sessions := self.config.profile.largeSessions
	large := self.largeNetwork("signout", sessions)
	runParallel(len(large.clients), 16, func(index int) {
		self.leaseCheck(large.clients[index].claims)
	})
	live := self.liveSessionCount(large.network.id)
	stormNode := self.nodeOf(self.networkSessionKey(large.network.id))
	background := self.startTaggedChecks("signout.check", self.config.profile.rates["lease"]/2, stormNode)
	marks := []time.Time{time.Now()}
	time.Sleep(5 * time.Second)
	before, err := self.admin.snapshot(self.ctx)
	self.must(err, "snapshot")
	self.must(self.admin.slowlogReset(self.ctx), "slowlog reset")
	marks = append(marks, time.Now())
	start := time.Now()
	plan, err := model.PlanNetworkSignOut(self.ctx, large.network.id, false)
	self.must(err, "plan sign-out-all")
	result, err := model.CommitNetworkSignOut(self.ctx, large.network.id, false, plan.Digest)
	opMs := float64(time.Since(start).Microseconds()) / 1000
	marks = append(marks, time.Now())
	self.ops.observe(fmt.Sprintf("signout.all@%d", live), start, err)
	self.must(err, "commit sign-out-all")
	after, err := self.admin.snapshot(self.ctx)
	self.must(err, "snapshot")
	self.recordSlowScripts(fmt.Sprintf("signout.all@%d", live))
	time.Sleep(3 * time.Second)
	marks = append(marks, time.Now())
	loads := background.finish()

	storm := &stormReport{
		Name:            "sign-out-all (reset retirement)",
		LiveSessions:    int(live),
		Synthetic:       large.synthetic,
		OpMs:            opMs,
		Revoked:         result.RevokedSessionCount,
		CleanupComplete: result.SessionCleanupComplete,
		StormNode:       stormNode,
		RevokeCost:      self.costSince(after, before),
		Windows:         self.windows("signout", "signout.check", marks, []string{"before", "sign-out-all", "after"}),
		BackgroundLoads: loads,
	}
	if result.EnforcementError != nil {
		storm.Error = result.EnforcementError.Error()
	}
	storm.RecordBytesAfter = self.networkRecordBytes(large.network.id)
	sessionBytes, _ := self.networkSessionBytes(large.network.id)
	storm.SessionBytesAfter = sessionBytes

	// the owner's sign-in is retired; hosted proxies are expected to survive
	refused, accepted := self.probe([]*credential{large.actor}, 1)
	self.report.addOutcome("the signed-in owner's credential after sign-out-all", "refused", refused, accepted)
	refused, accepted = self.probe(large.clients, 500)
	storm.ProbeRefused, storm.ProbeAccepted = refused, accepted
	self.report.addOutcome("hosted proxy credentials after sign-out-all (decision 4 expects accepted)", "accepted", accepted, refused)
	var reminted, remintRefused int
	var remintLock sync.Mutex
	sample := large.clients[:min(200, len(large.clients))]
	runParallel(len(sample), 8, func(index int) {
		claims := sample[index].claims
		_, err := self.hostedMint(large.network, *claims.DeviceId, *claims.ClientId)
		remintLock.Lock()
		defer remintLock.Unlock()
		if err == nil {
			reminted++
		} else {
			remintRefused++
			if remintRefused == 1 {
				text := err.Error()
				if 120 < len(text) {
					text = text[:120]
				}
				storm.RemintError = classify(err) + ": " + text
				if errors.Is(err, session.ErrSessionRevoked) {
					storm.RemintError = "session_revoked"
				}
			}
		}
	})
	self.report.addOutcome("hosted proxy re-mints after sign-out-all (decision 4 expects accepted)", "accepted", reminted, remintRefused)
	self.report.Storms = append(self.report.Storms, storm)
	self.logf("sign-out-all at %d live: %.1f ms, %d revoked; hosted proxies accepted %d / refused %d, re-minted %d / refused %d", live, opMs, result.RevokedSessionCount, accepted, refused, reminted, remintRefused)
}
