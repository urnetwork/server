package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// testing_addEgressDeadVerdict writes one egress-dead verdict (receive acks
// zero) at an explicit time. The time is explicit because the primary key
// includes create_time: three reports written in the same clock tick would
// collapse to one row and the same-reporter test would pass for the wrong
// reason.
func testing_addEgressDeadVerdict(
	ctx context.Context,
	providerClientId server.Id,
	reporterNetworkId server.Id,
	createTime time.Time,
) {
	AddProviderClientVerdict(ctx, &ProviderClientVerdict{
		ProviderClientId:  providerClientId,
		ReporterNetworkId: reporterNetworkId,
		Reason:            ProviderClientVerdictReasonNoReceiveAck,
		SendAckCount:      64,
		SendAckBytes:      8192,
		ReceiveAckCount:   0,
		ReceiveAckBytes:   0,
		WindowSeconds:     30,
		CreateTime:        createTime,
	})
}

func testing_quorumMet(ctx context.Context, providerClientId server.Id, now time.Time) bool {
	return ProviderClientVerdictQuorumMet(
		GetProviderClientVerdictsInWindow(ctx, providerClientId, now),
		now,
	)
}

// Three distinct reporter networks, all inside the window, all egress-dead:
// this is the case the quorum exists to detect.
func TestProviderClientVerdictQuorumMetByThreeDistinctReporters(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		providerClientId := server.NewId()

		if testing_quorumMet(ctx, providerClientId, now) {
			t.Fatal("quorum met with no verdicts at all")
		}

		testing_addEgressDeadVerdict(ctx, providerClientId, server.NewId(), now.Add(-10*time.Minute))
		testing_addEgressDeadVerdict(ctx, providerClientId, server.NewId(), now.Add(-5*time.Minute))
		if testing_quorumMet(ctx, providerClientId, now) {
			t.Fatal("quorum met with two reporters, want three")
		}

		testing_addEgressDeadVerdict(ctx, providerClientId, server.NewId(), now.Add(-time.Minute))
		if !testing_quorumMet(ctx, providerClientId, now) {
			t.Fatal("quorum not met with three distinct reporters inside the window")
		}

		// verdicts about a different provider are not this provider's problem
		connect.AssertEqual(t, testing_quorumMet(ctx, server.NewId(), now), false)
	})
}

// THE ANTI-GRIEFING PROPERTY. One network, reporting as many times as it likes,
// moves the count by exactly one.
//
// The table is append-only on purpose -- a reporter may SAY anything, as often
// as it likes -- so this cap is the only thing standing between a single
// account and a manufactured quorum. Turn the reporter set in
// ProviderClientVerdictQuorumMet into a counter and this test must fail; if it
// still passes, the cap is not being enforced anywhere.
func TestProviderClientVerdictQuorumNotMetByOneReporterRepeating(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		providerClientId := server.NewId()
		griefer := server.NewId()

		// well past the quorum in raw rows
		for i := range 12 {
			testing_addEgressDeadVerdict(
				ctx,
				providerClientId,
				griefer,
				now.Add(-time.Duration(i+1)*time.Minute),
			)
		}

		// every row was written -- the cap is on what a reporter can count for,
		// never on what it can write
		verdicts := GetProviderClientVerdictsInWindow(ctx, providerClientId, now)
		connect.AssertEqual(t, len(verdicts), 12)

		if ProviderClientVerdictQuorumMet(verdicts, now) {
			t.Fatal("one reporter network met the quorum by repeating itself")
		}

		// and two honest reporters on top of the flood are still only three
		// networks short of nothing: 1 + 2 = 3 distinct, which IS a quorum. The
		// flood contributed exactly one.
		testing_addEgressDeadVerdict(ctx, providerClientId, server.NewId(), now.Add(-time.Minute))
		if testing_quorumMet(ctx, providerClientId, now) {
			t.Fatal("two distinct networks met the quorum")
		}
		testing_addEgressDeadVerdict(ctx, providerClientId, server.NewId(), now.Add(-time.Minute))
		if !testing_quorumMet(ctx, providerClientId, now) {
			t.Fatal("three distinct networks did not meet the quorum")
		}
	})
}

