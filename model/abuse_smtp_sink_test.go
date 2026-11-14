//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// The real SmtpAbuseSink behind sidecar's report intake (abuse_smtp_sink.go):
// FS-39's case leg on MG5's abuse_case. Attribution is SMTP-M9-15's, faked
// here as sidecartest.FakeSmtpReportJoin, since that is all the sink sees of
// it. Names are .example and addresses RFC 5737's.

// Every line the intake logged.
type testAbuseSinkLogger struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *testAbuseSinkLogger) keep(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *testAbuseSinkLogger) text() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return strings.Join(self.lines, "\n")
}

func (self *testAbuseSinkLogger) Info(args ...any) { self.keep(fmt.Sprint(args...)) }

func (self *testAbuseSinkLogger) Infof(format string, args ...any) {
	self.keep(fmt.Sprintf(format, args...))
}

func (self *testAbuseSinkLogger) Warningf(format string, args ...any) {
	self.keep(fmt.Sprintf(format, args...))
}

func (self *testAbuseSinkLogger) Errorf(format string, args ...any) {
	self.keep(fmt.Sprintf(format, args...))
}

// Every level, so a verbose line is caught too.
func (self *testAbuseSinkLogger) V(level int32) sidecar.Verbose {
	return &testAbuseSinkVerbose{log: self}
}

type testAbuseSinkVerbose struct {
	log *testAbuseSinkLogger
}

func (self *testAbuseSinkVerbose) Enabled() bool { return true }

func (self *testAbuseSinkVerbose) Info(args ...any) { self.log.Info(args...) }

func (self *testAbuseSinkVerbose) Infof(format string, args ...any) {
	self.log.Infof(format, args...)
}

// The sink on the test settings, reading each report's arrival as its clock.
func newTestAbuseSmtpSink() smtpedgeservice.SmtpAbuseSink {
	return NewAbuseSmtpSink(&AbuseSmtpSinkDeps{Settings: testAbuseJoinSettings})
}

// A person's mail to abuse@<root>, enclosing a message from fromDomain with
// its own subject and body.
func testAbuseRoleMail(fromDomain string, senderAddress string, text string, enclosedSubject string, enclosedBody string) []byte {
	enclosed := (&sidecartest.SmtpFixtureMessage{FromDomain: fromDomain, MessageId: "<m2@" + fromDomain + ">", Subject: enclosedSubject, Body: enclosedBody}).Bytes()
	return []byte("From: A Person <" + senderAddress + ">\r\nTo: abuse@root.example\r\nSubject: " + text + "\r\n" +
		"Content-Type: multipart/mixed; boundary=\"" + sidecartest.SmtpFixtureBoundary + "\"\r\n\r\n" +
		"--" + sidecartest.SmtpFixtureBoundary + "\r\nContent-Type: text/plain\r\n\r\n" + text + "\r\n" +
		"--" + sidecartest.SmtpFixtureBoundary + "\r\nContent-Type: message/rfc822\r\n\r\n" + string(enclosed) + "\r\n--" + sidecartest.SmtpFixtureBoundary + "--\r\n")
}

