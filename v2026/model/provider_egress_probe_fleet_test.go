package model

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// TestDiagnoseProbeFleet is the pure rule behind common-prober attribution.
//
// It is table-driven and needs no database, which is the point: this decides
// whether a warning is EMITTED, and a warning that silently never fires is
// exactly the class of fault this whole exercise is about. The decision is a
// pure function shared by the model, metrics, and monitor.
//
// The cases that carry the weight are the negative ones. Any predicate at all
// fires on "100% no_consensus"; only a correct one stays quiet on a healthy
// fleet and on a cold deployment.
func TestDiagnoseProbeFleet(t *testing.T) {
	cases := []struct {
		name  string
		tally map[string]int
		// wantClass is "" when no diagnosis must be produced.
		wantClass string
		why       string
	}{
		{
			name: "the incident: every provider no_consensus",
			tally: map[string]int{
				"no_consensus": 152,
			},
			wantClass: "no_consensus",
			why: "a prober whose jwt was rejected reported this shape for 8 hours and nothing " +
				"said credential; if this case stops producing a diagnosis the warning is gone",
		},
		{
			name: "healthy fleet, every attempt succeeded",
			tally: map[string]int{
				ProbeAttemptSuccessClass: 152,
			},
			wantClass: "",
			why: "successes are stored as probe_failure = '', so a naive argmax over a complete " +
				"outcome tally calls a perfectly healthy fleet '100% failing with class \"\"'. " +
				"This pins that the success bucket contributes to the eligible and observed " +
				"denominators but can never become a failure class",
		},
		{
			name: "healthy fleet with an ordinary minority of failures",
			tally: map[string]int{
				ProbeAttemptSuccessClass: 140,
				"no_consensus":           8,
				"tunnel_failed":          4,
			},
			wantClass: "",
			why: "every fleet has a characteristic failure mode; 8 of 152 is normal operation. " +
				"MUTATION: measure the dominant share over the FAILURES (8/12) instead of over " +
				"all attempts (8/152) and this case fires, making the warning constant noise",
		},
		{
			name: "cold deployment: three attempts, all failed",
			tally: map[string]int{
				"tunnel_failed": 3,
			},
			wantClass: "",
			why: "3 providers failing is evidence about 3 providers, not about a fleet. " +
				"MUTATION: set MinProbeFleetDiagnosisAttempts to 0 and this case fires -- the " +
				"wolf-crier that teaches operators to ignore the message, after which the real " +
				"incident goes unread too",
		},
		{
			name: "exactly at the sample floor, wholly failing",
			tally: map[string]int{
				"no_consensus": MinProbeFleetDiagnosisAttempts,
			},
			wantClass: "no_consensus",
			why:       "the floor is inclusive; at the floor with 100% failure there is a real finding",
		},
		{
			name: "one attempt below the sample floor, wholly failing",
			tally: map[string]int{
				"no_consensus": MinProbeFleetDiagnosisAttempts - 1,
			},
			wantClass: "",
			why: "pins the floor's exact boundary; a test only at 3-vs-152 would pass with the " +
				"floor set to any value in between",
		},
		{
			name: "dominant class at exactly the share threshold",
			tally: map[string]int{
				"no_consensus":           90,
				ProbeAttemptSuccessClass: 10,
			},
			wantClass: "no_consensus",
			why:       "90/100 is exactly 9/10 and the comparison is >=, so this must fire",
		},
		{
			name: "dominant class one below the share threshold",
			tally: map[string]int{
				"no_consensus":           89,
				ProbeAttemptSuccessClass: 11,
			},
			wantClass: "",
			why: "89/100 is under 9/10 and must not fire. With the case above, this pins the " +
				"boundary exactly -- MUTATION: loosen the ratio to 4/5 and this case starts firing",
		},
		{
			name: "a real fleet outage spread across several classes",
			tally: map[string]int{
				"tunnel_failed":   60,
				"no_consensus":    50,
				"contract_failed": 42,
			},
			wantClass: "",
			why: "everything is failing but in THREE different ways, so the providers are not " +
				"failing identically and the prober is not implicated as the single common " +
				"factor. Diagnosing here would send an operator to check a credential during a " +
				"genuine fleet-wide outage",
		},
		{
			name:      "empty tally",
			tally:     map[string]int{},
			wantClass: "",
			why:       "nothing has been attempted; there is nothing to conclude",
		},
		{
			name: "zero-count buckets are not observations",
			tally: map[string]int{
				"no_consensus":           30,
				"tunnel_failed":          0,
				ProbeAttemptSuccessClass: 0,
			},
			wantClass: "no_consensus",
			why: "a zero bucket must not count toward the denominator or become the dominant " +
				"class; 30/30 is still a complete failure",
		},
	}

	for _, c := range cases {
		got := DiagnoseProbeFleet(c.tally)

		if c.wantClass == "" {
			if got != nil {
				t.Errorf("%s: got a diagnosis (class=%q %d/%d), want none\n  %s",
					c.name, got.DominantClass, got.DominantCount, got.Attempts, c.why)
			}
			continue
		}

		if got == nil {
			t.Errorf("%s: got no diagnosis, want dominant class %q\n  %s",
				c.name, c.wantClass, c.why)
			continue
		}
		if got.DominantClass != c.wantClass {
			t.Errorf("%s: dominant class = %q, want %q\n  %s",
				c.name, got.DominantClass, c.wantClass, c.why)
		}
		// the hint is the part the incident was missing -- the operator had the
		// failure class the whole time and still had no reason to suspect a
		// credential. A diagnosis with an empty hint reports the symptom and
		// withholds the diagnosis.
		if strings.TrimSpace(got.Hint) == "" {
			t.Errorf("%s: diagnosis carries no hint; naming the class without naming the "+
				"likely cause is what left the 8h outage unexplained", c.name)
		}
	}
}

