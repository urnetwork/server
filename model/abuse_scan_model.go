package model

// The abuse worker's scanners in the store (ABUSE-M5-13; §21.6, §21.8, §2.8 ext-6, I-13). The scans themselves are
// taskworker's (work/abuse_webrisk_work.go, abuse_feed_work.go, abuse_ct_work.go); what they read and write here:
//
//   - ListAbuseScanNames pages what §21.6's hourly check looks up against Web Risk: the published hosts
//     (ListTlsPublishedNames), then the owners of the user records (network_dns_record), each under one of the
//     roots, by a cursor, so a page that fails is read again and the scan does not start over.
//   - GetAbuseScanLiftedWrongTime answers when review last lifted as wrong a machine's action on a case joined by a
//     host. Feed and Web Risk hits recur, which §21.10's restore gap relies on, so a scanner checks this before it
//     reports a host again: a listing that has not changed since the lift is a false positive a person already
//     judged, and is not killed again on the next pass.
//   - OpenAbuseRootIncident records a scanner's hit on a root itself, or on a name above one, as a case with no
//     network. No ladder acts on a root (D-ABUSE-1), so it kills nothing; §21.11's report counts it.
//   - GetAbuseScanScreenFacts gives the CT watch the facts ScreenName and ScreenHost read for a network, and
//     GetAbuseScanLabelNetwork the network a label or alias under a root names.
//   - AbuseScanHost reads a feed's line as a report's url is read: its host and nothing else.
//
// Nothing here logs a name (I-13). The record names and every abuse table are MG5's, and nothing reads them unless
// the binary links MG5.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// Tunables of ListAbuseScanNames.
type AbuseScanSettings struct {
	// The most published names one page reads, before those under no root are dropped.
	PublishedPageCount int
	// The networks one published page reads from network_acl_head at a time.
	PublishedNetworkBatchCount int
	// The most network_dns_record rows one page examines, of every kind, so a page's work is bounded by the rows it
	// reads and not only by the names it keeps (CODESTYLE).
	RecordBatchCount int
}

// Pages of a thousand: a page is one round of lookups, and a failed one is read again whole.
func DefaultAbuseScanSettings() *AbuseScanSettings {
	return &AbuseScanSettings{
		PublishedPageCount:         1000,
		PublishedNetworkBatchCount: 200,
		RecordBatchCount:           1000,
	}
}

// One name §21.6's hourly check looks up.
type AbuseScanName struct {
	NetworkId server.Id
	// lowercase, with no trailing dot, under one of the roots
	Name string
}

// Where a page of ListAbuseScanNames resumes: in the published names, then in the records.
type AbuseScanNamesCursor struct {
	// The published names are done, and the records resume after RecordRoot, RecordOwner and RecordType.
	Records bool
	// After this published name; nil from the published names' start.
	Published *TlsPublishedNamesCursor
	// After this network_dns_record row, in its key's order; empty from the records' start.
	RecordRoot  string
	RecordOwner string
	RecordType  int
}

// Whether name lies under one of rootNames, and is not one. Names compare as the policy compares them.
func AbuseScanUnderRoot(name string, rootNames []string) bool {
	name = abuseName(name)
	for _, rootName := range rootNames {
		if rootName = abuseName(rootName); rootName != "" && strings.HasSuffix(name, "."+rootName) {
			return true
		}
	}
	return false
}

// The root of rootNames that name stands for or covers: the root itself, or a name a root lies under, as a
// listing of that name covers every name below it. ok false for any other name, a name under a root included.
func AbuseScanCoveredRoot(name string, rootNames []string) (root string, ok bool) {
	name = abuseName(name)
	if name == "" {
		return "", false
	}
	for _, rootName := range rootNames {
		rootName = abuseName(rootName)
		if rootName != "" && (rootName == name || strings.HasSuffix(rootName, "."+name)) {
			return rootName, true
		}
	}
	return "", false
}

// The name a check looks up for a served name: lowercase with no trailing dot, its labels up to the last glob, *
// or underscore label dropped, since no visitor's url names those. "" for a name that is then no host name.
func abuseScanLookupName(name string, settings *AbuseSettings) string {
	labels := strings.Split(abuseName(name), ".")
	start := 0
	for index, label := range labels {
		if strings.Contains(label, "*") || strings.HasPrefix(label, "_") {
			start = index + 1
		}
	}
	lookupName := strings.Join(labels[start:], ".")
	if !abuseHostNameValid(lookupName, settings) {
		return ""
	}
	return lookupName
}

