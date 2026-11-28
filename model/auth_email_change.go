package model

// The verified email change of an account (support inbox 33, 6446). An
// account could not change its email: a second email sign-in is refused (one
// per account), and the last sign-in method cannot be removed, so the only
// way was a new account.
//
// The network owner asks for the change with the new address. One code goes
// to the current email sign-in and another to the new address (an
// account_code request of two slots, account_code.go), and the change
// completes only when both are confirmed before the request expires: the
// current address authorizes it (the re-auth a stolen session alone cannot
// pass) and the new address proves it can receive mail. The email sign-in
// keeps its password; the account's own email (network_user.user_auth, where
// account emails go) follows it when it was the same address.
//
// The new address is claimed with the proof of its code (auth_user_auth_claim.go):
// a pending hold of another account (an unverified sign-up or add) yields, and
// a proven one refuses the change. The availability is checked when the
// change is asked for, and again in the transaction that makes it.
//
// Both steps owe the sign-in method security notice in their transactions
// (security_notice_model.go): the request to the account's sign-in addresses,
// and the change to those addresses before and after it, so the old address
// hears of it too. Any password reset or verification code still waiting for
// the account stops working with the change. The controller signs out the
// account's other sessions after it commits.

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// The purpose of the change's codes (account_code.go), and their slots.
const (
	accountCodePurposeEmailChange = "email_change"
	emailChangeSlotCurrent        = 0
	emailChangeSlotNew            = 1
	emailChangeSlots              = 2
)

// The request's own fields.
const (
	emailChangeNetworkIdField    = "network_id"
	emailChangeCurrentEmailField = "current_email"
	emailChangeNewEmailField     = "new_email"
)

// The account action budgets of the change (account_action_rate_limit.go): the
// changes an account asks for, and the codes sent to one address.
const (
	AccountActionEmailChangeRequest  = "email_change_request"
	AccountActionEmailChangeCodeSend = "email_change_code_send"
)

var (
	// the session's user does not administer the network
	ErrEmailChangeOwnerOnly = errors.New("only the network's own sign-in can change its email")
	// the account has no email sign-in to change
	ErrEmailChangeNoEmail = errors.New("the account has no email sign-in")
	// the current email sign-in is not verified
	ErrEmailChangeEmailUnverified = errors.New("the current email sign-in is not verified")
	ErrEmailChangeInvalidEmail    = errors.New("the new email is not an email address")
	ErrEmailChangeSameEmail       = errors.New("the new email is the current email")
	// another account holds the new email
	ErrEmailChangeEmailInUse = errors.New("another account signs in with the new email")
	// no change is waiting: it expired, was cancelled, completed or never started
	ErrEmailChangeNoChange = errors.New("no email change is waiting")
	// a code is wrong; the change is still waiting
	ErrEmailChangeCodeInvalid = errors.New("a code is wrong")
	// too many wrong codes ended the change
	ErrEmailChangeTooManyTries = errors.New("too many wrong codes ended the email change")
)

type EmailChangeSettings struct {
	// how long a change waits for its two codes. Both must be confirmed in it;
	// half an hour leaves time to open two inboxes, and bounds what a lost
	// code is worth.
	CodeTtl time.Duration
	// wrong tries a change takes across both codes; the last ends it. Five,
	// as a verification code takes (auth_verify_code_limit.go).
	TryLimit int
	// changes an account asks for in RequestWindow. Each sends two codes, so
	// this also bounds the mail the account can make and the guesses (TryLimit
	// for each).
	RequestLimit  int
	RequestWindow time.Duration
	// codes sent to one address (as an hmac of the address) in
	// CodeSendWindow, across every account: no account can use the change to
	// fill someone else's inbox.
	CodeSendLimit  int
	CodeSendWindow time.Duration
}

func DefaultEmailChangeSettings() *EmailChangeSettings {
	return &EmailChangeSettings{
		CodeTtl:        30 * time.Minute,
		TryLimit:       5,
		RequestLimit:   3,
		RequestWindow:  24 * time.Hour,
		CodeSendLimit:  5,
		CodeSendWindow: 24 * time.Hour,
	}
}

