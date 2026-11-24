package usagearchive

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Which weeks may leave PostgreSQL, the same for the exporter and for the
// server's drop fence. Nothing an SN epoch can still read leaves PostgreSQL.
//
//   - Every row of the week has a close time (a terminal row without one stops
//     every epoch read, and must keep doing so), and the week's latest close
//     time is before the close time floor.
//   - The week itself ends no later than the floor.
//   - The floor is DefaultCloseTimeFloor until the export control sets
//     another date, or rolling, which sets none; it is never later than
//     MinCloseAge before now, whatever the control sets. Zero-byte contracts
//     are archived from 8 days (the reaper's zero-byte horizon); the floor
//     keeps them in PostgreSQL as long as any other row of their week.
//   - The server's drop never lets the floor pass a week before the window of
//     the earliest SN epoch not yet recorded either
//     (server/db_provider_usage_archive_export.go), so a rolling floor moves
//     without anyone checking the epochs first.
//
// DefaultCloseTimeFloor is a month before the earliest SN earning cutoff, so
// no epoch window, census or recovery reads a row closed before it.
var DefaultCloseTimeFloor = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// A row closed within this long before now never leaves PostgreSQL: the
// contract reaper's horizon (N) and its weekly rescan (7 days), which deletes
// the contracts its first read kept because they closed late, so a week's
// contracts are archived in the week and not in the DEFAULT partition, which
// is never dropped. Owner, 2026-11-23: 25 days, N + 7 for the C1 cutover's
// transition horizon of 18 days (server/CONTRACT-PARTITION-CUTOVER-NOW.md,
// decision 8); 42 days (35 + 7) from 2026-11-21, 70 before. A horizon raised
// past 18 days needs this raised with it, or a week can leave before the
// reaper's horizon reaches it (ReaperCheckContracts), and the contracts the
// reaper archives later land in DEFAULT: kept, but never dropped.
//
// It does not guard the SN epochs: an SN epoch, a 7-day window, is recorded
// about 9 days after its window opens, and the server's drop holds its own
// floor a week before the window of the earliest epoch not yet recorded
// (server/db_provider_usage_archive_export.go), which binds instead of this
// one only when an epoch is recorded more than 9 days late.
const MinCloseAge = 25 * 24 * time.Hour

// The floor that names no date, for the export control and the exporter: the
// floor in force rolls with the clock, MinCloseAge before now.
const RollingCloseTimeFloorValue = "rolling"

// The date rolling stands for: later than any clock reaches, so MinCloseAge
// before now is always the floor in force.
var RollingCloseTimeFloor = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// A floor as an operator writes it: rolling, a UTC date (YYYY-MM-DD) or
// RFC 3339.
func ParseCloseTimeFloor(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == RollingCloseTimeFloorValue {
		return RollingCloseTimeFloor, nil
	}
	if floor, err := time.Parse(time.DateOnly, value); err == nil {
		return floor.UTC(), nil
	}
	if floor, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return floor.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("close time floor %q is neither %s, YYYY-MM-DD nor RFC 3339", value, RollingCloseTimeFloorValue)
}

// A floor as an operator writes it: rolling, a date at midnight UTC, or
// RFC 3339.
func FormatCloseTimeFloor(floor time.Time) string {
	floor = floor.UTC()
	switch {
	case !floor.Before(RollingCloseTimeFloor):
		return RollingCloseTimeFloorValue
	case floor.Equal(floor.Truncate(24 * time.Hour)):
		return floor.Format(time.DateOnly)
	default:
		return floor.Format(time.RFC3339Nano)
	}
}

// The floor in force at now: floor, never later than MinCloseAge before now.
func EffectiveCloseTimeFloor(floor time.Time, now time.Time) time.Time {
	if limit := now.UTC().Add(-MinCloseAge); limit.Before(floor) {
		return limit
	}
	return floor.UTC()
}

