package controller

// The signed price of demand deposits (sn/demand/pricesheet). The sheet at
// pricesheet.URL sets the price; the policy document and its hash never change, and
// are the baseline wherever no signed entry is in force. Only a deployment with a
// pinned signer fetches anything, and only the deposit task fetches, once per
// attempt. Everything else reads the last verified copy.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/demand/pricesheet"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// stDepositPriceRetryDelay spaces the attempts of a deposit whose price is unknown.
const stDepositPriceRetryDelay = 5 * time.Minute

// stDemandSheetStore keeps a deployment's last verified price sheet in the database.
// A database failure is an error, which the source reports as an unknown price.
type stDemandSheetStore struct {
	deploymentKey model.StDeploymentKey
}

func (self *stDemandSheetStore) Load(ctx context.Context) (raw []byte, err error) {
	defer stDemandStoreRecover("price sheet load", &err)
	return model.GetStDemandPriceSheet(ctx, self.deploymentKey), nil
}

func (self *stDemandSheetStore) Save(ctx context.Context, raw []byte) (err error) {
	defer stDemandStoreRecover("price sheet save", &err)
	model.SaveStDemandPriceSheet(ctx, self.deploymentKey, raw)
	return nil
}

// The seams tests replace: the pinned signers, the published address and the store.
var (
	stDemandSheetSigner = pricesheet.SignerFor
	stDemandSheetUrl    = pricesheet.URL
	stDemandSheetStorer = func(cfg *StConfig) pricesheet.Store {
		return &stDemandSheetStore{deploymentKey: cfg.DeploymentKey()}
	}
)

func stDemandSheetSignerFor(cfg *StConfig) (common.Address, bool) {
	if cfg == nil || cfg.Netuid == 0 || cfg.Netuid > 65535 {
		return common.Address{}, false
	}
	return stDemandSheetSigner(cfg.ChainId, uint16(cfg.Netuid))
}

// stDepositEffectivePolicy is the deposit policy in force for an epoch, from the
// published sheet, fetched here once for this attempt. A deployment with no pinned
// signer fetches nothing and keeps the policy baseline. When the published file
// cannot be taken, the last verified sheet is used. A stored copy that cannot be read
// or no longer verifies does not stop a published file that verifies, which is then
// priced from as after a lost store (pricesheet.Source.Refresh). When no verified
// sheet is held and none could be fetched, the price is unknown: the returned outcome
// stops the deposit, which sends nothing and retries.
func stDepositEffectivePolicy(ctx context.Context, cfg *StConfig, epoch uint64) (protocol.DepositPolicy, *StPublishOutcome) {
	base := stDepositPolicy(cfg)
	signer, pinned := stDemandSheetSignerFor(cfg)
	if !pinned {
		return base, nil
	}
	source := pricesheet.NewSource(stDemandSheetUrl, cfg.ChainId, uint16(cfg.Netuid), signer, stDemandSheetStorer(cfg))
	sheet, problem := source.Refresh(ctx)
	if problem != nil {
		if sheet == nil {
			reason := fmt.Sprintf("the price is unknown: %s", problem)
			glog.Errorf("[st]epoch %d DEMAND DEPOSIT PRICE UNKNOWN, nothing is sent: %s\n", epoch, problem)
			return base, &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: reason, Retry: true, RetryAt: server.NowUtc().Add(stDepositPriceRetryDelay)}
		}
		// Either the published file was not taken and this is the last verified sheet,
		// or it was and the stored copy could not be used.
		glog.Errorf("[st]epoch %d DEMAND DEPOSIT PRICE SHEET PROBLEM, pricing from the verified sheet held: %s\n", epoch, problem)
	}
	return pricesheet.Effective(sheet, epoch, base), nil
}

// stDemandStoredSheet is a pinned deployment's last verified sheet, verified again
// here. It fetches nothing. Nil with an error means the price is unknown.
func stDemandStoredSheet(ctx context.Context, cfg *StConfig) (*pricesheet.Sheet, error) {
	signer, pinned := stDemandSheetSignerFor(cfg)
	if !pinned {
		return nil, nil
	}
	raw, err := stDemandSheetStorer(cfg).Load(ctx)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("st: no price sheet has been verified yet")
	}
	sheet, err := pricesheet.Parse(raw)
	if err != nil {
		return nil, err
	}
	if err := sheet.Verify(cfg.ChainId, uint16(cfg.Netuid), signer); err != nil {
		return nil, err
	}
	return sheet, nil
}

// stDepositStoredPolicy is stDepositEffectivePolicy from the last verified sheet,
// for readers that must not fetch.
func stDepositStoredPolicy(ctx context.Context, cfg *StConfig, epoch uint64) (protocol.DepositPolicy, *StPublishOutcome) {
	base := stDepositPolicy(cfg)
	if _, pinned := stDemandSheetSignerFor(cfg); !pinned {
		return base, nil
	}
	sheet, err := stDemandStoredSheet(ctx, cfg)
	if err != nil {
		return base, &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: fmt.Sprintf("the price is unknown: %s", err), Retry: true, RetryAt: server.NowUtc().Add(stDepositPriceRetryDelay)}
	}
	return pricesheet.Effective(sheet, epoch, base), nil
}
