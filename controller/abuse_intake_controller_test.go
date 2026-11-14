package controller

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// §21.8's intake (abuse_intake_controller.go) on §12's real label map and MG5's
// tables, with a recording AbuseEffects and mailer. The store cases run in a
// build with sidecar_pending. Names are .example.

const testAbuseIntakeRoot = "root.example"

// §21's settings with the test root and a platform name, as abuse.yml would
// give them, for the rest of the test.
func newTestAbuseIntake(t testing.TB) *model.AbuseSettings {
	settings := model.DefaultAbuseSettings()
	settings.RootNames = []string{testAbuseIntakeRoot}
	settings.PlatformNames = []string{"platform.example"}
	previous := abuseIntakeDeps.settings
	abuseIntakeDeps.settings = func() *model.AbuseSettings {
		return settings
	}
	t.Cleanup(func() {
		abuseIntakeDeps.settings = previous
	})
	return settings
}

// A network with an admin and a label in the test root.
func newTestAbuseIntakeNetwork(t testing.TB, ctx context.Context) (networkId server.Id, zone string) {
	t.Helper()
	networkId = server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "abuse-"+networkId.String()[:8], server.NewId())
	label, err := model.EnsureNetworkLabel(ctx, networkId, testAbuseIntakeRoot)
	if err != nil {
		t.Fatal(err)
	}
	return networkId, label + "." + testAbuseIntakeRoot
}

// Takes report, failing on an error.
func testAcceptAbuseReport(t testing.TB, ctx context.Context, report *model.AbuseReport) *AbuseIntakeResult {
	t.Helper()
	result, err := AcceptAbuseReport(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("no result")
	}
	return result
}

// The kinds of a result's actions, in order.
func testAbuseIntakeKinds(result *AbuseIntakeResult) string {
	kinds := []string{}
	for _, action := range result.Actions {
		kinds = append(kinds, string(action.Action.Kind))
	}
	return strings.Join(kinds, ",")
}