// The population-aware assessment prevents the retained-attempt survivor bias
// that originally made a six-hour failure cohort look like a fleet. Successes
// may remain represented by trusted locations for seven days, while failures
// retry every six hours; unobserved providers therefore stay in the eligible
// denominator without being invented as either outcome.
func TestAssessProbeFleetOutcomesUsesTheEligiblePopulation(t *testing.T) {
	tests := []struct {
		name             string
		tally            map[string]int
		wantHigh         bool
		wantDominant     string
		wantEligible     int
		wantObserved     int
		wantFailures     int
		wantUnobserved   int
		wantInconsistent int
	}{
		{
			name: "common known failure with enough observed outcomes",
			tally: map[string]int{
				"no_consensus":            20,
				ProbeFleetUnobservedClass: 2,
			},
			wantHigh: true, wantDominant: "no_consensus",
			wantEligible: 22, wantObserved: 20, wantFailures: 20, wantUnobserved: 2,
		},
		{
			name: "observed floor cannot replace the complete eligible denominator",
			tally: map[string]int{
				"no_consensus":            20,
				ProbeFleetUnobservedClass: 3,
			},
			wantEligible: 23, wantObserved: 20, wantFailures: 20, wantUnobserved: 3,
		},
		{
			name: "eligible floor does not replace the observed floor",
			tally: map[string]int{
				"no_consensus":            18,
				ProbeFleetUnobservedClass: 2,
			},
			wantEligible: 20, wantObserved: 18, wantFailures: 18, wantUnobserved: 2,
		},
		{
			name: "mixed failure can cross total share without false attribution",
			tally: map[string]int{
				"no_consensus":           10,
				"locate_failed":          8,
				ProbeAttemptSuccessClass: 2,
			},
			wantHigh:     true,
			wantEligible: 20, wantObserved: 20, wantFailures: 18,
		},
		{
			name: "collapsed unknown classes never establish common mode",
			tally: map[string]int{
				ProbeFleetUnknownFailureClass: 20,
			},
			wantHigh:     true,
			wantEligible: 20, wantObserved: 20, wantFailures: 20,
		},
		{
			name: "unobserved and inconsistent states are neutral",
			tally: map[string]int{
				ProbeFleetUnobservedClass:   30,
				ProbeFleetInconsistentClass: 4,
			},
			wantEligible: 34, wantObserved: 0, wantUnobserved: 30, wantInconsistent: 4,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := AssessProbeFleetOutcomes(test.tally)
			if got.Eligible != test.wantEligible || got.Observed != test.wantObserved ||
				got.Failures != test.wantFailures || got.Unobserved != test.wantUnobserved ||
				got.Inconsistent != test.wantInconsistent || got.FailureShareHigh != test.wantHigh {
				t.Fatalf("assessment = %+v", got)
			}
			if test.wantDominant == "" {
				if got.Dominant != nil {
					t.Fatalf("dominant = %+v, want nil", got.Dominant)
				}
			} else if got.Dominant == nil || got.Dominant.DominantClass != test.wantDominant ||
				got.Dominant.Eligible != test.wantEligible || got.Dominant.Attempts != test.wantObserved {
				t.Fatalf("dominant = %+v, want class=%s eligible=%d observed=%d",
					got.Dominant, test.wantDominant, test.wantEligible, test.wantObserved)
			}
		})
	}
}

