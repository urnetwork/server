package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/reengagement"
	"github.com/urnetwork/server/task"
)

func testMarketingSuppressionSettings(batchSize int, syncsPerSecond float64) *MarketingSuppressionSettings {
	settings := DefaultMarketingSuppressionSettings()
	settings.BatchSize = batchSize
	settings.Mirror.SyncsPerSecond = syncsPerSecond
	return settings
}

// Pure: the digest binds the reason and the set of networks, whatever their
// order or repeats; a reason the command does not take, an empty list and a
// list over the limit are refused; the batches keep the sorted order.
func TestMarketingSuppressionRequestDigest(t *testing.T) {
	settings := testMarketingSuppressionSettings(2, 2)
	a, b, c := server.NewId(), server.NewId(), server.NewId()
	first, err := newMarketingSuppressionRequest([]server.Id{c, a, b}, reengagement.SuppressAutomatedSignup, settings)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newMarketingSuppressionRequest([]server.Id{b, b, a, c, a}, reengagement.SuppressAutomatedSignup, settings)
	if err != nil {
		t.Fatal(err)
	}
	if first.digest != second.digest || first.duplicates != 0 || second.duplicates != 2 || len(second.networkIds) != 3 {
		t.Fatal("order or repeats changed the request", first.digest, second.digest, second.duplicates)
	}
	if len(first.digest) != 64 {
		t.Fatal("digest", first.digest)
	}
	for i := 1; i < len(first.networkIds); i += 1 {
		if !first.networkIds[i-1].Less(first.networkIds[i]) {
			t.Fatal("not sorted")
		}
	}
	if batches := first.batches(settings); len(batches) != 2 || len(batches[0]) != 2 || len(batches[1]) != 1 ||
		!slices.Equal(slices.Concat(batches...), first.networkIds) {
		t.Fatal("batches", batches)
	}

	sanctions, err := newMarketingSuppressionRequest([]server.Id{a, b, c}, reengagement.SuppressSanctions, settings)
	if err != nil || sanctions.digest == first.digest {
		t.Fatal("another reason has the same digest", err)
	}
	fewer, err := newMarketingSuppressionRequest([]server.Id{a, b}, reengagement.SuppressAutomatedSignup, settings)
	if err != nil || fewer.digest == first.digest {
		t.Fatal("another list has the same digest", err)
	}

	for _, reason := range []string{"", "sunset", "switched", "complained", "Sanctions"} {
		if _, err := newMarketingSuppressionRequest([]server.Id{a}, reason, settings); err == nil {
			t.Errorf("reason %q accepted", reason)
		}
	}
	if _, err := newMarketingSuppressionRequest(nil, reengagement.SuppressSanctions, settings); err == nil {
		t.Error("no networks accepted")
	}
	limited := testMarketingSuppressionSettings(2, 2)
	limited.MaxNetworkIds = 2
	if _, err := newMarketingSuppressionRequest([]server.Id{a, b, c}, reengagement.SuppressSanctions, limited); err == nil {
		t.Error("a request over the limit accepted")
	}
	for _, broken := range []*MarketingSuppressionSettings{
		testMarketingSuppressionSettings(0, 2),
		{BatchSize: 2, MaxNetworkIds: 10},
	} {
		if _, err := newMarketingSuppressionRequest([]server.Id{a}, reengagement.SuppressSanctions, broken); err == nil {
			t.Error("broken settings accepted", broken)
		}
	}
}

// Pure: an apply whose --expect is not the digest of its networks and reason
// is refused before it reads or writes anything. This test has no database:
// any read would panic instead of returning the refusal.
func TestSuppressNetworksMarketingRefusesAnotherDigestBeforeReading(t *testing.T) {
	settings := testMarketingSuppressionSettings(2, 2)
	networkIds := []server.Id{server.NewId(), server.NewId()}
	request, err := newMarketingSuppressionRequest(networkIds, reengagement.SuppressAutomatedSignup, settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		networkIds []server.Id
		reason     string
		expect     string
	}{
		{networkIds, reengagement.SuppressAutomatedSignup, ""},
		{networkIds, reengagement.SuppressAutomatedSignup, strings.Repeat("0", 64)},
		// the digest of the dry run, for another reason or list
		{networkIds, reengagement.SuppressSanctions, request.digest},
		{append(slices.Clone(networkIds), server.NewId()), reengagement.SuppressAutomatedSignup, request.digest},
		{networkIds[:1], reengagement.SuppressAutomatedSignup, request.digest},
	} {
		result, err := SuppressNetworksMarketing(t.Context(), c.networkIds, c.reason, c.expect, settings)
		if result != nil || !errors.Is(err, ErrMarketingSuppressionDigestMismatch) {
			t.Fatal("not refused", c.reason, len(c.networkIds), result, err)
		}
	}
}

