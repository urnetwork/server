// The hot vacuum chain's record of when each invalid index was first found.
package server

import (
	"testing"
	"time"
)

// An index found invalid again under the same oid and name keeps when it was
// first found; one with another name under the oid, a time ahead of the run's
// clock and an index no longer found start again or are forgotten.
func TestHotVacuumInvalidIndexesCarryTheirFirstTime(t *testing.T) {
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	previous := map[string]DbHotVacuumInvalidIndex{
		"10": {Name: "public.kept", Table: "t", Since: at.Add(-3 * time.Hour)},
		"11": {Name: "public.renamed", Table: "t", Since: at.Add(-3 * time.Hour)},
		"12": {Name: "public.ahead", Table: "t", Since: at.Add(time.Hour)},
		"13": {Name: "public.gone", Table: "t", Since: at.Add(-5 * time.Hour)},
	}
	next := nextDbHotVacuumInvalidIndexes(previous, []dbInvalidIndex{
		{oid: 10, schema: "public", name: "kept", table: "t"},
		{oid: 11, schema: "public", name: "other", table: "t"},
		{oid: 12, schema: "public", name: "ahead", table: "t"},
		{oid: 14, schema: "public", name: "new", table: "u"},
	}, at)
	want := map[string]DbHotVacuumInvalidIndex{
		"10": {Name: "public.kept", Table: "t", Since: at.Add(-3 * time.Hour)},
		"11": {Name: "public.other", Table: "t", Since: at},
		"12": {Name: "public.ahead", Table: "t", Since: at},
		"14": {Name: "public.new", Table: "u", Since: at},
	}
	if len(next) != len(want) {
		t.Fatalf("next invalid indexes %v, want %v", next, want)
	}
	for key, index := range want {
		if got, ok := next[key]; !ok || got.Name != index.Name || got.Table != index.Table || !got.Since.Equal(index.Since) {
			t.Fatalf("index %s = %+v, want %+v", key, got, index)
		}
	}
	if age := dbHotVacuumInvalidIndexMaxAge(next, at); age != 3*time.Hour {
		t.Fatalf("max age %s, want 3h", age)
	}
	if next := nextDbHotVacuumInvalidIndexes(previous, nil, at); next != nil || dbHotVacuumInvalidIndexMaxAge(next, at) != 0 {
		t.Fatalf("with no invalid index the state keeps %v", next)
	}
}
