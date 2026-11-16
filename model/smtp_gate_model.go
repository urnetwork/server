package model

import (
	"context"
	"time"

	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// What smtpedge's production send gate loads of one network, off every
// preamble's path (SMTP-M9-20; §14.5, §14.7, §7.6, §12.7): the block's gate
// (server/cli/smtpedge/gate.go) calls this when a preamble missed its memory
// and on its refresh, never inside a preamble (I-12, §22.5). Each fact comes
// from the table that owns it, read only in a binary that links its group, and
// a group not linked leaves its fact empty, which the gate's rows answer as
// closed: no mail_network row is mail never enabled (DENIED), no label is a
// gate not loaded (TRY_LATER), no verified domain sends From none.
//
// Not loaded here yet, each answering closed where the gate reads it: a live
// alias's and a Team scope's zones (§12.7), the names a reviewer allowed
// (ABUSE-M5-08), and network_dns_zone.report_token, which has no column until
// plan-DNS-rua-token's lands, so Feedback-ID cannot be issued and every
// preamble answers TRY_LATER until it does. The limits are Quota.Limits', which
// the gate reads beside this.
func GetSmtpGateFacts(ctx context.Context, networkId server.Id, root string) (*smtpedgeservice.SmtpGateFacts, error) {
	facts := &smtpedgeservice.SmtpGateFacts{
		Label: GetNetworkLabel(ctx, networkId, root),
	}
	if server.SidecarMg7Linked() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT create_time FROM mail_network WHERE network_id = $1`, networkId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					var createTime time.Time
					server.Raise(result.Scan(&createTime))
					facts.MailEnableTime = createTime
				}
			})
		})
	}
	if server.SidecarMg6Linked() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT domain FROM network_domain WHERE network_id = $1 AND verify_time IS NOT NULL ORDER BY domain`,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var domain string
					server.Raise(result.Scan(&domain))
					facts.VerifiedDomains = append(facts.VerifiedDomains, domain)
				}
			})
		})
	}
	return facts, nil
}
