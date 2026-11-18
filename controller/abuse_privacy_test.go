package controller

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// §22.7's canary rows over §21's flows (ABUSE-M5-21; R-ABUSE-6, I-2, I-13, I-18): canaries in a reported message's
// body and subject, reported through a fixture feedback loop's ARF report, run through SMTP-M5-05's report intake on
// model's SmtpEdgeStore and §21's AbuseSmtpSink on MG5's tables as the platform block composes them, attributed and
// opening its case; and a canary in a form report's header block past what the header-only parse reads, which joins
// its host and reaches review. Then CC3, every row of every table in the test's database and every Redis key, and
// CC4, the intake's log lines, its metrics, the process's registry and the records mailed off site, hold zero hits.
// The listener's input, mx-4's exception to CC1, is the one place a canary may be, and this test never stores it.
//
// The form report alone is TestAcceptAbuseReportKeepsNoThirdPartyData, and mail to abuse@<root> is cli/mxedge's S7
// case. The block's glog lines are scanned by mxedgehost's child-process T3; here the intake logs through its Log
// seam, and what model's store and sink write to glog is not captured.

// A log that keeps every line, for the scan.
type testAbusePrivacyLog struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *testAbusePrivacyLog) keep(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *testAbusePrivacyLog) Lines() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.lines)
}

func (self *testAbusePrivacyLog) Info(args ...any) { self.keep(fmt.Sprint(args...)) }
func (self *testAbusePrivacyLog) Infof(format string, args ...any) {
	self.keep(fmt.Sprintf(format, args...))
}
func (self *testAbusePrivacyLog) Warningf(format string, args ...any) {
	self.keep(fmt.Sprintf(format, args...))
}
func (self *testAbusePrivacyLog) Errorf(format string, args ...any) {
	self.keep(fmt.Sprintf(format, args...))
}
func (self *testAbusePrivacyLog) V(level int32) sidecar.Verbose {
	return &testAbusePrivacyVerbose{log: self}
}

type testAbusePrivacyVerbose struct {
	log *testAbusePrivacyLog
}

func (self *testAbusePrivacyVerbose) Enabled() bool    { return true }
func (self *testAbusePrivacyVerbose) Info(args ...any) { self.log.Info(args...) }
func (self *testAbusePrivacyVerbose) Infof(format string, args ...any) {
	self.log.Infof(format, args...)
}

// CC3's Postgres half: every row of every table of the test's database, a table an item.
func testAbusePrivacyPostgresItems(t testing.TB, ctx context.Context) []*sidecartest.PrivacyItem {
	t.Helper()
	tables := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT table_name::text
			FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'
			ORDER BY table_name
		`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var table string
				server.Raise(result.Scan(&table))
				tables = append(tables, table)
			}
		})
	})
	items := []*sidecartest.PrivacyItem{}
	for _, table := range tables {
		rows := &bytes.Buffer{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT row_to_json(stored)::text FROM "`+table+`" stored`)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var row string
					server.Raise(result.Scan(&row))
					rows.WriteString(row + "\n")
				}
			})
		})
		items = append(items, &sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassPostgres, Name: "postgres " + table, Body: rows.Bytes()})
	}
	return items
}

