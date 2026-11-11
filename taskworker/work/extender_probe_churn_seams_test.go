package work

// The churn tests' control target and the in-memory store's way back
// (extender_probe_churn_test.go).

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

const (
	testChurnControlOk     = extenderProbeControlOk
	testChurnControlFailed = extenderProbeControlFailed
	testChurnControlAbsent = extenderProbeControlAbsent
)

// Answers the prober's control target as told, for the length of the test,
// with the control state of a fresh process.
func stubTestChurnControl(t testing.TB, answer func(ipVersion int) string) {
	t.Helper()
	previousControl := probeExtenderProbeControl
	previousControls := extenderProbeControls
	probeExtenderProbeControl = func(ctx context.Context, host string, ipVersion int) (string, error) {
		return answer(ipVersion), nil
	}
	extenderProbeControls = newExtenderProbeControlState()
	t.Cleanup(func() {
		probeExtenderProbeControl = previousControl
		extenderProbeControls = previousControls
	})
}

// When an address last worked, as the model reads it.
func (self *testChurnAddress) lastWorkTime() time.Time {
	if self.lastProbeSuccessTime != nil && self.activateTime.Before(*self.lastProbeSuccessTime) {
		return *self.lastProbeSuccessTime
	}
	return self.activateTime
}

// The model's selection (model.GetNetworkExtenderRecoveryProbeTargets).
func (self *testChurnStore) recoveryTargets(
	ctx context.Context,
	lastWorkedSince time.Time,
	minConsecutiveProbeFailures int,
) []*model.NetworkExtenderRecoveryProbeTarget {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	targets := []*model.NetworkExtenderRecoveryProbeTarget{}
	for _, extenderId := range self.order {
		extender := self.extenders[extenderId]
		if !extender.active && extender.revokeTime == nil {
			continue
		}
		for _, ipVersion := range []int{4, 6} {
			address, ok := extender.addresses[ipVersion]
			if !ok || address.active || address.failures < minConsecutiveProbeFailures {
				continue
			}
			if address.lastWorkTime().Before(lastWorkedSince) {
				continue
			}
			target := &model.NetworkExtenderRecoveryProbeTarget{
				NetworkExtenderProbeTarget: *testChurnProbeTarget(extender, address),
				LastWorkTime:               address.lastWorkTime(),
				LastProbeTime:              address.lastProbeTime,
				RevokeTime:                 extender.revokeTime,
			}
			targets = append(targets, target)
		}
	}
	slices.SortStableFunc(targets, func(a *model.NetworkExtenderRecoveryProbeTarget, b *model.NetworkExtenderRecoveryProbeTarget) int {
		switch {
		case a.LastProbeTime == nil && b.LastProbeTime == nil:
			return 0
		case a.LastProbeTime == nil:
			return -1
		case b.LastProbeTime == nil:
			return 1
		default:
			return a.LastProbeTime.Compare(*b.LastProbeTime)
		}
	})
	return targets
}

// The model's restore (model.RecordNetworkExtenderRecoveryProbeResult). The
// record is issued at the probe's time, or just after the revocation when the
// clocks disagree, as the model issues it at its own.
func (self *testChurnStore) recordRecovery(
	ctx context.Context,
	target *model.NetworkExtenderRecoveryProbeTarget,
	success bool,
	probeTime time.Time,
	lastWorkedSince time.Time,
	minConsecutiveProbeFailures int,
	signRecord model.NetworkExtenderRecordSigner,
) *model.NetworkExtenderRecoveryOutcome {
	outcome := &model.NetworkExtenderRecoveryOutcome{}
	if ctx.Err() != nil {
		panic(ctx.Err())
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	extender, ok := self.extenders[target.ExtenderId]
	if !ok {
		return outcome
	}
	address, ok := extender.addresses[target.IpVersion]
	if !ok || address.active || address.ip != target.Ip {
		return outcome
	}
	probeTime = probeTime.UTC()
	if !success {
		address.lastProbeTime = &probeTime
		return outcome
	}
	if !extender.active && extender.revokeTime == nil {
		return outcome
	}
	if address.failures < minConsecutiveProbeFailures || address.lastWorkTime().Before(lastWorkedSince) {
		return outcome
	}

	address.active = true
	address.failures = 0
	address.lastProbeTime = &probeTime
	address.lastProbeSuccessTime = &probeTime
	outcome.AddressRestored = true

	issueTime := probeTime
	if extender.revokeTime != nil {
		if revokedAfter := extender.revokeTime.Add(time.Millisecond); issueTime.Before(revokedAfter) {
			issueTime = revokedAfter
		}
	}
	if !extender.active {
		outcome.ExtenderRestored = true
		outcome.RevokedDuration = max(0, probeTime.Sub(*extender.revokeTime))
		extender.active = true
		extender.revokeTime = nil
	}

	addresses := []*model.NetworkExtenderAddress{}
	for _, ipVersion := range []int{4, 6} {
		if sibling, ok := extender.addresses[ipVersion]; ok && sibling.active {
			addresses = append(addresses, &model.NetworkExtenderAddress{
				ExtenderId:   extender.extenderId,
				IpVersion:    ipVersion,
				Ip:           sibling.ip,
				Carriers:     []string{connect.ExtenderCarrierTcp},
				ActivateTime: sibling.activateTime,
				Active:       true,
			})
		}
	}
	extender.recordIssueTime = &issueTime
	message, err := signRecord(testChurnModelExtender(extender), addresses, issueTime)
	if err != nil {
		panic(err)
	}
	self.publishes = append(self.publishes, testChurnPublish{
		extenderId: extender.extenderId,
		kind:       model.NetworkExtenderPublishKindRecord,
		message:    message,
	})
	outcome.RecordQueued = true
	return outcome
}
