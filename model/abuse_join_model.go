package model

// §21.8's pipeline, as far as the store (ABUSE-M5-09; D-ABUSE-5, R-ABUSE-2,
// R-ABUSE-6): a report, the join that ties it to a network, and the case it
// opens.
//
// JoinAbuseReport tries, in order:
//
//  1. a host, from a reported url or a bare name, or a mail address, through
//     §12's label map (ResolveNetworkLabel): shop.<label>.<root> names
//     <label>'s network, by its label or an alias, a retiring one included, and
//     a burner address names its <net> (sidecar.ParseMailRecipient);
//  2. a header block, by §14.8's header-only parse
//     (smtpedgeservice.ParseHeaderBlock): until SMTP-M9-15's attribution reads
//     send records, the block joins only by a DKIM d= or From domain under a
//     root, through the same label map;
//  3. an edge connection_id against tls_connection_record, within
//     AbuseSettings.ConnectionJoinWindow of the connection's end (tls-3 only:
//     mx-2 has no connection id).
//
// Third-party data dies at the join. What a join keeps is api-14's list: the
// category, the target host, label, sending domain or network, the join's kind
// and key, and the reporter class. Of a url only its host is ever read: no
// path and no query reaches a row or a log, and reviewers open the host
// (plan-ABUSE-small-readings #8's interim, until abuse_case has a column for
// owner decision 207's path). Of an address only its domain is kept, never the
// local part. AbuseReport has no field for a reporter's address or for report
// text, so neither can be stored, and nothing here logs a report's values
// (I-13).
//
// A report that names no URnetwork host, address or connection joins nothing:
// JoinAbuseReport answers nil, no row is written, and a caller answers its
// reporter the same either way, so a report is no oracle for which names exist
// (plan-ABUSE-small-readings #7).
//
// OpenAbuseReportCase opens the joined report's case on its §21.8 clock, or
// counts the report on the open case it coalesces with. Machine actions are the
// controller's (controller.AcceptAbuseReport).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// A report no case could hold: an unknown category, or a reporter class a
// report may not claim. The message names nothing of the report (I-13).
var ErrAbuseReportInvalid = errors.New("abuse report invalid")

// One report, from the form, a feed, a scanner or the mail sink. It carries
// only what a join reads: there is no field for a reporter's address, a client
// address or free text (R-ABUSE-6).
type AbuseReport struct {
	Category      AbuseCategory
	ReporterClass AbuseReporterClass
	// A reported url, or a bare host name. Only its host is read.
	Url string
	// A reported mail address under a root. Only its domain is kept.
	Address string
	// A header block as the form takes it, read by §14.8's header-only parse:
	// nothing past the block's blank line is read.
	HeaderBlock []byte
	// An edge connection id (tls-3), as tlsedge showed it.
	ConnectionId string
}

// What tied a report to a network.
type AbuseReportJoin struct {
	NetworkId server.Id
	JoinKind  AbuseJoinKind
	// the host, sending domain or connection id the join matched
	JoinKey string
	// the unit the case names
	Target AbuseTarget
	// <label>.<root> of the network's label or alias the join went through, or
	// "" when it went through none
	LabelName string
	// the report named a url whose host the join resolved (AbuseSignalOfReport)
	NamesUrl bool
}

// Counts reports by how they ended at the join: by a join's kind, or unjoined.
// No label carries a name.
var abuseReportJoinCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "abuse",
		Name:      "report_joins_total",
		Help:      "Abuse reports by the kind of join that tied them to a network, or unjoined",
	},
	[]string{"result"},
)

// Registers the join counter with the default registry.
func init() {
	prometheus.MustRegister(abuseReportJoinCounter)
}

// A name as the policy compares it: case, spaces and a trailing dot are no
// difference.
func AbuseName(name string) string {
	return abuseName(name)
}

