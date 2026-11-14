//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// §21.8's joins on §12's real label map and MG5's tables (abuse_join_model.go).
// Names are .example and every canary is a string no fixture needs.

const testAbuseJoinRoot = "root.example"

// §21's settings with the test root, and a platform name so the ladders act.
func testAbuseJoinSettings() *AbuseSettings {
	settings := DefaultAbuseSettings()
	settings.RootNames = []string{testAbuseJoinRoot}
	settings.PlatformNames = []string{"platform.example"}
	return settings
}

// A network with a label in the test root.
func testAbuseJoinNetwork(t testing.TB, ctx context.Context) (networkId server.Id, label string) {
	t.Helper()
	networkId = server.NewId()
	label, err := EnsureNetworkLabel(ctx, networkId, testAbuseJoinRoot)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// Joins report at now, failing on a store error.
func testJoinAbuseReport(t testing.TB, ctx context.Context, report *AbuseReport, now time.Time) *AbuseReportJoin {
	t.Helper()
	join, err := JoinAbuseReport(ctx, report, now, testAbuseJoinSettings())
	if err != nil {
		t.Fatal(err)
	}
	return join
}

// Every abuse_case and abuse_action row as text, for a canary search.
func testAbuseRowsText(t testing.TB, ctx context.Context) string {
	t.Helper()
	rows := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, table := range []string{"abuse_case", "abuse_action", "network_abuse_state"} {
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

// Of a url only the host is read: scheme, userinfo, port, path, query and
// fragment are dropped, a defanged form reads as its name, and anything that is
// no host name names nothing.
func TestAbuseReportUrlHostDropsEverythingButTheHost(t *testing.T) {
	settings := testAbuseJoinSettings()
	for reportUrl, host := range map[string]string{
		"https://shop.quietnet.root.example/a/b?c=d#e":          "shop.quietnet.root.example",
		"HTTP://User:Secret@Shop.Quietnet.Root.Example.:8443/x": "shop.quietnet.root.example",
		"shop.quietnet.root.example":                            "shop.quietnet.root.example",
		" shop.quietnet.root.example/path ":                     "shop.quietnet.root.example",
		"hxxps://shop[.]quietnet[.]root[.]example/login":        "shop.quietnet.root.example",
		"quietnet.root.example?next=https://other.example/":     "quietnet.root.example",
		"https://192.0.2.7/x":                                   "",
		"https://[2001:db8::1]/x":                               "",
		"https://localhost/x":                                   "",
		"https://shop_1.quietnet.root.example/":                 "",
		"https://-shop.quietnet.root.example/":                  "",
		"https:///path-only":                                    "",
		"":                                                      "",
	} {
		if got := abuseReportUrlHost(reportUrl, settings); got != host {
			t.Errorf("%q reads %q, not %q", reportUrl, got, host)
		}
	}
	// a url past the bound is dropped whole, never cut to a shorter name
	long := "https://shop.quietnet.root.example/" + strings.Repeat("a", settings.ReportUrlMaxByteCount)
	if got := abuseReportUrlHost(long, settings); got != "" {
		t.Fatalf("a url past the bound reads %q", got)
	}
}

// A report claims only the classes a report may: a registrar, a registry, law
// enforcement and staff act through their own routes.
func TestAbuseReportValidate(t *testing.T) {
	for _, class := range []AbuseReporterClass{
		AbuseReporterClassPublic, AbuseReporterClassRoleMail, AbuseReporterClassMailReport, AbuseReporterClassWebRisk,
		AbuseReporterClassFeed, AbuseReporterClassCtWatch, AbuseReporterClassSearchConsole, AbuseReporterClassTrustedFlagger,
	} {
		if err := (&AbuseReport{Category: AbuseCategoryPhishing, ReporterClass: class}).Validate(); err != nil {
			t.Errorf("%s: %v", class, err)
		}
	}
	for _, class := range []AbuseReporterClass{
		AbuseReporterClassRegistrar, AbuseReporterClassRegistry, AbuseReporterClassLawEnforcement, AbuseReporterClassStaff, "", "anyone",
	} {
		if err := (&AbuseReport{Category: AbuseCategoryPhishing, ReporterClass: class}).Validate(); !errors.Is(err, ErrAbuseReportInvalid) {
			t.Errorf("%q: %v", class, err)
		}
	}
	if err := (&AbuseReport{Category: "planted-category-value", ReporterClass: AbuseReporterClassPublic}).Validate(); !errors.Is(err, ErrAbuseReportInvalid) ||
		strings.Contains(err.Error(), "planted-category-value") {
		t.Fatalf("an unknown category: %v", err)
	}
	var none *AbuseReport
	if err := none.Validate(); !errors.Is(err, ErrAbuseReportInvalid) {
		t.Fatalf("no report: %v", err)
	}
}

// A url's host joins through the label map: a host under the network's label
// names the host, the apex names the label, and the miss twin, a label nothing
// holds, joins nothing, as does a name under no root and the root itself.
func TestJoinAbuseReportByHost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkId, label := testAbuseJoinNetwork(t, ctx)
		report := func(reportUrl string) *AbuseReport {
			return &AbuseReport{Category: AbuseCategoryPhishing, ReporterClass: AbuseReporterClassPublic, Url: reportUrl}
		}

		host := "shop." + label + "." + testAbuseJoinRoot
		join := testJoinAbuseReport(t, ctx, report("https://Shop."+strings.ToUpper(label)+".Root.Example.:8443/login?session=1"), now)
		if join == nil || join.NetworkId != networkId || join.JoinKind != AbuseJoinKindHost || join.JoinKey != host ||
			join.Target != (AbuseTarget{Kind: AbuseTargetKindHost, Name: host}) || join.LabelName != label+"."+testAbuseJoinRoot || !join.NamesUrl {
			t.Fatalf("a host's join %+v", join)
		}
		deep := testJoinAbuseReport(t, ctx, report("a.b."+host), now)
		if deep == nil || deep.NetworkId != networkId || deep.Target.Name != "a.b."+host || deep.Target.Kind != AbuseTargetKindHost {
			t.Fatalf("a deeper host's join %+v", deep)
		}
		apex := testJoinAbuseReport(t, ctx, report(label+"."+testAbuseJoinRoot), now)
		if apex == nil || apex.NetworkId != networkId || apex.Target != (AbuseTarget{Kind: AbuseTargetKindLabel, Name: label + "." + testAbuseJoinRoot}) {
			t.Fatalf("the apex's join %+v", apex)
		}

		for _, miss := range []string{
			"https://shop.nolabelholdsthis." + testAbuseJoinRoot + "/login",
			"https://shop." + label + ".other.example/login",
			"https://" + testAbuseJoinRoot + "/",
			"https://" + label + "/",
			"",
		} {
			if join := testJoinAbuseReport(t, ctx, report(miss), now); join != nil {
				t.Fatalf("%q joined %+v", miss, join)
			}
		}
		// with no roots configured no name is a URnetwork name
		settings := testAbuseJoinSettings()
		settings.RootNames = nil
		if join, err := JoinAbuseReport(ctx, report("https://"+host+"/"), now, settings); err != nil || join != nil {
			t.Fatalf("a join with no roots: %+v, %v", join, err)
		}
	})
}

