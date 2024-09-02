package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/docopt/docopt-go"

	"bringyour.com/bringyour"
	"bringyour.com/service/api/apirouter"
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

	// bringyour.Logger().Printf("%s\n", opts)

	port, _ := opts.Int("--port")

	bringyour.Logger().Printf(
		"Serving %s %s on *:%d\n",
		bringyour.RequireEnv(),
		bringyour.RequireVersion(),
		port,
	)

	err = http.ListenAndServe(fmt.Sprintf(":%d", port), apirouter.NewAPIRouter())

	bringyour.Logger().Fatal(err)
}
