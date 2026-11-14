package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// §21.8's pointer (abuse_auto_reply.go): which url the listener's refusal names, and that nothing is sent or
// kept for role mail dropped unread. Names are .example.

const testAbuseFormUrl = "https://abuse.report.example/report"

// mxedge.yml's settings as the platform block needs them, with no abuse url.
func testAutoReplyMxEdgeSettings() *mxedgeservice.MxEdgeSettings {
	settings := mxedgeservice.DefaultMxEdgeSettings()
	settings.BannerHostName = "mx.report.example"
	settings.DeviationUrl = "https://deviation.report.example/mx"
	settings.PlatformRootDomainName = "root.example"
	settings.PlatformReportHostName = "rpt.report.example"
	return settings
}

// A file that leaves the url out takes the other's, and the listener's settings then name abuse.yml's form.
func TestAbuseAutoReplyPointsAtTheFormUrl(t *testing.T) {
	for _, urls := range []struct {
		formUrl          string
		platformAbuseUrl string
	}{
		{testAbuseFormUrl, ""},
		{testAbuseFormUrl, testAbuseFormUrl},
		{" " + testAbuseFormUrl + "\n", testAbuseFormUrl},
		{"", testAbuseFormUrl},
	} {
		settings := testAutoReplyMxEdgeSettings()
		settings.PlatformAbuseUrl = urls.platformAbuseUrl
		reply, err := NewAbuseAutoReply(settings, urls.formUrl)
		if err != nil || reply.FormUrl != testAbuseFormUrl {
			t.Fatalf("%+v: %+v, %v", urls, reply, err)
		}
		reply.Apply(settings)
		if settings.PlatformAbuseUrl != testAbuseFormUrl {
			t.Fatalf("%+v: the listener would name %q", urls, settings.PlatformAbuseUrl)
		}
		if err := settings.ValidatePlatform(); err != nil {
			t.Fatalf("%+v: the listener refuses the settings: %v", urls, err)
		}
	}
	// a url that cannot go in a reply is the listener's to refuse, wherever it came from
	for _, formUrl := range []string{"https://abuse.report.example/a b", "http://abuse.report.example/report", "https://abuse.report.example/" + strings.Repeat("a", 500)} {
		settings := testAutoReplyMxEdgeSettings()
		reply, err := NewAbuseAutoReply(settings, formUrl)
		if err != nil {
			t.Fatal(err)
		}
		reply.Apply(settings)
		if err := settings.ValidatePlatform(); !errors.Is(err, mxedgeservice.ErrMxEdgeSettingsRefused) {
			t.Fatalf("%q: the listener took it: %v", formUrl, err)
		}
	}
}

// Two files that disagree, or that both leave the url out, are refused, and the refusal names the keys and
// never a value. The settings stay as they were.
func TestAbuseAutoReplyRefusesTwoPointers(t *testing.T) {
	settings := testAutoReplyMxEdgeSettings()
	settings.PlatformAbuseUrl = "https://planted-mx-value.example/report"
	reply, err := NewAbuseAutoReply(settings, "https://planted-abuse-value.example/report")
	if !errors.Is(err, ErrAbuseAutoReplyRefused) || reply != nil {
		t.Fatalf("two urls: %+v, %v", reply, err)
	}
	if strings.Contains(err.Error(), "planted") || !strings.Contains(err.Error(), "form_url") || !strings.Contains(err.Error(), "platform_abuse_url") {
		t.Fatalf("the refusal reads %q", err)
	}
	if settings.PlatformAbuseUrl != "https://planted-mx-value.example/report" {
		t.Fatal("a refusal changed the settings")
	}
	if reply, err := NewAbuseAutoReply(testAutoReplyMxEdgeSettings(), " "); !errors.Is(err, ErrAbuseAutoReplyRefused) || reply != nil {
		t.Fatalf("no url: %+v, %v", reply, err)
	}
	if reply, err := NewAbuseAutoReply(nil, testAbuseFormUrl); !errors.Is(err, ErrAbuseAutoReplyRefused) || reply != nil {
		t.Fatalf("no settings: %+v, %v", reply, err)
	}
}

