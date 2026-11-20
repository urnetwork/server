package model

import (
	"context"
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