// The two failure classes that mean "the prober never got a tunnel up" must
// point at credentials explicitly, by name.
//
// This is the one assertion that directly encodes "nothing said credential".
// A generic "something is wrong with probing" hint would satisfy the non-empty
// check in the table test above while still leaving the operator exactly where
// the incident left them.
//
// MUTATION THAT MUST BREAK THIS: collapse probeFleetHint's switch to its
// default branch. The word "CREDENTIAL" disappears from the no_consensus and
// tunnel_failed hints and this fails.
func TestProbeFleetHintNamesTheCredential(t *testing.T) {
	for _, class := range []string{"no_consensus", "tunnel_failed", "contract_failed"} {
		hint := probeFleetHint(class)
		if !strings.Contains(hint, "CREDENTIAL") {
			t.Errorf("probeFleetHint(%q) = %q, which never mentions a credential. "+
				"A prober whose jwt was rejected produces exactly this class fleet-wide, and the "+
				"operator's 8h of investigation went to the providers because nothing pointed at auth",
				class, hint)
		}
	}
	if hint := probeFleetHint("no_consensus"); strings.Contains(hint, "UR_PROBER_BY_JWT") ||
		!strings.Contains(hint, "prober_identity") {
		t.Fatalf("credential hint does not describe the current persisted identity boundary: %q", hint)
	}
	// and the fallback still has to say something actionable rather than
	// nothing, since an unknown class is precisely when an operator has least
	// to go on
	if strings.TrimSpace(probeFleetHint("something_new")) == "" {
		t.Error("an unrecognised failure class must still produce a hint")
	}
}

// TestGetProviderEgressProbeAttemptTally pins the raw retained-attempt
// instrumentation query. Fleet diagnosis must use the population-aware query.
//
// Both halves matter and both are asserted: successes must be tallied under
// ProbeAttemptSuccessClass, and failures must be tallied under their own class.
//
// MUTATION THAT MUST BREAK THIS: change the GROUP BY to filter out the empty
// class (`WHERE probe_failure != ”`), which is a plausible "tidy-up". The
// success bucket vanishes and the assertion below fails.
func TestGetProviderEgressProbeAttemptTally(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		// three failures of one class, two of another, and two successes
		wrote := map[string]int{
			"no_consensus":           3,
			"tunnel_failed":          2,
			ProbeAttemptSuccessClass: 2,
		}
		for class, n := range wrote {
			for i := 0; i < n; i++ {
				SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
					ClientId:     server.NewId(),
					AttemptAt:    now,
					ProbeFailure: class,
				})
			}
		}

		tally := GetProviderEgressProbeAttemptTally(ctx)

		// asserted as ">= what this test wrote" rather than as equality: this
		// reads the whole table, so an exact count would be a statement about
		// every other test's fixtures rather than about the grouping
		for class, n := range wrote {
			if tally[class] < n {
				t.Errorf("tally[%q] = %d, want at least the %d this test wrote",
					class, tally[class], n)
			}
		}
		// Called out separately because it is the bucket a "tidy-up" deletes.
		// Production attribution does not consume this survivor-biased tally, but
		// raw attempt instrumentation still has to represent successful rows.
		if tally[ProbeAttemptSuccessClass] < wrote[ProbeAttemptSuccessClass] {
			t.Errorf("successful attempts are not tallied under the empty class: "+
				"tally[%q] = %d; raw attempt instrumentation has lost successful rows",
				ProbeAttemptSuccessClass, tally[ProbeAttemptSuccessClass])
		}
	})
}

