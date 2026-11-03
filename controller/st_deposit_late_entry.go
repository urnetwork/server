package controller

// A price published after its epoch's deposit chain concluded at zero price. An
// epoch's chain ends at its first concluding deposit row, and at zero price that is
// the one skipped row the attempt has always written, while validators audit the epoch
// at the new price as soon as they fetch the sheet. So, on a deployment with a pinned
// signer, while the current epoch's only concluding deposit row is that zero-price
// skip, the sync turn keeps a check of the sheet scheduled about every
// StDepositLateEntryInterval (taskworker/work StDepositLateEntry), and once a verified
// sheet prices the epoch, the check seeds its deposit chain again. That chain pays like
// any other, under the attempt's own deadlines and retries. Nothing here sends.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server/model"
)

// StDepositLateEntryInterval spaces the checks of the sheet of an epoch skipped at zero
// price, and the seeding of its deposit chain again while that chain fails to land.
const StDepositLateEntryInterval = 15 * time.Minute

// stDepositZeroPriceSkip begins the reason of the zero-price skip row
// (StDepositSizing.String), which tells it from every other skipped row.
const stDepositZeroPriceSkip = "price is zero: "

// stDepositLateEntryOpen reports whether an epoch's deposit rows leave a late price to
// pay: a zero-price skip, and no deposit row that ends the chain otherwise, confirmed,
// refused or skipped for any other reason (already deposited, nothing owed at the new
// price, or a passed epoch).
func stDepositLateEntryOpen(publishes []*model.StPublish) bool {
	zeroPrice := false
	for _, publish := range publishes {
		if publish.Kind != model.StPublishKindDeposit {
			continue
		}
		switch publish.Status {
		case model.StPublishStatusConfirmed, model.StPublishStatusRefused:
			return false
		case model.StPublishStatusSkipped:
			if publish.Error == nil || !strings.HasPrefix(*publish.Error, stDepositZeroPriceSkip) {
				return false
			}
			zeroPrice = true
		}
	}
	return zeroPrice
}

// StDepositLateEntryWatched reports whether the sync turn keeps a check of an epoch's
// sheet scheduled. It reads the database only, and fetches nothing.
func StDepositLateEntryWatched(ctx context.Context, epoch uint64) bool {
	cfg, _, err := stRequire()
	if err != nil {
		return false
	}
	if _, pinned := stDemandSheetSignerFor(cfg); !pinned {
		return false
	}
	return stDepositLateEntryOpen(model.GetStPublishes(ctx, cfg.DeploymentKey(), epoch))
}

// StDepositLateEntryDue checks the sheet again for an epoch skipped at zero price and
// reports whether its deposit chain is to be seeded: its rows still leave it open, it
// is still the current epoch and its deposit deadline has not been reached, and the
// verified sheet now prices it. A fetch that fails is an error, and the next check
// tries again.
func StDepositLateEntryDue(ctx context.Context, epoch uint64) (bool, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return false, err
	}
	if _, pinned := stDemandSheetSignerFor(cfg); !pinned {
		return false, nil
	}
	if !stDepositLateEntryOpen(model.GetStPublishes(ctx, cfg.DeploymentKey(), epoch)) {
		return false, nil
	}
	state, err := client.Epoch(ctx)
	if err != nil {
		return false, errors.New(stScrubEndpoints(err.Error()))
	}
	switch {
	case stDepositEpochPassedAt(state, epoch):
		return false, nil
	case state.Epoch != epoch || state.PendingEpoch != epoch:
		// A view behind the chain decides nothing; the next check reads again.
		return false, fmt.Errorf("the chain view is at epoch %d (pending %d), behind epoch %d", state.Epoch, state.PendingEpoch, epoch)
	}
	// The deposit's deadline is the epoch's last block; nothing is seeded once the
	// finalized head has reached it.
	if end := state.EpochStartBlock + state.TEpochBlocks; end <= state.HeadBlock+1 {
		return false, nil
	}
	policy, unknown := stDepositEffectivePolicy(ctx, cfg, epoch)
	if unknown != nil {
		return false, errors.New(stScrubEndpoints(unknown.Reason))
	}
	return !policy.IsZeroPrice(), nil
}

// Testing_SetStDemandSheet points the price sheet's published address and pinned
// signer at a test's own, for the tests of other packages, and returns what restores
// them. Only the deployment of chainId and netuid is pinned.
func Testing_SetStDemandSheet(url string, signer common.Address, chainId uint64, netuid uint16) (restore func()) {
	previousUrl, previousSigner := stDemandSheetUrl, stDemandSheetSigner
	stDemandSheetUrl = url
	stDemandSheetSigner = func(chain uint64, sn uint16) (common.Address, bool) {
		return signer, chain == chainId && sn == netuid
	}
	return func() { stDemandSheetUrl, stDemandSheetSigner = previousUrl, previousSigner }
}
