// Ordinary URL catalogs need no hidden DNS, connectivity, or CDN class quotas.
package egresshealth

import "testing"

func TestUrlPoolAcceptsOnlyOrdinarySites(t *testing.T) {
	err := ValidateDestinations([]Destination{{Name: "synthetic-site", Class: ClassSite, Url: "https://site.example/"}})
	if err != nil {
		t.Fatalf("one valid configured site was rejected by obsolete class gates: %v", err)
	}
}
