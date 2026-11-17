package model

// §21.11's transparency counts (ABUSE-M5-17; D-ABUSE-7, Q-ABUSE-5, I-13): the semiannual report's numbers over a
// window, as totals and never a row, and naming no network. Each count reads what is retained when it runs, so a
// report counts a hold or a case only while its row is kept: counsel runs it before the window's oldest rows age out.
// A small count stays as it is, for counsel to band; nothing here rounds.
//
//	requests             legal_request filed in the window, by instrument and country
//	accounts affected    the distinct networks those requests name
//	productions          those requests produced
//	sealing orders       those requests sealed
//	holds                abuse_legal_hold's part 0 placed in the window, by kind
//	DMCA notices         copyright cases opened in the window
//	abuse actions        abuse_action rows created in the window, by kind: not lifts, which are no action, and not a
//	                     reviewer's screen_allow, which withholds nothing
//	appeals              abuse_appeal filed in the window, by state
//	NCMEC reports        CSAM cases opened in the window on which a person suspended the network, report_csam's
//	                     path; a report's filing with NCMEC is a person's and leaves no other row
//	root incidents       cases opened in the window that name no network and join by a root host (§21.6's
//	                     OpenAbuseRootIncident)
//
// AbuseReader.TransparencyCounts is the abuse role's way to them, its read logged first (lookup kind counts, no case).
// Nothing here reads anything unless the binary links MG5 (server.SidecarMg5Linked).

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// What abuse_access_log says of a transparency count's read.
const AbuseLookupKindCounts AbuseLookupKind = "counts"

// One instrument and country's count of requests.
type AbuseTransparencyRequestCount struct {
	Instrument string `json:"instrument"`
	Country    string `json:"country"`
	Count      int64  `json:"count"`
}

// A count by one key: a hold's kind, an action's kind, an appeal's state.
type AbuseTransparencyKeyCount struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// §21.11's numbers over [FromTime, ToTime).
type AbuseTransparencyCountSet struct {
	FromTimeMs       int64                            `json:"from_time_ms"`
	ToTimeMs         int64                            `json:"to_time_ms"`
	Requests         []*AbuseTransparencyRequestCount `json:"requests"`
	RequestCount     int64                            `json:"request_count"`
	AccountsAffected int64                            `json:"accounts_affected"`
	Productions      int64                            `json:"productions"`
	SealingOrders    int64                            `json:"sealing_orders"`
	Holds            []*AbuseTransparencyKeyCount     `json:"holds"`
	DmcaNotices      int64                            `json:"dmca_notices"`
	AbuseActions     []*AbuseTransparencyKeyCount     `json:"abuse_actions"`
	Appeals          []*AbuseTransparencyKeyCount     `json:"appeals"`
	NcmecReports     int64                            `json:"ncmec_reports"`
	RootIncidents    int64                            `json:"root_incidents"`
}

// A window that ends before it starts.
var ErrAbuseTransparencyWindowInvalid = fmt.Errorf("abuse transparency window invalid")

