package model

// §21.8's cases (abuse_case, §2.8 api-14). Every report, scan hit or filed
// notice that joins a network becomes a case, and every action and lift hangs
// off one (abuse_action_model.go). A case keeps api-14's list and nothing else:
// category, target, join kind and key, reporter class. Third-party data dies at
// the join, so no column holds a URL path, report text or reporter address
// (§21.8, I-13).
//
// A report on the network, target and category of a case opened inside
// AbuseSettings.CaseCoalesceWindow joins it and adds no row, so an hourly scan
// of one stale listing is one case, not a climbing ladder
// (abuse-core-policy-readings #7). Its review clock moves earlier when the
// report's clock is shorter, since every doubt takes the short clock. A barred
// case coalesces only with a barred report, so a sealing order never hides a
// case the network was already told of, and never joins one it was.
//
// Nothing here reads or writes the tables unless the binary links MG5
// (server.SidecarMg5Linked).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

// How a case was tied to a network (§21.8's pipeline), as abuse_case stores it.
type AbuseJoinKind string

const (
	// a host or label through §12's label map
	AbuseJoinKindHost AbuseJoinKind = "host"
	// a burner address, which names its <net>
	AbuseJoinKindAddress AbuseJoinKind = "address"
	// §14.8's header fields
	AbuseJoinKindHeaders AbuseJoinKind = "headers"
	// an edge connection_id within 24 h (tls-3)
	AbuseJoinKindConnection AbuseJoinKind = "connection"
	// a case that names its network directly: a staff filing, or the worker's
	// closure-growth case (§21.7)
	AbuseJoinKindNetwork AbuseJoinKind = "network"
)

func (self AbuseJoinKind) known() bool {
	switch self {
	case AbuseJoinKindHost, AbuseJoinKindAddress, AbuseJoinKindHeaders, AbuseJoinKindConnection, AbuseJoinKindNetwork:
		return true
	}
	return false
}

// A case's state, as abuse_case stores it.
type AbuseCaseState string

const (
	AbuseCaseStateOpen AbuseCaseState = "open"
	// every action lifted or upheld by review, or, with no action, reviewed
	// (plan-ABUSE-small-readings #9); an appeal reopens it
	AbuseCaseStateClosed AbuseCaseState = "closed"
)

// The widths of abuse_case's text columns.
const (
	abuseTargetMaxLength  = 256
	abuseJoinKeyMaxLength = 256
)

// A binary that does not link MG5 has no abuse tables, so it records nothing.
var ErrAbuseUnlinked = errors.New("abuse tables not linked")

// Arguments no row may hold. The message names no target or key (I-13).
var ErrAbuseCaseInvalid = errors.New("abuse case invalid")

// No case has the id.
var ErrAbuseCaseNotFound = errors.New("abuse case not found")

// Counts reports by whether they opened a case or joined an open one, so a
// coalesced report is still counted where no row records it.
var abuseCaseReportCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "abuse",
		Name:      "case_reports_total",
		Help:      "Abuse reports by whether they opened a case or joined an open one",
	},
	[]string{"result"},
)

// Registers the report counter with the default registry.
func init() {
	prometheus.MustRegister(abuseCaseReportCounter)
}

// A case to open, from a report, a scan hit or a filing that joined.
type AbuseCaseArgs struct {
	// nil until a join names a network
	NetworkId     *server.Id
	Category      AbuseCategory
	Target        AbuseTarget
	ReporterClass AbuseReporterClass
	JoinKind      AbuseJoinKind
	// the host, label, header key or connection id the join matched
	JoinKey string
	// a sealing order or a CSAM case bars every notice (§21.8)
	Barred bool
	// when review falls due (AbuseReviewDue); zero sets no clock
	ReviewDueTime time.Time
}

// One case as abuse_case holds it.
type AbuseCase struct {
	CaseId        server.Id
	NetworkId     *server.Id
	Category      AbuseCategory
	TargetKind    AbuseTargetKind
	Target        string
	ReporterClass AbuseReporterClass
	JoinKind      AbuseJoinKind
	JoinKey       string
	State         AbuseCaseState
	Barred        bool
	ReviewDueTime *time.Time
	CreateTime    time.Time
	CloseTime     *time.Time
}

