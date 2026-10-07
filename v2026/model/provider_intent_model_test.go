// Provider intent qualification: the pure state machine (explicit clocks, no
// stores) and the redis and database layers it runs on.
package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

// a fixed synthetic clock for the pure state machine tests
var providerIntentTestStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// An intent connection with no state, or with a failed state past its
// allowance, starts a new attempt whose first check runs at once; any other
// state is kept.
func TestProviderIntentConnectStartsAttemptOnlyWhenAllowed(t *testing.T) {
	now := providerIntentTestStart

	state, started := providerIntentConnectState(nil, now)
	connect.AssertEqual(t, started, true)
	connect.AssertEqual(t, state.Status, ProviderIntentStatusPending)
	connect.AssertEqual(t, state.AttemptTime, now)
	connect.AssertEqual(t, state.GraceEndTime, now.Add(time.Hour))
	connect.AssertEqual(t, state.CheckTime, now)

	attemptTime := now.Add(-2 * time.Hour)
	for _, status := range []ProviderIntentStatus{
		ProviderIntentStatusPending,
		ProviderIntentStatusQualified,
		ProviderIntentStatusNormal,
		ProviderIntentStatusOverLimit,
	} {
		existing := &ProviderIntentState{
			Status:       status,
			AttemptTime:  attemptTime,
			GraceEndTime: attemptTime.Add(providerIntentGraceTimeout),
			CheckTime:    now.Add(time.Hour),
		}
		next, started := providerIntentConnectState(existing, now)
		if started || next != existing {
			t.Fatalf("%s within the allowance: started=%t", status, started)
		}
	}

	// a failed client gets one attempt per allowance, measured from its attempt
	for _, status := range []ProviderIntentStatus{ProviderIntentStatusNormal, ProviderIntentStatusOverLimit} {
		failed := &ProviderIntentState{
			Status:      status,
			AttemptTime: now.Add(-ProviderIntentAttemptAllowance),
		}
		next, started := providerIntentConnectState(failed, now)
		connect.AssertEqual(t, started, true)
		connect.AssertEqual(t, next.Status, ProviderIntentStatusPending)
		connect.AssertEqual(t, next.AttemptTime, now)

		failed.AttemptTime = now.Add(-ProviderIntentAttemptAllowance + time.Millisecond)
		_, started = providerIntentConnectState(failed, now)
		connect.AssertEqual(t, started, false)
	}

	// a pending or qualified client is never restarted by a connection, however old
	for _, status := range []ProviderIntentStatus{ProviderIntentStatusPending, ProviderIntentStatusQualified} {
		old := &ProviderIntentState{
			Status:      status,
			AttemptTime: now.Add(-10 * ProviderIntentAttemptAllowance),
		}
		_, started := providerIntentConnectState(old, now)
		connect.AssertEqual(t, started, false)
	}
}

// The grace is a quarter of the probe refresh.
func TestProviderIntentGraceIsQuarterOfProbeRefresh(t *testing.T) {
	connect.AssertEqual(t, providerIntentGraceTimeout, time.Hour)
	connect.AssertEqual(t, providerIntentGraceTimeout, ProviderEgressProbeRefreshAge/4)
	connect.AssertEqual(t, providerIntentRecheckTimeout, ProviderEgressProbeRefreshAge)
}

// Checks in the grace judge nothing: they recur every extend timeout (to renew
// the probe priority) until the probe cycle is admitted, then wait for the end
// of the grace.
func TestProviderIntentChecksInGraceJudgeNothing(t *testing.T) {
	now := providerIntentTestStart

	ready, event, _ := providerIntentCheckState(newProviderIntentAttempt(now), &providerIntentEvidence{ProbeCycleReady: true}, now)
	connect.AssertEqual(t, ready.Status, ProviderIntentStatusPending)
	connect.AssertEqual(t, ready.CheckTime, now.Add(providerIntentGraceTimeout))
	connect.AssertEqual(t, event, "")

	state := newProviderIntentAttempt(now)

	checkTimes := []time.Time{}
	for state.CheckTime.Before(state.GraceEndTime) {
		_, needed := providerIntentEvidenceSince(state, state.CheckTime)
		connect.AssertEqual(t, needed, false)
		next, event, decideSlot := providerIntentCheckState(state, &providerIntentEvidence{}, state.CheckTime)
		connect.AssertEqual(t, next.Status, ProviderIntentStatusPending)
		connect.AssertEqual(t, event, "")
		connect.AssertEqual(t, decideSlot, false)
		checkTimes = append(checkTimes, next.CheckTime)
		state = next
	}
	connect.AssertEqual(t, checkTimes, []time.Time{
		now.Add(15 * time.Minute),
		now.Add(30 * time.Minute),
		now.Add(45 * time.Minute),
		now.Add(60 * time.Minute),
	})
	connect.AssertEqual(t, state.CheckTime, state.GraceEndTime)

	// a check run early (a connection pulled it forward) changes nothing
	early, event, _ := providerIntentCheckState(state, &providerIntentEvidence{}, state.CheckTime.Add(-time.Minute))
	connect.AssertEqual(t, early, state)
	connect.AssertEqual(t, event, "")
}

// The grace does not close until a probe was attempted, so a broken probe
// system extends it without bound instead of failing the provider.
func TestProviderIntentGraceExtendsUntilProbeAttempted(t *testing.T) {
	attemptTime := providerIntentTestStart
	state := &ProviderIntentState{
		Status:       ProviderIntentStatusPending,
		AttemptTime:  attemptTime,
		GraceEndTime: attemptTime.Add(providerIntentGraceTimeout),
		CheckTime:    attemptTime.Add(providerIntentGraceTimeout),
	}
	// a public provider with no probes at all
	evidence := &providerIntentEvidence{PublicProvider: true}

	now := state.GraceEndTime
	for range 1000 {
		since, needed := providerIntentEvidenceSince(state, now)
		connect.AssertEqual(t, needed, true)
		connect.AssertEqual(t, since, attemptTime)

		next, event, decideSlot := providerIntentCheckState(state, evidence, now)
		connect.AssertEqual(t, next.Status, ProviderIntentStatusPending)
		connect.AssertEqual(t, event, providerIntentEventGraceExtended)
		connect.AssertEqual(t, decideSlot, false)
		connect.AssertEqual(t, next.CheckTime, now.Add(ProviderIntentGraceExtendTimeout))
		// the attempt keeps its start: the probe window covers the whole grace
		connect.AssertEqual(t, next.AttemptTime, attemptTime)
		state = next
		now = next.CheckTime
	}
	// still pending long after the allowance of the attempt
	if now.Sub(attemptTime) < 10*ProviderIntentAttemptAllowance {
		t.Fatalf("extension too short: %s", now.Sub(attemptTime))
	}
	_, started := providerIntentConnectState(state, now)
	connect.AssertEqual(t, started, false)
}

