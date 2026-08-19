// This file defines deterministic, hashable live network schedules and the
// runner shared by direct calibration and full production-route measurements.
package perfvar

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	cellEdgeRateCollapseRecoverName = "cell-edge-rate-collapse-recover"
	cellEdgeOutage1sRecoverName     = "cell-edge-outage-1s-recover"
	cellEdgeMtuReductionRecoverName = "cell-edge-mtu-reduction-recover"
)

// A schedule is part of scenario identity. Events use application-device
// orientation: forward is device upload and reverse is device download.
type profileSchedule struct {
	Name   string         `json:"name"`
	Events []profileEvent `json:"events"`
}

// A measured event records scheduler accuracy without serializing unstable
// absolute wall-clock timestamps into PERFVAR results.
type profileEventObservation struct {
	Name              string        `json:"name"`
	ScheduledAfter    time.Duration `json:"scheduled_after_nanoseconds"`
	FirstAppliedAfter time.Duration `json:"first_applied_after_nanoseconds"`
	LastAppliedAfter  time.Duration `json:"last_applied_after_nanoseconds"`
	LinkNames         []string      `json:"link_names"`
}

// Dynamic profiles start on the moderate cell edge and identify their live
// trace in the ordinary profile filter. This avoids a second, ambiguous
// campaign-selection dimension.
func dynamicCellEdgeNetworkProfiles(seed int64) map[string]networkProfile {
	base := cellEdgeNetworkProfiles(seed)[cellEdge5mDown1mUpName]
	build := func(name string, note string) networkProfile {
		profile := base
		profile.Name = name
		profile.SourceNote = note
		return profile
	}
	return map[string]networkProfile{
		cellEdgeRateCollapseRecoverName: build(
			cellEdgeRateCollapseRecoverName,
			"synthetic device cell-edge fast-to-slow-to-fast schedule; not a field-network claim",
		),
		cellEdgeOutage1sRecoverName: build(
			cellEdgeOutage1sRecoverName,
			"synthetic device cell-edge one-second outage schedule; not a field-network claim",
		),
		cellEdgeMtuReductionRecoverName: build(
			cellEdgeMtuReductionRecoverName,
			"synthetic device cell-edge live outer-MTU reduction schedule; not a field-network claim",
		),
	}
}

// A copied direction can change its live outer MTU without changing the TUN's
// advertised inner MTU. MTU drops are expected only during this explicit phase.
func profileDirectionWithOuterMtu(profile linkProfile, outerMtu int) linkProfile {
	profile.OuterMtu = outerMtu
	profile.BurstByteCount = outerMtu
	profile.QueuePacketCount = max(8, (profile.QueueByteCount+outerMtu-1)/outerMtu)
	profile.AllowMtuDrops = true
	return profile
}

// Rate schedules isolate capacity from MTU. The outage and MTU schedules then
// isolate their own terminal causes so a candidate can be ranked diagnostically.
func profileScheduleForName(name string, seed int64) *profileSchedule {
	profiles := cellEdgeNetworkProfiles(seed)
	moderate := profiles[cellEdge5mDown1mUpName]
	withRates := func(uploadBitsPerSecond int64, downloadBitsPerSecond int64) networkProfile {
		profile := moderate
		profile.Forward.RateBitsPerSecond = uploadBitsPerSecond
		profile.Reverse.RateBitsPerSecond = downloadBitsPerSecond
		for _, direction := range []*linkProfile{&profile.Forward, &profile.Reverse} {
			direction.QueueByteCount = bandwidthDelayQueue(direction.RateBitsPerSecond, 100*time.Millisecond)
			direction.QueuePacketCount = max(
				8,
				(direction.QueueByteCount+direction.OuterMtu-1)/direction.OuterMtu,
			)
		}
		return profile
	}
	medium := withRates(250_000, 1_000_000)
	slow := withRates(64_000, 256_000)
	event := func(name string, after time.Duration, profile networkProfile) profileEvent {
		forward := profile.Forward
		reverse := profile.Reverse
		return profileEvent{
			Name:    name,
			After:   after,
			Forward: &forward,
			Reverse: &reverse,
		}
	}
	switch name {
	case cellEdgeRateCollapseRecoverName:
		return &profileSchedule{
			Name: name,
			Events: []profileEvent{
				event("rate-collapse-256k-down-64k-up", 500*time.Millisecond, slow),
				event("rate-partial-recovery-1m-down-250k-up", 2500*time.Millisecond, medium),
				event("rate-full-recovery-5m-down-1m-up", 4*time.Second, moderate),
			},
		}
	case cellEdgeOutage1sRecoverName:
		outage := moderate
		outage.Forward.Blackhole = true
		outage.Reverse.Blackhole = true
		return &profileSchedule{
			Name: name,
			Events: []profileEvent{
				event("outage-start", 750*time.Millisecond, outage),
				event("outage-recovery", 1750*time.Millisecond, moderate),
			},
		}
	case cellEdgeMtuReductionRecoverName:
		reduced := moderate
		reduced.Forward = profileDirectionWithOuterMtu(reduced.Forward, 1280)
		reduced.Reverse = profileDirectionWithOuterMtu(reduced.Reverse, 1280)
		return &profileSchedule{
			Name: name,
			Events: []profileEvent{
				event("outer-mtu-reduction-1280", 750*time.Millisecond, reduced),
				event("outer-mtu-recovery-1400", 1750*time.Millisecond, moderate),
			},
		}
	default:
		return nil
	}
}

