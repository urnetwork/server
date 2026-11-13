package model

// Stable machine codes for a verification code that was not sent. Clients pick
// a localized message from the code and fall back to `Message`.
const (
	// the send provider (email or sms) refused or failed the send
	AuthVerifySendErrorCodeSendFailed = "verify_send_failed"
	// the user auth attempt limit refused a new code; see `RetryAfterSeconds`
	AuthVerifySendErrorCodeRateLimited = "verify_rate_limited"
)

// Why a verification code was not sent.
//
// Before this, a rate-limit refusal or a send failure after login or sign-up
// was dropped and the apps said a code was sent. It rides additively on the
// existing results (`verification_required.send_error`, `error` on
// /auth/verify-send) so older clients keep their current behavior.
type AuthVerifySendError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// seconds until a new code can be requested. Zero when not known.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

// The auth attempt limit refusal for `userAuth`, as returned by
// `AuthVerifyCreateCode`. Exposed for tests in other packages.
func Testing_MaxUserAuthAttemptsError(userAuth *string) error {
	return maxUserAuthAttemptsError(userAuth)
}