// The fleet view must not mistake the six-hour failure retry cadence for the
// current eligible population. This synthetic population exercises the exact
// write-order and freshness boundaries used by the monitor query.
func TestGetProviderEgressProbeFleetOutcomeTallyReconstructsEligibleState(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		location := &Location{
			LocationType: LocationTypeCity,
			City:         "Synthetic City",
			Region:       "Synthetic Region",
			Country:      "Synthetic Country",
			CountryCode:  "zz",
		}
		CreateLocation(ctx, location)

		freshOnly := server.NewId()
		plainFailure := server.NewId()
		currentFailure := server.NewId()
		neverObserved := server.NewId()
		ambiguousLaterLocation := server.NewId()
		retainedFailure := server.NewId()
		inconsistent := server.NewId()
		ineligible := server.NewId()
		for index, clientId := range []server.Id{
			freshOnly, plainFailure, currentFailure, neverObserved, ambiguousLaterLocation,
			retainedFailure, inconsistent,
		} {
			testing_connectProbeableProvider(
				t, ctx, clientId, location.LocationId,
				fmt.Sprintf("192.0.2.%d:0", index+1),
				ProvideModePublic,
			)
		}
		testing_connectProbeableProvider(
			t, ctx, ineligible, location.LocationId, "198.51.100.1:0", ProvideModeNetwork,
		)
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: freshOnly, LocationId: location.LocationId,
			CountryCode: "zz", ObservedAt: now.Add(-2 * time.Hour),
		})
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: plainFailure, AttemptAt: now.Add(-time.Hour), ProbeFailure: "no_consensus",
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: currentFailure, LocationId: location.LocationId,
			CountryCode: "zz", ObservedAt: now.Add(-2 * time.Hour),
		})
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: currentFailure, AttemptAt: now.Add(-time.Hour), ProbeFailure: "no_consensus",
		})
		// Reprioritisation writes location.update_time without a successful
		// probe. It must not make this current failure look healthy.
		if !ReprioritiseProviderEgressProbe(ctx, currentFailure, now) {
			t.Fatal("synthetic current failure location was not reprioritised")
		}

		// A location written after a retained failure is ambiguous because the
		// attempt report for that later success may itself have failed. Remain
		// conservative until a new success attempt replaces the failure.
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: ambiguousLaterLocation, AttemptAt: now.Add(-time.Hour), ProbeFailure: "tunnel_failed",
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: ambiguousLaterLocation, LocationId: location.LocationId,
			CountryCode: "zz", ObservedAt: now.Add(-30 * time.Minute),
		})

		// A retained failure newer than the location but outside the six-hour
		// retry window is unobserved. It cannot resurrect the older success.
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: retainedFailure, LocationId: location.LocationId,
			CountryCode: "zz", ObservedAt: now.Add(-2 * time.Hour),
		})
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: retainedFailure, AttemptAt: now.Add(-7 * time.Hour), ProbeFailure: "tunnel_failed",
		})

		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: inconsistent, AttemptAt: now.Add(-time.Hour), ProbeFailure: "",
		})
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{
			ClientId: ineligible, AttemptAt: now.Add(-time.Hour), ProbeFailure: "no_consensus",
		})

		got := GetProviderEgressProbeFleetOutcomeTally(ctx)
		want := map[string]int{
			ProbeAttemptSuccessClass:    1,
			"no_consensus":              1,
			ProbeFleetUnobservedClass:   4,
			ProbeFleetInconsistentClass: 1,
		}
		for class, count := range want {
			if got[class] != count {
				t.Errorf("outcome tally[%q] = %d, want %d; all=%v", class, got[class], count, got)
			}
		}
		if assessment := AssessProbeFleetOutcomes(got); assessment.Eligible != 7 ||
			assessment.Observed != 2 || assessment.Failures != 1 ||
			assessment.Inconsistent != 1 || assessment.Unobserved != 4 {
			t.Fatalf("assessment = %+v, want eligible=7 observed=2 failures=1 inconsistent=1 unobserved=4", assessment)
		}
	})
}

// The database-backed diagnosis must agree with the pure rule applied to the
// reconstructed eligible population. It must never fall back to the retained,
// survivor-biased attempt table.
func TestDiagnoseProviderEgressProbeFleetMatchesTheTally(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		tally := GetProviderEgressProbeFleetOutcomeTally(ctx)
		want := AssessProbeFleetOutcomes(tally).Dominant
		got := DiagnoseProviderEgressProbeFleet(ctx)

		switch {
		case want == nil && got != nil:
			t.Errorf("the db-backed diagnosis produced %+v where the pure rule over the same "+
				"tally %v produced none", got, tally)
		case want != nil && got == nil:
			t.Errorf("the db-backed diagnosis produced nothing where the pure rule over the "+
				"same tally %v produced %+v", tally, want)
		case want != nil && got != nil && want.DominantClass != got.DominantClass:
			t.Errorf("db-backed dominant class = %q, pure rule = %q over tally %v",
				got.DominantClass, want.DominantClass, tally)
		}
	})
}
