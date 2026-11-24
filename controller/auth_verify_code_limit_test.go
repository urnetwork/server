package controller

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// No code is sent to an address locked out by wrong codes
// (model/auth_verify_code_limit.go). The refusal reaches /auth/verify-send, and
// the sign-up and password sign-in that send a code, as a rate limit that
// names its wait, like the other send refusals; an older client gets the 429
// with its Retry-After. No database, redis or network.
func TestVerifySendReportsAnAddressLockout(t *testing.T) {
	createCodeLocked := func(model.AuthVerifyCreateCodeArgs, *session.ClientSession) (*model.AuthVerifyCreateCodeResult, error) {
		return nil, model.Testing_VerifyCodeAddressLockedError(15 * time.Minute)
	}
	sender := &fakeVerifySendMessageSender{}
	result, err := authVerifySend(
		AuthVerifySendArgs{UserAuth: verifySendTestUserAuth},
		verifySendTestSession(),
		createCodeLocked,
		sender,
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if sender.sendCount != 0 {
		t.Fatalf("send count = %d, want 0", sender.sendCount)
	}
	if result.Error == nil || result.Error.Code != model.AuthVerifySendErrorCodeRateLimited {
		t.Fatalf("a lockout was not reported as a rate limit: %+v", result.Error)
	}
	if result.Error.RetryAfterSeconds != 900 {
		t.Fatalf("retry after = %d, want 900", result.Error.RetryAfterSeconds)
	}
	if strings.HasPrefix(result.Error.Message, "429") || !strings.Contains(result.Error.Message, "15 minutes") {
		t.Fatalf("message = %q, want the wait without the status", result.Error.Message)
	}

	legacy := legacyAuthVerifySendError(result.Error)
	var rateLimit interface{ RetryAfterSeconds() int }
	if !errors.As(legacy, &rateLimit) || rateLimit.RetryAfterSeconds() != 900 || !strings.HasPrefix(legacy.Error(), "429 ") {
		t.Fatalf("legacy error = %v, want a 429 with a retry of 900 s", legacy)
	}
}
