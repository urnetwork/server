// Release-catalog checks load identities only from the explicit private input.
package main

import (
	"net/netip"
	"os"
	"regexp/syntax"
	"strings"
	"testing"
)

// Production identities are never copied into test fixtures. The release gate
// supplies the exact reviewed config artifact; ordinary offline tests skip it.
func TestReviewedArinRules(t *testing.T) {
	path := os.Getenv("ARIN_REVIEWED_RULES_PATH")
	if path == "" {
		t.Skip("set ARIN_REVIEWED_RULES_PATH to the reviewed release rules")
	}
	rules, err := loadClassificationRules(path)
	if err != nil {
		t.Fatal(err)
	}
	address := netip.MustParseAddr("192.0.2.1")
	var hostingOwner, accessOwner *arinOrganization
	for _, rule := range rules.Rules {
		owners := []arinOrganization{}
		for _, handle := range rule.OrgHandles {
			owners = append(owners, arinOrganization{Handle: handle, Name: "Synthetic name"})
		}
		if rule.OrgNamePattern != "" {
			pattern := strings.TrimPrefix(rule.OrgNamePattern, "(?i)")
			if !strings.HasPrefix(pattern, "^") || !strings.HasSuffix(pattern, "$") {
				t.Fatal("a reviewed organization-name match is not anchored")
			}
			expression, err := syntax.Parse(rule.OrgNamePattern, syntax.Perl)
			if err != nil {
				t.Fatal("a reviewed name rule is malformed")
			}
			// Initial rules allow exact names and optional punctuation, not an
			// open-ended expression that could accidentally match an access ISP.
			var literalName func(*syntax.Regexp) string
			literalName = func(expression *syntax.Regexp) string {
				switch expression.Op {
				case syntax.OpEmptyMatch, syntax.OpBeginText, syntax.OpEndText:
					return ""
				case syntax.OpLiteral:
					return string(expression.Rune)
				case syntax.OpCapture, syntax.OpConcat, syntax.OpQuest:
					var result strings.Builder
					for _, child := range expression.Sub {
						result.WriteString(literalName(child))
					}
					return result.String()
				default:
					t.Fatal("a reviewed name rule is broader than an exact name")
					return ""
				}
			}
			name := literalName(expression)
			if !rule.pattern.MatchString(name) || rule.pattern.MatchString("Synthetic prefix "+name) || rule.pattern.MatchString(name+" synthetic suffix") {
				t.Fatal("an exact-name rule failed its matching or near-name control")
			}
			owners = append(owners, arinOrganization{Handle: "TEST-EXACT-NAME", Name: name})
		}
		for _, owner := range owners {
			classification := rules.classify([]arinOrganization{owner}, address)
			if classification.nonQuality != *rule.NonQuality || classification.ruleName != rule.Name || classification.source == "" || classification.reason == "" {
				t.Fatal("a reviewed rule lost its classification or provenance")
			}
			if *rule.NonQuality {
				copy := owner
				hostingOwner = &copy
			} else {
				copy := owner
				accessOwner = &copy
			}
		}
	}
	if hostingOwner == nil || accessOwner == nil {
		t.Fatal("reviewed catalog needs hosting positives and an explicit access-owner control")
	}
	unknown := arinOrganization{Handle: "TEST-UNREVIEWED", Name: "Synthetic cloud hosting and consumer broadband"}
	if unknownClass := rules.classify([]arinOrganization{unknown}, address); unknownClass.nonQuality != (rules.QualityPolicyVersion == 2) || unknownClass.ruleName != "" {
		t.Fatal("unreviewed names lost the explicit unknown policy")
	}
	if !rules.classify([]arinOrganization{*hostingOwner, unknown}, address).nonQuality {
		t.Fatal("a reviewed hosting owner did not pass classification to an unknown child")
	}
	if rules.classify([]arinOrganization{*hostingOwner, *accessOwner}, address).nonQuality {
		t.Fatal("an explicit access owner did not override a hosting ancestor")
	}
	if !rules.classify([]arinOrganization{*accessOwner, *hostingOwner}, address).nonQuality {
		t.Fatal("a direct hosting owner lost precedence over an access ancestor")
	}
}