// A mail address joins by its domain's <net>, a burner's and a scope's
// included, and keeps the domain only. The miss twin is an address at a label
// nothing holds.
func TestJoinAbuseReportByAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkId, label := testAbuseJoinNetwork(t, ctx)
		report := func(address string) *AbuseReport {
			return &AbuseReport{Category: AbuseCategorySpam, ReporterClass: AbuseReporterClassPublic, Address: address}
		}

		zone := label + "." + testAbuseJoinRoot
		for address, domain := range map[string]string{
			"canary-local-part@" + zone:                  zone,
			"Canary-Local-Part@" + strings.ToUpper(zone): zone,
			"canary-local-part@otter-1234." + zone:       "otter-1234." + zone,
			"canary-local-part@sales." + zone:            "sales." + zone,
		} {
			join := testJoinAbuseReport(t, ctx, report(address), now)
			if join == nil || join.NetworkId != networkId || join.JoinKind != AbuseJoinKindAddress || join.JoinKey != domain ||
				join.Target != (AbuseTarget{Kind: AbuseTargetKindSendingDomain, Name: domain}) || join.LabelName != zone || join.NamesUrl {
				t.Fatalf("%s: the join %+v", domain, join)
			}
			if strings.Contains(join.JoinKey+join.Target.Name, "canary") {
				t.Fatal("a join kept the local part")
			}
		}
		for _, miss := range []string{
			"someone@nolabelholdsthis." + testAbuseJoinRoot,
			"someone@" + label + ".other.example",
			"someone@" + testAbuseJoinRoot,
			"no-at-sign." + zone,
		} {
			if join := testJoinAbuseReport(t, ctx, report(miss), now); join != nil {
				t.Fatalf("%q joined %+v", miss, join)
			}
		}
	})
}

