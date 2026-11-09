package main

// The report intake mxedge's platform block hands its listener (SMTP-M5-06; §14.8, §15.4's platform mail,
// D-SMTP-12, D-SMTP-19, D-SMTP-23, plan-SMTP-report-intake-home): sidecar's smtpedgeservice.SmtpReportIntake
// in this process, on model's Postgres store, with the listener's mxedgeservice.PlatformIntake in front of it,
// since neither engine may import the other (§4.4). The listener calls it once for each recipient it took,
// before it answers the DATA or BDAT, and the intake keeps the envelope and the message in memory (mx-4).
//
// It reads config/<env>/smtpedge.yml for the names, the sending addresses, the loop allowlist and the report
// bounds, as the smtpedge block will (smtpedgeservice.LoadSmtpEdgeSettings). Each of the intake's seams that
// server cannot serve yet stays empty, and fails closed as smtpedgeservice.SmtpReportIntakeDeps says:
//
//   - Tokens: network_dns_zone.report_token has no column yet (plan-DNS-rua-token's interim), so every
//     <token>@ report is dropped unread;
//   - Keys: plan-SMTP-report-seal-key's interim, so no raw report is sealed: its bytes drop and only its
//     fields are kept;
//   - Join and Sink: SMTP-M9-15's attribution and ABUSE's case flow behind it, so no ARF report or role mail
//     is attributable, and the listener answers role mail plan-ABUSE-auto-reply's 550.
//
// <root>'s rows resolve through §12's label map (model.ResolveNetworkLabel), and reporters' DKIM keys come from
// DNS through the process's resolver. The intake reads MG2's dns_label and writes MG5's report tables, so the
// block is ready only in a binary that links both (reportIntakeGroups, HostDeps.IntakeGroups), and every store
// call answers model.ErrSmtpReportUnlinked in one that does not.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// The intake's own file in config/<env>/, which the smtpedge block reads too.
const reportIntakeSettingsName = "smtpedge.yml"

// The pending groups the intake reads: MG2's dns_label through the label map, and MG5's report tables.
func reportIntakeGroups() []*sidecarhost.RunGroup {
	return []*sidecarhost.RunGroup{
		{Name: "MG2", Linked: server.SidecarMg2Linked},
		{Name: "MG5", Linked: server.SidecarMg5Linked},
	}
}

// smtpedge.yml from config.
func loadReportIntakeSettings() (*smtpedgeservice.SmtpEdgeSettings, error) {
	resource, err := server.Config.SimpleResource(reportIntakeSettingsName)
	if err != nil {
		return nil, err
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, err
	}
	return smtpedgeservice.LoadSmtpEdgeSettings(data)
}

// The intake on store with settings, reporters' keys from txt and report tokens from tokens, either of which
// may be nil to fail closed, logging through glog. Its metrics go to the process's registry, and it reads real
// time.
func newPlatformReportIntake(store smtpedgeservice.SmtpEdgeStore, settings *smtpedgeservice.SmtpEdgeSettings, txt smtpedgeservice.SmtpDnsTxtSource, tokens smtpedgeservice.SmtpNetTokenSource) (*platformReportIntake, error) {
	if settings == nil {
		return nil, errors.New("report intake: no smtpedge.yml")
	}
	host := &sidecar.ServiceHost{
		Metrics: sidecarhost.NewMetrics(),
		Clock:   sidecar.RealClock{},
	}
	deps := &smtpedgeservice.SmtpReportIntakeDeps{
		Tokens: tokens,
		Labels: &reportLabelSource{root: settings.Root},
		Txt:    txt,
		Log:    reportIntakeLogger{},
	}
	intake, err := smtpedgeservice.NewSmtpReportIntake(host, store, deps, settings)
	if err != nil {
		return nil, err
	}
	return &platformReportIntake{
		intake: intake,
		root:   reportIntakeName(settings.Root),
	}, nil
}

// A name as both files hold it: lowercase, with no final dot.
func reportIntakeName(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

// The platform block's intake from config: smtpedge.yml's settings on model's store, reporters' keys from the
// process's resolver and no report tokens yet. smtpedge.yml's root and report host must be mxedge.yml's
// platform_root and platform_report_host, since the listener takes recipients by the one and the intake routes
// them by the other: mail one took and the other could not route would be answered 250 and dropped. An error
// leaves the block without an intake, so every recipient the listener answers for gets 451 4.3.0.
func newConfiguredReportIntake(mxEdgeSettings *mxedgeservice.MxEdgeSettings) (*platformReportIntake, error) {
	settings, err := loadReportIntakeSettings()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", reportIntakeSettingsName, err)
	}
	if mxEdgeSettings == nil {
		return nil, errors.New("report intake: no mxedge.yml")
	}
	if reportIntakeName(settings.Root) != reportIntakeName(mxEdgeSettings.PlatformRootDomainName) {
		return nil, fmt.Errorf("%s's root is not mxedge.yml's platform_root", reportIntakeSettingsName)
	}
	if reportIntakeName(settings.ReportHostName) != reportIntakeName(mxEdgeSettings.PlatformReportHostName) {
		return nil, fmt.Errorf("%s's report_host is not mxedge.yml's platform_report_host", reportIntakeSettingsName)
	}
	return newPlatformReportIntake(model.NewSmtpEdgeStore(nil), settings, &smtpedgeservice.SmtpResolverTxtSource{}, nil)
}