// Every event boundary and direction must be complete before a workload can
// claim to have replayed the trace.
func (self profileSchedule) validate() error {
	if self.Name == "" {
		return errors.New("profile schedule name is empty")
	}
	if len(self.Events) == 0 {
		return fmt.Errorf("profile schedule %q has no events", self.Name)
	}
	previousAfter := time.Duration(0)
	names := map[string]bool{}
	for eventIndex, event := range self.Events {
		if event.Name == "" || names[event.Name] {
			return fmt.Errorf("profile schedule %q event %d has an empty or duplicate name", self.Name, eventIndex)
		}
		names[event.Name] = true
		if event.After <= previousAfter {
			return fmt.Errorf(
				"profile schedule %q event %q boundary %s is not strictly after %s",
				self.Name,
				event.Name,
				event.After,
				previousAfter,
			)
		}
		if event.AfterDeliveredBytes != 0 || event.Rebind || event.Kick {
			return fmt.Errorf("profile schedule %q event %q uses an unsupported trigger", self.Name, event.Name)
		}
		if event.Forward == nil || event.Reverse == nil {
			return fmt.Errorf("profile schedule %q event %q is missing a direction", self.Name, event.Name)
		}
		profile := networkProfile{
			Name:     event.Name,
			InnerMtu: 576,
			Forward:  *event.Forward,
			Reverse:  *event.Reverse,
		}
		if err := profile.validate(); err != nil {
			return fmt.Errorf("profile schedule %q event %q: %w", self.Name, event.Name, err)
		}
		previousAfter = event.After
	}
	return nil
}

// A conservative capacity bound prevents the direct calibration from ending
// before the last event. Burst credit is included for every phase.
func perfvarScheduleMinimumPayloadByteCount(scenario perfvarScenario) int64 {
	if scenario.ProfileSchedule == nil || len(scenario.ProfileSchedule.Events) == 0 {
		return 0
	}
	profileForDirection := func(forward linkProfile, reverse linkProfile) linkProfile {
		if scenario.Direction == perfvarDirectionDownload {
			return reverse
		}
		return forward
	}
	current := profileForDirection(scenario.Profile.Forward, scenario.Profile.Reverse)
	previousAfter := time.Duration(0)
	maximumBytes := int64(current.BurstByteCount)
	for _, event := range scenario.ProfileSchedule.Events {
		phaseDuration := event.After - previousAfter
		if !current.Blackhole {
			maximumBytes += current.RateBitsPerSecond * phaseDuration.Nanoseconds() /
				(8 * int64(time.Second))
		}
		current = profileForDirection(*event.Forward, *event.Reverse)
		maximumBytes += int64(current.BurstByteCount)
		previousAfter = event.After
	}
	return maximumBytes + 1
}

// Dynamic schedules currently have exact measurement hooks only for one-flow
// TCP. Unsupported combinations are rejected instead of silently measuring a
// static path under a dynamic label.
func validatePerfvarProfileScheduleScenario(scenario perfvarScenario) error {
	if scenario.ProfileSchedule == nil {
		return nil
	}
	if err := scenario.ProfileSchedule.validate(); err != nil {
		return err
	}
	if scenario.Workload != perfvarWorkloadTCP && scenario.Workload != perfvarWorkloadTCPWarmed {
		return fmt.Errorf(
			"PERFVAR profile schedule %q supports tcp and tcp-warmed, not %q",
			scenario.ProfileSchedule.Name,
			scenario.Workload,
		)
	}
	if scenario.Topology != perfvarTopologyOneHop || scenario.ExtenderCount != 0 {
		return fmt.Errorf(
			"PERFVAR profile schedule %q requires one-hop topology with no extender",
			scenario.ProfileSchedule.Name,
		)
	}
	minimumPayloadByteCount := perfvarScheduleMinimumPayloadByteCount(scenario)
	if scenario.PayloadByteCount < minimumPayloadByteCount {
		return fmt.Errorf(
			"PERFVAR profile schedule %q %s payload %d must be at least %d bytes to cross its last event",
			scenario.ProfileSchedule.Name,
			scenario.Direction,
			scenario.PayloadByteCount,
			minimumPayloadByteCount,
		)
	}
	return nil
}

