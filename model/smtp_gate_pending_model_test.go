//go:build sidecar_pending

package model

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The gate's facts that need the pending migration: the report token is MG6's
// column of MG5's network_dns_zone, and the zone's root comes from testDnsRoot,
// which dns_write_model_test.go declares under this tag. The untagged ones are
// in smtp_gate_model_test.go.

// The zone's report token in root is the facts' Feedback-ID token, minted with the zone's set (DNS-M8-01), and a
// network with no zone in root has none, which the gate answers TRY_LATER.
func TestSmtpGateFactsReadTheZonesReportToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() || !server.SidecarMg6Linked() {
			return
		}
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "gate-"+server.NewId().String()[:8], server.NewId())
		if _, err := EnsureNetworkZone(ctx, networkId, root); err != nil {
			t.Fatal(err)
		}
		var token *string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT report_token FROM network_dns_zone WHERE root = $1 AND network_id = $2`, root, networkId).Scan(&token))
		})
		if token == nil || len(*token) != 26 {
			t.Fatalf("the zone holds report token %v", token)
		}
		// the root as the engine's settings name it, with no final dot
		facts, err := GetSmtpGateFacts(ctx, networkId, strings.TrimSuffix(root, "."))
		if err != nil {
			t.Fatal(err)
		}
		if facts.ReportToken != *token || facts.Label == "" {
			t.Fatalf("facts %+v", facts)
		}
		other, err := GetSmtpGateFacts(ctx, server.NewId(), root)
		if err != nil || other.ReportToken != "" {
			t.Fatalf("a network with no zone %+v, %v", other, err)
		}
	})
}
