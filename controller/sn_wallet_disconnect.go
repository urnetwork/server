package controller

// Disconnecting the network's payout wallet (support inbox 1099): the network
// owner removes the network's connected Bittensor wallet, so that no payout
// follows it, and the network has no payout wallet until one is connected and
// a payout consent is signed again.
//
//   - POST /sn/wallet/disconnect-code emails a confirmation code to the
//     account's sign-in addresses. The code is the re-auth: a stolen session
//     alone cannot disconnect the wallet, it also needs the account's inbox.
//   - POST /sn/wallet/disconnect takes the code and removes the wallet, in one
//     transaction that owes the payout wallet security notice
//     (model.DisconnectStWallets).
//
// Only a wallet connected without a signed payout consent is disconnected: the
// network's side copy (st_wallet) and the providers' account-history wallets
// set without a consent. A signed network consent or hotkey delegation in
// force ends only by a signed replacement: the protocol has no unsigned
// revocation (sn docs/NETWORK-WALLET-CONSENT.md §1.5), and independent replay
// verifies settlement against the signed chains, so while one is in force
// both routes answer consent_in_force and change nothing. A provider's own
// signed consent is never touched.
//
// Only the network owner's own sign-in may do either: a network token (the
// router refuses a client token), not an API key, an OAuth/MCP identity or a
// guest. Refusals come back in the result's error with a code.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The codes of a disconnect refusal (SnWalletDisconnectError.Code).
const (
	SnWalletDisconnectErrorCodeOwnerOnly      = "owner_only"
	SnWalletDisconnectErrorCodeNoWallet       = "no_wallet"
	SnWalletDisconnectErrorCodeConsentInForce = "consent_in_force"
	SnWalletDisconnectErrorCodeEmailRequired  = "email_required"
	SnWalletDisconnectErrorCodeRateLimited    = "rate_limited"
	SnWalletDisconnectErrorCodeSendFailed     = "send_failed"
	SnWalletDisconnectErrorCodeCodeInvalid    = "code_invalid"
	SnWalletDisconnectErrorCodeCodeExpired    = "code_expired"
)

type SnWalletDisconnectError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// for rate_limited, seconds until a code can be asked for again
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

type SnWalletDisconnectCodeArgs struct {
}

type SnWalletDisconnectCodeResult struct {
	// the masked sign-in email addresses the code was sent to
	SentTo []string `json:"sent_to,omitempty"`
	// when the code stops working, unix milliseconds
	ExpiresAtMillis int64                    `json:"expires_at_millis,omitempty"`
	Error           *SnWalletDisconnectError `json:"error,omitempty"`
}

type SnWalletDisconnectArgs struct {
	Code string `json:"code"`
}

type SnWalletDisconnectResult struct {
	Disconnected bool                     `json:"disconnected"`
	Error        *SnWalletDisconnectError `json:"error,omitempty"`
}

// The messages of the refusals, which a client that does not know the code
// shows.
const (
	snWalletDisconnectOwnerOnlyMessage      = "Only the network's own sign-in can disconnect its payout wallet."
	snWalletDisconnectNoWalletMessage       = "This network has no wallet to disconnect."
	snWalletDisconnectConsentInForceMessage = "Your signed payout consent sends your earnings to this wallet, and only another signed consent can replace it. Use Change wallet instead."
	snWalletDisconnectEmailRequiredMessage  = "Add an email sign-in to your account first. The confirmation code is sent to it."
	snWalletDisconnectSendFailedMessage     = "The code could not be sent. Please try again."
	snWalletDisconnectCodeInvalidMessage    = "That code is wrong. Check it and try again."
	snWalletDisconnectCodeExpiredMessage    = "That code expired or was already used. Ask for a new one."
)