type profileScheduleApply func(
	context.Context,
	profileEvent,
	time.Time,
) ([]networkProfileUpdateResult, error)

// Direct calibration always places the application device on the left side of
// its one physical link.
func applyTunPathProfileEvent(
	ctx context.Context,
	path *tunPath,
	event profileEvent,
	scheduledTime time.Time,
) ([]networkProfileUpdateResult, error) {
	return path.network.updateNodeProfiles(
		ctx,
		"left",
		event.Name,
		scheduledTime,
		event.Forward,
		event.Reverse,
	)
}

// Direct exchange calibration collapses the device and provider access links
// into one direction. Live events must preserve the unchanged clean-provider
// segment just as the initial calibration profile does.
func perfvarCalibrationProfileEvent(
	scenario perfvarScenario,
	event profileEvent,
) (profileEvent, error) {
	if event.Forward == nil || event.Reverse == nil {
		return profileEvent{}, fmt.Errorf("calibration profile event %q is missing a direction", event.Name)
	}
	if scenario.Route == fullTunRouteP2pFast || scenario.Route == fullTunRouteP2pLegacy {
		return event, nil
	}
	if scenario.Topology == perfvarTopologySplitExchange {
		return profileEvent{}, errors.New("live profile schedules do not support split exchange")
	}
	forward := combinedExchangeLink(*event.Forward, scenario.ProviderAccessProfile.Reverse)
	reverse := combinedExchangeLink(scenario.ProviderAccessProfile.Forward, *event.Reverse)
	event.Forward = &forward
	event.Reverse = &reverse
	return event, nil
}

// Full-route schedules change only the device access path. Exchange provider
// and internal links remain clean; direct P2P additionally changes its live
// data carrier with the endpoint orientation translated explicitly.
func applyFullTunProfileEvent(
	ctx context.Context,
	path *fullTunPath,
	event profileEvent,
	scheduledTime time.Time,
) ([]networkProfileUpdateResult, error) {
	if path.deviceCarrierNode == "" {
		return nil, errors.New("full-TUN profile event has no device carrier node")
	}
	updates, err := path.environment.network.updateNodeProfiles(
		ctx,
		path.deviceCarrierNode,
		event.Name,
		scheduledTime,
		event.Forward,
		event.Reverse,
	)
	if err != nil {
		return updates, err
	}
	if path.streamP2pNetwork != nil {
		return updates, errors.New("live profile schedules do not support multihop P2P")
	}
	if path.p2pNetwork != nil {
		if event.Forward == nil || event.Reverse == nil {
			return updates, errors.New("direct P2P profile event is missing a direction")
		}
		p2pUpdates, updateErr := path.p2pNetwork.updateProfiles(
			ctx,
			event.Name,
			scheduledTime,
			// Pion's forward physical link is provider-to-device.
			*event.Reverse,
			*event.Forward,
		)
		updates = append(updates, p2pUpdates...)
		if updateErr != nil {
			return updates, updateErr
		}
	}
	return updates, nil
}

type profileScheduleRunResult struct {
	observations []profileEventObservation
	err          error
}

// One run owns its timer goroutine and joins it at application completion.
type profileScheduleRun struct {
	cancel     context.CancelFunc
	done       <-chan profileScheduleRunResult
	finishOnce sync.Once
	result     profileScheduleRunResult
}

