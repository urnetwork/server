package model

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
)

// The scanners' store (abuse_scan_model.go): the hourly check's names paged by cursor over published hosts and user
// records, a lift as wrong found by its host, a hit on a root opened as one incident, and a feed's lines read as
// urls. The pure cases run in every build; the store cases need MG5 (-tags sidecar_pending) and skip without it.
// Names are .example.

const testAbuseScanRoot = "root.example"

// Skips a store case in a build that does not link MG5.
func requireAbuseScanTables(t testing.TB) {
	t.Helper()
	if !server.SidecarMg5Linked() || !server.SidecarMg2Linked() {
		t.Skip("MG2 and MG5 are pending: these cases run with -tags sidecar_pending")
	}
}

func TestAbuseScanHostReadsALineAsAUrl(t *testing.T) {
	settings := DefaultAbuseSettings()
	cases := []struct {
		line string
		want string
	}{
		{line: "https://shop.acme.root.example/login?next=/account", want: "shop.acme.root.example"},
		{line: "  hxxp://evil[.]acme.root.example/a/b  ", want: "evil.acme.root.example"},
		{line: "http://someone@Host.Acme.Root.Example:8443/path", want: "host.acme.root.example"},
		{line: "shop.acme.root.example\t2026-11-15 phishing", want: "shop.acme.root.example"},
		{line: "shop.acme.root.example.", want: "shop.acme.root.example"},
		{line: "# a comment naming shop.acme.root.example", want: ""},
		{line: "", want: ""},
		{line: "192.0.2.1", want: ""},
		{line: "http://[2001:db8::1]/", want: ""},
		{line: "under_score.root.example", want: ""},
	}
	for _, c := range cases {
		if got := AbuseScanHost(c.line, settings); got != c.want {
			t.Errorf("%q: %q, want %q", c.line, got, c.want)
		}
	}
}

func TestAbuseScanRootNames(t *testing.T) {
	roots := []string{"Root.Example.", "other.example"}
	under := map[string]bool{
		"shop.acme.root.example": true,
		"acme.root.example":      true,
		"root.example":           false,
		"example":                false,
		"notroot.example":        false,
		"shop.other.example":     true,
	}
	for name, want := range under {
		if got := AbuseScanUnderRoot(name, roots); got != want {
			t.Errorf("under %s: %t, want %t", name, got, want)
		}
	}
	covered := map[string]string{
		"root.example":      "root.example",
		"ROOT.example.":     "root.example",
		"example":           "root.example",
		"acme.root.example": "",
		"notroot.example":   "",
		"":                  "",
	}
	for name, want := range covered {
		if got, ok := AbuseScanCoveredRoot(name, roots); got != want || ok != (want != "") {
			t.Errorf("covered %q: %q, %t, want %q", name, got, ok, want)
		}
	}
	if AbuseScanUnderRoot("shop.acme.root.example", nil) {
		t.Error("a name under no roots at all")
	}
}

func TestAbuseScanLookupNameDropsWhatNoUrlNames(t *testing.T) {
	settings := DefaultAbuseSettings()
	cases := map[string]string{
		"shop.acme.root.example.":    "shop.acme.root.example",
		"*.acme.root.example":        "acme.root.example",
		"api-*.acme.root.example":    "acme.root.example",
		"_dmarc.acme.root.example.":  "acme.root.example",
		"a._tcp.b.acme.root.example": "b.acme.root.example",
		"*":                          "",
		"*.example":                  "",
	}
	for name, want := range cases {
		if got := abuseScanLookupName(name, settings); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

func TestAbuseScanFeedCategoryTakesOnlyAThreatRow(t *testing.T) {
	for text, want := range map[string]AbuseCategory{
		"phishing":            AbuseCategoryPhishing,
		" Malware ":           AbuseCategoryMalware,
		"command_and_control": AbuseCategoryCommandAndControl,
		"spam":                "",
		"copyright":           "",
		"":                    "",
		"phishing,malware":    "",
		"threat_to_life":      "",
	} {
		got, ok := AbuseScanFeedCategory(text)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q, %t", text, got, ok)
		}
	}
}

// A network with a log, as api's append path leaves its head, so the published list pages it.
func testAbuseScanNetwork(t testing.TB, ctx context.Context) server.Id {
	t.Helper()
	networkId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_acl_head (network_id, version, chain) VALUES ($1, 1, $2)`,
			networkId,
			make([]byte, 32),
		))
	})
	return networkId
}

// One network_dns_record row.
func testAbuseScanRecord(t testing.TB, ctx context.Context, root string, owner string, rrType int, networkId *server.Id, kind string, withheld bool, expireTime *time.Time) {
	t.Helper()
	var withheldTime *time.Time
	if withheld {
		now := server.NowUtc()
		withheldTime = &now
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_dns_record (root, owner, rrtype, network_id, kind, ttl, rdata, create_time, expire_time, withheld_time)
				VALUES ($1, $2, $3, $4, $5, 300, '{}', $6, $7, $8)
			`,
			root,
			owner,
			rrType,
			networkId,
			kind,
			server.NowUtc(),
			expireTime,
			withheldTime,
		))
	})
}

