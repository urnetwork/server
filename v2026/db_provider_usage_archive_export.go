// The usage archive's export off the primary: a week of st_provider_usage_archive may be dropped before its 52 weeks once a
// verified copy of exactly its rows is stored off the primary and recorded in
// the export ledger, and nothing that reads the archive can still need it.
//
// # Who reads archive rows
//
// Only the SN epoch usage statement (model/st_provider_usage.go
// stEpochProviderUsageSql, and the census statement built on it), through the
// v2 epoch close, the v2 ledger rows of an unrecorded epoch and the v1 epoch
// payout. Each reads the rows that closed inside an epoch window, and the
// statement's missing_time probe reads any row without a close time, in any
// week: one stops every epoch read. The monitor reads only the archive's
// shape.
//
// # The rule (usagearchive/eligibility.go)
//
// A week leaves only when nothing an SN epoch can still read is in it: every
// row has a close time, the latest is before the close time floor, and the
// week ends by the floor. The floor in force is the earliest of:
//   - the export control's floor: usagearchive.DefaultCloseTimeFloor
//     (2026-09-01, a month before the earliest SN earning cutoff) until the
//     control sets a date, and no date at all when it sets rolling;
//   - usagearchive.MinCloseAge (42 days) before the database's clock;
//   - a week before the window of the earliest SN epoch not yet recorded, in
//     the drop's own snapshot (providerUsageArchiveSnFloorSql).
//
// An epoch's window is read until the epoch is recorded: its close, its
// census and any rebuild asked of it. So the floor never passes a window that
// can still be read, however late its epoch is recorded, and a rolling floor
// moves without anyone checking the epochs first. The owner approved the
// 42-day floor (2026-11-21); payout audits of older windows restore the
// weeks they need from their copies.
//
// The week also waits until the contract horizon reaper has passed it, so the
// contracts the reaper still archives land in the week rather than in the
// DEFAULT partition, which is never dropped.
//
// # The ledger
//
// st_provider_usage_archive_export: one row per verified copy, written by the
// exporter (usagearchivectl, on the backup host) only after the stored file was
// read back and matched the figures PostgreSQL read in the export's snapshot.
// The drop that relies on a copy stamps its dropped_time in the drop's own
// transaction.
//
// # The drop
//
// The partition registry's guarded drop, as for the 52-week retention, on the
// retention task's cadence (every 15 minutes):
//   - RangeExpired: a week is due at its 52 weeks as before, or, while the
//     export control's mode is on or dry_run, once it ends by the floor.
//   - DropPrecondition, before the drop: the floor rules, the reaper's
//     progress, a verified ledger row holding the leaf's row count, and the
//     leaf's row digest computed afresh by PostgreSQL equal to the ledger's.
//     In a dry run the week stays, reported.
//   - DropFence, in the drop transaction before any detach: the retention
//     opt-in, then a week before its 52 weeks only with the control on, within
//     the pass's brake, and by the floor on the database's clock.
//   - DropPrecondition again in the drop transaction, after the detach, where
//     no insert can reach the leaf: the row count and the close times once
//     more, and the ledger row's dropped_time.
//
// The archive only grows (its guard refuses UPDATE, DELETE and TRUNCATE), so a
// leaf whose digest matched the copy and whose row count still equals it
// holds exactly the copy's rows.
//
// # The control
//
// The Redis hash `{usage_archive_export}control`, read once per retention
// run, before any transaction, and carried in the run's context
// (WithProviderUsageArchiveExportControl), so no Redis read happens while a
// drop holds its locks. Its fields, all optional:
//   - `mode`: on, dry_run or off. Absent, or any other value, is off: a
//     deploy drops nothing early until an operator sets it.
//   - `closeTimeFloor`: the floor, as YYYY-MM-DD or RFC 3339, or `rolling`
//     for none: the floor in force then rolls 42 days behind the database's
//     clock, held back by SN epochs not yet recorded. A value that does not
//     parse turns the mode off.
//   - `maxWeeksPerPass`: early drops (or dry-run verifications) per pass, at
//     most ProviderUsageArchiveExportSettings.MaxWeeksPerPass; zero, or a
//     value that does not parse, turns the mode off.
//
// A run that cannot read the hash drops nothing early. The kill switch is
// `mode off` (or deleting the hash): the next pass drops nothing early. The
// 52-week drops never read the control.
//
// Restoring a dropped week: usagearchivectl restore loads the copy into a new
// table and checks its digest; usagearchive.AttachSql gives the statements
// that attach it again.
package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026/usagearchive"
)

