package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestArinShadowFreshnessUsesSourceLookup(t *testing.T) {
	r, facts := shadowFixture(t, false)
	now := time.Now().UTC()
	r.start = now.Add(-80 * time.Second)
	r.clock = func() time.Time { return now }
	facts.At = now.Add(-79 * time.Second)
	if !r.Observe("a", "192.0.2.1", facts) {
		t.Fatal("fresh source lookup refused")
	}
	r.clock = func() time.Time { return now.Add(20 * time.Second) }
	report, err := shadowTestSnapshot(r, []ArinShadowProvider{{Token: "p", Connections: []string{"a"}, Buckets: []string{"us"}, BaseQuality: true}}, []string{"us"}, true, 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if report.ObservationComplete || report.Buckets[0].CompleteLookups != 0 || report.Buckets[0].MissingOrStale != 1 || report.Buckets[0].CandidateQuality != 0 {
		t.Fatal("receipt time rejuvenated an expired source lookup")
	}
}

func TestArinShadowMissingObservationIsNotKnownPolicyLoss(t *testing.T) {
	r, facts := shadowFixture(t, false)
	if !r.Observe("a", "192.0.2.1", facts) {
		t.Fatal("lookup failed")
	}
	report, err := shadowTestSnapshot(r, []ArinShadowProvider{{Token: "p", Connections: []string{"a", "missing"}, Buckets: []string{"us"}, BaseQuality: true, BaseSpeed: true, ActiveQuality: true, ActiveSpeed: true}}, []string{"us"}, true, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if report.ObservationComplete || report.Buckets[0].QualityRemoved != 0 || report.Buckets[0].QualityIndeterminate != 1 {
		t.Fatal("missing observation counted as a confirmed policy exclusion")
	}
}

func TestArinShadowCurrentCensusMustMatchLookupFacts(t *testing.T) {
	r, facts := shadowFixture(t, false)
	if !r.Observe("a", "192.0.2.1", facts) {
		t.Fatal("lookup failed")
	}
	provider := ArinShadowProvider{Token: "p", Connections: []string{"a"}, Buckets: []string{"us"}, BaseQuality: true, BaseSpeed: true, ActiveQuality: true}
	for _, mutate := range []func(*ArinShadowActiveFacts){
		func(f *ArinShadowActiveFacts) { f.At = f.At.Add(time.Microsecond) },
		func(f *ArinShadowActiveFacts) { f.Epoch++ },
		func(f *ArinShadowActiveFacts) { f.Risk = !f.Risk },
		func(f *ArinShadowActiveFacts) { f.NonQuality = !f.NonQuality },
		func(f *ArinShadowActiveFacts) { f.Verified = !f.Verified },
	} {
		current := facts
		mutate(&current)
		provider.Lookups = map[string]ArinShadowActiveFacts{"a": current}
		report, err := r.Snapshot([]ArinShadowProvider{provider}, []string{"us"}, true, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		row := report.Buckets[0]
		if report.ObservationComplete || row.CandidateQuality != 0 || row.QualityRemoved != 0 || row.QualityIndeterminate != 1 || row.MissingOrStale != 1 {
			t.Fatal("a reused connection identity joined different durable lookup facts")
		}
	}
	provider.Lookups = nil
	report, err := r.Snapshot([]ArinShadowProvider{provider}, []string{"us"}, true, time.Minute)
	if err != nil || report.ObservationComplete {
		t.Fatal("absent source lookup join qualified")
	}
	provider.Lookups = map[string]ArinShadowActiveFacts{"a": facts}
	report, err = r.Snapshot([]ArinShadowProvider{provider}, []string{"us"}, true, time.Minute)
	if err != nil || !report.ObservationComplete || report.Buckets[0].CandidateQuality != 1 || report.Buckets[0].QualityIndeterminate != 0 {
		t.Fatal("exact current source join did not qualify")
	}
}

func TestArinShadowSourceBooleanPresenceAndPrivateLabels(t *testing.T) {
	r, facts := shadowFixture(t, false)
	encoded, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"Epoch", "At", "Risk", "NonQuality", "Verified"} {
		var value map[string]any
		if err := json.Unmarshal(encoded, &value); err != nil {
			t.Fatal(err)
		}
		delete(value, field)
		incomplete, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ArinShadowActiveFacts
		if json.Unmarshal(incomplete, &decoded) == nil {
			t.Fatal("missing source fact became a measured zero")
		}
	}
	if !r.Observe("a", "192.0.2.1", facts) {
		t.Fatal("lookup failed")
	}
	for _, label := range []string{"private-provider", "00000000-0000-0000-0000-000000000001", "192.0.2.1", "operator:private"} {
		if _, err := r.Snapshot(nil, []string{label}, true, time.Minute); err == nil {
			t.Fatal("private or unbounded label accepted")
		}
	}
	r.capacity = 1
	if _, err := r.Snapshot([]ArinShadowProvider{{Token: "p", Connections: []string{"a", "b"}, Buckets: []string{"us"}}}, []string{"us"}, true, time.Minute); err == nil {
		t.Fatal("census connection capacity unbounded")
	}
}

func TestArinShadowKnownLossAndProxyRiskConsistency(t *testing.T) {
	active := mmdbtype.Map{"classifier_version": mmdbtype.Uint32(1), "risk": mmdbtype.Bool(false), "non_quality": mmdbtype.Bool(false)}
	candidate := mmdbtype.Map{"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2), "quality_state": mmdbtype.String("unknown"), "non_quality": mmdbtype.Bool(true), "risk": mmdbtype.Bool(false)}
	for _, inconsistent := range []bool{false, true} {
		if inconsistent {
			candidate["network_risk_evidence"] = mmdbtype.Slice{mmdbtype.Map{"category": mmdbtype.String("virtual_isp")}}
		}
		paths, pins := [2]string{}, [2]string{}
		dir := t.TempDir()
		for i, record := range []mmdbtype.Map{active, candidate} {
			data := testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{"192.0.2.0/24": record})
			paths[i] = filepath.Join(dir, fmt.Sprintf("%d.mmdb", i))
			if err := os.WriteFile(paths[i], data, 0600); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			pins[i] = hex.EncodeToString(sum[:])
		}
		now := NowUtc()
		recorder, err := OpenArinShadowRecorder(paths[0], pins[0], paths[1], pins[1], now.Add(-time.Second), 4)
		if err != nil {
			t.Fatal(err)
		}
		facts := ArinShadowActiveFacts{At: now, Epoch: recorder.active.Metadata.BuildTime().Unix()}
		observed := recorder.Observe("a", "192.0.2.1", facts)
		if observed == inconsistent {
			t.Fatal("contradictory proxy-risk record accepted or valid unknown rejected")
		}
		report, err := recorder.Snapshot([]ArinShadowProvider{{Token: "p", Connections: []string{"a"}, Lookups: map[string]ArinShadowActiveFacts{"a": facts}, Buckets: []string{"us"}, BaseQuality: true, BaseSpeed: true, ActiveQuality: true, ActiveSpeed: true}}, []string{"us"}, true, time.Minute)
		recorder.Close()
		if err != nil {
			t.Fatal(err)
		}
		row := report.Buckets[0]
		if inconsistent {
			if report.ObservationComplete || row.QualityRemoved != 0 || row.QualityIndeterminate != 1 {
				t.Fatal("invalid candidate evidence became a known policy loss")
			}
		} else if !report.ObservationComplete || row.QualityRemoved != 1 || row.Unknown != 1 || row.CandidateQuality != 0 || row.CandidateSpeed != 1 || row.QualityIndeterminate != 0 {
			t.Fatal("confirmed unknown-use exclusion or independent Speed control incorrect")
		}
	}
}