// The reporter classes a report may claim. A registrar, a registry, law
// enforcement and staff file a notice or act through the staff route, never
// through a report, so a forged sender pages no one and kills nothing
// (plan-ABUSE-notice-intake).
func abuseReportClassAllowed(class AbuseReporterClass) bool {
	switch class {
	case AbuseReporterClassPublic, AbuseReporterClassRoleMail, AbuseReporterClassMailReport,
		AbuseReporterClassWebRisk, AbuseReporterClassFeed, AbuseReporterClassCtWatch,
		AbuseReporterClassSearchConsole, AbuseReporterClassTrustedFlagger:
		return true
	}
	return false
}

// Refuses a report no case could hold.
func (self *AbuseReport) Validate() error {
	if self == nil {
		return fmt.Errorf("%w: no report", ErrAbuseReportInvalid)
	}
	if !abuseCategoryKnown(self.Category) {
		return fmt.Errorf("%w: an unknown category", ErrAbuseReportInvalid)
	}
	if !abuseReportClassAllowed(self.ReporterClass) {
		return fmt.Errorf("%w: a class no report may claim", ErrAbuseReportInvalid)
	}
	return nil
}

// A name under one of the roots, split: the root as the label map keys it, the
// label right under it, and the labels below that, joined, or "" at the apex.
// ok false for a name under no root, or equal to one.
func abuseNameUnderRoot(name string, rootNames []string) (root string, label string, host string, ok bool) {
	roots := slices.Clone(rootNames)
	// the longest root first, so a root under another root is matched whole
	slices.SortFunc(roots, func(a string, b string) int {
		return len(b) - len(a)
	})
	for _, rootName := range roots {
		rootName = abuseName(rootName)
		if rootName == "" {
			continue
		}
		under, found := strings.CutSuffix(name, "."+rootName)
		if !found || under == "" {
			continue
		}
		labels := strings.Split(under, ".")
		return rootName, labels[len(labels)-1], strings.Join(labels[:len(labels)-1], "."), true
	}
	return "", "", "", false
}

// The host a reported url names, lowercase with no trailing dot, or "" when it
// names none. Everything but the host is dropped here, unread: the scheme, any
// userinfo, the port, the path, the query and the fragment. A reporter's
// defanged form, hxxp and [.], reads as the name it stands for.
func abuseReportUrlHost(reportUrl string, settings *AbuseSettings) string {
	if reportUrl == "" || settings.ReportUrlMaxByteCount < len(reportUrl) {
		return ""
	}
	host := strings.TrimSpace(reportUrl)
	// a scheme is only what leads the url: a :// further in is the path's or
	// the query's, and is dropped with them
	if scheme, rest, found := strings.Cut(host, "://"); found && abuseUrlScheme(scheme) {
		host = rest
	}
	if end := strings.IndexAny(host, "/?#"); 0 <= end {
		host = host[:end]
	}
	if at := strings.LastIndexByte(host, '@'); 0 <= at {
		host = host[at+1:]
	}
	if port := strings.LastIndexByte(host, ':'); 0 <= port {
		host = host[:port]
	}
	host = abuseName(strings.ReplaceAll(host, "[.]", "."))
	if !abuseHostNameValid(host, settings) {
		return ""
	}
	return host
}

// Whether text can be a url's scheme: letters, digits, + and -.
func abuseUrlScheme(text string) bool {
	if text == "" {
		return false
	}
	for i := range len(text) {
		c := text[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '+' || c == '-') {
			return false
		}
	}
	return true
}

