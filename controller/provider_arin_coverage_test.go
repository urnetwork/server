// Connection provenance distinguishes evaluated database misses from overrides.
package controller

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A real database miss is evaluated unknown, while an override is not a lookup.
func TestArinConnectionFactsPreserveLookupProvenance(t *testing.T) {
	for _, test := range []struct {
		epoch            int64
		risk, nonQuality bool
	}{{epoch: 0}, {epoch: 1800000000}, {epoch: 1800000001, risk: true, nonQuality: true}} {
		scores := &model.ConnectionLocationScores{}
		setArinConnectionFacts(scores, &server.ArinInfo{DatabaseBuildEpoch: test.epoch, Risk: test.risk, NonQuality: test.nonQuality}, nil)
		if scores.ArinDatabaseBuildEpoch != test.epoch || (scores.ArinLookupAt != nil) != (test.epoch > 0) || scores.ArinRisk != test.risk || scores.ArinNonQuality != test.nonQuality || scores.ArinQualityVerified {
			t.Fatalf("lookup provenance lost: %+v", scores)
		}
		setArinConnectionFacts(scores, &server.ArinInfo{}, nil)
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
				NonQuality: state != "subscriber", Risk: risk}, nil)
			want := state == "subscriber" && !risk
			if scores.ArinQualityVerified != want || scores.ArinNonQuality != (state != "subscriber") || scores.ArinRisk != risk {
				t.Fatal("connection facts changed the subscriber or independent risk decision")
			}
		}
	}
}

// Each slot carries the facts of its own database: the old database's
// subscriber verdict and risk go to the old slot only, with its epoch, and
// never change the latest slot.
func TestArinConnectionFactsKeepBothSlotsSeparate(t *testing.T) {
	latest := &server.ArinInfo{DatabaseBuildEpoch: 1800086400, ClassifierVersion: 1, QualityPolicyVersion: 2,
		QualityState: "unknown", NonQuality: true}
	old := &server.ArinInfo{DatabaseBuildEpoch: 1800000000, ClassifierVersion: 1, QualityPolicyVersion: 2,
		QualityState: "subscriber", Risk: false}
	scores := &model.ConnectionLocationScores{}
	setArinConnectionFacts(scores, latest, old)
	if scores.ArinDatabaseBuildEpoch != 1800086400 || scores.ArinQualityVerified || !scores.ArinNonQuality || scores.ArinRisk {
		t.Fatalf("old facts changed the latest slot: %+v", scores)
	}
	if scores.ArinOldDatabaseBuildEpoch != 1800000000 || !scores.ArinOldQualityVerified || scores.ArinOldNonQuality || scores.ArinOldRisk {
		t.Fatalf("old slot lost the old database's facts: %+v", scores)
	}
	oldRisky := *old
	oldRisky.Risk = true
	setArinConnectionFacts(scores, latest, &oldRisky)
	if !scores.ArinOldRisk || scores.ArinOldQualityVerified || scores.ArinRisk {
		t.Fatalf("old risk crossed slots or kept a verified fact: %+v", scores)
	}
}

// No old database, a failed old lookup (both nil) and an old lookup without a
// database epoch leave the old slot unavailable, and a later write without
// old facts clears earlier ones.
func TestArinConnectionFactsWithoutOldFactsLeaveTheOldSlotUnavailable(t *testing.T) {
	latest := &server.ArinInfo{DatabaseBuildEpoch: 1800086400, ClassifierVersion: 1, QualityPolicyVersion: 2, QualityState: "subscriber"}
	scores := &model.ConnectionLocationScores{}
	setArinConnectionFacts(scores, latest, &server.ArinInfo{DatabaseBuildEpoch: 1800000000, ClassifierVersion: 1,
		QualityPolicyVersion: 2, QualityState: "subscriber"})
	if !scores.ArinOldQualityVerified || scores.ArinOldDatabaseBuildEpoch == 0 {
		t.Fatalf("fixture old slot not stamped: %+v", scores)
	}
	for _, old := range []*server.ArinInfo{nil, {QualityState: "subscriber", ClassifierVersion: 1, QualityPolicyVersion: 2}} {
		setArinConnectionFacts(scores, latest, old)
		if scores.ArinOldDatabaseBuildEpoch != 0 || scores.ArinOldQualityVerified || scores.ArinOldNonQuality || scores.ArinOldRisk {
			t.Fatalf("unavailable old facts kept an earlier old slot: %+v", scores)
		}
		if !scores.ArinQualityVerified || scores.ArinDatabaseBuildEpoch != 1800086400 {
			t.Fatalf("unavailable old facts changed the latest slot: %+v", scores)
		}
	}
}