// A verdict that reports received acks describes a provider that carried
// traffic back, whatever reason string it carries. It counts for nothing.
func TestProviderClientVerdictReceivingProviderDoesNotCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		providerClientId := server.NewId()

		testing_addEgressDeadVerdict(ctx, providerClientId, server.NewId(), now.Add(-time.Minute))
		testing_addEgressDeadVerdict(ctx, providerClientId, server.NewId(), now.Add(-time.Minute))

		// third reporter, inside the window, but the provider acknowledged and
		// returned traffic. Note the reason still says no-receive-ack: the
		// counts are what the aggregation reads, never the reason string.
		AddProviderClientVerdict(ctx, &ProviderClientVerdict{
			ProviderClientId:  providerClientId,
			ReporterNetworkId: server.NewId(),
			Reason:            ProviderClientVerdictReasonNoReceiveAck,
			SendAckCount:      64,
			SendAckBytes:      8192,
			ReceiveAckCount:   17,
			ReceiveAckBytes:   4096,
			WindowSeconds:     30,
			CreateTime:        now.Add(-time.Minute),
		})

		verdicts := GetProviderClientVerdictsInWindow(ctx, providerClientId, now)
		// the row is stored and readable -- it is just not counted
		connect.AssertEqual(t, len(verdicts), 3)
		if ProviderClientVerdictQuorumMet(verdicts, now) {
			t.Fatal("a verdict with receive acks counted toward the quorum")
		}
	})
}

// Decay: a verdict older than the window contributes nothing. Without this a
// provider accumulates a quorum out of unrelated incidents days apart.
func TestProviderClientVerdictOutsideWindowDoesNotCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		providerClientId := server.NewId()
		stale := server.NewId()

		testing_addEgressDeadVerdict(ctx, providerClientId, server.NewId(), now.Add(-time.Minute))
		testing_addEgressDeadVerdict(ctx, providerClientId, server.NewId(), now.Add(-2*time.Minute))
		// one minute past the window
		testing_addEgressDeadVerdict(
			ctx,
			providerClientId,
			stale,
			now.Add(-ProviderClientVerdictWindow-time.Minute),
		)

		// the window read already drops it, so the third reporter is not even
		// offered to the aggregation
		verdicts := GetProviderClientVerdictsInWindow(ctx, providerClientId, now)
		connect.AssertEqual(t, len(verdicts), 2)
		if ProviderClientVerdictQuorumMet(verdicts, now) {
			t.Fatal("a decayed verdict counted toward the quorum")
		}

		// and the pure aggregation drops it on its own, handed the row
		// directly: the two layers are checked independently on purpose, since
		// the window read is a scan bound and the aggregation is the policy
		stalePlusTwo := append(verdicts, ProviderClientVerdict{
			ProviderClientId:  providerClientId,
			ReporterNetworkId: stale,
			Reason:            ProviderClientVerdictReasonNoReceiveAck,
			ReceiveAckCount:   0,
			CreateTime:        now.Add(-ProviderClientVerdictWindow - time.Minute),
		})
		if ProviderClientVerdictQuorumMet(stalePlusTwo, now) {
			t.Fatal("the aggregation counted a verdict from outside the window")
		}

		// the same reporter, inside the window, is the third network
		testing_addEgressDeadVerdict(ctx, providerClientId, stale, now.Add(-time.Minute))
		if !testing_quorumMet(ctx, providerClientId, now) {
			t.Fatal("a fresh third verdict did not meet the quorum")
		}
	})
}

// The reason allowlist is closed, and is exactly the three conditions that make
// connect's detectBlackhole fire.
func TestProviderClientVerdictReasonAllowlistIsClosed(t *testing.T) {
	connect.AssertEqual(t, IsProviderClientVerdictReason(ProviderClientVerdictReasonNoSendAck), true)
	connect.AssertEqual(t, IsProviderClientVerdictReason(ProviderClientVerdictReasonNoReceiveAck), true)
	connect.AssertEqual(t, IsProviderClientVerdictReason(ProviderClientVerdictReasonNoReceiveSyn), true)
	connect.AssertEqual(t, IsProviderClientVerdictReason("slow"), false)
	connect.AssertEqual(t, IsProviderClientVerdictReason(""), false)
}

