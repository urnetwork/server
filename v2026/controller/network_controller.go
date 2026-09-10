package controller

import (
	// "time"
	"fmt"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func NetworkCreate(
	networkCreate model.NetworkCreateArgs,
	session *session.ClientSession,
) (*model.NetworkCreateResult, error) {
	result, err := model.NetworkCreate(networkCreate, session)
	if err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("%s", result.Error.Message)
	}

	/**
	 * we only add transfer balance if the user is not pro (no balance code redeemed)
	 *
	 * redeeming a balance code successfully automatically adds paid transfer balance for the network
	 */
	if !result.IsPro {
		// add regular balance
		AddRefreshTransferBalance(session.Ctx, result.Network.NetworkId)
	}

	if networkCreate.ReferralCode != nil {
		model.CreateNetworkReferral(
			session.Ctx,
			result.Network.NetworkId,
			*networkCreate.ReferralCode,
		)

		// note: should we check if the network subscribes before applying points?
		// if networkReferral != nil {
		// 	model.ApplyNetworkPoints(
		// 		session.Ctx,
		// 		*networkReferral.ReferralNetworkId,
		// 		"referral",
		// 	)
		// }

	}

	verifyUseNumeric := false

	if networkCreate.VerifyUseNumeric {
		verifyUseNumeric = true
	}

	// the sign-up form's product-updates line (default on). Persisted for the
	// network now, whichever branch below runs, so a verification-pending
	// sign-up keeps its choice until AuthVerify completes it.
	productUpdates := ProductUpdatesFromCreateArgs(&networkCreate)
	if result.Network != nil {
		model.AccountPreferencesSetForNetwork(session.Ctx, result.Network.NetworkId, productUpdates)
		WriteServerEvent(session, result.Network.NetworkId, model.EventSignupOptoutChanged, map[string]any{
			"product_updates": productUpdates,
		}, "")
	}

	// if verification required, send it
	if result.VerificationRequired != nil {
		verifySend := AuthVerifySendArgs{
			UserAuth:   result.VerificationRequired.UserAuth,
			UseNumeric: verifyUseNumeric,
		}
		AuthVerifySend(verifySend, session)
	} else {

		if result.UserAuth != nil && !result.SuppressAccountMessages {
			awsMessageSender := GetAWSMessageSender()
			awsMessageSender.SendAccountMessageTemplate(
				*result.UserAuth,
				&NetworkWelcomeTemplate{},
			)
		}

		byJwt, err := jwt.ParseByJwt(session.Ctx, *(result.Network.ByJwt))
		if err == nil {
			AccountPreferencesSet(
				&model.AccountPreferencesSetArgs{
					ProductUpdates: productUpdates,
				},
				session.WithByJwt(byJwt),
			)
		}

		// the onboarding campaign: the account exists and can be mailed now
		// (a verification-pending sign-up enters from AuthVerify instead)
		userAuth := ""
		if result.UserAuth != nil {
			userAuth = *result.UserAuth
		}
		StartOnboardingCampaign(session, result.Network.NetworkId, userAuth)

	}

	return result, nil
}

type UpdateNetworkNameArgs struct {
	NetworkName string `json:"network_name"`
}

type UpdateNetworkNameError struct {
	Message string `json:"message"`
}

type UpdateNetworkNameResult struct {
	Error *UpdateNetworkNameError `json:"error,omitempty"`
}

func UpdateNetworkName(
	args *UpdateNetworkNameArgs,
	clientSession *session.ClientSession,
) (*UpdateNetworkNameResult, error) {

	// get the current network name
	network := model.GetNetwork(clientSession)

	if network.NetworkName != args.NetworkName {
		// update the network name
		result, err := model.NetworkUpdate(
			model.NetworkUpdateArgs{NetworkName: args.NetworkName},
			clientSession,
		)
		if err != nil {
			return nil, err
		}

		if result.Error != nil {
			return &UpdateNetworkNameResult{
				Error: &UpdateNetworkNameError{
					Message: result.Error.Message,
				},
			}, nil
		}
	}

	return &UpdateNetworkNameResult{}, nil
}

type NetworkRemoveResultError struct {
	Message string `json:"message"`
}

type NetworkRemoveResult struct {
	Error *NetworkRemoveResultError `json:"error,omitempty"`
}

func NetworkRemove(session *session.ClientSession) (*NetworkRemoveResult, error) {
	// Authorize before the provider call. RemoveNetwork repeats this check while
	// holding the deletion row lock, but moving Stripe cancellation ahead of
	// deletion must not let a non-admin cancel the network's subscription.
	network := model.GetNetwork(session)
	if network == nil || network.AdminUserId == nil || *network.AdminUserId != session.ByJwt.UserId {
		return nil, fmt.Errorf("Could not remove network")
	}

	// Provider cancellation is the fail-closed prerequisite for deleting the
	// local owner. Each confirmed cancellation closes only its own renewal, so
	// partial progress is retryable and any remaining failure leaves the
	// network and its authentication context intact.
	if err := UnsubscribeStripe(session); err != nil {
		glog.Errorf("Failed to unsubscribe Stripe: %v", err)
		return &NetworkRemoveResult{
			Error: &NetworkRemoveResultError{
				Message: "Failed to unsubscribe Stripe",
			},
		}, nil
	}

	success, userAuths := model.RemoveNetwork(
		session.Ctx,
		session.ByJwt.NetworkId,
		&session.ByJwt.UserId,
	)

	if success {
		server.Tx(session.Ctx, func(tx server.PgTx) {
			for userAuth, _ := range userAuths {
				ScheduleRemoveProductUpdates(session, userAuth, tx)
			}
		})

		return &NetworkRemoveResult{}, nil
	}

	return nil, fmt.Errorf("Could not remove network")
}

type GetNetworkReliabilityResult struct {
	ReliabilityWindow *model.ReliabilityWindow    `json:"reliability_window,omitempty"`
	Error             *GetNetworkReliabilityError `json:"error,omitempty"`
}

type GetNetworkReliabilityError struct {
	Message string `json:"message"`
}

func GetNetworkReliability(
	session *session.ClientSession,
) (*GetNetworkReliabilityResult, error) {

	window, err := model.GetNetworkReliabilityWindow(session)
	if err != nil {
		return nil, err
	}

	return &GetNetworkReliabilityResult{
		ReliabilityWindow: window,
	}, nil
}
