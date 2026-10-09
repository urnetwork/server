package controller

import (
	// "context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	// "time"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

var SsoRedirectUrl = sync.OnceValue(func() string {
	c := server.Config.RequireSimpleResource("sso.yml").Parse()
	return c["web_connect"].(map[string]any)["redirect_url"].(string)
})

type AuthWalletChallengeArgs struct {
	WalletAddress *string `json:"wallet_address,omitempty"`
	Blockchain    *string `json:"blockchain,omitempty"`
}

type AuthWalletChallengeResult = model.WalletAuthChallengeResult

func AuthWalletChallenge(
	args AuthWalletChallengeArgs,
	session *session.ClientSession,
) (*AuthWalletChallengeResult, error) {
	walletAuthChallengeAttemptId, allow := model.WalletAuthChallengeAttempt(session)
	if !allow {
		return nil, model.MaxWalletAuthChallengeAttemptsError()
	}

	result := model.CreateWalletAuthChallenge(model.WalletAuthChallengeArgs{
		WalletAddress: args.WalletAddress,
		Blockchain:    args.Blockchain,
	}, session.Ctx)

	success := result.Error == nil
	model.SetWalletAuthChallengeAttemptSuccess(session.Ctx, walletAuthChallengeAttemptId, success)

	if !success {
		return nil, fmt.Errorf("%s", result.Error.Message)
	}

	return result, nil
}

func AuthLogin(
	login model.AuthLoginArgs,
	session *session.ClientSession,
) (*model.AuthLoginResult, error) {
	// fixme
	/*
	   userAuth, userAuthType := normalUserAuthV1(login.userAuth)

	   if userAuth == nil {
	       // fixme try to infer the login type based on the input
	       // if phone, and there there is no +xxx yyyy, infer the country code based on ipinfo
	   }
	*/

	return authLogin(login, session, model.AuthLogin)
}

// The model's login, injected so the result handling runs without a database.
type authLoginFunction func(model.AuthLoginArgs, *session.ClientSession) (*model.AuthLoginResult, error)

// Logs in. A coded refusal (a wallet signature that does not verify for its
// address) reaches a client that asked for `result_errors` in the result
// `error`; an older client gets it as the HTTP 401 it had before the code.
func authLogin(
	login model.AuthLoginArgs,
	session *session.ClientSession,
	modelLogin authLoginFunction,
) (*model.AuthLoginResult, error) {
	result, err := modelLogin(login, session)
	if err == nil && result != nil && result.Error != nil && result.Error.Code != "" && !login.ResultErrors {
		return nil, fmt.Errorf("401 %s", result.Error.Message)
	}
	return result, err
}

func AuthLoginWithPassword(
	loginWithPassword model.AuthLoginWithPasswordArgs,
	session *session.ClientSession,
) (*model.AuthLoginWithPasswordResult, error) {
	return authLoginWithPassword(loginWithPassword, session, model.AuthLoginWithPassword, authVerifySendResult)
}

type authLoginWithPasswordFunction func(model.AuthLoginWithPasswordArgs, *session.ClientSession) (*model.AuthLoginWithPasswordResult, error)

type authVerifySendFunction func(AuthVerifySendArgs, *session.ClientSession) (*AuthVerifySendResult, error)

// Logs in, and when the account still needs verification sends a code. Why a
// code was not sent goes to `verification_required.send_error`; it used to be
// dropped, so the apps said a code was sent when it was not.
func authLoginWithPassword(
	loginWithPassword model.AuthLoginWithPasswordArgs,
	session *session.ClientSession,
	login authLoginWithPasswordFunction,
	verifySend authVerifySendFunction,
) (*model.AuthLoginWithPasswordResult, error) {
	result, err := login(loginWithPassword, session)
	// if verification required, send it
	if result != nil && result.VerificationRequired != nil {
		result.VerificationRequired.SendError = sendVerification(
			result.VerificationRequired.UserAuth,
			loginWithPassword.VerifyOtpNumeric,
			session,
			verifySend,
		)
	}
	return result, err
}

// Sends a verification code after login or sign-up. Returns why no code was
// sent, or nil when it was.
func sendVerification(
	userAuth string,
	useNumeric bool,
	session *session.ClientSession,
	verifySend authVerifySendFunction,
) *AuthVerifySendError {
	result, err := verifySend(AuthVerifySendArgs{
		UserAuth:   userAuth,
		UseNumeric: useNumeric,
	}, session)
	if err != nil {
		// no code was created, so none can have been sent
		glog.Warningf("[auth]verification code not sent: %s\n", err)
		return &AuthVerifySendError{
			Code:    model.AuthVerifySendErrorCodeSendFailed,
			Message: verifySendFailedMessage,
		}
	}
	return result.Error
}

type AuthVerifySendArgs struct {
	UserAuth   string `json:"user_auth"`
	UseNumeric bool   `json:"use_numeric,omitempty"`
	// return a rate-limit refusal or a send failure in the result `error`
	// with a 200, instead of the HTTP 429 / 502 older clients expect
	ResultErrors bool `json:"result_errors,omitempty"`
}

type AuthVerifySendError = model.AuthVerifySendError

type AuthVerifySendResult struct {
	UserAuth string `json:"user_auth"`
	// set when no code was sent
	Error *AuthVerifySendError `json:"error,omitempty"`
}

const verifySendFailedMessage = "The verification code could not be sent. Please try again."

func AuthVerifySend(
	verifySend AuthVerifySendArgs,
	session *session.ClientSession,
) (*AuthVerifySendResult, error) {
	result, err := authVerifySendResult(verifySend, session)
	if err != nil {
		return nil, err
	}
	if result.Error != nil && !verifySend.ResultErrors {
		// older clients show any error status as a failed send
		return nil, legacyAuthVerifySendError(result.Error)
	}
	return result, nil
}

func authVerifySendResult(
	verifySend AuthVerifySendArgs,
	session *session.ClientSession,
) (*AuthVerifySendResult, error) {
	return authVerifySend(verifySend, session, model.AuthVerifyCreateCode, GetAWSMessageSender())
}

type authVerifyCreateCodeFunction func(model.AuthVerifyCreateCodeArgs, *session.ClientSession) (*model.AuthVerifyCreateCodeResult, error)

// Creates a verification code and sends it. A rate-limit refusal and a send
// failure come back in the result `Error`, never dropped: the caller must not
// tell the user a code was sent. Other errors are returned as errors.
func authVerifySend(
	verifySend AuthVerifySendArgs,
	session *session.ClientSession,
	createCode authVerifyCreateCodeFunction,
	messageSender MessageSender,
) (*AuthVerifySendResult, error) {
	userAuth, _ := model.NormalUserAuthV1(&verifySend.UserAuth)
	if userAuth == nil {
		return nil, fmt.Errorf("400 Invalid user auth.")
	}

	verifyCodeType := model.VerifyCodeDefault
	if verifySend.UseNumeric {
		verifyCodeType = model.VerifyCodeNumeric
	}

	verifyCreateCode := model.AuthVerifyCreateCodeArgs{
		UserAuth: *userAuth,
		CodeType: verifyCodeType,
	}
	verifyCreateCodeResult, err := createCode(verifyCreateCode, session)
	if err != nil {
		var rateLimit interface{ RetryAfterSeconds() int }
		if errors.As(err, &rateLimit) {
			return &AuthVerifySendResult{
				UserAuth: *userAuth,
				Error: &AuthVerifySendError{
					Code:              model.AuthVerifySendErrorCodeRateLimited,
					Message:           strings.TrimPrefix(err.Error(), "429 "),
					RetryAfterSeconds: rateLimit.RetryAfterSeconds(),
				},
			}, nil
		}
		return nil, err
	}

	result := &AuthVerifySendResult{
		UserAuth: *userAuth,
	}
	if verifyCreateCodeResult.VerifyCode == nil {
		message := verifySendFailedMessage
		if verifyCreateCodeResult.Error != nil {
			message = verifyCreateCodeResult.Error.Message
		}
		result.Error = &AuthVerifySendError{
			Code:    model.AuthVerifySendErrorCodeSendFailed,
			Message: message,
		}
		return result, nil
	}
	err = messageSender.SendAccountMessageTemplate(
		*userAuth,
		&AuthVerifyTemplate{
			VerifyCode: *verifyCreateCodeResult.VerifyCode,
		},
	)
	if err != nil {
		// the sender counts the failure and reports it at most once per
		// interval; a client can repeat this request, so the detail is verbose
		if glog.V(1) {
			glog.Infof("[auth]verification code send failed: %s\n", err)
		}
		result.Error = &AuthVerifySendError{
			Code:    model.AuthVerifySendErrorCodeSendFailed,
			Message: verifySendFailedMessage,
		}
	}
	return result, nil
}

// The error status /auth/verify-send answered before `result_errors`: 429 with
// a Retry-After for a rate limit (unchanged), and 502 for a failed send, which
// older clients show as a failed send instead of a 200 that claimed one.
func legacyAuthVerifySendError(sendError *AuthVerifySendError) error {
	switch sendError.Code {
	case model.AuthVerifySendErrorCodeRateLimited:
		return &verifySendStatusError{
			message:           fmt.Sprintf("429 %s", sendError.Message),
			retryAfterSeconds: sendError.RetryAfterSeconds,
		}
	default:
		return &verifySendStatusError{
			message: fmt.Sprintf("502 %s", sendError.Message),
		}
	}
}

// An error status the router maps from the "<status> " prefix, with the
// Retry-After hint it reads through `RetryAfterSeconds`.
type verifySendStatusError struct {
	message           string
	retryAfterSeconds int
}

func (self *verifySendStatusError) Error() string {
	return self.message
}

func (self *verifySendStatusError) RetryAfterSeconds() int {
	return self.retryAfterSeconds
}

func Testing_SendAuthVerifyCode(userAuth string) {
	normalUserAuth, _ := model.NormalUserAuthV1(&userAuth)

	verifyCode := model.Testing_CreateVerifyCode()

	awsMessageSender := GetAWSMessageSender()
	awsMessageSender.SendAccountMessageTemplate(
		*normalUserAuth,
		&AuthVerifyTemplate{
			VerifyCode: verifyCode,
		},
	)
}

type AuthPasswordResetArgs struct {
	UserAuth string `json:"user_auth"`
	// return a rate-limit refusal or a send failure in the result `error`
	// with a 200, instead of the HTTP 429 / 502 older clients expect
	ResultErrors bool `json:"result_errors,omitempty"`
}

type AuthPasswordResetResult struct {
	UserAuth string `json:"user_auth"`
	// set when no reset code was sent
	Error *AuthVerifySendError `json:"error,omitempty"`
}

func AuthPasswordReset(
	reset AuthPasswordResetArgs,
	session *session.ClientSession,
) (*AuthPasswordResetResult, error) {
	result, err := authPasswordReset(reset, session, model.AuthPasswordResetCreateCode, GetAWSMessageSender())
	if err != nil {
		return nil, err
	}
	if result.Error != nil && !reset.ResultErrors {
		// older clients show any error status as a failed send
		return nil, legacyAuthVerifySendError(result.Error)
	}
	return result, nil
}

const passwordResetSendFailedMessage = "The password reset code could not be sent. Please try again."

type authPasswordResetCreateCodeFunction func(model.AuthPasswordResetCreateCodeArgs, *session.ClientSession) (*model.AuthPasswordResetCreateCodeResult, error)

// Creates a password reset code and sends it. A rate-limit refusal and a send
// failure come back in the result `Error` with the verify send codes, never
// dropped: the caller must not tell the user a code was sent. Other errors are
// returned as errors.
func authPasswordReset(
	reset AuthPasswordResetArgs,
	session *session.ClientSession,
	createCode authPasswordResetCreateCodeFunction,
	messageSender MessageSender,
) (*AuthPasswordResetResult, error) {
	userAuth, _ := model.NormalUserAuthV1(&reset.UserAuth)
	if userAuth == nil {
		return nil, fmt.Errorf("Invalid user auth.")
	}

	resetCreateCode := model.AuthPasswordResetCreateCodeArgs{
		UserAuth: *userAuth,
	}
	resetCreateCodeResult, err := createCode(resetCreateCode, session)
	if err != nil {
		var rateLimit interface{ RetryAfterSeconds() int }
		if errors.As(err, &rateLimit) {
			return &AuthPasswordResetResult{
				UserAuth: *userAuth,
				Error: &AuthVerifySendError{
					Code:              model.AuthVerifySendErrorCodeRateLimited,
					Message:           strings.TrimPrefix(err.Error(), "429 "),
					RetryAfterSeconds: rateLimit.RetryAfterSeconds(),
				},
			}, nil
		}
		return nil, err
	}

	result := &AuthPasswordResetResult{
		UserAuth: *userAuth,
	}
	if resetCreateCodeResult.ResetCode == nil {
		message := passwordResetSendFailedMessage
		if resetCreateCodeResult.Error != nil {
			message = resetCreateCodeResult.Error.Message
		}
		result.Error = &AuthVerifySendError{
			Code:    model.AuthVerifySendErrorCodeSendFailed,
			Message: message,
		}
		return result, nil
	}
	err = messageSender.SendAccountMessageTemplate(
		*userAuth,
		&AuthPasswordResetTemplate{
			ResetCode: *resetCreateCodeResult.ResetCode,
		},
	)
	if err != nil {
		// the sender counts the failure and reports it at most once per
		// interval; a client can repeat this request, so the detail is verbose
		if glog.V(1) {
			glog.Infof("[auth]password reset code send failed: %s\n", err)
		}
		result.Error = &AuthVerifySendError{
			Code:    model.AuthVerifySendErrorCodeSendFailed,
			Message: passwordResetSendFailedMessage,
		}
	}
	return result, nil
}

type AuthPasswordSetResult struct {
	Error *model.AuthPasswordSetError `json:"error,omitempty"`
}

func AuthPasswordSet(passwordSet model.AuthPasswordSetArgs, session *session.ClientSession) (*AuthPasswordSetResult, error) {
	passwordSetResult, err := model.AuthPasswordSet(passwordSet, session)
	if err != nil {
		return nil, err
	}
	if passwordSetResult.Error != nil {
		return &AuthPasswordSetResult{
			Error: passwordSetResult.Error,
		}, nil
	}
	// The password-changed notice was added to the account message outbox in
	// the transaction that changed the password (model.AuthPasswordSet), for
	// the admin's email or phone when the account has one.

	safePasswordSetResult := &AuthPasswordSetResult{}
	return safePasswordSetResult, nil
}

func AuthVerify(
	verify model.AuthVerifyArgs,
	session *session.ClientSession,
) (*model.AuthVerifyResult, error) {
	result, err := model.AuthVerify(verify, session)
	if err == nil {
		completeAuthVerify(result, verify.UserAuth, session, defaultAuthVerifyEffects())
	}
	return result, err
}

// authVerifyEffects are the side effects of a successful verification, after
// its commit. The welcome is not one of them: model.AuthVerify adds it to the
// account message outbox in the verification's transaction.
type authVerifyEffects struct {
	// enrollOnboarding enrolls the verified network and returns its parsed jwt
	enrollOnboarding func(*model.AuthVerifyResult, string, *session.ClientSession) *jwt.ByJwt
	parseByJwt       func(*session.ClientSession, string) *jwt.ByJwt
	// syncProductUpdates runs with the verified network's jwt
	syncProductUpdates func(*session.ClientSession)
}

func defaultAuthVerifyEffects() authVerifyEffects {
	return authVerifyEffects{
		enrollOnboarding: enrollAuthVerifyOnboardingPostPrimary,
		parseByJwt: func(clientSession *session.ClientSession, signedByJwt string) *jwt.ByJwt {
			byJwt, err := jwt.ParseByJwt(clientSession.Ctx, signedByJwt)
			if err != nil {
				return nil
			}
			return byJwt
		},
		syncProductUpdates: func(verifiedSession *session.ClientSession) {
			// the preference the sign-up form asked for was persisted by
			// NetworkCreate; completing verification syncs it, it never
			// overrides an opt-out with a default
			productUpdates := true
			if preferences := model.AccountPreferencesGet(verifiedSession); preferences != nil {
				productUpdates = preferences.ProductUpdates
			}
			AccountPreferencesSet(
				&model.AccountPreferencesSetArgs{
					ProductUpdates: productUpdates,
				},
				verifiedSession,
			)
		},
	}
}

// completeAuthVerify runs after the verification is committed. The onboarding
// campaign, like the welcome email the verification owed in its transaction, is
// for a new account only: verifying an email or phone added later to an
// existing account (one created with Apple, Google, a wallet, or another email
// or phone) is not a sign-up. Every verification syncs the network's existing
// product-updates preference, which never overrides an opt-out.
func completeAuthVerify(
	result *model.AuthVerifyResult,
	userAuth string,
	clientSession *session.ClientSession,
	effects authVerifyEffects,
) {
	if result == nil || result.Network == nil {
		return
	}
	var byJwt *jwt.ByJwt
	if result.NewAccount {
		byJwt = effects.enrollOnboarding(result, userAuth, clientSession)
	} else {
		byJwt = effects.parseByJwt(clientSession, result.Network.ByJwt)
	}
	if byJwt != nil {
		effects.syncProductUpdates(clientSession.WithByJwt(byJwt))
	}
}

func enrollAuthVerifyOnboardingPostPrimary(
	result *model.AuthVerifyResult,
	userAuth string,
	clientSession *session.ClientSession,
) (byJwt *jwt.ByJwt) {
	if result == nil || result.Network == nil {
		return nil
	}
	runPostPrimaryOnboarding(clientSession, func(postSession *session.ClientSession) {
		parsedByJwt, err := jwt.ParseByJwt(postSession.Ctx, result.Network.ByJwt)
		if err != nil {
			return
		}
		byJwt = parsedByJwt
		// Verification is already committed. Enroll before optional welcome
		// and preference projections so their failures cannot skip the row.
		EnrollNetworkOnboarding(postSession, byJwt.NetworkId, userAuth, false)
	})
	return
}

/**
 * Refresh JWT
 */

type RefreshTokenError struct {
	Message string `json:"message"`
}

type RefreshTokenResult struct {
	ByJwt string             `json:"by_jwt,omitempty"`
	Error *RefreshTokenError `json:"error,omitempty"`
}

// refreshMintHook runs after the router's state check and before a refresh
// mints its token. Tests land a credential change in that gap with it.
var refreshMintHook atomic.Pointer[func(*session.ClientSession)]

// Testing_SetRefreshMintHook runs hook between the router's state check and the
// mint of /auth/refresh and /auth/network-refresh. It returns the function that
// restores the previous hook.
func Testing_SetRefreshMintHook(hook func(*session.ClientSession)) func() {
	previous := refreshMintHook.Swap(&hook)
	return func() {
		refreshMintHook.Store(previous)
	}
}

func runRefreshMintHook(session *session.ClientSession) {
	if hook := refreshMintHook.Load(); hook != nil && *hook != nil {
		(*hook)(session)
	}
}

func RefreshToken(session *session.ClientSession) (*RefreshTokenResult, error) {
	networkId := session.ByJwt.NetworkId

	if session.ByJwt.ClientId == nil {
		return &RefreshTokenResult{
			Error: &RefreshTokenError{
				Message: "Client ID is required for token refresh.",
			},
		}, nil
	}

	if session.ByJwt.DeviceId == nil {
		return &RefreshTokenResult{
			Error: &RefreshTokenError{
				Message: "Device ID is required for token refresh.",
			},
		}, nil
	}

	// active only: a removed client must stop refreshing (the app logs out
	// on this error), not keep its jwt alive until the row is reaped
	clientNetworkId, err := model.FindActiveClientNetwork(
		session.Ctx,
		*session.ByJwt.ClientId,
	)
	if err != nil {
		return &RefreshTokenResult{
			Error: &RefreshTokenError{
				Message: "Client does not exist",
			},
		}, nil
	}

	if clientNetworkId != networkId {
		// not sure why this would happen, but doesn't hurt to check
		return &RefreshTokenResult{
			Error: &RefreshTokenError{
				Message: "Client does not belong to the authenticated network.",
			},
		}, nil
	}

	isPro := model.IsProFresh(
		session.Ctx,
		&networkId,
	)

	// Re-read the current network name from the DB rather than carrying
	// forward session.ByJwt.NetworkName -- that's just whatever name was
	// baked into the JWT being refreshed, so a rename via
	// change-name/claim-name would never show up in the client until it
	// happened to get a truly fresh JWT (e.g. by logging out and back in).
	networkName := session.ByJwt.NetworkName
	server.Db(session.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
				SELECT network_name FROM network
				WHERE admin_user_id = $1
			`,
			session.ByJwt.UserId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkName))
			}
		})
	})

	// The refreshed token keeps the presented token's create time, as every
	// derived credential keeps its root's (AuthCodeCreate, AuthNetworkClient).
	// A password reset expires the tokens created before it
	// (credential_change_time), and stamps its transaction's start, so a reset
	// that commits after the router's state check can still be stamped before
	// this mint. A fresh create time would outlive that reset; the presented
	// token's expires with it.
	runRefreshMintHook(session)
	byJwt := jwt.NewByJwtWithCreateTime(
		networkId,
		session.ByJwt.UserId,
		networkName,
		session.ByJwt.CreateTime,
		false,
		isPro,
	)

	return &RefreshTokenResult{
		ByJwt: byJwt.Client(*session.ByJwt.DeviceId, *session.ByJwt.ClientId).Sign(),
	}, nil
}

/**
 * Refresh a network JWT
 */

// The network token is the network's sign-in token: a by_jwt with no client.
// It lives 30 days like every token, and /auth/refresh renews only client
// tokens, so a network token renews here. The route is network only
// (api/route_authz.go): the router refuses a client token before this runs,
// because a client token must never obtain a network token. That is why
// /auth/refresh became client only (3b48aa3d).

type NetworkRefreshTokenResult = RefreshTokenResult

// the bounded outcomes of a network token refresh
const (
	networkRefreshRenewed       = "renewed"
	networkRefreshRefusedClient = "refused_client"
	networkRefreshRefusedApiKey = "refused_api_key"
	networkRefreshStateInvalid  = "state_invalid"
)

var networkRefreshCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "auth",
		Name:      "network_refreshes_total",
		Help:      "Network token refreshes (POST /auth/network-refresh) by bounded outcome",
	},
	[]string{"outcome"},
)

func init() {
	for _, outcome := range []string{
		networkRefreshRenewed,
		networkRefreshRefusedClient,
		networkRefreshRefusedApiKey,
		networkRefreshStateInvalid,
	} {
		networkRefreshCounter.WithLabelValues(outcome)
	}
	prometheus.MustRegister(networkRefreshCounter)
}

func NetworkRefreshToken(session *session.ClientSession) (*NetworkRefreshTokenResult, error) {
	if session.ByJwt.ClientId != nil || session.ByJwt.DeviceId != nil {
		// the router refuses a client token first. This keeps the handler from
		// minting a network token for one whatever routes to it.
		networkRefreshCounter.WithLabelValues(networkRefreshRefusedClient).Inc()
		return &NetworkRefreshTokenResult{
			Error: &RefreshTokenError{
				Message: "A client token is not refreshed as a network token. Refresh it with /auth/refresh.",
			},
		}, nil
	}
	if session.ApiKeyAuthenticated {
		// An API key does not expire. Its session holds the network identity
		// the key stands for, and signing that would turn a key that can be
		// removed into a token that outlives the removal.
		networkRefreshCounter.WithLabelValues(networkRefreshRefusedApiKey).Inc()
		return &NetworkRefreshTokenResult{
			Error: &RefreshTokenError{
				Message: "An API key does not expire and is not refreshed.",
			},
		}, nil
	}

	networkId := session.ByJwt.NetworkId
	// the current name, not the one in the presented token, so a rename shows
	// on the next refresh
	network := model.GetNetwork(session)
	if network == nil || network.AdminUserId == nil || *network.AdminUserId != session.ByJwt.UserId {
		// the network was removed, or changed admin, after the router's state
		// check
		networkRefreshCounter.WithLabelValues(networkRefreshStateInvalid).Inc()
		return nil, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
	}
	isPro := model.IsProFresh(session.Ctx, &networkId)

	// keeps the presented token's create time, for the reason RefreshToken
	// does: the password reset that expires the presented token expires the
	// refreshed one too, whenever the reset commits
	runRefreshMintHook(session)
	byJwt := jwt.NewByJwtWithCreateTime(
		networkId,
		session.ByJwt.UserId,
		network.NetworkName,
		session.ByJwt.CreateTime,
		session.ByJwt.GuestMode,
		isPro,
	)
	byJwt.Roles = slices.Clone(session.ByJwt.Roles)
	byJwt.Principal = session.ByJwt.Principal

	networkRefreshCounter.WithLabelValues(networkRefreshRenewed).Inc()
	return &NetworkRefreshTokenResult{
		ByJwt: byJwt.Sign(),
	}, nil
}

type AddAuthArgs = model.AddAuthMethod
type AddAuthResult = model.AddAuthMethodResult

func AddAuth(args AddAuthArgs, session *session.ClientSession) (*AddAuthResult, error) {
	return model.AddAuth(args, session)
}

type RemoveAuthArgs struct {
	AuthType string `json:"auth_type"`
}

type RemoveAuthResult struct {
	Error *RemoveAuthError `json:"error,omitempty"`
}

type RemoveAuthError struct {
	Message string `json:"message"`
}

func RemoveAuth(args RemoveAuthArgs, session *session.ClientSession) (*RemoveAuthResult, error) {
	err := model.RemoveAuth(session.Ctx, session.ByJwt.UserId, args.AuthType)
	if err != nil {
		// Every other refusal keeps the spec'd 200 + RemoveAuthResult.error
		// shape (bringyour.yml RemoveAuthResult), so the structured field stays
		// reachable. Peel the status prefix the model uses for the paths that
		// DO answer with a status, or the literal digits render in the client's
		// error toast.
		return &RemoveAuthResult{
			Error: &RemoveAuthError{
				Message: model.PeelStatusPrefix(err.Error()),
			},
		}, nil
	}
	return &RemoveAuthResult{}, nil
}