// A change that was asked for: the codes to send, never stored.
type EmailChangeStart struct {
	RequestId    server.Id
	CurrentEmail string
	NewEmail     string
	CurrentCode  string
	NewCode      string
	ExpireTime   time.Time
}

// A waiting change, as the status reads it.
type EmailChangeState struct {
	RequestId        server.Id
	CurrentEmail     string
	NewEmail         string
	CurrentConfirmed bool
	NewConfirmed     bool
	ExpireTime       time.Time
}

// The outcome of a confirmation.
type EmailChangeConfirm struct {
	RequestId        server.Id
	CurrentConfirmed bool
	NewConfirmed     bool
	// both addresses are confirmed and the email sign-in is Email
	Complete bool
	Email    string
}

// A refusal inside a transaction, raised so the transaction rolls back what it
// recorded (the budgets it spent) and recovered as the refusal.
type emailChangeAbort struct {
	cause error
}

// The admin's email sign-in, read in the caller's transaction and locked
// against a concurrent change: the address and whether it is verified, or
// found false.
func emailSignInForUpdateInTx(ctx context.Context, tx server.PgTx, userId server.Id) (userAuth string, verified bool, found bool) {
	result, err := tx.Query(
		ctx,
		`
			SELECT user_auth, verified
			FROM network_user_auth_password
			WHERE user_id = $1 AND auth_type = $2
			FOR UPDATE
		`,
		userId,
		UserAuthTypeEmail,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&userAuth, &verified))
			found = true
		}
	})
	return
}

// An email address as the apps and the notices show it ("j…@example.com").
func MaskEmailAddress(emailAddress string) string {
	return maskEmailAddress(emailAddress)
}

// The normalized new address, or the refusal.
func normalEmailChangeAddress(newEmail string) (string, error) {
	normalUserAuth, userAuthType := NormalUserAuth(strings.TrimSpace(newEmail))
	if userAuthType != UserAuthTypeEmail || normalUserAuth == "" {
		return "", ErrEmailChangeInvalidEmail
	}
	return normalUserAuth, nil
}

// Asks for the change of the network owner's email sign-in to newEmail: checks
// the owner, the current sign-in and the new address, spends the budgets,
// owes the request's security notice and stores the codes, which the caller
// sends. A new request replaces the waiting one.
func StartEmailChange(ctx context.Context, settings *EmailChangeSettings, userId server.Id, networkId server.Id, newEmail string) (start *EmailChangeStart, returnErr error) {
	newUserAuth, err := normalEmailChangeAddress(newEmail)
	if err != nil {
		return nil, err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if abort, ok := recovered.(emailChangeAbort); ok {
				start, returnErr = nil, abort.cause
			} else {
				panic(recovered)
			}
		}
	}()
	var currentUserAuth string
	server.Tx(ctx, func(tx server.PgTx) {
		refuse := func(err error) {
			panic(emailChangeAbort{cause: err})
		}
		if !networkAdminInTx(ctx, tx, userId, networkId) {
			refuse(ErrEmailChangeOwnerOnly)
		}
		userAuth, verified, found := emailSignInForUpdateInTx(ctx, tx, userId)
		switch {
		case !found:
			refuse(ErrEmailChangeNoEmail)
		case !verified:
			refuse(ErrEmailChangeEmailUnverified)
		case newUserAuth == userAuth:
			refuse(ErrEmailChangeSameEmail)
		}
		currentUserAuth = userAuth
		// read-only here: the holds are released only when the change is made
		if _, err := userAuthClaimInTx(ctx, tx, newUserAuth, userId, true); err != nil {
			refuse(ErrEmailChangeEmailInUse)
		}
		allow, retryAfter := checkAndRecordAccountActionRateLimitInTx(ctx, tx, userId, AccountActionEmailChangeRequest, settings.RequestLimit, settings.RequestWindow)
		if !allow {
			refuse(&rateLimitError{
				message:           "429 " + maxAccountActionAttemptsRetryError(AccountActionEmailChangeRequest, settings.RequestWindow, retryAfter).Error(),
				retryAfterSeconds: int((retryAfter + time.Second - 1) / time.Second),
			})
		}
		for _, address := range []string{currentUserAuth, newUserAuth} {
			if testAuthPolicyForUserAuth(&address).BypassRateLimits {
				continue
			}
			allow, retryAfter := checkAndRecordAccountActionRateLimitInTx(ctx, tx, userAuthCodeSendBudgetId(address), AccountActionEmailChangeCodeSend, settings.CodeSendLimit, settings.CodeSendWindow)
			if !allow {
				refuse(maxUserAuthCodeSendsError(retryAfter))
			}
		}
		watch := watchSignInMethodsInTx(ctx, tx, userId)
		change := newSecuritySignInChange(ctx, SecuritySignInChangeRequested, SecuritySignInMethodEmail, maskEmailAddress(currentUserAuth))
		change.NewIdentity = maskEmailAddress(newUserAuth)
		watch.noticeInTx(ctx, tx, change)
	})
	request := newAccountCodeRequest(accountCodePurposeEmailChange, userId, emailChangeSlots, server.NowUtc().Add(settings.CodeTtl), map[string]string{
		emailChangeNetworkIdField:    networkId.String(),
		emailChangeCurrentEmailField: currentUserAuth,
		emailChangeNewEmailField:     newUserAuth,
	})
	putAccountCodeRequest(ctx, request)
	return &EmailChangeStart{
		RequestId:    request.Id,
		CurrentEmail: currentUserAuth,
		NewEmail:     newUserAuth,
		CurrentCode:  request.Codes[emailChangeSlotCurrent],
		NewCode:      request.Codes[emailChangeSlotNew],
		ExpireTime:   request.ExpireTime,
	}, nil
}

