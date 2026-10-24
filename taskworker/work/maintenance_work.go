package work

import (
	"time"

	"github.com/urnetwork/server"
	// "github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
)

// An interrupted or deferred run carries what it finished in the next task's
// arguments, so the same epoch continues instead of being skipped. Arguments
// written before these fields existed decode to an epoch with nothing done.
type DbMaintenanceArgs struct {
	Epoch       uint64   `json:"epoch"`
	DoneTables  []string `json:"done_tables,omitempty"`
	DoneIndexes []string `json:"done_indexes,omitempty"`
}

type DbMaintenanceResult struct {
	// empty when the epoch finished
	Stopped     server.DbMaintenanceStop `json:"stopped,omitempty"`
	Reason      string                   `json:"reason,omitempty"`
	DoneTables  []string                 `json:"done_tables,omitempty"`
	DoneIndexes []string                 `json:"done_indexes,omitempty"`
}

// start maintenance at the earliest available of 3:00 UTC daily or 23:00 PST (7:00 UTC)
func dbMaintenanceNextRunAt(now time.Time) time.Time {
	year, month, day := now.Date()
	orderedOptions := []time.Time{
		time.Date(year, month, day, 3, 0, 0, 0, time.UTC),
		time.Date(year, month, day, 7, 0, 0, 0, time.UTC),
		time.Date(year, month, day+1, 3, 0, 0, 0, time.UTC),
		time.Date(year, month, day+1, 7, 0, 0, 0, time.UTC),
	}
	// first in the future
	for _, t := range orderedOptions {
		if now.Before(t) {
			return t
		}
	}
	return orderedOptions[len(orderedOptions)-1]
}

func ScheduleDbMaintenance(clientSession *session.ClientSession, tx server.PgTx, epoch uint64) {
	scheduleDbMaintenance(clientSession, tx, &DbMaintenanceArgs{
		Epoch: epoch,
	}, dbMaintenanceNextRunAt(server.NowUtc()))
}

func scheduleDbMaintenance(clientSession *session.ClientSession, tx server.PgTx, args *DbMaintenanceArgs, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		DbMaintenance,
		args,
		clientSession,
		task.RunOnce("db_maintenance"),
		task.RunAt(runAt),
		task.MaxTime(24*time.Hour),
	)
}

// A drain interrupts every block of a rolling deploy within minutes; the
// continuation starts after that wave instead of on a block about to drain. The
// interrupted run already cleaned up after its own rebuild, and the
// continuation's sweep drops anything that cleanup could not.
const dbMaintenanceInterruptedContinuationDelay = 5 * time.Minute

// The next task of the chain: the next epoch at the next slot once an epoch
// finished; the same epoch shortly after an interruption (a drain or
// deadline); the same epoch at the next slot after a load deferral.
func dbMaintenanceNextTask(args *DbMaintenanceArgs, result *DbMaintenanceResult, now time.Time) (*DbMaintenanceArgs, time.Time) {
	continuation := &DbMaintenanceArgs{
		Epoch:       args.Epoch,
		DoneTables:  result.DoneTables,
		DoneIndexes: result.DoneIndexes,
	}
	switch result.Stopped {
	case server.DbMaintenanceInterrupted:
		return continuation, now.Add(dbMaintenanceInterruptedContinuationDelay)
	case server.DbMaintenanceDeferred:
		return continuation, dbMaintenanceNextRunAt(now)
	default:
		return &DbMaintenanceArgs{Epoch: args.Epoch + 1}, dbMaintenanceNextRunAt(now)
	}
}

// The options of one run: the defaults, what the epoch already finished, and
// whether bloated priority indexes are rebuilt, which db_maintenance.yml
// decides when the run starts (off unless priority_reindex_enabled is true).
func dbMaintenanceOptions(dbMaintenance *DbMaintenanceArgs) *server.DbMaintenanceOptions {
	opts := server.DefaultDbMaintenanceOptions()
	opts.DoneTables = dbMaintenance.DoneTables
	opts.DoneIndexes = dbMaintenance.DoneIndexes
	opts.PriorityReindex = server.DbMaintenancePriorityReindexEnabled()
	return opts
}

func DbMaintenance(dbMaintenance *DbMaintenanceArgs, clientSession *session.ClientSession) (*DbMaintenanceResult, error) {
	opts := dbMaintenanceOptions(dbMaintenance)
	outcome := server.DbMaintenance(clientSession.Ctx, dbMaintenance.Epoch, opts)
	return &DbMaintenanceResult{
		Stopped:     outcome.Stopped,
		Reason:      outcome.Reason,
		DoneTables:  outcome.DoneTables,
		DoneIndexes: outcome.DoneIndexes,
	}, nil
}

