// Claim selection uses the original provider contribution, not the network's
// current wallet or the informational representative of an aggregated leaf.
package controller

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/startifact"
)

// A missing contribution is held, including excluded/zero-work providers that
// happen to share a coldkey with a contributing provider. No wallet fallback.
func snPoolClaimOwnedLeaf(args *SnPoolClaimArgs, clientSession *session.ClientSession, cfg *StConfig, epoch *model.StEpoch, record *model.StPayoutArtifact) (*model.StPayoutLeaf, error) {
	ctx := clientSession.Ctx
	leaves := model.GetStPayoutLeaves(ctx, cfg.DeploymentKey(), epoch.Epoch, cfg.NoId)
	if clientSession.ByJwt.ClientId == nil {
		// This explicit compatibility read exposes only a committed proof for
		// the requested original coldkey. It asserts no historical ownership.
		if args.LegacyColdkey == "" || record != nil || len(leaves) == 0 {
			return nil, nil
		}
		coldkey, err := ss58.DecodeWithPrefix(args.LegacyColdkey, ss58.BittensorPrefix)
		if err != nil {
			return nil, errors.New("400 legacy original coldkey is not an ss58 address")
		}
		for _, leaf := range leaves {
			if leaf.ClientId != nil {
				return nil, nil
			}
		}
		for _, leaf := range leaves {
			if leaf.Coldkey == coldkey {
				return leaf, nil
			}
		}
		return nil, nil
	}
	if args.LegacyColdkey != "" || record == nil {
		return nil, nil
	}
	store, available := server.LoadBlobStore()
	if !available {
		return nil, snPoolClaimUnavailable("payout artifact store unavailable", nil)
	}
	artifact, _, err := startifact.Read(ctx, store, record.ContentHash)
	if err != nil {
		return nil, snPoolClaimUnavailable("payout artifact integrity failure", err)
	}
	if artifact.Epoch != epoch.Epoch || artifact.NoID != cfg.NoId || artifact.PayoutRoot != record.PayoutRoot || artifact.DeploymentID != cfg.DeploymentId || artifact.ChainID != cfg.ChainId || artifact.Coordinator != cfg.ContractAddress || artifact.SettlementVault != cfg.SettlementVault || artifact.Netuid != uint16(cfg.Netuid) || !strings.EqualFold(artifact.GenesisHash, fmt.Sprintf("0x%x", cfg.GenesisHash)) {
		return nil, snPoolClaimUnavailable("payout artifact differs from its original claim scope", nil)
	}
	for _, provider := range artifact.Providers {
		if provider.ClientID != stId16(*clientSession.ByJwt.ClientId) || provider.NetworkID != stId16(clientSession.ByJwt.NetworkId) || !provider.Eligible || provider.HeadExcluded || provider.UsageBytes == 0 || provider.ReliabilityPPM == 0 || provider.Coldkey == ([32]byte{}) {
			continue
		}
		for _, leaf := range leaves {
			if leaf.Coldkey == provider.Coldkey {
				return leaf, nil
			}
		}
	}
	return nil, nil
}

// A retained artifact or leaf set the claim cannot use is the operator's
// failure, not the caller's. The cause, which can name the store's paths or
// endpoint, stays in the server log; the caller gets the class, as a 503.
func snPoolClaimUnavailable(class string, cause error) error {
	glog.Infof("[sn]pool claim refused: %s: %v\n", class, cause)
	return fmt.Errorf("%d %s", http.StatusServiceUnavailable, class)
}
