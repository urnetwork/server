// Connection provenance distinguishes evaluated database misses from overrides.
package controller

import (
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A real database miss is evaluated unknown, while an override is not a lookup.
func TestArinConnectionFactsPreserveLookupProvenance(t *testing.T) {
	for _, test := range []struct {
		epoch            int64
		risk, nonQuality bool
	}{{epoch: 0}, {epoch: 1800000000}, {epoch: 1800000001, risk: true, nonQuality: true}} {
		scores := &model.ConnectionLocationScores{}
		setArinConnectionFacts(scores, &server.ArinInfo{DatabaseBuildEpoch: test.epoch, Risk: test.risk, NonQuality: test.nonQuality})
		if scores.ArinDatabaseBuildEpoch != test.epoch || (scores.ArinLookupAt != nil) != (test.epoch > 0) || scores.ArinRisk != test.risk || scores.ArinNonQuality != test.nonQuality || scores.ArinQualityVerified {
			t.Fatalf("lookup provenance lost: %+v", scores)
		}
		setArinConnectionFacts(scores, &server.ArinInfo{})
		if scores.ArinLookupAt != nil || scores.ArinDatabaseBuildEpoch != 0 {
			t.Fatal("an override inherited another lookup's attestation")
		}
	}
}

// Only an attested subscriber lookup may produce a persisted affirmative fact.
func TestArinConnectionFactsRequireAffirmativeSubscriber(t *testing.T) {
	for _, state := range []string{"subscriber", "excluded", "unknown", "ambiguous"} {
		for _, risk := range []bool{false, true} {
			scores := &model.ConnectionLocationScores{}
			setArinConnectionFacts(scores, &server.ArinInfo{DatabaseBuildEpoch: 1800000000,
				ClassifierVersion: 1, QualityPolicyVersion: 2, QualityState: state,
				NonQuality: state != "subscriber", Risk: risk})
			want := state == "subscriber" && !risk
			if scores.ArinQualityVerified != want || scores.ArinNonQuality != (state != "subscriber") || scores.ArinRisk != risk {
				t.Fatal("connection facts changed the subscriber or independent risk decision")
			}
		}
	}
}
