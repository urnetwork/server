package controller

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Password reset code sends with a fake code creator (standing in for the auth
// attempt limiter) and a fake message sender. No database, redis or network.
//
// Root cause: /auth/password-reset ignored the message sender's error and
// answered 200, so the apps said a reset code was sent when it was not. The
// typed error is the one /auth/verify-send uses.

func resetCreateCodeOk(model.AuthPasswordResetCreateCodeArgs, *session.ClientSession) (*model.AuthPasswordResetCreateCodeResult, error) {
	resetCode := "reset123"
	return &model.AuthPasswordResetCreateCodeResult{
		ResetCode: &resetCode,
	}, nil
}

func resetCreateCodeRateLimited(args model.AuthPasswordResetCreateCodeArgs, _ *session.ClientSession) (*model.AuthPasswordResetCreateCodeResult, error) {
	return nil, model.Testing_MaxUserAuthAttemptsError(&args.UserAuth)
}

func TestAuthPasswordResetReportsSendFailure(t *testing.T) {
	sender := &fakeVerifySendMessageSender{err: errors.New("ses: throttled")}
	result, err := authPasswordReset(
		AuthPasswordResetArgs{UserAuth: verifySendTestUserAuth, ResultErrors: true},
		verifySendTestSession(),
		resetCreateCodeOk,
		sender,
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if sender.sendCount != 1 {
		t.Fatalf("send count = %d, want 1", sender.sendCount)
	}
	if result.Error == nil {
		t.Fatalf("a failed reset code send was reported as sent: %+v", result)
	}
	if result.Error.Code != model.AuthVerifySendErrorCodeSendFailed {
		t.Fatalf("code = %q, want %q", result.Error.Code, model.AuthVerifySendErrorCodeSendFailed)
	}
	if result.Error.Message == "" || strings.Contains(result.Error.Message, "ses:") {
		t.Fatalf("message = %q, want a client message without provider detail", result.Error.Message)
	}
}

func TestAuthPasswordResetReportsRateLimit(t *testing.T) {
	sender := &fakeVerifySendMessageSender{}
	result, err := authPasswordReset(
		AuthPasswordResetArgs{UserAuth: verifySendTestUserAuth, ResultErrors: true},
		verifySendTestSession(),
		resetCreateCodeRateLimited,
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

func TestAuthPasswordResetSent(t *testing.T) {
	sender := &fakeVerifySendMessageSender{}
	result, err := authPasswordReset(
		AuthPasswordResetArgs{UserAuth: verifySendTestUserAuth, ResultErrors: true},
		verifySendTestSession(),
		resetCreateCodeOk,
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

// Older clients do not send `result_errors`: the public handler keeps the 429
// for a rate limit and turns a failed send into a 502, as /auth/verify-send.
func TestAuthPasswordResetLegacyStatus(t *testing.T) {
	sender := &fakeVerifySendMessageSender{err: errors.New("ses: throttled")}
	result, err := authPasswordReset(
		AuthPasswordResetArgs{UserAuth: verifySendTestUserAuth},
		verifySendTestSession(),
		resetCreateCodeOk,
		sender,
	)
	if err != nil || result.Error == nil {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if legacyErr := legacyAuthVerifySendError(result.Error); !strings.HasPrefix(legacyErr.Error(), "502 ") {
		t.Fatalf("send failed = %v", legacyErr)
	}
}