// The intake as the platform listener reaches it. Safe for concurrent use, as the intake is.
type platformReportIntake struct {
	intake *smtpedgeservice.SmtpReportIntake
	// <root>, lowercased, whose abuse@ and postmaster@ are role mail
	root string
}

var _ mxedgeservice.PlatformIntake = (*platformReportIntake)(nil)

// Whether recipient is abuse@ or postmaster@ on <root>, as the listener lowercased it.
func (self *platformReportIntake) roleRecipient(recipient string) bool {
	local, domain, ok := strings.Cut(strings.ToLower(recipient), "@")
	if !ok {
		return false
	}
	return (local == "abuse" || local == "postmaster") && strings.TrimSuffix(domain, ".") == self.root
}

// See mxedgeservice.PlatformIntake. A role mail the intake could not make a case of is unread, which the
// listener answers with plan-ABUSE-auto-reply's 550 when every recipient's ends so; everything else the intake
// finished with is taken, counted, stored or dropped, which tells the sender nothing. An error is the intake's,
// a store or DNS that did not answer, which carries no report text, and the listener answers it 451.
func (self *platformReportIntake) Accept(ctx context.Context, message *mxedgeservice.PlatformMessage) (mxedgeservice.PlatformOutcome, error) {
	outcome, err := self.intake.Accept(ctx, &smtpedgeservice.SmtpReportMessage{
		Recipient:   message.Recipient,
		ArrivalTime: message.ArrivalTime,
		Raw:         message.Raw,
	})
	if err != nil {
		return 0, err
	}
	if self.roleRecipient(message.Recipient) && outcome != smtpedgeservice.SmtpReportOutcomeCase {
		return mxedgeservice.PlatformOutcomeUnread, nil
	}
	return mxedgeservice.PlatformOutcomeTaken, nil
}

// §12's label map as the intake reads <root>'s rows: the network a <net> label or alias names in root, a
// retiring alias's included, since a network's mail still names it (plan-SMTP-net-token). A name nothing holds
// is no network; a read that failed is an error, so the reporter retries.
type reportLabelSource struct {
	root string
}

// See smtpedgeservice.SmtpLabelSource.
func (self *reportLabelSource) NetworkForLabel(ctx context.Context, label string) (networkId sidecar.Id, ok bool, returnErr error) {
	defer func() {
		// the model raises a failed read, which reaches the intake as an error
		if r := recover(); r != nil {
			if server.IsDoneError(r) {
				panic(r)
			}
			returnErr = fmt.Errorf("report intake: the label map: %v", r)
		}
	}()
	resolvedId, _, err := model.ResolveNetworkLabel(ctx, self.root, label, false)
	if errors.Is(err, sidecar.ErrNetworkLabelUnknown) {
		return sidecar.Id{}, false, nil
	}
	if err != nil {
		return sidecar.Id{}, false, err
	}
	return sidecar.Id(resolvedId), true, nil
}

// glog as the intake's sidecar.Logger. The intake logs a reason as a word and never report text, an address
// or a recipient (I-13), and nothing here adds one.
type reportIntakeLogger struct{}

func (self reportIntakeLogger) Info(args ...any) {
	glog.Info(args...)
}

func (self reportIntakeLogger) Infof(format string, args ...any) {
	glog.Infof(format, args...)
}

func (self reportIntakeLogger) Warningf(format string, args ...any) {
	glog.Warningf(format, args...)
}

func (self reportIntakeLogger) Errorf(format string, args ...any) {
	glog.Errorf(format, args...)
}

func (self reportIntakeLogger) V(level int32) sidecar.Verbose {
	return reportIntakeVerbose(glog.V(glog.Level(level)))
}

// glog's verbose level as the intake's sidecar.Verbose.
type reportIntakeVerbose glog.Verbose

func (self reportIntakeVerbose) Enabled() bool {
	return bool(self)
}

func (self reportIntakeVerbose) Info(args ...any) {
	glog.Verbose(self).Info(args...)
}

func (self reportIntakeVerbose) Infof(format string, args ...any) {
	glog.Verbose(self).Infof(format, args...)
}
