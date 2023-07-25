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

    worker := startTaskWorker(quitEvent)
    
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
    err := http.ListenAndServe(fmt.Sprintf(":%d", port), routerHandler)
    bringyour.Logger().Fatal(err)
}


func startTaskWorker(quitEvent *bringyour.Event) *TaskWorker {

    worker := &TaskWorker{
        quitEvent: quitEvent,
    }

    go worker.stats()

    return worker
}

