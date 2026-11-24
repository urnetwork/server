//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The platform listener's report intake (world_test.go; §14.8, §15.4's platform mail, plan-SMTP-report-intake-home,
// plan-SMTP-report-seal-key, plan-SMTP-net-token): what server/cli/mxedge's main hands mxedge's platform block,
// composed again here since that is package main (its report_intake.go and report_token_source.go), each run of the
// block building its own. sidecar's smtpedgeservice.SmtpReportIntake on model's Postgres store with smtpedge.yml's
// names, the world's smtpedge's, which must be mxedge.yml's platform root and report host, as main holds them, and the
// listener's mxedgeservice.PlatformIntake in front of it:
//
//	tokens   network_dns_zone's unique index on report_token over the world's Postgres
//	         (model.GetDnsNetworkForReportToken), where the binary links MG6, a read that raised answered as an error
//	         so the listener answers 451
//	labels   §12's label map for <root>'s rows (model.ResolveNetworkLabel), a retiring alias's included
//	keys     api's report-key route (GET /internal/gate/report-key/<network_id>) on the platform listener's own gate
//	         bearer, at api's origin in process (smtpedgeservice.SmtpReportKeyGateSource), which the world's internal
//	         gate serves as api's main does (installInternalGate) from the report seal key source the T3World's api
//	         installs over its gate cache, gkm at the world's gkm block
//	sink     §21's real one (model.NewAbuseSmtpSink)
//	txt      reporters' DKIM keys from the world's DNS above its root (world_mail_dns_test.go), asked as the plain DoH a
//	         receiver's checks ask, where main's intake asks the process's resolver (the World's header), so the
//	         internet sender's reports verify under its zone's key
//
// What main gives the intake beside these and the world does not, each closed as main's intake is without it: role
// mail's notice check at api on the same bearer (main's platformNoticeFiler), so no role mail files a notice; and
// SMTP-M9-15's join and SMTP-M9-16's ladder over the X-UR-Ref keys (main's report_join.go), so no ARF report or role
// mail is attributed, and every role mail is answered unread.
//
// Every intake of the world's counts its answers by outcome (World.PlatformReportOutcomes), what a report case
// reconciles its deliveries against.

package sidecare2e

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"

	"github.com/miekg/dns"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/mxedgehost"
	"github.com/urnetwork/server/sidecarhost"
)

// What a call the intake could not finish is counted under: an error, which the listener answers 451 4.3.0.
const worldReportOutcomeFailed smtpedgeservice.SmtpReportOutcome = "failed"

// What the platform's intakes answered across the block's runs, by outcome, and how many calls they could not finish,
// which a scenario waits on. Safe for concurrent use.
type worldReportOutcomes struct {
	stateLock     sync.Mutex
	outcomeCounts map[smtpedgeservice.SmtpReportOutcome]int

	failureCount *sidecar.MonitorValue[int]
}

func newWorldReportOutcomes() *worldReportOutcomes {
	return &worldReportOutcomes{
		outcomeCounts: map[smtpedgeservice.SmtpReportOutcome]int{},
		failureCount:  sidecar.NewMonitorValue(0),
	}
}

func (self *worldReportOutcomes) add(outcome smtpedgeservice.SmtpReportOutcome) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.outcomeCounts[outcome] += 1
	}()
	if outcome == worldReportOutcomeFailed {
		self.failureCount.Update(func(count int) int {
			return count + 1
		})
	}
}

func (self *worldReportOutcomes) counts() map[smtpedgeservice.SmtpReportOutcome]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return maps.Clone(self.outcomeCounts)
}

// A copy of what the platform's intakes answered so far, by outcome, a call that could not finish under
// worldReportOutcomeFailed.
func (self *World) PlatformReportOutcomes() map[smtpedgeservice.SmtpReportOutcome]int {
	return self.reportOutcomes.counts()
}

// How many calls the platform's intakes could not finish so far, and a channel that closes at the next.
func (self *World) PlatformReportFailures() (int, chan struct{}) {
	return self.reportOutcomes.failureCount.Get()
}