// The export ledger. A new table: nothing is rewritten, and older binaries
// never read it. No column has a default; the exporter supplies every value.
const providerUsageArchiveExportLedgerSql = `
	CREATE TABLE st_provider_usage_archive_export (
		export_id uuid NOT NULL,
		week_start timestamp NOT NULL,
		leaf_name varchar(64) NOT NULL,
		lower_contract_id uuid NOT NULL,
		upper_contract_id uuid NOT NULL,
		row_count bigint NOT NULL,
		null_close_count bigint NOT NULL,
		min_contract_id uuid NULL,
		max_contract_id uuid NULL,
		min_close_time timestamp NULL,
		max_close_time timestamp NULL,
		row_digest numeric NOT NULL,
		copy_sha256 char(64) NOT NULL,
		copy_bytes bigint NOT NULL,
		file_name text NOT NULL,
		file_sha256 char(64) NOT NULL,
		file_bytes bigint NOT NULL,
		location text NOT NULL,
		export_host text NOT NULL,
		snapshot_time timestamp NOT NULL,
		verified_time timestamp NOT NULL,
		create_time timestamp NOT NULL,
		dropped_time timestamp NULL,

		PRIMARY KEY (export_id),
		UNIQUE (file_sha256)
	);
	CREATE INDEX st_provider_usage_archive_export_leaf ON st_provider_usage_archive_export (leaf_name, row_count);
`

var providerUsageArchiveExportLedgerMigration = newSqlMigration(providerUsageArchiveExportLedgerSql)

// The version that creates the export ledger, wherever merges place it.
func ProviderUsageArchiveExportLedgerMigrationVersion() int {
	return migrationVersion(providerUsageArchiveExportLedgerMigration)
}

const providerUsageArchiveExportControlKey = "{usage_archive_export}control"

const (
	ProviderUsageArchiveExportControlFieldMode            = "mode"
	ProviderUsageArchiveExportControlFieldCloseTimeFloor  = "closeTimeFloor"
	ProviderUsageArchiveExportControlFieldMaxWeeksPerPass = "maxWeeksPerPass"
)

// The closeTimeFloor that sets no date: the floor in force rolls with the
// database's clock.
const ProviderUsageArchiveExportRollingFloor = usagearchive.RollingCloseTimeFloorValue

// What the export control lets the retention pass do with exported weeks.
type ProviderUsageArchiveExportMode int

const (
	// no week drops before its 52 weeks
	ProviderUsageArchiveExportModeOff ProviderUsageArchiveExportMode = 0
	// eligible weeks are verified and reported, and stay
	ProviderUsageArchiveExportModeDryRun ProviderUsageArchiveExportMode = 1
	// eligible, verified weeks drop
	ProviderUsageArchiveExportModeOn ProviderUsageArchiveExportMode = 2
)

func (self ProviderUsageArchiveExportMode) String() string {
	switch self {
	case ProviderUsageArchiveExportModeDryRun:
		return "dry_run"
	case ProviderUsageArchiveExportModeOn:
		return "on"
	default:
		return "off"
	}
}

// Tunables of the early drops.
type ProviderUsageArchiveExportSettings struct {
	// early drops per pass while the control sets none, and the most it can set
	DefaultWeeksPerPass int
	MaxWeeksPerPass     int
	// bounds the control read
	ControlTimeout time.Duration
	// bounds the digest a precondition computes over a leaf
	DigestTimeout time.Duration
}

