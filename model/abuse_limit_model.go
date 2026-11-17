package model

// §21.7's limits (ABUSE-M5-19; D-ABUSE-1, D-ABUSE-4, D-QUOTA-7, FS-47, I-4): §21 tightens a network's limits only
// through network_sidecar_limit rows citing its case, and the abuse worker's one machine step here opens a case and
// takes no action.
//
//	limit_tighten  an abuse_action whose target is <limit_key>=<limit_value>, riding §21's one path (ABUSE-M5-05):
//	               mailed, reviewed and lifted through its case. Its effect writes the row, reason abuse:<case_id>;
//	               a lift's effect clears it, so the row outlives no case. Only AbuseTightenableLimitKeys' keys,
//	               none for forwarding or sockets (owner decision 66), and every row passes
//	               ValidateNetworkSidecarLimit first, which refuses the fixed keys (evs_closure_bytes_floor,
//	               evs_closure_uploads_per_s and the two erasure rates a minute) and evs_closure_bytes_per_day below
//	               the 4 MiB floor (owner decisions 146 and 524); QUOTA's clamp stays the second line
//	closure growth nightly: a network whose closure bytes taken today are above zero, whose storage_over is set and
//	               whose stored count is past its storage limit gets one case, category other, joined by its
//	               network, with no machine action (QD-328), and none while such a case is open
//
// A tightening is a row, never a state, a gate or a code path, so NetworkSidecarLimits stays the one read, and it is
// not a suspension. Erasure stays possible at the storage limit: no row takes an eraser's allowance below the floor.
// M6 adds the directory keys (ABUSE-M6-01).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// §21.7's tightening, as abuse_action stores it.
const AbuseActionKindLimitTighten AbuseActionKind = "limit_tighten"

// The abuse worker's closure-growth finding, as abuse_case stores who it came from.
const AbuseReporterClassClosureGrowth AbuseReporterClass = "closure_growth"

// The keys §21 may tighten (§21.7): each eraser's closure allowance, to the floor at most, and the day's statements,
// which bound the admissions, each a new eraser, that a day adds. Forwarding and sockets have no key (owner decision
// 66). From M6 the team directory's request limits (ABUSE-M6-01, §20.4, §16.11), which §16.11's daemons and readers
// apply as GET /sidecar/limits gives them, since the server sees no request's content: they take a row only in a
// binary that links MG6 (abuseDirectoryLimitKeys).
var AbuseTightenableLimitKeys = []string{
	"evs_closure_bytes_per_day",
	"config_statements_per_day",
	"directory_requests_pending",
	"directory_requests_per_day",
	"directory_requests_per_run",
	"directory_target_pending",
}

// The directory's keys of AbuseTightenableLimitKeys, which wait for MG6.
var abuseDirectoryLimitKeys = []string{
	"directory_requests_pending",
	"directory_requests_per_day",
	"directory_requests_per_run",
	"directory_target_pending",
}

// A tightening no row may take: a key off AbuseTightenableLimitKeys, a value ValidateNetworkSidecarLimit refuses, or
// a target that does not read as <limit_key>=<limit_value>. The message names the key, never a network.
var ErrAbuseLimitRefused = errors.New("abuse limit tightening refused")

// The reason a tightening's row cites: its case.
func AbuseLimitReason(caseId server.Id) string {
	return "abuse:" + caseId.String()
}

// A tightening's target: <limit_key>=<limit_value>.
func AbuseLimitTightenTarget(limitKey string, limitValue int64) string {
	return limitKey + "=" + strconv.FormatInt(limitValue, 10)
}

// The key and value a tightening's target names.
func ParseAbuseLimitTightenTarget(target string) (limitKey string, limitValue int64, err error) {
	limitKey, valueText, found := strings.Cut(target, "=")
	if !found || limitKey == "" {
		return "", 0, fmt.Errorf("%w: a target that is not <limit_key>=<limit_value>", ErrAbuseLimitRefused)
	}
	limitValue, err = strconv.ParseInt(valueText, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("%w: a value that is not an integer", ErrAbuseLimitRefused)
	}
	return limitKey, limitValue, nil
}

