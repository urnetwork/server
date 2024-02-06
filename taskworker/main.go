package main

import (
    "context"
    "time"
    "fmt"
    "net/http"
    "os"
    "syscall"

    "github.com/docopt/docopt-go"
    
    "bringyour.com/service/taskworker/work"    
    "bringyour.com/bringyour"
    "bringyour.com/bringyour/session"
    "bringyour.com/bringyour/router"
    "bringyour.com/bringyour/task"
)


const RemoveTaskTimeout = 90 * 24 * time.Hour
const RetryTimeoutAfterError = 30 * time.Second
const PollTimeout = 1 * time.Second


func main() {
    usage := `BringYour task worker.

Usage:
  taskworker [--port=<port>] [--count=<count>] [--batch_size=<batch_size>]
  taskworker -h | --help
  taskworker --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].
  -n --count=<count>  Number of worker processes [default: 16].
  -b --batch_size=<batch_size>  Batch size [default: 8].`

    opts, err := docopt.ParseArgs(usage, os.Args[1:], bringyour.RequireVersion())
    if err != nil {
        panic(err)
    }

    quitEvent := bringyour.NewEventWithContext(context.Background())
    closeFn := quitEvent.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM)
    defer closeFn()

    count, _ := opts.Int("--count")
    batchSize, _ := opts.Int("--batch_size")
    port, _ := opts.Int("--port")

    taskWorker := initTaskWorker(quitEvent.Ctx)
    for i := 0; i < count; i += 1 {
        go runTaskWorker(quitEvent.Ctx, taskWorker, batchSize)
    }
    
    routes := []*router.Route{
        router.NewRoute("GET", "/status", router.WarpStatus),
    }


    bringyour.Logger().Printf(
        "Serving %s %s on *:%d\n",
        bringyour.RequireEnv(),
        bringyour.RequireVersion(),
        port,
    )

    routerHandler := router.NewRouter(quitEvent.Ctx, routes)
    err = http.ListenAndServe(fmt.Sprintf(":%d", port), routerHandler)
    bringyour.Logger().Fatal(err)
}


func initTaskWorker(ctx context.Context) *task.TaskWorker {
    taskWorker := task.NewTaskWorker(ctx)
    
    taskWorker.AddTargets(
        task.NewTaskTargetWithPost(work.WarmEmail, work.WarmEmailPost),
        task.NewTaskTargetWithPost(work.ExportStats, work.ExportStatsPost),
        task.NewTaskTargetWithPost(TaskCleanup, TaskCleanupPost),
    )

    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)

        work.ScheduleWarmEmail(clientSession, tx)
        work.ScheduleExportStats(clientSession, tx)
        ScheduleTaskCleanup(clientSession, tx)
    }))

    return taskWorker
}


func runTaskWorker(ctx context.Context, taskWorker *task.TaskWorker, batchSize int) {
    for {
        select {
        case <- ctx.Done():
            return
        default:
        }

        finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds, err := taskWorker.EvalTasks(batchSize)
        if err != nil {
            bringyour.Logger().Printf("Error running tasks: %s\n", err)
            select {
            case <- ctx.Done():
                return
            case <- time.After(RetryTimeoutAfterError):
            }
        } else if len(finishedTaskIds) + len(rescheduledTaskIds) + len(postRescheduledTaskIds) == 0 {
            select {
            case <- ctx.Done():
                return
            case <- time.After(PollTimeout):
            }
        }
    }
}


// PERIODIC CLEANUP

type TaskCleanupArgs struct {
}

type TaskCleanupResult struct {
}

func ScheduleTaskCleanup(clientSession *session.ClientSession, tx bringyour.PgTx) {
    task.ScheduleTaskInTx(
        tx,
        TaskCleanup,
        &TaskCleanupArgs{},
        clientSession,
        task.RunOnce("task_cleanup"),
        task.RunAt(time.Now().Add(1 * time.Hour)),
    )
}

func TaskCleanup(
    taskCleanup *TaskCleanupArgs,
    clientSession *session.ClientSession,
) (*TaskCleanupResult, error) {
    minTime := time.Now().Add(-RemoveTaskTimeout)
    task.RemoveFinishedTasks(clientSession.Ctx, minTime)
    return &TaskCleanupResult{}, nil
}

func TaskCleanupPost(
    taskCleanup *TaskCleanupArgs,
    taskCleanupResult *TaskCleanupResult,
    clientSession *session.ClientSession,
    tx bringyour.PgTx,
) error {
    ScheduleTaskCleanup(clientSession, tx)
    return nil
}
