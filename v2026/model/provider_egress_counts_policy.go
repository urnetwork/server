package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/urnetwork/server/v2026"
)

// ProviderEgressCountsPolicyKey separates shared dashboard snapshots when
// workers resolve different count rules. This is not a data-generation key:
// observed providers and accepted health results retain the snapshot's age.
// It resolves the rules at the moment of the call, so the key changes at the
// modern provider gate's start; the dashboard cache checks it before and
// after each fill, so a fill that spans the start is never published.
func ProviderEgressCountsPolicyKey() string {
	return providerEgressCountsPolicyKey(
		egressIndexSettings(), providerEgressTestEnabled(),
		providerReliabilityMinimums(), SelectedProviderUrlProbePolicyVersion(),
		server.NowUtc(),
	)
}

func providerEgressCountsPolicyKey(
	settings *EgressIndexSettings,
	enabled bool,
	minimums map[int]float64,
	probePolicyVersion int,
	now time.Time,
) string {
	// Increment the schema when count semantics change. Request-only cache and
	// routing settings do not change this aggregate and are intentionally absent.
	// Schema 2 adds the URL-success rule and its no_success reason, which a
	// schema 1 snapshot neither applies nor carries. The floor's margin
	// changes which countries the dashboard counts exempt, so it is part of
	// the policy too. Schema 3 adds the modern provider gate, its not_modern
	// reason and its shadow counts; whether the gate is on at now moves
	// providers between the buckets and that reason, and its evidence window
	// is code. The gate is keyed by that decision, not by its switch and
	// start, so workers that decide alike share a snapshot and none taken
	// before the start is read after it.
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
		// Schema 4 adds the Online URL ratio and its low_success reason: the
		// ratio and its country exemption move providers out of Online and
		// the counts, and a schema 3 snapshot carries no such reason.
		OnlineUrlSuccessRatio          float64
		OnlineUrlRatioCountryExemption bool
	}{
		Schema: 4, MaxIndex: settings.MaxIndex(),
		EvidenceMaxAge:   min(settings.EvidenceMaxAge, ProviderEgressHealthMaxAge),
		QualityNumerator: settings.QualityOkNumerator, QualityDenominator: settings.QualityOkDenominator,
		CountryGate: settings.CountryGate, EgressEnabled: enabled,
		ReliabilityMinimum:                 [3]float64{minimums[1], minimums[2], minimums[3]},
		ProbePolicyVersion:                 probePolicyVersion,
		OnlineUrlSuccessRatio:              settings.OnlineUrlSuccessRatio,
		OnlineUrlRatioCountryExemption:     settings.OnlineUrlRatioCountryExemption,
		OnlineRequiresUrlSuccess:           settings.OnlineRequiresUrlSuccess,
		OnlineSuccessCountryFloor:          settings.OnlineSuccessCountryFloor,
		OnlineSuccessCountryFloorMargin:    settings.OnlineSuccessCountryFloorMargin,
		QualitySpeedRequiresModernProvider: settings.RequiresModernProvider(now),
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