// Refuses a tightening no row may take, before anything records it: a key §21 does not tighten, then
// ValidateNetworkSidecarLimit's own refusals, the fixed keys and the floor among them.
func ValidateAbuseLimitTighten(limitKey string, limitValue int64, caseId server.Id) error {
	if !slices.Contains(AbuseTightenableLimitKeys, limitKey) {
		return fmt.Errorf("%w: §21 does not tighten %q", ErrAbuseLimitRefused, limitKey)
	}
	if slices.Contains(abuseDirectoryLimitKeys, limitKey) && !server.SidecarMg6Linked() {
		return fmt.Errorf("%w: %q waits for the team directory (MG6)", ErrAbuseLimitRefused, limitKey)
	}
	if err := ValidateNetworkSidecarLimit(limitKey, limitValue, AbuseLimitReason(caseId)); err != nil {
		return fmt.Errorf("%w: %w", ErrAbuseLimitRefused, err)
	}
	return nil
}

// A tightening's target, validated as ValidateAbuseLimitTighten does.
func validateAbuseLimitTightenTarget(target string, caseId server.Id) error {
	limitKey, limitValue, err := ParseAbuseLimitTightenTarget(target)
	if err != nil {
		return err
	}
	return ValidateAbuseLimitTighten(limitKey, limitValue, caseId)
}

// Writes a tightening's row: the effect of a limit_tighten action. Idempotent, as an effect is.
func SetAbuseLimitRow(ctx context.Context, networkId server.Id, caseId server.Id, target string) error {
	limitKey, limitValue, err := ParseAbuseLimitTightenTarget(target)
	if err != nil {
		return err
	}
	if err := ValidateAbuseLimitTighten(limitKey, limitValue, caseId); err != nil {
		return err
	}
	return SetNetworkSidecarLimit(ctx, networkId, limitKey, limitValue, AbuseLimitReason(caseId))
}

// Clears a tightening's row: the effect of its lift. Only a row still citing this case goes, so a lift never clears
// a row another case's tightening wrote over it. cleared is false when no such row stood.
func ClearAbuseLimitRow(ctx context.Context, networkId server.Id, caseId server.Id, target string) (cleared bool, returnErr error) {
	if !server.SidecarMg1Linked() {
		return false, ErrNetworkSidecarLimitUnavailable
	}
	limitKey, _, err := ParseAbuseLimitTightenTarget(target)
	if err != nil {
		return false, err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_sidecar_limit
				WHERE network_id = $1 AND limit_key = $2 AND reason = $3
			`,
			networkId,
			limitKey,
			AbuseLimitReason(caseId),
		))
		cleared = tag.RowsAffected() == 1
	})
	if returnErr == nil {
		ReloadNetworkSidecarLimits(ctx)
	}
	return
}

// What one closure-growth pass did.
type AbuseClosureGrowthResult struct {
	// networks over their storage limit the pass read
	CheckedCount int
	// cases it opened
	OpenedCount int
	// networks it could not judge this time, a Redis read failing, left to the next pass
	FailedCount int
}

// The networks whose storage_over is set, after cursor, with their stored counts.
func listStorageOverNetworks(ctx context.Context, after server.Id, limit int) (networkIds []server.Id, storedByteCounts []int64) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_id, stored_byte_count
				FROM network_quota_state
				WHERE storage_over AND network_id > $1
				ORDER BY network_id
				LIMIT $2
			`,
			after,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var storedByteCount int64
				server.Raise(result.Scan(&networkId, &storedByteCount))
				networkIds = append(networkIds, networkId)
				storedByteCounts = append(storedByteCounts, storedByteCount)
			}
		})
	})
	return
}