// At the end of the grace, after a probe was attempted, the provider qualifies
// with a registered public provide key and a passing probe in the grace.
// Anything else fails it into a normal slot decision.
func TestProviderIntentGraceEndJudgesPublicProviderAndPassingProbe(t *testing.T) {
	attemptTime := providerIntentTestStart
	now := attemptTime.Add(providerIntentGraceTimeout)
	state := &ProviderIntentState{
		Status:       ProviderIntentStatusPending,
		AttemptTime:  attemptTime,
		GraceEndTime: now,
		CheckTime:    now,
	}
	cases := []struct {
		evidence  providerIntentEvidence
		qualifies bool
	}{
		{providerIntentEvidence{PublicProvider: true, ProbeAttempted: true, ProbePassed: true}, true},
		{providerIntentEvidence{PublicProvider: true, ProbeAttempted: true, ProbePassed: false}, false},
		{providerIntentEvidence{PublicProvider: false, ProbeAttempted: true, ProbePassed: true}, false},
		{providerIntentEvidence{PublicProvider: false, ProbeAttempted: true, ProbePassed: false}, false},
	}
	for _, c := range cases {
		next, event, decideSlot := providerIntentCheckState(state, &c.evidence, now)
		if c.qualifies {
			connect.AssertEqual(t, next.Status, ProviderIntentStatusQualified)
			connect.AssertEqual(t, event, providerIntentEventQualified)
			connect.AssertEqual(t, decideSlot, false)
			connect.AssertEqual(t, next.CheckTime, now.Add(providerIntentRecheckTimeout))
		} else {
			connect.AssertEqual(t, next.Status, ProviderIntentStatusNormal)
			connect.AssertEqual(t, event, providerIntentEventFailed)
			connect.AssertEqual(t, decideSlot, true)
			// the allowance runs from the attempt, so the next attempt is 8h after it
			connect.AssertEqual(t, next.AttemptTime, attemptTime)
			connect.AssertEqual(t, next.CheckTime, attemptTime.Add(ProviderIntentAttemptAllowance))
		}
	}
}

// The 12 hour reliability floor applies only once the client has that much
// history; before that, a new provider is judged on probes alone.
func TestProviderIntentReliabilityFloorNeedsHistory(t *testing.T) {
	now := providerIntentTestStart
	low := 0.2
	high := 0.9
	cases := []struct {
		clientAge  time.Duration
		weight     *float64
		qualifies  bool
		reasonText string
	}{
		{1 * time.Hour, &low, true, "a new provider is not judged on reliability"},
		{12*time.Hour - time.Second, &low, true, "less than the lookback of history"},
		{12 * time.Hour, &low, false, "a full lookback of history under the floor"},
		{30 * 24 * time.Hour, &low, false, "an old client under the floor"},
		{30 * 24 * time.Hour, &high, true, "an old client over the floor"},
		{30 * 24 * time.Hour, nil, true, "missing history is neutral"},
	}
	for _, c := range cases {
		evidence := &providerIntentEvidence{
			PublicProvider:     true,
			ProbeAttempted:     true,
			ProbePassed:        true,
			ClientCreateTime:   now.Add(-c.clientAge),
			ReliabilityWeight:  c.weight,
			ReliabilityMinimum: 0.7,
		}
		if evidence.qualifies(now) != c.qualifies {
			t.Fatalf("%s: qualifies=%t", c.reasonText, !c.qualifies)
		}
		if evidence.requalifies(now) != c.qualifies {
			t.Fatalf("%s: requalifies=%t", c.reasonText, !c.qualifies)
		}
	}
	// the floor is the provider selection gate's floor for the 12 hour lookback
	connect.AssertEqual(t, ClientLookbacks[providerIntentReliabilityLookbackIndex], providerIntentReliabilityHistory)
}

// A qualified provider is re-checked every probe refresh: it stays qualified
// while it passes, a window without any attempted probe is neutral, and a
// failure ends the qualification with the allowance restarted at the failure.
func TestProviderIntentRecheckEveryProbeRefresh(t *testing.T) {
	qualifyTime := providerIntentTestStart
	state := &ProviderIntentState{
		Status:       ProviderIntentStatusQualified,
		AttemptTime:  qualifyTime.Add(-providerIntentGraceTimeout),
		GraceEndTime: qualifyTime,
		CheckTime:    qualifyTime.Add(providerIntentRecheckTimeout),
	}

	// not due yet
	_, needed := providerIntentEvidenceSince(state, qualifyTime.Add(time.Hour))
	connect.AssertEqual(t, needed, false)

	now := state.CheckTime
	since, needed := providerIntentEvidenceSince(state, now)
	connect.AssertEqual(t, needed, true)
	connect.AssertEqual(t, since, now.Add(-providerIntentRecheckTimeout))

	passed, event, decideSlot := providerIntentCheckState(state, &providerIntentEvidence{
		PublicProvider: true,
		ProbeAttempted: true,
		ProbePassed:    true,
	}, now)
	connect.AssertEqual(t, passed.Status, ProviderIntentStatusQualified)
	connect.AssertEqual(t, event, providerIntentEventRecheckPassed)
	connect.AssertEqual(t, decideSlot, false)
	connect.AssertEqual(t, passed.CheckTime, now.Add(providerIntentRecheckTimeout))

	// no probe attempted in the window: a broken probe system fails nobody
	neutral, event, _ := providerIntentCheckState(state, &providerIntentEvidence{
		PublicProvider: true,
	}, now)
	connect.AssertEqual(t, neutral.Status, ProviderIntentStatusQualified)
	connect.AssertEqual(t, event, providerIntentEventRecheckPassed)

	for _, evidence := range []*providerIntentEvidence{
		// probes attempted and none passed
		{PublicProvider: true, ProbeAttempted: true, ProbePassed: false},
		// no longer a registered public provider
		{PublicProvider: false, ProbeAttempted: true, ProbePassed: true},
	} {
		failed, event, decideSlot := providerIntentCheckState(state, evidence, now)
		connect.AssertEqual(t, failed.Status, ProviderIntentStatusNormal)
		connect.AssertEqual(t, event, providerIntentEventRecheckFailed)
		connect.AssertEqual(t, decideSlot, true)
		// leaving qualification waits a full allowance before the next attempt
		connect.AssertEqual(t, failed.AttemptTime, now)
		connect.AssertEqual(t, failed.CheckTime, now.Add(ProviderIntentAttemptAllowance))
		_, started := providerIntentConnectState(failed, now.Add(ProviderIntentAttemptAllowance-time.Second))
		connect.AssertEqual(t, started, false)
		_, started = providerIntentConnectState(failed, now.Add(ProviderIntentAttemptAllowance))
		connect.AssertEqual(t, started, true)
	}
}