func DefaultProviderUsageArchiveExportSettings() *ProviderUsageArchiveExportSettings {
	return &ProviderUsageArchiveExportSettings{
		DefaultWeeksPerPass: 1,
		MaxWeeksPerPass:     4,
		ControlTimeout:      2 * time.Second,
		DigestTimeout:       20 * time.Minute,
	}
}

// The effective control of one retention pass.
type ProviderUsageArchiveExportControl struct {
	// the hash was present
	Present         bool
	Mode            ProviderUsageArchiveExportMode
	CloseTimeFloor  time.Time
	MaxWeeksPerPass int
	// why a field turned the mode off, if one did
	Invalid string
	// weeks this pass verified or dropped early
	used atomic.Int64
}

// The control the fields narrow the settings to. Pure: fields is the hash as
// read, empty when the key is absent. A field that does not parse turns the
// mode off.
func NarrowProviderUsageArchiveExportControl(fields map[string]string, settings *ProviderUsageArchiveExportSettings) *ProviderUsageArchiveExportControl {
	control := &ProviderUsageArchiveExportControl{
		Present:         0 < len(fields),
		Mode:            ProviderUsageArchiveExportModeOff,
		CloseTimeFloor:  usagearchive.DefaultCloseTimeFloor,
		MaxWeeksPerPass: min(settings.DefaultWeeksPerPass, settings.MaxWeeksPerPass),
	}
	switch strings.TrimSpace(fields[ProviderUsageArchiveExportControlFieldMode]) {
	case "on":
		control.Mode = ProviderUsageArchiveExportModeOn
	case "dry_run":
		control.Mode = ProviderUsageArchiveExportModeDryRun
	}
	if value, ok := fields[ProviderUsageArchiveExportControlFieldCloseTimeFloor]; ok {
		floor, err := parseProviderUsageArchiveExportFloor(value)
		if err != nil {
			control.Invalid = err.Error()
		} else {
			control.CloseTimeFloor = floor
		}
	}
	if value, ok := fields[ProviderUsageArchiveExportControlFieldMaxWeeksPerPass]; ok {
		weeks, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || weeks <= 0 {
			control.Invalid = fmt.Sprintf("maxWeeksPerPass %q is not a positive integer", value)
		} else {
			control.MaxWeeksPerPass = min(weeks, settings.MaxWeeksPerPass)
		}
	}
	if control.Invalid != "" || control.MaxWeeksPerPass <= 0 {
		control.Mode = ProviderUsageArchiveExportModeOff
	}
	return control
}

func parseProviderUsageArchiveExportFloor(value string) (time.Time, error) {
	floor, err := usagearchive.ParseCloseTimeFloor(value)
	if err != nil {
		return time.Time{}, fmt.Errorf("closeTimeFloor: %w", err)
	}
	return floor, nil
}

// The floor in force at now, before the SN epochs hold it back (the drop
// reads those in its own snapshot).
func (self *ProviderUsageArchiveExportControl) EffectiveFloor(now time.Time) time.Time {
	return usagearchive.EffectiveCloseTimeFloor(self.CloseTimeFloor, now)
}

// The control's floor as an operator writes it: rolling, or a date.
func (self *ProviderUsageArchiveExportControl) CloseTimeFloorText() string {
	return usagearchive.FormatCloseTimeFloor(self.CloseTimeFloor)
}

// Takes one of the pass's weeks, or reports the brake reached.
func (self *ProviderUsageArchiveExportControl) takeWeek() bool {
	if self.used.Add(1) <= int64(self.MaxWeeksPerPass) {
		return true
	}
	self.used.Add(-1)
	return false
}

func (self *ProviderUsageArchiveExportControl) weeksLeft() bool {
	return self.used.Load() < int64(self.MaxWeeksPerPass)
}

