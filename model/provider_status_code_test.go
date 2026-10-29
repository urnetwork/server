package model

// The reason codes of a provider status without a database: the one table
// (providerStatusOutcomes) tells every internal cause, the status's own rules
// and every exclusion reason of the selection, as a reason and a code; the
// anti-abuse signals are one outcome, withheld; and a cause the table does
// not enter is told as withheld, never by its own name.

import (
	"slices"
	"testing"
)

// What a provider is told of every internal cause, entered twice on purpose:
// a cause added to the status or to ProviderExcludedReasons fails this test
// until it is classified here and in the table alike. Every cause not listed
// as told is an anti-abuse signal, withheld.
var testingProviderStatusToldCauses = map[string]providerStatusOutcome{
	// the status's own rules
	"not_providing":          {ProviderStatusReasonNotProviding, "not_providing"},
	"not_connected":          {ProviderStatusReasonNotConnected, "not_connected"},
	"location_invalid":       {ProviderStatusReasonLocationInvalid, "location_invalid"},
	"network_only":           {ProviderStatusReasonNetworkOnly, "network_only"},
	"reliability_warming_up": {ProviderStatusReasonReliabilityWarmingUp, "warming_up"},
	"egress_warming_up":      {ProviderStatusReasonEgressWarmingUp, "warming_up"},
	"modern_priority":        {ProviderStatusReasonNotModern, "not_modern"},
	"speed_test_missing":     {ProviderStatusReasonSpeedTestMissing, "warming_up"},
	"slow":                   {ProviderStatusReasonSlow, "slow"},
	"none":                   {ProviderStatusReasonNone, "none"},
	// the selection's exclusion reasons a provider can act on
	ProviderExcludedReliability: {ProviderStatusReasonReliabilityLow, "reliability_low"},
	ProviderExcludedNoSuccess:   {ProviderStatusReasonEgressNoSuccess, "egress_failing"},
	ProviderExcludedLowSuccess:  {ProviderStatusReasonEgressLowSuccess, "egress_failing"},
	ProviderExcludedUnprobed:    {ProviderStatusReasonEgressUnprobed, "warming_up"},
	ProviderExcludedHealth:      {ProviderStatusReasonEgressFailing, "egress_failing"},
	ProviderExcludedNotModern:   {ProviderStatusReasonNotModern, "not_modern"},
}

// The anti-abuse signals among the selection's exclusion reasons: ARIN risk,
// the URL security quarantine, ARIN non-quality, and the legacy dark verdict
// and country labels.
var testingProviderStatusWithheldCauses = []string{
	ProviderExcludedArinRisk,
	ProviderExcludedTls,
	ProviderExcludedArinNonQuality,
	ProviderExcludedBlackhole,
	ProviderExcludedCountry,
}

// The table enters every internal cause, the status's own rules and every
// exclusion reason of the selection, and nothing else; each is told exactly
// as classified above, and every anti-abuse signal is told as the one
// outcome not_eligible and withheld.
func TestProviderStatusOutcomesCoverEveryCause(t *testing.T) {
	causes := []string{}
	for _, cause := range providerStatusRuleCauses {
		if slices.Contains(causes, cause) {
			t.Fatalf("the rule cause %s is listed twice", cause)
		}
		causes = append(causes, cause)
	}
	for _, reason := range ProviderExcludedReasons {
		// the two lists name different things, so one key cannot stand for
		// both
		if slices.Contains(causes, reason) {
			t.Fatalf("the exclusion reason %s is also a cause of the status's own", reason)
		}
		causes = append(causes, reason)
	}

	for _, cause := range causes {
		outcome, ok := providerStatusOutcomes[cause]
		if !ok {
			t.Errorf("the cause %s has no outcome: a provider would be told withheld", cause)
			continue
		}
		told, isTold := testingProviderStatusToldCauses[cause]
		withheld := slices.Contains(testingProviderStatusWithheldCauses, cause)
		switch {
		case isTold == withheld:
			t.Errorf("the cause %s is classified neither or twice", cause)
		case isTold && outcome != told:
			t.Errorf("the cause %s is told as %+v, want %+v", cause, outcome, told)
		case withheld && outcome != providerStatusWithheld:
			t.Errorf("the anti-abuse signal %s is told as %+v, want %+v", cause, outcome, providerStatusWithheld)
		}
	}
	for cause := range providerStatusOutcomes {
		if !slices.Contains(causes, cause) {
			t.Errorf("the table enters %s, which is no cause", cause)
		}
	}
	if providerStatusWithheld.reason != ProviderStatusReasonNotEligible || providerStatusWithheld.code != "withheld" {
		t.Fatalf("withheld is told as %+v", providerStatusWithheld)
	}
}

// The code is a function of the reason, so it tells nothing the reason does
// not; withheld codes not_eligible and nothing else; every reason has a code;
// and the codes are listed in the order of the reasons they first code.
func TestProviderStatusCodesFollowTheReasons(t *testing.T) {
	reasonCodes := map[string]string{}
	for cause, outcome := range providerStatusOutcomes {
		if !slices.Contains(ProviderStatusReasons, outcome.reason) {
			t.Errorf("%s: the reason %s is not listed", cause, outcome.reason)
		}
		if !slices.Contains(ProviderStatusCodes, outcome.code) {
			t.Errorf("%s: the code %s is not listed", cause, outcome.code)
		}
		if code, ok := reasonCodes[outcome.reason]; ok && code != outcome.code {
			t.Errorf("the reason %s has the codes %s and %s", outcome.reason, code, outcome.code)
		}
		reasonCodes[outcome.reason] = outcome.code
		if (outcome.code == ProviderStatusCodeWithheld) != (outcome.reason == ProviderStatusReasonNotEligible) {
			t.Errorf("%s: the reason %s has the code %s", cause, outcome.reason, outcome.code)
		}
	}

	codes := []string{}
	for _, reason := range ProviderStatusReasons {
		code, ok := reasonCodes[reason]
		if !ok {
			t.Errorf("no cause is told as the reason %s", reason)
			continue
		}
		if !slices.Contains(codes, code) {
			codes = append(codes, code)
		}
	}
	if !slices.Equal(codes, ProviderStatusCodes) {
		t.Fatalf("the codes in the reasons' order are %v, listed %v", codes, ProviderStatusCodes)
	}
}

// A cause the table does not enter, as a new exclusion reason would be before
// it is classified, or the hard failure without a reason, is told as
// withheld: never by its own name.
func TestProviderStatusUnknownCauseIsWithheld(t *testing.T) {
	for _, cause := range []string{"", "fraud_score", "arin_risk_v2", "blackhole_v2"} {
		if outcome := providerStatusOutcomeOf(cause); outcome != providerStatusWithheld {
			t.Fatalf("the unknown cause %q is told as %+v", cause, outcome)
		}
	}
	for cause, outcome := range providerStatusOutcomes {
		if providerStatusOutcomeOf(cause) != outcome {
			t.Fatalf("the cause %s is not told as entered", cause)
		}
	}
}
