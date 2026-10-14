package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// ProviderEgressCountsPolicyKey separates shared dashboard snapshots when
// workers resolve different count rules. This is not a data-generation key:
// observed providers and accepted health results retain the snapshot's age.
// The ARIN serving epoch the counts follow (ArindbServingEpoch) is part of the
// rules, so a canary switch starts a fresh snapshot instead of serving the
// previous choice's. The caller reads it; the key itself does no I/O.
func ProviderEgressCountsPolicyKey(arinServingEpoch int64) string {
	return providerEgressCountsPolicyKey(
		egressIndexSettings(), providerEgressTestEnabled(),
		providerReliabilityMinimums(), SelectedProviderUrlProbePolicyVersion(),
		arinServingEpoch,
	)
}

func providerEgressCountsPolicyKey(
	settings *EgressIndexSettings,
	enabled bool,
	minimums map[int]float64,
	probePolicyVersion int,
	arinServingEpoch int64,
) string {
	// Increment the schema when count semantics change. Request-only cache and
	// routing settings do not change this aggregate and are intentionally absent.
	// Schema 2 adds the URL-success rule and its no_success reason, which a
	// schema 1 snapshot neither applies nor carries. The floor's margin
	// changes which countries the dashboard counts exempt, so it is part of
	// the policy too. Schema 3 adds the modern provider gate, its not_modern
	// reason and its shadow counts; its switch moves providers between the
	// buckets and that reason, and its evidence window is code. Schema 4
	// selects Quality by the published ARIN serving epoch, which a schema 3
	// snapshot did not.
	policy := struct {
		Schema                             int
		MaxIndex                           int
		EvidenceMaxAge                     time.Duration
		QualityNumerator                   int
		QualityDenominator                 int
		CountryGate                        bool
		EgressEnabled                      bool
		ReliabilityMinimum                 [3]float64
		ProbePolicyVersion                 int
		OnlineRequiresUrlSuccess           bool
		OnlineSuccessCountryFloor          float64
		OnlineSuccessCountryFloorMargin    float64
		QualitySpeedRequiresModernProvider bool
		ArinServingEpoch                   int64
	}{
		Schema: 4, MaxIndex: settings.MaxIndex(),
		EvidenceMaxAge:   min(settings.EvidenceMaxAge, ProviderEgressHealthMaxAge),
		QualityNumerator: settings.QualityOkNumerator, QualityDenominator: settings.QualityOkDenominator,
		CountryGate: settings.CountryGate, EgressEnabled: enabled,
		ReliabilityMinimum:                 [3]float64{minimums[1], minimums[2], minimums[3]},
		ProbePolicyVersion:                 probePolicyVersion,
		OnlineRequiresUrlSuccess:           settings.OnlineRequiresUrlSuccess,
		OnlineSuccessCountryFloor:          settings.OnlineSuccessCountryFloor,
		OnlineSuccessCountryFloorMargin:    settings.OnlineSuccessCountryFloorMargin,
		QualitySpeedRequiresModernProvider: settings.QualitySpeedRequiresModernProvider,
		ArinServingEpoch:                   arinServingEpoch,
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
