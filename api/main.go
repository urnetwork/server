package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/docopt/docopt-go"
	
	"bringyour.com/api/handlers"
	"bringyour.com/bringyour"
	"bringyour.com/bringyour/router"
)



var routes = []*router.Route{
	router.NewRoute("GET", "/health", handlers.Health),
	router.NewRoute("GET", "/stats/last-90", handlers.StatsLast90),
	router.NewRoute("POST", "/auth/login", handlers.AuthLogin),
	router.NewRoute("POST", "/auth/login-with-password", handlers.AuthLoginWithPassword),
	router.NewRoute("POST", "/auth/validate", handlers.AuthValidate),
	router.NewRoute("POST", "/auth/validate-send", handlers.AuthValidateSend),
	router.NewRoute("POST", "/auth/password-reset", handlers.AuthPasswordReset),
	router.NewRoute("POST", "/auth/password-set", handlers.AuthPasswordSet),
	router.NewRoute("POST", "/auth/network-check", handlers.AuthNetworkCheck),
	router.NewRoute("POST", "/auth/network-create", handlers.AuthNetworkCreate),
	router.NewRoute("POST", "/preferences/set-preferences", handlers.PreferencesSet),
	router.NewRoute("POST", "/feedback/send-feedback", handlers.FeedbackSend),
}


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

	opts, err := docopt.ParseArgs(usage, os.Args[1:], bringyour.Env.Version())
	if err != nil {
		panic(err)
	}

	type ApiArgs struct {
		Port int `docopt:"--port"`
	}

	// bringyour.Logger().Printf("%s\n", opts)

	args := ApiArgs{}
	opts.Bind(&args)

	bringyour.Logger().Printf(
		"Serving %s %s on *:%d\n",
		bringyour.Env.EnvName(),
		bringyour.Env.Version(),
		args.Port,
	)

	routerHandler := router.NewRouter(routes)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", args.Port), routerHandler))
}
