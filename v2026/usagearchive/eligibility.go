package usagearchive

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Which weeks may leave PostgreSQL, the same for the exporter and for the
// server's drop fence. The owner's decisions on the boundary are open, so the
// rule is the conservative one: nothing an SN epoch or a payout audit can
// read leaves PostgreSQL.
//
//   - Every row of the week has a close time (a terminal row without one stops
//     every epoch read, and must keep doing so), and the week's latest close
//     time is before the close time floor.
//   - The week itself ends no later than the floor.
//   - The floor is DefaultCloseTimeFloor until the export control sets
//     another date, and never later than MinCloseAge before now, whatever the
//     control sets: 35 days past the lowest horizon the contract reaper can
//     run (35 days).
//
// DefaultCloseTimeFloor is a month before the earliest SN earning cutoff, so
// no epoch window, census or recovery reads a row closed before it.
var DefaultCloseTimeFloor = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// A row closed within this long before now never leaves PostgreSQL.
const MinCloseAge = 70 * 24 * time.Hour

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
// still live.
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
