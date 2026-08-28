package model

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// TestDiagnoseProbeFleet is the pure rule behind the credential warning.
//
// It is table-driven and needs no database, which is the point: this decides
// whether a warning is EMITTED, and a warning that silently never fires is
// exactly the class of fault this whole exercise is about. Testing the log call
// is impossible here (nothing in this repo captures glog output), so the
// decision is a pure function and the glog call is a one-line shell around it.
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
			why: "successes are stored as probe_failure = '', so a NAIVE ARGMAX over the tally " +
				"calls a perfectly healthy fleet '100% failing with class \"\"'. That is the most " +
				"likely bug in this function and this case is the only thing standing in front of " +
				"it. VERIFIED MUTATION: writing the naive argmax -- dropping BOTH the " +
				"ProbeAttemptSuccessClass skip inside the loop AND the post-loop guard -- makes " +
				"this case report class=\"\" 152/152. (Either guard alone still returns nil, so " +
				"they are deliberate defence in depth; removing just one is not enough to break it " +
				"and is not what this pins.)",
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
	// and the fallback still has to say something actionable rather than
	// nothing, since an unknown class is precisely when an operator has least
	// to go on
	if strings.TrimSpace(probeFleetHint("something_new")) == "" {
		t.Error("an unrecognised failure class must still produce a hint")
	}
}

// TestGetProviderEgressProbeAttemptTally pins the query the diagnosis reads.
//
// Both halves matter and both are asserted: successes must be tallied under
// ProbeAttemptSuccessClass (they are what the failure share is measured
// against, so losing them inflates every ratio to 100%), and failures must be
// tallied under their own class.
//
// MUTATION THAT MUST BREAK THIS: change the GROUP BY to filter out the empty
// class (`WHERE probe_failure != ”`), which is a plausible "tidy-up". The
// success bucket vanishes, the assertion below fails -- and had it not, every
// healthy fleet would have been diagnosed as 100% failing.
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
		// called out separately because it is the bucket a "tidy-up" deletes:
		// successes are stored as probe_failure = '' and are the denominator the
		// failure share is measured against, so dropping them makes every fleet
		// look 100% failing
		if tally[ProbeAttemptSuccessClass] < wrote[ProbeAttemptSuccessClass] {
			t.Errorf("successful attempts are not tallied under the empty class: "+
				"tally[%q] = %d. They are the denominator the failure share is measured "+
				"against, and without them every healthy fleet reads as 100%% failing",
				ProbeAttemptSuccessClass, tally[ProbeAttemptSuccessClass])
		}
	})
}

// The database-backed diagnosis must agree with the pure rule applied to the
// same table. This is the seam between the two halves: a query that returned
// the right rows and a predicate that read them under different key names would
// leave both unit tests green and the warning permanently silent.
func TestDiagnoseProviderEgressProbeFleetMatchesTheTally(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		tally := GetProviderEgressProbeAttemptTally(ctx)
		want := DiagnoseProbeFleet(tally)
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
