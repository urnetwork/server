package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/bringyour"
	"github.com/urnetwork/server/bringyour/controller"
	"github.com/urnetwork/server/bringyour/router"
	"github.com/urnetwork/server/gpt/handlers"
)

func main() {
	usage := `BringYour GPT API server.

Usage:
  api [--port=<port>]
  api innerhtml --url=<url>
  api privacypolicy --url=<url>
  api -h | --help
  api --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].
  --url=<url>   Test url.`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], bringyour.RequireVersion())
	if err != nil {
		panic(err)
	}

	// FIXME signal cancel
	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if test, _ := opts.Bool("innerhtml"); test {
		url, _ := opts.String("--url")
		html, err := controller.GetBodyHtml(cancelCtx, url, 15*time.Second)
		if err != nil {
			panic(err)
		}
		fmt.Printf("%s\n", html)
	} else if test, _ := opts.Bool("privacypolicy"); test {
		url, _ := opts.String("--url")

		collector := controller.NewPrivacyPolicyCollector(
			cancelCtx,
			"test",
			[]string{url},
			30*time.Second,
			10*time.Second,
		)
		privacyPolicyText, extractedUrls, err := collector.Run()
		if err != nil {
			panic(err)
		}
		for _, extractedUrl := range extractedUrls {
			fmt.Printf("Extracted URL: %s\n", extractedUrl)
		}
		fmt.Printf("%s\n", privacyPolicyText)
	} else {
		routes := []*router.Route{
			router.NewRoute("GET", "/privacy.txt", router.Txt),
			router.NewRoute("GET", "/terms.txt", router.Txt),
			router.NewRoute("GET", "/vdp.txt", router.Txt),
			router.NewRoute("GET", "/status", router.WarpStatus),
			router.NewRoute("POST", "/gpt/privacypolicy", handlers.GptPrivacyPolicy),
			router.NewRoute("POST", "/gpt/bemyprivacyagent", handlers.GptBeMyPrivacyAgent),
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
}