// Refuses what no row may hold.
func (self *AbuseCaseArgs) validate() error {
	if !abuseCategoryKnown(self.Category) {
		return fmt.Errorf("%w: an unknown category", ErrAbuseCaseInvalid)
	}
	switch self.Target.Kind {
	case AbuseTargetKindHost, AbuseTargetKindSendingDomain, AbuseTargetKindLabel, AbuseTargetKindNetwork:
	default:
		return fmt.Errorf("%w: an unknown target kind", ErrAbuseCaseInvalid)
	}
	target := abuseName(self.Target.Name)
	if target == "" || abuseTargetMaxLength < len(target) {
		return fmt.Errorf("%w: a target of %d bytes", ErrAbuseCaseInvalid, len(target))
	}
	if self.ReporterClass == "" || 32 < len(self.ReporterClass) {
		return fmt.Errorf("%w: no reporter class", ErrAbuseCaseInvalid)
	}
	if !self.JoinKind.known() {
		return fmt.Errorf("%w: an unknown join kind", ErrAbuseCaseInvalid)
	}
	if self.JoinKey == "" || abuseJoinKeyMaxLength < len(self.JoinKey) {
		return fmt.Errorf("%w: a join key of %d bytes", ErrAbuseCaseInvalid, len(self.JoinKey))
	}
	return nil
}

// Whether the category is one of §21.4's rows or this plan's two more.
func abuseCategoryKnown(category AbuseCategory) bool {
	switch category {
	case AbuseCategorySpam, AbuseCategoryPhishing, AbuseCategoryMalware, AbuseCategoryCommandAndControl,
		AbuseCategoryCopyright, AbuseCategoryExposedPanel, AbuseCategoryCompromise,
		AbuseCategoryCsam, AbuseCategoryThreatToLife, AbuseCategoryBrand, AbuseCategoryOther:
		return true
	}
	return false
}

// Opens a case in the caller's transaction, or joins the open case of the same
// network, target, category and bar opened inside the coalescing window,
// adding no row. opened is false for a join. Two reports racing on one target
// serialize on a transaction-scoped lock, so they make one case.
func OpenAbuseCaseInTx(
	ctx context.Context,
	tx server.PgTx,
	args *AbuseCaseArgs,
	now time.Time,
	settings *AbuseSettings,
) (caseId server.Id, opened bool, err error) {
	if !server.SidecarMg5Linked() {
		return server.Id{}, false, ErrAbuseUnlinked
	}
	if err := args.validate(); err != nil {
		return server.Id{}, false, err
	}
	target := abuseName(args.Target.Name)
	var reviewDueTime *time.Time
	if !args.ReviewDueTime.IsZero() {
		due := args.ReviewDueTime.UTC()
		reviewDueTime = &due
	}

	if args.NetworkId != nil && 0 < settings.CaseCoalesceWindow {
		// one lock per network, category and target, held to the end of the
		// caller's transaction
		server.RaisePgResult(tx.Exec(
			ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended('abuse_case:' || $1::uuid::text || ':' || $2 || ':' || $3, 0))`,
			*args.NetworkId,
			string(args.Category),
			target,
		))
		found := false
		result, err := tx.Query(
			ctx,
			`
				SELECT abuse_case.case_id
				FROM abuse_case
				WHERE
					abuse_case.network_id = $1 AND
					abuse_case.target = $2 AND
					abuse_case.category = $3 AND
					abuse_case.state = 'open' AND
					abuse_case.barred = $4 AND
					$5 <= abuse_case.create_time
				ORDER BY abuse_case.create_time DESC, abuse_case.case_id DESC
				LIMIT 1
				FOR UPDATE
			`,
			*args.NetworkId,
			target,
			string(args.Category),
			args.Barred,
			now.Add(-settings.CaseCoalesceWindow).UTC(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&caseId))
				found = true
			}
		})
		if found {
			if reviewDueTime != nil {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						UPDATE abuse_case
						SET review_due_time = LEAST(COALESCE(abuse_case.review_due_time, $2), $2)
						WHERE abuse_case.case_id = $1
					`,
					caseId,
					*reviewDueTime,
				))
			}
			abuseCaseReportCounter.WithLabelValues("coalesced").Inc()
			return caseId, false, nil
		}
	}

	caseId = server.NewId()
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO abuse_case (
				case_id,
				network_id,
				category,
				target_kind,
				target,
				reporter_class,
				join_kind,
				join_key,
				state,
				barred,
				review_due_time,
				create_time
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'open', $9, $10, $11)
		`,
		caseId,
		args.NetworkId,
		string(args.Category),
		string(args.Target.Kind),
		target,
		string(args.ReporterClass),
		string(args.JoinKind),
		args.JoinKey,
		args.Barred,
		reviewDueTime,
		now.UTC(),
	))
	abuseCaseReportCounter.WithLabelValues("opened").Inc()
	return caseId, true, nil
}

// Every column of a case, in scanAbuseCase's order.
const abuseCaseColumns = `
	abuse_case.case_id,
	abuse_case.network_id,
	abuse_case.category,
	abuse_case.target_kind,
	abuse_case.target,
	abuse_case.reporter_class,
	abuse_case.join_kind,
	abuse_case.join_key,
	abuse_case.state,
	abuse_case.barred,
	abuse_case.review_due_time,
	abuse_case.create_time,
	abuse_case.close_time