func DbMaintenancePost(
	dbMaintenance *DbMaintenanceArgs,
	dbMaintenanceResult *DbMaintenanceResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	args, runAt := dbMaintenanceNextTask(dbMaintenance, dbMaintenanceResult, server.NowUtc())
	scheduleDbMaintenance(clientSession, tx, args, runAt)
	return nil
}

// vacuum hot tables

// A run's state goes into the next run's arguments: the table it resumes at,
// each table's last finished vacuum and analyze, and the last index pass (see
// server.VacuumHotTables). A chain scheduled fresh starts with none.
type VacuumHotTablesArgs struct {
	State server.DbHotVacuumState `json:"state"`
}

type VacuumHotTablesResult struct {
	State server.DbHotVacuumState `json:"state"`
	// when the run started, which times the next run
	StartTime time.Time `json:"start_time"`
}

func ScheduleVacuumHotTables(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleVacuumHotTables(clientSession, tx, &VacuumHotTablesArgs{}, server.NowUtc().Add(1*time.Minute))
}

// One run is active at a time: the run-once key keeps one row, and the
// claim's ownership lock one execution.
func scheduleVacuumHotTables(clientSession *session.ClientSession, tx server.PgTx, args *VacuumHotTablesArgs, runAt time.Time) {
	settings := server.DefaultDbHotVacuumSettings()
	task.ScheduleTaskInTx(
		tx,
		VacuumHotTables,
		args,
		clientSession,
		task.RunOnce("vacuum_hot_tables"),
		task.RunAt(runAt),
		// a run starts no work past its budget; this only bounds a run whose
		// cleanup outlives it
		task.MaxTime(2*settings.Interval),
	)
}

// The next run starts an interval after this one started, so the cadence holds
// while runs take less than the interval, and at once after a longer one.
func vacuumHotTablesNextRunAt(startTime time.Time, now time.Time, interval time.Duration) time.Time {
	runAt := startTime.Add(interval)
	if runAt.Before(now) {
		return now
	}
	return runAt
}

func VacuumHotTables(
	vacuumHotTables *VacuumHotTablesArgs,
	clientSession *session.ClientSession,
) (*VacuumHotTablesResult, error) {
	startTime := server.NowUtc()
	state := server.VacuumHotTables(clientSession.Ctx, vacuumHotTables.State, server.DefaultDbHotVacuumSettings())
	return &VacuumHotTablesResult{
		State:     state,
		StartTime: startTime,
	}, nil
}

func VacuumHotTablesPost(
	vacuumHotTables *VacuumHotTablesArgs,
	vacuumHotTablesResult *VacuumHotTablesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := server.DefaultDbHotVacuumSettings()
	scheduleVacuumHotTables(
		clientSession,
		tx,
		&VacuumHotTablesArgs{State: vacuumHotTablesResult.State},
		vacuumHotTablesNextRunAt(vacuumHotTablesResult.StartTime, server.NowUtc(), settings.Interval),
	)
	return nil
}

// warm email

type WarmEmailArgs struct {
}

type WarmEmailResult struct {
}

func ScheduleWarmEmail(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		WarmEmail,
		&WarmEmailArgs{},
		clientSession,
		task.RunOnce("warm_email"),
		task.RunAt(server.NowUtc().Add(1*time.Hour)),
	)
}

func WarmEmail(
	warmEmail *WarmEmailArgs,
	clientSession *session.ClientSession,
) (*WarmEmailResult, error) {
	// FIXME we need to monitor these for bounces
	// FIXME we can just use warmupinbox for now
	// send a continuous verification code message to a bunch of popular email providers
	emails := []string{
		"reallilwidget@gmail.com",
		"reallilwidget@protonmail.com",
		"reallilwidget@aol.com",
		"reallilwidget@gmx.com",
		"reallilwidget@outlook.com",
		"reallilwidget@yahoo.com",
		"reallilwidget@yandex.com",
		"reallilwidget@zohomail.com",
		"reallilwidget@icloud.com",
	}
	// todo add mail.com, hushmail, mailfence, tutanota

	for _, email := range emails {
		controller.Testing_SendAuthVerifyCode(email)
	}
	return &WarmEmailResult{}, nil
}

func WarmEmailPost(
	warmEmail *WarmEmailArgs,
	warmEmailResult *WarmEmailResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleWarmEmail(clientSession, tx)
	return nil
}
