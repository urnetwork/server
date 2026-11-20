// A provider is judged by its probed exit (provider_egress_exit_model.go).
// Pure: a connection's ARIN facts become its exit's Quality facts, and a
// provider whose residential connection exits from a datacenter leaves Quality
// but stays in Speed and Online. The network comparison needs the address hash
// pepper, so it is tested with the database (provider_egress_exit_model_test.go).
package model

import (
	"testing"
	"time"
)

// The build epochs of the fixtures' latest and old ARIN databases.
const (
	testProviderExitLatestEpoch = int64(1800086400)
	testProviderExitOldEpoch    = int64(1800000000)
)

// The facts a residential subscriber address carries in both slots.
func testProviderExitResidentialScores() ConnectionLocationScores {
	lookupAt := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	return ConnectionLocationScores{
		ArinQualityVerified:       true,
		ArinDatabaseBuildEpoch:    testProviderExitLatestEpoch,
		ArinLookupAt:              &lookupAt,
		ArinOldQualityVerified:    true,
		ArinOldDatabaseBuildEpoch: testProviderExitOldEpoch,
	}
}

// The judgement of a datacenter exit, unknown to the subscriber catalog in
// both slots.
func testProviderExitDatacenterJudgement() *ProviderEgressExitJudgement {
	return &ProviderEgressExitJudgement{
		ObservedAt:                time.Date(2026, 11, 20, 12, 5, 0, 0, time.UTC),
		ArinNonQuality:            true,
		ArinDatabaseBuildEpoch:    testProviderExitLatestEpoch,
		ArinLookupAt:              time.Date(2026, 11, 20, 12, 5, 1, 0, time.UTC),
		ArinOldNonQuality:         true,
		ArinOldDatabaseBuildEpoch: testProviderExitOldEpoch,
	}
}

// A residential connection whose provider exits from a datacenter carries the
// datacenter's Quality facts in both slots; its own risk stays.
func TestProviderEgressExitJudgesQualityByTheExit(t *testing.T) {
	scores := testProviderExitResidentialScores()
	judgement := testProviderExitDatacenterJudgement()
	judgement.ApplyArinFacts(&scores)
	if !scores.ArinNonQuality || scores.ArinQualityVerified || scores.ArinDatabaseBuildEpoch != testProviderExitLatestEpoch {
		t.Fatalf("the latest slot was not judged by the exit: %+v", scores)
	}
	if scores.ArinLookupAt == nil || !scores.ArinLookupAt.Equal(judgement.ArinLookupAt) {
		t.Fatalf("the latest slot kept its own lookup time %v, want the exit's %v", scores.ArinLookupAt, judgement.ArinLookupAt)
	}
	if !scores.ArinOldNonQuality || scores.ArinOldQualityVerified || scores.ArinOldDatabaseBuildEpoch != testProviderExitOldEpoch {
		t.Fatalf("the old slot was not judged by the exit: %+v", scores)
	}
	if scores.ArinRisk || scores.ArinOldRisk {
		t.Fatalf("the exit's judgement changed the connection's risk: %+v", scores)
	}
}

// A verified exit never verifies a risky connection: risk stays the
// connection address's in each slot, and a risky slot is never verified.
func TestProviderEgressExitKeepsTheConnectionsRisk(t *testing.T) {
	scores := testProviderExitResidentialScores()
	scores.ArinRisk = true
	scores.ArinQualityVerified = false
	scores.ArinOldRisk = true
	scores.ArinOldQualityVerified = false
	verifiedExit := &ProviderEgressExitJudgement{
		ArinQualityVerified:       true,
		ArinDatabaseBuildEpoch:    testProviderExitLatestEpoch,
		ArinOldQualityVerified:    true,
		ArinOldDatabaseBuildEpoch: testProviderExitOldEpoch,
	}
	verifiedExit.ApplyArinFacts(&scores)
	if !scores.ArinRisk || scores.ArinQualityVerified || !scores.ArinOldRisk || scores.ArinOldQualityVerified {
		t.Fatalf("a verified exit cleared or verified a risky connection: %+v", scores)
	}
}

// An exit without old facts clears the connection's old slot, so a fallback to
// the old database cannot judge the connection by its own address. An exit no
// database classified, and no judgement at all, change nothing.
func TestProviderEgressExitSlotsFollowTheExit(t *testing.T) {
	scores := testProviderExitResidentialScores()
	judgement := testProviderExitDatacenterJudgement()
	judgement.ArinOldNonQuality = false
	judgement.ArinOldDatabaseBuildEpoch = 0
	judgement.ApplyArinFacts(&scores)
	if scores.ArinOldDatabaseBuildEpoch != 0 || scores.ArinOldQualityVerified || scores.ArinOldNonQuality || scores.ArinOldRisk {
		t.Fatalf("an exit without old facts left the connection's old slot: %+v", scores)
	}
	if !scores.ArinNonQuality || scores.ArinDatabaseBuildEpoch != testProviderExitLatestEpoch {
		t.Fatalf("the latest slot was not judged by the exit: %+v", scores)
	}

	for _, unjudged := range []*ProviderEgressExitJudgement{nil, {ArinNonQuality: true, ExitDiffers: true}} {
		scores := testProviderExitResidentialScores()
		unjudged.ApplyArinFacts(&scores)
		if scores.ArinNonQuality || !scores.ArinQualityVerified || scores.ArinDatabaseBuildEpoch != testProviderExitLatestEpoch ||
			scores.ArinOldNonQuality || !scores.ArinOldQualityVerified || scores.ArinOldDatabaseBuildEpoch != testProviderExitOldEpoch {
			t.Fatalf("an unclassified exit %+v changed the connection's facts: %+v", unjudged, scores)
		}
	}
	// a nil judgement and nil scores are no-ops
	(*ProviderEgressExitJudgement)(nil).ApplyArinFacts(nil)
	judgement.ApplyArinFacts(nil)
}

// A provider connecting from a verified residential address but exiting from
// a datacenter is in Quality when judged by its connection, as before, and
// out of Quality but still in Speed and Online when judged by its exit. Its
// facts reach the decision as the reliability rollup folds them, under the
// subscriber policy: non-Quality, or not verified.
func TestProviderEgressExitLeavesQualityKeepsSpeedAndOnline(t *testing.T) {
	decide := func(scores ConnectionLocationScores) providerEgressDecision {
		quality := true
		return decideProviderEgress(&providerEgressFacts{
			arinRisk:       scores.ArinRisk,
			arinNonQuality: scores.ArinNonQuality || !scores.ArinQualityVerified,
			egressQuality:  &quality,
			qualityRunsMet: true,
			speedRunsMet:   true,
		}, true)
	}

	byConnection := decide(testProviderExitResidentialScores())
	if !byConnection.quality || !byConnection.speed || !byConnection.online {
		t.Fatalf("judged by its connection: %+v, want quality, speed and online", byConnection)
	}

	scores := testProviderExitResidentialScores()
	testProviderExitDatacenterJudgement().ApplyArinFacts(&scores)
	byExit := decide(scores)
	if byExit.quality {
		t.Fatalf("judged by its datacenter exit, the provider is still in Quality: %+v", byExit)
	}
	if !byExit.speed || !byExit.online || !byExit.counted || byExit.hardExcluded {
		t.Fatalf("judged by its exit, the provider left Speed or Online: %+v", byExit)
	}
	if byExit.reason != ProviderExcludedArinNonQuality {
		t.Fatalf("judged by its exit, the reason is %q, want %q", byExit.reason, ProviderExcludedArinNonQuality)
	}
}