// The hash's fields as stored, empty when it is absent. One bounded Redis
// read, never retried.
func GetProviderUsageArchiveExportControlFields(ctx context.Context, settings *ProviderUsageArchiveExportSettings) (fields map[string]string, returnErr error) {
	readCtx, cancel := context.WithTimeout(ctx, settings.ControlTimeout)
	defer cancel()
	returnErr = RedisWithDeadline(readCtx, func(r RedisClient) error {
		var err error
		fields, err = r.HGetAll(readCtx, providerUsageArchiveExportControlKey).Result()
		return err
	}, OptRedisNoPreflight())
	return
}

// Sets control fields, keeping the others. For operator tools and tests. ctx
// must carry a deadline.
func SetProviderUsageArchiveExportControl(ctx context.Context, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	values := make([]any, 0, 2*len(fields))
	for field, value := range fields {
		values = append(values, field, value)
	}
	return RedisWithDeadline(ctx, func(r RedisClient) error {
		return r.HSet(ctx, providerUsageArchiveExportControlKey, values...).Err()
	}, OptRedisNoPreflight())
}

// Deletes the control, so nothing drops early. ctx must carry a deadline.
func RemoveProviderUsageArchiveExportControl(ctx context.Context) error {
	return RedisWithDeadline(ctx, func(r RedisClient) error {
		return r.Del(ctx, providerUsageArchiveExportControlKey).Err()
	}, OptRedisNoPreflight())
}

type providerUsageArchiveExportContextKey struct{}

// ctx carrying control for the drops of one retention pass.
func WithProviderUsageArchiveExportControlValue(ctx context.Context, control *ProviderUsageArchiveExportControl) context.Context {
	return context.WithValue(ctx, providerUsageArchiveExportContextKey{}, control)
}

// ctx carrying the export control as the hash sets it now. One bounded Redis
// read; when it fails, ctx is returned as is and nothing drops early. Call it
// once per retention run, before any transaction.
func WithProviderUsageArchiveExportControl(ctx context.Context) context.Context {
	settings := DefaultProviderUsageArchiveExportSettings()
	fields, err := GetProviderUsageArchiveExportControlFields(ctx, settings)
	if err != nil {
		glog.V(1).Infof("[partitions]usage archive export control unread, no week drops early: %s\n", err)
		return ctx
	}
	control := NarrowProviderUsageArchiveExportControl(fields, settings)
	if control.Invalid != "" {
		glog.Errorf("[partitions]usage archive export control is off: %s\n", control.Invalid)
	}
	return WithProviderUsageArchiveExportControlValue(ctx, control)
}

// The pass's export control, or nil.
func providerUsageArchiveExportControlIn(ctx context.Context) *ProviderUsageArchiveExportControl {
	control, _ := ctx.Value(providerUsageArchiveExportContextKey{}).(*ProviderUsageArchiveExportControl)
	return control
}

// The archive's partition family name, for the retention task's reports.
const ProviderUsageArchiveFamilyName = "st_provider_usage_archive"

// Whether ctx's export control lets weeks drop or verify early.
func ProviderUsageArchiveExportActive(ctx context.Context) bool {
	return providerUsageArchiveExportActive(ctx) != nil
}

// The pass's control when it may drop or verify early, else nil.
func providerUsageArchiveExportActive(ctx context.Context) *ProviderUsageArchiveExportControl {
	control := providerUsageArchiveExportControlIn(ctx)
	if control == nil || control.Mode == ProviderUsageArchiveExportModeOff {
		return nil
	}
	return control
}

// Whether the week r has expired at now (the family's RangeExpired): its 52
// weeks are past, or the export control is on or in a dry run and the week
// ends by the close time floor. The precondition and the fence decide whether
// it drops.
func providerUsageArchiveRangeExpired(ctx context.Context, now time.Time, r PartitionRange) bool {
	if !now.Before(r.Upper.Add(providerUsageArchiveRetention)) {
		return true
	}
	control := providerUsageArchiveExportActive(ctx)
	if control == nil || r.Legacy || r.Lower.IsZero() {
		return false
	}
	return !control.EffectiveFloor(now).Before(r.Upper)
}

