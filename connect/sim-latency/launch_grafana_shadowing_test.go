// Launch preflight must reject a canonical-looking route that earlier manual policy consumes before the existing main receiver.
package main

import (
	"context"
	"testing"

	servergrafana "github.com/urnetwork/server/grafana"
)

// The old existence-only shared helper accepts this real composed preflight shape despite first-match routing semantics.
func TestLaunchGrafanaRejectsShadowedCanonicalMainRoute(t *testing.T) {
	manual := launchCanonicalCompetitionRoute(servergrafana.CompetitionAlertContactName)
	manual["group_wait"] = "5s"
	policy := map[string]any{"receiver": servergrafana.DefaultAlertContactName, "routes": []any{manual, launchCanonicalCompetitionRoute(servergrafana.DefaultAlertContactName)}}
	contacts := append(launchExistingMainContacts(), map[string]any{"uid": servergrafana.CompetitionAlertContactUid, "name": servergrafana.CompetitionAlertContactName, "type": "email", "settings": map[string]any{"addresses": servergrafana.CompetitionIncidentContact}})
	server := launchGrafanaRoutingServer(t, contacts, policy)
	if _, err := checkCompetitionGrafana(context.Background(), &launchPreflightConfig{GrafanaUrl: server.URL, GrafanaToken: "synthetic-monitoring-token"}); err == nil {
		t.Fatal("unreachable canonical route passed launch preflight")
	}
}
