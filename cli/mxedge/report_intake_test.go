package main

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The platform block's report intake (SMTP-M5-06): the listener's outcomes over the intake's, config's
// smtpedge.yml, and FS-39's report-intake cases at T3, on the private Postgres through the real store and
// §12's real label map. Names are .example and addresses RFC 5737's.

// smtpedge.yml's values for these tests.
func testReportIntakeSettings() *smtpedgeservice.SmtpEdgeSettings {
	settings := smtpedgeservice.DefaultSmtpEdgeSettings()
	settings.Root = "root.example"
	settings.ReportHostName = "rpt.report.example"
	settings.SendingAddresses = []smtpedgeservice.SmtpSendingAddress{
		{Address: netip.MustParseAddr("192.0.2.10"), HeloName: "o1.out.report.example"},
	}
	settings.LoopAllowlist = []string{"loop.example"}
	return settings
}

// Reporters' keys in DNS, and report tokens.
type testReportWorld struct {
	txt         *sidecartest.FakeSmtpDnsTxt
	tokens      *sidecartest.FakeSmtpNetTokens
	reporterKey *sidecartest.SmtpDkimFixtureKey
	arrival     time.Time
}

func newTestReportWorld() *testReportWorld {
	self := &testReportWorld{
		txt:         sidecartest.NewFakeSmtpDnsTxt(),
		tokens:      sidecartest.NewFakeSmtpNetTokens(),
		reporterKey: sidecartest.NewSmtpDkimFixtureRsaKey("rpt1", "reporter.example"),
		arrival:     time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC),
	}
	self.txt.Set(self.reporterKey.RecordName(), self.reporterKey.TxtRecord())
	return self
}

// A gzip DMARC aggregate to recipient, signed by the reporter, its organization named orgName.
func (self *testReportWorld) dmarcAggregate(recipient string, orgName string, reportId string, records []*sidecartest.SmtpFixtureDmarcRecord) []byte {
	xml := sidecartest.BuildSmtpDmarcXml(orgName, reportId, time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC), records)
	message := sidecartest.BuildSmtpAggregateMessage(self.reporterKey.Domain, recipient, "application/gzip", "report.xml.gz", sidecartest.SmtpFixtureGzip(xml))
	return sidecartest.SignSmtpDkimFixture(message, self.reporterKey, &sidecartest.SmtpDkimFixtureOptions{HeaderRelaxed: true, BodyRelaxed: true, Headers: []string{"from", "to", "subject", "date", "message-id"}})
}

// The listener's call for one recipient.
func (self *testReportWorld) accept(t testing.TB, intake mxedgeservice.PlatformIntake, recipient string, raw []byte) (mxedgeservice.PlatformOutcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return intake.Accept(ctx, &mxedgeservice.PlatformMessage{Recipient: recipient, ArrivalTime: self.arrival, Raw: raw})
}

// A person's mail to a role address, enclosing a message from fromDomain.
func testRoleMail(fromDomain string) []byte {
	enclosed := (&sidecartest.SmtpFixtureMessage{FromDomain: fromDomain, MessageId: "<m2@" + fromDomain + ">", Subject: "s", Body: "b"}).Bytes()
	return []byte("From: A Person <person@person.example>\r\nTo: abuse@root.example\r\nSubject: spam from your network\r\n" +
		"Content-Type: multipart/mixed; boundary=\"" + sidecartest.SmtpFixtureBoundary + "\"\r\n\r\n" +
		"--" + sidecartest.SmtpFixtureBoundary + "\r\nContent-Type: text/plain\r\n\r\nPlease look at the attached.\r\n" +
		"--" + sidecartest.SmtpFixtureBoundary + "\r\nContent-Type: message/rfc822\r\n\r\n" + string(enclosed) + "\r\n--" + sidecartest.SmtpFixtureBoundary + "--\r\n")
}

// The intake reads MG2's label map and writes MG5's report tables, so the block waits on both.
func TestReportIntakeGroupsAreTheTablesItReads(t *testing.T) {
	groups := reportIntakeGroups()
	names := []string{}
	for _, group := range groups {
		names = append(names, group.Name)
		if group.Linked == nil {
			t.Fatalf("%s has no marker", group.Name)
		}
	}
	if !slices.Equal(names, []string{"MG2", "MG5"}) {
		t.Fatalf("groups %v", names)
	}
	if groups[0].Linked() != server.SidecarMg2Linked() || groups[1].Linked() != server.SidecarMg5Linked() {
		t.Fatal("a group's marker is not its own")
	}
}

