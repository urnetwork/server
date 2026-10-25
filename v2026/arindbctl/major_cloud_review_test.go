// Public ARIN handles are reviewed policy inputs, not private provider IDs.
package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// CI binds the exact Config artifact or checks out the adjacent Config repo.
// Independent required owners catch omissions that catalog enumeration misses.
func TestReviewedArinMajorCloudOwners(t *testing.T) {
	path := os.Getenv("ARIN_REVIEWED_RULES_PATH")
	if path == "" {
		path = filepath.Join("..", "..", "config", "main", "arindb.yml")
	}
	rules, err := loadClassificationRules(path)
	if err != nil {
		t.Fatalf("major-cloud gate requires valid Config/main/arindb.yml or ARIN_REVIEWED_RULES_PATH: %v", err)
	}
	address := netip.MustParseAddr("192.0.2.1")
	for _, expected := range []struct {
		handle, rule string
	}{
		{handle: "GOOGL-2", rule: "google-cloud-customers"},
		{handle: "AL-3", rule: "alibaba-cloud"},
		{handle: "IBMC-24", rule: "ibm-softlayer-cloud"},
		{handle: "SOFTL", rule: "ibm-softlayer-cloud"},
		{handle: "AMAZO-4", rule: "aws-ec2-registration"},
		{handle: "OC-195", rule: "oracle-public-cloud"},
	} {
		owner := arinOrganization{Handle: expected.handle, Name: "Synthetic owner"}
		classification := rules.classify([]arinOrganization{owner}, address)
		if !classification.nonQuality || classification.ruleName != expected.rule || classification.orgHandle != owner.Handle || classification.reason == "" || classification.source == "" {
			t.Errorf("required cloud registration %s missing exact non_quality rule/provenance", expected.handle)
		}
		child := arinOrganization{Handle: "TEST-UNKNOWN-CHILD", Name: "Synthetic unreviewed child"}
		if inherited := rules.classify([]arinOrganization{owner, child}, address); !inherited.nonQuality || inherited.orgHandle != owner.Handle {
			t.Errorf("cloud registration %s lost organization-child inheritance", expected.handle)
		}
		for _, handle := range []string{"GF", "GF-231", "GF-238"} {
			access := arinOrganization{Handle: handle, Name: "Synthetic access owner"}
			clean := rules.classify([]arinOrganization{owner, access}, address)
			if clean.nonQuality || clean.ruleName != "google-fiber-consumer-access" || clean.orgHandle != access.Handle {
				t.Errorf("reviewed access child %s lost precedence over %s", handle, expected.handle)
			}
		}
	}
	for _, name := range []string{"Synthetic Residential Access", "Synthetic Business Access", "Synthetic Cloud Hosting Access"} {
		owner := arinOrganization{Handle: "TEST-UNREVIEWED", Name: name}
		if unknown := rules.classify([]arinOrganization{owner}, address); unknown.nonQuality != (rules.QualityPolicyVersion == 2) || unknown.ruleName != "" {
			t.Errorf("unreviewed synthetic access lost its unknown classification: %s", name)
		}
	}
}
