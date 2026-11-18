package model

import (
	"reflect"
	"strings"
	"testing"
)

// §21.8's notice classes and the role-mail classifier (abuse_notice_classify.go): pure.

// notice_senders as abuse.yml would give it.
type testAbuseNoticeSenders map[string]string

func (self testAbuseNoticeSenders) NoticeSenderClass(domain string) string {
	return self[domain]
}

var testNoticeSenders = testAbuseNoticeSenders{
	"registrar.example": "registrar",
	"registry.example":  "registry",
	"police.example":    "law_enforcement",
	"typo.example":      "someone",
}

func testNoticeMail(localPart string, signatures ...AbuseNoticeSignature) *AbuseNoticeMail {
	return &AbuseNoticeMail{RecipientLocalPart: localPart, Signatures: signatures}
}

// An unlisted sender, a bad signature and a listed one: only a signature that verified under a listed domain
// classifies, to abuse@ or postmaster@ alone.
func TestClassifyAbuseNoticeReadsOnlyAVerifiedListedSignature(t *testing.T) {
	cases := []struct {
		name string
		mail *AbuseNoticeMail
		want AbuseNoticeClass
	}{
		{"a listed registrar", testNoticeMail("abuse", AbuseNoticeSignature{Domain: "registrar.example", Verified: true}), AbuseNoticeClassRegistrar},
		{"a listed registry to postmaster", testNoticeMail("postmaster", AbuseNoticeSignature{Domain: "registry.example", Verified: true}), AbuseNoticeClassRegistry},
		{"a listed police sender", testNoticeMail("abuse", AbuseNoticeSignature{Domain: "police.example", Verified: true}), AbuseNoticeClassLawEnforcementEmergency},
		{"case and a trailing dot", testNoticeMail("Abuse", AbuseNoticeSignature{Domain: "Registrar.Example.", Verified: true}), AbuseNoticeClassRegistrar},
		{"an unlisted sender", testNoticeMail("abuse", AbuseNoticeSignature{Domain: "stranger.example", Verified: true}), AbuseNoticeClassNone},
		{"a bad signature", testNoticeMail("abuse", AbuseNoticeSignature{Domain: "registrar.example", Verified: false}), AbuseNoticeClassNone},
		{"no signature", testNoticeMail("abuse"), AbuseNoticeClassNone},
		{"a signature with no domain", testNoticeMail("abuse", AbuseNoticeSignature{Verified: true}), AbuseNoticeClassNone},
		{"a subdomain of a listed sender", testNoticeMail("abuse", AbuseNoticeSignature{Domain: "mail.registrar.example", Verified: true}), AbuseNoticeClassNone},
		{"a parent of a listed sender", testNoticeMail("abuse", AbuseNoticeSignature{Domain: "example", Verified: true}), AbuseNoticeClassNone},
		{"a class the file made up", testNoticeMail("abuse", AbuseNoticeSignature{Domain: "typo.example", Verified: true}), AbuseNoticeClassNone},
		{"another recipient", testNoticeMail("sales", AbuseNoticeSignature{Domain: "registrar.example", Verified: true}), AbuseNoticeClassNone},
		{"a report token's recipient", testNoticeMail("k7f3q", AbuseNoticeSignature{Domain: "registrar.example", Verified: true}), AbuseNoticeClassNone},
		{"no recipient", testNoticeMail("", AbuseNoticeSignature{Domain: "registrar.example", Verified: true}), AbuseNoticeClassNone},
		{
			"a stranger's signature beside a listed one",
			testNoticeMail("abuse", AbuseNoticeSignature{Domain: "stranger.example", Verified: true}, AbuseNoticeSignature{Domain: "registrar.example", Verified: true}),
			AbuseNoticeClassRegistrar,
		},
		{
			"a failed listed signature beside a verified stranger",
			testNoticeMail("abuse", AbuseNoticeSignature{Domain: "registrar.example", Verified: false}, AbuseNoticeSignature{Domain: "stranger.example", Verified: true}),
			AbuseNoticeClassNone,
		},
	}
	for _, c := range cases {
		if got := ClassifyAbuseNotice(c.mail, testNoticeSenders); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if got := ClassifyAbuseNotice(nil, testNoticeSenders); got != AbuseNoticeClassNone {
		t.Errorf("no mail: %q", got)
	}
	if got := ClassifyAbuseNotice(testNoticeMail("abuse", AbuseNoticeSignature{Domain: "registrar.example", Verified: true}), nil); got != AbuseNoticeClassNone {
		t.Errorf("no sender list: %q", got)
	}
}

// Several listed signers answer the same class whatever order the headers came in.
func TestClassifyAbuseNoticeDoesNotDependOnHeaderOrder(t *testing.T) {
	registrar := AbuseNoticeSignature{Domain: "registrar.example", Verified: true}
	registry := AbuseNoticeSignature{Domain: "registry.example", Verified: true}
	police := AbuseNoticeSignature{Domain: "police.example", Verified: true}
	orders := [][]AbuseNoticeSignature{
		{registrar, registry, police},
		{police, registry, registrar},
		{registry, police, registrar},
	}
	for _, order := range orders {
		if got := ClassifyAbuseNotice(testNoticeMail("abuse", order...), testNoticeSenders); got != AbuseNoticeClassLawEnforcementEmergency {
			t.Errorf("%v: %q", order, got)
		}
	}
	if got := ClassifyAbuseNotice(testNoticeMail("abuse", registrar, registry), testNoticeSenders); got != AbuseNoticeClassRegistry {
		t.Errorf("registrar then registry: %q", got)
	}
	if got := ClassifyAbuseNotice(testNoticeMail("abuse", registry, registrar), testNoticeSenders); got != AbuseNoticeClassRegistry {
		t.Errorf("registry then registrar: %q", got)
	}
}

// The classifier has nothing of a body to read: what it takes holds a recipient's local part and signatures'
// domains and verdicts, and no field a body, a subject or a From could ride.
func TestAbuseNoticeMailHoldsNoBody(t *testing.T) {
	fieldNames := func(value reflect.Type) []string {
		names := []string{}
		for i := range value.NumField() {
			names = append(names, value.Field(i).Name)
		}
		return names
	}
	if names := fieldNames(reflect.TypeOf(AbuseNoticeMail{})); strings.Join(names, ",") != "RecipientLocalPart,Signatures" {
		t.Errorf("AbuseNoticeMail holds %v", names)
	}
	if names := fieldNames(reflect.TypeOf(AbuseNoticeSignature{})); strings.Join(names, ",") != "Domain,Verified" {
		t.Errorf("AbuseNoticeSignature holds %v", names)
	}
}

// Every class that pages has its own §21.8 row, a reporter class and a route, and nothing else pages.
func TestAbuseNoticeClassRows(t *testing.T) {
	cases := []struct {
		class    AbuseNoticeClass
		signal   AbuseSignal
		reporter AbuseReporterClass
		route    AbuseNoticeRoute
	}{
		{AbuseNoticeClassRegistrar, AbuseSignalRegistrarNotice, AbuseReporterClassRegistrar, AbuseNoticeRouteRota},
		{AbuseNoticeClassRegistry, AbuseSignalRegistryNotice, AbuseReporterClassRegistry, AbuseNoticeRouteRota},
		{AbuseNoticeClassEuRemovalOrder, AbuseSignalEuRemovalOrder, AbuseReporterClassLawEnforcement, AbuseNoticeRouteRota},
		{AbuseNoticeClassLawEnforcementEmergency, AbuseSignalLawEnforcementEmergency, AbuseReporterClassLawEnforcement, AbuseNoticeRouteCounsel},
		{AbuseNoticeClassProductionOrder, AbuseSignalProductionOrder, AbuseReporterClassLawEnforcement, AbuseNoticeRouteCounsel},
	}
	for _, c := range cases {
		if !c.class.Pages() || c.class.Signal() != c.signal || c.class.ReporterClass() != c.reporter || c.class.Route() != c.route {
			t.Errorf("%s: pages %t, signal %s, reporter %s, route %s", c.class, c.class.Pages(), c.class.Signal(), c.class.ReporterClass(), c.class.Route())
		}
		// a notice kills nothing by machine (owner decision 530)
		if kind := AbuseMachineAction(c.class.Signal()); kind != AbuseActionKindNone {
			t.Errorf("%s: a machine takes %q on its signal", c.class, kind)
		}
	}
	for _, class := range []AbuseNoticeClass{
		AbuseNoticeClassNone, "search_console", "public", "law_enforcement", "trusted_flagger", "Registrar", "registrar ",
	} {
		if class.Pages() {
			t.Errorf("%q pages", class)
		}
		if class.Signal() != AbuseSignalReport {
			t.Errorf("%q sits on %s, want the report row", class, class.Signal())
		}
	}
}
