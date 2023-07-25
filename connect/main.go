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


func main() {
    usage := `BringYour task worker.

Usage:
  connect [--port=<port>]
  connect -h | --help
  connect --version

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

    connectRouter := NewConnectRouter(quitEvent)

    routes := []*router.Route{
        router.NewRoute("GET", "/status", router.WarpStatus),
        router.NewRoute("GET", "/", connectRouter.Connect),
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
