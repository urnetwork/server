package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func shadowScoreTestSource(t testing.TB, capacity int) (*ArinShadowScoreCapture, *arinShadowScoreAttempt) {
	t.Helper()
	c, err := NewArinShadowScoreCapture(capacity)
	if err != nil {
		t.Fatal(err)
	}
	InstallArinShadowScoreCapture(c)
	t.Cleanup(c.Close)
	return c, beginArinShadowScoreCapture()
}

func shadowScoreTestRow(id server.Id) *ClientScore {
	return &ClientScore{ClientId: id, LookbackIndex: 0, IndependentReliabilityWeight: 1, Scores: map[string]int{RankModeQuality: 1, RankModeSpeed: 1}}
}

func shadowScoreTestCensus(start time.Time, q, s, o int) *ClientScoreNativeCensus {
	return &ClientScoreNativeCensus{SchemaVersion: 1, PublicationId: server.NewId().String(), SourceStartedAt: start, SourceCompletedAt: start.Add(time.Millisecond), PublishedAt: start.Add(2 * time.Millisecond), Buckets: map[string]ClientScoreNativeBucketCount{RankModeQuality: {Providers: q}, RankModeSpeed: {Providers: s}, "online": {Providers: o}}}
}

func TestArinCurrentScoreCaptureReusesNativeDecisionsAndOnlyRemovesArinInputs(t *testing.T) {
	for _, which := range []string{"arin_risk", "arin_non_quality", "tls_failure", "reliability_failure", "measured_failure", "unmeasured", "performance_missing", "reliability_weight", "conflicting_repeat"} {
		t.Run(which, func(t *testing.T) {
			c, a := shadowScoreTestSource(t, 8)
			id := server.NewId()
			row := shadowScoreTestRow(id)
			country := "us"
			passing := true
			facts := &providerEgressFacts{egressQuality: &passing}
			switch which {
			case "arin_risk":
				facts.arinRisk = true
			case "arin_non_quality":
				facts.arinNonQuality = true
			case "tls_failure":
				facts.tlsAuthenticationFailed = true
			case "reliability_failure":
				facts.reliabilityFailed = true
			case "measured_failure":
				passing = false
			case "unmeasured":
				facts.egressQuality = nil
			case "performance_missing":
				row.Scores = map[string]int{}
			case "reliability_weight":
				row.LookbackIndex = 1
				row.IndependentReliabilityWeight = 0
			}
			a.observe(row, &country, facts, true)
			if which == "conflicting_repeat" {
				country = "ca"
				a.observe(row, &country, facts, true)
			}
			a.publish(shadowScoreTestCensus(server.NowUtc(), 0, 0, 0))
			snapshot := c.Snapshot()
			if snapshot == nil {
				t.Fatal("source unexpectedly unavailable")
			}
			member := snapshot.member(id, 1)
			wantBase := which == "arin_risk" || which == "arin_non_quality" || which == "conflicting_repeat"
			if member.BaseQuality != wantBase || member.BaseSpeed != wantBase || member.ActiveQuality || member.ActiveSpeed {
				t.Fatal("non-ARIN gate changed or native membership invented")
			}
			if member.MembershipUnavailable != (which == "conflicting_repeat") {
				t.Fatal("conflicting source not indeterminate")
			}
			if missing := snapshot.member(server.NewId(), 1); !missing.MembershipUnavailable || missing.ActiveQuality || missing.BaseQuality {
				t.Fatal("absent source member became measured false/positive")
			}
		})
	}
}

func TestArinCurrentScoreCapturePublicationAndCapacityFences(t *testing.T) {
	c, a := shadowScoreTestSource(t, 1)
	id := server.NewId()
	row := shadowScoreTestRow(id)
	country := "us"
	passing := true
	a.observe(row, &country, &providerEgressFacts{egressQuality: &passing}, true)
	if c.Snapshot() != nil {
		t.Fatal("unpublished source exposed")
	}
	native := *row
	native.PassesMinimums = map[string]bool{RankModeQuality: true, RankModeSpeed: true}
	native.Online = true
	targets := map[server.Id]map[server.Id]*ClientScore{server.NewId(): {id: &native}}
	census := shadowScoreTestCensus(server.NowUtc(), 1, 1, 1)
	a.publish(census, targets, targets)
	first := c.Snapshot()
	if first == nil || !first.member(id, 1).ActiveQuality {
		t.Fatal("deduplicated actual native membership lost")
	}
	a.publish(shadowScoreTestCensus(server.NowUtc(), 0, 1, 1), targets)
	if c.Snapshot() != nil {
		t.Fatal("same-looking mismatched publication certified")
	}
	b := beginArinShadowScoreCapture()
	b.observe(row, &country, &providerEgressFacts{egressQuality: &passing}, true)
	b.observe(shadowScoreTestRow(server.NewId()), &country, &providerEgressFacts{egressQuality: &passing}, true)
	b.publish(census, targets)
	if c.Snapshot() != nil {
		t.Fatal("capacity overflow silently omitted a source member")
	}
	c.Close()
	a.publish(census, targets)
	if c.Snapshot() != nil {
		t.Fatal("closed source published")
	}
}

