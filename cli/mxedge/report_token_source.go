package main

// The platform listener's report tokens (plan-SMTP-net-token, plan-DNS-rua-token, QD-157; §14.6, §14.8, §12.7): the
// report intake's Tokens, which tie a report at <token>@<report host> to the network whose zone holds that token, by
// network_dns_zone's unique index on report_token, read over the block's own Postgres pool, as §14.6 has mxedge's
// platform listener find a token's network and as the intake's label map reads dns_label. Each of a network's zones
// draws a token of its own with its system set and keeps it for the network's life, and the zone's _dmarc and
// _smtp._tls name it in their rua (§12.7), so a token names one network.
//
// The column is MG6's, on MG5's table, so the block resolves tokens only in a binary that links MG6
// (resolveBlockReportTokens), and its readiness then waits on MG6 beside the intake's own groups (reportTokenGroups). A
// binary that does not link MG6 draws no token and publishes none in a rua, and the block says so at start and leaves
// the intake's Tokens empty, so a report at a token is dropped unread, as the intake's empty seam has it.
//
// A token no zone holds is no network, and the intake drops its report unread. A read that failed, the pool's or one a
// context ended under, the intake's StoreTimeout or the block's drain, is an error, which fails the report: the
// listener answers 451 and the reporter sends it again. Nothing here logs a token or puts one in an error, since a
// token is a recipient's local part (I-13). Safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// The binary does not link MG6, whose network_dns_zone.report_token a report token is.
var errReportTokenUnlinked = errors.New("this binary does not link MG6, whose network_dns_zone.report_token a report token is")

// network_dns_zone's index on report_token as the intake reads it, over the block's Postgres pool.
type reportTokenSource struct{}

var _ smtpedgeservice.SmtpNetTokenSource = (*reportTokenSource)(nil)

// See smtpedgeservice.SmtpNetTokenSource. A binary that does not link MG6 answers errReportTokenUnlinked, never no
// network, since it has no column to say no from.
func (self *reportTokenSource) NetworkForReportToken(ctx context.Context, token string) (networkId sidecar.Id, ok bool, returnErr error) {
	if !server.SidecarMg6Linked() {
		return sidecar.Id{}, false, errReportTokenUnlinked
	}
	defer func() {
		// the model raises a failed read, a context that ended under it included, and every raise reaches the intake
		// as an error, as the report store's do: the listener calls the intake under no server handler, so a raise
		// that left here would end the connection with go-smtp's 421 where the intake answers 451
		if r := recover(); r != nil {
			networkId, ok = sidecar.Id{}, false
			if err, isErr := r.(error); isErr {
				returnErr = fmt.Errorf("report intake: the report token index: %w", err)
			} else {
				returnErr = fmt.Errorf("report intake: the report token index: %v", r)
			}
		}
	}()
	resolvedId, found := model.GetDnsNetworkForReportToken(ctx, token)
	if !found {
		return sidecar.Id{}, false, nil
	}
	return sidecar.Id(resolvedId), true, nil
}

// The pending group the tokens read beside the intake's: MG6's network_dns_zone.report_token, a column of MG5's table,
// whose group the intake's own hold (reportIntakeGroups).
func reportTokenGroups() []*sidecarhost.RunGroup {
	return []*sidecarhost.RunGroup{
		{Name: "MG6", Linked: server.SidecarMg6Linked},
	}
}

// Ties each report at a network's token to that network from now on, through tokens: the intake is made again with
// them, its other seams as they were. An error leaves the intake as it was. Called before the listener has the intake,
// as sealThrough and attributeThrough are.
func (self *platformReportIntake) resolveTokensThrough(tokens smtpedgeservice.SmtpNetTokenSource) error {
	if tokens == nil {
		return errors.New("report intake: no token source")
	}
	deps := *self.deps
	deps.Tokens = tokens
	intake, err := smtpedgeservice.NewSmtpReportIntake(self.host, self.store, &deps, self.settings)
	if err != nil {
		return err
	}
	self.intake = intake
	self.deps = &deps
	return nil
}

// The block's tokens: network_dns_zone's index over its own Postgres, in a binary that links MG6. An error says why the
// block ties no report to a network by its token, and leaves the intake as it was.
func resolveBlockReportTokens(intake *platformReportIntake) error {
	if !server.SidecarMg6Linked() {
		return errReportTokenUnlinked
	}
	return intake.resolveTokensThrough(&reportTokenSource{})
}
