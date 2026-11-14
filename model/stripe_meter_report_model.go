package model

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// §20.9's meter rows (QUOTA-M6-03; D-QUOTA-8, FI-13(b)): stripe_meter_report(network_id, meter,
// usage_day, version, identifier, value, state, attempts, next_time), primary key (network_id, meter,
// usage_day, version), kept 13 months (§2.8 api-13).
//
//	value       the closed UTC day's quantity in the meter's whole unit, fixed by RollupServiceUsage
//	            (team_usage_model.go); exact bytes stay in network_service_usage
//	version     1 for a day's first value; a changed value is a new version, sent only once every
//	            earlier version's outcome is known (QUOTA-M6-04's sender), so the newest value is what
//	            Stripe keeps for the day: pre-aggregated daily meters keep a day's most recent event
//	identifier  hex SHA-256(network_id || meter || usage_day || version), with network_id its 16 bytes,
//	            meter its UTF-8 bytes, usage_day "YYYY-MM-DD" in ASCII and version 4 bytes big-endian:
//	            a re-send of a version carries the same identifier, which Stripe dedups for 24 h
//	state       queued -> sending -> sent | uncertain | failed | missed (stripe_meter_report_send.go)
//
// A day's row is written once by the rollup and never rewritten by a late debit: a debit counts on
// the day the pass first sees it (plan-QUOTA-overage-accounting), so a closed day changes only when a
// recompute (RecomputeStripeMeterReports, after a restore) finds another value, which is a new
// version. The billed quantity of a day is its newest version's value (FI-13(b)).

// The Stripe meters a Team reports (QD-430), each also its Stripe event_name by default.
const (
	TeamMeterTransferOverage = "team_transfer_overage"
	TeamMeterStorageOverage  = "team_storage_overage"
	TeamMeterSeats           = "team_seats"
)

// The Stripe meters, in the order a day's rows are written.
var TeamStripeMeters = []string{TeamMeterSeats, TeamMeterTransferOverage, TeamMeterStorageOverage}

// A row's send state.
type StripeMeterReportState string

const (
	// written, not yet sent
	StripeMeterReportStateQueued StripeMeterReportState = "queued"
	// claimed by a sender; a sender that dies here leaves the row to be sent again as uncertain
	StripeMeterReportStateSending StripeMeterReportState = "sending"
	// Stripe answered 200 or named a duplicate identifier
	StripeMeterReportStateSent StripeMeterReportState = "sent"
	// a timeout, 429 or 5xx: re-sent with the same identifier and value
	StripeMeterReportStateUncertain StripeMeterReportState = "uncertain"
	// any other 4xx: never re-sent, alerted
	StripeMeterReportStateFailed StripeMeterReportState = "failed"
	// still unsent when its invoice finalized: unbilled, counted, never added to a later invoice
	StripeMeterReportStateMissed StripeMeterReportState = "missed"
)

// Whether a row in state is done: its outcome is known and nothing sends it again.
func (self StripeMeterReportState) Final() bool {
	switch self {
	case StripeMeterReportStateSent, StripeMeterReportStateFailed, StripeMeterReportStateMissed:
		return true
	}
	return false
}

const stripeMeterReportDayLayout = "2006-01-02"

// One row of stripe_meter_report.
type StripeMeterReport struct {
	NetworkId  server.Id
	Meter      string
	UsageDay   time.Time
	Version    int
	Identifier string
	Value      int64
	State      StripeMeterReportState
	Attempts   int
	NextTime   time.Time
}

// §20.9's identifier: hex SHA-256(network_id || meter || usage_day || version), as the file header
// lays the bytes out.
func StripeMeterIdentifier(networkId server.Id, meter string, usageDay time.Time, version int) string {
	hash := sha256.New()
	hash.Write(networkId[:])
	hash.Write([]byte(meter))
	hash.Write([]byte(networkUsageDay(usageDay).Format(stripeMeterReportDayLayout)))
	var versionBytes [4]byte
	binary.BigEndian.PutUint32(versionBytes[:], uint32(version))
	hash.Write(versionBytes[:])
	return hex.EncodeToString(hash.Sum(nil))
}

var ErrStripeMeterReportUnavailable = errors.New("Stripe meter reports need MG6.")

const stripeMeterReportColumns = `network_id, meter, usage_day, version, identifier, value, state, attempts, next_time`

