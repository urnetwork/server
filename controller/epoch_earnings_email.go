// epoch_earnings_email is the one earnings email per finalized epoch. It
// replaces the USDC "you got paid" payout mail: points first, the network's
// share of the operator pool, its leaderboard rank, its Top 200 standing, and
// — only once a Bittensor wallet is attached — the SN25α that is now
// claimable. The claim itself happens between the app and the vault.
package controller

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// pace sends so one finalized epoch does not burst past the SES send rate
const stEpochEarningsSendInterval = 80 * time.Millisecond
const stEpochEarningsSendTimeout = 2 * time.Hour

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
	// coldkey(s); nil when the pool total could not be read.
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

// HasUnclaimed: a wallet is attached and the network holds a leaf this epoch.
func (self *EpochEarningsTemplate) HasUnclaimed() bool {
	return self.HasWallet && 0 < self.ShareBps
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
func stMarkEpochFinalized(ctx context.Context, epoch uint64) {
	prior := model.GetStEpoch(ctx, epoch)
	model.SetStEpochStatus(ctx, epoch, model.StEpochStatusFinalized)
	if prior != nil && prior.Status == model.StEpochStatusFinalized {
		return
	}
	StNotifyEpochEarnings(ctx, epoch)
}

// StNotifyEpochEarnings sends the epoch earnings email to every network that
// earned points inside the epoch window or holds a payout leaf in the epoch.
// It runs once per epoch (ClaimStEpochNotification) in the background.
func StNotifyEpochEarnings(ctx context.Context, epoch uint64) {
	if !model.ClaimStEpochNotification(ctx, epoch) {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				glog.Errorf("[st]epoch %d earnings email failed: %v\n", epoch, r)
			}
		}()
		sendCtx, cancel := context.WithTimeout(context.Background(), stEpochEarningsSendTimeout)
		defer cancel()
		sent, skipped := stSendEpochEarnings(sendCtx, epoch, GetAWSMessageSender())
		glog.Infof("[st]epoch %d earnings email: sent %d, skipped %d\n", epoch, sent, skipped)
	}()
}

// stSendEpochEarnings builds and sends one email per recipient network.
func stSendEpochEarnings(ctx context.Context, epoch uint64, sender MessageSender) (sent int, skipped int) {
	row := model.GetStEpoch(ctx, epoch)
	if row == nil {
		return 0, 0
	}
	for networkId, template := range stEpochEarningsTemplates(ctx, epoch, row) {
		userAuth, err := model.GetUserAuth(ctx, networkId)
		if err != nil || userAuth == "" {
			skipped += 1
			continue
		}
		if err := sender.SendAccountMessageTemplate(userAuth, template); err != nil {
			glog.Warningf("[st]epoch %d earnings email to network %s failed: %s\n", epoch, networkId, err)
			skipped += 1
		} else {
			sent += 1
		}
		select {
		case <-ctx.Done():
			return sent, skipped
		case <-time.After(stEpochEarningsSendInterval):
		}
	}
	return sent, skipped
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
// the epoch window, share from the committed leaves, rank from the
// leaderboard ranking, Top 200 from the head estimate and the binding
// registry, alpha from the pool total × share.
func stEpochEarningsTemplates(ctx context.Context, epoch uint64, row *model.StEpoch) map[server.Id]*EpochEarningsTemplate {
	start, end := snEpochWindow(ctx, row)
	points := model.GetNetworkNanoPointsInWindow(ctx, start, end)
	noId := uint64(0)
	if cfg := stConfig(); cfg != nil {
		noId = cfg.NoId
	}
	shares := model.GetStPayoutNetworkShares(ctx, epoch, noId)
	rankings, err := model.GetNetworkLeaderboardRankings(ctx)
	if err != nil {
		rankings = map[server.Id]model.NetworkRanking{}
	}
	head := snHeadRankingSafe(ctx)
	poolTotal := stEpochPoolTotal(ctx, epoch, noId)

	recipients := map[server.Id]bool{}
	for networkId := range points {
		recipients[networkId] = true
	}
	for networkId, share := range shares {
		if 0 < share {
			recipients[networkId] = true
		}
	}
	templates := map[server.Id]*EpochEarningsTemplate{}
	for networkId := range recipients {
		template := &EpochEarningsTemplate{
			Epoch:     epoch,
			Points:    float64(points[networkId]) / snNanoPointsPerPoint,
			ShareBps:  shares[networkId],
			Rank:      rankings[networkId].LeaderboardRank,
			Total:     len(rankings),
			HasWallet: snNetworkHasWallet(ctx, networkId),
			EpochEnd:  end,
		}
		if poolTotal != nil && 0 < template.ShareBps {
			unclaimed := new(big.Int).Mul(poolTotal, big.NewInt(int64(template.ShareBps)))
			template.UnclaimedRao = unclaimed.Div(unclaimed, big.NewInt(10_000))
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