// A failed client that stays connected waits for its allowance, then the check
// starts a new attempt.
func TestProviderIntentFailedClientRetriesAfterAllowance(t *testing.T) {
	attemptTime := providerIntentTestStart
	for _, status := range []ProviderIntentStatus{ProviderIntentStatusNormal, ProviderIntentStatusOverLimit} {
		state := &ProviderIntentState{
			Status:      status,
			AttemptTime: attemptTime,
			CheckTime:   attemptTime.Add(time.Hour),
		}
		waiting, event, decideSlot := providerIntentCheckState(state, &providerIntentEvidence{}, attemptTime.Add(time.Hour))
		connect.AssertEqual(t, waiting.Status, status)
		connect.AssertEqual(t, waiting.CheckTime, attemptTime.Add(ProviderIntentAttemptAllowance))
		connect.AssertEqual(t, event, "")
		connect.AssertEqual(t, decideSlot, false)

		now := attemptTime.Add(ProviderIntentAttemptAllowance)
		restarted, event, decideSlot := providerIntentCheckState(waiting, &providerIntentEvidence{}, now)
		connect.AssertEqual(t, restarted.Status, ProviderIntentStatusPending)
		connect.AssertEqual(t, restarted.AttemptTime, now)
		connect.AssertEqual(t, restarted.CheckTime, now)
		connect.AssertEqual(t, event, providerIntentEventStarted)
		connect.AssertEqual(t, decideSlot, false)
	}
}

// Records keep the status readable without json, and survive a round trip.
func TestProviderIntentRecordRoundTrip(t *testing.T) {
	state := newProviderIntentAttempt(providerIntentTestStart)
	record := state.record()
	if record[:len("pending ")] != "pending " {
		t.Fatalf("record does not lead with its status: %s", record)
	}
	parsed, err := parseProviderIntentRecord(record)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, *parsed, *state)

	none, err := parseProviderIntentRecord("")
	connect.AssertEqual(t, err, nil)
	if none != nil {
		t.Fatal("an empty record is no state")
	}
	_, err = parseProviderIntentRecord("pending")
	if err == nil {
		t.Fatal("a record without json parsed")
	}

	// a failed record is kept at least through its allowance
	failed := &ProviderIntentState{
		Status:      ProviderIntentStatusOverLimit,
		AttemptTime: providerIntentTestStart,
	}
	connect.AssertEqual(t, failed.expireTime(providerIntentTestStart.Add(time.Hour)), providerIntentTestStart.Add(time.Hour).Add(providerIntentRecordRetention))
	if failed.expireTime(providerIntentTestStart).Before(providerIntentTestStart.Add(ProviderIntentAttemptAllowance)) {
		t.Fatal("a failed record expires before its allowance")
	}
}

// Sets up a network with the concurrent client limit enforced at `limit` and
// returns its id.
func testingProviderIntentNetwork(ctx context.Context) server.Id {
	networkId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("pi%s", networkId), server.NewId())
	Testing_ClearNetworkPeersEnabledCache()
	return networkId
}

// Creates a top-level client of the network registered as a connected peer
// for longer than any synthetic timeline of these tests.
func testingProviderIntentConnectedClient(ctx context.Context, networkId server.Id) server.Id {
	clientId := server.NewId()
	Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic", "synthetic")
	AddNetworkPeer(ctx, networkId, &NetworkPeer{ClientId: clientId}, server.NewId(), 24*time.Hour)
	return clientId
}

// Writes a client's state directly, as the check would.
func testingSetProviderIntentState(t testing.TB, ctx context.Context, networkId server.Id, clientId server.Id, state *ProviderIntentState, presenceTimeout time.Duration, now time.Time) {
	record, _ := getProviderIntentRecord(ctx, networkId, clientId, now)
	presenceExpireTime := time.Time{}
	if 0 < presenceTimeout {
		presenceExpireTime = now.Add(presenceTimeout)
	}
	result := applyProviderIntentState(ctx, networkId, clientId, record, state, false, 0, presenceExpireTime, now)
	if !result.applied {
		t.Fatalf("state not applied: %s", result.record)
	}
}