func startProfileScheduleRun(
	ctx context.Context,
	schedule profileSchedule,
	apply profileScheduleApply,
) *profileScheduleRun {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan profileScheduleRunResult, 1)
	startTime := time.Now()
	go func() {
		result := profileScheduleRunResult{
			observations: make([]profileEventObservation, 0, len(schedule.Events)),
		}
		for _, event := range schedule.Events {
			scheduledTime := startTime.Add(event.After)
			if wait := time.Until(scheduledTime); 0 < wait {
				timer := time.NewTimer(wait)
				select {
				case <-runCtx.Done():
					timer.Stop()
					result.err = fmt.Errorf("apply profile event %q: %w", event.Name, runCtx.Err())
					done <- result
					return
				case <-timer.C:
				}
			}
			updates, err := apply(runCtx, event, scheduledTime)
			if err != nil {
				result.err = fmt.Errorf("apply profile event %q: %w", event.Name, err)
				done <- result
				return
			}
			if len(updates) == 0 {
				result.err = fmt.Errorf("profile event %q updated no links", event.Name)
				done <- result
				return
			}
			observation := profileEventObservation{
				Name:              event.Name,
				ScheduledAfter:    event.After,
				FirstAppliedAfter: updates[0].ActualTime.Sub(startTime),
				LastAppliedAfter:  updates[0].ActualTime.Sub(startTime),
				LinkNames:         make([]string, 0, len(updates)),
			}
			seenLinks := map[string]bool{}
			for _, update := range updates {
				if update.EventName != event.Name || !update.ScheduledTime.Equal(scheduledTime) {
					result.err = fmt.Errorf(
						"profile event %q returned mismatched update boundary %+v",
						event.Name,
						update,
					)
					done <- result
					return
				}
				if update.ActualTime.Before(scheduledTime) {
					result.err = fmt.Errorf(
						"profile event %q link %q applied before its scheduled boundary",
						event.Name,
						update.LinkName,
					)
					done <- result
					return
				}
				if update.LinkName == "" || seenLinks[update.LinkName] {
					result.err = fmt.Errorf(
						"profile event %q returned an empty or duplicate link name %q",
						event.Name,
						update.LinkName,
					)
					done <- result
					return
				}
				seenLinks[update.LinkName] = true
				observation.FirstAppliedAfter = min(observation.FirstAppliedAfter, update.ActualTime.Sub(startTime))
				observation.LastAppliedAfter = max(observation.LastAppliedAfter, update.ActualTime.Sub(startTime))
				observation.LinkNames = append(observation.LinkNames, update.LinkName)
			}
			slices.Sort(observation.LinkNames)
			result.observations = append(result.observations, observation)
		}
		done <- result
	}()
	return &profileScheduleRun{cancel: cancel, done: done}
}

// Finish cancels events that missed application completion, joins the runner,
// and distinguishes an incomplete trace from the workload's own error.
func (self *profileScheduleRun) Finish(expectedEventCount int) ([]profileEventObservation, error) {
	if self == nil {
		return nil, errors.New("profile schedule did not reach its start hook")
	}
	self.finishOnce.Do(func() {
		self.cancel()
		self.result = <-self.done
		if self.result.err == nil && len(self.result.observations) != expectedEventCount {
			self.result.err = fmt.Errorf(
				"workload ended after %d/%d profile events",
				len(self.result.observations),
				expectedEventCount,
			)
		}
	})
	return self.result.observations, self.result.err
}

// Adds schedule observations to a workload result while preserving both
// independent failure causes.
func finishScheduledWorkload(
	result workloadResult,
	measureErr error,
	schedule *profileSchedule,
	run *profileScheduleRun,
) (workloadResult, error) {
	if schedule == nil {
		return result, measureErr
	}
	observations, scheduleErr := run.Finish(len(schedule.Events))
	result.ProfileEvents = observations
	return result, errors.Join(measureErr, scheduleErr)
}