// How far before the window of the earliest SN epoch not yet recorded the
// floor stays, and the block time that places a window: the margin absorbs
// the placement's error, a chain halt of up to a week included. While epochs
// are recorded on time (about 9 days after their window opens), the floor in
// force is MinCloseAge before now, a month earlier, and this never binds.
const (
	providerUsageArchiveSnFloorMargin  = 7 * 24 * time.Hour
	providerUsageArchiveSnBlockSeconds = 12
)

// The SN epochs' floor, as one timestamp: a week before the window of the
// earliest epoch an epoch read can still read; '-infinity', so nothing drops
// early, when such a window cannot be placed; NULL when the database holds no
// epoch, which least() ignores. An epoch is recorded when
//   - before its deployment's v2 first epoch (st_ledger_v2_activation), or
//     with no activation: its st_epoch status is finalized;
//   - from the v2 first epoch on: a root of the activation's pool is
//     finalized with its amounts recorded (recorded_time), no root of the
//     epoch is pending, and no rebuild of its rows is requested.
//
// Every deployment's latest epoch counts as not recorded: an epoch the chain
// sync has not mirrored yet starts after it. A v2 root or rows of an epoch
// st_epoch does not hold have no window to place. A window opens at its
// start_block, placed 12 s a block before the deployment's chain sync
// high-water block at its update time; a deployment without a chain sync row
// has no placed windows. Every deployment counts: one retired with epochs
// never recorded holds the floor until those rows are dealt with.
var providerUsageArchiveSnFloorSql = fmt.Sprintf(`
	SELECT min(unrecorded.window_start) - interval '%[1]d seconds'
	FROM (
		SELECT CASE
			WHEN anchor.high_water_block IS NULL THEN '-infinity'::timestamp
			ELSE anchor.update_time - (anchor.high_water_block - epoch.start_block) * interval '%[2]d seconds'
		END AS window_start
		FROM st_epoch AS epoch
		LEFT JOIN st_chain_sync AS anchor ON anchor.deployment_key = epoch.deployment_key AND anchor.singleton_id = 1
		LEFT JOIN st_ledger_v2_activation AS activation ON activation.deployment_key = epoch.deployment_key
		WHERE epoch.epoch = (SELECT max(latest.epoch) FROM st_epoch AS latest WHERE latest.deployment_key = epoch.deployment_key)
			OR NOT CASE
				WHEN activation.first_epoch IS NULL OR epoch.epoch < activation.first_epoch THEN epoch.status = 'finalized'
				ELSE EXISTS (
						SELECT 1 FROM st_ledger_v2_root AS root
						WHERE root.deployment_key = epoch.deployment_key AND root.no_id = activation.no_id AND root.epoch = epoch.epoch
							AND root.status = 'finalized' AND root.recorded_time IS NOT NULL
					)
					AND NOT EXISTS (
						SELECT 1 FROM st_ledger_v2_root AS root
						WHERE root.deployment_key = epoch.deployment_key AND root.no_id = activation.no_id AND root.epoch = epoch.epoch
							AND root.status = 'pending'
					)
					AND NOT EXISTS (
						SELECT 1 FROM st_ledger_v2_epoch_rows AS epoch_rows
						WHERE epoch_rows.deployment_key = epoch.deployment_key AND epoch_rows.no_id = activation.no_id AND epoch_rows.epoch = epoch.epoch
							AND epoch_rows.rebuild_requested_by IS NOT NULL
					)
			END
		UNION ALL
		SELECT '-infinity'::timestamp
		WHERE EXISTS (
			SELECT 1
			FROM (
				SELECT deployment_key, epoch FROM st_ledger_v2_root
				UNION ALL
				SELECT deployment_key, epoch FROM st_ledger_v2_epoch_rows
			) AS v2
			WHERE NOT EXISTS (SELECT 1 FROM st_epoch AS known WHERE known.deployment_key = v2.deployment_key AND known.epoch = v2.epoch)
		)
	) AS unrecorded
`, int64(providerUsageArchiveSnFloorMargin/time.Second), providerUsageArchiveSnBlockSeconds)

