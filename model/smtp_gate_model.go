package model

import (
	"context"
	"time"

	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// What smtpedge's production send gate loads of one network, off every
// preamble's path (SMTP-M9-20; §14.5, §14.7, §7.6, §12.7): the block's gate
// (server/smtpedgehost/gate.go) calls this when a preamble missed its memory
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
// until its set is written again. The custom domains' facts, the verified
// domains, their withhold time and the strict ones, are
// smtp_custom_domain_model.go's (SMTP-M9-34).
//
// The SMTP host adds its signed Team scope labels from its watched projection
// and its limits from Quota.Limits. A cold projection keeps the gate unavailable.
func GetSmtpGateFacts(ctx context.Context, networkId server.Id, root string) (*smtpedgeservice.SmtpGateFacts, error) {
	return GetSmtpGateFactsWithSettings(ctx, networkId, root, DefaultSmtpCustomDomainSettings())
}

// GetSmtpGateFacts with the custom domains' settings.
func GetSmtpGateFactsWithSettings(ctx context.Context, networkId server.Id, root string, settings *SmtpCustomDomainSettings) (*smtpedgeservice.SmtpGateFacts, error) {
	root = dnsLabelRoot(root)
	label, aliases := getNetworkLabelAndLiveAliases(ctx, networkId, root)
	facts := &smtpedgeservice.SmtpGateFacts{
		Label:              label,
		Aliases:            aliases,
		ReviewAllowedNames: GetNetworkScreenAllowedNames(ctx, networkId),
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
	fillSmtpGateCustomDomainFacts(ctx, networkId, facts, settings)
	// network_dns_zone is MG5's and its report_token MG6's
	if server.SidecarMg5Linked() && server.SidecarMg6Linked() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT report_token FROM network_dns_zone WHERE root = $1 AND network_id = $2`,
				root,
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