// An exempt intent client is counted apart from normal clients only while it
// has a live intent connection: a client that reconnects without intent stops
// refreshing its presence and counts as a normal client again.
func TestProviderIntentExemptionNeedsLiveIntentConnection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := testingProviderIntentNetwork(ctx)
		now := server.NowUtc()

		normalId := testingProviderIntentConnectedClient(ctx, networkId)
		pendingId := testingProviderIntentConnectedClient(ctx, networkId)
		qualifiedId := testingProviderIntentConnectedClient(ctx, networkId)
		failedId := testingProviderIntentConnectedClient(ctx, networkId)
		_ = normalId

		presenceTimeout := 5 * time.Minute
		result := connectProviderIntentAt(ctx, networkId, pendingId, presenceTimeout, now)
		connect.AssertEqual(t, result.AttemptStarted, true)
		connect.AssertEqual(t, result.ScheduleCheck, true)
		connect.AssertEqual(t, result.State.Status, ProviderIntentStatusPending)
		testingSetProviderIntentState(t, ctx, networkId, qualifiedId, &ProviderIntentState{
			Status:      ProviderIntentStatusQualified,
			AttemptTime: now,
			CheckTime:   now.Add(providerIntentRecheckTimeout),
		}, presenceTimeout, now)
		testingSetProviderIntentState(t, ctx, networkId, failedId, &ProviderIntentState{
			Status:      ProviderIntentStatusNormal,
			AttemptTime: now,
			CheckTime:   now.Add(ProviderIntentAttemptAllowance),
		}, presenceTimeout, now)

		normalCount, providerIntentCount := getNetworkConnectedClientCountsAt(ctx, networkId, now)
		// the ordinary and the failed client count; pending and qualified are exempt
		connect.AssertEqual(t, normalCount, 2)
		connect.AssertEqual(t, providerIntentCount, 2)

		// the qualified client keeps observing, the pending client stopped
		// declaring intent and its presence lapses
		later := now.Add(presenceTimeout + time.Second)
		observed := observeProviderIntentAt(ctx, networkId, qualifiedId, presenceTimeout, later)
		connect.AssertEqual(t, observed.Status, ProviderIntentStatusQualified)
		normalCount, providerIntentCount = getNetworkConnectedClientCountsAt(ctx, networkId, later)
		connect.AssertEqual(t, normalCount, 3)
		connect.AssertEqual(t, providerIntentCount, 1)

		// a hosted proxy client is always a normal client
		AddNetworkProxyPeer(ctx, networkId, server.NewId(), time.Hour)
		normalCount, _ = getNetworkConnectedClientCountsAt(ctx, networkId, later)
		connect.AssertEqual(t, normalCount, 4)
	})
}

// A client that failed qualification takes a normal slot when one is free and
// is over the limit otherwise; the decision is atomic with the count, and is
// made again on each new connection of the client.
func TestProviderIntentFailedClientTakesFreeNormalSlot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetConcurrentClientsLimit(2, 2)()
		networkId := testingProviderIntentNetwork(ctx)
		now := server.NowUtc()
		presenceTimeout := 5 * time.Minute

		testingProviderIntentConnectedClient(ctx, networkId)
		failedId := testingProviderIntentConnectedClient(ctx, networkId)
		testingSetProviderIntentState(t, ctx, networkId, failedId, &ProviderIntentState{
			Status:      ProviderIntentStatusOverLimit,
			AttemptTime: now,
			CheckTime:   now.Add(ProviderIntentAttemptAllowance),
		}, 0, now)

		// one normal client of a limit of two: the failed client takes the slot
		result := connectProviderIntentAt(ctx, networkId, failedId, presenceTimeout, now)
		connect.AssertEqual(t, result.AttemptStarted, false)
		connect.AssertEqual(t, result.ScheduleCheck, false)
		connect.AssertEqual(t, result.State.Status, ProviderIntentStatusNormal)
		// the failed attempt keeps its allowance
		connect.AssertEqual(t, result.State.AttemptTime.Equal(now), true)

		// a second ordinary client fills the limit; the failed client's next
		// connection finds no free slot
		testingProviderIntentConnectedClient(ctx, networkId)
		result = connectProviderIntentAt(ctx, networkId, failedId, presenceTimeout, now.Add(time.Minute))
		connect.AssertEqual(t, result.State.Status, ProviderIntentStatusOverLimit)

		// exempt intent clients never take a normal slot
		exemptId := testingProviderIntentConnectedClient(ctx, networkId)
		connect.AssertEqual(t, connectProviderIntentAt(ctx, networkId, exemptId, presenceTimeout, now).State.Status, ProviderIntentStatusPending)
		normalCount, providerIntentCount := getNetworkConnectedClientCountsAt(ctx, networkId, now.Add(time.Minute))
		connect.AssertEqual(t, normalCount, 3)
		connect.AssertEqual(t, providerIntentCount, 1)

		// after the allowance the failed client starts a new attempt instead
		result = connectProviderIntentAt(ctx, networkId, failedId, presenceTimeout, now.Add(ProviderIntentAttemptAllowance))
		connect.AssertEqual(t, result.AttemptStarted, true)
		connect.AssertEqual(t, result.State.Status, ProviderIntentStatusPending)
	})
}

// Shadow mode: while enforce_concurrent_clients is false the slot decision is
// computed and counted, and the connection gate admits everyone.
func TestProviderIntentShadowModeCountsDecisionsAndAdmitsAll(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceConcurrentClients(false)()
		defer Testing_SetConcurrentClientsLimit(1, 1)()
		networkId := testingProviderIntentNetwork(ctx)
		now := server.NowUtc()

		testingProviderIntentConnectedClient(ctx, networkId)
		failedId := testingProviderIntentConnectedClient(ctx, networkId)
		testingSetProviderIntentState(t, ctx, networkId, failedId, &ProviderIntentState{
			Status:      ProviderIntentStatusNormal,
			AttemptTime: now,
			CheckTime:   now.Add(ProviderIntentAttemptAllowance),
		}, 0, now)

		shadowOverLimit := providerIntentSlotDecisionsCounter.WithLabelValues("over_limit", "shadow")
		enforcedOverLimit := providerIntentSlotDecisionsCounter.WithLabelValues("over_limit", "enforced")
		shadowBefore := testutil.ToFloat64(shadowOverLimit)
		enforcedBefore := testutil.ToFloat64(enforcedOverLimit)

		result := connectProviderIntentAt(ctx, networkId, failedId, 5*time.Minute, now)
		connect.AssertEqual(t, result.State.Status, ProviderIntentStatusOverLimit)
		connect.AssertEqual(t, testutil.ToFloat64(shadowOverLimit), shadowBefore+1)
		connect.AssertEqual(t, testutil.ToFloat64(enforcedOverLimit), enforcedBefore)

		// the gate does no work and refuses nobody while dark
		connect.AssertEqual(t, CanConnectNetworkPeer(ctx, failedId, true), true)
		connect.AssertEqual(t, CanConnectNetworkPeer(ctx, server.NewId(), false), true)
	})
}