// The pointer holds its url and nothing else: there is no sender, hash, store or clock to keep, since the
// answer to plan-ABUSE-auto-reply is that nothing is sent. A field added here is a reply path added, which
// needs the owner's decision first.
func TestAbuseAutoReplyHoldsOnlyTheUrl(t *testing.T) {
	replyType := reflect.TypeOf(AbuseAutoReply{})
	if replyType.NumField() != 1 || replyType.Field(0).Name != "FormUrl" || replyType.Field(0).Type.Kind() != reflect.String {
		t.Fatalf("AbuseAutoReply holds %d fields", replyType.NumField())
	}
}

// Role mail dropped unread gets the listener's refusal and nothing else: whether it came from a person, a
// null sender's bounce or an Auto-Submitted robot, the intake ends unread without one call to its store, so
// nothing of the message or its sender is kept, and there is no reply for any of them to loop on.
func TestAbuseAutoReplySendsNothingAndStoresNothing(t *testing.T) {
	world := newTestReportWorld()
	store := sidecartest.NewFakeSmtpEdgeStore(nil, nil)
	intake, err := newPlatformReportIntake(store, testReportIntakeSettings(), world.txt, world.tokens)
	if err != nil {
		t.Fatal(err)
	}
	person := testRoleMail("quietnet.root.example")
	robot := []byte(strings.Replace(string(person), "Subject: spam from your network\r\n", "Subject: spam from your network\r\nAuto-Submitted: auto-replied\r\n", 1))
	bounce := []byte(strings.Replace(string(person), "From: A Person <person@person.example>\r\n", "From: Mail Delivery Subsystem <>\r\nReturn-Path: <>\r\nAuto-Submitted: auto-generated\r\n", 1))
	if string(robot) == string(person) || string(bounce) == string(person) {
		t.Fatal("a fixture did not change")
	}
	for _, raw := range [][]byte{person, robot, bounce} {
		for _, recipient := range []string{"abuse@root.example", "postmaster@root.example"} {
			outcome, err := world.accept(t, intake, recipient, raw)
			if err != nil || outcome != mxedgeservice.PlatformOutcomeUnread {
				t.Fatalf("%s: %v, %v", recipient, outcome, err)
			}
		}
	}
	if counts := store.OpCounts(); len(counts) != 0 {
		t.Fatalf("unread role mail reached the store: %v", counts)
	}
	if len(store.Complaints()) != 0 || len(store.Aggregates()) != 0 {
		t.Fatal("unread role mail left a row")
	}
}

// The block's intake settles the pointer first: with abuse.yml's form it is built and mxedge.yml's settings
// name the form, and two files that disagree leave the block without an intake.
func TestReportIntakeWithAutoReply(t *testing.T) {
	smtpEdgeYml := []byte(
		"root: root.example\n" +
			"report_host: rpt.report.example\n" +
			"sending_addresses:\n" +
			"  - address: 192.0.2.10\n" +
			"    helo: o1.out.report.example\n",
	)
	defer server.Config.PushSimpleResource(reportIntakeSettingsName, smtpEdgeYml)()

	settings := testAutoReplyMxEdgeSettings()
	intake, err := newReportIntakeWithAutoReply(settings, testAbuseFormUrl)
	if err != nil || intake == nil || settings.PlatformAbuseUrl != testAbuseFormUrl {
		t.Fatalf("abuse.yml's form: %v, the listener names %q", err, settings.PlatformAbuseUrl)
	}
	settings = testAutoReplyMxEdgeSettings()
	settings.PlatformAbuseUrl = "https://other.report.example/report"
	intake, err = newReportIntakeWithAutoReply(settings, testAbuseFormUrl)
	if !errors.Is(err, ErrAbuseAutoReplyRefused) || intake != nil {
		t.Fatalf("two urls: %v", err)
	}
}
