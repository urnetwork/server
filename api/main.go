package main

import (
    "fmt"
    "net/http"
    "os"

    "github.com/docopt/docopt-go"
    
    "bringyour.com/service/api/handlers"
    "bringyour.com/bringyour"
    "bringyour.com/bringyour/router"
)


func main() {
    usage := `BringYour API server.

Usage:
  api [--port=<port>]
  api -h | --help
  api --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].`

    opts, err := docopt.ParseArgs(usage, os.Args[1:], bringyour.RequireVersion())
    if err != nil {
        panic(err)
    }

    routes := []*router.Route{
        router.NewRoute("GET", "/status", router.WarpStatus),
        router.NewRoute("GET", "/stats/last-90", handlers.StatsLast90),
        router.NewRoute("POST", "/auth/login", handlers.AuthLogin),
        router.NewRoute("POST", "/auth/login-with-password", handlers.AuthLoginWithPassword),
        router.NewRoute("POST", "/auth/verify", handlers.AuthVerify),
        router.NewRoute("POST", "/auth/verify-send", handlers.AuthVerifySend),
        router.NewRoute("POST", "/auth/password-reset", handlers.AuthPasswordReset),
        router.NewRoute("POST", "/auth/password-set", handlers.AuthPasswordSet),
        router.NewRoute("POST", "/auth/network-check", handlers.AuthNetworkCheck),
        router.NewRoute("POST", "/auth/network-create", handlers.AuthNetworkCreate),
        router.NewRoute("POST", "/preferences/set-preferences", handlers.PreferencesSet),
        router.NewRoute("POST", "/feedback/send-feedback", handlers.FeedbackSend),
    }

    // bringyour.Logger().Printf("%s\n", opts)

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

