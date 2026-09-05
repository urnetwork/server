// Payout canonicalization regressions cover unordered and ambiguous database
// rows before they can affect positional bindings or signed snapshots.
package controller

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Builds one valid database-shaped payout row with independently selectable
// provider and network identities.
func stCanonicalUsageRow(client, network byte, usage int64) *model.StProviderUsage {
	return &model.StProviderUsage{
		ClientId:        server.Id{client},
		NetworkId:       server.Id{network},
		PayoutByteCount: usage,
	}
}

// Proves arbitrary database result order produces one stable client order
// without mutating the caller's slice.
func TestStCanonicalProviderUsagesOrdersIndependentDatabaseResults(t *testing.T) {
	first := stCanonicalUsageRow(1, 11, 100)
	second := stCanonicalUsageRow(2, 12, 200)
	third := stCanonicalUsageRow(3, 13, 300)
	permutations := [][]*model.StProviderUsage{
		{third, first, second},
		{second, third, first},
		{first, second, third},
	}
	for permutationIndex, rows := range permutations {
		original := append([]*model.StProviderUsage(nil), rows...)
		ordered, err := stCanonicalProviderUsages(rows)
		if err != nil {
			t.Fatalf("permutation %d: %v", permutationIndex, err)
		}
		if len(ordered) != 3 || ordered[0] != first || ordered[1] != second || ordered[2] != third {
			t.Fatalf("permutation %d produced noncanonical order: %+v", permutationIndex, ordered)
		}
		for index := range rows {
			if rows[index] != original[index] {
				t.Fatalf("permutation %d mutated caller row %d", permutationIndex, index)
			}
		}
	}
}

// Rejects ambiguous or malformed source rows before positional binding lookup
// can authenticate the wrong client or snapshot bytes.
func TestStCanonicalProviderUsagesRejectsDuplicateAndMalformedRows(t *testing.T) {
	valid := stCanonicalUsageRow(1, 11, 100)
	tests := []struct {
		name string
		rows []*model.StProviderUsage
	}{
		{name: "duplicate-client-across-networks", rows: []*model.StProviderUsage{valid, stCanonicalUsageRow(1, 12, 200)}},
		{name: "nil-row", rows: []*model.StProviderUsage{nil}},
		{name: "zero-client", rows: []*model.StProviderUsage{{ClientId: server.Id{}, NetworkId: server.Id{1}, PayoutByteCount: 1}}},
		{name: "zero-network", rows: []*model.StProviderUsage{{ClientId: server.Id{1}, NetworkId: server.Id{}, PayoutByteCount: 1}}},
		{name: "negative-usage", rows: []*model.StProviderUsage{{ClientId: server.Id{1}, NetworkId: server.Id{1}, PayoutByteCount: -1}}},
	}
	for _, test := range tests {
		if ordered, err := stCanonicalProviderUsages(test.rows); err == nil {
			t.Fatalf("%s unexpectedly produced %+v", test.name, ordered)
		}
	}
}
