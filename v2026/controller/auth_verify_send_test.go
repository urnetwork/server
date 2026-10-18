package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Verification code sends with a fake code creator (standing in for the auth
// attempt limiter) and a fake message sender. No database, redis or network.
//
// Root cause: a rate-limit refusal or a send failure while sending a code was
// dropped. /auth/verify-send answered 200 after a failed send, and login with
// password and sign-up ignored the send result entirely, so the apps said a
// code was sent when it was not.

const verifySendTestUserAuth = "verify-send@example.com"

type fakeVerifySendMessageSender struct {
	err       error
	sendCount int
}

func (self *fakeVerifySendMessageSender) SendAccountMessageTemplate(userAuth string, template Template, sendOpts ...any) error {
	self.sendCount += 1
	return self.err
}

func verifySendTestSession() *session.ClientSession {
	return session.Testing_CreateClientSession(context.Background(), nil)
}

func createCodeOk(model.AuthVerifyCreateCodeArgs, *session.ClientSession) (*model.AuthVerifyCreateCodeResult, error) {
	verifyCode := "abc123"
	return &model.AuthVerifyCreateCodeResult{
		VerifyCode: &verifyCode,
	}, nil
}

func createCodeRateLimited(args model.AuthVerifyCreateCodeArgs, _ *session.ClientSession) (*model.AuthVerifyCreateCodeResult, error) {
	return nil, model.Testing_MaxUserAuthAttemptsError(&args.UserAuth)
}

func TestAuthVerifySendReportsSendFailure(t *testing.T) {
	sender := &fakeVerifySendMessageSender{err: errors.New("ses: throttled")}
	result, err := authVerifySend(
		AuthVerifySendArgs{UserAuth: verifySendTestUserAuth},
		verifySendTestSession(),
		createCodeOk,
		sender,
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if sender.sendCount != 1 {
		t.Fatalf("send count = %d, want 1", sender.sendCount)
	}
	if result.Error == nil {
		t.Fatalf("a failed send was reported as sent: %+v", result)
	}
	if result.Error.Code != model.AuthVerifySendErrorCodeSendFailed {
		t.Fatalf("code = %q, want %q", result.Error.Code, model.AuthVerifySendErrorCodeSendFailed)
	}
	if result.Error.Message == "" || strings.Contains(result.Error.Message, "ses:") {
		t.Fatalf("message = %q, want a client message without provider detail", result.Error.Message)
	}
}

func TestAuthVerifySendReportsRateLimit(t *testing.T) {
	sender := &fakeVerifySendMessageSender{}
	result, err := authVerifySend(
		AuthVerifySendArgs{UserAuth: verifySendTestUserAuth},
		verifySendTestSession(),
		createCodeRateLimited,
		sender,
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if sender.sendCount != 0 {
		t.Fatalf("send count = %d, want 0", sender.sendCount)
	}
	if result.Error == nil || result.Error.Code != model.AuthVerifySendErrorCodeRateLimited {
		t.Fatalf("error = %+v, want %q", result.Error, model.AuthVerifySendErrorCodeRateLimited)
	}
	if want := int(model.AttemptLookback.Seconds()); result.Error.RetryAfterSeconds != want {
		t.Fatalf("retry after = %d, want %d", result.Error.RetryAfterSeconds, want)
	}
	if strings.HasPrefix(result.Error.Message, "429") {
		t.Fatalf("message keeps the status prefix: %q", result.Error.Message)
	}
}

func TestAuthVerifySendSent(t *testing.T) {
	sender := &fakeVerifySendMessageSender{}
	result, err := authVerifySend(
		AuthVerifySendArgs{UserAuth: verifySendTestUserAuth},
		verifySendTestSession(),
		createCodeOk,
		sender,
	)
	if err != nil || result.Error != nil || sender.sendCount != 1 {
		t.Fatalf("result = %+v, err = %v, send count = %d", result, err, sender.sendCount)
	}
	// a sent code keeps the response old clients parse
	body, _ := json.Marshal(result)
	if string(body) != `{"user_auth":"verify-send@example.com"}` {
		t.Fatalf("body = %s", body)
	}
}

// Older clients do not send `result_errors`: they keep the 429 (with
// Retry-After) for a rate limit, and a failed send becomes an error status they
// already show as a failed send.
func TestAuthVerifySendLegacyStatus(t *testing.T) {
	rateLimitErr := legacyAuthVerifySendError(&AuthVerifySendError{
		Code:              model.AuthVerifySendErrorCodeRateLimited,
		Message:           "Too many.",
		RetryAfterSeconds: 300,
	})
	var retryAfter interface{ RetryAfterSeconds() int }
	if !strings.HasPrefix(rateLimitErr.Error(), "429 ") || !errors.As(rateLimitErr, &retryAfter) || retryAfter.RetryAfterSeconds() != 300 {
		t.Fatalf("rate limit = %v", rateLimitErr)
	}
	sendErr := legacyAuthVerifySendError(&AuthVerifySendError{
		Code:    model.AuthVerifySendErrorCodeSendFailed,
		Message: verifySendFailedMessage,
	})
	if !strings.HasPrefix(sendErr.Error(), "502 ") {
		t.Fatalf("send failed = %v", sendErr)
	}
}

func verificationRequiredLogin(args model.AuthLoginWithPasswordArgs, _ *session.ClientSession) (*model.AuthLoginWithPasswordResult, error) {
	networkName := "verifysend"
	return &model.AuthLoginWithPasswordResult{
		VerificationRequired: &model.AuthLoginWithPasswordResultVerification{
			UserAuth: args.UserAuth,
		},
		Network: &model.AuthLoginWithPasswordResultNetwork{
			NetworkName: &networkName,
		},
	}, nil
}

func TestAuthLoginWithPasswordReportsVerifySendFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		createCode authVerifyCreateCodeFunction
		sendErr    error
		wantCode   string
	}{
		{"send failed", createCodeOk, errors.New("ses: throttled"), model.AuthVerifySendErrorCodeSendFailed},
		{"rate limited", createCodeRateLimited, nil, model.AuthVerifySendErrorCodeRateLimited},
	} {
		t.Run(test.name, func(t *testing.T) {
			sender := &fakeVerifySendMessageSender{err: test.sendErr}
			verifySend := func(args AuthVerifySendArgs, clientSession *session.ClientSession) (*AuthVerifySendResult, error) {
				return authVerifySend(args, clientSession, test.createCode, sender)
			}
			result, err := authLoginWithPassword(
				model.AuthLoginWithPasswordArgs{UserAuth: verifySendTestUserAuth, Password: "password"},
				verifySendTestSession(),
				verificationRequiredLogin,
				verifySend,
			)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			body, _ := json.Marshal(result)
			sendError := result.VerificationRequired.SendError
			if sendError == nil || sendError.Code != test.wantCode {
				t.Fatalf("verification_required.send_error = %+v, want code %q (response %s)", sendError, test.wantCode, body)
			}
		})
	}
}

func TestAuthLoginWithPasswordVerifySent(t *testing.T) {
	sender := &fakeVerifySendMessageSender{}
	verifySend := func(args AuthVerifySendArgs, clientSession *session.ClientSession) (*AuthVerifySendResult, error) {
		return authVerifySend(args, clientSession, createCodeOk, sender)
	}
	result, err := authLoginWithPassword(
		model.AuthLoginWithPasswordArgs{UserAuth: verifySendTestUserAuth, Password: "password"},
		verifySendTestSession(),
		verificationRequiredLogin,
		verifySend,
	)
	if err != nil || result.VerificationRequired.SendError != nil || sender.sendCount != 1 {
		t.Fatalf("result = %+v, err = %v, send count = %d", result, err, sender.sendCount)
	}
}

func TestSendVerificationReportsVerifySendError(t *testing.T) {
	// sign-up shares this with login; an unexpected error also means no code
	sendError := sendVerification(verifySendTestUserAuth, false, verifySendTestSession(), func(AuthVerifySendArgs, *session.ClientSession) (*AuthVerifySendResult, error) {
		return nil, errors.New("Invalid login.")
	})
	if sendError == nil || sendError.Code != model.AuthVerifySendErrorCodeSendFailed {
		t.Fatalf("send error = %+v", sendError)
	}
}