// Role mail the intake cannot make a case of, which until SMTP-M9-15 is all of it, is unread, so the listener
// can answer plan-ABUSE-auto-reply's 550; every other finished outcome is taken, whatever it did.
func TestReportIntakeAnswersUnattributedRoleMailUnread(t *testing.T) {
	world := newTestReportWorld()
	store := sidecartest.NewFakeSmtpEdgeStore(nil, nil)
	intake, err := newPlatformReportIntake(store, testReportIntakeSettings(), world.txt, world.tokens)
	if err != nil {
		t.Fatal(err)
	}
	for _, recipient := range []string{"abuse@root.example", "postmaster@root.example", "Abuse@Root.Example."} {
		outcome, err := world.accept(t, intake, recipient, testRoleMail("quietnet.root.example"))
		if err != nil || outcome != mxedgeservice.PlatformOutcomeUnread {
			t.Fatalf("%s: %v, %v", recipient, outcome, err)
		}
	}
	// abuse@ on another domain is no role address of this listener's, and an unknown token drops unread; both
	// are taken, which tells their senders nothing
	for _, recipient := range []string{"abuse@other.example", "abcdefghijklmnopqrstuvwxyz@rpt.report.example"} {
		outcome, err := world.accept(t, intake, recipient, testRoleMail("quietnet.root.example"))
		if err != nil || outcome != mxedgeservice.PlatformOutcomeTaken {
			t.Fatalf("%s: %v, %v", recipient, outcome, err)
		}
	}
	if len(store.Complaints()) != 0 || len(store.Aggregates()) != 0 {
		t.Fatal("unattributable mail left a row")
	}
}

// An aggregate at a network's token is taken and counted once, and a store that did not answer is an error,
// which the listener answers 451, so the reporter retries and the retry counts.
func TestReportIntakeTakesAnAggregateAndAsksForARetryOnAStoreFailure(t *testing.T) {
	world := newTestReportWorld()
	faults := sidecartest.NewFaultQueue()
	store := sidecartest.NewFakeSmtpEdgeStore(nil, faults)
	intake, err := newPlatformReportIntake(store, testReportIntakeSettings(), world.txt, world.tokens)
	if err != nil {
		t.Fatal(err)
	}
	networkId := sidecar.NewId()
	recipient := world.tokens.Add(networkId) + "@rpt.report.example"
	raw := world.dmarcAggregate(recipient, "reporter.example", "report-1", []*sidecartest.SmtpFixtureDmarcRecord{
		{SourceIp: "192.0.2.10", Count: 5, HeaderFrom: "quietnet.root.example", Disposition: "none", DkimAligned: "pass", SpfAligned: "fail", DkimDomain: "quietnet.root.example", DkimSelector: "abcdefghijklmnopqrstuvwxyz", DkimResult: "pass"},
	})
	if _, err := faults.Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpHost, KeyPrefix: "smtpedge.InsertAggregate", Result: sidecartest.FailBeforeApply}); err != nil {
		t.Fatal(err)
	}
	if _, err := world.accept(t, intake, recipient, raw); err == nil {
		t.Fatal("a store failure was taken")
	}
	for range 2 {
		outcome, err := world.accept(t, intake, recipient, raw)
		if err != nil || outcome != mxedgeservice.PlatformOutcomeTaken {
			t.Fatalf("%v, %v", outcome, err)
		}
	}
	aggregates := store.Aggregates()
	if len(aggregates) != 1 || aggregates[0].Counts[0].NetworkId != networkId || aggregates[0].Counts[0].MessageCount != 5 {
		t.Fatalf("aggregates %+v", aggregates)
	}
}

