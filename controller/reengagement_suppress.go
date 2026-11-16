// SPDX-License-Identifier: MPL-2.0

package controller

// An operator's marketing suppression of one network or a list of them
// (bringyourctl reengagement suppress). Support runs it for an account holder
// who said they are in a sanctioned country (--reason=sanctions; factury
// support/RUN-MAIN.md §9; owner, 2026-11-13). --reason=automated_signup is
// for a sign-up cohort that automation made, if the owner decides to suppress
// one: on 2026-11-13 the owner chose to keep mailing every account, bots
// included.
//
// The suppression stops every marketing mail to those networks: the
// onboarding sequence in flight exits at its next step, re-engagement opens no
// episode and ends any open one, and the product-updates sync takes the
// admin's logins off the Brevo lists. Its reason is one no connection lifts
// (reengagement.OperatorSuppressions).
// Service and account mail (receipts, security notices, trial and Pro-ending
// reminders) never reads a suppression and still goes.
//
// The dry run only reads. Its digest binds the reason and the requested
// networks, sorted and once each, not their state: the apply reads each batch
// again in the batch's transaction and writes only what still needs it. So
// applying the same digest again, after a partial apply or a bounce recorded
// in between, writes the rest and nothing twice. Another list or reason has
// another digest, and the apply refuses it before it reads or writes anything.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/reengagement"
	"github.com/urnetwork/server/session"
)

const marketingSuppressionDigestVersion = "urnetwork reengagement suppress v1"

var ErrMarketingSuppressionDigestMismatch = errors.New("reengagement suppress: --expect does not match the digest of these networks and this reason; nothing was written; run the dry run again")

// MarketingSuppressionSettings tune an operator's suppression batch.
type MarketingSuppressionSettings struct {
	// networks read in one query and written in one transaction: bounds each
	// statement's array and each transaction's row locks and queued syncs.
	// Non-positive is refused.
	BatchSize int
	// the most network ids one request may name, repeats included: bounds the
	// request's memory and the ids file the command reads (MaxNetworkIdsBytes).
	// Non-positive is refused.
	MaxNetworkIds int
	// paces the Brevo list syncs the written networks queue, across batches
	Mirror *ProductUpdatesMirrorSettings
}

func DefaultMarketingSuppressionSettings() *MarketingSuppressionSettings {
	return &MarketingSuppressionSettings{
		BatchSize: 500,
		// a sign-up surge of bots is thousands of networks; this leaves
		// two orders of magnitude
		MaxNetworkIds: 1000000,
		Mirror:        DefaultProductUpdatesMirrorSettings(),
	}
}

// MaxNetworkIdsBytes bounds an ids file: MaxNetworkIds lines of a network id
// with room for a comment or surrounding space.
func (self *MarketingSuppressionSettings) MaxNetworkIdsBytes() int {
	return self.MaxNetworkIds * 128
}

// MarketingSuppressionCounts are the requested networks by what a suppression
// does to them.
type MarketingSuppressionCounts struct {
	// ids that name a network
	Found int `json:"found"`
	// ids that name no network: nothing to write
	Missing int `json:"missing"`
	// suppressed already for a reason no connection lifts: left as they are
	AlreadySuppressed int `json:"already_suppressed"`
	// not suppressed, or suppressed for a reason a connection lifts: written
	ToSuppress int `json:"to_suppress"`
	// of ToSuppress, those suppressed for a reason a connection lifts, which
	// the operator's reason replaces
	ReplacesLiftable int `json:"replaces_liftable"`
}

func (self *MarketingSuppressionCounts) add(other MarketingSuppressionCounts) {
	self.Found += other.Found
	self.Missing += other.Missing
	self.AlreadySuppressed += other.AlreadySuppressed
	self.ToSuppress += other.ToSuppress
	self.ReplacesLiftable += other.ReplacesLiftable
}

// MarketingSuppressionPlan is a suppression batch as the dry run read it.
type MarketingSuppressionPlan struct {
	Reason string `json:"reason"`
	// the requested networks, sorted, once each
	NetworkIds []server.Id `json:"-"`
	// how many requested ids repeated an earlier one
	Duplicates int                        `json:"duplicates"`
	Counts     MarketingSuppressionCounts `json:"counts"`
	Digest     string                     `json:"digest"`
}

// MarketingSuppressionResult is what an apply did.
type MarketingSuppressionResult struct {
	Reason     string `json:"reason"`
	Requested  int    `json:"requested"`
	Duplicates int    `json:"duplicates"`
	// as each batch's transaction read them before its write
	Counts MarketingSuppressionCounts `json:"counts"`
	// networks written, each with the product-updates sync of its admin
	// queued in the same transaction
	Suppressed int `json:"suppressed"`
	// committed transactions
	Batches int `json:"batches"`
}

// marketingSuppressionRequest is a batch's reason and its networks, sorted and
// once each, with the digest that binds them.
type marketingSuppressionRequest struct {
	reason     string
	networkIds []server.Id
	duplicates int
	digest     string
}