// One page of the names §21.6's hourly check looks up at now, after cursor, nil for the first page: the published
// hosts, then the owners of the user records, each lowercase with no trailing dot and under one of rootNames, each
// once a page. The returned cursor resumes after the page and is nil once both parts are done; a page may hold no
// name while the cursor moves on.
//
// A name's glob, wildcard and underscore labels are dropped from its front, so what is looked up is the most specific
// name a visitor's url can have that the network serves. A custom host under a network's own domain (M6) lies under
// no root and is left out: §21.8's join reads only names under a root, so a hit on one could take no action. A
// record that is withheld, expired or of any kind but user is not a name the network serves of its own.
//
// The published names answer ErrTlsPublishedNamesUnavailable in a process with no source, which the check treats as
// a failed pass (QD-770). An error moves no cursor.
func ListAbuseScanNames(
	ctx context.Context,
	cursor *AbuseScanNamesCursor,
	rootNames []string,
	now time.Time,
	settings *AbuseScanSettings,
) ([]*AbuseScanName, *AbuseScanNamesCursor, error) {
	if cursor == nil {
		cursor = &AbuseScanNamesCursor{}
	}
	nameSettings := DefaultAbuseSettings()
	names := []*AbuseScanName{}
	seenNames := map[string]bool{}
	keep := func(networkId server.Id, servedName string) {
		name := abuseScanLookupName(servedName, nameSettings)
		if name == "" || seenNames[name] || !AbuseScanUnderRoot(name, rootNames) {
			return
		}
		seenNames[name] = true
		names = append(names, &AbuseScanName{NetworkId: networkId, Name: name})
	}

	if !cursor.Records {
		published, publishedNext, err := ListTlsPublishedNamesWithSettings(ctx, cursor.Published, now, &TlsPublishedNamesSettings{
			Limit:             settings.PublishedPageCount,
			NetworkBatchCount: settings.PublishedNetworkBatchCount,
		})
		if err != nil {
			return nil, nil, err
		}
		for _, publishedName := range published {
			if publishedName.Alias != "" || publishedName.Name == "" {
				continue
			}
			keep(publishedName.NetworkId, publishedName.Name)
		}
		if publishedNext == nil {
			return names, &AbuseScanNamesCursor{Records: true}, nil
		}
		return names, &AbuseScanNamesCursor{Published: publishedNext}, nil
	}

	if !server.SidecarMg5Linked() {
		return nil, nil, ErrAbuseUnlinked
	}
	rowCount := 0
	next := &AbuseScanNamesCursor{Records: true}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_dns_record.root,
					network_dns_record.owner,
					network_dns_record.rrtype,
					network_dns_record.network_id,
					network_dns_record.kind,
					network_dns_record.withheld_time IS NOT NULL,
					network_dns_record.expire_time
				FROM network_dns_record
				WHERE (network_dns_record.root, network_dns_record.owner, network_dns_record.rrtype) > ($1, $2, $3)
				ORDER BY network_dns_record.root, network_dns_record.owner, network_dns_record.rrtype
				LIMIT $4
			`,
			cursor.RecordRoot,
			cursor.RecordOwner,
			cursor.RecordType,
			settings.RecordBatchCount,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId *server.Id
				var kind string
				var withheld bool
				var expireTime *time.Time
				server.Raise(result.Scan(
					&next.RecordRoot,
					&next.RecordOwner,
					&next.RecordType,
					&networkId,
					&kind,
					&withheld,
					&expireTime,
				))
				rowCount += 1
				if networkId == nil || kind != string(dnsservice.DnsRecordKindUser) || withheld ||
					(expireTime != nil && !now.Before(*expireTime)) {
					continue
				}
				keep(*networkId, next.RecordOwner)
			}
		})
	})
	if rowCount < settings.RecordBatchCount {
		return names, nil, nil
	}
	return names, next, nil
}

// When review last lifted as wrong a machine's action on a case joined by host (§21.8's host join, whose key is the
// host as the policy compares it), ok false when it never has. A binary that does not link MG5 has none.
func GetAbuseScanLiftedWrongTime(ctx context.Context, host string) (liftTime time.Time, ok bool) {
	if !server.SidecarMg5Linked() {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		var latest *time.Time
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT MAX(abuse_action.review_time)
				FROM abuse_case
				INNER JOIN abuse_action ON abuse_action.case_id = abuse_case.case_id
				WHERE
					abuse_case.join_kind = $1 AND
					abuse_case.join_key = $2 AND
					abuse_action.by_machine AND
					abuse_action.review_state = $3
			`,
			string(AbuseJoinKindHost),
			abuseName(host),
			string(AbuseReviewStateLiftedWrong),
		).Scan(&latest))
		if latest != nil {
			liftTime = latest.UTC()
			ok = true
		}
	})
	return
}

// A root incident to open.
type AbuseRootIncidentArgs struct {
	// the root the hit stands for or covers (AbuseScanCoveredRoot)
	Root          string
	Category      AbuseCategory
	ReporterClass AbuseReporterClass
	// the hit's §21.8 row, which sets the review clock
	Signal AbuseSignal
}

// A root incident no row may hold. The message names nothing of it (I-13).
var ErrAbuseRootIncidentInvalid = errors.New("abuse root incident invalid")

