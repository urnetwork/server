package main

import (
    "time"
    "fmt"
    "net/http"
    "os"
    "syscall"

    "github.com/docopt/docopt-go"
    
    "bringyour.com/bringyour"
    "bringyour.com/bringyour/router"
    "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/controller"
)


type TaskWorker struct {
    quitEvent *bringyour.Event
}

func (self *TaskWorker) stats() {
    for !self.quitEvent.IsSet() {
        stats := model.ComputeStats90()
        model.ExportStats(stats)

        self.quitEvent.WaitForSet(60 * time.Second)
    }
}

func (self *TaskWorker) warmEmail() {
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
        // todo replace with reallilwidget@icloud.com
        "xcolwell@icloud.com",
    }
    // todo add icloud.com, mail.com, hushmail, mailfence, tutanota

    for !self.quitEvent.IsSet() {
        for _, email := range emails {
            controller.TestAuthVerifyCode(email)
        }

        self.quitEvent.WaitForSet(15 * time.Minute)
    }
}


func main() {
    usage := `BringYour task worker.

Usage:
  taskworker [--port=<port>]
  taskworker -h | --help
  taskworker --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].`

    opts, err := docopt.ParseArgs(usage, os.Args[1:], bringyour.RequireVersion())
    if err != nil {
        panic(err)
    }

    // bringyour.Logger().Printf("%s\n", opts)

    quitEvent := bringyour.NewEvent()

    closeFn := quitEvent.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM)
    defer closeFn()

    startTaskWorker(quitEvent)
    
    routes := []*router.Route{
        router.NewRoute("GET", "/status", router.WarpStatus),
    }

    port, _ := opts.Int("--port")

    bringyour.Logger().Printf(
        "Serving %s %s on *:%d\n",
        bringyour.RequireEnv(),
        bringyour.RequireVersion(),
        port,
    )

    routerHandler := router.NewRouter(routes)
    err = http.ListenAndServe(fmt.Sprintf(":%d", port), routerHandler)
    bringyour.Logger().Fatal(err)
}


func startTaskWorker(quitEvent *bringyour.Event) *TaskWorker {

    worker := &TaskWorker{
        quitEvent: quitEvent,
    }

    go worker.stats()
    go worker.warmEmail()

    return worker
}

