// The pair a process opened is observable in pushed metrics, so fleet adoption
// of a selected release and its old copy is measurable without host access.
package server

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	arinDatabaseBuildEpochMetricName    = "urnetwork_arin_database_build_epoch_seconds"
	arinOldDatabaseBuildEpochMetricName = "urnetwork_arin_old_database_build_epoch_seconds"
	arinDatabaseInfoMetricName          = "urnetwork_arin_database_info"
)

// The gathered value of an unlabeled gauge in the pushed registry. Absence is
// reported separately from a zero value.
func gatheredArinGauge(t *testing.T, name string) (float64, bool) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		metrics := family.GetMetric()
		if len(metrics) != 1 || metrics[0].GetGauge() == nil {
			t.Fatalf("%s has %d samples, want one unlabeled gauge", name, len(metrics))
		}
		return metrics[0].GetGauge().GetValue(), true
	}
	return 0, false
}

// The info samples by slot: their state and hash labels, each valued 1.
func gatheredArinDatabaseInfo(t *testing.T) map[string][2]string {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	slots := map[string][2]string{}
	for _, family := range families {
		if family.GetName() != arinDatabaseInfoMetricName {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if metric.GetGauge().GetValue() != 1 {
				t.Fatalf("%s slot %s = %v, want 1", arinDatabaseInfoMetricName, labels["slot"], metric.GetGauge().GetValue())
			}
			if _, repeated := slots[labels["slot"]]; repeated {
				t.Fatalf("%s repeats slot %s", arinDatabaseInfoMetricName, labels["slot"])
			}
			slots[labels["slot"]] = [2]string{labels["state"], labels["sha256"]}
		}
	}
	return slots
}

func publishTestArinPair(t *testing.T, pair *arinDatabasePair) {
	t.Helper()
	previous := arinServingPair.Load()
	t.Cleanup(func() { arinServingPair.Store(previous) })
	arinServingPair.Store(pair)
}

// Before the pair opens every gauge is present: both epochs zero and both
// slots unopened, so a process that never classifies an address is
// distinguishable from a missing push.
func TestArinDatabaseGaugesAreZeroAndUnopenedBeforeThePairOpens(t *testing.T) {
	publishTestArinPair(t, nil)
	for _, name := range []string{arinDatabaseBuildEpochMetricName, arinOldDatabaseBuildEpochMetricName} {
		value, ok := gatheredArinGauge(t, name)
		if !ok {
			t.Fatalf("%s is not registered in the pushed registry", name)
		}
		if value != 0 {
			t.Fatalf("unopened pair reports %s %v, want 0", name, value)
		}
	}
	info := gatheredArinDatabaseInfo(t)
	if len(info) != 2 || info["latest"] != [2]string{ArinDatabaseUnopened, "none"} || info["old"] != [2]string{ArinDatabaseUnopened, "none"} {
		t.Fatalf("unopened pair info = %v", info)
	}
}

// Both slots carry their state and hash in every old-slot state, and the old
// epoch is reported only while the old slot is loaded.
func TestArinDatabaseGaugesCarryBothSlotsInEveryState(t *testing.T) {
	latestHash := "aa00000000000000000000000000000000000000000000000000000000000000"
	oldHash := "bb00000000000000000000000000000000000000000000000000000000000000"
	latest := ArinDatabaseIdentity{State: ArinDatabaseLoaded, BuildEpoch: testArinLatestEpoch, Sha256: latestHash}
	for _, c := range []struct {
		old       ArinDatabaseIdentity
		oldEpoch  float64
		oldLabels [2]string
	}{
		{old: ArinDatabaseIdentity{State: ArinDatabaseLoaded, BuildEpoch: testArinOldEpoch, Sha256: oldHash}, oldEpoch: float64(testArinOldEpoch), oldLabels: [2]string{ArinDatabaseLoaded, oldHash}},
		{old: ArinDatabaseIdentity{State: ArinDatabaseMissing}, oldLabels: [2]string{ArinDatabaseMissing, "none"}},
		{old: ArinDatabaseIdentity{State: ArinDatabaseIdentical, BuildEpoch: testArinLatestEpoch, Sha256: latestHash}, oldLabels: [2]string{ArinDatabaseIdentical, latestHash}},
		{old: ArinDatabaseIdentity{State: ArinDatabaseInvalid, BuildEpoch: testArinOldEpoch, Sha256: oldHash}, oldLabels: [2]string{ArinDatabaseInvalid, oldHash}},
		{old: ArinDatabaseIdentity{State: ArinDatabaseInvalid}, oldLabels: [2]string{ArinDatabaseInvalid, "none"}},
	} {
		publishTestArinPair(t, &arinDatabasePair{latestIdentity: latest, oldIdentity: c.old})
		if value, _ := gatheredArinGauge(t, arinDatabaseBuildEpochMetricName); value != float64(testArinLatestEpoch) {
			t.Errorf("old %s: latest epoch %v, want %d", c.old.State, value, testArinLatestEpoch)
		}
		if value, _ := gatheredArinGauge(t, arinOldDatabaseBuildEpochMetricName); value != c.oldEpoch {
			t.Errorf("old %s: old epoch %v, want %v", c.old.State, value, c.oldEpoch)
		}
		info := gatheredArinDatabaseInfo(t)
		if len(info) != 2 || info["latest"] != [2]string{ArinDatabaseLoaded, latestHash} || info["old"] != c.oldLabels {
			t.Errorf("old %s: info = %v, want latest %v and old %v", c.old.State, info, [2]string{ArinDatabaseLoaded, latestHash}, c.oldLabels)
		}
	}
}

// A latest that failed to open reports no epoch and its state, never the
// identity of a reader it does not hold.
func TestArinDatabaseGaugesReportAFailedLatest(t *testing.T) {
	publishTestArinPair(t, &arinDatabasePair{
		latestIdentity: ArinDatabaseIdentity{State: ArinDatabaseInvalid, BuildEpoch: testArinLatestEpoch},
		oldIdentity:    ArinDatabaseIdentity{State: ArinDatabaseUnopened},
	})
	if value, _ := gatheredArinGauge(t, arinDatabaseBuildEpochMetricName); value != 0 {
		t.Fatalf("failed latest reports epoch %v", value)
	}
	if info := gatheredArinDatabaseInfo(t); info["latest"] != [2]string{ArinDatabaseInvalid, "none"} || info["old"] != [2]string{ArinDatabaseUnopened, "none"} {
		t.Fatalf("failed latest info = %v", info)
	}
}
