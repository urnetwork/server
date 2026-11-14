package model

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// network_service_usage's reads and its daily rollup (QUOTA-M2-12; §20.14, D-QUOTA-8):
//
//	GET /network/usage   the network's QuotaLimits, its stored count, storage_over and spent, and its day rows
//	                     {usage_day, meter, quantity} for up to 13 months, newest first and paged by (day,
//	                     meter) (plan-QUOTA-small-readings #10); the meters plan-QUOTA-overage-accounting keeps
//	                     for its own sums, team_credit_window and team_credit_running, are hidden
//	RollupServiceUsage   after a UTC day closes, each network's stored bytes for that day under meter storage,
//	                     paged by network id so a pass is bounded and a pass killed mid-way resumes from its
//	                     cursor; a day row is written once and never changed after, so a rerun writes nothing
//	                     twice. Transfer meters are summed by the debit flush as it applies (QUOTA-M2-06), and
//	                     RollupTeamServiceUsage adds Team's rows after it (team_usage_model.go)
//	retention            network_service_usage 13 months, service_usage_batch 24 h and, once MG6 links,
//	                     stripe_meter_report 13 months, deleted in bounded pages (D-OPS-18 lists the
//	                     tables, so no backup outlives these ttls)

// The meter a day's stored bytes roll up under: §20.4's storage key.
const NetworkUsageStorageMeter = "storage"

// The meters GET /network/usage hides: plan-QUOTA-overage-accounting's helper sums.
var networkUsageHiddenMeters = []string{"team_credit_window", "team_credit_running"}

type NetworkUsageSettings struct {
	// How far back the day rows reach and are kept: 13 months.
	RetentionMonths int
	// How long an applied batch's id is kept: service_usage_batch's 24 h.
	BatchRetention time.Duration
	// The most day rows one page answers, and the default when a caller names none.
	MaxDayCount     int
	DefaultDayCount int
}

func DefaultNetworkUsageSettings() *NetworkUsageSettings {
	return &NetworkUsageSettings{
		RetentionMonths: 13,
		BatchRetention:  24 * time.Hour,
		MaxDayCount:     1000,
		DefaultDayCount: 200,
	}
}

var networkUsageSettings = DefaultNetworkUsageSettings()

// One day's quantity of one meter.
type NetworkUsageDay struct {
	// YYYY-MM-DD, the UTC day.
	UsageDay string `json:"usage_day"`
	Meter    string `json:"meter"`
	Quantity int64  `json:"quantity"`
}

// Where a page of day rows ends: the next page answers rows strictly after it in (day descending, meter).
type NetworkUsageCursor struct {
	UsageDay string `json:"usage_day"`
	Meter    string `json:"meter"`
}

// GET /network/usage's body.
type NetworkUsage struct {
	Limits          *sidecar.QuotaLimits `json:"limits"`
	StoredByteCount int64                `json:"stored_byte_count"`
	StorageOver     bool                 `json:"storage_over"`
	Spent           bool                 `json:"spent"`
	Days            []*NetworkUsageDay   `json:"days"`
	// nil on the last page
	Next *NetworkUsageCursor `json:"next,omitempty"`
}

const networkUsageDayLayout = "2006-01-02"