// CC3's Redis half: every key of the test's Redis, each value read by its type.
func testAbusePrivacyRedisItems(t testing.TB, ctx context.Context) []*sidecartest.PrivacyItem {
	t.Helper()
	items := []*sidecartest.PrivacyItem{}
	server.Redis(ctx, func(client server.RedisClient) {
		keys := []string{}
		scanNode := func(ctx context.Context, node redis.Cmdable) error {
			cursor := uint64(0)
			for {
				found, next, err := node.Scan(ctx, cursor, "*", 1000).Result()
				if err != nil {
					return err
				}
				keys = append(keys, found...)
				if next == 0 {
					return nil
				}
				cursor = next
			}
		}
		var err error
		if clusterClient, ok := client.(*redis.ClusterClient); ok {
			var keysLock sync.Mutex
			err = clusterClient.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
				keysLock.Lock()
				defer keysLock.Unlock()
				return scanNode(ctx, node)
			})
		} else {
			err = scanNode(ctx, client)
		}
		if err != nil {
			t.Fatalf("SCAN: %v", err)
		}
		sort.Strings(keys)
		for _, key := range slices.Compact(keys) {
			value := &bytes.Buffer{}
			value.WriteString(key + "\n")
			switch client.Type(ctx, key).Val() {
			case "string":
				value.WriteString(client.Get(ctx, key).Val())
			case "hash":
				for field, fieldValue := range client.HGetAll(ctx, key).Val() {
					value.WriteString(field + "\x00" + fieldValue + "\n")
				}
			case "list":
				value.WriteString(strings.Join(client.LRange(ctx, key, 0, -1).Val(), "\n"))
			case "set":
				value.WriteString(strings.Join(client.SMembers(ctx, key).Val(), "\n"))
			case "zset":
				value.WriteString(strings.Join(client.ZRange(ctx, key, 0, -1).Val(), "\n"))
			case "stream":
				for _, message := range client.XRange(ctx, key, "-", "+").Val() {
					for field, fieldValue := range message.Values {
						value.WriteString(fmt.Sprintf("%s\x00%v\n", field, fieldValue))
					}
				}
			}
			items = append(items, &sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassRedis, Name: "redis " + key, Body: value.Bytes()})
		}
	})
	return items
}