// The one and only effect of a met quorum: the provider's stored observed_at
// moves back far enough that the prober is offered it, and NOTHING else about
// the row changes.
//
// The two-sided bound is the point. Too little and the provider is never
// offered; past ProviderEgressLocationMaxAge and the location stops being
// trusted by the connection path and the sweep deletes the row -- which would
// turn a client-verdict quorum into a selection-path effect, exactly what this
// design forbids.
func TestReprioritiseProviderEgressProbeMovesOnlyTheDueTime(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		clientId := server.NewId()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:      clientId,
			LocationId:    city.LocationId,
			CountryCode:   "us",
			ASN:           64500,
			Org:           "Example Hosting",
			Hosting:       true,
			CityConfident: true,
			ObservedAt:    now.Add(-time.Hour),
			Verdict:       "verified",
			VerdictReason: "",
		})
		before := GetProviderEgressLocation(ctx, clientId)
		if before == nil {
			t.Fatal("expected a stored egress location")
		}

		if !ReprioritiseProviderEgressProbe(ctx, clientId, now) {
			t.Fatal("reprioritise did not move a fresh row")
		}

		after := GetProviderEgressLocation(ctx, clientId)
		if after == nil {
			t.Fatal("reprioritise removed the row")
		}

		age := now.Sub(after.ObservedAt.UTC())
		// due: older than the api handler's cutoff, which is half the max age
		if age <= ProviderEgressLocationMaxAge/2 {
			t.Fatalf("observed_at is %s old, not old enough to be due (> %s)",
				age, ProviderEgressLocationMaxAge/2)
		}
		// still fresh: inside the max age, so the connection path still
		// resolves this location and the expiry sweep does not delete it
		if ProviderEgressLocationMaxAge <= age {
			t.Fatalf("observed_at is %s old, past the max age %s: quorum must not expire a location",
				age, ProviderEgressLocationMaxAge)
		}
		fresh := GetFreshProviderEgressLocation(ctx, clientId, ProviderEgressLocationMaxAge)
		if fresh == nil {
			t.Fatal("the location stopped being fresh: a met quorum must not change what selection sees")
		}

		// everything selection could read is byte-identical
		connect.AssertEqual(t, after.LocationId, before.LocationId)
		connect.AssertEqual(t, after.CountryCode, before.CountryCode)
		connect.AssertEqual(t, after.ASN, before.ASN)
		connect.AssertEqual(t, after.Org, before.Org)
		connect.AssertEqual(t, after.Hosting, before.Hosting)
		connect.AssertEqual(t, after.Proxy, before.Proxy)
		connect.AssertEqual(t, after.Mobile, before.Mobile)
		connect.AssertEqual(t, after.CityConfident, before.CityConfident)
		connect.AssertEqual(t, after.Verdict, before.Verdict)
		connect.AssertEqual(t, after.VerdictReason, before.VerdictReason)
		connect.AssertEqual(t, after.Assurance, before.Assurance)

		// NON-RATCHETING: a second quorum must not walk the row further back.
		// Repeated quorums that each subtracted a fixed age would eventually
		// push the location out of the freshness window, which is a slow
		// version of the exclusion this design forbids.
		if ReprioritiseProviderEgressProbe(ctx, clientId, now) {
			t.Fatal("a second reprioritise moved an already-due row")
		}
		again := GetProviderEgressLocation(ctx, clientId)
		if !again.ObservedAt.UTC().Equal(after.ObservedAt.UTC()) {
			t.Fatalf("observed_at ratcheted from %s to %s", after.ObservedAt.UTC(), again.ObservedAt.UTC())
		}

		// a provider with no row at all is not a miss: it has never been probed
		// successfully, so the due queue already sorts it ahead of every probed
		// provider and there is nothing to bring forward
		connect.AssertEqual(t, ReprioritiseProviderEgressProbe(ctx, server.NewId(), now), false)
	})
}
