// This regression can accompany the minimal frozen-source instrument overlay:
// it depends on no newer evaluation, source-activation or API model fields.
package model

import (
	"encoding/json"
	"testing"
)

// A standalone optional contract proves omission keeps exact scorer-one bytes
// while an explicit supported policy names scorer two and rejects ambiguity.
func TestCompetitionG2StandaloneLegacyContractCodec(t *testing.T) {
	type contract struct {
		ScorerVersion  string                    `json:"scorer_version"`
		G2VolumePolicy CompetitionG2VolumePolicy `json:"g2_volume_policy,omitempty"`
	}
	legacyBytes, err := json.Marshal(contract{ScorerVersion: CompetitionScorerVersion})
	if err != nil || string(legacyBytes) != `{"scorer_version":"sim-latency-score/1"}` {
		t.Fatal("standalone instrument changed historical contract bytes", string(legacyBytes), err)
	}
	var legacy contract
	if err := json.Unmarshal(legacyBytes, &legacy); err != nil {
		t.Fatal(err)
	}
	version, err := CompetitionScorerVersionForG2Policy(legacy.G2VolumePolicy)
	if err != nil || version != legacy.ScorerVersion {
		t.Fatal("legacy omission acquired a new meaning", err)
	}
	minimumWorkBytes, err := json.Marshal(contract{ScorerVersion: CompetitionMinimumWorkScorerVersion, G2VolumePolicy: CompetitionG2VolumePolicyMinimumWork})
	if err != nil || string(minimumWorkBytes) != `{"scorer_version":"sim-latency-score/2","g2_volume_policy":"minimum-work/1"}` {
		t.Fatal("standalone instrument omitted explicit future provenance", string(minimumWorkBytes), err)
	}
	for _, invalid := range []string{`null`, `""`, `"unknown/1"`, `false`, `1`} {
		var decoded contract
		if json.Unmarshal([]byte(`{"scorer_version":"sim-latency-score/2","g2_volume_policy":`+invalid+`}`), &decoded) == nil {
			t.Errorf("ambiguous standalone policy %s decoded", invalid)
		}
	}
}