// The week's figures and its ledger row, in one snapshot. $1 the week's upper
// bound, $2 the control's floor, $3 the leaf's name, $4 and $5 the week's
// contract id bounds. The floor in force is the earliest of the control's,
// MinCloseAge before now, and the SN epochs' floor.
func providerUsageArchiveWeekFiguresSql(leaf string) string {
	quoted := "public." + quoteIdentifier(leaf)
	return fmt.Sprintf(`
		WITH sn AS (
			SELECT (%[4]s) AS floor
		), clock AS (
			SELECT
				least($2::timestamp, (now() AT TIME ZONE 'UTC') - interval '%[2]d seconds', sn.floor) AS floor,
				sn.floor AS sn_floor
			FROM sn
		), figures AS (
			SELECT
				$1::timestamp + interval '%[3]d seconds' <= (now() AT TIME ZONE 'UTC') AS retention_due,
				$1::timestamp <= clock.floor AS week_by_floor,
				clock.floor::text AS floor,
				EXISTS (SELECT 1 FROM %[1]s WHERE close_time IS NULL) AS null_close,
				(SELECT max(close_time) FROM %[1]s) AS max_close,
				(SELECT count(*) FROM %[1]s) AS row_count
			FROM clock
		)
		SELECT
			figures.retention_due, figures.week_by_floor, figures.floor, figures.null_close,
			coalesce(figures.max_close::text, ''),
			coalesce(figures.max_close < clock.floor, true),
			figures.row_count,
			coalesce(export.export_id::text, ''),
			coalesce(export.row_digest::text, ''),
			coalesce(export.file_name, ''),
			coalesce(clock.floor = clock.sn_floor, false)
		FROM figures
		CROSS JOIN clock
		LEFT JOIN LATERAL (
			SELECT export_id, row_digest, file_name
			FROM st_provider_usage_archive_export AS ledger
			WHERE ledger.leaf_name = $3 AND ledger.row_count = figures.row_count
				AND ledger.lower_contract_id = $4::uuid AND ledger.upper_contract_id = $5::uuid
				AND ledger.dropped_time IS NULL
			ORDER BY ledger.verified_time DESC, ledger.export_id
			LIMIT 1
		) AS export ON true
	`, quoted, int64(usagearchive.MinCloseAge/time.Second), int64(providerUsageArchiveRetention/time.Second), providerUsageArchiveSnFloorSql)
}

// The week's figures, as the precondition reads them.
type providerUsageArchiveWeekFigures struct {
	retentionDue bool
	weekByFloor  bool
	floor        string
	nullClose    bool
	maxClose     string
	closedBefore bool
	rows         int64
	exportId     string
	exportDigest string
	exportFile   string
	// the SN epochs' floor is the floor in force
	snHolds bool
}

// Why the floor in force is where it is, for a blocker that names it.
func (self *providerUsageArchiveWeekFigures) floorText() string {
	if self.snHolds {
		return self.floor + " (a week before the window of an SN epoch not yet recorded)"
	}
	return self.floor
}

func readProviderUsageArchiveWeekFigures(ctx context.Context, conn PgQuerier, r PartitionRange, floor time.Time) (*providerUsageArchiveWeekFigures, error) {
	leaf := usagearchive.LeafOfWeek(r.Lower)
	figures := &providerUsageArchiveWeekFigures{}
	err := conn.QueryRow(
		ctx,
		providerUsageArchiveWeekFiguresSql(leaf),
		r.Upper.UTC().Format(partitionTimestampLayout),
		floor.UTC().Format(partitionTimestampLayout),
		leaf,
		ContractIdFloor(r.Lower).String(),
		ContractIdFloor(r.Upper).String(),
	).Scan(
		&figures.retentionDue, &figures.weekByFloor, &figures.floor, &figures.nullClose,
		&figures.maxClose, &figures.closedBefore, &figures.rows,
		&figures.exportId, &figures.exportDigest, &figures.exportFile,
		&figures.snHolds,
	)
	if err != nil {
		return nil, err
	}
	return figures, nil
}