func testAbuseIntakeCaseCount(t testing.TB, ctx context.Context) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_case`).Scan(&count))
	})
	return
}

// Every row intake or the action path can write, as text, for a canary search.
func testAbuseIntakeRowsText(t testing.TB, ctx context.Context) string {
	t.Helper()
	rows := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, table := range []string{"abuse_case", "abuse_action", "network_abuse_state", "account_message_outbox", "network_admin_event"} {
			result, err := conn.Query(ctx, `SELECT row_to_json(stored)::text FROM `+table+` stored`)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var row string
					server.Raise(result.Scan(&row))
					rows = append(rows, row)
				}
			})
		}
	})
	return strings.Join(rows, "\n")
}

// A binary that does not link MG5 takes no report.
func TestAcceptAbuseReportUnlinked(t *testing.T) {
	if server.SidecarMg5Linked() {
		t.Skip("MG5 is linked: this case runs without -tags sidecar_pending")
	}
	result, err := AcceptAbuseReport(context.Background(), &model.AbuseReport{
		Category:      model.AbuseCategoryPhishing,
		ReporterClass: model.AbuseReporterClassPublic,
		Url:           "https://shop.quietnet.root.example/",
	})
	if !errors.Is(err, model.ErrAbuseUnlinked) || result != nil {
		t.Fatalf("%+v, %v", result, err)
	}
}

// A Web Risk hit on a host opens a case and kills the host at once, through the
// action path: recorded, applied, noticed and mailed. The same listing again
// counts on the case and kills nothing twice. A hit on a second host of the
// network climbs the ladder to the label kill and the halt, and a third to the
// suspension.
func TestAcceptAbuseReportWebRiskHitTakesTheLaddersRung(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, mailer := newTestAbuseDeps(t)
		newTestAbuseIntake(t)
		networkId, zone := newTestAbuseIntakeNetwork(t, ctx)
		hit := func(host string) *model.AbuseReport {
			return &model.AbuseReport{
				Category:      model.AbuseCategoryPhishing,
				ReporterClass: model.AbuseReporterClassWebRisk,
				Url:           "https://" + host + "/login",
			}
		}

		before := server.NowUtc()
		first := testAcceptAbuseReport(t, ctx, hit("shop."+zone))
		if !first.Joined || !first.Opened || first.Signal != model.AbuseSignalWebRiskHit || testAbuseIntakeKinds(first) != "host_kill" {
			t.Fatalf("the first hit: %+v, kinds %s", first, testAbuseIntakeKinds(first))
		}
		kill := first.Actions[0]
		if !kill.Inserted || !kill.Applied || !kill.Mailed || kill.Action.CaseId != first.CaseId || kill.Action.Target != "shop."+zone ||
			kill.Action.NetworkId == nil || *kill.Action.NetworkId != networkId || kill.Action.Category != model.AbuseCategoryPhishing {
			t.Fatalf("the kill: %+v, action %+v", kill, kill.Action)
		}
		if applied := effects.applied(); len(applied) != 1 || applied[0].Action.ActionId != kill.Action.ActionId || applied[0].Barred {
			t.Fatalf("effects %+v", applied)
		}
		if len(mailer.mailed()) != 1 {
			t.Fatalf("%d records mailed", len(mailer.mailed()))
		}
		if userAuths, _ := testAbuseNotices(t, ctx, networkId); len(userAuths) != 1 {
			t.Fatalf("%d notices", len(userAuths))
		}
		abuseCase := model.GetAbuseCase(ctx, first.CaseId)
		if abuseCase == nil || abuseCase.ReporterClass != model.AbuseReporterClassWebRisk || abuseCase.JoinKind != model.AbuseJoinKindHost ||
			abuseCase.JoinKey != "shop."+zone || abuseCase.Target != "shop."+zone || abuseCase.ReviewDueTime == nil || abuseCase.ReviewDueTime.Before(before) {
			t.Fatalf("the case reads %+v", abuseCase)
		}

		// the same listing again
		again := testAcceptAbuseReport(t, ctx, hit("shop."+zone))
		if !again.Joined || again.Opened || again.CaseId != first.CaseId || len(again.Actions) != 0 {
			t.Fatalf("the repeat: %+v", again)
		}
		if len(effects.applied()) != 1 || testAbuseIntakeCaseCount(t, ctx) != 1 {
			t.Fatal("the repeat acted or opened a case")
		}

		// the second strike: a label kill and a halt, each naming the label
		second := testAcceptAbuseReport(t, ctx, hit("pay."+zone))
		if !second.Opened || second.CaseId == first.CaseId || testAbuseIntakeKinds(second) != "label_kill,mail_halt" {
			t.Fatalf("the second strike: %+v, kinds %s", second, testAbuseIntakeKinds(second))
		}
		for _, action := range second.Actions {
			if action.Action.Target != zone || action.Action.CaseId != second.CaseId {
				t.Fatalf("a second-strike action reads %+v", action.Action)
			}
		}

		// the third: the suspension
		third := testAcceptAbuseReport(t, ctx, hit("bank."+zone))
		if !third.Opened || testAbuseIntakeKinds(third) != "network_suspend" || third.Actions[0].Action.Target != zone {
			t.Fatalf("the third strike: %+v, kinds %s", third, testAbuseIntakeKinds(third))
		}
		// and under a suspension a further hit adds nothing
		fourth := testAcceptAbuseReport(t, ctx, hit("more."+zone))
		if !fourth.Opened || len(fourth.Actions) != 0 {
			t.Fatalf("a hit under the suspension: %+v, kinds %s", fourth, testAbuseIntakeKinds(fourth))
		}
	})
}

// Reporter classes separate a scanner from a stranger: whatever a public
// report, a role mail or a watch claims, it opens a case for review and no
// machine acts on it.
func TestAcceptAbuseReportFromAStrangerOnlyReachesReview(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, mailer := newTestAbuseDeps(t)
		newTestAbuseIntake(t)
		networkId, zone := newTestAbuseIntakeNetwork(t, ctx)
		for index, expected := range []struct {
			report *model.AbuseReport
			signal model.AbuseSignal
		}{
			{&model.AbuseReport{Category: model.AbuseCategoryPhishing, ReporterClass: model.AbuseReporterClassPublic, Url: "https://a." + zone + "/"}, model.AbuseSignalThreatReport},
			{&model.AbuseReport{Category: model.AbuseCategoryCsam, ReporterClass: model.AbuseReporterClassPublic, Url: "https://b." + zone + "/"}, model.AbuseSignalReport},
			{&model.AbuseReport{Category: model.AbuseCategoryMalware, ReporterClass: model.AbuseReporterClassRoleMail, Address: "someone@" + zone}, model.AbuseSignalReport},
			{&model.AbuseReport{Category: model.AbuseCategoryPhishing, ReporterClass: model.AbuseReporterClassCtWatch, Url: "https://c." + zone + "/"}, model.AbuseSignalThreatReport},
			{&model.AbuseReport{Category: model.AbuseCategoryMalware, ReporterClass: model.AbuseReporterClassSearchConsole, Url: "https://d." + zone + "/"}, model.AbuseSignalThreatReport},
			// a keyed flagger's report that names no url is a report like any other
			{&model.AbuseReport{Category: model.AbuseCategoryPhishing, ReporterClass: model.AbuseReporterClassTrustedFlagger, Address: "other@sales." + zone}, model.AbuseSignalReport},
			// and a feed hit that names no host has no host to kill
			{&model.AbuseReport{Category: model.AbuseCategoryMalware, ReporterClass: model.AbuseReporterClassFeed, Address: "feed@otter-1234." + zone}, model.AbuseSignalFeedHit},
		} {
			result := testAcceptAbuseReport(t, ctx, expected.report)
			if !result.Joined || !result.Opened || result.Signal != expected.signal || len(result.Actions) != 0 {
				t.Fatalf("report %d: %+v", index, result)
			}
			abuseCase := model.GetAbuseCase(ctx, result.CaseId)
			if abuseCase == nil || abuseCase.NetworkId == nil || *abuseCase.NetworkId != networkId || abuseCase.ReviewDueTime == nil ||
				abuseCase.Barred != (expected.report.Category == model.AbuseCategoryCsam) {
				t.Fatalf("report %d: the case reads %+v", index, abuseCase)
			}
		}
		if len(effects.applied()) != 0 || len(mailer.mailed()) != 0 {
			t.Fatal("a stranger's report acted")
		}
		if userAuths, _ := testAbuseNotices(t, ctx, networkId); len(userAuths) != 0 {
			t.Fatalf("a stranger's report sent %d notices", len(userAuths))
		}
	})
}

// A report that names no URnetwork host leaves no row, and the answer is the
// one a joined report gets: no error either way, so the form is no oracle for
// which names exist.
func TestAcceptAbuseReportUnjoinableLeavesNoRowAndTheSameAnswer(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, _ := newTestAbuseDeps(t)
		newTestAbuseIntake(t)
		_, zone := newTestAbuseIntakeNetwork(t, ctx)
		for _, report := range []*model.AbuseReport{
			{Category: model.AbuseCategoryPhishing, ReporterClass: model.AbuseReporterClassPublic, Url: "https://shop.nolabelholdsthis." + testAbuseIntakeRoot + "/"},
			{Category: model.AbuseCategoryPhishing, ReporterClass: model.AbuseReporterClassWebRisk, Url: "https://elsewhere.example/"},
			{Category: model.AbuseCategorySpam, ReporterClass: model.AbuseReporterClassPublic, Address: "someone@elsewhere.example"},
			{Category: model.AbuseCategoryMalware, ReporterClass: model.AbuseReporterClassPublic, ConnectionId: server.NewId().String()},
			{Category: model.AbuseCategoryOther, ReporterClass: model.AbuseReporterClassPublic},
			// the root itself is no network's
			{Category: model.AbuseCategoryPhishing, ReporterClass: model.AbuseReporterClassFeed, Url: "https://" + testAbuseIntakeRoot + "/"},
		} {
			result, err := AcceptAbuseReport(ctx, report)
			if err != nil || result == nil || result.Joined || len(result.Actions) != 0 {
				t.Fatalf("an unjoinable report: %+v, %v", result, err)
			}
		}
		if got := testAbuseIntakeCaseCount(t, ctx); got != 0 {
			t.Fatalf("an unjoinable report left %d cases", got)
		}
		if len(effects.applied()) != 0 {
			t.Fatal("an unjoinable report acted")
		}
		// its joined twin answers the same: a result and no error
		result, err := AcceptAbuseReport(ctx, &model.AbuseReport{Category: model.AbuseCategoryPhishing, ReporterClass: model.AbuseReporterClassPublic, Url: "https://shop." + zone + "/"})
		if err != nil || result == nil || !result.Joined {
			t.Fatalf("the joined twin: %+v, %v", result, err)
		}
	})
}

// A report may not claim a class that acts through its own route, nor a
// category §21.4 has no row for. A refusal leaves no row and names nothing of
// the report.
func TestAcceptAbuseReportRefusals(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, _ := newTestAbuseDeps(t)
		newTestAbuseIntake(t)
		_, zone := newTestAbuseIntakeNetwork(t, ctx)
		reports := []*model.AbuseReport{nil}
		for _, class := range []model.AbuseReporterClass{
			model.AbuseReporterClassRegistrar, model.AbuseReporterClassRegistry, model.AbuseReporterClassLawEnforcement, model.AbuseReporterClassStaff, "",
		} {
			reports = append(reports, &model.AbuseReport{Category: model.AbuseCategoryPhishing, ReporterClass: class, Url: "https://shop." + zone + "/"})
		}
		reports = append(reports, &model.AbuseReport{Category: "planted-category", ReporterClass: model.AbuseReporterClassWebRisk, Url: "https://shop." + zone + "/"})
		for _, report := range reports {
			result, err := AcceptAbuseReport(ctx, report)
			if !errors.Is(err, model.ErrAbuseReportInvalid) || result != nil {
				t.Fatalf("%+v: %+v, %v", report, result, err)
			}
			if strings.Contains(err.Error(), zone) || strings.Contains(err.Error(), "planted-category") {
				t.Fatalf("the refusal names the report: %v", err)
			}
		}
		if got := testAbuseIntakeCaseCount(t, ctx); got != 0 || len(effects.applied()) != 0 {
			t.Fatalf("a refusal left %d cases", got)
		}
	})
}

// A copyright notice about a host is forwarded to the network's Owners and
// Admins through the action path's notice, once a case, and kills nothing,
// whoever sent it: the rung past the forward is a person's. One that names no
// host, about evs content say, is forwarded the same way.
func TestAcceptAbuseReportCopyrightForwardsOnceAndKillsNothing(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, _ := newTestAbuseDeps(t)
		newTestAbuseIntake(t)
		networkId, zone := newTestAbuseIntakeNetwork(t, ctx)
		notice := func(class model.AbuseReporterClass, reportUrl string) *model.AbuseReport {
			return &model.AbuseReport{Category: model.AbuseCategoryCopyright, ReporterClass: class, Url: reportUrl}
		}

		first := testAcceptAbuseReport(t, ctx, notice(model.AbuseReporterClassPublic, "https://films."+zone+"/watch"))
		if !first.Opened || testAbuseIntakeKinds(first) != "notice_forward" || first.Actions[0].Action.Target != "films."+zone {
			t.Fatalf("the notice: %+v, kinds %s", first, testAbuseIntakeKinds(first))
		}
		userAuths, templates := testAbuseNotices(t, ctx, networkId)
		if len(userAuths) != 1 || len(templates) != 1 {
			t.Fatalf("%d notices", len(userAuths))
		}
		// the repeat counts on the case and mails no one again
		again := testAcceptAbuseReport(t, ctx, notice(model.AbuseReporterClassPublic, "https://films."+zone+"/other"))
		if again.Opened || again.CaseId != first.CaseId || len(again.Actions) != 0 {
			t.Fatalf("the repeat: %+v", again)
		}
		// a keyed flagger's url is §21.8's kill signal, and a copyright notice still only forwards
		flagged := testAcceptAbuseReport(t, ctx, notice(model.AbuseReporterClassTrustedFlagger, "https://music."+zone+"/"))
		if flagged.Signal != model.AbuseSignalTrustedFlaggerUrl || testAbuseIntakeKinds(flagged) != "notice_forward" {
			t.Fatalf("a flagger's notice: %+v, kinds %s", flagged, testAbuseIntakeKinds(flagged))
		}
		// the network's label names no host
		label := testAcceptAbuseReport(t, ctx, notice(model.AbuseReporterClassPublic, zone))
		if testAbuseIntakeKinds(label) != "notice_forward" || label.Actions[0].Action.Target != zone {
			t.Fatalf("a notice with no host: %+v, kinds %s", label, testAbuseIntakeKinds(label))
		}
		for _, applied := range effects.applied() {
			if applied.Action.Kind != model.AbuseActionKindNoticeForward {
				t.Fatalf("a copyright notice took a %s", applied.Action.Kind)
			}
		}
		if userAuths, _ := testAbuseNotices(t, ctx, networkId); len(userAuths) != 3 {
			t.Fatalf("%d notices for three cases", len(userAuths))
		}
	})
}

// Any other category on a kill signal takes §21.8's host kill and no more, once
// a host. A CSAM hit's case is barred, so its effect reveals nothing, and its
// suspension stays a person's. A platform name takes no action at all.
func TestAcceptAbuseReportKillSignalsOffTheThreatLadder(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, _ := newTestAbuseDeps(t)
		settings := newTestAbuseIntake(t)
		_, zone := newTestAbuseIntakeNetwork(t, ctx)
		hit := func(category model.AbuseCategory, host string) *model.AbuseReport {
			return &model.AbuseReport{Category: category, ReporterClass: model.AbuseReporterClassFeed, Url: "https://" + host + "/"}
		}

		spam := testAcceptAbuseReport(t, ctx, hit(model.AbuseCategorySpam, "ads."+zone))
		if spam.Signal != model.AbuseSignalFeedHit || testAbuseIntakeKinds(spam) != "host_kill" || spam.Actions[0].Action.Target != "ads."+zone {
			t.Fatalf("a spam listing: %+v, kinds %s", spam, testAbuseIntakeKinds(spam))
		}
		// another category against the host already killed adds nothing
		brand := testAcceptAbuseReport(t, ctx, hit(model.AbuseCategoryBrand, "ads."+zone))
		if !brand.Opened || len(brand.Actions) != 0 {
			t.Fatalf("a second listing of a killed host: %+v, kinds %s", brand, testAbuseIntakeKinds(brand))
		}

		csam := testAcceptAbuseReport(t, ctx, hit(model.AbuseCategoryCsam, "img."+zone))
		if testAbuseIntakeKinds(csam) != "host_kill" {
			t.Fatalf("a CSAM listing: %+v, kinds %s", csam, testAbuseIntakeKinds(csam))
		}
		applied := effects.applied()
		if last := applied[len(applied)-1]; last.Action.CaseId != csam.CaseId || !last.Barred {
			t.Fatalf("the CSAM kill's effect %+v", last)
		}
		if abuseCase := model.GetAbuseCase(ctx, csam.CaseId); abuseCase == nil || !abuseCase.Barred {
			t.Fatalf("the CSAM case reads %+v", abuseCase)
		}

		// an exposed panel's kill protects and strikes nothing: a phishing hit
		// after it is still the first strike
		panel := testAcceptAbuseReport(t, ctx, hit(model.AbuseCategoryExposedPanel, "admin."+zone))
		if testAbuseIntakeKinds(panel) != "host_kill" {
			t.Fatalf("an exposed panel: %+v, kinds %s", panel, testAbuseIntakeKinds(panel))
		}

		// a name the platform protects takes no action, though its case opens
		settings.PlatformNames = append(settings.PlatformNames, "status."+zone)
		protected := testAcceptAbuseReport(t, ctx, hit(model.AbuseCategoryPhishing, "status."+zone))
		if !protected.Opened || len(protected.Actions) != 0 {
			t.Fatalf("a protected name: %+v, kinds %s", protected, testAbuseIntakeKinds(protected))
		}
		// and with no protected names configured, nothing acts
		settings.PlatformNames = nil
		unconfigured := testAcceptAbuseReport(t, ctx, hit(model.AbuseCategoryPhishing, "shop."+zone))
		if !unconfigured.Opened || len(unconfigured.Actions) != 0 {
			t.Fatalf("a hit with no protected names: %+v, kinds %s", unconfigured, testAbuseIntakeKinds(unconfigured))
		}
	})
}

// A canary in a url's path and query, in an address's local part and in header
// text reaches no column the intake or the action path writes, no mailed
// record and no log line, on a hit that kills and on a report that does not.
func TestAcceptAbuseReportKeepsNoThirdPartyData(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, mailer := newTestAbuseDeps(t)
		newTestAbuseIntake(t)
		_, zone := newTestAbuseIntakeNetwork(t, ctx)
		canary := "canary7f3a9c"

		previousVerbosity := flag.Lookup("v").Value.String()
		flag.Set("v", "2")
		defer flag.Set("v", previousVerbosity)
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		realStderr := os.Stderr
		os.Stderr = write
		captured := make(chan []byte, 1)
		go func() {
			logged, _ := io.ReadAll(read)
			captured <- logged
		}()
		results := []*AbuseIntakeResult{}
		var acceptErr error
		for _, report := range []*model.AbuseReport{
			{Category: model.AbuseCategoryPhishing, ReporterClass: model.AbuseReporterClassWebRisk,
				Url: "https://" + canary + "@shop." + zone + "/" + canary + "?victim=" + canary + "#" + canary},
			{Category: model.AbuseCategorySpam, ReporterClass: model.AbuseReporterClassPublic, Address: canary + "@" + zone},
			{Category: model.AbuseCategoryMalware, ReporterClass: model.AbuseReporterClassPublic,
				HeaderBlock: []byte("From: " + canary + " <" + canary + "@" + zone + ">\r\nSubject: " + canary + "\r\nReply-To: " + canary + "@reporter.example\r\n\r\n" + canary + "\r\n")},
			// one that joins nothing
			{Category: model.AbuseCategoryPhishing, ReporterClass: model.AbuseReporterClassPublic, Url: "https://" + canary + ".elsewhere.example/" + canary},
		} {
			result, err := AcceptAbuseReport(ctx, report)
			if err != nil {
				acceptErr = err
				break
			}
			results = append(results, result)
		}
		// the capture's own proof that it reads what glog writes
		glog.Infof("[test]abuse intake capture marker\n")
		glog.Flush()
		os.Stderr = realStderr
		write.Close()
		logged := string(<-captured)

		if acceptErr != nil {
			t.Fatal(acceptErr)
		}
		if !strings.Contains(logged, "abuse intake capture marker") {
			t.Fatal("the capture read nothing glog wrote")
		}
		if len(results) != 4 || testAbuseIntakeKinds(results[0]) != "host_kill" || !results[1].Joined || !results[2].Joined || results[3].Joined {
			t.Fatalf("results %+v", results)
		}
		if strings.Contains(logged, canary) {
			t.Fatal("a log line holds third-party data")
		}
		if rows := testAbuseIntakeRowsText(t, ctx); strings.Contains(rows, canary) || strings.Contains(rows, "reporter.example") {
			t.Fatalf("a row holds third-party data: %s", rows)
		}
		for _, body := range mailer.mailed() {
			if strings.Contains(string(body), canary) {
				t.Fatal("a mailed record holds third-party data")
			}
		}
	})
}

// An effect that fails leaves its action owed and the case open on its clock:
// the report is still taken.
func TestAcceptAbuseReportWithAFailedEffectStillOpensTheCase(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, _ := newTestAbuseDeps(t)
		effects.err = errors.New("the edge did not answer")
		newTestAbuseIntake(t)
		_, zone := newTestAbuseIntakeNetwork(t, ctx)
		result := testAcceptAbuseReport(t, ctx, &model.AbuseReport{
			Category:      model.AbuseCategoryMalware,
			ReporterClass: model.AbuseReporterClassFeed,
			Url:           "https://files." + zone + "/x",
		})
		if !result.Opened || testAbuseIntakeKinds(result) != "host_kill" || result.Actions[0].Applied {
			t.Fatalf("%+v, kinds %s", result, testAbuseIntakeKinds(result))
		}
		owed := model.GetAbuseCaseActions(ctx, result.CaseId)
		if len(owed) != 1 || owed[0].AppliedTime != nil {
			t.Fatalf("the owed action reads %+v", owed)
		}
		if abuseCase := model.GetAbuseCase(ctx, result.CaseId); abuseCase == nil || abuseCase.State != model.AbuseCaseStateOpen {
			t.Fatalf("the case reads %+v", abuseCase)
		}
	})
}

// The intake reads its clock through its seam, so a connection id joins by the
// report's time and not the test's.
func TestAcceptAbuseReportJoinsAConnectionByItsClock(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestAbuseDeps(t)
		newTestAbuseIntake(t)
		networkId, zone := newTestAbuseIntakeNetwork(t, ctx)
		endTime := server.NowUtc().Truncate(time.Second)
		record := &model.TlsConnectionRecord{
			ConnectionId: server.NewId(),
			NetworkId:    networkId,
			Host:         "www",
			StartTime:    endTime.Add(-time.Minute),
			EndTime:      endTime,
			ByteCount:    4096,
			VisitorHash:  []byte("0123456789abcdef"),
		}
		if _, err := model.Testing_WriteTlsConnectionRecordsAt(ctx, []*model.TlsConnectionRecord{record}, endTime); err != nil {
			t.Fatal(err)
		}
		now := endTime
		previous := abuseIntakeDeps.now
		abuseIntakeDeps.now = func() time.Time {
			return now
		}
		defer func() {
			abuseIntakeDeps.now = previous
		}()
		report := &model.AbuseReport{Category: model.AbuseCategoryMalware, ReporterClass: model.AbuseReporterClassPublic, ConnectionId: record.ConnectionId.String()}

		now = endTime.Add(25 * time.Hour)
		if late := testAcceptAbuseReport(t, ctx, report); late.Joined {
			t.Fatalf("a report at 25 h joined: %+v", late)
		}
		now = endTime.Add(23 * time.Hour)
		joined := testAcceptAbuseReport(t, ctx, report)
		if !joined.Joined || !joined.Opened || len(joined.Actions) != 0 {
			t.Fatalf("a report at 23 h: %+v", joined)
		}
		abuseCase := model.GetAbuseCase(ctx, joined.CaseId)
		if abuseCase == nil || abuseCase.JoinKind != model.AbuseJoinKindConnection || abuseCase.JoinKey != record.ConnectionId.String() ||
			abuseCase.Target != "www."+zone || abuseCase.TargetKind != model.AbuseTargetKindHost {
			t.Fatalf("the case reads %+v", abuseCase)
		}
	})
}
