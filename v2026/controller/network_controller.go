package controller

import (
	// "time"
	"fmt"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// A coded network create refusal for a client that asked for `result_errors`.
// It keeps the refusal's HTTP status, because the signup liveness monitor counts
// every 2xx network create as a created network, and the router answers it
// with the result as the JSON body, where the client reads `error.code`.
type networkCreateCodedRefusal struct {
	result *model.NetworkCreateResult
}

// The refusal's "<status> <message>" transport text.
func (self *networkCreateCodedRefusal) Error() string {
	return self.result.Error.Error()
}

// The body the router writes with the refusal's status.
func (self *networkCreateCodedRefusal) HttpErrorResultBody() any {
	return self.result
}

func NetworkCreate(
	networkCreate model.NetworkCreateArgs,
	clientSession *session.ClientSession,
) (*model.NetworkCreateResult, error) {
	result, err := model.NetworkCreate(networkCreate, clientSession)
	if err != nil {
		return nil, err
	}
	if result.Error != nil {
		if result.Error.Code != "" && networkCreate.ResultErrors {
			// a coded refusal, for a client that reads the code: still the
			// refusal's status, with the result as the body
			return nil, &networkCreateCodedRefusal{result: result}
		}
		// Preserve the model's explicit client-refusal classification. Turning
		// every result message into a plain error reported ordinary form and
		// duplicate-account refusals as unhandled HTTP 500 failures.
		return nil, result.Error
	}
	enrollNetworkCreateOnboardingPostPrimary(result, clientSession)

	/**
	 * we only add transfer balance if the user is not pro (no balance code redeemed)
	 *
	 * redeeming a balance code successfully automatically adds paid transfer balance for the network
	 */
	if !result.IsPro {
		// add regular balance
		AddRefreshTransferBalance(clientSession.Ctx, result.Network.NetworkId)
	}

	if networkCreate.ReferralCode != nil {
		model.CreateNetworkReferral(
			clientSession.Ctx,
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
		model.AccountPreferencesSetForNetwork(clientSession.Ctx, result.Network.NetworkId, productUpdates)
		WriteServerEvent(clientSession, result.Network.NetworkId, model.EventSignupOptoutChanged, map[string]any{
			"product_updates": productUpdates,
		}, "")
	}

	// if verification required, send it
	if result.VerificationRequired != nil {
		// why no code was sent goes back to the client; it used to be dropped
		result.VerificationRequired.SendError = sendVerification(
			result.VerificationRequired.UserAuth,
			verifyUseNumeric,
			clientSession,
			authVerifySendResult,
		)
	} else {

		if result.UserAuth != nil && !result.SuppressAccountMessages {
			awsMessageSender := GetAWSMessageSender()
			awsMessageSender.SendAccountMessageTemplate(
				*result.UserAuth,
				&NetworkWelcomeTemplate{},
			)
		}

		byJwt, err := session.ParseByJwt(clientSession.Ctx, *(result.Network.ByJwt))
		if err == nil {
			AccountPreferencesSet(
				&model.AccountPreferencesSetArgs{
					ProductUpdates: productUpdates,
				},
				clientSession.WithByJwt(byJwt),
			)
		}

	}

	return result, nil
}

func enrollNetworkCreateOnboardingPostPrimary(
	result *model.NetworkCreateResult,
	clientSession *session.ClientSession,
) {
	if result == nil || result.Network == nil || result.VerificationRequired != nil {
		return
	}
	// Account creation is already committed. Enroll before optional balance,
	// referral, preference, event, or welcome-message work so a failure in one
	// of those projections cannot silently skip the campaign row.
	userAuth := ""
	if result.UserAuth != nil {
		userAuth = *result.UserAuth
	}
	runPostPrimaryOnboarding(clientSession, func(postSession *session.ClientSession) {
		EnrollNetworkOnboarding(postSession, result.Network.NetworkId, userAuth, false)
	})
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

func NetworkRemove(clientSession *session.ClientSession) (*NetworkRemoveResult, error) {
	// Authorize before the provider call. RemoveNetwork repeats this check while
	// holding the deletion row lock, but moving Stripe cancellation ahead of
	// deletion must not let a non-admin cancel the network's subscription.
	network := model.GetNetwork(clientSession)
	if network == nil || network.AdminUserId == nil || *network.AdminUserId != clientSession.ByJwt.UserId {
		return nil, fmt.Errorf("Could not remove network")
	}

	// Provider cancellation is the fail-closed prerequisite for deleting the
	// local owner. Each confirmed cancellation closes only its own renewal, so
	// partial progress is retryable and any remaining failure leaves the
	// network and its authentication context intact.
	return networkRemoveWithSteps(clientSession, &defaultNetworkRemoveSteps)
}

type GetNetworkReliabilityResult struct {
	ReliabilityWindow *model.ReliabilityWindow    `json:"reliability_window,omitempty"`
	Error             *GetNetworkReliabilityError `json:"error,omitempty"`
}

type GetNetworkReliabilityError struct {
	Message string `json:"message"`
}

func GetNetworkReliability(
	clientSession *session.ClientSession,
) (*GetNetworkReliabilityResult, error) {

	window, err := model.GetNetworkReliabilityWindow(clientSession)
	if err != nil {
		return nil, err
	}

	return &GetNetworkReliabilityResult{
		ReliabilityWindow: window,
	}, nil
}
