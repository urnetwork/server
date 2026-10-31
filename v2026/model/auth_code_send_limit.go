package model

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The daily budgets of the verification and password reset codes sent to one
// email or phone (support inbox 835).
//
// Anyone can sign up with someone else's email. The sign-up stays unverified,
// and every verification or password reset request for the address sent it
// another code. The only limit was the auth attempt limit, five a client
// address in five minutes, which a stranger asking from many client addresses
// never meets. Now each address is sent a few codes of each kind a day,
// however many client addresses ask, and a password reset code goes only to an
// address that was verified (AuthPasswordResetCreateCode).
//
// The budgets are account action budgets (account_action_rate_limit.go) keyed
// by an hmac of the address instead of a user, so a budget follows the address
// and the table holds no email or phone.

const (
	AccountActionVerifyCodeSend = "verify_code_send"
	AccountActionResetCodeSend  = "reset_code_send"
)

// A sign-up sends one verification code, and a person who misses it asks for a
// few more, so five of each kind a day leave room for that.
const (
	UserAuthCodeSendLimit  = 5
	UserAuthCodeSendWindow = 24 * time.Hour
)

// The id the code budgets of an email or phone are recorded under.
func userAuthCodeSendBudgetId(userAuth string) server.Id {
	mac := hmac.New(sha256.New, passwordPepper())
	_, err := mac.Write([]byte("auth-code-send\x00"))
	server.Raise(err)
	_, err = mac.Write([]byte(userAuth))
	server.Raise(err)
	var budgetId server.Id
	copy(budgetId[:], mac.Sum(nil))
	return budgetId
}

// Spends one code of `action` from the budget of userAuth, a normalized email
// or phone, in the caller's transaction. A refusal spends nothing, so the
// caller must refuse before it replaces the code already sent.
func spendUserAuthCodeSendInTx(
	ctx context.Context,
	tx server.PgTx,
	userAuth string,
	action string,
) error {
	// the fixed acceptance phone is exercised repeatedly, as in UserAuthAttempt
	if testAuthPolicyForUserAuth(&userAuth).BypassRateLimits {
		return nil
	}
	allow, retryAfter := checkAndRecordAccountActionRateLimitInTx(
		ctx,
		tx,
		userAuthCodeSendBudgetId(userAuth),
		action,
		UserAuthCodeSendLimit,
		UserAuthCodeSendWindow,
	)
	if allow {
		return nil
	}
	return maxUserAuthCodeSendsError(retryAfter)
}

// The refusal of a code whose address has been sent its day's budget. The
// clients read the retry hint (`retry_after_seconds` and Retry-After).
func maxUserAuthCodeSendsError(retryAfter time.Duration) error {
	return &rateLimitError{
		message: fmt.Sprintf(
			"429 Too many codes have been sent to this email or phone in the last %d hours. "+
				"The last code sent still works until it expires. Please try again later.",
			int(UserAuthCodeSendWindow/time.Hour),
		),
		retryAfterSeconds: int((retryAfter + time.Second - 1) / time.Second),
	}
}