// A header block joins by a DKIM d= or From domain under a root, read by
// §14.8's header-only parse: a bare-LF paste reads the same, and nothing past
// the block's blank line is read, so a body naming a network joins nothing.
func TestJoinAbuseReportByHeaders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkId, label := testAbuseJoinNetwork(t, ctx)
		otherNetworkId, otherLabel := testAbuseJoinNetwork(t, ctx)
		zone := label + "." + testAbuseJoinRoot
		otherZone := otherLabel + "." + testAbuseJoinRoot
		report := func(block string) *AbuseReport {
			return &AbuseReport{Category: AbuseCategorySpam, ReporterClass: AbuseReporterClassPublic, HeaderBlock: []byte(block)}
		}

		fromBlock := "From: Canary Display <canary-sender@" + zone + ">\r\nTo: victim@receiver.example\r\nSubject: canary subject text\r\n" +
			"Message-ID: <m1@" + zone + ">\r\n\r\ncanary body from someone@" + otherZone + "\r\n"
		join := testJoinAbuseReport(t, ctx, report(fromBlock), now)
		if join == nil || join.NetworkId != networkId || join.JoinKind != AbuseJoinKindHeaders || join.JoinKey != zone ||
			join.Target != (AbuseTarget{Kind: AbuseTargetKindSendingDomain, Name: zone}) || join.NamesUrl {
			t.Fatalf("a From join %+v", join)
		}
		// a pasted block with bare line feeds
		if join := testJoinAbuseReport(t, ctx, report(strings.ReplaceAll(fromBlock, "\r\n", "\n")), now); join == nil || join.NetworkId != networkId {
			t.Fatalf("a bare-LF block's join %+v", join)
		}
		// the signature's d= names the network before the From does
		dkimBlock := "DKIM-Signature: v=1; a=ed25519-sha256; d=" + otherZone + "; s=abcdefghijklmnopqrstuvwxyz; h=from; bh=AAAA; b=AAAA\r\n" +
			"From: Sender <sender@" + zone + ">\r\n\r\n"
		if join := testJoinAbuseReport(t, ctx, report(dkimBlock), now); join == nil || join.NetworkId != otherNetworkId || join.JoinKey != otherZone {
			t.Fatalf("a DKIM join %+v", join)
		}

		// the miss twins: headers that name no network, though the body does;
		// a label nothing holds; and bytes that are no header block
		for _, miss := range []string{
			"From: Sender <sender@elsewhere.example>\r\nSubject: s\r\n\r\nFrom: Sender <sender@" + zone + ">\r\nsee https://shop." + zone + "/\r\n",
			"From: Sender <sender@nolabelholdsthis." + testAbuseJoinRoot + ">\r\n\r\n",
			"no header here " + zone,
		} {
			if join := testJoinAbuseReport(t, ctx, report(miss), now); join != nil {
				t.Fatalf("%q joined %+v", miss, join)
			}
		}
	})
}