// Pure: Brevo's rate limit answer (429) on a list removal is an error, never
// a removal, so the product-updates sync fails and the task retries it with
// its backoff until Brevo takes the call.
func TestBrevoRemoveFromListRateLimitIsAnError(t *testing.T) {
	email := "synthetic-rate-limit@example.invalid"
	var limited atomic.Bool
	limited.Store(true)
	var requests atomic.Int64
	newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/v3/contacts/lists/8/contacts/remove" {
			t.Error("unexpected request", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if limited.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "too_many_requests", "message": "synthetic rate limit"})
			return
		}
		_ = json.NewEncoder(w).Encode(BrevoListResult{Contacts: &BrevoListResultContacts{Success: []string{email}}})
	}))
	err := BrevoRemoveFromList(t.Context(), email, 8)
	if err == nil || !strings.Contains(err.Error(), "status=429 Too Many Requests") || strings.Contains(err.Error(), email) {
		t.Fatal("a rate-limited removal", err)
	}
	limited.Store(false)
	if err := BrevoRemoveFromList(t.Context(), email, 8); err != nil {
		t.Fatal("the retried removal", err)
	}
	if requests.Load() != 2 {
		t.Fatal("requests", requests.Load())
	}
}

// Pure: a batch's syncs start one slot apart, in the order they are asked
// for; a non-positive rate starts them all at once.
func TestProductUpdatesSyncPacer(t *testing.T) {
	start := time.Date(2026, 11, 13, 6, 0, 0, 0, time.UTC)
	pacer := newProductUpdatesSyncPacer(start, &ProductUpdatesMirrorSettings{SyncsPerSecond: 2})
	for i := range 5 {
		if at := pacer.next(); !at.Equal(start.Add(time.Duration(i) * 500 * time.Millisecond)) {
			t.Fatal("slot", i, at)
		}
	}
	unpaced := newProductUpdatesSyncPacer(start, &ProductUpdatesMirrorSettings{SyncsPerSecond: 0})
	for range 3 {
		if at := unpaced.next(); !at.Equal(start) {
			t.Fatal("an unpaced slot", at)
		}
	}
	// 2 a second keeps the two list calls of each sync under half of Brevo's
	// 10 requests a second
	if settings := DefaultProductUpdatesMirrorSettings(); settings.SyncsPerSecond != 2 || DefaultProductUpdatesMirrorSettings() == settings {
		t.Fatal("defaults", settings)
	}
	if DefaultMarketingSuppressionSettings().Mirror == DefaultMarketingSuppressionSettings().Mirror {
		t.Fatal("the defaults share their mirror settings")
	}
}