// With enforcement on, a public provider without declared intent is no longer
// exempt from the client limit (the blanket public provider exemption is
// gone), while a connection with intent is judged by its provider intent state.
func TestNetworkProviderIntentExemptFromLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceConcurrentClients(true)()
		defer Testing_SetConcurrentClientsLimit(1, 1)()
		networkId := testingProviderIntentNetwork(ctx)
		presenceTimeout := 5 * time.Minute

		// one ordinary connected client: the network is at its limit of 1
		testingProviderIntentConnectedClient(ctx, networkId)
		connect.AssertEqual(t, GetNetworkEnforceableConnectedCount(ctx, networkId), 1)
		ordinaryId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), ordinaryId, "synthetic", "synthetic")
		connect.AssertEqual(t, CanConnectNetworkPeer(ctx, ordinaryId, false), false)

		publicStream := map[ProvideMode][]byte{
			ProvideModePublic: make([]byte, 32),
			ProvideModeStream: make([]byte, 32),
		}

		// a public provider that did not declare intent counts like any client
		withoutIntentId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), withoutIntentId, "synthetic", "synthetic")
		SetProvide(ctx, withoutIntentId, publicStream)
		connect.AssertEqual(t, CanConnectNetworkPeer(ctx, withoutIntentId, false), false)
		AddNetworkPeer(ctx, networkId, &NetworkPeer{
			ClientId:     withoutIntentId,
			ProvideModes: []ProvideMode{ProvideModePublic, ProvideModeStream},
		}, server.NewId(), time.Hour)
		connect.AssertEqual(t, GetNetworkEnforceableConnectedCount(ctx, networkId), 2)

		// intent providers connect past the limit while they qualify, and are
		// counted apart from the normal clients
		for i := range 5 {
			providerId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), providerId, fmt.Sprintf("provider %d", i), "synthetic")
			SetProvide(ctx, providerId, publicStream)
			result := ConnectProviderIntent(ctx, networkId, providerId, presenceTimeout)
			connect.AssertEqual(t, result.State.Status, ProviderIntentStatusPending)
			if !CanConnectNetworkPeer(ctx, providerId, true) {
				t.Fatalf("intent provider %d refused at the network's client limit", i)
			}
			AddNetworkPeer(ctx, networkId, &NetworkPeer{
				ClientId:     providerId,
				ProvideModes: []ProvideMode{ProvideModePublic, ProvideModeStream},
			}, server.NewId(), time.Hour)
		}
		normalCount, providerIntentCount := GetNetworkConnectedClientCounts(ctx, networkId)
		connect.AssertEqual(t, normalCount, 2)
		connect.AssertEqual(t, providerIntentCount, 5)
		connect.AssertEqual(t, GetNetworkEnforceableConnectedCount(ctx, networkId), 2)

		// an intent client over the limit is refused
		overLimitId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), overLimitId, "synthetic", "synthetic")
		now := server.NowUtc()
		testingSetProviderIntentState(t, ctx, networkId, overLimitId, &ProviderIntentState{
			Status:      ProviderIntentStatusOverLimit,
			AttemptTime: now,
			CheckTime:   now.Add(ProviderIntentAttemptAllowance),
		}, presenceTimeout, now)
		connect.AssertEqual(t, CanConnectNetworkPeer(ctx, overLimitId, true), false)
	})
}

// Inserts one accepted URL probe outcome for a client.
func testingProviderIntentUrlProbe(ctx context.Context, clientId server.Id, measuredAt time.Time, passed bool) {
	okCount := 0
	if passed {
		okCount = 1
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO provider_egress_health_history (
					run_id,
					client_id,
					measured_at,
					ok_count,
					total_count,
					class_results,
					tls_authentication_failure,
					url_probe
				)
				VALUES ($1, $2, $3, $4, 1, '{}', false, true)
			`,
			server.NewId(),
			clientId,
			measuredAt.UTC(),
			okCount,
		))
	})
}

// Inserts a URL probe run that completed without a measurement (a setup
// failure): the probe system attempted the provider.
func testingProviderIntentCompletedRun(ctx context.Context, clientId server.Id, claimedAt time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO provider_url_probe_run (
					client_id,
					claim_ordinal,
					claimed_at,
					completed_at,
					received_at,
					probe_failure
				)
				VALUES ($1, 1, $2, $2, $2, 'synthetic_setup_failure')
			`,
			clientId,
			claimedAt.UTC(),
		))
	})
}

// The evidence read covers exactly the requested probe window, and a
// completed run without a measurement is an attempt.
func TestProviderIntentEvidenceWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := testingProviderIntentNetwork(ctx)
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic", "synthetic")
		since := server.NowUtc().Truncate(time.Microsecond)
		now := since.Add(time.Hour)

		evidence := getProviderIntentEvidence(ctx, clientId, since, now)
		connect.AssertEqual(t, evidence.PublicProvider, false)
		connect.AssertEqual(t, evidence.ProbeAttempted, false)
		connect.AssertEqual(t, evidence.ProbePassed, false)
		if evidence.ClientCreateTime.IsZero() {
			t.Fatal("missing client create time")
		}
		if evidence.ReliabilityWeight != nil {
			t.Fatal("a client without a score has no weight")
		}

		SetProvide(ctx, clientId, map[ProvideMode][]byte{ProvideModePublic: make([]byte, 32)})
		// a pass before the window and a pass after `now` do not count
		testingProviderIntentUrlProbe(ctx, clientId, since.Add(-time.Second), true)
		testingProviderIntentUrlProbe(ctx, clientId, now.Add(time.Second), true)
		evidence = getProviderIntentEvidence(ctx, clientId, since, now)
		connect.AssertEqual(t, evidence.PublicProvider, true)
		connect.AssertEqual(t, evidence.ProbeAttempted, false)
		connect.AssertEqual(t, evidence.ProbePassed, false)

		testingProviderIntentCompletedRun(ctx, clientId, since.Add(time.Minute))
		evidence = getProviderIntentEvidence(ctx, clientId, since, now)
		connect.AssertEqual(t, evidence.ProbeAttempted, true)
		connect.AssertEqual(t, evidence.ProbePassed, false)

		testingProviderIntentUrlProbe(ctx, clientId, since.Add(2*time.Minute), false)
		evidence = getProviderIntentEvidence(ctx, clientId, since, now)
		connect.AssertEqual(t, evidence.ProbePassed, false)

		testingProviderIntentUrlProbe(ctx, clientId, since.Add(3*time.Minute), true)
		evidence = getProviderIntentEvidence(ctx, clientId, since, now)
		connect.AssertEqual(t, evidence.ProbePassed, true)
	})
}

