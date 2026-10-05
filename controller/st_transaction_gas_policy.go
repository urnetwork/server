package controller

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func (self *CoreStClient) operatorGasNow() time.Time {
	if self.gasNow != nil {
		return self.gasNow()
	}
	return server.NowUtc()
}

// Missing production policy denies new EVM signing/broadcast, while receipt
// reconciliation and existing non-EVM payment paths keep their own admission.
func (self *CoreStClient) operatorGasAdmission(ctx context.Context, signingAccounts ...common.Address) (*server.StOperatorGasPolicy, *server.StOperatorGasAuthority, error) {
	if self.cfg == nil {
		return nil, nil, errStNotConfigured
	}
	policy := self.cfg.OperatorGasPolicy.Clone()
	if policy == nil && self.cfg.Profile == "testnet" && self.cfg.ChainId == 945 {
		if env, _ := server.Env(); env != "main" {
			for _, key := range []*ecdsa.PrivateKey{self.cfg.DepositKey, self.cfg.RootKey} {
				if key != nil {
					signingAccounts = append(signingAccounts, crypto.PubkeyToAddress(key.PublicKey))
				}
			}
			for _, account := range signingAccounts {
				enrolled, err := model.StOperatorGasAccountEnrolled(ctx, self.cfg.ChainId, fmt.Sprintf("0x%x", self.cfg.GenesisHash), account.Hex())
				if err != nil {
					return nil, nil, err
				}
				if enrolled {
					return nil, nil, fmt.Errorf("%w: an enrolled account cannot remove its signed policy", model.ErrStOperatorGasAllowance)
				}
			}
			return nil, nil, nil
		}
	}
	refuse := func(err error) (*server.StOperatorGasPolicy, *server.StOperatorGasAuthority, error) {
		return nil, nil, fmt.Errorf("%w: %w", model.ErrStOperatorGasAllowance, err)
	}
	if policy == nil {
		return refuse(errors.New("independently approved operator gas policy is absent"))
	}
	if err := ctx.Err(); err != nil {
		return refuse(err)
	}
	if err := policy.Validate(); err != nil {
		return refuse(err)
	}
	if self.cfg.DepositKey == nil || self.cfg.RootKey == nil || policy.Profile != self.cfg.Profile || policy.ChainId != self.cfg.ChainId || policy.GenesisHash != fmt.Sprintf("0x%x", self.cfg.GenesisHash) || policy.NoId != self.cfg.NoId || policy.Coordinator != strings.ToLower(self.cfg.ContractAddress.Hex()) || policy.PolicyHash != fmt.Sprintf("0x%x", self.cfg.PolicyHash) || policy.Accounts[0].Address != strings.ToLower(crypto.PubkeyToAddress(self.cfg.DepositKey.PublicKey).Hex()) || policy.Accounts[1].Address != strings.ToLower(crypto.PubkeyToAddress(self.cfg.RootKey.PublicKey).Hex()) {
		return refuse(errors.New("operator gas policy differs from the selected deployment and vault role accounts"))
	}
	var encoded []byte
	var err error
	if self.gasAuthority != nil {
		encoded, err = self.gasAuthority(ctx)
	} else {
		resource, loadErr := server.Config.SimpleResource("operator-gas-authority.yml")
		if loadErr != nil {
			return refuse(loadErr)
		}
		encoded, err = resource.BytesBoundedE(ctx, 16*1024)
	}
	if err != nil {
		return refuse(err)
	}
	authority, err := server.ParseStOperatorGasAuthority(encoded)
	if err != nil {
		return refuse(err)
	}
	if err = policy.Verify(authority, self.operatorGasNow()); err != nil {
		return refuse(err)
	}
	if err = model.AdmitStOperatorGasPolicy(ctx, policy, authority); err != nil {
		return refuse(err)
	}
	return policy, authority, nil
}