// Checked-in traces pin timing, directionality, rate, outage, and MTU axes so
// edits cannot silently turn one diagnostic schedule into another.
func TestDynamicCellEdgeProfileSchedulesResolveExactEvents(t *testing.T) {
	profiles := allNetworkProfiles(20260817)
	for _, name := range []string{
		cellEdgeRateCollapseRecoverName,
		cellEdgeOutage1sRecoverName,
		cellEdgeMtuReductionRecoverName,
	} {
		profile, ok := profiles[name]
		if !ok || profile.Name != name {
			t.Fatalf("dynamic profile %q missing: %+v", name, profile)
		}
		schedule := profileScheduleForName(name, profile.Seed)
		if schedule == nil || schedule.Name != name {
			t.Fatalf("dynamic schedule %q missing: %+v", name, schedule)
		}
		if err := schedule.validate(); err != nil {
			t.Fatalf("dynamic schedule %q: %v", name, err)
		}
	}

	rate := profileScheduleForName(cellEdgeRateCollapseRecoverName, 20260817)
	if len(rate.Events) != 3 ||
		rate.Events[0].After != 500*time.Millisecond ||
		rate.Events[1].After != 2500*time.Millisecond ||
		rate.Events[2].After != 4*time.Second {
		t.Fatalf("rate schedule boundaries=%+v", rate.Events)
	}
	if rate.Events[0].Forward.RateBitsPerSecond != 64_000 ||
		rate.Events[0].Reverse.RateBitsPerSecond != 256_000 ||
		rate.Events[1].Forward.RateBitsPerSecond != 250_000 ||
		rate.Events[1].Reverse.RateBitsPerSecond != 1_000_000 ||
		rate.Events[2].Forward.RateBitsPerSecond != 1_000_000 ||
		rate.Events[2].Reverse.RateBitsPerSecond != 5_000_000 {
		t.Fatalf("rate schedule directionality=%+v", rate.Events)
	}
	for _, event := range rate.Events {
		if event.Forward.OuterMtu != 1400 || event.Reverse.OuterMtu != 1400 {
			t.Fatalf("rate schedule changed MTU in %q: %+v", event.Name, event)
		}
		moderate := profiles[cellEdge5mDown1mUpName]
		if event.Forward.BaseDelay != moderate.Forward.BaseDelay ||
			event.Reverse.BaseDelay != moderate.Reverse.BaseDelay ||
			event.Forward.Jitter != moderate.Forward.Jitter ||
			event.Reverse.Jitter != moderate.Reverse.Jitter ||
			event.Forward.LossModel != moderate.Forward.LossModel ||
			event.Reverse.LossModel != moderate.Reverse.LossModel ||
			event.Forward.LossProbability != moderate.Forward.LossProbability ||
			event.Reverse.LossProbability != moderate.Reverse.LossProbability {
			t.Fatalf("rate-only schedule changed another impairment in %q: %+v", event.Name, event)
		}
	}

	outage := profileScheduleForName(cellEdgeOutage1sRecoverName, 20260817)
	if len(outage.Events) != 2 ||
		outage.Events[0].After != 750*time.Millisecond ||
		outage.Events[1].After != 1750*time.Millisecond ||
		!outage.Events[0].Forward.Blackhole || !outage.Events[0].Reverse.Blackhole ||
		outage.Events[1].Forward.Blackhole || outage.Events[1].Reverse.Blackhole {
		t.Fatalf("outage schedule=%+v", outage.Events)
	}

	mtu := profileScheduleForName(cellEdgeMtuReductionRecoverName, 20260817)
	if len(mtu.Events) != 2 ||
		mtu.Events[0].Forward.OuterMtu != 1280 || mtu.Events[0].Reverse.OuterMtu != 1280 ||
		!mtu.Events[0].Forward.AllowMtuDrops || !mtu.Events[0].Reverse.AllowMtuDrops ||
		mtu.Events[1].Forward.OuterMtu != 1400 || mtu.Events[1].Reverse.OuterMtu != 1400 ||
		mtu.Events[1].Forward.AllowMtuDrops || mtu.Events[1].Reverse.AllowMtuDrops {
		t.Fatalf("MTU schedule=%+v", mtu.Events)
	}
}