// An edge connection id joins its record for the join window after the
// connection ended: at 23 h it joins, naming the record's host under the
// network's label, and at 25 h it joins nothing. The clock is the join's own.
func TestJoinAbuseReportByConnectionWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, label := testAbuseJoinNetwork(t, ctx)
		unlabeledNetworkId := server.NewId()
		endTime := server.NowUtc().Truncate(time.Second)
		record := tlsTestRecord(networkId, endTime)
		apexRecord := tlsTestRecord(networkId, endTime)
		apexRecord.Host = "@"
		unlabeledRecord := tlsTestRecord(unlabeledNetworkId, endTime)
		result, err := writeTlsConnectionRecordsAt(ctx, []*TlsConnectionRecord{record, apexRecord, unlabeledRecord}, endTime, DefaultTlsConnectionSettings())
		if err != nil || result == nil {
			t.Fatalf("write: %+v, %v", result, err)
		}
		if tlsTestRecordCount(ctx, networkId) != 2 {
			t.Fatal("the records were not stored")
		}
		report := func(connectionId string) *AbuseReport {
			return &AbuseReport{Category: AbuseCategoryMalware, ReporterClass: AbuseReporterClassPublic, ConnectionId: connectionId}
		}

		zone := label + "." + testAbuseJoinRoot
		join := testJoinAbuseReport(t, ctx, report(" "+record.ConnectionId.String()+" "), endTime.Add(23*time.Hour))
		if join == nil || join.NetworkId != networkId || join.JoinKind != AbuseJoinKindConnection || join.JoinKey != record.ConnectionId.String() ||
			join.Target != (AbuseTarget{Kind: AbuseTargetKindHost, Name: "www." + zone}) || join.LabelName != zone || join.NamesUrl {
			t.Fatalf("the join at 23 h %+v", join)
		}
		if join := testJoinAbuseReport(t, ctx, report(record.ConnectionId.String()), endTime.Add(25*time.Hour)); join != nil {
			t.Fatalf("the join at 25 h %+v", join)
		}
		// the window's own edge: inside by a second, and out at the bound
		window := testAbuseJoinSettings().ConnectionJoinWindow
		if join := testJoinAbuseReport(t, ctx, report(record.ConnectionId.String()), endTime.Add(window-time.Second)); join == nil {
			t.Fatal("no join a second inside the window")
		}
		if join := testJoinAbuseReport(t, ctx, report(record.ConnectionId.String()), endTime.Add(window)); join != nil {
			t.Fatal("a join at the window's end")
		}

		apex := testJoinAbuseReport(t, ctx, report(apexRecord.ConnectionId.String()), endTime.Add(time.Hour))
		if apex == nil || apex.Target != (AbuseTarget{Kind: AbuseTargetKindLabel, Name: zone}) {
			t.Fatalf("the apex record's join %+v", apex)
		}
		// a network with no label is named by its id
		unlabeled := testJoinAbuseReport(t, ctx, report(unlabeledRecord.ConnectionId.String()), endTime.Add(time.Hour))
		if unlabeled == nil || unlabeled.NetworkId != unlabeledNetworkId ||
			unlabeled.Target != (AbuseTarget{Kind: AbuseTargetKindNetwork, Name: unlabeledNetworkId.String()}) || unlabeled.LabelName != "" {
			t.Fatalf("an unlabeled network's join %+v", unlabeled)
		}

		// the miss twins: an id no record holds, and text that is no id
		for _, miss := range []string{server.NewId().String(), "not-a-connection-id", ""} {
			if join := testJoinAbuseReport(t, ctx, report(miss), endTime.Add(time.Hour)); join != nil {
				t.Fatalf("%q joined %+v", miss, join)
			}
		}
		// a window of nothing joins nothing
		settings := testAbuseJoinSettings()
		settings.ConnectionJoinWindow = 0
		if join, err := JoinAbuseReport(ctx, report(record.ConnectionId.String()), endTime.Add(time.Hour), settings); err != nil || join != nil {
			t.Fatalf("a join with no window: %+v, %v", join, err)
		}
	})
}

// The joins run in §21.8's order: a host before an address, an address before
// headers, headers before a connection.
func TestJoinAbuseReportOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		hostNetworkId, hostLabel := testAbuseJoinNetwork(t, ctx)
		addressNetworkId, addressLabel := testAbuseJoinNetwork(t, ctx)
		headerNetworkId, headerLabel := testAbuseJoinNetwork(t, ctx)
		connectionNetworkId, _ := testAbuseJoinNetwork(t, ctx)
		record := tlsTestRecord(connectionNetworkId, now)
		if _, err := writeTlsConnectionRecordsAt(ctx, []*TlsConnectionRecord{record}, now, DefaultTlsConnectionSettings()); err != nil {
			t.Fatal(err)
		}
		report := &AbuseReport{
			Category:      AbuseCategoryPhishing,
			ReporterClass: AbuseReporterClassPublic,
			Url:           "https://shop." + hostLabel + "." + testAbuseJoinRoot + "/",
			Address:       "someone@" + addressLabel + "." + testAbuseJoinRoot,
			HeaderBlock:   []byte("From: Sender <sender@" + headerLabel + "." + testAbuseJoinRoot + ">\r\n\r\n"),
			ConnectionId:  record.ConnectionId.String(),
		}
		for _, expected := range []struct {
			networkId server.Id
			joinKind  AbuseJoinKind
			drop      func()
		}{
			{hostNetworkId, AbuseJoinKindHost, func() { report.Url = "https://shop.nolabelholdsthis." + testAbuseJoinRoot + "/" }},
			{addressNetworkId, AbuseJoinKindAddress, func() { report.Address = "" }},
			{headerNetworkId, AbuseJoinKindHeaders, func() { report.HeaderBlock = nil }},
			{connectionNetworkId, AbuseJoinKindConnection, func() { report.ConnectionId = "" }},
		} {
			join := testJoinAbuseReport(t, ctx, report, now.Add(time.Minute))
			if join == nil || join.NetworkId != expected.networkId || join.JoinKind != expected.joinKind {
				t.Fatalf("expected a %s join, got %+v", expected.joinKind, join)
			}
			expected.drop()
		}
		if join := testJoinAbuseReport(t, ctx, report, now.Add(time.Minute)); join != nil {
			t.Fatalf("a report with nothing left joined %+v", join)
		}
	})
}