// Why the week r must stay, or "" (the family's DropPrecondition). A week at
// its 52 weeks, or any week while the export control is off, is left to the
// fence, as before the export. Otherwise the week stays unless it meets the
// rule and a verified export holds exactly its rows. Before the drop, on a
// maintenance connection, PostgreSQL computes the leaf's row digest afresh
// and it must equal the export's; in the drop's transaction, after the
// detach, the row count and the close times are checked once more and the
// ledger row records the drop.
func providerUsageArchiveWeekBlocker(ctx context.Context, conn PgQuerier, r PartitionRange) string {
	if r.Default || r.Legacy || r.Lower.IsZero() || r.Upper.IsZero() {
		return "the legacy and DEFAULT partitions are never dropped"
	}
	control := providerUsageArchiveExportActive(ctx)
	if control == nil {
		return ""
	}
	_, inDrop := conn.(pgx.Tx)
	// a pass whose brake is used reads nothing more, unless the week may be
	// at its 52 weeks (the database's clock decides that below)
	if !inDrop && !control.weeksLeft() && NowUtc().Before(r.Upper.Add(providerUsageArchiveRetention)) {
		return fmt.Sprintf("the export brake's %d weeks this pass are used", control.MaxWeeksPerPass)
	}
	figures, err := readProviderUsageArchiveWeekFigures(ctx, conn, r, control.CloseTimeFloor)
	if err != nil {
		return fmt.Sprintf("the week's figures: %s", err)
	}
	if figures.retentionDue {
		return ""
	}
	switch {
	case !figures.weekByFloor:
		return fmt.Sprintf("the week ends after the close time floor %s", figures.floorText())
	case figures.nullClose:
		return "a row has no close time"
	case !figures.closedBefore:
		return fmt.Sprintf("a row closed at %s, not before the close time floor %s", figures.maxClose, figures.floorText())
	case figures.exportId == "":
		return fmt.Sprintf("no verified export holds the week's %d rows", figures.rows)
	}
	// the archive only grows, so a leaf whose digest matched an export once
	// and whose row count still equals it holds the same rows
	verifiedKey := figures.exportId + ":" + strconv.FormatInt(figures.rows, 10) + ":" + figures.exportDigest
	if !inDrop {
		leaf := usagearchive.LeafOfWeek(r.Lower)
		var live, total int64
		if err := conn.QueryRow(ctx, usagearchive.ReaperProgressSql("public."+quoteIdentifier(leaf)), ContractIdFloor(r.Lower).String(), ContractIdFloor(r.Upper).String()).Scan(&live, &total); err != nil {
			return fmt.Sprintf("the reaper's progress: %s", err)
		}
		if blocker := usagearchive.ReaperProgressBlocker(live, total); blocker != "" {
			return blocker
		}
		if !control.weeksLeft() {
			return fmt.Sprintf("the export brake's %d weeks this pass are used", control.MaxWeeksPerPass)
		}
		if _, verified := providerUsageArchiveDigestVerified.Load(verifiedKey); !verified {
			rows, digest, err := providerUsageArchiveLeafDigest(ctx, conn, leaf)
			if err != nil {
				return fmt.Sprintf("the week's digest: %s", err)
			}
			if rows != figures.rows || digest != figures.exportDigest {
				return fmt.Sprintf("the week's %d rows have digest %s, not export %s's %s", rows, digest, figures.exportFile, figures.exportDigest)
			}
			providerUsageArchiveDigestVerified.Store(verifiedKey, true)
		}
		if control.Mode == ProviderUsageArchiveExportModeDryRun {
			control.takeWeek()
			glog.Infof("[partitions]st_provider_usage_archive dry run: %s would drop, its %d rows held by export %s\n", leaf, figures.rows, figures.exportFile)
			return fmt.Sprintf("dry run: would drop, its %d rows held by export %s", figures.rows, figures.exportFile)
		}
		return ""
	}
	// in the drop transaction, after the detach: the rows are the ones whose
	// digest this process matched before the drop, the copy covers them, and
	// the drop is recorded with it
	if _, verified := providerUsageArchiveDigestVerified.Load(verifiedKey); !verified {
		return fmt.Sprintf("the week's digest was not matched against export %s before the drop", figures.exportFile)
	}
	tag, err := conn.Exec(ctx, `UPDATE st_provider_usage_archive_export SET dropped_time = now() AT TIME ZONE 'UTC' WHERE export_id = $1::uuid AND dropped_time IS NULL`, figures.exportId)
	if err != nil {
		return fmt.Sprintf("record the drop: %s", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Sprintf("export %s was already used", figures.exportFile)
	}
	glog.Infof("[partitions]st_provider_usage_archive drops the week of %s: its %d rows are in export %s\n", r.Lower.Format(time.DateOnly), figures.rows, figures.exportFile)
	return ""
}

// The exports whose digest this process matched against their leaf, by export
// id, row count and digest.
var providerUsageArchiveDigestVerified sync.Map

// The leaf's rows and row digest, read in a transaction of their own on the
// connection, printed under the export's session settings.
func providerUsageArchiveLeafDigest(ctx context.Context, conn PgQuerier, leaf string) (rows int64, digest string, returnErr error) {
	pooled, ok := conn.(PgConn)
	if !ok {
		return 0, "", errors.New("the digest needs a maintenance connection")
	}
	settings := DefaultProviderUsageArchiveExportSettings()
	tx, err := pooled.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for _, statement := range append(usagearchive.SessionSettingsSql(),
		fmt.Sprintf(`SET LOCAL statement_timeout = %d`, settings.DigestTimeout.Milliseconds()),
		liftTransactionTimeoutInTxSql,
	) {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return 0, "", err
		}
	}
	if err := tx.QueryRow(ctx, usagearchive.DigestSql("public."+quoteIdentifier(leaf))).Scan(&rows, &digest); err != nil {
		return 0, "", err
	}
	return rows, digest, nil
}