// The case in the file header.
func TestAbusePrivacyCanariesInReportedMailReachNoStoreLogOrMetric(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, mailer := newTestAbuseDeps(t)
		newTestAbuseIntake(t)
		capture := sidecartest.NewPrivacyCapture()
		bodyCanary := capture.CanaryText("reported mail body")
		subjectCanary := capture.CanaryText("reported mail subject")
		blockCanary := capture.CanaryText("form header block body")
		networkId, zone := newTestAbuseIntakeNetwork(t, ctx)
		fromDomain := "mail." + zone

		// the platform block's intake over the real store and sink, a feedback loop's key published, the network's
		// mail key held so its report is sealed, and the enclosed message attributed to the network at high
		// confidence (SMTP-M9-15's join, faked)
		arrival := server.NowUtc().Truncate(time.Second)
		intakeFakes := sidecartest.NewFakeHost(ctx, sidecartest.NewFakeClock(arrival), nil, nil, nil)
		txt := sidecartest.NewFakeSmtpDnsTxt()
		loopKey := sidecartest.NewSmtpDkimFixtureEd25519Key("loop", "fbl1", "loop.example")
		txt.Set(loopKey.RecordName(), loopKey.TxtRecord())
		keys := sidecartest.NewFakeSmtpReportKeys()
		keys.Add(sidecar.Id(networkId))
		join := sidecartest.NewFakeSmtpReportJoin()
		join.Attribute(fromDomain, sidecar.Id(networkId), "high")
		settings := smtpedgeservice.DefaultSmtpEdgeSettings()
		settings.Root = testAbuseIntakeRoot
		settings.ReportHostName = "rpt.report.example"
		settings.LoopAllowlist = []string{"loop.example"}
		intakeLog := &testAbusePrivacyLog{}
		intake, err := smtpedgeservice.NewSmtpReportIntake(intakeFakes.ServiceHost(), model.NewSmtpEdgeStore(nil), &smtpedgeservice.SmtpReportIntakeDeps{
			Tokens: sidecartest.NewFakeSmtpNetTokens(),
			Labels: sidecartest.NewFakeSmtpLabels(),
			Keys:   keys,
			Txt:    txt,
			Join:   join,
			Sink:   model.NewAbuseSmtpSink(nil),
			Log:    intakeLog,
		}, settings)
		if err != nil {
			t.Fatal(err)
		}

		// the feedback loop's report of the network's message, the canaries in its subject and body
		enclosed := (&sidecartest.SmtpFixtureMessage{
			FromDomain: fromDomain,
			MessageId:  "<m1@" + fromDomain + ">",
			Subject:    subjectCanary,
			Body:       bodyCanary,
		}).Bytes()
		arf := sidecartest.SignSmtpDkimFixture(sidecartest.BuildSmtpArfReport(loopKey.Domain, "abuse", enclosed, false), loopKey, &sidecartest.SmtpDkimFixtureOptions{
			HeaderRelaxed: true,
			BodyRelaxed:   true,
			Headers:       []string{"from", "to", "subject", "date", "message-id"},
		})
		outcome, err := intake.Accept(ctx, &smtpedgeservice.SmtpReportMessage{Recipient: "root@" + settings.ReportHostName, ArrivalTime: arrival, Raw: arf})
		if err != nil || (outcome != smtpedgeservice.SmtpReportOutcomeCase && outcome != smtpedgeservice.SmtpReportOutcomeComplaint) {
			t.Fatalf("the feedback loop's report: %s, %v", outcome, err)
		}
		// the report's parse output and its case: one complaint row of the network's, and a case naming it
		complaints, err := model.FindSmtpComplaints(ctx, &smtpedgeservice.SmtpComplaintKey{NetworkId: sidecar.Id(networkId)}, 10)
		if err != nil || len(complaints) != 1 || complaints[0].JoinResult != "high" {
			t.Fatalf("the report's rows: %+v, %v", complaints, err)
		}
		var networkCases int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_case WHERE network_id = $1`, networkId).Scan(&networkCases))
		})
		if outcome == smtpedgeservice.SmtpReportOutcomeCase && networkCases != 1 {
			t.Fatalf("the report's case: %d cases of the network", networkCases)
		}

		// a form report whose header block carries a canary past its blank line, which the header-only parse never reads
		report := testAcceptAbuseReport(t, ctx, &model.AbuseReport{
			Category:      model.AbuseCategorySpam,
			ReporterClass: model.AbuseReporterClassPublic,
			HeaderBlock:   []byte("From: Someone <someone@" + fromDomain + ">\r\nTo: person@person.example\r\nSubject: offer\r\n\r\n" + blockCanary + "\r\n"),
		})
		if !report.Joined {
			t.Fatalf("the form's header block joined nothing: %+v", report)
		}

		// CC3 and CC4
		postgresItems := testAbusePrivacyPostgresItems(t, ctx)
		for _, item := range postgresItems {
			capture.AddItem(item)
		}
		// the scan's control: the case's join key, the reported host's zone, is stored, so a capture forbidding it
		// over the same rows finds it, and a scan finding no canary read the rows it means to
		control := sidecartest.NewPrivacyCapture()
		control.Forbid("the reported zone", []byte(fromDomain))
		for _, item := range postgresItems {
			control.AddItem(item)
		}
		if len(control.Scan()) == 0 {
			t.Fatal("the rows the scan reads do not hold the reported zone, so they are not the rows it means to read")
		}
		for _, item := range testAbusePrivacyRedisItems(t, ctx) {
			capture.AddItem(item)
		}
		for _, line := range intakeLog.Lines() {
			capture.AddItem(&sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassLog, Name: "intake log", Body: []byte(line)})
		}
		for _, line := range intakeFakes.Metrics.Dump() {
			capture.AddItem(&sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassMetric, Name: "intake metric", Body: []byte(line)})
		}
		families, err := prometheus.DefaultGatherer.Gather()
		if err != nil {
			t.Fatal(err)
		}
		exposition := &bytes.Buffer{}
		for _, family := range families {
			if _, err := expfmt.MetricFamilyToText(exposition, family); err != nil {
				t.Fatal(err)
			}
		}
		capture.AddItem(&sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassMetric, Name: "registry", Body: exposition.Bytes()})
		for i, record := range mailer.mailed() {
			capture.AddItem(&sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassLog, Name: fmt.Sprintf("mailed record %d", i), Body: record})
		}
		capture.Forbid("the reporter's address", []byte("person@person.example"))
		if capture.ItemCount(sidecartest.PrivacyClassPostgres) == 0 || !strings.Contains(exposition.String(), "urnetwork_") {
			t.Fatal("the scan read no table or no metric of ours")
		}
		for _, finding := range capture.Scan() {
			t.Errorf("%s", finding)
		}
	})
}
