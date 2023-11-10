package main

import (
    "context"
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

    // FIXME signal cancel
    cancelCtx, cancel := context.WithCancel(context.Background())
    defer cancel()

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
        router.NewRoute("POST", "/network/auth-client", handlers.AuthNetworkClient),
        router.NewRoute("POST", "/network/remove-client", handlers.RemoveNetworkClient),
        router.NewRoute("GET", "/network/clients", handlers.NetworkClients),
        router.NewRoute("GET", "/network/provider-locations", handlers.NetworkGetActiveProviderLocations),
        router.NewRoute("POST", "/network/find-provider-locations", handlers.NetworkFindActiveProviderLocations),
        router.NewRoute("POST", "/network/find-locations", handlers.NetworkFindLocations),
        router.NewRoute("POST", "/network/find-providers", handlers.NetworkFindActiveProviders),
        router.NewRoute("POST", "/preferences/set-preferences", handlers.PreferencesSet),
        router.NewRoute("POST", "/feedback/send-feedback", handlers.FeedbackSend),
        router.NewRoute("POST", "/pay/stripe", handlers.StripeWebhook),
        router.NewRoute("POST", "/pay/coinbase", handlers.CoinbaseWebhook),
        router.NewRoute("POST", "/pay/circle", handlers.CircleWebhook),
    }

    // bringyour.Logger().Printf("%s\n", opts)

    port, _ := opts.Int("--port")

    bringyour.Logger().Printf(
        "Serving %s %s on *:%d\n",
        bringyour.RequireEnv(),
        bringyour.RequireVersion(),
        port,
    )

    routerHandler := router.NewRouter(cancelCtx, routes)
    err = http.ListenAndServe(fmt.Sprintf(":%d", port), routerHandler)

    bringyour.Logger().Fatal(err)
}