// Whether the network has an open closure-growth case.
func networkHasOpenClosureGrowthCaseInTx(ctx context.Context, tx server.PgTx, networkId server.Id) (open bool) {
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT EXISTS (
				SELECT 1
				FROM abuse_case
				WHERE
					abuse_case.network_id = $1 AND
					abuse_case.state = 'open' AND
					abuse_case.reporter_class = $2
			)
		`,
		networkId,
		string(AbuseReporterClassClosureGrowth),
	).Scan(&open))
	return
}

// The closure bytes a network took today: model.NetworkClosureBytesToday, which a test replaces.
var abuseClosureBytesToday = NetworkClosureBytesToday

// Replaces where the pass reads closure bytes until the returned restore.
func Testing_SetAbuseClosureBytesToday(read func(ctx context.Context, networkId server.Id, now time.Time) (int64, error)) (restore func()) {
	previous := abuseClosureBytesToday
	abuseClosureBytesToday = read
	return func() {
		abuseClosureBytesToday = previous
	}
}

// One closure-growth pass at now (see the file header): every network with storage_over set whose closure bytes
// today are above zero and whose stored count is past its storage limit gets one case, no action, unless such a case
// is open; until ClosureGrowthPassMaxTime has passed. A binary that does not link MG2 and MG5 does nothing.
func RunAbuseClosureGrowth(ctx context.Context, now time.Time, settings *AbuseSettings) *AbuseClosureGrowthResult {
	result := &AbuseClosureGrowthResult{}
	if !server.SidecarMg5Linked() || !server.SidecarMg2Linked() {
		return result
	}
	calendar, err := NewAbuseCalendar(AbuseReviewHoursOf(server.GetAbuseConfig()), &settings.AbusePolicySettings)
	if err != nil {
		calendar, _ = NewAbuseCalendar(DefaultAbuseReviewHours(), &settings.AbusePolicySettings)
	}
	deadline := time.Now().Add(settings.ClosureGrowthPassMaxTime)
	cursor := server.Id{}
	for ctx.Err() == nil && time.Now().Before(deadline) {
		networkIds, storedByteCounts := listStorageOverNetworks(ctx, cursor, max(settings.ClosureGrowthBatchSize, 1))
		if len(networkIds) == 0 {
			return result
		}
		cursor = networkIds[len(networkIds)-1]
		for i, networkId := range networkIds {
			result.CheckedCount += 1
			if storedByteCounts[i] <= NetworkStorageLimitByteCount(ctx, networkId) {
				continue
			}
			closureBytes, err := abuseClosureBytesToday(ctx, networkId, now)
			if err != nil {
				result.FailedCount += 1
				continue
			}
			if closureBytes <= 0 {
				continue
			}
			opened := false
			var openErr error
			server.Tx(ctx, func(tx server.PgTx) {
				// one at a time per network, so two passes open one case
				server.RaisePgResult(tx.Exec(
					ctx,
					`SELECT pg_advisory_xact_lock(hashtextextended('abuse_closure_growth:' || $1::uuid::text, 0))`,
					networkId,
				))
				if networkHasOpenClosureGrowthCaseInTx(ctx, tx, networkId) {
					return
				}
				_, opened, openErr = OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
					NetworkId:     &networkId,
					Category:      AbuseCategoryOther,
					Target:        AbuseTarget{Kind: AbuseTargetKindNetwork, Name: networkId.String()},
					ReporterClass: AbuseReporterClassClosureGrowth,
					JoinKind:      AbuseJoinKindNetwork,
					JoinKey:       networkId.String(),
					ReviewDueTime: AbuseReviewDue(AbuseSignalReport, "", "", now, calendar),
				}, now, settings)
			})
			if openErr != nil {
				result.FailedCount += 1
				continue
			}
			if opened {
				result.OpenedCount += 1
			}
		}
	}
	return result
}

// When the next closure-growth pass runs after now: the next ClosureGrowthPassTime of a UTC day, late in the day,
// so today's closure bytes have accumulated and the night's stored count stands.
func NextAbuseClosureGrowthTime(now time.Time, settings *AbuseSettings) time.Time {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(settings.ClosureGrowthPassTime)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}