func newMarketingSuppressionRequest(networkIds []server.Id, reason string, settings *MarketingSuppressionSettings) (*marketingSuppressionRequest, error) {
	if !slices.Contains(reengagement.OperatorSuppressions(), reason) {
		return nil, fmt.Errorf("reengagement suppress: --reason must be one of %s, not %q", strings.Join(reengagement.OperatorSuppressions(), ", "), reason)
	}
	if settings.BatchSize <= 0 || settings.MaxNetworkIds <= 0 || settings.Mirror == nil {
		return nil, fmt.Errorf("reengagement suppress: batch size %d and id limit %d must be positive, with mirror settings", settings.BatchSize, settings.MaxNetworkIds)
	}
	if len(networkIds) == 0 {
		return nil, errors.New("reengagement suppress: no network ids")
	}
	if settings.MaxNetworkIds < len(networkIds) {
		return nil, fmt.Errorf("reengagement suppress: %d network ids, more than the %d one request may name; split the list", len(networkIds), settings.MaxNetworkIds)
	}
	unique := slices.Clone(networkIds)
	slices.SortFunc(unique, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	unique = slices.Compact(unique)
	lines := []string{
		marketingSuppressionDigestVersion,
		fmt.Sprintf("reason %s", reason),
	}
	for _, networkId := range unique {
		lines = append(lines, fmt.Sprintf("network %s", networkId))
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	return &marketingSuppressionRequest{
		reason:     reason,
		networkIds: unique,
		duplicates: len(networkIds) - len(unique),
		digest:     hex.EncodeToString(sum[:]),
	}, nil
}

// batches splits the networks into the settings' batch size, in order.
func (self *marketingSuppressionRequest) batches(settings *MarketingSuppressionSettings) [][]server.Id {
	return slices.Collect(slices.Chunk(self.networkIds, settings.BatchSize))
}

// countMarketingSuppression counts one batch from the states read for it (an
// id without a state names no network).
func countMarketingSuppression(networkIds []server.Id, states map[server.Id]*model.MarketingSuppressionState) (counts MarketingSuppressionCounts) {
	for _, networkId := range networkIds {
		state, ok := states[networkId]
		switch {
		case !ok:
			counts.Missing += 1
		case state.SuppressedAt == nil:
			counts.Found += 1
			counts.ToSuppress += 1
		case reengagement.LiftableSuppression(state.SuppressedReason):
			counts.Found += 1
			counts.ToSuppress += 1
			counts.ReplacesLiftable += 1
		default:
			counts.Found += 1
			counts.AlreadySuppressed += 1
		}
	}
	return
}

// PlanNetworksMarketingSuppression is the dry run: it reads the networks, one
// query a batch, and counts what an apply would do. It writes nothing.
func PlanNetworksMarketingSuppression(
	ctx context.Context,
	networkIds []server.Id,
	reason string,
	settings *MarketingSuppressionSettings,
) (*MarketingSuppressionPlan, error) {
	request, err := newMarketingSuppressionRequest(networkIds, reason, settings)
	if err != nil {
		return nil, err
	}
	plan := &MarketingSuppressionPlan{
		Reason:     request.reason,
		NetworkIds: request.networkIds,
		Duplicates: request.duplicates,
		Digest:     request.digest,
	}
	for _, batch := range request.batches(settings) {
		plan.Counts.add(countMarketingSuppression(batch, model.GetNetworksMarketingSuppression(ctx, batch)))
	}
	return plan, nil
}

// SuppressNetworksMarketing applies the batch whose dry run printed `expect`:
// one transaction a batch reads the batch's networks, records the suppression
// where model.SuppressNetworksMarketingInTx writes it, and queues the
// product-updates sync of each written network's admin, paced across the
// whole run (settings.Mirror) so that the Brevo list calls stay inside the
// API limit. The suppression itself takes effect at the commit: the campaigns
// read it at their next step. A digest of another list or reason is refused
// before anything is read. An error after some batches committed returns the
// result so far; those batches stay written, and the same command finishes
// the rest.
func SuppressNetworksMarketing(
	ctx context.Context,
	networkIds []server.Id,
	reason string,
	expect string,
	settings *MarketingSuppressionSettings,
) (result *MarketingSuppressionResult, returnErr error) {
	request, err := newMarketingSuppressionRequest(networkIds, reason, settings)
	if err != nil {
		return nil, err
	}
	if request.digest != expect {
		return nil, ErrMarketingSuppressionDigestMismatch
	}
	result = &MarketingSuppressionResult{
		Reason:     request.reason,
		Requested:  len(request.networkIds),
		Duplicates: request.duplicates,
	}
	now := server.NowUtc()
	pacer := newProductUpdatesSyncPacer(now, settings.Mirror)
	server.HandleError(func() {
		for _, batch := range request.batches(settings) {
			var counts MarketingSuppressionCounts
			var written []*model.ProductUpdatesNetworkAdmin
			server.Tx(ctx, func(tx server.PgTx) {
				counts = countMarketingSuppression(batch, model.GetNetworksMarketingSuppressionInTx(tx, ctx, batch))
				written = model.SuppressNetworksMarketingInTx(tx, ctx, batch, request.reason, now)
				// the owner scheduleSyncProductUpdatesForNetworkInTx uses
				clientSession := session.NewLocalClientSession(ctx, "", nil)
				defer clientSession.Cancel()
				// a rerun of this callback takes new slots: the syncs only
				// spread further
				for _, admin := range written {
					scheduleSyncProductUpdatesForUserAt(clientSession, admin.AdminUserId, pacer.next(), tx)
				}
			})
			result.Counts.add(counts)
			result.Suppressed += len(written)
			result.Batches += 1
		}
	}, func(err error) {
		returnErr = fmt.Errorf("reengagement suppress: stopped after %d batches wrote %d networks; they stay written, and the same command writes the rest: %w", result.Batches, result.Suppressed, err)
	})
	return result, returnErr
}