// Whether a signed network payout consent or hotkey delegation is in force:
// its last epoch is at or after the current one. With the current epoch
// unknown, any such consent counts as in force. A retained consent that no
// longer verifies is an error, never "none".
func snSignedNetworkPayoutInForce(ctx context.Context, clientSession *session.ClientSession) (bool, error) {
	domain, ok := stClientKeyHistoryDomain()
	if !ok {
		// no deployment is configured, so no consent can be in force
		return false, nil
	}
	networkId := clientSession.ByJwt.NetworkId
	var epoch uint64
	epochKnown := false
	if cfg := stConfig(); cfg != nil && cfg.DeploymentKey() != "" {
		if summary := model.GetStEpochSummaryCache(ctx, cfg.DeploymentKey()); summary != nil {
			epoch, epochKnown = summary.Epoch, true
		} else if stEpoch := model.GetLatestStEpoch(ctx, cfg.DeploymentKey()); stEpoch != nil {
			epoch, epochKnown = stEpoch.Epoch, true
		}
	}
	inForce := func(throughEpoch uint64) bool {
		return !epochKnown || epoch <= throughEpoch
	}
	consent, err := model.GetNetworkWalletMappingConsent(ctx, domain, networkId)
	if err != nil {
		return false, err
	}
	if consent != nil && inForce(consent.ThroughEpoch) {
		return true, nil
	}
	delegation, err := snHotkeyWallet(ctx, domain, networkId)
	if err != nil {
		return false, err
	}
	if delegation != nil && inForce(delegation.ThroughEpoch) {
		return true, nil
	}
	return false, nil
}

// The refusal for a disconnect step that must not go on, or nil: not the
// owner's own sign-in, or a signed consent in force.
func snWalletDisconnectRefusal(clientSession *session.ClientSession) (*SnWalletDisconnectError, error) {
	if !accountOwnerSession(clientSession) {
		return &SnWalletDisconnectError{Code: SnWalletDisconnectErrorCodeOwnerOnly, Message: snWalletDisconnectOwnerOnlyMessage}, nil
	}
	inForce, err := snSignedNetworkPayoutInForce(clientSession.Ctx, clientSession)
	if err != nil {
		return nil, err
	}
	if inForce {
		return &SnWalletDisconnectError{Code: SnWalletDisconnectErrorCodeConsentInForce, Message: snWalletDisconnectConsentInForceMessage}, nil
	}
	return nil, nil
}

// The refusal for a model error, or the error to return.
func snWalletDisconnectModelRefusal(err error) (*SnWalletDisconnectError, error) {
	var rateLimit interface{ RetryAfterSeconds() int }
	switch {
	case errors.Is(err, model.ErrStWalletDisconnectOwnerOnly):
		return &SnWalletDisconnectError{Code: SnWalletDisconnectErrorCodeOwnerOnly, Message: snWalletDisconnectOwnerOnlyMessage}, nil
	case errors.Is(err, model.ErrStWalletDisconnectNoWallet):
		return &SnWalletDisconnectError{Code: SnWalletDisconnectErrorCodeNoWallet, Message: snWalletDisconnectNoWalletMessage}, nil
	case errors.Is(err, model.ErrStWalletDisconnectEmailRequired):
		return &SnWalletDisconnectError{Code: SnWalletDisconnectErrorCodeEmailRequired, Message: snWalletDisconnectEmailRequiredMessage}, nil
	case errors.Is(err, model.ErrStWalletDisconnectCodeInvalid):
		return &SnWalletDisconnectError{Code: SnWalletDisconnectErrorCodeCodeInvalid, Message: snWalletDisconnectCodeInvalidMessage}, nil
	case errors.Is(err, model.ErrStWalletDisconnectCodeExpired):
		return &SnWalletDisconnectError{Code: SnWalletDisconnectErrorCodeCodeExpired, Message: snWalletDisconnectCodeExpiredMessage}, nil
	case errors.As(err, &rateLimit):
		return &SnWalletDisconnectError{
			Code:              SnWalletDisconnectErrorCodeRateLimited,
			Message:           strings.TrimPrefix(err.Error(), "429 "),
			RetryAfterSeconds: rateLimit.RetryAfterSeconds(),
		}, nil
	default:
		return nil, err
	}
}

