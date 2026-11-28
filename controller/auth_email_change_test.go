package controller

// The email change routes' gate, refusals, code emails and notices, without a
// database.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Every model refusal has its code, a rate limit carries its wait, and any
// other error stays an error.
func TestAuthEmailChangeRefusals(t *testing.T) {
	for _, c := range []struct {
		err  error
		code string
	}{
		{model.ErrEmailChangeOwnerOnly, AuthEmailChangeErrorCodeOwnerOnly},
		{model.ErrEmailChangeNoEmail, AuthEmailChangeErrorCodeNoEmail},
		{model.ErrEmailChangeEmailUnverified, AuthEmailChangeErrorCodeEmailUnverified},
		{model.ErrEmailChangeInvalidEmail, AuthEmailChangeErrorCodeInvalidEmail},
		{model.ErrEmailChangeSameEmail, AuthEmailChangeErrorCodeSameEmail},
		{model.ErrEmailChangeEmailInUse, AuthEmailChangeErrorCodeEmailInUse},
		{model.ErrEmailChangeNoChange, AuthEmailChangeErrorCodeNoChange},
		{model.ErrEmailChangeCodeInvalid, AuthEmailChangeErrorCodeCodeInvalid},
		{model.ErrEmailChangeTooManyTries, AuthEmailChangeErrorCodeTooManyTries},
	} {
		refusal, err := authEmailChangeRefusal(fmt.Errorf("wrapped: %w", c.err))
		if err != nil || refusal == nil || refusal.Code != c.code || refusal.Message == "" {
			t.Errorf("%v: refusal = %+v, err = %v, want %s", c.err, refusal, err, c.code)
		}
	}
	rateLimit := &verifySendStatusError{message: "429 Too many codes.", retryAfterSeconds: 3600}
	refusal, err := authEmailChangeRefusal(rateLimit)
	if err != nil || refusal.Code != AuthEmailChangeErrorCodeRateLimited || refusal.RetryAfterSeconds != 3600 || strings.HasPrefix(refusal.Message, "429") {
		t.Fatalf("rate limit: refusal = %+v, err = %v", refusal, err)
	}
	other := errors.New("database unavailable")
	if refusal, err := authEmailChangeRefusal(other); refusal != nil || !errors.Is(err, other) {
		t.Fatalf("another error: refusal = %+v, err = %v", refusal, err)
	}
}

// An API key acts as the network but is not its owner: every step refuses it
// before the model or Redis is read, and sends nothing.
func TestAuthEmailChangeRefusesAnApiKeyBeforeReading(t *testing.T) {
	clientSession := session.Testing_CreateClientSession(context.Background(), &session.ByJwt{NetworkId: server.NewId(), UserId: server.NewId()})
	clientSession.ApiKeyAuthenticated = true
	sender := &emailChangeTestSender{}

	started, err := authEmailChange(&AuthEmailChangeArgs{NewEmail: "new@example.com"}, clientSession, model.DefaultEmailChangeSettings(), sender)
	if err != nil || started.Error == nil || started.Error.Code != AuthEmailChangeErrorCodeOwnerOnly {
		t.Fatalf("start: %+v %v", started, err)
	}
	status, err := AuthEmailChangeStatus(clientSession)
	if err != nil || status.Error == nil || status.Error.Code != AuthEmailChangeErrorCodeOwnerOnly || status.Pending {
		t.Fatalf("status: %+v %v", status, err)
	}
	signOuts := 0
	confirmed, err := authEmailChangeConfirm(&AuthEmailChangeConfirmArgs{CurrentCode: "1", NewCode: "2"}, clientSession, model.DefaultEmailChangeSettings(), func(*session.ClientSession, *model.EmailChangeConfirm) bool {
		signOuts += 1
		return true
	})
	if err != nil || confirmed.Error == nil || confirmed.Error.Code != AuthEmailChangeErrorCodeOwnerOnly || confirmed.Complete || signOuts != 0 {
		t.Fatalf("confirm: %+v %v", confirmed, err)
	}
	cancelled, err := AuthEmailChangeCancel(&AuthEmailChangeCancelArgs{}, clientSession)
	if err != nil || cancelled.Error == nil || cancelled.Error.Code != AuthEmailChangeErrorCodeOwnerOnly {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	if len(sender.sends) != 0 {
		t.Fatalf("an api key sent %d codes", len(sender.sends))
	}
}

// The code emails say which side of the change their address is, name the
// other address masked, and tell the current address what to do if the
// change is not theirs.
func TestAuthEmailChangeCodeEmails(t *testing.T) {
	_, _, currentText, err := RenderEmailTemplate(&AuthEmailChangeCodeTemplate{Code: "482917", Minutes: 30, Current: true, OtherEmail: "n…@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"482917", "from\nthis address to n…@example.com", "30 minutes", "Don't share the code", "https://ur.io/app/account/sessions", "security@ur.io"} {
		if !strings.Contains(currentText, want) {
			t.Errorf("the current address's email lacks %q:\n%s", want, currentText)
		}
	}
	_, _, newText, err := RenderEmailTemplate(&AuthEmailChangeCodeTemplate{Code: "193746", Minutes: 30, Current: false, OtherEmail: "j…@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"193746", "make this address the email", "Nothing happens without the code"} {
		if !strings.Contains(newText, want) {
			t.Errorf("the new address's email lacks %q:\n%s", want, newText)
		}
	}
	if strings.Contains(newText, "482917") {
		t.Fatal("the new address's email carries the current address's code")
	}
}

// The sign-in notice of an email change names the old and the new address in
// its change line, and its subject says what happened.
func TestSecuritySignInNoticeSaysTheEmailChange(t *testing.T) {
	for _, c := range []struct {
		action   string
		subject  string
		headline string
		change   string
	}{
		{model.SecuritySignInChangeRequested, "A change of your URnetwork account's email was requested", "EMAIL CHANGE REQUESTED", "Change: Change to n…@example.com requested"},
		{model.SecuritySignInChanged, "Your URnetwork account's email was changed", "EMAIL CHANGED", "Change: Changed to n…@example.com"},
	} {
		notice := &SecuritySignInChangedTemplate{SecuritySignInNotice: model.SecuritySignInNotice{
			Changes: []*model.SecuritySignInChange{{
				ChangeTime:  time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC),
				Action:      c.action,
				Method:      model.SecuritySignInMethodEmail,
				Identity:    "j…@example.com",
				NewIdentity: "n…@example.com",
				Via:         model.SecurityNoticeViaAndroid,
			}},
		}}
		subject, _, bodyText := securityNoticeText(t, notice)
		if subject != c.subject {
			t.Errorf("%s: subject %q, want %q", c.action, subject, c.subject)
		}
		for _, want := range []string{c.headline, "Sign-in method: Email (j…@example.com)", c.change, securityNoticeContactLine} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("%s: text lacks %q:\n%s", c.action, want, bodyText)
			}
		}
	}
}

// A sender that records what it sends.
type emailChangeTestSender struct {
	sends []emailChangeTestSend
	fail  func(userAuth string) bool
}

type emailChangeTestSend struct {
	userAuth string
	template Template
}

func (self *emailChangeTestSender) SendAccountMessageTemplate(userAuth string, template Template, sendOpts ...any) error {
	if self.fail != nil && self.fail(userAuth) {
		return errors.New("send failed")
	}
	self.sends = append(self.sends, emailChangeTestSend{userAuth: userAuth, template: template})
	return nil
}