func scanStripeMeterReport(row interface{ Scan(dest ...any) error }) *StripeMeterReport {
	report := &StripeMeterReport{}
	var state string
	server.Raise(row.Scan(
		&report.NetworkId,
		&report.Meter,
		&report.UsageDay,
		&report.Version,
		&report.Identifier,
		&report.Value,
		&state,
		&report.Attempts,
		&report.NextTime,
	))
	report.State = StripeMeterReportState(state)
	report.UsageDay = networkUsageDay(report.UsageDay)
	report.NextTime = report.NextTime.UTC()
	return report
}

func stripeMeterReportsIn(ctx context.Context, conn server.PgCanQuery, sql string, args ...any) (reports []*StripeMeterReport) {
	result, err := conn.Query(ctx, sql, args...)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			reports = append(reports, scanStripeMeterReport(result))
		}
	})
	return
}

// Writes a closed day's values, one per meter, at now: a meter with no row gets version 1, queued;
// one whose newest version holds another value gets the next version, queued; one whose newest
// version holds this value gets nothing. Returns the rows written. A negative value is refused.
func WriteStripeMeterRowsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, usageDay time.Time, meterValues map[string]int64, now time.Time) (written []*StripeMeterReport, returnErr error) {
	if !server.SidecarMg6Linked() {
		return nil, ErrStripeMeterReportUnavailable
	}
	usageDay = networkUsageDay(usageDay)
	for _, meter := range TeamStripeMeters {
		value, ok := meterValues[meter]
		if !ok {
			continue
		}
		if value < 0 {
			return nil, fmt.Errorf("a negative value for %s", meter)
		}
		newest := newestStripeMeterReportInTx(ctx, tx, networkId, meter, usageDay)
		version := 1
		if newest != nil {
			if newest.Value == value {
				continue
			}
			version = newest.Version + 1
		}
		report := &StripeMeterReport{
			NetworkId:  networkId,
			Meter:      meter,
			UsageDay:   usageDay,
			Version:    version,
			Identifier: StripeMeterIdentifier(networkId, meter, usageDay, version),
			Value:      value,
			State:      StripeMeterReportStateQueued,
			NextTime:   now.UTC(),
		}
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO stripe_meter_report (`+stripeMeterReportColumns+`)
				VALUES ($1, $2, $3, $4, $5, $6, $7, 0, $8)
				ON CONFLICT (network_id, meter, usage_day, version) DO NOTHING
			`,
			report.NetworkId,
			report.Meter,
			report.UsageDay,
			report.Version,
			report.Identifier,
			report.Value,
			string(report.State),
			report.NextTime,
		))
		if tag.RowsAffected() == 1 {
			written = append(written, report)
		}
	}
	return
}

// The newest version of the day's row of meter, or nil.
func newestStripeMeterReportInTx(ctx context.Context, tx server.PgTx, networkId server.Id, meter string, usageDay time.Time) *StripeMeterReport {
	reports := stripeMeterReportsIn(
		ctx,
		tx,
		`
			SELECT `+stripeMeterReportColumns+`
			FROM stripe_meter_report
			WHERE network_id = $1 AND meter = $2 AND usage_day = $3
			ORDER BY version DESC
			LIMIT 1
		`,
		networkId,
		meter,
		networkUsageDay(usageDay),
	)
	if len(reports) == 0 {
		return nil
	}
	return reports[0]
}

// Every version of the network's rows from the day from on, by meter, day and version.
func GetStripeMeterReports(ctx context.Context, networkId server.Id, from time.Time) (reports []*StripeMeterReport) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		reports = stripeMeterReportsIn(
			ctx,
			conn,
			`
				SELECT `+stripeMeterReportColumns+`
				FROM stripe_meter_report
				WHERE network_id = $1 AND $2 <= usage_day
				ORDER BY meter, usage_day, version
			`,
			networkId,
			networkUsageDay(from),
		)
	})
	return
}

// Deletes at most pageCount rows of days older than the retention's 13 months, at now. Returns how
// many went.
func removeExpiredStripeMeterReportsInTx(ctx context.Context, tx server.PgTx, oldestDay time.Time, pageCount int) int {
	if !server.SidecarMg6Linked() {
		return 0
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM stripe_meter_report
			WHERE (network_id, meter, usage_day, version) IN (
				SELECT network_id, meter, usage_day, version
				FROM stripe_meter_report
				WHERE usage_day < $1
				LIMIT $2
			)
		`,
		networkUsageDay(oldestDay),
		pageCount,
	))
	return int(tag.RowsAffected())
}