// The code email: the code that confirms the disconnect of the payout wallet.
type WalletDisconnectCodeTemplate struct {
	Code string
	// how long the code works, in minutes
	Minutes int
	BaseTemplate
}

func (self *WalletDisconnectCodeTemplate) Name() string {
	return "account_wallet_disconnect_code"
}

func SnWalletDisconnectCode(args *SnWalletDisconnectCodeArgs, clientSession *session.ClientSession) (*SnWalletDisconnectCodeResult, error) {
	return snWalletDisconnectCode(clientSession, model.DefaultStWalletDisconnectSettings(), GetAWSMessageSender())
}

func snWalletDisconnectCode(clientSession *session.ClientSession, settings *model.StWalletDisconnectSettings, messageSender MessageSender) (*SnWalletDisconnectCodeResult, error) {
	refusal, err := snWalletDisconnectRefusal(clientSession)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return &SnWalletDisconnectCodeResult{Error: refusal}, nil
	}
	code, err := model.SendStWalletDisconnectCode(clientSession.Ctx, settings, clientSession.ByJwt.UserId, clientSession.ByJwt.NetworkId)
	if err != nil {
		refusal, err := snWalletDisconnectModelRefusal(err)
		if err != nil {
			return nil, err
		}
		return &SnWalletDisconnectCodeResult{Error: refusal}, nil
	}
	// every sign-in address gets the code; the result lists those it went to
	sentTo := []string{}
	for _, recipient := range code.Recipients {
		err := messageSender.SendAccountMessageTemplate(
			recipient.Address,
			&WalletDisconnectCodeTemplate{
				Code:    code.Code,
				Minutes: int(settings.CodeTtl.Minutes()),
			},
		)
		if err != nil {
			if glog.V(1) {
				glog.Infof("[sn]payout wallet disconnect code send failed: %s\n", err)
			}
			continue
		}
		sentTo = append(sentTo, recipient.Masked)
	}
	if len(sentTo) == 0 {
		return &SnWalletDisconnectCodeResult{Error: &SnWalletDisconnectError{Code: SnWalletDisconnectErrorCodeSendFailed, Message: snWalletDisconnectSendFailedMessage}}, nil
	}
	return &SnWalletDisconnectCodeResult{
		SentTo:          sentTo,
		ExpiresAtMillis: code.ExpireTime.UnixMilli(),
	}, nil
}

func SnWalletDisconnect(args *SnWalletDisconnectArgs, clientSession *session.ClientSession) (*SnWalletDisconnectResult, error) {
	return snWalletDisconnect(args, clientSession, model.DefaultStWalletDisconnectSettings())
}

func snWalletDisconnect(args *SnWalletDisconnectArgs, clientSession *session.ClientSession, settings *model.StWalletDisconnectSettings) (*SnWalletDisconnectResult, error) {
	if args == nil {
		return nil, fmt.Errorf("400 A code is required.")
	}
	refusal, err := snWalletDisconnectRefusal(clientSession)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return &SnWalletDisconnectResult{Error: refusal}, nil
	}
	disconnected, err := model.DisconnectStWallets(clientSession.Ctx, settings, clientSession.ByJwt.UserId, clientSession.ByJwt.NetworkId, args.Code)
	if err != nil {
		refusal, err := snWalletDisconnectModelRefusal(err)
		if err != nil {
			return nil, err
		}
		return &SnWalletDisconnectResult{Error: refusal}, nil
	}
	glog.Infof(
		"[sn]payout wallet disconnected network_id=%s network_wallet=%t provider_wallets=%d\n",
		clientSession.ByJwt.NetworkId,
		disconnected.NetworkWallet,
		disconnected.ProviderWalletCount,
	)
	return &SnWalletDisconnectResult{Disconnected: true}, nil
}