// The cases of one network, oldest first.
func testNetworkAbuseCases(t testing.TB, ctx context.Context, networkId server.Id) (cases []*AbuseCase) {
	t.Helper()
	caseIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT case_id FROM abuse_case WHERE network_id = $1 ORDER BY create_time, case_id`, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var caseId server.Id
				server.Raise(result.Scan(&caseId))
				caseIds = append(caseIds, caseId)
			}
		})
	})
	for _, caseId := range caseIds {
		cases = append(cases, GetAbuseCase(ctx, caseId))
	}
	return
}

// A role mail goes through sidecar's whole report intake to the real sink: an
// attributed one opens one case, and nothing of the message, its body, its
// subject, its sender or the enclosed message's text, reaches a row or a log.
// The same mail again counts on the open case, and a mail no join attributes
// leaves no row at all.
func TestAbuseIntakeNeverReadsBody(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		faults := sidecartest.NewFaultQueue()
		host := sidecartest.NewFakeHost(ctx, clock, faults, nil, nil)
		store := sidecartest.NewFakeSmtpEdgeStore(clock, faults)
		join := sidecartest.NewFakeSmtpReportJoin()
		log := &testAbuseSinkLogger{}
		settings := smtpedgeservice.DefaultSmtpEdgeSettings()
		settings.Root = testAbuseJoinRoot
		settings.ReportHostName = "rpt.report.example"
		settings.SendingAddresses = []smtpedgeservice.SmtpSendingAddress{
			{Address: netip.MustParseAddr("192.0.2.10"), HeloName: "o1.out.report.example"},
		}
		intake, err := smtpedgeservice.NewSmtpReportIntake(host.ServiceHost(), store, &smtpedgeservice.SmtpReportIntakeDeps{
			Labels: sidecartest.NewFakeSmtpLabels(),
			Txt:    sidecartest.NewFakeSmtpDnsTxt(),
			Join:   join,
			Sink:   newTestAbuseSmtpSink(),
			Log:    log,
		}, settings)
		if err != nil {
			t.Fatal(err)
		}

		networkId := sidecar.NewId()
		domain := "quietnet." + testAbuseJoinRoot
		join.Attribute(domain, networkId, "medium")
		canary := "canary7f3a9c"
		raw := testAbuseRoleMail(domain, canary+"@person.example", "report text "+canary, "subject "+canary, "body "+canary)
		arrival := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)
		accept := func(message []byte, at time.Time) smtpedgeservice.SmtpReportOutcome {
			outcome, err := intake.Accept(ctx, &smtpedgeservice.SmtpReportMessage{Recipient: "abuse@" + testAbuseJoinRoot, ArrivalTime: at, Raw: message})
			if err != nil {
				t.Fatal(err)
			}
			return outcome
		}

		if outcome := accept(raw, arrival); outcome != smtpedgeservice.SmtpReportOutcomeCase {
			t.Fatalf("an attributed role mail ended %s", outcome)
		}
		cases := testNetworkAbuseCases(t, ctx, server.Id(networkId))
		if len(cases) != 1 {
			t.Fatalf("%d cases", len(cases))
		}
		abuseCase := cases[0]
		if abuseCase.Category != AbuseCategorySpam || abuseCase.TargetKind != AbuseTargetKindSendingDomain || abuseCase.Target != domain ||
			abuseCase.ReporterClass != AbuseReporterClassRoleMail || abuseCase.JoinKind != AbuseJoinKindHeaders || abuseCase.JoinKey != domain ||
			abuseCase.State != AbuseCaseStateOpen || abuseCase.Barred || abuseCase.ReviewDueTime == nil || !abuseCase.CreateTime.Equal(arrival) {
			t.Fatalf("the case reads %+v", abuseCase)
		}
		// a stranger's mail reaches review and nothing else
		if actions := GetAbuseCaseActions(ctx, abuseCase.CaseId); len(actions) != 0 {
			t.Fatalf("a role mail took %d actions", len(actions))
		}

		// FS-39's case leg: the repeat counts on the open case
		if outcome := accept(raw, arrival.Add(time.Hour)); outcome != smtpedgeservice.SmtpReportOutcomeCase {
			t.Fatalf("the repeat ended %s", outcome)
		}
		if cases := testNetworkAbuseCases(t, ctx, server.Id(networkId)); len(cases) != 1 || cases[0].CaseId != abuseCase.CaseId {
			t.Fatalf("the repeat left %d cases", len(cases))
		}

		// a role mail no join attributes is unread and leaves no row
		before := testAbuseCaseCount(t, ctx)
		unattributed := testAbuseRoleMail("nobody."+testAbuseJoinRoot, canary+"@person.example", canary, canary, canary)
		if outcome := accept(unattributed, arrival); outcome == smtpedgeservice.SmtpReportOutcomeCase {
			t.Fatal("an unattributed role mail opened a case")
		}
		if got := testAbuseCaseCount(t, ctx); got != before {
			t.Fatalf("an unattributed role mail left a row: %d cases", got)
		}

		if rows := testAbuseRowsText(t, ctx); strings.Contains(rows, canary) || strings.Contains(rows, "person.example") {
			t.Fatalf("a row holds the message's text or its sender: %s", rows)
		}
		if logged := log.text(); strings.Contains(logged, canary) || strings.Contains(logged, "person.example") {
			t.Fatalf("a log line holds the message's text or its sender: %s", logged)
		}
	})
}

// What the sink makes of each report: a role mail sits on a report's row, a
// feedback loop's complaint on §14.8's by its join's confidence, and the
// complaint's type names the category.
func TestAbuseSmtpSinkOpensCasesByClassAndType(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sink := newTestAbuseSmtpSink()
		arrival := server.NowUtc().Truncate(time.Second)
		networkId := sidecar.NewId()
		for index, expected := range []struct {
			complaintType string
			reporterClass string
			category      AbuseCategory
			class         AbuseReporterClass
		}{
			{smtpedgeservice.SmtpComplaintTypeRole, smtpedgeservice.SmtpReporterClassRole, AbuseCategorySpam, AbuseReporterClassRoleMail},
			{smtpedgeservice.SmtpComplaintTypeAbuse, smtpedgeservice.SmtpReporterClassLoop, AbuseCategorySpam, AbuseReporterClassMailReport},
			{smtpedgeservice.SmtpComplaintTypeFraud, smtpedgeservice.SmtpReporterClassLoop, AbuseCategoryPhishing, AbuseReporterClassMailReport},
			{smtpedgeservice.SmtpComplaintTypeVirus, smtpedgeservice.SmtpReporterClassLoop, AbuseCategoryMalware, AbuseReporterClassMailReport},
			{smtpedgeservice.SmtpComplaintTypeOther, smtpedgeservice.SmtpReporterClassLoop, AbuseCategoryOther, AbuseReporterClassMailReport},
		} {
			// a domain each, so no two coalesce
			domain := fmt.Sprintf("Otter-%04d.Quietnet.Root.Example.", index)
			err := sink.OpenSmtpCase(ctx, &smtpedgeservice.SmtpAbuseReport{
				ComplaintId:   sidecar.NewId(),
				NetworkId:     networkId,
				Domain:        domain,
				Category:      expected.complaintType,
				JoinResult:    "high",
				ReporterClass: expected.reporterClass,
				ArrivalTime:   arrival,
			})
			if err != nil {
				t.Fatalf("%s: %v", expected.complaintType, err)
			}
			cases := testNetworkAbuseCases(t, ctx, server.Id(networkId))
			if len(cases) != index+1 {
				t.Fatalf("%s: %d cases", expected.complaintType, len(cases))
			}
			var abuseCase *AbuseCase
			for _, candidate := range cases {
				if candidate.Target == fmt.Sprintf("otter-%04d.quietnet.root.example", index) {
					abuseCase = candidate
				}
			}
			if abuseCase == nil || abuseCase.Category != expected.category || abuseCase.ReporterClass != expected.class ||
				abuseCase.TargetKind != AbuseTargetKindSendingDomain || abuseCase.JoinKind != AbuseJoinKindHeaders || abuseCase.ReviewDueTime == nil {
				t.Fatalf("%s: the case reads %+v", expected.complaintType, abuseCase)
			}
			if actions := GetAbuseCaseActions(ctx, abuseCase.CaseId); len(actions) != 0 {
				t.Fatalf("%s: the sink took %d actions", expected.complaintType, len(actions))
			}
		}
	})
}

// A report the sink cannot place is refused with an error, which the intake
// answers 451, and leaves no row.
func TestAbuseSmtpSinkRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sink := newTestAbuseSmtpSink()
		valid := func() *smtpedgeservice.SmtpAbuseReport {
			return &smtpedgeservice.SmtpAbuseReport{
				ComplaintId:   sidecar.NewId(),
				NetworkId:     sidecar.NewId(),
				Domain:        "quietnet.root.example",
				Category:      smtpedgeservice.SmtpComplaintTypeRole,
				ReporterClass: smtpedgeservice.SmtpReporterClassRole,
				ArrivalTime:   server.NowUtc(),
			}
		}
		noNetwork := valid()
		noNetwork.NetworkId = sidecar.Id{}
		noDomain := valid()
		noDomain.Domain = " . "
		unknownClass := valid()
		unknownClass.ReporterClass = "planted-class-value"
		for name, report := range map[string]*smtpedgeservice.SmtpAbuseReport{
			"no report": nil, "no network": noNetwork, "no domain": noDomain, "an unknown class": unknownClass,
		} {
			err := sink.OpenSmtpCase(ctx, report)
			if !errors.Is(err, ErrAbuseReportInvalid) || strings.Contains(err.Error(), "planted-class-value") {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if got := testAbuseCaseCount(t, ctx); got != 0 {
			t.Fatalf("a refusal left %d cases", got)
		}
		// the default sink reads the process's config, and takes a valid report
		if err := NewAbuseSmtpSink(nil).OpenSmtpCase(ctx, valid()); err != nil {
			t.Fatal(err)
		}
		if got := testAbuseCaseCount(t, ctx); got != 1 {
			t.Fatalf("%d cases", got)
		}
	})
}
