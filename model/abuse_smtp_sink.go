package model

// §21's case flow as rpt.bringyour.com's report intake reaches it
// (ABUSE-M5-09; D-ABUSE-5, D-SMTP-19, D-MAIL-10, FS-39): the real
// smtpedgeservice.SmtpAbuseSink of mxedge's platform block. The intake hands it
// only an attributed report's fields: the network SMTP's join named, the
// reported From domain, the complaint's type, the join's confidence and the
// reporter's class. No envelope, header text, body or reporter address reaches
// this file, by the type it takes (mx-4, R-ABUSE-2), so none can reach a row.
//
// An attributed role mail opens a case against the network's sending domain,
// joined by headers, on §21.8's report clock, and a repeat inside the
// coalescing window counts on the case already open. A feedback loop's
// complaint takes §14.8's own signal by its join's confidence. No machine
// action follows here: a stranger's report reaches only review (§21.17), and
// §14.8's ladder is smtpedge's, each halt reaching §21 as its own action.
// SMTP-M9-16's suspension request and SMTP-M9-17's investigation signal add
// their calls at M9.
//
// A report the sink cannot place is an error, which the intake answers 451, so
// the sender retries and no report is lost unread; a binary that does not link
// MG5 answers ErrAbuseUnlinked the same way.

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// §14.8's attribution confidences, as smtp_complaint's join_result holds them
// (D-SMTP-8).
const (
	abuseSmtpJoinHigh   = "high"
	abuseSmtpJoinMedium = "medium"
)

// The sink's seams: the clock and the settings, which a test replaces.
type AbuseSmtpSinkDeps struct {
	// The time a case opens at. nil reads the report's arrival time, and the
	// process's clock for a report with none.
	Now func() time.Time
	// §21's settings for each report. nil reads the defaults with abuse.yml's
	// protected names laid over them.
	Settings func() *AbuseSettings
}

// The real sink. Safe for concurrent use: it holds no state between calls.
type abuseSmtpSink struct {
	deps *AbuseSmtpSinkDeps
}

var _ smtpedgeservice.SmtpAbuseSink = (*abuseSmtpSink)(nil)

// The sink mxedge's platform block gives its report intake
// (smtpedgeservice.SmtpReportIntakeDeps.Sink). nil deps reads the process's
// clock and config.
func NewAbuseSmtpSink(deps *AbuseSmtpSinkDeps) smtpedgeservice.SmtpAbuseSink {
	if deps == nil {
		deps = &AbuseSmtpSinkDeps{}
	}
	return &abuseSmtpSink{deps: deps}
}

// §21.4's category for a complaint's type: a role mail and a loop's abuse
// complaint are about mail the network sent, fraud is phishing, a virus is
// malware, and anything else is for a person to read.
func abuseSmtpCategory(complaintType string) AbuseCategory {
	switch complaintType {
	case smtpedgeservice.SmtpComplaintTypeRole, smtpedgeservice.SmtpComplaintTypeAbuse:
		return AbuseCategorySpam
	case smtpedgeservice.SmtpComplaintTypeFraud:
		return AbuseCategoryPhishing
	case smtpedgeservice.SmtpComplaintTypeVirus:
		return AbuseCategoryMalware
	}
	return AbuseCategoryOther
}

// See smtpedgeservice.SmtpAbuseSink.
func (self *abuseSmtpSink) OpenSmtpCase(ctx context.Context, report *smtpedgeservice.SmtpAbuseReport) (returnErr error) {
	if !server.SidecarMg5Linked() {
		return ErrAbuseUnlinked
	}
	if report == nil || report.NetworkId == (sidecar.Id{}) {
		return fmt.Errorf("%w: no network", ErrAbuseReportInvalid)
	}
	domain := abuseName(report.Domain)
	if domain == "" {
		return fmt.Errorf("%w: no domain", ErrAbuseReportInvalid)
	}
	defer func() {
		// the store raises a failed read or write, which reaches the intake as
		// an error, so its sender retries
		if r := recover(); r != nil {
			if server.IsDoneError(r) {
				panic(r)
			}
			returnErr = fmt.Errorf("abuse smtp sink: %v", r)
		}
	}()

	abuseReport := &AbuseReport{
		Category: abuseSmtpCategory(report.Category),
	}
	var signal AbuseSignal
	switch report.ReporterClass {
	case smtpedgeservice.SmtpReporterClassLoop:
		abuseReport.ReporterClass = AbuseReporterClassMailReport
		// §14.8's joins take their signals directly, by confidence
		switch report.JoinResult {
		case abuseSmtpJoinHigh:
			signal = AbuseSignalMailJoinHigh
		case abuseSmtpJoinMedium:
			signal = AbuseSignalMailJoinMedium
		default:
			signal = AbuseSignalMailJoinLow
		}
	case smtpedgeservice.SmtpReporterClassRole:
		abuseReport.ReporterClass = AbuseReporterClassRoleMail
		// anyone may mail a role address, so it sits on a report's row
		signal = AbuseSignalOfReport(abuseReport.ReporterClass, abuseReport.Category, false)
	default:
		return fmt.Errorf("%w: an unknown reporter class", ErrAbuseReportInvalid)
	}

	settings := self.settings()
	join := &AbuseReportJoin{
		NetworkId: server.Id(report.NetworkId),
		JoinKind:  AbuseJoinKindHeaders,
		JoinKey:   domain,
		Target:    AbuseTarget{Kind: AbuseTargetKindSendingDomain, Name: domain},
	}
	_, err := OpenAbuseReportCase(ctx, abuseReport, join, signal, self.now(report), settings)
	return err
}

// When the report's case opens.
func (self *abuseSmtpSink) now(report *smtpedgeservice.SmtpAbuseReport) time.Time {
	if self.deps.Now != nil {
		return self.deps.Now()
	}
	if !report.ArrivalTime.IsZero() {
		return report.ArrivalTime.UTC()
	}
	return server.NowUtc()
}

// §21's settings for one report.
func (self *abuseSmtpSink) settings() *AbuseSettings {
	if self.deps.Settings != nil {
		return self.deps.Settings()
	}
	settings := DefaultAbuseSettings()
	ApplyAbuseConfig(settings, server.GetAbuseConfig())
	return settings
}