// Opens a root incident at now (§21.6, §21.11): a case with no network, its target and join key the root, on the
// hit's review clock, or counts the hit on the open incident of the same root, category and reporter class opened
// inside CaseCoalesceWindow, adding no row, so an hourly scan of one listing is one incident. No action follows:
// every ladder refuses a root (D-ABUSE-1). Two hits racing on one root serialize on a transaction-scoped lock.
func OpenAbuseRootIncident(
	ctx context.Context,
	args *AbuseRootIncidentArgs,
	now time.Time,
	settings *AbuseSettings,
) (caseId server.Id, opened bool, returnErr error) {
	if !server.SidecarMg5Linked() {
		return server.Id{}, false, ErrAbuseUnlinked
	}
	root := abuseName(args.Root)
	if root == "" || !abuseCategoryKnown(args.Category) || !abuseReportClassAllowed(args.ReporterClass) {
		return server.Id{}, false, fmt.Errorf("%w: no root, or an unknown category or class", ErrAbuseRootIncidentInvalid)
	}
	reviewDueTime := AbuseReviewDue(args.Signal, "", "", now, abuseReportCalendar(settings))
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended('abuse_root_incident:' || $1 || ':' || $2 || ':' || $3, 0))`,
			root,
			string(args.Category),
			string(args.ReporterClass),
		))
		if 0 < settings.CaseCoalesceWindow {
			found := false
			result, err := tx.Query(
				ctx,
				`
					SELECT abuse_case.case_id
					FROM abuse_case
					WHERE
						abuse_case.join_kind = $1 AND
						abuse_case.join_key = $2 AND
						abuse_case.network_id IS NULL AND
						abuse_case.category = $3 AND
						abuse_case.reporter_class = $4 AND
						abuse_case.state = $5 AND
						$6 <= abuse_case.create_time
					ORDER BY abuse_case.create_time DESC, abuse_case.case_id DESC
					LIMIT 1
					FOR UPDATE
				`,
				string(AbuseJoinKindHost),
				root,
				string(args.Category),
				string(args.ReporterClass),
				string(AbuseCaseStateOpen),
				now.Add(-settings.CaseCoalesceWindow).UTC(),
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&caseId))
					found = true
				}
			})
			if found {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						UPDATE abuse_case
						SET review_due_time = LEAST(COALESCE(abuse_case.review_due_time, $2), $2)
						WHERE abuse_case.case_id = $1
					`,
					caseId,
					reviewDueTime.UTC(),
				))
				return
			}
		}
		caseId, opened, returnErr = OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
			Category:      args.Category,
			Target:        AbuseTarget{Kind: AbuseTargetKindHost, Name: root},
			ReporterClass: args.ReporterClass,
			JoinKind:      AbuseJoinKindHost,
			JoinKey:       root,
			ReviewDueTime: reviewDueTime,
		}, now, settings)
	})
	return
}

// The facts ScreenName and ScreenHost read for a live name of the network at now, as the CT watch screens it: its
// verified domains (none before MG6), and probation for a network on probation or restricted. ReviewAllowed stays
// false until ABUSE-M5-08's screen_allow exists, so a brand with a keyword reads as a name the lists refuse.
func GetAbuseScanScreenFacts(ctx context.Context, networkId server.Id, now time.Time) *sidecar.ScreenFacts {
	facts := &sidecar.ScreenFacts{
		VerifiedDomains: GetNetworkVerifiedDomains(ctx, networkId),
	}
	if standing, found := GetNetworkAbuseStanding(ctx, networkId, now); found {
		switch standing.Standing {
		case AbuseStandingProbation, AbuseStandingRestricted:
			facts.Probation = true
		}
	}
	return facts
}

// The network a label or alias names under root (ResolveNetworkLabel, a retiring alias included), ok false when
// none does. An error is the store not answering.
func GetAbuseScanLabelNetwork(ctx context.Context, root string, label string) (networkId server.Id, ok bool, returnErr error) {
	return abuseLabelNetwork(ctx, abuseName(root), label)
}

// The host a feed's line names, read as a reported url is (abuseReportUrlHost): its first field, with any scheme, a
// defanged hxxp included, any userinfo, the port, the path, the query and the fragment dropped unread, and [.] read
// as a dot. "" for a blank line, a comment, or a line that names no host name.
func AbuseScanHost(line string, settings *AbuseSettings) string {
	fields := strings.Fields(line)
	if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
		return ""
	}
	return abuseReportUrlHost(fields[0], settings)
}

// A feed's category, from abuse.yml: one of §21.4's threat rows, which §21.8's kill signal and its ladder serve, ok
// false for anything else.
func AbuseScanFeedCategory(text string) (AbuseCategory, bool) {
	category := AbuseCategory(strings.ToLower(strings.TrimSpace(text)))
	if !abuseThreatCategory(category) {
		return "", false
	}
	return category, true
}