// The fence's branch for a week before its 52 weeks, in the drop transaction
// after the opt-in: the export control is on, the pass's brake has a week
// left, and the week ends by the floor in force on the database's clock and
// its SN epochs. It raises to refuse.
func providerUsageArchiveExportFence(ctx context.Context, tx PgTx, r PartitionRange) {
	keptUntil := r.Upper.Add(providerUsageArchiveRetention).Format(time.RFC3339)
	control := providerUsageArchiveExportControlIn(ctx)
	if control == nil || control.Mode != ProviderUsageArchiveExportModeOn {
		panic(fmt.Errorf("archived provider usage is append-only: the week of %s is kept until %s, or until its export drops it with the usage archive export control on", r.Lower.Format(time.DateOnly), keptUntil))
	}
	var byFloor bool
	Raise(tx.QueryRow(
		ctx,
		fmt.Sprintf(
			`SELECT coalesce($1::timestamp <= least($2::timestamp, (now() AT TIME ZONE 'UTC') - interval '%d seconds', (%s)), false)`,
			int64(usagearchive.MinCloseAge/time.Second),
			providerUsageArchiveSnFloorSql,
		),
		r.Upper.UTC().Format(partitionTimestampLayout),
		control.CloseTimeFloor.UTC().Format(partitionTimestampLayout),
	).Scan(&byFloor))
	if !byFloor {
		panic(fmt.Errorf("archived provider usage is append-only: the week of %s ends after the export's close time floor, and is kept until %s", r.Lower.Format(time.DateOnly), keptUntil))
	}
	if !control.takeWeek() {
		panic(fmt.Errorf("the usage archive export brake's %d weeks this pass are used; the week of %s waits", control.MaxWeeksPerPass, r.Lower.Format(time.DateOnly)))
	}
}
