package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The gate's facts on a private database (SMTP-M9-20): mail_network's enable
// time, the verified domains and none that are not, and an unknown network's
// facts empty, which the gate answers closed.
func TestSmtpGateFactsReadOneNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg6Linked() || !server.SidecarMg7Linked() {
			return
		}
		ctx := context.Background()
		networkId := server.NewId()
		enabled := server.NowUtc().Add(-48 * time.Hour).Truncate(time.Millisecond)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO mail_network (network_id, create_time) VALUES ($1, $2)`, networkId, enabled))
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_domain (network_id, domain, challenge, verify_time) VALUES ($1, 'mail.team.example', 'c1', $2), ($1, 'pending.team.example', 'c2', NULL)`,
				networkId, enabled,
			))
		})
		facts, err := GetSmtpGateFacts(ctx, networkId, "root.example")
		if err != nil {
			t.Fatal(err)
		}
		if !facts.MailEnableTime.Equal(enabled) || len(facts.VerifiedDomains) != 1 || facts.VerifiedDomains[0] != "mail.team.example" {
			t.Fatalf("facts %+v", facts)
		}
		empty, err := GetSmtpGateFacts(ctx, server.NewId(), "root.example")
		if err != nil || !empty.MailEnableTime.IsZero() || len(empty.VerifiedDomains) != 0 || empty.Label != "" || empty.ReportToken != "" {
			t.Fatalf("an unknown network's facts %+v, %v", empty, err)
		}
	})
}

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
