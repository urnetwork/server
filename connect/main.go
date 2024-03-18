package main

import (
    "context"
    // "time"
    "fmt"
    "net/http"
    "os"
    "syscall"

    "github.com/docopt/docopt-go"
    
    "bringyour.com/bringyour"
    "bringyour.com/bringyour/router"
    // "bringyour.com/bringyour/model"
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

    cancelCtx, cancel := context.WithCancel(context.Background())
    defer cancel()

    quitEvent := bringyour.NewEventWithContext(cancelCtx)

    closeFn := quitEvent.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM)
    defer closeFn()

    exchange := NewExchangeFromEnvWithDefaults(cancelCtx)
    defer exchange.Close()

    connectHandler := NewConnectHandlerWithDefaults(cancelCtx, exchange)

    routes := []*router.Route{
        router.NewRoute("GET", "/status", router.WarpStatus),
        router.NewRoute("GET", "/", connectHandler.Connect),
    }

    port, _ := opts.Int("--port")

    bringyour.Logger().Printf(
        "Serving %s %s on *:%d\n",
        bringyour.RequireEnv(),
        bringyour.RequireVersion(),
        port,
    )

    routerHandler := router.NewRouter(cancelCtx, routes)
    if err := http.ListenAndServe(fmt.Sprintf(":%d", port), routerHandler); err != nil {
        bringyour.Logger().Fatal(err)
    }
}
