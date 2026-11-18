package model

// A filed notice's case, and what the clock pass reads (ABUSE-M5-10; §21.8,
// D-ABUSE-3, FS-49): the case a paged notice opens, the open notices the pass
// pages again, and the count of open cases past their review clock.
//
// A notice's case keeps its class as its join key under join kind notice, so
// the pass can page it again by case id alone, and the abuse role can list a
// class's notices by join key. It names a network only when the name the
// notice is about is a host under a root that the label map resolves: then the
// rota's kill of that host takes §21's one action path under the case. A
// notice about a root itself names none.
//
// A notice opens its own case and never joins an open one: its hour is its
// own, and a page whose key is a case some report opened would say nothing of
// a notice. Nothing here reads a notice's text, sender or URL: the case keeps
// a host name at most (I-13).

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// One notice to open a case for.
type AbuseNoticeCaseArgs struct {
	Class AbuseNoticeClass
	// the name the notice is about: a root, or a host under one
	Target string
	// §21.4's row, when the filer knows it; empty is other
	Category AbuseCategory
	// a sealing order bars every notice to the network (§21.8)
	Barred bool
}

// A notice's case as opened.
type AbuseNoticeCase struct {
	CaseId server.Id
	Class  AbuseNoticeClass
	// the network the named host joined, or nil
	NetworkId *server.Id
	// the paged hour's end, which ignores the calendar
	ReviewDueTime time.Time
}

// Opens the case of a notice at now, due in the paged hour. A class that does
// not page, or a target that is no host name, is refused with
// ErrAbuseCaseInvalid, and a binary that does not link MG5 answers
// ErrAbuseUnlinked.
func OpenAbuseNoticeCase(
	ctx context.Context,
	args *AbuseNoticeCaseArgs,
	now time.Time,
	settings *AbuseSettings,
) (*AbuseNoticeCase, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	if !args.Class.Pages() {
		return nil, fmt.Errorf("%w: a notice of a class that does not page", ErrAbuseCaseInvalid)
	}
	target := abuseName(args.Target)
	if !abuseHostNameValid(target, settings) {
		return nil, fmt.Errorf("%w: a notice about no host name", ErrAbuseCaseInvalid)
	}
	category := args.Category
	if category == "" {
		category = AbuseCategoryOther
	}
	if !abuseCategoryKnown(category) {
		return nil, fmt.Errorf("%w: an unknown category", ErrAbuseCaseInvalid)
	}

	caseArgs := &AbuseCaseArgs{
		Category:      category,
		Target:        AbuseTarget{Kind: AbuseTargetKindHost, Name: target},
		ReporterClass: args.Class.ReporterClass(),
		JoinKind:      AbuseJoinKindNotice,
		JoinKey:       string(args.Class),
		// a CSAM case stays barred whoever files it
		Barred: args.Barred || category == AbuseCategoryCsam,
		// tier and standing do not enter a paged notice's clock
		ReviewDueTime: AbuseReviewDue(args.Class.Signal(), "", "", now, abuseReportCalendar(settings)),
	}
	join, err := abuseJoinName(ctx, target, settings)
	if err != nil {
		return nil, err
	}
	if join != nil {
		networkId := join.NetworkId
		caseArgs.NetworkId = &networkId
		caseArgs.Target = join.Target
	}

	// its own case, whatever is open on the target
	ownSettings := *settings
	ownSettings.CaseCoalesceWindow = 0
	noticeCase := &AbuseNoticeCase{
		Class:         args.Class,
		NetworkId:     caseArgs.NetworkId,
		ReviewDueTime: caseArgs.ReviewDueTime,
	}
	server.Tx(ctx, func(tx server.PgTx) {
		noticeCase.CaseId, _, err = OpenAbuseCaseInTx(ctx, tx, caseArgs, now, &ownSettings)
	})
	if err != nil {
		return nil, err
	}
	return noticeCase, nil
}

// The class of a notice's case, or none for any other case.
func AbuseNoticeClassOfCase(abuseCase *AbuseCase) AbuseNoticeClass {
	if abuseCase == nil || abuseCase.JoinKind != AbuseJoinKindNotice {
		return AbuseNoticeClassNone
	}
	if class := AbuseNoticeClass(abuseCase.JoinKey); class.Pages() {
		return class
	}
	return AbuseNoticeClassNone
}

// Up to limit open notice cases filed at or after since, oldest first. Nothing
// in a binary that does not link MG5.
func ListOpenAbuseNoticeCases(ctx context.Context, since time.Time, limit int) (cases []*AbuseCase) {
	cases = []*AbuseCase{}
	if !server.SidecarMg5Linked() || limit <= 0 {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+abuseCaseColumns+`
				FROM abuse_case
				WHERE
					abuse_case.join_kind = $1 AND
					abuse_case.state = 'open' AND
					$2 <= abuse_case.create_time
				ORDER BY abuse_case.create_time, abuse_case.case_id
				LIMIT $3
			`,
			string(AbuseJoinKindNotice),
			since.UTC(),
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				cases = append(cases, scanAbuseCase(result))
			}
		})
	})
	return
}

// How many open cases are past their review clock at now (§21.8): the reviews
// that are late. A closed case and a case with no clock count for nothing.
// Zero in a binary that does not link MG5.
func CountOverdueAbuseReviews(ctx context.Context, now time.Time) (count int64) {
	if !server.SidecarMg5Linked() {
		return 0
	}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT COUNT(*)
				FROM abuse_case
				WHERE abuse_case.state = 'open' AND abuse_case.review_due_time < $1
			`,
			now.UTC(),
		).Scan(&count))
	})
	return
}