// The intake reads smtpedge.yml from config, and a file it refuses leaves the block without one, saying which
// file and never its values; so does one whose root or report host is not mxedge.yml's, since the listener
// would take mail the intake cannot route.
func TestReportIntakeReadsSmtpEdgeYmlFromConfig(t *testing.T) {
	mxEdgeSettings := mxedgeservice.DefaultMxEdgeSettings()
	mxEdgeSettings.PlatformRootDomainName = "Root.Example."
	mxEdgeSettings.PlatformReportHostName = "rpt.report.example"
	smtpEdgeYml := func(root string, reportHost string) []byte {
		return []byte(
			"root: " + root + "\n" +
				"report_host: " + reportHost + "\n" +
				"sending_addresses:\n" +
				"  - address: 192.0.2.10\n" +
				"    helo: o1.out.report.example\n" +
				"loop_allowlist: [loop.example]\n",
		)
	}
	popValid := server.Config.PushSimpleResource(reportIntakeSettingsName, smtpEdgeYml("root.example", "rpt.report.example"))
	intake, err := newConfiguredReportIntake(mxEdgeSettings)
	popValid()
	if err != nil || intake == nil || intake.root != "root.example" {
		t.Fatalf("a valid smtpedge.yml: %v", err)
	}
	for _, mismatch := range [][2]string{{"other-root.example", "rpt.report.example"}, {"root.example", "rpt.other.example"}} {
		pop := server.Config.PushSimpleResource(reportIntakeSettingsName, smtpEdgeYml(mismatch[0], mismatch[1]))
		intake, err := newConfiguredReportIntake(mxEdgeSettings)
		pop()
		if err == nil || intake != nil || !strings.Contains(err.Error(), "mxedge.yml") {
			t.Fatalf("names %v that are not mxedge.yml's: %v", mismatch, err)
		}
	}
	// report_host left out, and a planted value that must not reach the error
	popRefused := server.Config.PushSimpleResource(reportIntakeSettingsName, []byte("root: planted-root-value.example\n"))
	defer popRefused()
	intake, err = newConfiguredReportIntake(mxEdgeSettings)
	if err == nil || intake != nil {
		t.Fatal("an smtpedge.yml with no report_host built an intake")
	}
	if !strings.Contains(err.Error(), reportIntakeSettingsName) || strings.Contains(err.Error(), "planted-root-value") {
		t.Fatalf("the error %q", err)
	}
}

// FS-39's report-intake case at T3: an aggregate at a network's token counts once per reporter and report id
// through the real store, a repeat counts nothing, and neither table holds the report's text.
func TestReportIntakeCountsAnAggregateOnceOnPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		world := newTestReportWorld()
		intake, err := newPlatformReportIntake(model.NewSmtpEdgeStore(nil), testReportIntakeSettings(), world.txt, world.tokens)
		if err != nil {
			t.Fatal(err)
		}
		networkId := sidecar.NewId()
		recipient := world.tokens.Add(networkId) + "@rpt.report.example"
		// the organization's name is report text, which no row may hold
		canary := "canary-org-7f3a9c.example"
		raw := world.dmarcAggregate(recipient, canary, "report-1", []*sidecartest.SmtpFixtureDmarcRecord{
			{SourceIp: "192.0.2.10", Count: 5, HeaderFrom: "quietnet.root.example", Disposition: "none", DkimAligned: "pass", SpfAligned: "fail", DkimDomain: "quietnet.root.example", DkimSelector: "abcdefghijklmnopqrstuvwxyz", DkimResult: "pass"},
			{SourceIp: "192.0.2.10", Count: 2, HeaderFrom: "quiet-otter.quietnet.root.example", Disposition: "reject", DkimAligned: "fail", SpfAligned: "fail", DkimDomain: "quiet-otter.quietnet.root.example", DkimSelector: "bcdefghijklmnopqrstuvwxyza", DkimResult: "fail"},
			// a third party's row, dropped at parse
			{SourceIp: "198.51.100.7", Count: 9, HeaderFrom: "quietnet.root.example", Disposition: "reject", DkimAligned: "fail", SpfAligned: "fail", DkimDomain: "thirdparty.example", DkimSelector: "x", DkimResult: "fail"},
		})
		for range 2 {
			outcome, err := world.accept(t, intake, recipient, raw)
			if err != nil || outcome != mxedgeservice.PlatformOutcomeTaken {
				t.Fatalf("%v, %v", outcome, err)
			}
		}
		reports := model.GetSmtpAggregateReports(ctx, server.Id(networkId), time.Time{})
		if len(reports) != 1 || reports[0].Reporter != "reporter.example" || reports[0].ReportId != "report-1" || len(reports[0].Counts) != 2 {
			t.Fatalf("reports %+v", reports)
		}
		byDomain := map[string]uint64{}
		for _, count := range reports[0].Counts {
			byDomain[count.Domain] = count.MessageCount
		}
		if byDomain["quietnet.root.example"] != 5 || byDomain["quiet-otter.quietnet.root.example"] != 2 {
			t.Fatalf("counts %v", byDomain)
		}
		// no mail key, so no sealed copy and no rua row (plan-SMTP-report-seal-key's interim)
		if complaints := model.GetSmtpComplaints(ctx, server.Id(networkId), time.Time{}); len(complaints) != 0 {
			t.Fatalf("%d complaint rows with no mail key", len(complaints))
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, table := range []string{"smtp_dmarc_daily", "smtp_complaint"} {
				var found bool
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` stored WHERE strpos(row_to_json(stored)::text, $1) > 0 OR strpos(row_to_json(stored)::text, '198.51.100.7') > 0)`, canary).Scan(&found))
				if found {
					t.Errorf("%s holds report text or a third party's address", table)
				}
			}
		})
	})
}