// A name as both files hold it: lowercase, with no final dot.
func worldReportName(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

// The platform block's intake for one run and the pending groups its readiness waits on, as main composes them (the
// file's header): refused, as main refuses it, where smtpedge.yml's root or report host is not mxedge's platform_root
// or platform_report_host, since the listener takes recipients by the one and the intake routes them by the other.
func (self *World) platformReportIntake(mxEdge *mxedgeservice.MxEdgeSettings) (mxedgeservice.PlatformIntake, []*sidecarhost.RunGroup, error) {
	hostsSettings := self.HostsSettings()
	// smtpedge.yml as the platform block reads it beside mxedge.yml: the world's smtpedge's names (runSmtpedge)
	settings, err := smtpedgeservice.LoadSmtpEdgeSettings([]byte(fmt.Sprintf(
		"root: %s\nreport_host: %s\nhelo_domain: %s\nsending_addresses:\n  - {address: %s, helo: %s}\n",
		hostsSettings.SmtpRoot, hostsSettings.SmtpReportHost, hostsSettings.SmtpHeloDomain, hostsSettings.SmtpAddress, hostsSettings.SmtpHeloName,
	)))
	if err != nil {
		return nil, nil, fmt.Errorf("the platform intake's smtpedge.yml: %w", err)
	}
	if worldReportName(settings.Root) != worldReportName(mxEdge.PlatformRootDomainName) {
		return nil, nil, errors.New("the platform intake: smtpedge.yml's root is not mxedge.yml's platform_root")
	}
	if worldReportName(settings.ReportHostName) != worldReportName(mxEdge.PlatformReportHostName) {
		return nil, nil, errors.New("the platform intake: smtpedge.yml's report_host is not mxedge.yml's platform_report_host")
	}
	keys, err := smtpedgeservice.NewSmtpReportKeyGateSource(&smtpedgeservice.SmtpReportKeyGateSourceArgs{
		ApiUrl:     self.apiOrigin(),
		Bearer:     self.bearer(server.InternalGateCallerMxedgePlatform),
		HttpClient: self.apiClient(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("the platform intake's key source: %w", err)
	}
	// main's host: the process's metrics and real time
	host := &sidecar.ServiceHost{
		Metrics: sidecarhost.NewMetrics(),
		Clock:   sidecar.RealClock{},
	}
	intake, err := smtpedgeservice.NewSmtpReportIntake(host, model.NewSmtpEdgeStore(nil), &smtpedgeservice.SmtpReportIntakeDeps{
		Tokens: worldReportTokenSource{},
		Labels: &worldReportLabelSource{root: settings.Root},
		Keys:   keys,
		Txt:    &worldReportTxtSource{doh: self.dnssec},
		Sink:   model.NewAbuseSmtpSink(nil),
		Log:    mxedgehost.HostLogger(),
	}, settings)
	if err != nil {
		return nil, nil, fmt.Errorf("the platform intake: %w", err)
	}
	groups := []*sidecarhost.RunGroup{
		// the intake's own: MG2's dns_label through the label map, and MG5's report tables
		{Name: "MG2", Linked: server.SidecarMg2Linked},
		{Name: "MG5", Linked: server.SidecarMg5Linked},
		// the tokens': MG6's network_dns_zone.report_token
		{Name: "MG6", Linked: server.SidecarMg6Linked},
	}
	return &worldPlatformIntake{intake: intake, root: worldReportName(settings.Root), outcomes: self.reportOutcomes}, groups, nil
}

// The intake as the platform listener reaches it, as main's platformReportIntake with no notice check: role mail the
// intake could not make a case of is unread, which the listener answers with plan-ABUSE-auto-reply's 550 when every
// recipient's ends so, and everything else it finished with is taken. Safe for concurrent use, as the intake is.
type worldPlatformIntake struct {
	intake *smtpedgeservice.SmtpReportIntake
	// <root>, lowercased, whose abuse@ and postmaster@ are role mail
	root     string
	outcomes *worldReportOutcomes
}

var _ mxedgeservice.PlatformIntake = (*worldPlatformIntake)(nil)

// See mxedgeservice.PlatformIntake.
func (self *worldPlatformIntake) Accept(ctx context.Context, message *mxedgeservice.PlatformMessage) (mxedgeservice.PlatformOutcome, error) {
	outcome, err := self.intake.Accept(ctx, &smtpedgeservice.SmtpReportMessage{
		Recipient:   message.Recipient,
		ArrivalTime: message.ArrivalTime,
		Raw:         message.Raw,
	})
	if err != nil {
		self.outcomes.add(worldReportOutcomeFailed)
		return 0, err
	}
	self.outcomes.add(outcome)
	local, domain, ok := strings.Cut(strings.ToLower(message.Recipient), "@")
	role := ok && (local == "abuse" || local == "postmaster") && strings.TrimSuffix(domain, ".") == self.root
	if role && outcome != smtpedgeservice.SmtpReportOutcomeCase {
		return mxedgeservice.PlatformOutcomeUnread, nil
	}
	return mxedgeservice.PlatformOutcomeTaken, nil
}

// network_dns_zone's index on report_token as the intake reads it, over the world's Postgres pool, as main's
// reportTokenSource reads it over the block's.
type worldReportTokenSource struct{}

var _ smtpedgeservice.SmtpNetTokenSource = worldReportTokenSource{}

// See smtpedgeservice.SmtpNetTokenSource. A binary that does not link MG6 answers an error, never no network.
func (self worldReportTokenSource) NetworkForReportToken(ctx context.Context, token string) (networkId sidecar.Id, ok bool, returnErr error) {
	if !server.SidecarMg6Linked() {
		return sidecar.Id{}, false, errors.New("report intake: this binary does not link MG6, whose network_dns_zone.report_token a report token is")
	}
	defer func() {
		// the model raises a failed read, which reaches the intake as an error, never a raise past the listener's call
		if r := recover(); r != nil {
			networkId, ok = sidecar.Id{}, false
			returnErr = fmt.Errorf("report intake: the report token index: %v", r)
		}
	}()
	resolvedId, found := model.GetDnsNetworkForReportToken(ctx, token)
	if !found {
		return sidecar.Id{}, false, nil
	}
	return sidecar.Id(resolvedId), true, nil
}

// §12's label map as the intake reads <root>'s rows, as main's reportLabelSource: the network a <net> label or alias
// names in root. A name nothing holds is no network; a read that failed is an error, so the reporter retries.
type worldReportLabelSource struct {
	root string
}

var _ smtpedgeservice.SmtpLabelSource = (*worldReportLabelSource)(nil)

// See smtpedgeservice.SmtpLabelSource.
func (self *worldReportLabelSource) NetworkForLabel(ctx context.Context, label string) (networkId sidecar.Id, ok bool, returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			networkId, ok = sidecar.Id{}, false
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

// Reporters' DKIM keys as the intake reads them: TXT over the world's plain DoH (the file's header). A name the world
// answers NXDOMAIN or NODATA for holds none; an answer that is not usable, or a name the world serves no zone for, is
// DNS not answering, which RFC 6376 has the intake read as temperror.
type worldReportTxtSource struct {
	doh sidecar.MailDohForwarder
}

var _ smtpedgeservice.SmtpDnsTxtSource = (*worldReportTxtSource)(nil)

// See smtpedgeservice.SmtpDnsTxtSource.
func (self *worldReportTxtSource) LookupTxt(ctx context.Context, name string) ([]string, error) {
	response, usable := self.doh.Forward(ctx, dnsmessage.TypeTXT, strings.TrimSuffix(name, "."))
	if !usable {
		return nil, errors.New("report intake: the world's DNS gave no usable answer")
	}
	answer := &dns.Msg{}
	if err := answer.Unpack(response); err != nil {
		return nil, fmt.Errorf("report intake: the world's DNS answer: %w", err)
	}
	switch answer.Rcode {
	case dns.RcodeNameError:
		return nil, nil
	case dns.RcodeSuccess:
	default:
		return nil, fmt.Errorf("report intake: the world's DNS answered %s", dns.RcodeToString[answer.Rcode])
	}
	asked := dns.CanonicalName(dns.Fqdn(name))
	records := []string{}
	for _, record := range answer.Answer {
		if txt, ok := record.(*dns.TXT); ok && dns.CanonicalName(txt.Hdr.Name) == asked {
			records = append(records, strings.Join(txt.Txt, ""))
		}
	}
	return records, nil
}