// The contract horizon reaper deletes contracts in contract_id order and keeps
// the ones it must (payment holds and the like, about a tenth). Until it has
// passed a week, most of the week's contracts are still live and will be
// archived later; dropping the week first would send them all to the DEFAULT
// partition, which is never dropped. So a week leaves only once at most a
// quarter of its newest ReaperCheckContracts contracts, live or archived, are
// still live. The reaper's zero-byte pass archives a week's zero-byte
// contracts, about half of them, before its horizon reaches the week, so the
// share stands for the horizon's progress only while the horizon and its
// rescan reach the week within MinCloseAge: at a longer horizon, a week whose
// newest contracts were three quarters zero-byte could leave before the
// horizon reached it, and its other contracts would be archived to DEFAULT.
const (
	ReaperCheckContracts = 1000
	// live * ReaperLiveShareDenominator > total keeps the week
	ReaperLiveShareDenominator = 4
)

// The newest contracts of a week, live and archived: (live, total). leaf is a
// quoted or plain identifier the caller controls; $1 and $2 are the week's
// contract id bounds.
func ReaperProgressSql(leaf string) string {
	return fmt.Sprintf(`
		SELECT count(*) FILTER (WHERE newest.live), count(*)
		FROM (
			SELECT candidates.contract_id, candidates.live
			FROM (
				(SELECT contract_id, true AS live FROM transfer_contract
				 WHERE $1::uuid <= contract_id AND contract_id < $2::uuid
				 ORDER BY contract_id DESC LIMIT %[2]d)
				UNION ALL
				(SELECT contract_id, false AS live FROM %[1]s
				 ORDER BY contract_id DESC LIMIT %[2]d)
			) AS candidates
			ORDER BY candidates.contract_id DESC
			LIMIT %[2]d
		) AS newest
	`, leaf, ReaperCheckContracts)
}

// Why a week whose newest contracts are (live, total) waits for the reaper,
// or "".
func ReaperProgressBlocker(live int64, total int64) string {
	if total == 0 {
		return "the week holds no contracts"
	}
	if total < live*ReaperLiveShareDenominator {
		return fmt.Sprintf("the contract reaper has not passed the week: %d of its newest %d contracts are live", live, total)
	}
	return ""
}

// The close times of a week leaf, from its close-time index: whether a row has
// none, and the latest. leaf is a quoted or plain identifier the caller
// controls.
func CloseTimesSql(leaf string) string {
	return fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %[1]s WHERE close_time IS NULL), coalesce((SELECT max(close_time) FROM %[1]s)::text, '')`, leaf)
}

// Whether the exporter should export the week leaf now, and why: the week's
// rows all closed before floor, the reaper has passed it, and no verified copy
// already holds its rows as they are now (the archive only grows, so a copy
// with as many rows holds the same rows). manifests are the leaf's verified
// copies on disk.
func ExportCandidate(ctx context.Context, conn *pgx.Conn, leaf WeekLeaf, floor time.Time, manifests []*Manifest) (bool, string, error) {
	quoted := `public."` + leaf.Leaf + `"`
	var hasNullClose bool
	var maxClose string
	if err := conn.QueryRow(ctx, CloseTimesSql(quoted)).Scan(&hasNullClose, &maxClose); err != nil {
		return false, "", err
	}
	if hasNullClose {
		return false, "a row has no close time", nil
	}
	if maxClose == "" {
		return false, "the week has no rows", nil
	}
	var beforeFloor bool
	if err := conn.QueryRow(ctx, `SELECT $1::timestamp < $2::timestamp`, maxClose, floor.UTC().Format("2006-01-02 15:04:05.999999")).Scan(&beforeFloor); err != nil {
		return false, "", err
	}
	if !beforeFloor {
		return false, fmt.Sprintf("a row closed at %s, not before the floor", maxClose), nil
	}
	var live, total int64
	if err := conn.QueryRow(ctx, ReaperProgressSql(quoted), leaf.LowerContractId, leaf.UpperContractId).Scan(&live, &total); err != nil {
		return false, "", err
	}
	if blocker := ReaperProgressBlocker(live, total); blocker != "" {
		return false, blocker, nil
	}
	var rows int64
	if err := conn.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, quoted)).Scan(&rows); err != nil {
		return false, "", err
	}
	for _, manifest := range manifests {
		if manifest.Rows == rows {
			return false, fmt.Sprintf("%s already holds its %d rows", manifest.File, rows), nil
		}
	}
	return true, fmt.Sprintf("%d rows, latest close %s", rows, maxClose), nil
}
