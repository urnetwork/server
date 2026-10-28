package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The compact form of a provider status (GET /network/provider-status
// ?format=compact) without a database: the statuses the model decides, without
// their English texts, with every fact kept and the anti-abuse signals still
// one generic reason.

func testingProviderStatusJson(t *testing.T, value any) string {
	t.Helper()
	valueJson, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(valueJson)
}

// Every fact of a decided status, its texts aside, reads the same in the
// compact form: a number gives a value, minimum or maximum exactly when it
// has one, and a count and total exactly when it is counted with a value.
func TestProviderStatusCompactKeepsEveryFactButTheTexts(t *testing.T) {
	passing := testingProviderStatusFacts()
	passingFilter := testingProviderStatusFilter(passing)
	testingProviderStatusModern(passing, &passingFilter)

	withoutEvidence := testingProviderStatusFacts()
	withoutEvidence.lookbacks = providerStatusNeutralLookbacks()
	withoutEvidence.hasReliabilityHistory = false
	withoutEvidenceFilter := testingProviderStatusFilter(withoutEvidence)
	delete(withoutEvidenceFilter.healthCounts, withoutEvidence.clientId)

	notProviding := testingProviderStatusFacts()
	notProviding.providePublic = false
	notProviding.provideNetwork = false
	notProviding.publishedCountryCode = nil

	for name, testCase := range map[string]struct {
		facts       *providerStatusFacts
		countFilter providerCountFilter
	}{
		"passing":          {passing, passingFilter},
		"without_evidence": {withoutEvidence, withoutEvidenceFilter},
		"not_providing":    {notProviding, testingProviderStatusFilter(notProviding)},
	} {
		_, status := testingProviderStatus(testCase.facts, testCase.countFilter)
		status.Appearances = &ProviderAppearanceHistogram{StartMinute: 1, BucketSeconds: 60, AppearancesPerMinute: []int64{0, 2}}
		fullJson := testingProviderStatusJson(t, status)
		compact := NewProviderStatusCompact(status)
		compactJson := testingProviderStatusJson(t, compact)
		t.Logf("%s: full %d bytes, compact %d bytes", name, len(fullJson), len(compactJson))

		for _, key := range []string{`"reason_text"`, `"explanation"`, `"has_value"`, `"has_minimum"`, `"has_maximum"`} {
			if strings.Contains(compactJson, key) {
				t.Fatalf("%s: the compact form carries %s: %s", name, key, compactJson)
			}
		}
		if fullJson != testingProviderStatusJson(t, status) {
			t.Fatalf("%s: compacting changed the status", name)
		}
		if compact.ClientId != status.ClientId || compact.Reason != status.Reason ||
			testingProviderStatusJson(t, compact.Admission) != testingProviderStatusJson(t, status.Admission) ||
			!compact.EvaluateTime.Equal(status.EvaluateTime) ||
			testingProviderStatusJson(t, compact.Appearances) != testingProviderStatusJson(t, status.Appearances) {
			t.Fatalf("%s: compact %s, full %s", name, compactJson, fullJson)
		}

		if len(compact.Ranking) != len(status.Ranking) {
			t.Fatalf("%s: %d compact numbers, %d full", name, len(compact.Ranking), len(status.Ranking))
		}
		for i, number := range status.Ranking {
			compactNumber := compact.Ranking[i]
			if compactNumber.Name != number.Name || compactNumber.Passes != number.Passes {
				t.Fatalf("%s: number %d %+v, full %+v", name, i, compactNumber, number)
			}
			if (compactNumber.Value != nil) != number.HasValue || (number.HasValue && *compactNumber.Value != number.Value) {
				t.Fatalf("%s: %s value %v, full %+v", name, number.Name, compactNumber.Value, number)
			}
			if (compactNumber.Minimum != nil) != number.HasMinimum || (number.HasMinimum && *compactNumber.Minimum != number.Minimum) {
				t.Fatalf("%s: %s minimum %v, full %+v", name, number.Name, compactNumber.Minimum, number)
			}
			if (compactNumber.Maximum != nil) != number.HasMaximum || (number.HasMaximum && *compactNumber.Maximum != number.Maximum) {
				t.Fatalf("%s: %s maximum %v, full %+v", name, number.Name, compactNumber.Maximum, number)
			}
			counted := number.Name == ProviderStatusNumberUrlChecks && number.HasValue
			if (compactNumber.Count != nil) != counted || (compactNumber.Total != nil) != counted ||
				(counted && (*compactNumber.Count != number.Count || *compactNumber.Total != number.Total)) {
				t.Fatalf("%s: %s count %v total %v, full %+v", name, number.Name, compactNumber.Count, compactNumber.Total, number)
			}
		}

		if (compact.Country == nil) != (status.Country == nil) {
			t.Fatalf("%s: compact country %+v, full %+v", name, compact.Country, status.Country)
		}
		if status.Country != nil && (compact.Country.CountryCode != status.Country.CountryCode ||
			compact.Country.Country != status.Country.Country ||
			compact.Country.ObservedCountryCode != status.Country.ObservedCountryCode) {
			t.Fatalf("%s: compact country %+v, full %+v", name, compact.Country, status.Country)
		}
		if 2*len(compactJson) >= len(fullJson) {
			t.Fatalf("%s: the compact form is %d bytes, the full %d", name, len(compactJson), len(fullJson))
		}
	}
}

// The anti-abuse signals read the same in the compact form as in the full:
// one generic not_eligible, byte for byte alike whichever signal applies.
func TestProviderStatusCompactAntiAbuseSignalsAreIndistinguishable(t *testing.T) {
	compactJson := func(modify func(*providerStatusFacts, *providerCountFilter)) string {
		facts := testingProviderStatusFacts()
		facts.clientId = server.Id{1}
		countFilter := testingProviderStatusFilter(facts)
		modify(facts, &countFilter)
		_, status := testingProviderStatus(facts, countFilter)
		return testingProviderStatusJson(t, NewProviderStatusCompact(status))
	}

	var firstJson string
	for name, signal := range map[string]func(*providerStatusFacts, *providerCountFilter){
		"arin_risk": func(f *providerStatusFacts, c *providerCountFilter) {
			c.arinRisk[f.clientId] = true
		},
		"arin_non_quality": func(f *providerStatusFacts, c *providerCountFilter) {
			c.arinNonQuality[f.clientId] = true
		},
		"tls": func(f *providerStatusFacts, c *providerCountFilter) {
			c.tlsAuthenticationFailed[f.clientId] = true
		},
		"dark_and_risk": func(f *providerStatusFacts, c *providerCountFilter) {
			c.arinRisk[f.clientId] = true
			c.blackholed = map[server.Id]bool{f.clientId: true}
		},
	} {
		signalJson := compactJson(signal)
		if !strings.Contains(signalJson, `"reason":"not_eligible"`) {
			t.Fatalf("%s: %s", name, signalJson)
		}
		if firstJson == "" {
			firstJson = signalJson
		} else if signalJson != firstJson {
			t.Fatalf("%s reads differently from another signal:\n%s\n%s", name, signalJson, firstJson)
		}
	}
}
