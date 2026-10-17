package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server"
)

// Same Main LAN routing and credentials as migrate/load-defaults, with an
// enforced read-only session before any connection opens. Never call migration
// helpers here: even the historical version diagnostic can create a table.
func releasePreflightCommand(opts docopt.Opts) {
	defer func() {
		if recover() != nil {
			fmt.Fprintln(os.Stderr, "Main release preflight failed while opening read-only resources; check LAN routing, credentials, and database availability (credential-bearing error detail withheld)")
			os.Exit(1)
		}
	}()
	server.RequirePgReadOnly()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var probeErr error
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		query := func(ctx context.Context, sql string, dest ...any) error {
			return conn.QueryRow(ctx, sql).Scan(dest...)
		}
		selected := conn.Conn().Config()
		probeErr = checkReleaseDatabase(ctx, query, server.MigrationCount(), selected.Database, selected.User, os.Stdout)
	}, server.OptNoRetry())
	if probeErr == nil {
		grafanaURL, password := grafanaAdminTarget(opts)
		brevoKey := server.Vault.RequireSimpleResource("brevo.yml").RequireString("brevo", "api_key")
		client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		probeErr = checkReleaseHTTP(ctx, client, grafanaURL, password, "https://api.brevo.com/v3", brevoKey, os.Stdout)
	}
	if probeErr != nil {
		fmt.Fprintln(os.Stderr, probeErr)
		os.Exit(1)
	}
}