// Every page from the start, at most limit pages.
func testAbuseScanAllNames(t testing.TB, ctx context.Context, rootNames []string, settings *AbuseScanSettings, now time.Time) []string {
	t.Helper()
	names := []string{}
	var cursor *AbuseScanNamesCursor
	for range 100 {
		page, next, err := ListAbuseScanNames(ctx, cursor, rootNames, now, settings)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range page {
			names = append(names, name.Name)
		}
		if next == nil {
			return names
		}
		cursor = next
	}
	t.Fatal("the list did not end")
	return nil
}

// The published hosts, then the user records' owners, each under a root and each once, by a cursor that a failed
// page does not move: an alias (a custom host), another root's names, glob and underscore labels, and a record that
// is withheld, expired or not the network's own are dropped or cut to what a url names.
func TestListAbuseScanNamesPagesPublishedHostsThenRecords(t *testing.T) {
	requireAbuseScanTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		first := testAbuseScanNetwork(t, ctx)
		second := testAbuseScanNetwork(t, ctx)
		failing := false
		published := map[server.Id][]*TlsPublishedName{
			first: {
				{Host: "shop", Port: 443, Label: "acme", Name: "shop.acme." + testAbuseScanRoot, Audience: protocol.AclAudience_ACL_AUDIENCE_PUBLIC},
				{Host: "shop", Port: 8443, Label: "acme", Name: "shop.acme." + testAbuseScanRoot, Audience: protocol.AclAudience_ACL_AUDIENCE_PUBLIC},
				{Host: "@", Port: 443, Label: "acme", Name: "acme." + testAbuseScanRoot, Audience: protocol.AclAudience_ACL_AUDIENCE_PUBLIC},
				{Host: "*", Port: 443, Label: "acme", Name: "*.acme." + testAbuseScanRoot, Audience: protocol.AclAudience_ACL_AUDIENCE_PUBLIC},
				{Host: "blog", Port: 443, Alias: "blog.customer.example", Name: "blog.customer.example", Audience: protocol.AclAudience_ACL_AUDIENCE_PUBLIC},
			},
			second: {
				{Host: "mail", Port: 443, Label: "beta", Name: "mail.beta.other.example", Audience: protocol.AclAudience_ACL_AUDIENCE_PUBLIC},
				{Host: "api", Port: 443, Label: "beta", Name: "api.beta." + testAbuseScanRoot, Audience: protocol.AclAudience_ACL_AUDIENCE_LINK},
			},
		}
		SetTlsPublishedNamesSource(func(ctx context.Context, networkId server.Id, now time.Time) ([]*TlsPublishedName, error) {
			if failing {
				return nil, errors.New("a projection that does not load")
			}
			return published[networkId], nil
		})
		defer SetTlsPublishedNamesSource(nil)

		root := testAbuseScanRoot + "."
		expired := now.Add(-time.Minute)
		testAbuseScanRecord(t, ctx, root, "www.acme."+root, 1, &first, "user", false, nil)
		testAbuseScanRecord(t, ctx, root, "www.acme."+root, 16, &first, "user", false, nil)
		testAbuseScanRecord(t, ctx, root, "_dmarc.acme."+root, 16, &first, "user", false, nil)
		testAbuseScanRecord(t, ctx, root, "*.beta."+root, 1, &second, "user", false, nil)
		testAbuseScanRecord(t, ctx, root, "held.beta."+root, 1, &second, "user", true, nil)
		testAbuseScanRecord(t, ctx, root, "gone.beta."+root, 16, &second, "user", false, &expired)
		testAbuseScanRecord(t, ctx, root, "mx.beta."+root, 1, &second, "system", false, nil)
		testAbuseScanRecord(t, ctx, root, root, 2, nil, "system", false, nil)
		testAbuseScanRecord(t, ctx, "other.example.", "x.beta.other.example.", 1, &second, "user", false, nil)

		settings := &AbuseScanSettings{PublishedPageCount: 2, PublishedNetworkBatchCount: 1, RecordBatchCount: 2}
		names := testAbuseScanAllNames(t, ctx, []string{testAbuseScanRoot}, settings, now)
		// a name repeats only across pages, so the names compare as sets
		distinct := func(names []string) []string {
			return slices.Compact(slices.Sorted(slices.Values(names)))
		}
		want := []string{
			"shop.acme." + testAbuseScanRoot,
			"acme." + testAbuseScanRoot,
			"api.beta." + testAbuseScanRoot,
			"www.acme." + testAbuseScanRoot,
			"beta." + testAbuseScanRoot,
		}
		if !slices.Equal(distinct(names), distinct(want)) {
			t.Fatalf("names %q, want %q", names, want)
		}
		for _, name := range names {
			if !AbuseScanUnderRoot(name, []string{testAbuseScanRoot}) {
				t.Fatalf("%s is under no root", name)
			}
		}

		// a page that fails moves nothing, and the same cursor reads it once the source answers
		page, cursor, err := ListAbuseScanNames(ctx, nil, []string{testAbuseScanRoot}, now, settings)
		if err != nil || len(page) == 0 || cursor == nil || cursor.Records {
			t.Fatalf("the first page: %v, %+v, %v", page, cursor, err)
		}
		failing = true
		if page, next, err := ListAbuseScanNames(ctx, cursor, []string{testAbuseScanRoot}, now, settings); err == nil || page != nil || next != nil {
			t.Fatalf("a failing page answered %v, %+v, %v", page, next, err)
		}
		failing = false
		if _, _, err := ListAbuseScanNames(ctx, cursor, []string{testAbuseScanRoot}, now, settings); err != nil {
			t.Fatalf("the page read again: %v", err)
		}

		// with no source the check fails, never an empty list (QD-770)
		SetTlsPublishedNamesSource(nil)
		if _, _, err := ListAbuseScanNames(ctx, nil, []string{testAbuseScanRoot}, now, settings); !errors.Is(err, ErrTlsPublishedNamesUnavailable) {
			t.Fatalf("no source answered %v", err)
		}
	})
}