// The counts over [fromTime, toTime). A machine read: the abuse role reaches it through AbuseReader.TransparencyCounts.
func AbuseTransparencyCounts(ctx context.Context, fromTime time.Time, toTime time.Time) (*AbuseTransparencyCountSet, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	if !fromTime.Before(toTime) {
		return nil, ErrAbuseTransparencyWindowInvalid
	}
	fromTime, toTime = fromTime.UTC(), toTime.UTC()
	counts := &AbuseTransparencyCountSet{
		FromTimeMs:   fromTime.UnixMilli(),
		ToTimeMs:     toTime.UnixMilli(),
		Requests:     []*AbuseTransparencyRequestCount{},
		Holds:        []*AbuseTransparencyKeyCount{},
		AbuseActions: []*AbuseTransparencyKeyCount{},
		Appeals:      []*AbuseTransparencyKeyCount{},
	}
	keyCounts := func(conn server.PgConn, sql string, args ...any) []*AbuseTransparencyKeyCount {
		keyCounts := []*AbuseTransparencyKeyCount{}
		result, err := conn.Query(ctx, sql, args...)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				keyCount := &AbuseTransparencyKeyCount{}
				server.Raise(result.Scan(&keyCount.Key, &keyCount.Count))
				keyCounts = append(keyCounts, keyCount)
			}
		})
		return keyCounts
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT instrument, country, COUNT(*)
				FROM legal_request
				WHERE $1 <= create_time AND create_time < $2
				GROUP BY instrument, country
				ORDER BY instrument, country
			`,
			fromTime,
			toTime,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				requestCount := &AbuseTransparencyRequestCount{}
				server.Raise(result.Scan(&requestCount.Instrument, &requestCount.Country, &requestCount.Count))
				counts.Requests = append(counts.Requests, requestCount)
				counts.RequestCount += requestCount.Count
			}
		})
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT
					COUNT(*) FILTER (WHERE production_state = $3),
					COUNT(*) FILTER (WHERE sealed)
				FROM legal_request
				WHERE $1 <= create_time AND create_time < $2
			`,
			fromTime,
			toTime,
			LegalProductionStateProduced,
		).Scan(&counts.Productions, &counts.SealingOrders))
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT COUNT(DISTINCT account.network_id)
				FROM legal_request, unnest(legal_request.network_ids) AS account(network_id)
				WHERE $1 <= legal_request.create_time AND legal_request.create_time < $2
			`,
			fromTime,
			toTime,
		).Scan(&counts.AccountsAffected))
		counts.Holds = keyCounts(
			conn,
			`
				SELECT kind, COUNT(*)
				FROM abuse_legal_hold
				WHERE part = 0 AND $1 <= place_time AND place_time < $2
				GROUP BY kind
				ORDER BY kind
			`,
			fromTime,
			toTime,
		)
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT
					COUNT(*) FILTER (WHERE category = $3),
					COUNT(*) FILTER (WHERE network_id IS NULL AND join_kind = $4)
				FROM abuse_case
				WHERE $1 <= create_time AND create_time < $2
			`,
			fromTime,
			toTime,
			string(AbuseCategoryCopyright),
			string(AbuseJoinKindHost),
		).Scan(&counts.DmcaNotices, &counts.RootIncidents))
		counts.AbuseActions = keyCounts(
			conn,
			`
				SELECT kind, COUNT(*)
				FROM abuse_action
				WHERE $1 <= create_time AND create_time < $2 AND kind NOT IN ($3, $4)
				GROUP BY kind
				ORDER BY kind
			`,
			fromTime,
			toTime,
			string(AbuseActionKindLift),
			string(AbuseActionKindScreenAllow),
		)
		counts.Appeals = keyCounts(
			conn,
			`
				SELECT state, COUNT(*)
				FROM abuse_appeal
				WHERE $1 <= create_time AND create_time < $2
				GROUP BY state
				ORDER BY state
			`,
			fromTime,
			toTime,
		)
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT COUNT(*)
				FROM abuse_case
				WHERE
					$1 <= abuse_case.create_time AND abuse_case.create_time < $2 AND
					abuse_case.category = $3 AND
					EXISTS (
						SELECT 1
						FROM abuse_action
						WHERE
							abuse_action.case_id = abuse_case.case_id AND
							abuse_action.kind = $4 AND
							NOT abuse_action.by_machine
					)
			`,
			fromTime,
			toTime,
			string(AbuseCategoryCsam),
			string(AbuseActionKindNetworkSuspend),
		).Scan(&counts.NcmecReports))
	})
	return counts, nil
}

// The counts over [fromTime, toTime), the read logged first with no case (§21.10).
func (self *AbuseReader) TransparencyCounts(ctx context.Context, fromTime time.Time, toTime time.Time) (*AbuseTransparencyCountSet, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	if !fromTime.Before(toTime) {
		return nil, ErrAbuseTransparencyWindowInvalid
	}
	if err := self.logRead(ctx, AbuseLookupKindCounts, nil); err != nil {
		return nil, err
	}
	return AbuseTransparencyCounts(ctx, fromTime, toTime)
}
