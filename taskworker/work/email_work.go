package work

import (
    "time"
    
    "bringyour.com/bringyour"
    "bringyour.com/bringyour/task"
    // "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/controller"
    "bringyour.com/bringyour/session"
)


type WarmEmailArgs struct {
}

type WarmEmailResult struct {
}

func ScheduleWarmEmail(clientSession *session.ClientSession, tx bringyour.PgTx) {
    task.ScheduleTaskInTx(
        tx,
        WarmEmail,
        &WarmEmailArgs{},
        clientSession,
        task.RunOnce("warm_email"),
        task.RunAt(time.Now().Add(1 * time.Hour)),
    )
}

func WarmEmail(
	warmEmail *WarmEmailArgs,
	clientSession *session.ClientSession,
) (*WarmEmailResult, error) {
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
    tx bringyour.PgTx,
) error {
    ScheduleWarmEmail(clientSession, tx)
    return nil
}