// The owner's waiting change of the network, or nil.
func GetEmailChange(ctx context.Context, userId server.Id, networkId server.Id) *EmailChangeState {
	state := getAccountCodeRequest(ctx, accountCodePurposeEmailChange, userId, emailChangeSlots)
	if state == nil || state.Fields[emailChangeNetworkIdField] != networkId.String() {
		return nil
	}
	return &EmailChangeState{
		RequestId:        state.Id,
		CurrentEmail:     state.Fields[emailChangeCurrentEmailField],
		NewEmail:         state.Fields[emailChangeNewEmailField],
		CurrentConfirmed: state.Confirmed[emailChangeSlotCurrent],
		NewConfirmed:     state.Confirmed[emailChangeSlotNew],
		ExpireTime:       state.ExpireTime,
	}
}

// Ends the owner's waiting change, if any. Its codes stop working.
func CancelEmailChange(ctx context.Context, userId server.Id, networkId server.Id) {
	if state := GetEmailChange(ctx, userId, networkId); state != nil {
		removeAccountCodeRequest(ctx, accountCodePurposeEmailChange, userId, state.RequestId)
	}
}

// Confirms the waiting change with the code sent to the current address, the
// code sent to the new one, or both. When both are confirmed the change is
// made (completeEmailChange). A wrong code answers ErrEmailChangeCodeInvalid
// with the confirmations it left; the try that reaches the limit ends the
// change.
func ConfirmEmailChange(ctx context.Context, settings *EmailChangeSettings, userId server.Id, networkId server.Id, currentCode string, newCode string) (*EmailChangeConfirm, error) {
	currentCode = normalizeAccountCode(currentCode)
	newCode = normalizeAccountCode(newCode)
	state := GetEmailChange(ctx, userId, networkId)
	if state == nil {
		return nil, ErrEmailChangeNoChange
	}
	confirm := &EmailChangeConfirm{
		RequestId:        state.RequestId,
		CurrentConfirmed: state.CurrentConfirmed,
		NewConfirmed:     state.NewConfirmed,
	}
	if currentCode != "" || newCode != "" {
		candidates := make([]string, emailChangeSlots)
		candidates[emailChangeSlotCurrent] = currentCode
		candidates[emailChangeSlotNew] = newCode
		tried := tryAccountCodeRequest(ctx, accountCodePurposeEmailChange, userId, state.RequestId, candidates, settings.TryLimit)
		switch {
		case tried.Missing:
			return nil, ErrEmailChangeNoChange
		case tried.Spent:
			return nil, ErrEmailChangeTooManyTries
		}
		confirm.CurrentConfirmed = tried.Confirmed[emailChangeSlotCurrent]
		confirm.NewConfirmed = tried.Confirmed[emailChangeSlotNew]
		if tried.Wrong {
			return confirm, ErrEmailChangeCodeInvalid
		}
	}
	if !confirm.CurrentConfirmed || !confirm.NewConfirmed {
		return confirm, nil
	}
	if err := completeEmailChange(ctx, userId, networkId, state); err != nil {
		if errors.Is(err, ErrEmailChangeEmailInUse) || errors.Is(err, ErrEmailChangeNoChange) {
			// the change cannot be made any more: it ends
			removeAccountCodeRequest(ctx, accountCodePurposeEmailChange, userId, state.RequestId)
		}
		return confirm, err
	}
	removeAccountCodeRequest(ctx, accountCodePurposeEmailChange, userId, state.RequestId)
	confirm.Complete = true
	confirm.Email = state.NewEmail
	return confirm, nil
}

