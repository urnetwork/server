package model

import (
	"strings"
	"testing"
)

// A provider is live for a named Quality request exactly when it is live for
// native Quality: the two queries share the live relation text for text and
// differ only from the final selection on. The named query never reads the
// ARIN quality facts; the native query still does.
func TestNamedSubscriberQueryKeepsTheQualityLiveRelation(t *testing.T) {
	live := func(name string, query string) string {
		end := strings.Index(query, "\tSELECT candidates.client_id")
		if end < 0 {
			t.Fatalf("%s has no final candidate selection", name)
		}
		return query[:end]
	}
	if live("named", providerNamedSubscriberExclusionsSql) != live("native", providerSubscriberExclusionsSql) {
		t.Fatal("the named Quality read's live relation differs from native Quality's")
	}
	for _, fact := range []string{"arin_quality_verified", "arin_non_quality"} {
		if strings.Contains(providerNamedSubscriberExclusionsSql, fact) {
			t.Errorf("the named Quality read reads %s", fact)
		}
		if !strings.Contains(providerSubscriberExclusionsSql, fact) {
			t.Errorf("the native Quality read no longer reads %s", fact)
		}
	}
	having := strings.Index(providerNamedSubscriberExclusionsSql, "HAVING")
	if having < 0 {
		t.Fatal("the named Quality read has no refusal condition")
	}
	for _, refusal := range []string{"count(live.connection_id) = 0", "bool_or(COALESCE(location.arin_risk, false))"} {
		if !strings.Contains(providerNamedSubscriberExclusionsSql[having:], refusal) {
			t.Errorf("the named Quality read does not refuse on %s", refusal)
		}
	}
}
