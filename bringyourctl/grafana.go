package main

// Grafana commands act on <env>-grafana.<domain> (or --grafana_url) as the
// grafana admin from vault grafana.yml. Alert routing also reads the optional
// Slack webhook from vault monitor.yml alerts.slack_webhook_url, the webhook
// the monitor's notifier posts to. The webhook is a credential: no command
// prints it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/grafana"
)

// load the default dashboards into the grafana service, then reconcile the
// alert routing
func grafanaLoadDefaults(opts docopt.Opts) {
	grafanaUrl, adminPassword := grafanaAdminTarget(opts)

	fmt.Printf("Loading default dashboards into %s (folder %s)\n", grafanaUrl, grafana.FolderTitle)

	titles, public, err := grafana.LoadDefaults(context.Background(), grafanaUrl, "admin", adminPassword)
	if err != nil {
		panic(err)
	}
	if err := grafanaReconcileAlerting(context.Background(), os.Stdout, grafanaUrl, adminPassword, grafanaAlertRoutingSettings()); err != nil {
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

// reconcile only the alert routing: the competition route and the Slack
// default contact point
func grafanaReconcileAlertingCommand(opts docopt.Opts) {
	grafanaUrl, adminPassword := grafanaAdminTarget(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := grafanaReconcileAlerting(ctx, os.Stdout, grafanaUrl, adminPassword, grafanaAlertRoutingSettings()); err != nil {
		panic(err)
	}
}

// send one test notification through the default contact point
func grafanaTestAlertCommand(opts docopt.Opts) {
	grafanaUrl, adminPassword := grafanaAdminTarget(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := grafanaTestAlert(ctx, os.Stdout, grafanaUrl, adminPassword, grafanaAlertRoutingSettings()); err != nil {
		panic(err)
	}
}

func grafanaAdminTarget(opts docopt.Opts) (string, string) {
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
	return grafanaUrl, adminPassword
}

// The webhook is optional: an absent monitor.yml or an empty value leaves the
// default receiver as it is. A present but unreadable or malformed document
// stops the command rather than silently dropping the Slack route.
func grafanaAlertRoutingSettings() grafana.AlertRoutingSettings {
	resource, err := server.Vault.SimpleResource("monitor.yml")
	if err != nil {
		return grafana.AlertRoutingSettings{}
	}
	data, err := resource.BytesE()
	if err != nil {
		panic(errors.New("vault monitor.yml could not be read"))
	}
	settings, err := grafana.AlertRoutingSettingsFromMonitorVault(data)
	if err != nil {
		panic(err)
	}
	return settings
}

func grafanaReconcileAlerting(ctx context.Context, out io.Writer, grafanaUrl string, adminPassword string, settings grafana.AlertRoutingSettings) error {
	result, err := grafana.ReconcileAlertRouting(ctx, grafanaUrl, "admin", adminPassword, settings)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Competition warn/page alerts route to %s\n", grafana.CompetitionIncidentContact)
	if result.SlackConfigured {
		fmt.Fprintf(out, "Unrouted alerts go to Slack: default receiver %s (was %s)\n", result.DefaultReceiver, result.PreviousDefaultReceiver)
		fmt.Fprintf(out, "Send a test notification with: bringyourctl grafana test-alert\n")
		return nil
	}
	fmt.Fprintf(out, "Slack default contact point skipped: vault monitor.yml alerts.slack_webhook_url is empty. The default receiver stays %s.\n", result.DefaultReceiver)
	if !result.DefaultReceiverHasIntegrations {
		fmt.Fprintf(out, "WARNING: the default receiver %s has no integrations, so alerts that match no child route reach no one.\n", result.DefaultReceiver)
	}
	return nil
}

func grafanaTestAlert(ctx context.Context, out io.Writer, grafanaUrl string, adminPassword string, settings grafana.AlertRoutingSettings) error {
	result, err := grafana.SendDefaultAlertTest(ctx, grafanaUrl, "admin", adminPassword, settings)
	if err != nil {
		return err
	}
	if result.Status != "success" {
		return fmt.Errorf("Grafana could not deliver the test notification through %s after %s: %s", grafana.DefaultAlertContactName, result.Duration, result.Error)
	}
	fmt.Fprintf(out, "Grafana delivered a test notification through %s (Slack) in %s. Confirm it in the Slack channel.\n", grafana.DefaultAlertContactName, result.Duration)
	return nil
}
