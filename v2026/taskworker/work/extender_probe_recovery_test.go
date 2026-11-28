package work

// The way back (extender_probe_recovery.go): which deactivated addresses a
// pass probes for their return, and what a restore publishes. The churn
// tests' in-memory store and clock stand in for the database
// (extender_probe_churn_test.go).

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestExtenderProbeRecoverySchedule(t *testing.T) {
	now := testExtenderChurnStart
	at := func(ago time.Duration) *time.Time {
		at := now.Add(-ago)
		return &at
	}
	target := func(sinceWork time.Duration, sinceProbe *time.Time) *model.NetworkExtenderRecoveryProbeTarget {
		return &model.NetworkExtenderRecoveryProbeTarget{
			LastWorkTime:  now.Add(-sinceWork),
			LastProbeTime: sinceProbe,
		}
	}
	for _, test := range []struct {
		name   string
		target *model.NetworkExtenderRecoveryProbeTarget
		due    bool
	}{
		{"just deactivated", target(30*time.Minute, at(0)), true},
		{"probed for its return a pass ago", target(time.Hour, at(ExtenderProbeTimeout)), true},
		{"the end of the every-pass window", target(ExtenderProbeRecoveryEveryPassWindow-time.Second, at(0)), true},
		{"past it, probed a pass ago", target(ExtenderProbeRecoveryEveryPassWindow, at(ExtenderProbeTimeout)), false},
		{"past it, probed an interval ago", target(5*time.Hour, at(ExtenderProbeRecoveryInterval)), true},
		{"past it, never probed", target(5*time.Hour, nil), true},
		{"the end of the window", target(ExtenderProbeRecoveryWindow-time.Second, at(ExtenderProbeRecoveryInterval)), true},
		{"past the window", target(ExtenderProbeRecoveryWindow, at(ExtenderProbeRecoveryInterval)), false},
	} {
		if due := extenderProbeRecoveryDue(test.target, now); due != test.due {
			t.Errorf("%s: due = %t, want %t", test.name, due, test.due)
		}
	}
	connect.AssertEqual(t, len(extenderProbeDueRecoveryTargets([]*model.NetworkExtenderRecoveryProbeTarget{
		target(time.Hour, at(0)),
		target(ExtenderProbeRecoveryWindow, nil),
		target(5*time.Hour, at(time.Minute)),
		target(5*time.Hour, nil),
	}, now)), 2)
}

// Deactivates every address of the world's extenders with six counted
// failures of a down extender, then sets every probe to answer as told.
func (self *testChurnWorld) revokeAll() {
	self.t.Helper()
	self.setAnswer(testChurnAlwaysFail(errTestChurnExtenderDown))
	for range ExtenderMaxConsecutiveProbeFailures {
		self.pass()
	}
}

// Past the window, an extender is gone: it is never probed again and comes back
// only through an activation of its own. Before the window ends it is probed
// every pass at first, then once an interval.
func TestExtenderProbeRecoveryEndsWithTheWindow(t *testing.T) {
	world := newTestChurnWorld(t)
	extenderId := world.store.addExtender(server.NewId(), testExtenderChurnStart, 4)
	world.pass()
	world.revokeAll()
	connect.AssertEqual(t, world.store.extender(extenderId).active, false)

	probes := 0
	world.setAnswer(func(target *model.NetworkExtenderProbeTarget) error {
		probes += 1
		return errTestChurnExtenderDown
	})
	passesInWindow := int(ExtenderProbeRecoveryWindow / ExtenderProbeTimeout)
	for range passesInWindow + 12 {
		world.pass()
	}
	// It last worked at the first pass, t0, and was deactivated at t0+30m.
	// Every pass probes it from t0+35m until two hours after t0, 17 passes,
	// and then one pass in six: t0+145m, and every half hour after to the end
	// of the window, 140 more. Every pass for the three days would be 864.
	connect.AssertEqual(t, probes, 17+140)

	// past the window nothing probes it, even when it would answer
	world.setAnswer(func(target *model.NetworkExtenderProbeTarget) error {
		probes += 1
		return nil
	})
	before := probes
	result := world.pass()
	connect.AssertEqual(t, result.RecoveryProbed, 0)
	connect.AssertEqual(t, probes, before)
	connect.AssertEqual(t, world.store.extender(extenderId).active, false)
}