// A joined report opens one case on its clock, and a duplicate inside the
// coalescing window counts on it. A CSAM case is barred. The review clock reads
// the installed standing source, and the short clock with none.
func TestOpenAbuseReportCaseCoalescesDuplicates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		settings := testAbuseJoinSettings()
		networkId, label := testAbuseJoinNetwork(t, ctx)
		report := &AbuseReport{Category: AbuseCategoryOther, ReporterClass: AbuseReporterClassPublic, Url: "https://shop." + label + "." + testAbuseJoinRoot + "/x"}
		join := testJoinAbuseReport(t, ctx, report, now)
		signal := AbuseSignalOfReport(report.ReporterClass, report.Category, join.NamesUrl)

		first, err := OpenAbuseReportCase(ctx, report, join, signal, now, settings)
		if err != nil || !first.Opened || first.Barred || first.Signal != AbuseSignalReport || !now.Before(first.ReviewDueTime) {
			t.Fatalf("the first report: %+v, %v", first, err)
		}
		second, err := OpenAbuseReportCase(ctx, report, join, signal, now.Add(time.Hour), settings)
		if err != nil || second.Opened || second.CaseId != first.CaseId {
			t.Fatalf("a duplicate: %+v, %v", second, err)
		}
		if got := testAbuseCaseCount(t, ctx); got != 1 {
			t.Fatalf("%d cases", got)
		}
		abuseCase := GetAbuseCase(ctx, first.CaseId)
		if abuseCase == nil || abuseCase.NetworkId == nil || *abuseCase.NetworkId != networkId || abuseCase.Category != AbuseCategoryOther ||
			abuseCase.TargetKind != AbuseTargetKindHost || abuseCase.Target != join.Target.Name || abuseCase.JoinKind != AbuseJoinKindHost ||
			abuseCase.JoinKey != join.JoinKey || abuseCase.ReporterClass != AbuseReporterClassPublic || abuseCase.Barred || abuseCase.ReviewDueTime == nil {
			t.Fatalf("the case reads %+v", abuseCase)
		}

		// a known paid standing reviews no earlier than an unknown one
		restore := SetAbuseReportStandingSource(func(ctx context.Context, sourceNetworkId server.Id) (AbuseTier, AbuseStanding) {
			if sourceNetworkId != networkId {
				t.Errorf("the standing of another network was read")
			}
			return AbuseTierTeam, AbuseStandingTrusted
		})
		otherReport := &AbuseReport{Category: AbuseCategoryBrand, ReporterClass: AbuseReporterClassPublic, Url: report.Url}
		known, err := OpenAbuseReportCase(ctx, otherReport, join, signal, now, settings)
		restore()
		if err != nil || !known.Opened || known.ReviewDueTime.Before(first.ReviewDueTime) {
			t.Fatalf("a known standing: %+v, %v", known, err)
		}

		csam := &AbuseReport{Category: AbuseCategoryCsam, ReporterClass: AbuseReporterClassPublic, Url: report.Url}
		barred, err := OpenAbuseReportCase(ctx, csam, join, signal, now, settings)
		if err != nil || !barred.Barred {
			t.Fatalf("a CSAM report: %+v, %v", barred, err)
		}
		if abuseCase := GetAbuseCase(ctx, barred.CaseId); abuseCase == nil || !abuseCase.Barred {
			t.Fatalf("the CSAM case reads %+v", abuseCase)
		}

		// refusals leave no row
		before := testAbuseCaseCount(t, ctx)
		if _, err := OpenAbuseReportCase(ctx, report, nil, signal, now, settings); !errors.Is(err, ErrAbuseReportInvalid) {
			t.Fatalf("no join: %v", err)
		}
		staff := &AbuseReport{Category: AbuseCategoryOther, ReporterClass: AbuseReporterClassStaff, Url: report.Url}
		if _, err := OpenAbuseReportCase(ctx, staff, join, signal, now, settings); !errors.Is(err, ErrAbuseReportInvalid) {
			t.Fatalf("a staff report: %v", err)
		}
		if got := testAbuseCaseCount(t, ctx); got != before {
			t.Fatalf("a refusal left a row: %d cases", got)
		}
	})
}