// The batch suppression stops all marketing mail to the networks it writes:
// the dry run counts and writes nothing; an apply with another digest writes
// nothing; the apply writes in bounded batches, one transaction each, with
// the Brevo list sync of each written network queued at its paced slot; the
// onboarding step in flight and the re-engagement step exit as suppressed;
// no connection lifts it; and a repeat writes nothing and queues nothing.
func TestSuppressNetworksMarketingAppliesInBatches(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		now := server.NowUtc()
		earlier := now.Add(-24 * time.Hour)
		type fixture struct {
			networkId server.Id
			userId    server.Id
		}
		create := func(name string) fixture {
			f := fixture{networkId: server.NewId(), userId: server.NewId()}
			model.Testing_CreateNetwork(ctx, f.networkId, name, f.userId)
			model.AccountPreferencesSetForNetwork(ctx, f.networkId, true)
			return f
		}
		// an onboarding sequence in flight, a lapsed network, a fresh one,
		// a sunset a connection would lift, and a complaint
		inFlight := create("synthetic-suppress-in-flight")
		at := now.Add(time.Hour)
		model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{
			NetworkId: inFlight.networkId, CreatedAt: earlier, Email: true, Country: "JP",
			NextStep: onboarding.StepE2, NextSendAt: &at,
		})
		lapsed := create("synthetic-suppress-lapsed")
		fresh := create("synthetic-suppress-fresh")
		sunset := create("synthetic-suppress-sunset")
		complained := create("synthetic-suppress-complained")
		server.Tx(ctx, func(tx server.PgTx) {
			model.SuppressNetworkMarketingInTx(tx, ctx, sunset.networkId, reengagement.SuppressSunset, earlier)
			model.SuppressNetworkMarketingInTx(tx, ctx, complained.networkId, onboarding.ExitComplained, earlier)
		})
		missingNetworkId := server.NewId()
		networkIds := []server.Id{
			inFlight.networkId, lapsed.networkId, fresh.networkId, sunset.networkId, complained.networkId,
			missingNetworkId, fresh.networkId,
		}
		written := []fixture{inFlight, lapsed, fresh, sunset}
		settings := testMarketingSuppressionSettings(2, 1)
		reason := reengagement.SuppressAutomatedSignup
		pending := func() int {
			return len(pendingProductUpdatesSyncs(ctx))
		}

		// the onboarding step in flight would send before the suppression
		facts, _ := loadOnboardingFacts(ctx, model.GetNetworkOnboarding(ctx, inFlight.networkId), now)
		if decision := onboarding.Decide(onboarding.StepE2, facts); decision.Action == onboarding.ActionExit {
			tb.Fatal("the in-flight sequence exited before the suppression", decision.ExitReason)
		}

		plan, err := PlanNetworksMarketingSuppression(ctx, networkIds, reason, settings)
		if err != nil {
			tb.Fatal(err)
		}
		wantCounts := MarketingSuppressionCounts{Found: 5, Missing: 1, AlreadySuppressed: 1, ToSuppress: 4, ReplacesLiftable: 1}
		if plan.Counts != wantCounts || plan.Duplicates != 1 || len(plan.NetworkIds) != 6 || plan.Reason != reason {
			tb.Fatal("plan", plan.Counts, plan.Duplicates, len(plan.NetworkIds))
		}
		if state := model.GetNetworkMailState(ctx, fresh.networkId); state.SuppressedAt != nil || pending() != 0 {
			tb.Fatal("the dry run wrote")
		}

		// another digest: nothing written, nothing queued
		if result, err := SuppressNetworksMarketing(ctx, networkIds, reason, strings.Repeat("0", 64), settings); result != nil || !errors.Is(err, ErrMarketingSuppressionDigestMismatch) {
			tb.Fatal("another digest was applied", result, err)
		}
		if state := model.GetNetworkMailState(ctx, fresh.networkId); state.SuppressedAt != nil || pending() != 0 {
			tb.Fatal("a refused apply wrote")
		}

		result, err := SuppressNetworksMarketing(ctx, networkIds, reason, plan.Digest, settings)
		if err != nil {
			tb.Fatal(err)
		}
		if result.Suppressed != 4 || result.Batches != 3 || result.Counts != wantCounts || result.Requested != 6 || result.Duplicates != 1 {
			tb.Fatal("result", result.Suppressed, result.Batches, result.Counts)
		}
		for _, f := range written {
			if state := model.GetNetworkMailState(ctx, f.networkId); state.SuppressedAt == nil || state.SuppressedReason != reason {
				tb.Fatal("not suppressed", state.SuppressedAt, state.SuppressedReason)
			}
		}
		if state := model.GetNetworkMailState(ctx, complained.networkId); state.SuppressedReason != onboarding.ExitComplained {
			tb.Fatal("a complaint was replaced", state.SuppressedReason)
		}
		if state := model.GetNetworkMailState(ctx, sunset.networkId); !state.SuppressedAt.Equal(earlier) {
			tb.Fatal("the replaced sunset lost its time", state.SuppressedAt)
		}

		// one sync per written network, none for the complaint, one second apart
		syncs := pendingProductUpdatesSyncs(ctx)
		if len(syncs) != len(written) {
			tb.Fatal("queued", len(syncs), "syncs")
		}
		runAts := []time.Time{}
		for _, f := range written {
			queued := requireQueuedMirror(tb, ctx, f.userId)
			runAts = append(runAts, queued.RunAt)
		}
		if syncs[task.RunOnce("sync_product_updates_for_user", complained.userId).String()] != nil {
			tb.Fatal("a network left as it was queued a sync")
		}
		slices.SortFunc(runAts, func(a time.Time, b time.Time) int {
			return a.Compare(b)
		})
		for i := 1; i < len(runAts); i += 1 {
			if gap := runAts[i].Sub(runAts[i-1]); gap != time.Second {
				tb.Fatal("syncs not paced one second apart", gap)
			}
		}

		// the onboarding step in flight and a re-engagement step now exit
		facts, _ = loadOnboardingFacts(ctx, model.GetNetworkOnboarding(ctx, inFlight.networkId), now)
		if decision := onboarding.Decide(onboarding.StepE2, facts); decision.Action != onboarding.ActionExit || decision.ExitReason != onboarding.ExitSuppressed {
			tb.Fatal("the in-flight onboarding sequence did not exit", decision.Action, decision.ExitReason)
		}
		row := &model.NetworkReengagement{NetworkId: lapsed.networkId, Stream: reengagement.StreamLapsed, StartedAt: earlier}
		reengagementFacts := loadReengagementFacts(ctx, row, now)
		if decision := reengagement.Decide(reengagement.StepR1, reengagementFacts, reengagement.DefaultSchedule); decision.Action != reengagement.ActionExit || decision.ExitReason != reengagement.ExitSuppressed {
			tb.Fatal("re-engagement did not exit", decision.Action, decision.ExitReason)
		}

		// no connection lifts it
		for _, f := range written {
			if LiftNetworkMarketingSuppression(ctx, f.networkId, reengagement.LiftableSuppressions()) {
				tb.Fatal("a connection lifted an operator's suppression")
			}
		}

		// the same digest again writes nothing and queues nothing new
		again, err := SuppressNetworksMarketing(ctx, networkIds, reason, plan.Digest, settings)
		if err != nil {
			tb.Fatal(err)
		}
		if again.Suppressed != 0 || again.Counts.AlreadySuppressed != 5 || again.Counts.ToSuppress != 0 || pending() != len(written) {
			tb.Fatal("a repeat wrote", again.Suppressed, again.Counts, pending())
		}
	})
}
