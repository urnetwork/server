package controller

// The verified email change routes (model/auth_email_change.go, support inbox
// 33 and 6446):
//
//   - POST /auth/email-change starts a change: a code to the current email
//     sign-in and another to the new address;
//   - GET /auth/email-change reads the waiting change;
//   - POST /auth/email-change/confirm takes one or both codes, and completes
//     the change once both are confirmed, then signs out the account's other
//     sessions (session REVOKE.md, as Settings > Sessions' "sign out all
//     others" does: the current session stays);
//   - POST /auth/email-change/cancel ends the waiting change.
//
// Only the network owner's own sign-in may do any of them
// (accountOwnerSession): never a client token (the router refuses it), an API
// key, an OAuth/MCP identity or a guest. Refusals come back in the result's
// error with a code.

import (
	"errors"
	"strings"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The codes of an email change refusal (AuthEmailChangeError.Code).
const (
	AuthEmailChangeErrorCodeOwnerOnly       = "owner_only"
	AuthEmailChangeErrorCodeNoEmail         = "no_email"
	AuthEmailChangeErrorCodeEmailUnverified = "email_unverified"
	AuthEmailChangeErrorCodeInvalidEmail    = "invalid_email"
	AuthEmailChangeErrorCodeSameEmail       = "same_email"
	AuthEmailChangeErrorCodeEmailInUse      = "email_in_use"
	AuthEmailChangeErrorCodeRateLimited     = "rate_limited"
	AuthEmailChangeErrorCodeSendFailed      = "send_failed"
	AuthEmailChangeErrorCodeNoChange        = "no_change"
	AuthEmailChangeErrorCodeCodeInvalid     = "code_invalid"
	AuthEmailChangeErrorCodeTooManyTries    = "too_many_tries"
)

type AuthEmailChangeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// for rate_limited, seconds until it can be asked again
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

type AuthEmailChangeArgs struct {
	NewEmail string `json:"new_email"`
}

type AuthEmailChangeResult struct {
	// the current email, masked
	CurrentEmail string `json:"current_email,omitempty"`
	// the new email, normalized
	NewEmail        string                `json:"new_email,omitempty"`
	ExpiresAtMillis int64                 `json:"expires_at_millis,omitempty"`
	Error           *AuthEmailChangeError `json:"error,omitempty"`
}

type AuthEmailChangeStatusResult struct {
	Pending          bool                  `json:"pending"`
	CurrentEmail     string                `json:"current_email,omitempty"`
	NewEmail         string                `json:"new_email,omitempty"`
	CurrentConfirmed bool                  `json:"current_confirmed,omitempty"`
	NewConfirmed     bool                  `json:"new_confirmed,omitempty"`
	ExpiresAtMillis  int64                 `json:"expires_at_millis,omitempty"`
	Error            *AuthEmailChangeError `json:"error,omitempty"`
}

type AuthEmailChangeConfirmArgs struct {
	CurrentCode string `json:"current_code,omitempty"`
	NewCode     string `json:"new_code,omitempty"`
}

type AuthEmailChangeConfirmResult struct {
	CurrentConfirmed       bool                  `json:"current_confirmed"`
	NewConfirmed           bool                  `json:"new_confirmed"`
	Complete               bool                  `json:"complete"`
	Email                  string                `json:"email,omitempty"`
	OtherSessionsSignedOut bool                  `json:"other_sessions_signed_out,omitempty"`
	Error                  *AuthEmailChangeError `json:"error,omitempty"`
}

type AuthEmailChangeCancelArgs struct {
}

type AuthEmailChangeCancelResult struct {
	Error *AuthEmailChangeError `json:"error,omitempty"`
}

// The messages of the refusals, which a client that does not know the code
// shows.
const (
	authEmailChangeOwnerOnlyMessage       = "Sign in again to change your email."
	authEmailChangeNoEmailMessage         = "Your account has no email sign-in. Add one as a sign-in method instead."
	authEmailChangeEmailUnverifiedMessage = "Verify your current email before you change it."
	authEmailChangeInvalidEmailMessage    = "Enter a valid email address."
	authEmailChangeSameEmailMessage       = "That's already your email."
	authEmailChangeEmailInUseMessage      = "Another account already uses that email."
	authEmailChangeSendFailedMessage      = "The code could not be sent. Please try again."
	authEmailChangeNoChangeMessage        = "This email change ended before it was confirmed. Start again."
	authEmailChangeCodeInvalidMessage     = "That code is wrong. Check it and try again."
	authEmailChangeTooManyTriesMessage    = "Too many wrong codes. Start again."
)

// The refusal for a model error, or the error to return.
func authEmailChangeRefusal(err error) (*AuthEmailChangeError, error) {
	var rateLimit interface{ RetryAfterSeconds() int }
	refusal := func(code string, message string) (*AuthEmailChangeError, error) {
		return &AuthEmailChangeError{Code: code, Message: message}, nil
	}
	switch {
	case errors.Is(err, model.ErrEmailChangeOwnerOnly):
		return refusal(AuthEmailChangeErrorCodeOwnerOnly, authEmailChangeOwnerOnlyMessage)
	case errors.Is(err, model.ErrEmailChangeNoEmail):
		return refusal(AuthEmailChangeErrorCodeNoEmail, authEmailChangeNoEmailMessage)
	case errors.Is(err, model.ErrEmailChangeEmailUnverified):
		return refusal(AuthEmailChangeErrorCodeEmailUnverified, authEmailChangeEmailUnverifiedMessage)
	case errors.Is(err, model.ErrEmailChangeInvalidEmail):
		return refusal(AuthEmailChangeErrorCodeInvalidEmail, authEmailChangeInvalidEmailMessage)
	case errors.Is(err, model.ErrEmailChangeSameEmail):
		return refusal(AuthEmailChangeErrorCodeSameEmail, authEmailChangeSameEmailMessage)
	case errors.Is(err, model.ErrEmailChangeEmailInUse):
		return refusal(AuthEmailChangeErrorCodeEmailInUse, authEmailChangeEmailInUseMessage)
	case errors.Is(err, model.ErrEmailChangeNoChange):
		return refusal(AuthEmailChangeErrorCodeNoChange, authEmailChangeNoChangeMessage)
	case errors.Is(err, model.ErrEmailChangeCodeInvalid):
		return refusal(AuthEmailChangeErrorCodeCodeInvalid, authEmailChangeCodeInvalidMessage)
	case errors.Is(err, model.ErrEmailChangeTooManyTries):
		return refusal(AuthEmailChangeErrorCodeTooManyTries, authEmailChangeTooManyTriesMessage)
	case errors.As(err, &rateLimit):
		return &AuthEmailChangeError{
			Code:              AuthEmailChangeErrorCodeRateLimited,
			Message:           strings.TrimPrefix(err.Error(), "429 "),
			RetryAfterSeconds: rateLimit.RetryAfterSeconds(),
		}, nil
	default:
		return nil, err
	}
}

func authEmailChangeOwnerOnly() *AuthEmailChangeError {
	return &AuthEmailChangeError{Code: AuthEmailChangeErrorCodeOwnerOnly, Message: authEmailChangeOwnerOnlyMessage}
}

// The code email of an email change: to the current address, which authorizes
// the change, or to the new one, which proves it receives mail.
type AuthEmailChangeCodeTemplate struct {
	Code string
	// how long the code works, in minutes
	Minutes int
	// sent to the current address; false for the new one
	Current bool
	// the other address of the change, masked
	OtherEmail string
	BaseTemplate
}

func (self *AuthEmailChangeCodeTemplate) Name() string {
	return "auth_email_change_code"
}

func AuthEmailChange(args *AuthEmailChangeArgs, clientSession *session.ClientSession) (*AuthEmailChangeResult, error) {
	return authEmailChange(args, clientSession, model.DefaultEmailChangeSettings(), GetAWSMessageSender())
}

func authEmailChange(args *AuthEmailChangeArgs, clientSession *session.ClientSession, settings *model.EmailChangeSettings, messageSender MessageSender) (*AuthEmailChangeResult, error) {
	if !accountOwnerSession(clientSession) {
		return &AuthEmailChangeResult{Error: authEmailChangeOwnerOnly()}, nil
	}
	newEmail := ""
	if args != nil {
		newEmail = args.NewEmail
	}
	start, err := model.StartEmailChange(clientSession.Ctx, settings, clientSession.ByJwt.UserId, clientSession.ByJwt.NetworkId, newEmail)
	if err != nil {
		refusal, err := authEmailChangeRefusal(err)
		if err != nil {
			return nil, err
		}
		return &AuthEmailChangeResult{Error: refusal}, nil
	}
	minutes := int(settings.CodeTtl.Minutes())
	maskedCurrent := model.MaskEmailAddress(start.CurrentEmail)
	maskedNew := model.MaskEmailAddress(start.NewEmail)
	sends := []struct {
		address  string
		template *AuthEmailChangeCodeTemplate
	}{
		{start.CurrentEmail, &AuthEmailChangeCodeTemplate{Code: start.CurrentCode, Minutes: minutes, Current: true, OtherEmail: maskedNew}},
		{start.NewEmail, &AuthEmailChangeCodeTemplate{Code: start.NewCode, Minutes: minutes, Current: false, OtherEmail: maskedCurrent}},
	}
	for _, send := range sends {
		if err := messageSender.SendAccountMessageTemplate(send.address, send.template); err != nil {
			if glog.V(1) {
				glog.Infof("[auth]email change code send failed: %s\n", err)
			}
			// both codes are needed: without one the change cannot complete
			model.CancelEmailChange(clientSession.Ctx, clientSession.ByJwt.UserId, clientSession.ByJwt.NetworkId)
			return &AuthEmailChangeResult{Error: &AuthEmailChangeError{Code: AuthEmailChangeErrorCodeSendFailed, Message: authEmailChangeSendFailedMessage}}, nil
		}
	}
	return &AuthEmailChangeResult{
		CurrentEmail:    maskedCurrent,
		NewEmail:        start.NewEmail,
		ExpiresAtMillis: start.ExpireTime.UnixMilli(),
	}, nil
}

func AuthEmailChangeStatus(clientSession *session.ClientSession) (*AuthEmailChangeStatusResult, error) {
	if !accountOwnerSession(clientSession) {
		return &AuthEmailChangeStatusResult{Error: authEmailChangeOwnerOnly()}, nil
	}
	state := model.GetEmailChange(clientSession.Ctx, clientSession.ByJwt.UserId, clientSession.ByJwt.NetworkId)
	if state == nil {
		return &AuthEmailChangeStatusResult{Pending: false}, nil
	}
	return &AuthEmailChangeStatusResult{
		Pending:          true,
		CurrentEmail:     model.MaskEmailAddress(state.CurrentEmail),
		NewEmail:         state.NewEmail,
		CurrentConfirmed: state.CurrentConfirmed,
		NewConfirmed:     state.NewConfirmed,
		ExpiresAtMillis:  state.ExpireTime.UnixMilli(),
	}, nil
}

// Signs out the account's other sessions after a completed change, keeping
// the current one: the same operation as Settings > Sessions' "sign out all
// others", under an id derived from the change so a retried confirmation
// finds it. False when it could not: the current sign-in has no session to
// keep (a token from before sessions), or the operation was refused.
type authEmailChangeSignOutOthers func(clientSession *session.ClientSession, change *model.EmailChangeConfirm) bool

func authEmailChangeSessionSignOut(clientSession *session.ClientSession, change *model.EmailChangeConfirm) bool {
	if clientSession.ByJwt.SessionId == nil {
		return false
	}
	_, err := session.RevokeOtherNetworkSessions(
		&session.RevokeOtherSessionsArgs{OperationId: model.EmailChangeSignOutOperationId(clientSession.ByJwt.UserId, change.RequestId)},
		clientSession,
	)
	if err != nil {
		glog.Infof("[auth]email change of network %s: the other sessions were not signed out: %s\n", clientSession.ByJwt.NetworkId, err)
		return false
	}
	return true
}

func AuthEmailChangeConfirm(args *AuthEmailChangeConfirmArgs, clientSession *session.ClientSession) (*AuthEmailChangeConfirmResult, error) {
	return authEmailChangeConfirm(args, clientSession, model.DefaultEmailChangeSettings(), authEmailChangeSessionSignOut)
}

func authEmailChangeConfirm(args *AuthEmailChangeConfirmArgs, clientSession *session.ClientSession, settings *model.EmailChangeSettings, signOutOthers authEmailChangeSignOutOthers) (*AuthEmailChangeConfirmResult, error) {
	if !accountOwnerSession(clientSession) {
		return &AuthEmailChangeConfirmResult{Error: authEmailChangeOwnerOnly()}, nil
	}
	if args == nil {
		args = &AuthEmailChangeConfirmArgs{}
	}
	confirm, err := model.ConfirmEmailChange(clientSession.Ctx, settings, clientSession.ByJwt.UserId, clientSession.ByJwt.NetworkId, args.CurrentCode, args.NewCode)
	result := &AuthEmailChangeConfirmResult{}
	if confirm != nil {
		result.CurrentConfirmed = confirm.CurrentConfirmed
		result.NewConfirmed = confirm.NewConfirmed
		result.Complete = confirm.Complete
		result.Email = confirm.Email
	}
	if err != nil {
		refusal, err := authEmailChangeRefusal(err)
		if err != nil {
			return nil, err
		}
		result.Error = refusal
		return result, nil
	}
	if confirm.Complete {
		glog.Infof("[auth]email changed network_id=%s user_id=%s\n", clientSession.ByJwt.NetworkId, clientSession.ByJwt.UserId)
		result.OtherSessionsSignedOut = signOutOthers(clientSession, confirm)
	}
	return result, nil
}

func AuthEmailChangeCancel(args *AuthEmailChangeCancelArgs, clientSession *session.ClientSession) (*AuthEmailChangeCancelResult, error) {
	if !accountOwnerSession(clientSession) {
		return &AuthEmailChangeCancelResult{Error: authEmailChangeOwnerOnly()}, nil
	}
	model.CancelEmailChange(clientSession.Ctx, clientSession.ByJwt.UserId, clientSession.ByJwt.NetworkId)
	return &AuthEmailChangeCancelResult{}, nil
}