// Selection exposes each dynamic trace through the normal profile filter and
// rejects a payload that direct traffic could finish before the final event.
func TestPerfvarDynamicProfileScenarioDefaultsAndBounds(t *testing.T) {
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE": "p2p-fast,p2p-legacy,exchange-h1,exchange-h3",
		"CONNECT_PERFVAR_PROFILE": strings.Join([]string{
			cellEdgeRateCollapseRecoverName,
			cellEdgeOutage1sRecoverName,
			cellEdgeMtuReductionRecoverName,
		}, ","),
		"CONNECT_PERFVAR_WORKLOAD":  "tcp",
		"CONNECT_PERFVAR_DIRECTION": "upload,download",
		"CONNECT_PERFVAR_TOPOLOGY":  "one-hop",
	}
	config, err := loadPerfvarConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 3*4*2 {
		t.Fatalf("dynamic scenario count=%d want=%d", len(scenarios), 3*4*2)
	}
	for _, scenario := range scenarios {
		if scenario.ProfileSchedule == nil || scenario.ProfileSchedule.Name != scenario.Profile.Name {
			t.Fatalf("dynamic scenario omitted schedule: %+v", scenario)
		}
		if scenario.ProviderAccessProfile.Name != "clean-lan" ||
			scenario.PayloadByteCount != 2*1024*1024 {
			t.Fatalf("dynamic scenario defaults=%+v", scenario)
		}
		if err := validatePerfvarProfileScheduleScenario(scenario); err != nil {
			t.Fatalf("dynamic scenario %s/%s/%s: %v", scenario.Route, scenario.Profile.Name, scenario.Direction, err)
		}
	}
	hashScenario := scenarios[0]
	originalHash, err := hashScenario.profilesHash()
	if err != nil {
		t.Fatal(err)
	}
	changedSchedule := *hashScenario.ProfileSchedule
	changedSchedule.Events = slices.Clone(changedSchedule.Events)
	changedSchedule.Events[0].After += time.Nanosecond
	hashScenario.ProfileSchedule = &changedSchedule
	changedHash, err := hashScenario.profilesHash()
	if err != nil {
		t.Fatal(err)
	}
	if changedHash == originalHash {
		t.Fatal("profile schedule timing did not change the profile hash")
	}
	originalTrace, err := perfvarTraceForRun(scenarios[0], 1)
	if err != nil {
		t.Fatal(err)
	}
	changedTrace, err := perfvarTraceForRun(hashScenario, 1)
	if err != nil {
		t.Fatal(err)
	}
	if changedTrace.IdentityHash == originalTrace.IdentityHash {
		t.Fatal("profile schedule timing did not change the trace identity")
	}

	values["CONNECT_PERFVAR_ROUTE"] = "exchange-h1"
	values["CONNECT_PERFVAR_PROFILE"] = cellEdgeMtuReductionRecoverName
	values["CONNECT_PERFVAR_DIRECTION"] = "download"
	values["CONNECT_PERFVAR_BYTE_COUNT"] = "1"
	config, err = loadPerfvarConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePerfvarScenarios(config); err == nil || !strings.Contains(err.Error(), "cross its last event") {
		t.Fatalf("short dynamic payload error=%v", err)
	}

	values["CONNECT_PERFVAR_PROFILE"] = "clean-lan"
	values["CONNECT_PERFVAR_TOPOLOGY"] = "split-exchange"
	values["CONNECT_PERFVAR_INTERNAL_PROFILE"] = cellEdgeRateCollapseRecoverName
	delete(values, "CONNECT_PERFVAR_BYTE_COUNT")
	if _, err := loadPerfvarConfig(func(name string) string { return values[name] }); err == nil ||
		!strings.Contains(err.Error(), "CONNECT_PERFVAR_INTERNAL_PROFILE has unknown value") {
		t.Fatalf("dynamic internal-profile error=%v", err)
	}
}