// The check chain: a new attempt requests probe priority at once, the grace
// extends while no probe was attempted, a passing probe qualifies the
// provider, and the chain stops when the client has no live intent connection.
func TestProviderIntentCheckChain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := testingProviderIntentNetwork(ctx)
		clientId := testingProviderIntentConnectedClient(ctx, networkId)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{ProvideModePublic: make([]byte, 32)})
		start := server.NowUtc().Truncate(time.Microsecond)
		// presence through the grace and its extension, lapsed by the re-check
		presenceTimeout := 2 * time.Hour

		connectResult := connectProviderIntentAt(ctx, networkId, clientId, presenceTimeout, start)
		connect.AssertEqual(t, connectResult.ScheduleCheck, true)

		// the first check runs at the attempt start and requests probe priority
		checkTime := checkProviderIntentAt(ctx, networkId, clientId, start)
		connect.AssertEqual(t, checkTime.Equal(start.Add(ProviderIntentGraceExtendTimeout)), true)
		connect.AssertEqual(t, testingProviderIntentProbePriority(ctx, clientId), true)

		// an early run reschedules without a change
		early := checkProviderIntentAt(ctx, networkId, clientId, start.Add(time.Minute))
		connect.AssertEqual(t, early.Equal(*checkTime), true)

		// the checks in the grace renew the priority up to the end of the grace
		for checkTime.Before(start.Add(providerIntentGraceTimeout)) {
			checkTime = checkProviderIntentAt(ctx, networkId, clientId, *checkTime)
			connect.AssertEqual(t, testingProviderIntentProbePriority(ctx, clientId), true)
		}
		connect.AssertEqual(t, checkTime.Equal(start.Add(providerIntentGraceTimeout)), true)

		// no probe attempted at the end of the grace: extended, still pending
		graceExtended := providerIntentAttemptsCounter.WithLabelValues(providerIntentEventGraceExtended)
		graceExtendedBefore := testutil.ToFloat64(graceExtended)
		extended := checkProviderIntentAt(ctx, networkId, clientId, *checkTime)
		connect.AssertEqual(t, extended.Equal(checkTime.Add(ProviderIntentGraceExtendTimeout)), true)
		connect.AssertEqual(t, testutil.ToFloat64(graceExtended), graceExtendedBefore+1)
		connect.AssertEqual(t, getProviderIntentStateAt(ctx, networkId, clientId, *extended).Status, ProviderIntentStatusPending)
		connect.AssertEqual(t, testingProviderIntentProbePriority(ctx, clientId), true)

		// a passing probe within the extended grace qualifies the provider
		testingProviderIntentUrlProbe(ctx, clientId, extended.Add(-time.Minute), true)
		qualifiedCheckTime := checkProviderIntentAt(ctx, networkId, clientId, *extended)
		connect.AssertEqual(t, qualifiedCheckTime.Equal(extended.Add(providerIntentRecheckTimeout)), true)
		connect.AssertEqual(t, getProviderIntentStateAt(ctx, networkId, clientId, *extended).Status, ProviderIntentStatusQualified)
		// the attempt resolved: no more priority
		connect.AssertEqual(t, testingProviderIntentProbePriority(ctx, clientId), false)

		// the client's intent connections ended: the chain stops at the re-check,
		// and the record stays until it expires
		stopped := providerIntentAttemptsCounter.WithLabelValues(providerIntentEventStopped)
		stoppedBefore := testutil.ToFloat64(stopped)
		if next := checkProviderIntentAt(ctx, networkId, clientId, *qualifiedCheckTime); next != nil {
			t.Fatalf("the chain continued without a live intent connection: %s", next)
		}
		connect.AssertEqual(t, testutil.ToFloat64(stopped), stoppedBefore+1)
		connect.AssertEqual(t, getProviderIntentStateAt(ctx, networkId, clientId, *qualifiedCheckTime).Status, ProviderIntentStatusQualified)
	})
}

// A provider whose probes are attempted and fail at the end of the grace fails
// into a normal slot decision, and leaves no probe priority behind.
func TestProviderIntentCheckFailsWithoutPassingProbe(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetConcurrentClientsLimit(1, 1)()
		networkId := testingProviderIntentNetwork(ctx)
		// the one normal slot is taken
		testingProviderIntentConnectedClient(ctx, networkId)
		clientId := testingProviderIntentConnectedClient(ctx, networkId)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{ProvideModePublic: make([]byte, 32)})
		start := server.NowUtc().Truncate(time.Microsecond)
		presenceTimeout := 24 * time.Hour

		connectProviderIntentAt(ctx, networkId, clientId, presenceTimeout, start)
		checkTime := checkProviderIntentAt(ctx, networkId, clientId, start)
		for checkTime.Before(start.Add(providerIntentGraceTimeout)) {
			checkTime = checkProviderIntentAt(ctx, networkId, clientId, *checkTime)
		}
		testingProviderIntentUrlProbe(ctx, clientId, start.Add(10*time.Minute), false)

		failedCheckTime := checkProviderIntentAt(ctx, networkId, clientId, *checkTime)
		connect.AssertEqual(t, failedCheckTime.Equal(start.Add(ProviderIntentAttemptAllowance)), true)
		state := getProviderIntentStateAt(ctx, networkId, clientId, *checkTime)
		connect.AssertEqual(t, state.Status, ProviderIntentStatusOverLimit)
		connect.AssertEqual(t, testingProviderIntentProbePriority(ctx, clientId), false)
	})
}