// A pending result is reconstructed only from its durable exact unsigned
// envelope. Changed fees or a request to cancel cannot overwrite that owner.
func (self *CoreStClient) signReservedStTransaction(ctx context.Context, client *ethclient.Client, key *ecdsa.PrivateKey, intent *model.StTransactionIntent, requestedKind string, reservation *model.StTransactionGasReservation, policy *server.StOperatorGasPolicy, authority *server.StOperatorGasAuthority) (result *model.StTransactionAttempt, resultErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				resultErr = errors.Join(resultErr, err)
				result = nil
			} else {
				panic(recovered)
			}
		}
	}()
	if reservation == nil || reservation.IntentId != intent.IntentId || reservation.Kind != requestedKind || reservation.ScopeKey != policy.Scope() {
		return nil, fmt.Errorf("%w: pending original signing outcome cannot become a different request", model.ErrStOperatorGasAllowance)
	}
	var unsigned types.Transaction
	if err := unsigned.UnmarshalBinary(reservation.UnsignedTransaction); err != nil {
		return nil, err
	}
	maximum, err := model.ValidateStTransactionGasEnvelope(policy, &unsigned)
	if err != nil {
		return nil, err
	}
	signer := types.LatestSignerForChainID(new(big.Int).SetUint64(intent.ChainId))
	if strings.ToLower(signer.Hash(&unsigned).Hex()) != reservation.SigningHash || maximum.String() != reservation.MaximumLiabilityWei || unsigned.Nonce() != intent.Nonce {
		return nil, errors.New("operator gas original reservation bytes or liability changed")
	}
	if err := self.validateDepositAttempt(ctx, client, intent, reservation.Kind); err != nil {
		return nil, err
	}
	if err := stPayoutAdmission(ctx, self.cfg); err != nil {
		return nil, err
	}
	if err := errors.Join(ctx.Err(), policy.Verify(authority, self.operatorGasNow())); err != nil {
		return nil, err
	}
	if reservation.SignedTxHash != nil {
		attempts := model.GetStTransactionAttempts(ctx, intent.IntentId)
		for _, attempt := range attempts {
			if attempt.Attempt == reservation.Attempt && strings.EqualFold(attempt.TxHash, *reservation.SignedTxHash) {
				_, _, _, observeErr := self.observeTransactionAttempts(ctx, client, intent, attempts)
				return attempt, observeErr
			}
		}
		return nil, errors.New("operator gas original signed reservation has no matching durable attempt")
	}
	if err := model.RequireStTransactionGasUnsettled(ctx, intent.IntentId); err != nil {
		return nil, err
	}
	var signed *types.Transaction
	if self.transactionSigner != nil {
		signed, err = self.transactionSigner(ctx, &unsigned, signer, key)
	} else {
		signed, err = types.SignTx(&unsigned, signer, key)
	}
	if signed == nil {
		if err == nil {
			err = errors.New("operator transaction signer returned no result")
		}
		return nil, fmt.Errorf("operator signing outcome remains reserved: %w", err)
	}
	raw, encodeErr := signed.MarshalBinary()
	if encodeErr != nil {
		return nil, errors.Join(err, encodeErr)
	}
	attempt := &model.StTransactionAttempt{IntentId: intent.IntentId, Attempt: reservation.Attempt, Kind: reservation.Kind, TxHash: strings.ToLower(signed.Hash().Hex()), RawTransaction: raw, GasLimit: signed.Gas()}
	if signed.Type() == types.LegacyTxType {
		attempt.GasPrice = stBigIntString(signed.GasPrice())
	} else {
		attempt.GasTipCap = stBigIntString(signed.GasTipCap())
		attempt.GasFeeCap = stBigIntString(signed.GasFeeCap())
	}
	// Once a signature exists, caller cancellation cannot erase its durable
	// outcome. This bounded local commit grants no permission to broadcast.
	commitCtx, stopCommit := context.WithTimeout(context.WithoutCancel(ctx), stSendTimeout)
	defer stopCommit()
	result = model.AddStTransactionAttempt(commitCtx, attempt)
	if result == nil {
		return nil, errors.New("operator gas signed intent became terminal; original reservation retained")
	}
	if finishErr := errors.Join(err, ctx.Err(), policy.Verify(authority, self.operatorGasNow())); finishErr != nil {
		return result, finishErr
	}
	if reservation.Reused {
		// A lost signer result may already have its original canonical outcome.
		// Reconcile the reconstructed exact hash before any identical-byte send.
		_, _, _, observeErr := self.observeTransactionAttempts(ctx, client, intent, model.GetStTransactionAttempts(ctx, intent.IntentId))
		if observeErr != nil {
			return result, observeErr
		}
	}
	return result, nil
}

// Preserve a stable, public byte representation for offline policy producers.
// This function performs no signature and never reads the operator vault.
func StOperatorGasPolicyApprovalRequest(policy *server.StOperatorGasPolicy) ([]byte, error) {
	if _, err := policy.SigningBytes(); err != nil {
		return nil, err
	}
	if policy.Signature != "" {
		return nil, errors.New("operator gas review request must be unsigned")
	}
	return json.MarshalIndent(policy, "", "  ")
}