// Makes the confirmed change in one transaction. A change already made (a
// concurrent confirmation won) completes again without another write.
func completeEmailChange(ctx context.Context, userId server.Id, networkId server.Id, state *EmailChangeState) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = nil
		if !networkAdminInTx(ctx, tx, userId, networkId) {
			returnErr = ErrEmailChangeOwnerOnly
			return
		}
		// the account row first, the order a claim locks in
		// (userAuthHoldsInTx)
		server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM network_user WHERE user_id = $1 FOR UPDATE`, userId))
		userAuth, _, found := emailSignInForUpdateInTx(ctx, tx, userId)
		switch {
		case found && userAuth == state.NewEmail:
			// already changed
			return
		case !found || userAuth != state.CurrentEmail:
			// the sign-in changed since the request: it no longer applies
			returnErr = ErrEmailChangeNoChange
			return
		}
		watch := watchSignInMethodsInTx(ctx, tx, userId)
		// the new address's code proves it: a pending hold yields
		if err := claimUserAuthInTx(ctx, tx, state.NewEmail, userId, true); err != nil {
			returnErr = ErrEmailChangeEmailInUse
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_user_auth_password
				SET
					user_auth = $3,
					verified = true
				WHERE user_id = $1 AND auth_type = $2 AND user_auth = $4
			`,
			userId,
			UserAuthTypeEmail,
			state.NewEmail,
			state.CurrentEmail,
		))
		// the account's own email follows when it was the same address
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_user SET user_auth = $2 WHERE user_id = $1 AND user_auth = $3`,
			userId,
			state.NewEmail,
			state.CurrentEmail,
		))
		// the codes still waiting for the old address stop working
		server.RaisePgResult(tx.Exec(ctx, `UPDATE user_auth_reset SET used = true WHERE user_id = $1 AND NOT used`, userId))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE user_auth_verify SET used = true WHERE user_id = $1 AND NOT used`, userId))
		change := newSecuritySignInChange(ctx, SecuritySignInChanged, SecuritySignInMethodEmail, maskEmailAddress(state.CurrentEmail))
		change.NewIdentity = maskEmailAddress(state.NewEmail)
		watch.noticeInTx(ctx, tx, change)
	})
	if returnErr == nil {
		// the new address is verified: its strikes and lockout end
		verifyCodeAddressOf(state.NewEmail).clear(ctx)
	}
	return
}

// The id the session sign-out of a completed change runs under: derived from
// the request, so a retried confirmation finds the same operation.
func EmailChangeSignOutOperationId(userId server.Id, requestId server.Id) server.Id {
	mac, err := hex.DecodeString(accountCodeMac(accountCodePurposeEmailChange, userId, requestId, emailChangeSlots, "sign-out"))
	server.Raise(err)
	var operationId server.Id
	copy(operationId[:], mac)
	return operationId
}
