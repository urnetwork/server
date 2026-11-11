package controller

import (
	"encoding/json"
	"math/rand"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// The pool exactly as the prober decodes it: each served destination's wire
// fields by name, general and country lists alike.
func testServedDestinationFields(t *testing.T, sites *ProviderEgressSites, stored []*model.ProviderEgressDestination) map[string]map[string]any {
	t.Helper()
	pool, err := sites.servePool(stored, time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC), rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(pool)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Destinations []map[string]any            `json:"destinations"`
		Countries    map[string][]map[string]any `json:"countries"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	fields := map[string]map[string]any{}
	for _, destination := range wire.Destinations {
		fields[destination["name"].(string)] = destination
	}
	for _, destinations := range wire.Countries {
		for _, destination := range destinations {
			fields[destination["name"].(string)] = destination
		}
	}
	return fields
}

// h3 on a catalog entry is read wherever the catalog is parsed, and served to
// the prober: destinations, promoted candidates and country lists. A missing
// key is false, and false is served as no h3 at all, so a URL without it is
// judged by HTTPS alone. A stored row lends only its pool state: the
// configured h3 is served whatever the row says.
func TestQualityprobeCatalogH3DefaultsFalseAndIsServed(t *testing.T) {
	sites, err := testParseProviderEgressSites(`schema_version: 1
destinations:
  - name: synthetic-h3-site
    class: site
    category: reference
    url: https://h3.example/
    h3: true
  - name: synthetic-tcp-site
    class: site
    category: reference
    url: https://tcp.example/
  - name: synthetic-explicit-tcp-site
    class: site
    category: reference
    url: https://explicit-tcp.example/
    h3: false
candidates:
  - name: synthetic-h3-candidate
    class: cdn
    category: cdn
    url: https://candidate.example/robots.txt
    h3: true
    revision: 1
countries:
  de:
    - name: synthetic-h3-country-site
      class: site
      category: news
      url: https://country.example/
      h3: true
`)
	if err != nil {
		t.Fatal(err)
	}
	stored := []*model.ProviderEgressDestination{
		{Name: "synthetic-h3-site", Class: "site", Url: "https://h3.example/", Expect: "body", Active: true},
		{Name: "synthetic-h3-candidate", Class: "cdn", Url: "https://candidate.example/robots.txt", Expect: "body", Active: true},
	}
	fields := testServedDestinationFields(t, sites, stored)
	for _, name := range []string{"synthetic-h3-site", "synthetic-h3-candidate", "synthetic-h3-country-site"} {
		destination, served := fields[name]
		if !served || destination["h3"] != true {
			t.Errorf("%s: served=%t fields=%v; h3: true must reach the prober", name, served, destination)
		}
	}
	for _, name := range []string{"synthetic-tcp-site", "synthetic-explicit-tcp-site"} {
		destination, served := fields[name]
		if _, declared := destination["h3"]; !served || declared {
			t.Errorf("%s: served=%t fields=%v; a URL without h3 must be served without it", name, served, destination)
		}
	}
}

// h3 is part of the served contract, so the pool version names it: a probe's
// log line says which contract it ran.
func TestQualityprobeCatalogH3ChangesThePoolVersion(t *testing.T) {
	versionOf := func(h3 string) int {
		sites, err := testParseProviderEgressSites(`schema_version: 1
destinations:
  - name: synthetic-site
    class: site
    category: reference
    url: https://site.example/
` + h3)
		if err != nil {
			t.Fatal(err)
		}
		pool, err := sites.servePool(nil, time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC), rand.New(rand.NewSource(1)))
		if err != nil {
			t.Fatal(err)
		}
		return pool.Version
	}
	if versionOf("") == versionOf("    h3: true\n") {
		t.Fatal("declaring h3 left the pool version unchanged")
	}
	if versionOf("") != versionOf("    h3: false\n") {
		t.Fatal("an explicit h3: false changed the pool version")
	}
}