// Reads a state at an explicit time.
func getProviderIntentStateAt(ctx context.Context, networkId server.Id, clientId server.Id, now time.Time) *ProviderIntentState {
	_, state := getProviderIntentRecord(ctx, networkId, clientId, now)
	return state
}

// Whether the client holds a probe priority row.
func testingProviderIntentProbePriority(ctx context.Context, clientId server.Id) (exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM provider_intent_probe_priority WHERE client_id = $1)`,
			clientId,
		).Scan(&exists))
	})
	return
}

// Probe priority admits an intent provider in its grace to egress URL probes
// before it has the reliability history the probe gate otherwise requires, and
// makes its probe cycle due from the start of its attempt. Serving eligibility
// is not changed.
func TestProviderIntentProbePriorityAdmitsNewProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		provider := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		// a new provider: an hour of history over a 12 hour lookback
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO client_connection_reliability_score (
						client_id,
						lookback_index,
						independent_reliability_score,
						independent_reliability_weight,
						reliability_score,
						reliability_weight,
						min_block_number,
						max_block_number,
						city_location_id,
						region_location_id,
						country_location_id
					)
					VALUES ($1, 2, 1, 0.08, 1, 0.08, 1, 1, $2, $3, $4)
				`,
				provider.clientId,
				city.LocationId,
				city.RegionLocationId,
				city.CountryLocationId,
			))
		})

		eligible := func() (eligible bool) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, providerUrlProbeClientEligibilitySql(), provider.clientId, ProvideModePublic).Scan(&eligible))
			})
			return
		}
		// the reliability floor keeps the new provider out of URL probes
		connect.AssertEqual(t, eligible(), false)
		claimAt := server.NowUtc().Add(time.Second).Truncate(time.Microsecond)
		connect.AssertEqual(t, len(ClaimProviderUrlProbeDue(ctx, claimAt, 1, 0, 1)), 0)

		attemptTime := claimAt.Add(-30 * time.Minute)
		// the location is ready, so the cycle is admitted at once
		connect.AssertEqual(t, setProviderIntentProbePriority(ctx, provider.clientId, attemptTime), true)
		connect.AssertEqual(t, eligible(), true)
		var nextAttemptAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT next_attempt_at FROM provider_egress_probe_cycle WHERE client_id = $1 AND eligible`,
				provider.clientId,
			).Scan(&nextAttemptAt))
		})
		connect.AssertEqual(t, nextAttemptAt.Equal(attemptTime), true)

		due := ClaimProviderUrlProbeDue(ctx, claimAt, 1, 0, 1)
		if len(due) != 1 || due[0].ClientId != provider.clientId {
			t.Fatalf("intent provider not admitted to URL probes: %+v", due)
		}

		// the reconciliation keeps the priority provider eligible, and drops it
		// once the priority ends
		server.Tx(ctx, func(tx server.PgTx) {
			updateProviderUrlProbeEligibility(ctx, tx)
		})
		connect.AssertEqual(t, testingProviderIntentCycleEligible(ctx, provider.clientId), true)
		removeProviderIntentProbePriority(ctx, provider.clientId)
		server.Tx(ctx, func(tx server.PgTx) {
			updateProviderUrlProbeEligibility(ctx, tx)
		})
		connect.AssertEqual(t, testingProviderIntentCycleEligible(ctx, provider.clientId), false)
	})
}

// A new intent provider seeded by the periodic eligibility pass is due from the
// start of its attempt.
func TestProviderIntentProbePrioritySeedsBackdatedCycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		provider := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		attemptTime := server.NowUtc().Add(-45 * time.Minute).Truncate(time.Microsecond)

		// the priority is requested before the provider's location is ready, so
		// no cycle exists yet
		connect.AssertEqual(t, setProviderIntentProbePriority(ctx, provider.clientId, attemptTime), false)
		connect.AssertEqual(t, testingProviderIntentCycleEligible(ctx, provider.clientId), false)

		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		server.Tx(ctx, func(tx server.PgTx) {
			updateProviderUrlProbeEligibility(ctx, tx)
		})
		var nextAttemptAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT next_attempt_at FROM provider_egress_probe_cycle WHERE client_id = $1 AND eligible`,
				provider.clientId,
			).Scan(&nextAttemptAt))
		})
		connect.AssertEqual(t, nextAttemptAt.Equal(attemptTime), true)
	})
}

// A probe priority no check renewed for the retention is dropped by the
// periodic cleanup; a renewed one stays.
func TestProviderIntentProbePriorityExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		setProviderIntentProbePriority(ctx, clientId, server.NowUtc())
		updateTime := server.NowUtc()

		RemoveExpiredProviderIntentProbePriorities(ctx, updateTime.Add(providerIntentProbePriorityRetention-time.Minute))
		connect.AssertEqual(t, testingProviderIntentProbePriority(ctx, clientId), true)

		RemoveExpiredProviderIntentProbePriorities(ctx, updateTime.Add(providerIntentProbePriorityRetention+time.Minute))
		connect.AssertEqual(t, testingProviderIntentProbePriority(ctx, clientId), false)
	})
}