// Whether name is a host name a join may look up: lowercase LDH labels within
// the settings' lengths, two or more. Anything else, an address literal
// included, names no URnetwork host.
func abuseHostNameValid(name string, settings *AbuseSettings) bool {
	if name == "" || settings.HostNameMaxByteCount < len(name) {
		return false
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return false
	}
	// an address literal ends in digits, which no top-level name does
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return false
	}
	for _, label := range labels {
		if label == "" || settings.HostLabelMaxByteCount < len(label) || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// The network a label or alias names in root, ok false when none does.
func abuseLabelNetwork(ctx context.Context, root string, label string) (networkId server.Id, ok bool, returnErr error) {
	networkId, _, err := ResolveNetworkLabel(ctx, root, label, false)
	if errors.Is(err, sidecar.ErrNetworkLabelUnknown) {
		return server.Id{}, false, nil
	}
	if err != nil {
		return server.Id{}, false, err
	}
	return networkId, true, nil
}

// The join of a name under a root through the label map, or nil.
func abuseJoinName(ctx context.Context, name string, settings *AbuseSettings) (*AbuseReportJoin, error) {
	root, label, host, ok := abuseNameUnderRoot(name, settings.RootNames)
	if !ok {
		return nil, nil
	}
	networkId, ok, err := abuseLabelNetwork(ctx, root, label)
	if err != nil || !ok {
		return nil, err
	}
	join := &AbuseReportJoin{
		NetworkId: networkId,
		JoinKey:   name,
		LabelName: label + "." + root,
		Target:    AbuseTarget{Kind: AbuseTargetKindHost, Name: name},
	}
	if host == "" {
		// the apex names the label itself
		join.Target.Kind = AbuseTargetKindLabel
	}
	return join, nil
}

// §14.8's parse bounds for a header block the form takes, under root. The
// parse reads the root only to know a setting is whole.
func abuseHeaderParseSettings(root string) *smtpedgeservice.SmtpEdgeSettings {
	settings := smtpedgeservice.DefaultSmtpEdgeSettings()
	settings.Root = root
	settings.ReportHostName = root
	return settings
}

// A pasted block's lines as the parse reads them: a bare LF becomes CRLF,
// inside the parse's own bound, so nothing past the bound is touched.
func abuseHeaderBlockLines(block []byte, maxByteCount int) []byte {
	if maxByteCount < len(block) {
		block = block[:maxByteCount]
	}
	block = bytes.ReplaceAll(block, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(block, []byte("\n"), []byte("\r\n"))
}

// Ties a report to a network, in the file header's order, at now. nil when the
// report joins nothing: no row follows, and the caller's answer is the same.
// An error is a store that did not answer, never a report's fault.
func JoinAbuseReport(ctx context.Context, report *AbuseReport, now time.Time, settings *AbuseSettings) (join *AbuseReportJoin, returnErr error) {
	defer func() {
		result := "unjoined"
		switch {
		case returnErr != nil:
			result = "failed"
		case join != nil:
			result = string(join.JoinKind)
		}
		abuseReportJoinCounter.WithLabelValues(result).Inc()
	}()
	if report == nil || len(settings.RootNames) == 0 {
		// with no roots no name is a URnetwork name
		return nil, nil
	}

	// 1. a host, then an address
	if host := abuseReportUrlHost(report.Url, settings); host != "" {
		join, err := abuseJoinName(ctx, host, settings)
		if err != nil {
			return nil, err
		}
		if join != nil {
			join.JoinKind = AbuseJoinKindHost
			join.NamesUrl = true
			return join, nil
		}
	}
	if report.Address != "" {
		for _, rootName := range settings.RootNames {
			recipient, err := sidecar.ParseMailRecipient(abuseName(rootName), report.Address)
			if err != nil {
				continue
			}
			networkId, ok, err := abuseLabelNetwork(ctx, recipient.Zone.Root, recipient.Zone.Net)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			domain := abuseName(recipient.Domain())
			return &AbuseReportJoin{
				NetworkId: networkId,
				JoinKind:  AbuseJoinKindAddress,
				JoinKey:   domain,
				LabelName: abuseName(recipient.Zone.Net + "." + recipient.Zone.Root),
				Target:    AbuseTarget{Kind: AbuseTargetKindSendingDomain, Name: domain},
			}, nil
		}
	}

	// 2. headers: a DKIM d= domain, then the From domain, under a root
	if 0 < len(report.HeaderBlock) {
		parseSettings := abuseHeaderParseSettings(abuseName(settings.RootNames[0]))
		block := abuseHeaderBlockLines(report.HeaderBlock, parseSettings.ReportHeaderByteCount)
		if headers, err := smtpedgeservice.ParseHeaderBlock(block, parseSettings); err == nil && headers != nil {
			domains := []string{}
			for _, signature := range headers.DkimSignatures {
				domains = append(domains, signature.Domain)
			}
			domains = append(domains, headers.FromDomain)
			for _, domain := range domains {
				domain = abuseName(domain)
				if domain == "" {
					continue
				}
				join, err := abuseJoinName(ctx, domain, settings)
				if err != nil {
					return nil, err
				}
				if join != nil {
					join.JoinKind = AbuseJoinKindHeaders
					join.Target.Kind = AbuseTargetKindSendingDomain
					return join, nil
				}
			}
		}
	}

	// 3. an edge connection id, inside its window
	if report.ConnectionId != "" && 0 < settings.ConnectionJoinWindow && server.SidecarMg5Linked() {
		connectionId, err := server.ParseId(strings.TrimSpace(report.ConnectionId))
		if err == nil {
			networkId, host, ok := abuseConnectionNetwork(ctx, connectionId, now, settings)
			if ok {
				join := &AbuseReportJoin{
					NetworkId: networkId,
					JoinKind:  AbuseJoinKindConnection,
					JoinKey:   connectionId.String(),
					Target:    AbuseTarget{Kind: AbuseTargetKindNetwork, Name: networkId.String()},
				}
				// the record's host is relative to the network's zone: name it
				// under the network's label where it has one
				for _, rootName := range settings.RootNames {
					label := GetNetworkLabel(ctx, networkId, abuseName(rootName))
					if label == "" {
						continue
					}
					join.LabelName = label + "." + abuseName(rootName)
					if host == "@" {
						join.Target = AbuseTarget{Kind: AbuseTargetKindLabel, Name: join.LabelName}
					} else {
						join.Target = AbuseTarget{Kind: AbuseTargetKindHost, Name: host + "." + join.LabelName}
					}
					break
				}
				return join, nil
			}
		}
	}
	return nil, nil
}

// The network and published host of an edge connection that ended inside the
// join window at now, ok false for none. The window's start bounds the
// partitions the read touches.
func abuseConnectionNetwork(
	ctx context.Context,
	connectionId server.Id,
	now time.Time,
	settings *AbuseSettings,
) (networkId server.Id, host string, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT tls_connection_record.network_id, tls_connection_record.host
				FROM tls_connection_record
				WHERE
					tls_connection_record.connection_id = $1 AND
					$2 < tls_connection_record.end_time AND
					tls_connection_record.end_time <= $3
				ORDER BY tls_connection_record.end_time DESC
				LIMIT 1
			`,
			connectionId,
			now.Add(-settings.ConnectionJoinWindow).UTC(),
			now.UTC(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId, &host))
				ok = true
			}
		})
	})
	return
}

// A network's tier and standing, for §21.8's last row. Unknown values take the
// short clock (AbuseReviewDue).
type AbuseReportStandingSource func(ctx context.Context, networkId server.Id) (AbuseTier, AbuseStanding)

// The process's standing source, set at startup.
var abuseReportStanding = struct {
	stateLock sync.Mutex
	source    AbuseReportStandingSource
}{}

// Installs what reads a network's tier and standing for a report's review
// clock (ABUSE-M5-07's GetNetworkAbuseStanding), returning what puts the
// previous one back. With none, every report against a network reviews on the
// short clock, since a standing nothing knows reviews early.
func SetAbuseReportStandingSource(source AbuseReportStandingSource) (restore func()) {
	abuseReportStanding.stateLock.Lock()
	defer abuseReportStanding.stateLock.Unlock()
	previous := abuseReportStanding.source
	abuseReportStanding.source = source
	return func() {
		abuseReportStanding.stateLock.Lock()
		defer abuseReportStanding.stateLock.Unlock()
		abuseReportStanding.source = previous
	}
}

// The network's tier and standing, unknown with no source.
func abuseReportStandingOf(ctx context.Context, networkId server.Id) (AbuseTier, AbuseStanding) {
	abuseReportStanding.stateLock.Lock()
	source := abuseReportStanding.source
	abuseReportStanding.stateLock.Unlock()
	if source == nil {
		return "", ""
	}
	return source(ctx, networkId)
}

// What became of a joined report.
type AbuseReportCase struct {
	CaseId server.Id
	// false when the report joined a case already open
	Opened bool
	// the §21.8 row the report sits on
	Signal AbuseSignal
	// when its review falls due
	ReviewDueTime time.Time
	// a CSAM case bars every notice (§21.8)
	Barred bool
}

// The review calendar in force: abuse.yml's, or the default's when the file's
// block is refused.
func abuseReportCalendar(settings *AbuseSettings) *AbuseCalendar {
	calendar, err := NewAbuseCalendar(AbuseReviewHoursOf(server.GetAbuseConfig()), &settings.AbusePolicySettings)
	if err == nil {
		return calendar
	}
	calendar, err = NewAbuseCalendar(DefaultAbuseReviewHours(), &settings.AbusePolicySettings)
	if err != nil {
		panic(err)
	}
	return calendar
}

// Opens the case of a joined report at now, on signal's clock, or counts the
// report on the open case of the same network, target and category
// (OpenAbuseCaseInTx). signal is AbuseSignalOfReport's for a report, or
// §14.8's own for a mail join. A CSAM case is barred.
func OpenAbuseReportCase(
	ctx context.Context,
	report *AbuseReport,
	join *AbuseReportJoin,
	signal AbuseSignal,
	now time.Time,
	settings *AbuseSettings,
) (*AbuseReportCase, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	if err := report.Validate(); err != nil {
		return nil, err
	}
	if join == nil {
		return nil, fmt.Errorf("%w: no join", ErrAbuseReportInvalid)
	}
	tier, standing := abuseReportStandingOf(ctx, join.NetworkId)
	reportCase := &AbuseReportCase{
		Signal:        signal,
		ReviewDueTime: AbuseReviewDue(signal, tier, standing, now, abuseReportCalendar(settings)),
		Barred:        report.Category == AbuseCategoryCsam,
	}
	networkId := join.NetworkId
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		reportCase.CaseId, reportCase.Opened, err = OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      report.Category,
			Target:        join.Target,
			ReporterClass: report.ReporterClass,
			JoinKind:      join.JoinKind,
			JoinKey:       join.JoinKey,
			Barred:        reportCase.Barred,
			ReviewDueTime: reportCase.ReviewDueTime,
		}, now, settings)
	})
	if err != nil {
		return nil, err
	}
	return reportCase, nil
}

// One network's recorded actions as the ladders read them (NextAbuseStep),
// oldest first. Nothing in a binary that does not link MG5.
func GetNetworkAbuseHistory(ctx context.Context, networkId server.Id) (history []AbuseHistoryAction) {
	history = []AbuseHistoryAction{}
	if !server.SidecarMg5Linked() {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+abuseActionColumns+`
				FROM abuse_action
				WHERE abuse_action.network_id = $1
				ORDER BY abuse_action.create_time, abuse_action.action_id
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				action := scanAbuseAction(result)
				historyAction := AbuseHistoryAction{
					CaseId:      action.CaseId,
					Category:    action.Category,
					Kind:        action.Kind,
					Target:      action.Target,
					ReviewState: action.ReviewState,
					CreateTime:  action.CreateTime,
				}
				if action.UntilTime != nil {
					historyAction.UntilTime = *action.UntilTime
				}
				history = append(history, historyAction)
			}
		})
	})
	return
}
