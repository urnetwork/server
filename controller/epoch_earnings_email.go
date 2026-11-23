// epoch_earnings_email is the one earnings email per finalized epoch. It
// replaces the USDC "you got paid" payout mail: points first, the network's
// share of the operator pool, its leaderboard rank, its Top 200 standing, and
// — only once a Bittensor wallet is attached — the SN25α that is now
// claimable. The claim itself happens between the app and the vault.
//
// A personal network's email goes to its admin's sign-in. A Team's admin is
// an owner of record that never signs in (D-TEAM-2), so a Team's email goes to
// its billing recipients, its Owners and Billing admins, one email per address
// (QD-1222), as its failed-payment and missing-wallet mail do (QD-1219).
package controller

import (
	"context"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Tunables of the epoch earnings email (QD-702), from
// defaultStEpochEarningsSettings.
type stEpochEarningsSettings struct {
	// The wait after each email: 80 ms, so one finalized epoch does not burst
	// past the SES send rate. Each address is one email, so each of a Team's
	// billing recipients waits its own interval. Non-positive sends without
	// waiting.
	SendInterval time.Duration
	// How long one epoch's emails may take to send: 2 hours. The emails not
	// sent by then are not sent, since the epoch's notification is claimed
	// once. Non-positive sets no bound.
	SendTimeout time.Duration
	// The most networks whose recipients one read finds: 1,000, so each read's
	// arrays stay small however many networks earned in the epoch.
	// Non-positive reads every network's at once.
	RecipientReadNetworkCount int
}

func defaultStEpochEarningsSettings() *stEpochEarningsSettings {
	return &stEpochEarningsSettings{
		SendInterval:              80 * time.Millisecond,
		SendTimeout:               2 * time.Hour,
		RecipientReadNetworkCount: 1000,
	}
}

// EpochEarningsTemplate renders `subscription_epoch_earnings`.
type EpochEarningsTemplate struct {
	Epoch    uint64
	Points   float64
	ShareBps int
	Rank     int
	Total    int
	// Top200Eligible: the server estimate places the network inside the
	// cutoff; Top200Rank is that estimate. Top200Bound/Top200Uid: an active
	// head binding.
	Top200Eligible bool
	Top200Bound    bool
	Top200Uid      uint64
	Top200Rank     int
	HasWallet      bool
	// UnclaimedRao is this epoch's claimable alpha for the network's
	// coldkey(s): a v2 epoch's ledger amount, or a v1 epoch's pool total ×
	// share, nil when the pool total could not be read.
	UnclaimedRao *big.Int
	EpochEnd     time.Time
	BaseTemplate
}

func (self *EpochEarningsTemplate) Name() string {
	return "subscription_epoch_earnings"
}

func (self *EpochEarningsTemplate) Funcs(funcs texttemplate.FuncMap) {
	self.BaseTemplate.Funcs(funcs)
	funcs["PointsText"] = self.PointsText
	funcs["SharePercent"] = self.SharePercent
	funcs["RankText"] = self.RankText
	funcs["Top200Text"] = self.Top200Text
	funcs["AlphaText"] = self.AlphaText
	funcs["EpochEndText"] = self.EpochEndText
}

// PointsText prints points with up to two decimals and no trailing zeros.
func (self *EpochEarningsTemplate) PointsText() string {
	text := strconv.FormatFloat(self.Points, 'f', 2, 64)
	if strings.Contains(text, ".") {
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	}
	return text
}

// SharePercent is share_bps as a percentage of the current block.
func (self *EpochEarningsTemplate) SharePercent() string {
	return fmt.Sprintf("%.2f%%", float64(self.ShareBps)/100)
}

// RankText is "#R of T", or "unranked" for a network without ranked payouts.
func (self *EpochEarningsTemplate) RankText() string {
	if self.Rank <= 0 || self.Total <= 0 {
		return "unranked"
	}
	return fmt.Sprintf("#%d of %d", self.Rank, self.Total)
}

// Top200Text is the badge line: the bound status when a head binding is
// active, "you qualify" when the estimate is inside the cutoff, else "" (the
// templates omit the section).
func (self *EpochEarningsTemplate) Top200Text() string {
	if self.Top200Bound {
		if 0 < self.Top200Rank {
			return fmt.Sprintf("Top 200 · UID %d · rank #%d", self.Top200Uid, self.Top200Rank)
		}
		return fmt.Sprintf("Top 200 · UID %d", self.Top200Uid)
	}
	if self.Top200Eligible {
		return "Top 200 · you qualify"
	}
	return ""
}

// HasUnclaimed: a wallet is attached and the network holds a leaf, or a v2
// amount, this epoch.
func (self *EpochEarningsTemplate) HasUnclaimed() bool {
	return self.HasWallet && (0 < self.ShareBps || (self.UnclaimedRao != nil && 0 < self.UnclaimedRao.Sign()))
}

// AlphaText is the claimable amount as "3.2410 SN25α", or "" when the pool
// total was not readable.
func (self *EpochEarningsTemplate) AlphaText() string {
	if self.UnclaimedRao == nil || self.UnclaimedRao.Sign() <= 0 {
		return ""
	}
	alpha := new(big.Rat).SetFrac(self.UnclaimedRao, big.NewInt(1_000_000_000))
	return alpha.FloatString(4) + " SN25α"
}

func (self *EpochEarningsTemplate) EpochEndText() string {
	return self.EpochEnd.UTC().Format("Jan 2, 2006")
}

// stMarkEpochFinalized advances the mirror row to finalized and, on the
// actual transition, queues the epoch earnings email once.
func stMarkEpochFinalized(ctx context.Context, deploymentKey model.StDeploymentKey, epoch uint64) {
	prior := model.GetStEpoch(ctx, deploymentKey, epoch)
	model.SetStEpochStatus(ctx, deploymentKey, epoch, model.StEpochStatusFinalized)
	if prior != nil && prior.Status == model.StEpochStatusFinalized {
		return
	}
	// a v2 epoch notifies when its finalized root's amounts are recorded
	if v1, err := StV1EpochAllowed(ctx, epoch); err == nil && !v1 {
		return
	}
	// a new finalized epoch changes blocks-with-points and streaks
	TriggerRebuildPointsLeaderboard(ctx)
	StNotifyEpochEarnings(ctx, deploymentKey, epoch)
}

// StNotifyEpochEarnings sends the epoch earnings email to every network that
// earned points inside the epoch window or holds a payout leaf in the epoch.
// It runs once per epoch (ClaimStEpochNotification) in the background.
func StNotifyEpochEarnings(ctx context.Context, deploymentKey model.StDeploymentKey, epoch uint64) {
	if !model.ClaimStEpochNotification(ctx, deploymentKey, epoch) {
		return
	}
	settings := defaultStEpochEarningsSettings()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				glog.Errorf("[st]epoch %d earnings email failed: %v\n", epoch, r)
			}
		}()
		var sendCtx context.Context
		var cancel context.CancelFunc
		if 0 < settings.SendTimeout {
			sendCtx, cancel = context.WithTimeout(context.Background(), settings.SendTimeout)
		} else {
			sendCtx, cancel = context.WithCancel(context.Background())
		}
		defer cancel()
		sent, skipped := stSendEpochEarnings(sendCtx, deploymentKey, epoch, GetAWSMessageSender(), settings)
		glog.Infof("[st]epoch %d earnings email: sent %d, skipped %d\n", epoch, sent, skipped)
	}()
}

