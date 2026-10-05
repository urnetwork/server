package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urfoundation/sn/nativefee"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The CLI selects only original proof inputs and a retained signed candidate.
// It cannot supply denomination, native semantics or executable authority.
func SettleNativeTransactionFee(ctx context.Context, intentId server.Id, request nativefee.Reference, transactionHash string, budget time.Duration) (*model.StTransactionNativeFeeSettlement, error) {
	// The protected public authority independently selects the deployment.
	// Constructing the ordinary StClient would load unrelated signing keys.
	return settleNativeTransactionFee(ctx, nil, intentId, request, transactionHash, budget)
}

// Settlement never calls an RPC, signer or broadcaster. Every unknown result
// leaves the original maximum liability and attempt count retained by model.
func (self *CoreStClient) SettleNativeTransactionFee(ctx context.Context, intentId server.Id, request nativefee.Reference, transactionHash string, budget time.Duration) (*model.StTransactionNativeFeeSettlement, error) {
	if self == nil || self.cfg == nil {
		return nil, errStNotConfigured
	}
	return settleNativeTransactionFee(ctx, self.cfg, intentId, request, transactionHash, budget)
}

func settleNativeTransactionFee(ctx context.Context, cfg *StConfig, intentId server.Id, request nativefee.Reference, transactionHash string, budget time.Duration) (*model.StTransactionNativeFeeSettlement, error) {
	if ctx == nil || budget < time.Minute || budget > 15*time.Minute {
		return nil, errors.New("native fee settlement requires an owned 60s–15m budget")
	}
	owner, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	read := func(ctx context.Context, name string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resource, err := server.Config.SimpleResource(name)
		if err != nil {
			return nil, err
		}
		raw, err := resource.BytesE()
		return raw, errors.Join(err, ctx.Err())
	}
	policy, authority, err := loadNativeFeeDenomination(owner, cfg, read)
	if err != nil {
		return nil, err
	}
	verified, err := nativefee.Invoke(owner, policy.NativeAuthority, request, transactionHash, budget)
	if err != nil {
		return nil, err
	}
	return model.SettleStTransactionNativeFee(owner, intentId, policy, authority, verified)
}

func loadNativeFeeDenomination(ctx context.Context, cfg *StConfig, read func(context.Context, string) ([]byte, error)) (*server.StNativeFeeDenominationPolicy, *server.StNativeFeeDenominationAuthority, error) {
	if ctx == nil || read == nil {
		return nil, nil, errStNotConfigured
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	authorityBytes, err := read(ctx, "native-fee-denomination-authority.yml")
	if err != nil {
		return nil, nil, err
	}
	authority, err := server.ParseStNativeFeeDenominationAuthority(authorityBytes)
	if err != nil {
		return nil, nil, err
	}
	policyBytes, err := read(ctx, "native-fee-denomination-policy.yml")
	if err != nil {
		return nil, nil, err
	}
	policy, err := server.ParseStNativeFeeDenominationPolicy(policyBytes)
	if err != nil {
		return nil, nil, err
	}
	if cfg != nil && (policy.Profile != cfg.Profile || policy.ChainId != cfg.ChainId || policy.GenesisHash != fmt.Sprintf("0x%x", cfg.GenesisHash) || policy.NoId != cfg.NoId) {
		return nil, nil, errors.New("native fee denomination differs from the configured operator deployment")
	}
	if err := errors.Join(ctx.Err(), policy.Verify(authority)); err != nil {
		return nil, nil, err
	}
	return policy, authority, nil
}