// Whether the client's URL probe cycle exists and is eligible.
func testingProviderIntentCycleEligible(ctx context.Context, clientId server.Id) (eligible bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM provider_egress_probe_cycle WHERE client_id = $1 AND eligible)`,
			clientId,
		).Scan(&eligible))
	})
	return
}

// A client created with provide intent is exempt from the top-level client cap
// and the concurrent connected client check; without it, the caps refuse.
func TestAuthNetworkClientProvideIntentExemptFromCaps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer Testing_SetEnforceConcurrentClients(true)()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "test", userId)
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})
		Testing_ClearNetworkPeersEnabledCache()

		// fill the top-level cap without connecting anyone
		defer Testing_SetConcurrentClientsLimit(0, 0)()
		for range LimitTopLevelClientIdsPerNetwork {
			result, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "synthetic"}, userSession)
			connect.AssertEqual(t, err, nil)
			if result.Error != nil {
				t.Fatalf("setup refused: %s", result.Error.Message)
			}
		}
		result, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "one too many"}, userSession)
		connect.AssertEqual(t, err, nil)
		if result.Error == nil || !result.Error.ClientLimitExceeded {
			t.Fatal("the top-level cap did not refuse a client without intent")
		}
		result, err = AuthNetworkClient(&AuthNetworkClientArgs{Description: "provider", ProvideIntent: true}, userSession)
		connect.AssertEqual(t, err, nil)
		if result.Error != nil {
			t.Fatalf("provider refused by the top-level cap: %s", result.Error.Message)
		}

		// the concurrent connected check: one connected client of a limit of one
		defer Testing_SetConcurrentClientsLimit(1, 1)()
		AddNetworkPeer(ctx, networkId, &NetworkPeer{ClientId: *result.ClientId}, server.NewId(), time.Hour)
		connect.AssertEqual(t, NetworkConcurrentClientsExceeded(ctx, networkId), true)
		result, err = AuthNetworkClient(&AuthNetworkClientArgs{Description: "provider 2", ProvideIntent: true}, userSession)
		connect.AssertEqual(t, err, nil)
		if result.Error != nil {
			t.Fatalf("provider refused by the concurrent connected check: %s", result.Error.Message)
		}
		// an ancillary client ignores the flag, and a re-auth ignores it too
		_, err = AuthNetworkClient(&AuthNetworkClientArgs{Description: "derived", SourceClientId: result.ClientId, ProvideIntent: true}, userSession)
		connect.AssertEqual(t, err, nil)
	})
}

// A network with more than the top-level client limit of provider installs
// (top-level clients created with provide intent) keeps its peer list for its
// other clients: installs are not counted by the peer valve, nor by the
// top-level client cap a normal client creation is held to.
func TestNetworkProviderInstallsKeepPeerList(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer Testing_SetEnforceConcurrentClients(true)()
		// no connected client limit, so only the top-level cap can refuse
		defer Testing_SetConcurrentClientsLimit(0, 0)()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "test", userId)
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})

		installIds := []server.Id{}
		for i := range LimitTopLevelClientIdsPerNetwork + 5 {
			result, err := AuthNetworkClient(&AuthNetworkClientArgs{
				Description:   fmt.Sprintf("install %d", i),
				ProvideIntent: true,
			}, userSession)
			connect.AssertEqual(t, err, nil)
			if result.Error != nil {
				t.Fatalf("install %d refused: %s", i, result.Error.Message)
			}
			installIds = append(installIds, *result.ClientId)
		}

		// a normal client is still created: installs do not fill the cap
		result, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "laptop"}, userSession)
		connect.AssertEqual(t, err, nil)
		if result.Error != nil {
			t.Fatalf("a normal client was refused by provider installs: %s", result.Error.Message)
		}
		normalClientId := *result.ClientId

		Testing_ClearNetworkPeersEnabledCache()
		connect.AssertEqual(t, NetworkPeersEnabled(ctx, networkId), true)
		_, topLevel, category, profile, peersEnabled := GetNetworkPeerProfile(ctx, normalClientId)
		connect.AssertEqual(t, topLevel, true)
		connect.AssertEqual(t, category, NetworkPeerCategoryClient)
		connect.AssertNotEqual(t, profile, nil)
		connect.AssertEqual(t, peersEnabled, true)

		// an install is categorized as a provider install, never a client peer
		_, topLevel, category, profile, _ = GetNetworkPeerProfile(ctx, installIds[0])
		connect.AssertEqual(t, topLevel, true)
		connect.AssertEqual(t, category, NetworkPeerCategoryProvider)
		connect.AssertNotEqual(t, profile, nil)

		// the valve still counts the normal clients
		for i := range LimitTopLevelClientIdsPerNetwork {
			Testing_CreateDevice(ctx, networkId, server.NewId(), server.NewId(), fmt.Sprintf("normal %d", i), "synthetic")
		}
		Testing_ClearNetworkPeersEnabledCache()
		connect.AssertEqual(t, NetworkPeersEnabled(ctx, networkId), false)
	})
}

// Provider installs are registered apart from peers: counted by the provider
// intent rules, never in the network's peer list.
func TestNetworkProviderInstallsNeverPeers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := testingProviderIntentNetwork(ctx)
		normalId := testingProviderIntentConnectedClient(ctx, networkId)
		installId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), installId, "install", "synthetic")
		AddNetworkProviderPeer(ctx, networkId, installId, 24*time.Hour)

		_, peers := GetNetworkPeers(ctx, networkId)
		connect.AssertEqual(t, len(peers), 1)
		connect.AssertEqual(t, peers[0].ClientId, normalId)
		connect.AssertEqual(t, GetNetworkConnectedCount(ctx, networkId), 2)

		// an install without a live exempt state counts like a normal client
		now := server.NowUtc()
		normalCount, providerIntentCount := getNetworkConnectedClientCountsAt(ctx, networkId, now)
		connect.AssertEqual(t, normalCount, 2)
		connect.AssertEqual(t, providerIntentCount, 0)

		// an install qualifying as a provider is exempt
		connectProviderIntentAt(ctx, networkId, installId, 5*time.Minute, now)
		normalCount, providerIntentCount = getNetworkConnectedClientCountsAt(ctx, networkId, now)
		connect.AssertEqual(t, normalCount, 1)
		connect.AssertEqual(t, providerIntentCount, 1)

		RemoveNetworkProviderPeer(ctx, networkId, installId)
		connect.AssertEqual(t, GetNetworkConnectedCount(ctx, networkId), 1)
		_, peers = GetNetworkPeers(ctx, networkId)
		connect.AssertEqual(t, len(peers), 1)
	})
}