// stSendEpochEarnings sends the epoch's earnings email to each recipient
// network, in network id order, at the addresses stEpochEarningsRecipients
// reads for it: one email per address, each followed by the settings'
// SendInterval. sent counts the emails sent; skipped counts each network with
// no address and each email whose send failed.
func stSendEpochEarnings(
	ctx context.Context,
	deploymentKey model.StDeploymentKey,
	epoch uint64,
	sender MessageSender,
	settings *stEpochEarningsSettings,
) (sent int, skipped int) {
	row := model.GetStEpoch(ctx, deploymentKey, epoch)
	if row == nil {
		return 0, 0
	}
	templates := stEpochEarningsTemplates(ctx, deploymentKey, epoch, row)
	networkIds := slices.SortedFunc(maps.Keys(templates), func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	readNetworkCount := settings.RecipientReadNetworkCount
	if readNetworkCount <= 0 {
		readNetworkCount = max(len(networkIds), 1)
	}
	for readNetworkIds := range slices.Chunk(networkIds, readNetworkCount) {
		networkIdUserAuths := stEpochEarningsRecipients(ctx, readNetworkIds)
		for _, networkId := range readNetworkIds {
			userAuths := networkIdUserAuths[networkId]
			if len(userAuths) == 0 {
				skipped += 1
				continue
			}
			for i, userAuth := range userAuths {
				if err := sender.SendAccountMessageTemplate(userAuth, templates[networkId]); err != nil {
					glog.Warningf("[st]epoch %d earnings email %d of %d to network %s failed: %s\n", epoch, i+1, len(userAuths), networkId, err)
					skipped += 1
				} else {
					sent += 1
				}
				if 0 < settings.SendInterval {
					select {
					case <-ctx.Done():
						return sent, skipped
					case <-time.After(settings.SendInterval):
					}
				} else if ctx.Err() != nil {
					return sent, skipped
				}
			}
		}
	}
	return sent, skipped
}

// The addresses the earnings email of each of the networks goes to, read in
// one transaction. A Team's are its billing recipients
// (model.GetTeamBillingRecipientsInTx, QD-1222): its Owners and Billing admins
// at their sign-in email addresses, Owners first and within the bound on the
// people a Team's mail reaches (QD-1218), and none for a Team none of whose
// such members has one. An Admin holds no billing or payouts (§7.5) and is not
// told. A personal network's is its admin's sign-in, email or phone, as it
// always was (model.GetUserAuthsInTx). A network with none has no entry.
func stEpochEarningsRecipients(ctx context.Context, networkIds []server.Id) map[server.Id][]string {
	networkIdUserAuths := map[server.Id][]string{}
	server.Tx(ctx, func(tx server.PgTx) {
		teamNetworkIdRecipients := model.GetTeamBillingRecipientsInTx(ctx, tx, networkIds)
		personalNetworkIds := []server.Id{}
		for _, networkId := range networkIds {
			if recipients, team := teamNetworkIdRecipients[networkId]; team {
				if 0 < len(recipients) {
					networkIdUserAuths[networkId] = recipients
				}
			} else {
				personalNetworkIds = append(personalNetworkIds, networkId)
			}
		}
		for networkId, userAuth := range model.GetUserAuthsInTx(ctx, tx, personalNetworkIds) {
			if userAuth != "" {
				networkIdUserAuths[networkId] = []string{userAuth}
			}
		}
	})
	return networkIdUserAuths
}

// stEpochPoolTotal reads the finalized pool total for (epoch, noId), or nil
// when the chain is not reachable.
func stEpochPoolTotal(ctx context.Context, epoch uint64, noId uint64) *big.Int {
	_, client, err := stRequire()
	if err != nil {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, snChainCallTimeout)
	defer cancel()
	pool, err := client.PoolState(callCtx, epoch, noId)
	if err != nil || pool == nil || pool.PoolTotalRao == nil || pool.PoolTotalRao.Sign() <= 0 {
		return nil
	}
	return pool.PoolTotalRao
}

// stEpochEarningsTemplates assembles the per-network templates: points from
// the epoch window, share and alpha from stEpochNetworkEarnings, rank from the
// leaderboard ranking, Top 200 from the head estimate and the binding
// registry.
func stEpochEarningsTemplates(ctx context.Context, deploymentKey model.StDeploymentKey, epoch uint64, row *model.StEpoch) map[server.Id]*EpochEarningsTemplate {
	start, end := snEpochWindow(ctx, row)
	points := model.GetNetworkNanoPointsInWindow(ctx, start, end)
	noId := uint64(0)
	if cfg := stConfig(); cfg != nil {
		noId = cfg.NoId
	}
	shares, unclaimed := stEpochNetworkEarnings(ctx, deploymentKey, epoch, noId)
	rankings, err := model.GetNetworkLeaderboardRankings(ctx)
	if err != nil {
		rankings = map[server.Id]model.NetworkRanking{}
	}
	head := snHeadRankingSafe(ctx)

	recipients := map[server.Id]bool{}
	for networkId := range points {
		recipients[networkId] = true
	}
	for networkId, share := range shares {
		if 0 < share || unclaimed[networkId] != nil {
			recipients[networkId] = true
		}
	}
	templates := map[server.Id]*EpochEarningsTemplate{}
	for networkId := range recipients {
		template := &EpochEarningsTemplate{
			Epoch:        epoch,
			Points:       float64(points[networkId]) / snNanoPointsPerPoint,
			ShareBps:     shares[networkId],
			Rank:         rankings[networkId].LeaderboardRank,
			Total:        len(rankings),
			HasWallet:    snNetworkHasWallet(ctx, networkId),
			UnclaimedRao: unclaimed[networkId],
			EpochEnd:     end,
		}
		if head != nil {
			score := head.scores[networkId]
			rank := head.rankOf(score)
			if 0 < score && rank <= SnHeadCutoff {
				template.Top200Eligible = true
				template.Top200Rank = rank
			}
		}
		if bound := snHeadBoundState(ctx, networkId, false, nil); bound.bound {
			template.Top200Bound = true
			template.Top200Uid = bound.uid
		}
		templates[networkId] = template
	}
	return templates
}