// The UTC day of t.
func networkUsageDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// The network's usage as GET /network/usage answers it, at now, one page of at most dayCount day rows after
// the cursor (nil: the newest). A cursor that does not parse is an error.
func GetNetworkUsage(ctx context.Context, networkId server.Id, after *NetworkUsageCursor, dayCount int, now time.Time) (*NetworkUsage, error) {
	if !server.SidecarMg2Linked() {
		return nil, ErrNetworkQuotaStateUnavailable
	}
	settings := networkUsageSettings
	if dayCount <= 0 {
		dayCount = settings.DefaultDayCount
	}
	dayCount = min(dayCount, settings.MaxDayCount)
	var afterDay *time.Time
	afterMeter := ""
	if after != nil {
		day, err := time.Parse(networkUsageDayLayout, after.UsageDay)
		if err != nil {
			return nil, errors.New("a cursor whose day does not parse")
		}
		afterDay = &day
		afterMeter = after.Meter
	}
	usage := &NetworkUsage{
		Limits: NetworkQuotaLimits(ctx, networkId),
		Days:   []*NetworkUsageDay{},
	}
	oldestDay := networkUsageDay(now).AddDate(0, -settings.RetentionMonths, 0)
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT stored_byte_count, storage_over, spent_time IS NOT NULL FROM network_quota_state WHERE network_id = $1`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&usage.StoredByteCount, &usage.StorageOver, &usage.Spent))
			}
		})
		result, err = conn.Query(
			ctx,
			`
				SELECT usage_day, meter, quantity
				FROM network_service_usage
				WHERE
					network_id = $1 AND
					$2 <= usage_day AND
					NOT (meter = ANY($3)) AND
					($4::date IS NULL OR usage_day < $4 OR (usage_day = $4 AND $5 < meter))
				ORDER BY usage_day DESC, meter
				LIMIT $6
			`,
			networkId,
			oldestDay,
			networkUsageHiddenMeters,
			afterDay,
			afterMeter,
			dayCount+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var day time.Time
				row := &NetworkUsageDay{}
				server.Raise(result.Scan(&day, &row.Meter, &row.Quantity))
				row.UsageDay = day.Format(networkUsageDayLayout)
				usage.Days = append(usage.Days, row)
			}
		})
	})
	if dayCount < len(usage.Days) {
		usage.Days = usage.Days[:dayCount]
		last := usage.Days[len(usage.Days)-1]
		usage.Next = &NetworkUsageCursor{UsageDay: last.UsageDay, Meter: last.Meter}
	}
	return usage, nil
}

// One page of RollupServiceUsage.
type RollupServiceUsageResult struct {
	// the networks the page looked at, and the day rows it wrote (a rerun's are already there)
	LookedCount  int
	WrittenCount int
	// the page's last network: the next page starts after it; nil when the pass is done
	LastNetworkId *server.Id
}

// Writes each network's stored bytes for the closed UTC day of day, at most pageCount networks after after
// (nil: from the start), in network order. A row already there is kept as it was: a closed day's row is
// written once.
func RollupServiceUsage(ctx context.Context, day time.Time, after *server.Id, pageCount int) (result RollupServiceUsageResult, returnErr error) {
	if !server.SidecarMg2Linked() {
		return result, ErrNetworkQuotaStateUnavailable
	}
	if pageCount <= 0 {
		return result, errors.New("a rollup page of no networks")
	}
	usageDay := networkUsageDay(day)
	server.Tx(ctx, func(tx server.PgTx) {
		rows, err := tx.Query(
			ctx,
			`
				WITH page AS (
					SELECT network_id, stored_byte_count
					FROM network_quota_state
					WHERE $1::uuid IS NULL OR $1 < network_id
					ORDER BY network_id
					LIMIT $2
				),
				written AS (
					INSERT INTO network_service_usage (network_id, usage_day, meter, quantity)
					SELECT network_id, $3, $4, stored_byte_count FROM page
					ON CONFLICT (network_id, usage_day, meter) DO NOTHING
					RETURNING 1
				)
				SELECT
					(SELECT count(*) FROM page),
					(SELECT count(*) FROM written),
					(SELECT network_id FROM page ORDER BY network_id DESC LIMIT 1)
			`,
			after,
			pageCount,
			usageDay,
			NetworkUsageStorageMeter,
		)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				var lastNetworkId *server.Id
				server.Raise(rows.Scan(&result.LookedCount, &result.WrittenCount, &lastNetworkId))
				if result.LookedCount == pageCount {
					result.LastNetworkId = lastNetworkId
				}
			}
		})
	})
	return
}

// One page of RemoveExpiredServiceUsage.
type RemoveExpiredServiceUsageResult struct {
	UsageRemovedCount int
	BatchRemovedCount int
	// stripe_meter_report's rows past their 13 months, once MG6 links (QUOTA-M6-03)
	ReportRemovedCount int
	// whether any table had more past its ttl than one page took
	More bool
}

// Deletes at most pageCount network_service_usage rows older than 13 months and at most pageCount
// service_usage_batch ids older than 24 h, at now.
func RemoveExpiredServiceUsage(ctx context.Context, now time.Time, pageCount int) (result RemoveExpiredServiceUsageResult, returnErr error) {
	if !server.SidecarMg2Linked() {
		return result, ErrNetworkQuotaStateUnavailable
	}
	if pageCount <= 0 {
		return result, errors.New("a retention page of no rows")
	}
	settings := networkUsageSettings
	oldestDay := networkUsageDay(now).AddDate(0, -settings.RetentionMonths, 0)
	oldestBatchTime := now.UTC().Add(-settings.BatchRetention)
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_service_usage
				WHERE (network_id, usage_day, meter) IN (
					SELECT network_id, usage_day, meter
					FROM network_service_usage
					WHERE usage_day < $1
					LIMIT $2
				)
			`,
			oldestDay,
			pageCount,
		))
		result.UsageRemovedCount = int(tag.RowsAffected())
		tag = server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM service_usage_batch
				WHERE batch_id IN (
					SELECT batch_id
					FROM service_usage_batch
					WHERE applied_time < $1
					LIMIT $2
				)
			`,
			oldestBatchTime,
			pageCount,
		))
		result.BatchRemovedCount = int(tag.RowsAffected())
		result.ReportRemovedCount = removeExpiredStripeMeterReportsInTx(ctx, tx, oldestDay, pageCount)
	})
	result.More = result.UsageRemovedCount == pageCount || result.BatchRemovedCount == pageCount || result.ReportRemovedCount == pageCount
	return
}
