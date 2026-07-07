package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/grafana"
)

// load the default dashboards into the grafana service
// as the grafana admin (vault grafana.yml)
func grafanaLoadDefaults(opts docopt.Opts) {
	grafanaUrl, err := opts.String("--grafana_url")
	if err != nil || grafanaUrl == "" {
		env, envErr := server.Env()
		domain, domainErr := server.Domain()
		if envErr != nil || domainErr != nil {
			panic(errors.New("WARP_ENV and WARP_DOMAIN are not set. Use --grafana_url=<grafana_url>."))
		}
		grafanaUrl = fmt.Sprintf("https://%s-grafana.%s", env, domain)
	}

	adminPassword := server.Vault.RequireSimpleResource("grafana.yml").RequireString("grafana", "admin_password")

	fmt.Printf("Loading default dashboards into %s (folder %s)\n", grafanaUrl, grafana.FolderTitle)

	titles, public, err := grafana.LoadDefaults(context.Background(), grafanaUrl, "admin", adminPassword)
	if err != nil {
		panic(err)
	}
	for _, title := range titles {
		fmt.Printf("- %s\n", title)
	}
	if 0 < len(public) {
		fmt.Printf("\nPublic dashboards (read-only, no login):\n")
		for _, pd := range public {
			fmt.Printf("- %s\n    %s/public-dashboards/%s\n", pd.Title, grafanaUrl, pd.AccessToken)
		}
		fmt.Printf("\nPublic directory: %s/stats\n", grafanaUrl)
	}
}