// FS-39's split at T3: an aggregate at <root>'s rua names two networks by their labels in §12's real label
// map, a burner's <blabel>.<net>.<root> counting toward <net>, and a label nothing holds counts toward no one.
func TestReportIntakeSplitsRootRowsByTheLabelMapOnPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() || !server.SidecarMg2Linked() {
			return
		}
		ctx := context.Background()
		world := newTestReportWorld()
		settings := testReportIntakeSettings()
		intake, err := newPlatformReportIntake(model.NewSmtpEdgeStore(nil), settings, world.txt, nil)
		if err != nil {
			t.Fatal(err)
		}
		firstNetworkId := server.NewId()
		secondNetworkId := server.NewId()
		firstLabel, err := model.EnsureNetworkLabel(ctx, firstNetworkId, settings.Root)
		if err != nil {
			t.Fatal(err)
		}
		secondLabel, err := model.EnsureNetworkLabel(ctx, secondNetworkId, settings.Root)
		if err != nil {
			t.Fatal(err)
		}
		recipient := "root@rpt.report.example"
		raw := world.dmarcAggregate(recipient, "reporter.example", "root-report-1", []*sidecartest.SmtpFixtureDmarcRecord{
			{SourceIp: "192.0.2.10", Count: 3, HeaderFrom: firstLabel + ".root.example", Disposition: "none", DkimAligned: "pass", SpfAligned: "fail", DkimDomain: firstLabel + ".root.example", DkimSelector: "abcdefghijklmnopqrstuvwxyz", DkimResult: "pass"},
			{SourceIp: "192.0.2.10", Count: 4, HeaderFrom: "quiet-otter." + secondLabel + ".root.example", Disposition: "none", DkimAligned: "pass", SpfAligned: "fail", DkimDomain: "quiet-otter." + secondLabel + ".root.example", DkimSelector: "bcdefghijklmnopqrstuvwxyza", DkimResult: "pass"},
			{SourceIp: "192.0.2.10", Count: 6, HeaderFrom: "nobodyholds-this-label.root.example", Disposition: "none", DkimAligned: "pass", SpfAligned: "fail", DkimDomain: "x.example", DkimSelector: "x", DkimResult: "fail"},
		})
		outcome, err := world.accept(t, intake, recipient, raw)
		if err != nil || outcome != mxedgeservice.PlatformOutcomeTaken {
			t.Fatalf("%v, %v", outcome, err)
		}
		first := model.GetSmtpAggregateReports(ctx, firstNetworkId, time.Time{})
		second := model.GetSmtpAggregateReports(ctx, secondNetworkId, time.Time{})
		if len(first) != 1 || len(first[0].Counts) != 1 || first[0].Counts[0].MessageCount != 3 {
			t.Fatalf("the first network's reports %+v", first)
		}
		if len(second) != 1 || len(second[0].Counts) != 1 || second[0].Counts[0].MessageCount != 4 || second[0].Counts[0].Domain != "quiet-otter."+secondLabel+".root.example" {
			t.Fatalf("the second network's reports %+v", second)
		}
		// the same report at <root> again counts nothing for either network
		if _, err := world.accept(t, intake, recipient, raw); err != nil {
			t.Fatal(err)
		}
		if len(model.GetSmtpAggregateReports(ctx, firstNetworkId, time.Time{})) != 1 {
			t.Fatal("a repeat counted again")
		}
	})
}

// A label read that fails reaches the intake as an error, so the reporter retries, never as no network: here
// a binary without MG2, whose label map answers unlinked.
func TestReportLabelSourceSaysUnknownOrFails(t *testing.T) {
	source := &reportLabelSource{root: "root.example"}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, ok, err := source.NetworkForLabel(ctx, "nobodyholds-this-label")
		if !server.SidecarMg2Linked() {
			if ok || !errors.Is(err, model.ErrDnsLabelUnlinked) {
				t.Fatalf("without MG2: %t, %v", ok, err)
			}
			return
		}
		if ok || err != nil {
			t.Fatalf("an unheld label: %t, %v", ok, err)
		}
		networkId := server.NewId()
		label, err := model.EnsureNetworkLabel(ctx, networkId, "root.example")
		if err != nil {
			t.Fatal(err)
		}
		resolved, ok, err := source.NetworkForLabel(ctx, label)
		if err != nil || !ok || resolved != sidecar.Id(networkId) {
			t.Fatalf("a held label: %v, %t, %v", resolved, ok, err)
		}
	})
}
