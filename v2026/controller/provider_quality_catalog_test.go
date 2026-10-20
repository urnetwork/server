// Explicit catalogs are required; missing configuration is not provider evidence.
package controller

import (
	"testing"

	"github.com/urnetwork/server/v2026/model"
	"gopkg.in/yaml.v3"
)

func TestQualityProbeCatalogRequiresExplicitDestinations(t *testing.T) {
	_, err := ParseProviderEgressSites(func(value any) error {
		return yaml.Unmarshal([]byte(`
candidates:
  - name: synthetic-candidate
    class: site
    category: reference
    url: https://candidate.example/
`), value)
	})
	if err == nil {
		t.Fatal("a missing authoritative URL catalog was accepted")
	}
}

func TestQualityProbeCatalogRemovesStaleDatabaseTargets(t *testing.T) {
	sites, err := testParseProviderEgressSites(`
schema_version: 1
destinations:
  - name: selected
    class: site
    category: reference
    url: https://selected.example/new
`)
	if err != nil {
		t.Fatal(err)
	}
	stored := []*model.ProviderEgressDestination{
		{Name: "selected", Url: "https://old.example/", Active: true},
		{Name: "removed", Url: "https://removed.example/", Active: true},
	}
	rows := sites.configuredRows(stored)
	if len(rows) != 1 || rows[0].Name != "selected" || rows[0].Url != "https://selected.example/new" {
		t.Fatal("stored state overrode the authoritative catalog")
	}
	if stored[0].Url != "https://old.example/" {
		t.Fatal("pool construction mutated stored rows")
	}
}

func TestQualityProbeCatalogKeepsRetirementAndNewEntries(t *testing.T) {
	sites, err := testParseProviderEgressSites(`
schema_version: 1
destinations:
  - name: retained
    class: site
    category: reference
    url: https://retained.example/
  - name: newly-configured
    class: site
    category: reference
    url: https://newly-configured.example/
`)
	if err != nil {
		t.Fatal(err)
	}
	rows := sites.configuredRows([]*model.ProviderEgressDestination{{Name: "retained", Active: false}})
	if len(rows) != 2 {
		t.Fatal("new configured entry disappeared before background refresh")
	}
	for _, row := range rows {
		if row.Active != (row.Name == "newly-configured") {
			t.Fatalf("retirement state changed for %s", row.Name)
		}
	}
}
