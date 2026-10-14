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
func ProviderEgressCountsPolicyKey() string {
	return providerEgressCountsPolicyKey(
		egressIndexSettings(), providerEgressTestEnabled(),
		providerReliabilityMinimums(), SelectedProviderUrlProbePolicyVersion(),
	)
}

func providerEgressCountsPolicyKey(
	settings *EgressIndexSettings,
	enabled bool,
	minimums map[int]float64,
	probePolicyVersion int,
) string {
	// Increment the schema when count semantics change. Request-only cache and
	// routing settings do not change this aggregate and are intentionally absent.
	// Schema 2 adds the URL-success rule and its no_success reason, which a
	// schema 1 snapshot neither applies nor carries. The floor's margin
	// changes which countries the dashboard counts exempt, so it is part of
	// the policy too.
	policy := struct {
		Schema                          int
		MaxIndex                        int
		EvidenceMaxAge                  time.Duration
		QualityNumerator                int
		QualityDenominator              int
		CountryGate                     bool
		EgressEnabled                   bool
		ReliabilityMinimum              [3]float64
		ProbePolicyVersion              int
		OnlineRequiresUrlSuccess        bool
		OnlineSuccessCountryFloor       float64
		OnlineSuccessCountryFloorMargin float64
	}{
		Schema: 2, MaxIndex: settings.MaxIndex(),
		EvidenceMaxAge:   min(settings.EvidenceMaxAge, ProviderEgressHealthMaxAge),
		QualityNumerator: settings.QualityOkNumerator, QualityDenominator: settings.QualityOkDenominator,
		CountryGate: settings.CountryGate, EgressEnabled: enabled,
		ReliabilityMinimum:              [3]float64{minimums[1], minimums[2], minimums[3]},
		ProbePolicyVersion:              probePolicyVersion,
		OnlineRequiresUrlSuccess:        settings.OnlineRequiresUrlSuccess,
		OnlineSuccessCountryFloor:       settings.OnlineSuccessCountryFloor,
		OnlineSuccessCountryFloorMargin: settings.OnlineSuccessCountryFloorMargin,
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
