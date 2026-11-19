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
// gate not loaded (TRY_LATER), no verified domain sends From none, and no
// report token is a Feedback-ID that cannot be issued (TRY_LATER).
//
// The report token is the network's zone's in root, network_dns_zone's
// report_token (plan-DNS-rua-token, plan-SMTP-net-token), which MG6 adds and
// the zone's system set mints (DNS-M8-01), so a zone made before that has none
// until its set is written again.
//
// Not loaded here yet, each answering closed where the gate reads it: a live
// alias's and a Team scope's zones (§12.7) and the names a reviewer allowed
// (ABUSE-M5-08). The limits are Quota.Limits', which the gate reads beside
// this.
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
	// network_dns_zone is MG5's and its report_token MG6's
	if server.SidecarMg5Linked() && server.SidecarMg6Linked() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT report_token FROM network_dns_zone WHERE root = $1 AND network_id = $2`,
				dnsLabelRoot(root),
				networkId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					var token *string
					server.Raise(result.Scan(&token))
					if token != nil {
						facts.ReportToken = *token
					}
				}
			})
		})
	}
	return facts, nil
}