func TestArinCurrentScoreCaptureValidatesActualRedisGeneration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		c, a := shadowScoreTestSource(t, 4)
		id := server.NewId()
		row := shadowScoreTestRow(id)
		country := "us"
		passing := true
		a.observe(row, &country, &providerEgressFacts{egressQuality: &passing}, true)
		row.PassesMinimums = map[string]bool{RankModeQuality: true, RankModeSpeed: true}
		row.Online = true
		target := map[server.Id]map[server.Id]*ClientScore{server.NewId(): {id: row}}
		start := server.NowUtc().Add(-time.Second)
		end := server.NowUtc()
		census := newClientScoreNativeCensus(start, end, end, map[server.Id]ProviderEgressHealthCounts{}, target)
		if err := writeClientScoreNativeCensus(ctx, census, time.Minute); err != nil {
			t.Fatal(err)
		}
		a.publish(census, target)
		snapshot := c.Snapshot()
		if snapshot == nil || !snapshot.Validate(ctx) {
			t.Fatal("published generation unavailable")
		}
		// Equal counts and a fresh publication clock are insufficient: the
		// complete immutable publication identity must remain identical.
		replacement := *census
		replacement.PublicationId = server.NewId().String()
		if err := writeClientScoreNativeCensus(ctx, &replacement, time.Minute); err != nil {
			t.Fatal(err)
		}
		if snapshot.Validate(ctx) {
			t.Fatal("same-count replacement reused old memberships")
		}
	})
}

func TestArinCurrentScoreCaptureOwningPublisherPreservesServingAndPreArinGates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := testing_healthGateCity(ctx, t)
		ids := testing_connectQualifyingProviders(ctx, t, city, 4)
		testing_setProviderEgressHealth(ctx, ids[0], 1, 1)
		testing_setProviderEgressHealth(ctx, ids[1], 1, 1)
		testing_setProviderEgressHealth(ctx, ids[2], 0, 1)
		testing_rollUpEgress(ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET arin_risk=true WHERE client_id=$1`, ids[1]))
		})
		if err := UpdateClientScores(ctx, time.Hour, 1); err != nil {
			t.Fatal(err)
		}
		before, err := GetClientScoreNativeCensus(ctx)
		if err != nil || before == nil {
			t.Fatal("baseline source missing")
		}
		c, _ := shadowScoreTestSource(t, 16)
		if err := UpdateClientScores(ctx, time.Hour, 1); err != nil {
			t.Fatal(err)
		}
		after, err := GetClientScoreNativeCensus(ctx)
		snapshot := c.Snapshot()
		if err != nil || after == nil || snapshot == nil || !snapshot.Validate(ctx) {
			t.Fatal("owning publication was not observed", err)
		}
		for _, bucket := range []string{RankModeQuality, RankModeSpeed, "online"} {
			if before.Buckets[bucket].Providers != after.Buckets[bucket].Providers {
				t.Fatal("observer changed serving membership")
			}
		}
		for i, id := range ids {
			member := snapshot.member(id, 1)
			if member.MembershipUnavailable {
				t.Fatal("complete owning source unexpectedly indeterminate")
			}
			if member.BaseQuality != (i < 2) || member.BaseSpeed != (i < 2) || member.ActiveQuality != (i == 0) || member.ActiveSpeed != (i == 0) {
				t.Fatal("owning pre-ARIN/native gate mismatch")
			}
		}
		// An enabled observer with insufficient memory fails its own snapshot;
		// the real publisher still succeeds with identical native membership.
		limited, err := NewArinShadowScoreCapture(1)
		if err != nil {
			t.Fatal(err)
		}
		defer limited.Close()
		InstallArinShadowScoreCapture(limited)
		if err := UpdateClientScores(ctx, time.Hour, 1); err != nil {
			t.Fatal("diagnostic overflow broke serving", err)
		}
		last, err := GetClientScoreNativeCensus(ctx)
		if err != nil || last == nil || limited.Snapshot() != nil {
			t.Fatal("overflow concealed or publication lost")
		}
		for _, bucket := range []string{RankModeQuality, RankModeSpeed, "online"} {
			if last.Buckets[bucket].Providers != after.Buckets[bucket].Providers {
				t.Fatal("overflow changed serving membership")
			}
		}
	})
}