// A case of a host, with one machine kill recorded on it.
func testAbuseScanKill(t testing.TB, ctx context.Context, networkId server.Id, host string, reviewerId *server.Id) (caseId server.Id, actionId server.Id) {
	t.Helper()
	settings := DefaultAbuseSettings()
	actionId = server.NewId()
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		caseId, _, err = OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      AbuseCategoryPhishing,
			Target:        AbuseTarget{Kind: AbuseTargetKindHost, Name: host},
			ReporterClass: AbuseReporterClassWebRisk,
			JoinKind:      AbuseJoinKindHost,
			JoinKey:       host,
		}, server.NowUtc(), settings)
		if err != nil {
			return
		}
		_, _, _, err = RecordAbuseActionInTx(ctx, tx, &AbuseActionArgs{
			ActionId:   actionId,
			CaseId:     caseId,
			Kind:       AbuseActionKindHostKill,
			Target:     host,
			ReviewerId: reviewerId,
		}, server.NowUtc(), settings)
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

func testAbuseScanLift(t testing.TB, ctx context.Context, caseId server.Id, actionId server.Id, reviewState AbuseReviewState) {
	t.Helper()
	reviewerId := server.NewId()
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		_, _, _, _, err = RecordAbuseLiftInTx(ctx, tx, &AbuseLiftArgs{
			ActionId:       server.NewId(),
			CaseId:         caseId,
			LiftedActionId: actionId,
			ReviewState:    reviewState,
			ReviewerId:     &reviewerId,
		}, server.NowUtc(), DefaultAbuseSettings())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Only a machine's action lifted as wrong counts: a lift that leaves a strike, or a person's action lifted as wrong,
// does not, and the latest of several answers.
func TestGetAbuseScanLiftedWrongTimeFindsAMachinesWrongKill(t *testing.T) {
	requireAbuseScanTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		host := "shop.acme." + testAbuseScanRoot
		if _, ok := GetAbuseScanLiftedWrongTime(ctx, host); ok {
			t.Fatal("a host with no case")
		}

		caseId, actionId := testAbuseScanKill(t, ctx, networkId, host, nil)
		if _, ok := GetAbuseScanLiftedWrongTime(ctx, host); ok {
			t.Fatal("a kill that stands")
		}
		testAbuseScanLift(t, ctx, caseId, actionId, AbuseReviewStateLifted)
		if _, ok := GetAbuseScanLiftedWrongTime(ctx, host); ok {
			t.Fatal("a lift that keeps the strike")
		}

		staffId := server.NewId()
		staffCaseId, staffActionId := testAbuseScanKill(t, ctx, networkId, "blog.acme."+testAbuseScanRoot, &staffId)
		testAbuseScanLift(t, ctx, staffCaseId, staffActionId, AbuseReviewStateLiftedWrong)
		if _, ok := GetAbuseScanLiftedWrongTime(ctx, "blog.acme."+testAbuseScanRoot); ok {
			t.Fatal("a person's action lifted as wrong")
		}

		before := server.NowUtc().Add(-time.Second)
		wrongCaseId, wrongActionId := testAbuseScanKill(t, ctx, networkId, host, nil)
		testAbuseScanLift(t, ctx, wrongCaseId, wrongActionId, AbuseReviewStateLiftedWrong)
		liftTime, ok := GetAbuseScanLiftedWrongTime(ctx, "SHOP.acme."+testAbuseScanRoot+".")
		if !ok || liftTime.Before(before) {
			t.Fatalf("the wrong kill: %v, %t", liftTime, ok)
		}
	})
}