// One family of a dual-stack extender that came back is restored into the
// extender's record: the record queued names both families, so a client learns
// the address it had lost without waiting for the drip.
func TestExtenderProbeRecoveryRestoresAFamilyIntoTheRecord(t *testing.T) {
	world := newTestChurnWorld(t)
	extenderId := world.store.addExtender(server.NewId(), testExtenderChurnStart, 4, 6)
	world.pass()

	world.setAnswer(func(target *model.NetworkExtenderProbeTarget) error {
		if target.IpVersion == 4 {
			return errTestChurnExtenderDown
		}
		return nil
	})
	for range ExtenderMaxConsecutiveProbeFailures {
		world.pass()
	}
	extender := world.store.extender(extenderId)
	connect.AssertEqual(t, extender.active, true)
	connect.AssertEqual(t, extender.addresses[4].active, false)
	connect.AssertEqual(t, len(world.store.publishesOf(extenderId)), 0)

	world.setAnswer(func(target *model.NetworkExtenderProbeTarget) error {
		return nil
	})
	result := world.pass()
	connect.AssertEqual(t, result.Restored, 1)
	connect.AssertEqual(t, result.Unrevoked, 0)
	extender = world.store.extender(extenderId)
	connect.AssertEqual(t, extender.addresses[4].active, true)

	publishes := world.store.publishesOf(extenderId)
	connect.AssertEqual(t, len(publishes), 1)
	connect.AssertEqual(t, publishes[0].kind, model.NetworkExtenderPublishKindRecord)
	message := &protocol.ExtenderGossipMessage{}
	if err := proto.Unmarshal(publishes[0].message, message); err != nil {
		t.Fatal(err)
	}
	body, err := connect.NewExtenderRootKeySet(world.rootPublicKey).VerifyRecord(message.GetRecord())
	if err != nil {
		t.Fatal(err)
	}
	ipVersions := []uint32{}
	for _, address := range body.Addresses {
		ipVersions = append(ipVersions, address.IpVersion)
	}
	connect.AssertEqual(t, ipVersions, []uint32{4, 6})
}

// A probe for a return is no evidence about the prober: a directory of dead
// extenders waiting to come back does not make a healthy pass look like a
// prober fault, and the active addresses' failures still count.
func TestExtenderProbeRecoveryFailuresAreNotJudged(t *testing.T) {
	world := newTestChurnWorld(t)
	stubTestChurnControl(t, func(ipVersion int) string {
		return testChurnControlAbsent
	})

	// four operators with two extenders each, and twenty operators with one
	// each, whose extenders go down
	networkIds := []server.Id{}
	liveIds := []server.Id{}
	for range 4 {
		networkId := server.NewId()
		networkIds = append(networkIds, networkId)
		for range 2 {
			liveIds = append(liveIds, world.store.addExtender(networkId, testExtenderChurnStart, 4))
		}
	}
	dead := map[server.Id]bool{}
	for range 20 {
		dead[world.store.addExtender(server.NewId(), testExtenderChurnStart, 4)] = true
	}
	world.pass()
	world.setAnswer(func(target *model.NetworkExtenderProbeTarget) error {
		if dead[target.ExtenderId] {
			return errTestChurnExtenderDown
		}
		return nil
	})
	for range ExtenderMaxConsecutiveProbeFailures {
		world.pass()
	}
	connect.AssertEqual(t, world.store.revokedCount(), 20)

	// then one of the four operators goes down too. With the twenty probes for
	// a return counted, 22 of 28 addresses on 21 of 24 networks would read as
	// the prober's; the active addresses alone are 2 of 8
	world.setAnswer(func(target *model.NetworkExtenderProbeTarget) error {
		if dead[target.ExtenderId] || target.NetworkId == networkIds[0] {
			return errTestChurnExtenderDown
		}
		return nil
	})
	revoked := 0
	for range ExtenderMaxConsecutiveProbeFailures {
		result := world.pass()
		connect.AssertEqual(t, result.RecoveryProbed, 20)
		connect.AssertEqual(t, len(result.HeldFamilies), 0)
		revoked += result.Revoked
	}
	connect.AssertEqual(t, revoked, 2)
	for _, extenderId := range liveIds[:2] {
		connect.AssertEqual(t, world.store.extender(extenderId).active, false)
	}
	for _, extenderId := range liveIds[2:] {
		connect.AssertEqual(t, world.store.extender(extenderId).active, true)
	}
}

// In a held pass the probes for a return that fail are not stamped either, so
// a prober fault does not push the return of an extender out by an interval.
func TestExtenderProbeRecoveryFailuresInAHeldPassAreNotStamped(t *testing.T) {
	world := newTestChurnWorld(t)
	control := testChurnControlOk
	stubTestChurnControl(t, func(ipVersion int) string {
		return control
	})

	downId := world.store.addExtender(server.NewId(), testExtenderChurnStart, 4)
	upId := world.store.addExtender(server.NewId(), testExtenderChurnStart, 4)
	world.pass()
	world.setAnswer(func(target *model.NetworkExtenderProbeTarget) error {
		if target.ExtenderId == downId {
			return errTestChurnExtenderDown
		}
		return nil
	})
	for range ExtenderMaxConsecutiveProbeFailures {
		world.pass()
	}
	connect.AssertEqual(t, world.store.extender(downId).active, false)
	stampedAt := *world.store.extender(downId).addresses[4].lastProbeTime

	// the prober loses its route, its control with it
	control = testChurnControlFailed
	world.setAnswer(testChurnAlwaysFail(errTestChurnProberUnreachable))
	result := world.pass()
	connect.AssertEqual(t, result.HeldFamilies, []string{"ipv4 " + extenderProbeVerdictControlFailed})
	connect.AssertEqual(t, result.RecoveryProbed, 1)
	connect.AssertEqual(t, world.store.extender(upId).addresses[4].failures, 0)
	connect.AssertEqual(t, *world.store.extender(downId).addresses[4].lastProbeTime, stampedAt)
}
