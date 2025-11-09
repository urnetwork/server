package work

import (
	"time"

	"github.com/urnetwork/server/v2025"
	// "github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/task"

	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/session"
)

type DbMaintenanceArgs struct {
	Epoch uint64 `json:"epoch"`
}

type DbMaintenanceResult struct {
}

func ScheduleDbMaintenance(clientSession *session.ClientSession, tx server.PgTx, epoch uint64) {
	runAt := func() time.Time {
		now := server.NowUtc()
		year, month, day := now.Date()
		// start maintenance at the earliest available of 3:00 UTC daily or 23:00 PST (7:00 UTC)
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
	}()

	task.ScheduleTaskInTx(
		tx,
		DbMaintenance,
		&DbMaintenanceArgs{
			Epoch: epoch,
		},
		clientSession,
		task.RunOnce("db_maintenance"),
		task.RunAt(runAt),
		task.MaxTime(12*time.Hour),
	)
}

func DbMaintenance(dbMaintenance *DbMaintenanceArgs, clientSession *session.ClientSession) (*DbMaintenanceResult, error) {
	server.DbMaintenance(clientSession.Ctx, dbMaintenance.Epoch)
	return &DbMaintenanceResult{}, nil
}

func DbMaintenancePost(
	dbMaintenance *DbMaintenanceArgs,
	dbMaintenanceResult *DbMaintenanceResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleDbMaintenance(clientSession, tx, dbMaintenance.Epoch+1)
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