// A hit on a root opens one case with no network and no action; the same root, category and class again counts on
// it; another category is its own incident.
func TestOpenAbuseRootIncidentCoalescesAndActsOnNothing(t *testing.T) {
	requireAbuseScanTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseSettings()
		now := server.NowUtc()
		args := &AbuseRootIncidentArgs{
			Root:          "Root.Example.",
			Category:      AbuseCategoryPhishing,
			ReporterClass: AbuseReporterClassWebRisk,
			Signal:        AbuseSignalWebRiskHit,
		}
		caseId, opened, err := OpenAbuseRootIncident(ctx, args, now, settings)
		if err != nil || !opened {
			t.Fatalf("%v, %t, %v", caseId, opened, err)
		}
		again, opened, err := OpenAbuseRootIncident(ctx, args, now.Add(time.Hour), settings)
		if err != nil || opened || again != caseId {
			t.Fatalf("the second hit: %v, %t, %v", again, opened, err)
		}
		malware := *args
		malware.Category = AbuseCategoryMalware
		other, opened, err := OpenAbuseRootIncident(ctx, &malware, now, settings)
		if err != nil || !opened || other == caseId {
			t.Fatalf("another category: %v, %t, %v", other, opened, err)
		}

		abuseCase := GetAbuseCase(ctx, caseId)
		if abuseCase == nil || abuseCase.NetworkId != nil || abuseCase.Target != testAbuseScanRoot ||
			abuseCase.JoinKind != AbuseJoinKindHost || abuseCase.JoinKey != testAbuseScanRoot || abuseCase.ReviewDueTime == nil {
			t.Fatalf("the incident: %+v", abuseCase)
		}
		var actionCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_action`).Scan(&actionCount))
		})
		if actionCount != 0 {
			t.Fatalf("a root incident took %d actions", actionCount)
		}

		if _, _, err := OpenAbuseRootIncident(ctx, &AbuseRootIncidentArgs{Root: " ", Category: AbuseCategoryPhishing, ReporterClass: AbuseReporterClassFeed}, now, settings); !errors.Is(err, ErrAbuseRootIncidentInvalid) {
			t.Fatalf("no root: %v", err)
		}
	})
}
