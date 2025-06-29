package work

import (
	"time"

	"github.com/urnetwork/server"
	// "github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
)

type DbMaintenanceArgs struct {
}

type DbMaintenanceResult struct {
}

func ScheduleDbMaintenance(clientSession *session.ClientSession, tx server.PgTx) {
	runAt := func() time.Time {
		now := time.Now().UTC()
		year, month, day := now.Date()
		return time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC)
	}()

	task.ScheduleTaskInTx(
		tx,
		DbMaintenance,
		&DbMaintenanceArgs{},
		clientSession,
		task.RunOnce("db_maintenance"),
		task.RunAt(runAt),
		task.MaxTime(1*time.Hour),
	)
}

func DbMaintenance(dbMaintenance *DbMaintenanceArgs, clientSession *session.ClientSession) (*DbMaintenanceResult, error) {
	server.DbMaintenance(clientSession.Ctx)
	return &DbMaintenanceResult{}, nil
}

func DbMaintenancePost(
	dbMaintenance *DbMaintenanceArgs,
	dbMaintenanceResult *DbMaintenanceResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleDbMaintenance(clientSession, tx)
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
		task.RunAt(time.Now().Add(1*time.Hour)),
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