`

// One row of abuseCaseColumns.
func scanAbuseCase(result server.PgResult) *AbuseCase {
	abuseCase := &AbuseCase{}
	var category, targetKind, reporterClass, joinKind, state string
	server.Raise(result.Scan(
		&abuseCase.CaseId,
		&abuseCase.NetworkId,
		&category,
		&targetKind,
		&abuseCase.Target,
		&reporterClass,
		&joinKind,
		&abuseCase.JoinKey,
		&state,
		&abuseCase.Barred,
		&abuseCase.ReviewDueTime,
		&abuseCase.CreateTime,
		&abuseCase.CloseTime,
	))
	abuseCase.Category = AbuseCategory(category)
	abuseCase.TargetKind = AbuseTargetKind(targetKind)
	abuseCase.ReporterClass = AbuseReporterClass(reporterClass)
	abuseCase.JoinKind = AbuseJoinKind(joinKind)
	abuseCase.State = AbuseCaseState(state)
	return abuseCase
}

// The case, read in the caller's transaction and locked to its end when
// forUpdate, or nil.
func getAbuseCaseInTx(ctx context.Context, tx server.PgTx, caseId server.Id, forUpdate bool) *AbuseCase {
	sql := `SELECT ` + abuseCaseColumns + ` FROM abuse_case WHERE abuse_case.case_id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var abuseCase *AbuseCase
	result, err := tx.Query(ctx, sql, caseId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			abuseCase = scanAbuseCase(result)
		}
	})
	return abuseCase
}

// The case, or nil, including nil in a binary that does not link MG5.
func GetAbuseCase(ctx context.Context, caseId server.Id) (abuseCase *AbuseCase) {
	if !server.SidecarMg5Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+abuseCaseColumns+` FROM abuse_case WHERE abuse_case.case_id = $1`,
			caseId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				abuseCase = scanAbuseCase(result)
			}
		})
	})
	return
}

// Whether the network has an open case: §12.6's restore step flags its restored
// RRsets until the abuse role clears them (DNS-M5-15). A binary that does not
// link MG5 has no cases.
func NetworkHasOpenAbuseCase(ctx context.Context, networkId server.Id) (open bool) {
	if !server.SidecarMg5Linked() {
		return false
	}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1
					FROM abuse_case
					WHERE abuse_case.network_id = $1 AND abuse_case.state = 'open'
				)
			`,
			networkId,
		).Scan(&open))
	})
	return
}
