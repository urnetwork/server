package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/urnetwork/server"
)

// ProviderEgressCountsPolicyKey separates shared dashboard snapshots when
// workers resolve different count rules. This is not a data-generation key:
// observed providers and accepted health results retain the snapshot's age.
// It resolves the rules at the moment of the call, so the key changes at the
// modern provider gate's start; the dashboard cache checks it before and
// after each fill, so a fill that spans the start is never published. The ARIN
// serving epoch the counts follow (ArindbServingEpoch) is part of the rules,
// so a canary switch starts a fresh snapshot instead of serving the previous
// choice's. The caller reads it; the key itself does no I/O.
func ProviderEgressCountsPolicyKey(arinServingEpoch int64) string {
	return providerEgressCountsPolicyKey(
		egressIndexSettings(), providerEgressTestEnabled(),
		providerReliabilityMinimums(), SelectedProviderUrlProbePolicyVersion(),
		server.NowUtc(), arinServingEpoch,
	)
}

func providerEgressCountsPolicyKey(
	settings *EgressIndexSettings,
	enabled bool,
	minimums map[int]float64,
	probePolicyVersion int,
	now time.Time,
	arinServingEpoch int64,
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
	// before the start is read after it. 4 = the URL ratio, 5 = the ARIN
	// serving epoch.
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
		// Schema 5 selects Quality by the published ARIN serving epoch, which
		// a schema 4 snapshot did not.
		ArinServingEpoch int64
	}{
		Schema: 5, MaxIndex: settings.MaxIndex(),
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
		ArinServingEpoch:                   arinServingEpoch,
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		panic(err)
	}
	// The native URL-run minimums (egress_native_min_runs.go) move a provider
	// whose passing ratio rests on too few runs out of Quality and Speed to
	// unprobed, which a snapshot of any schema above counted in both. The key
	// covers them beside the policy rather than in a new schema, so the change
	// applies alike to every line of workers that carries one schema.
	nativeMinRuns, err := json.Marshal(providerNativeMinRuns)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(append(encoded, nativeMinRuns...))
	return hex.EncodeToString(sum[:])
}