// The runner uses acknowledged link timestamps, sorts link identities, and
// owns cancellation when the application ends before a future event.
func TestProfileScheduleRunnerCompletionAndEarlyFinish(t *testing.T) {
	profile := cellEdgeNetworkProfiles(9201)[cellEdge5mDown1mUpName]
	event := func(name string, after time.Duration) profileEvent {
		forward := profile.Forward
		reverse := profile.Reverse
		return profileEvent{Name: name, After: after, Forward: &forward, Reverse: &reverse}
	}
	schedule := profileSchedule{
		Name: "runner-test",
		Events: []profileEvent{
			event("first", 5*time.Millisecond),
			event("second", 10*time.Millisecond),
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	applied := make(chan string, len(schedule.Events))
	run := startProfileScheduleRun(
		ctx,
		schedule,
		func(_ context.Context, event profileEvent, scheduledTime time.Time) ([]networkProfileUpdateResult, error) {
			actualTime := time.Now()
			applied <- event.Name
			return []networkProfileUpdateResult{
				{LinkName: "z-link", EventName: event.Name, ScheduledTime: scheduledTime, ActualTime: actualTime},
				{LinkName: "a-link", EventName: event.Name, ScheduledTime: scheduledTime, ActualTime: actualTime},
			}, nil
		},
	)
	for range schedule.Events {
		select {
		case <-ctx.Done():
			t.Fatalf("wait for schedule events: %v", ctx.Err())
		case <-applied:
		}
	}
	observations, err := run.Finish(len(schedule.Events))
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 {
		t.Fatalf("schedule observations=%+v", observations)
	}
	for eventIndex, observation := range observations {
		if observation.Name != schedule.Events[eventIndex].Name ||
			observation.ScheduledAfter != schedule.Events[eventIndex].After ||
			observation.FirstAppliedAfter < observation.ScheduledAfter ||
			observation.LastAppliedAfter < observation.FirstAppliedAfter ||
			!slices.Equal(observation.LinkNames, []string{"a-link", "z-link"}) {
			t.Fatalf("schedule observation %d=%+v", eventIndex, observation)
		}
	}

	earlySchedule := profileSchedule{Name: "early", Events: []profileEvent{event("future", time.Hour)}}
	earlyApplied := make(chan struct{}, 1)
	early := startProfileScheduleRun(
		ctx,
		earlySchedule,
		func(context.Context, profileEvent, time.Time) ([]networkProfileUpdateResult, error) {
			earlyApplied <- struct{}{}
			return nil, errors.New("future event unexpectedly applied")
		},
	)
	if observations, err := early.Finish(1); err == nil || len(observations) != 0 ||
		!strings.Contains(err.Error(), "profile event \"future\"") {
		t.Fatalf("early finish observations=%+v err=%v", observations, err)
	}
	select {
	case <-earlyApplied:
		t.Fatal("canceled future event was applied")
	default:
	}
}

// The route-neutral calibration uses the same measured-start hook and live
// profile machinery as a production-route run, and retains both link updates
// in the serialized workload observation.
func TestMeasurePerfvarUnderlayReplaysLiveProfileSchedule(t *testing.T) {
	initialDirection := newLinkProfile(
		1_000_000,
		time.Millisecond,
		0,
		0,
		100*time.Millisecond,
	)
	initialDirection.BurstByteCount = 1400
	initialDirection.OuterMtu = 1400
	profile := networkProfile{
		Name:       "short-live-underlay",
		Seed:       9251,
		InnerMtu:   1320,
		Forward:    initialDirection,
		Reverse:    initialDirection,
		SourceNote: "deterministic short live schedule test",
	}
	slow := initialDirection
	slow.RateBitsPerSecond = 250_000
	slow.QueueByteCount = bandwidthDelayQueue(slow.RateBitsPerSecond, 100*time.Millisecond)
	forwardSlow := slow
	reverseSlow := slow
	forwardRestore := initialDirection
	reverseRestore := initialDirection
	schedule := &profileSchedule{
		Name: "short-live-underlay",
		Events: []profileEvent{
			{
				Name:    "slow",
				After:   15 * time.Millisecond,
				Forward: &forwardSlow,
				Reverse: &reverseSlow,
			},
			{
				Name:    "restore",
				After:   60 * time.Millisecond,
				Forward: &forwardRestore,
				Reverse: &reverseRestore,
			},
		},
	}
	scenario := perfvarScenario{
		Route:                 fullTunRouteExchangeH1,
		Profile:               profile,
		ProfileSchedule:       schedule,
		ProviderAccessProfile: profile,
		Workload:              perfvarWorkloadTCP,
		Direction:             perfvarDirectionUpload,
		Topology:              perfvarTopologyOneHop,
		Resource:              perfvarResourceDefault,
		PayloadByteCount:      128 * 1024,
		FlowCount:             1,
	}
	if err := validatePerfvarProfileScheduleScenario(scenario); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	result, err := measurePerfvarUnderlay(ctx, scenario)
	if err != nil {
		t.Fatal(err)
	}
	if result.UsefulByteCount != scenario.PayloadByteCount ||
		result.ContentHash != deterministicPayloadHash(scenario.PayloadByteCount) ||
		result.Duration < schedule.Events[len(schedule.Events)-1].After {
		t.Fatalf("scheduled underlay result=%+v", result)
	}
	if result.ForwardLink.ProfileUpdateCount != 2 || result.ReverseLink.ProfileUpdateCount != 2 ||
		len(result.ProfileEvents) != 2 {
		t.Fatalf("scheduled underlay events=%+v forward=%+v reverse=%+v", result.ProfileEvents, result.ForwardLink, result.ReverseLink)
	}
	for _, observation := range result.ProfileEvents {
		if !slices.Equal(observation.LinkNames, []string{"left->right", "right->left"}) {
			t.Fatalf("scheduled underlay link scope=%+v", observation)
		}
	}
}

// Exchange calibration retains the unchanged provider segment at every live
// event, while direct P2P uses the device-to-provider event unchanged.
func TestPerfvarCalibrationProfileEventPreservesProviderAccess(t *testing.T) {
	profiles := allNetworkProfiles(9271)
	device := profiles[cellEdge256kDown64kUpName]
	provider := profiles["clean-lan"]
	forward := device.Forward
	reverse := device.Reverse
	event := profileEvent{Name: "change", Forward: &forward, Reverse: &reverse}
	exchangeEvent, err := perfvarCalibrationProfileEvent(perfvarScenario{
		Route:                 fullTunRouteExchangeH3,
		Topology:              perfvarTopologyOneHop,
		ProviderAccessProfile: provider,
	}, event)
	if err != nil {
		t.Fatal(err)
	}
	if exchangeEvent.Forward.BaseDelay != device.Forward.BaseDelay+provider.Reverse.BaseDelay ||
		exchangeEvent.Reverse.BaseDelay != provider.Forward.BaseDelay+device.Reverse.BaseDelay ||
		exchangeEvent.Forward.RateBitsPerSecond != device.Forward.RateBitsPerSecond ||
		exchangeEvent.Reverse.RateBitsPerSecond != device.Reverse.RateBitsPerSecond {
		t.Fatalf("composed calibration event=%+v", exchangeEvent)
	}
	p2pEvent, err := perfvarCalibrationProfileEvent(perfvarScenario{
		Route:    fullTunRouteP2pFast,
		Topology: perfvarTopologyOneHop,
	}, event)
	if err != nil {
		t.Fatal(err)
	}
	if p2pEvent.Forward != event.Forward || p2pEvent.Reverse != event.Reverse {
		t.Fatalf("direct P2P calibration event changed: before=%+v after=%+v", event, p2pEvent)
	}
}

// Full-route application changes touch only device access and translate the
// provider-left P2P fixture back to application-oriented directions.
func TestApplyFullTunProfileEventScopesDeviceAndP2pDirections(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	profiles := cellEdgeNetworkProfiles(9301)
	initial := profiles[cellEdge5mDown1mUpName]
	changed := profiles[cellEdge1mDown250kUpName]
	network := newSimulatedIPNetwork(ctx)
	defer network.close()
	addLink := func(source string, destination string, profile linkProfile, seed int64) {
		network.links[tunLinkKey{source: source, destination: destination}] =
			newDirectionalLink(ctx, profile, seed, func([]byte) bool { return true })
	}
	addLink("client-1", "edge", initial.Forward, 1)
	addLink("edge", "client-1", initial.Reverse, 2)
	addLink("client-2", "edge", initial.Forward, 3)
	addLink("edge", "client-2", initial.Reverse, 4)
	p2p, err := newP2pNetwork(oneHopP2pNetworkProfile(initial))
	if err != nil {
		t.Fatal(err)
	}
	defer p2p.close()
	p2p.forwardLink.stateLock.Lock()
	initialP2pForward := p2p.forwardLink.profile
	p2p.forwardLink.stateLock.Unlock()
	p2p.reverseLink.stateLock.Lock()
	initialP2pReverse := p2p.reverseLink.profile
	p2p.reverseLink.stateLock.Unlock()
	if initialP2pForward.RateBitsPerSecond != initial.Reverse.RateBitsPerSecond ||
		initialP2pReverse.RateBitsPerSecond != initial.Forward.RateBitsPerSecond {
		t.Fatalf(
			"initial P2P physical directionality forward=%d reverse=%d",
			initialP2pForward.RateBitsPerSecond,
			initialP2pReverse.RateBitsPerSecond,
		)
	}
	path := &fullTunPath{
		environment:       &routeEnvironment{network: network},
		deviceCarrierNode: "client-2",
		p2pNetwork:        p2p,
	}
	forward := changed.Forward
	reverse := changed.Reverse
	updates, err := applyFullTunProfileEvent(ctx, path, profileEvent{
		Name:    "device-change",
		Forward: &forward,
		Reverse: &reverse,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 4 {
		t.Fatalf("device/P2P update count=%d updates=%+v", len(updates), updates)
	}
	access := network.snapshotProfiles()
	if access["client-1->edge"].RateBitsPerSecond != initial.Forward.RateBitsPerSecond ||
		access["edge->client-1"].RateBitsPerSecond != initial.Reverse.RateBitsPerSecond {
		t.Fatalf("provider access changed: %+v", access)
	}
	if access["client-2->edge"].RateBitsPerSecond != changed.Forward.RateBitsPerSecond ||
		access["edge->client-2"].RateBitsPerSecond != changed.Reverse.RateBitsPerSecond {
		t.Fatalf("device access directionality=%+v", access)
	}
	p2p.forwardLink.stateLock.Lock()
	p2pForward := p2p.forwardLink.profile
	p2p.forwardLink.stateLock.Unlock()
	p2p.reverseLink.stateLock.Lock()
	p2pReverse := p2p.reverseLink.profile
	p2p.reverseLink.stateLock.Unlock()
	if p2pForward.RateBitsPerSecond != changed.Reverse.RateBitsPerSecond ||
		p2pReverse.RateBitsPerSecond != changed.Forward.RateBitsPerSecond {
		t.Fatalf("P2P physical directionality forward=%+v reverse=%+v", p2pForward, p2pReverse)
	}
}