// Third-party data dies at the join: a canary in a url's userinfo, path, query
// and fragment, in an address's local part and in header text reaches no
// column, whichever join ran.
func TestAbuseJoinKeepsNoThirdPartyData(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		settings := testAbuseJoinSettings()
		_, label := testAbuseJoinNetwork(t, ctx)
		zone := label + "." + testAbuseJoinRoot
		canary := "canary7f3a9c"
		reports := []*AbuseReport{
			{Category: AbuseCategoryPhishing, ReporterClass: AbuseReporterClassPublic,
				Url: "https://" + canary + ":" + canary + "@shop." + zone + "/" + canary + "/login?victim=" + canary + "#" + canary},
			{Category: AbuseCategorySpam, ReporterClass: AbuseReporterClassPublic, Address: canary + "@" + zone},
			{Category: AbuseCategoryMalware, ReporterClass: AbuseReporterClassPublic,
				HeaderBlock: []byte("From: " + canary + " <" + canary + "@" + zone + ">\r\nTo: " + canary + "@receiver.example\r\nSubject: " + canary +
					"\r\nReply-To: " + canary + "@reporter.example\r\n\r\n" + canary + "\r\n")},
		}
		for _, report := range reports {
			join := testJoinAbuseReport(t, ctx, report, now)
			if join == nil {
				t.Fatalf("a report did not join: %+v", report.Category)
			}
			if _, err := OpenAbuseReportCase(ctx, report, join, AbuseSignalOfReport(report.ReporterClass, report.Category, join.NamesUrl), now, settings); err != nil {
				t.Fatal(err)
			}
		}
		if got := testAbuseCaseCount(t, ctx); got != len(reports) {
			t.Fatalf("%d cases", got)
		}
		if rows := testAbuseRowsText(t, ctx); strings.Contains(rows, canary) {
			t.Fatalf("a row holds third-party data: %s", rows)
		}
	})
}

// A network's recorded actions read as the ladders take them, oldest first,
// and another network's are not among them.
func TestGetNetworkAbuseHistory(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		networkId := server.NewId()
		caseId, _ := testOpenAbuseCase(t, ctx, testAbuseCaseArgs(networkId, "shop.root.example"), now)
		otherCaseId, _ := testOpenAbuseCase(t, ctx, testAbuseCaseArgs(server.NewId(), "shop.root.example"), now)
		until := now.Add(48 * time.Hour)
		for index, args := range []*AbuseActionArgs{
			{ActionId: server.NewId(), CaseId: caseId, Kind: AbuseActionKindHostKill, Target: "shop.root.example"},
			{ActionId: server.NewId(), CaseId: caseId, Kind: AbuseActionKindHostSuspend, Target: "shop.root.example", UntilTime: &until},
			{ActionId: server.NewId(), CaseId: otherCaseId, Kind: AbuseActionKindHostKill, Target: "shop.root.example"},
		} {
			if _, _, err := testRecordAbuseAction(t, ctx, args, now.Add(time.Duration(index)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		history := GetNetworkAbuseHistory(ctx, networkId)
		if len(history) != 2 {
			t.Fatalf("%d actions", len(history))
		}
		if history[0].Kind != AbuseActionKindHostKill || history[0].CaseId != caseId || history[0].Category != AbuseCategoryPhishing ||
			history[0].Target != "shop.root.example" || !history[0].CreateTime.Equal(now) || !history[0].UntilTime.IsZero() {
			t.Fatalf("the first action %+v", history[0])
		}
		if history[1].Kind != AbuseActionKindHostSuspend || !history[1].UntilTime.Equal(until) {
			t.Fatalf("the second action %+v", history[1])
		}
		if got := GetNetworkAbuseHistory(ctx, server.NewId()); len(got) != 0 {
			t.Fatalf("a network with no action has %d", len(got))
		}
	})
}
