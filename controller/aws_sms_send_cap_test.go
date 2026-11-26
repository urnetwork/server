package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The send caps of account texts (aws_sms_send_cap.go): the production sender,
// pointed at a local stand-in for Pinpoint so no test calls AWS, with the
// production caps in the test Redis and a clock the tests move.
//
// Root cause: the only limits on the texts of verification and password reset
// codes were per email or phone and per client address. Codes asked for many
// different numbers met neither, so every text of a burst reached Pinpoint, and
// unexpected high-cost SMS volume could spend the month's send budget in hours.

// The answer of the Pinpoint stand-in to a SendTextMessage call.
const (
	smsSendCapTestContentType = "application/x-amz-json-1.0"
	smsSendCapTestAnswer      = `{"MessageId":"synthetic-message-id"}`
)

// When the tests' clock starts: on the hour, a moment no window has seen.
var smsSendCapTestStart = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

// A clock the tests move by hand.
type smsSendCapTestClock struct {
	now time.Time
}

func (self *smsSendCapTestClock) Now() time.Time {
	return self.now
}

// A sender pointed at the stand-in whose texts pass the production caps, kept
// in the test Redis, reading the test clock.
func (self *awsEndpointFixture) cappedSender(clock *smsSendCapTestClock) *AWSMessageSender {
	sender := self.sender(5 * time.Second)
	sender.smsCaps = &redisSmsSendCaps{now: clock.Now}
	return sender
}

// The texts a number format makes in one country: two digits replace %02d.
var smsSendCapTestNumberFormats = map[string]string{
	"US": "+1 201-555-01%02d",
	"CA": "+1 613-555-01%02d",
	"GB": "+44 7400 1234%02d",
	"FR": "+33 6 12 34 56 %02d",
	"DE": "+49 1512 34567%02d",
	"NL": "+31 6 123456%02d",
}

// count distinct numbers in country, each checked to be in it.
func smsSendCapTestNumbers(t testing.TB, country string, count int) []string {
	format, ok := smsSendCapTestNumberFormats[country]
	if !ok || 100 < count {
		t.Fatalf("no %d test numbers in %s", count, country)
	}
	numbers := []string{}
	for i := range count {
		number := fmt.Sprintf(format, i)
		if got := smsDestinationCountry(number); got != country {
			t.Fatalf("test number %s is in %q, want %s", number, got, country)
		}
		numbers = append(numbers, number)
	}
	return numbers
}

// Sends a verification code text to number, a second of the test clock after
// the last text.
func sendSmsSendCapTestText(t testing.TB, sender *AWSMessageSender, clock *smsSendCapTestClock, number string) error {
	clock.now = clock.now.Add(time.Second)
	return sendWithGuard(t, func() error {
		return sender.SendAccountMessageTemplate(number, &AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"})
	})
}

// The refusal of err by capKind, failing the test unless err is one: a rate
// limit (the 429 status and a wait no longer than the window) to country.
func requireSmsSendCapRefusal(t testing.TB, err error, capKind string, country string) *smsSendCapRefusal {
	var refusal *smsSendCapRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("send error = %v, want a refusal by the %s send cap", err, capKind)
	}
	if refusal.capKind != capKind || refusal.country != country {
		t.Fatalf("refused by the %s cap for %s, want the %s cap for %s", refusal.capKind, refusal.country, capKind, country)
	}
	var rateLimit interface{ RetryAfterSeconds() int }
	if !strings.HasPrefix(err.Error(), "429 ") || !errors.As(err, &rateLimit) {
		t.Fatalf("refusal %q is not a rate limit", err)
	}
	if refusal.retryAfter <= 0 || AccountSmsSendCapWindow < refusal.retryAfter || rateLimit.RetryAfterSeconds() <= 0 {
		t.Fatalf("refusal waits %s (%d s), want a wait within the %s window", refusal.retryAfter, rateLimit.RetryAfterSeconds(), AccountSmsSendCapWindow)
	}
	return refusal
}

// Texts to many different numbers in one country stop at the country's cap:
// Pinpoint is asked for no text past it, and texts to other countries still go.
func TestSmsSendCapStopsTextsToManyNumbersInOneCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		endpoint := newAwsEndpoint(t, smsSendCapTestContentType, smsSendCapTestAnswer)
		clock := &smsSendCapTestClock{now: smsSendCapTestStart}
		sender := endpoint.cappedSender(clock)
		refusedBefore := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(smsSendCapCountry))

		const overCap = 5
		for i, number := range smsSendCapTestNumbers(t, "FR", AccountSmsSendCountryCap+overCap) {
			err := sendSmsSendCapTestText(t, sender, clock, number)
			if i < AccountSmsSendCountryCap {
				if err != nil {
					t.Fatalf("text %d to a new number under the country cap: send error = %v", i, err)
				}
				continue
			}
			refusal := requireSmsSendCapRefusal(t, err, smsSendCapCountry, "FR")
			// room again when the first text leaves the window
			if want := AccountSmsSendCapWindow - time.Duration(i)*time.Second; refusal.retryAfter != want {
				t.Fatalf("text %d: refusal waits %s, want %s", i, refusal.retryAfter, want)
			}
		}
		if calls := endpoint.requestCount.Load(); calls != AccountSmsSendCountryCap {
			t.Fatalf("Pinpoint was asked for %d texts, want %d: the country cap must stop the rest before the call", calls, AccountSmsSendCountryCap)
		}
		if refused := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(smsSendCapCountry)) - refusedBefore; refused != overCap {
			t.Fatalf("counted %v country cap refusals, want %d", refused, overCap)
		}

		// another country's cap is its own
		for _, number := range smsSendCapTestNumbers(t, "DE", 2) {
			if err := sendSmsSendCapTestText(t, sender, clock, number); err != nil {
				t.Fatalf("text to %s after another country's cap was spent: send error = %v", number, err)
			}
		}
		if calls := endpoint.requestCount.Load(); calls != AccountSmsSendCountryCap+2 {
			t.Fatalf("Pinpoint was asked for %d texts, want %d", calls, AccountSmsSendCountryCap+2)
		}
	})
}

// The global cap binds across countries, each under its own cap, and a text a
// cap refuses spends neither cap: the texts the country cap refused leave the
// global cap's room to texts elsewhere.
func TestSmsSendCapGlobalBindsAcrossCountries(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		endpoint := newAwsEndpoint(t, smsSendCapTestContentType, smsSendCapTestAnswer)
		clock := &smsSendCapTestClock{now: smsSendCapTestStart}
		sender := endpoint.cappedSender(clock)
		refusedBefore := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(smsSendCapGlobal))

		// a burst at one country: the country cap admits its cap and refuses
		// the rest, which must not spend the global cap
		const refusedByCountry = 5
		admitted := 0
		for i, number := range smsSendCapTestNumbers(t, "GB", AccountSmsSendCountryCap+refusedByCountry) {
			err := sendSmsSendCapTestText(t, sender, clock, number)
			if i < AccountSmsSendCountryCap {
				if err != nil {
					t.Fatalf("text %d to GB under its cap: send error = %v", i, err)
				}
				admitted += 1
				continue
			}
			requireSmsSendCapRefusal(t, err, smsSendCapCountry, "GB")
		}

		// then texts to several countries in turn, none past its own cap: the
		// global cap admits exactly its remaining room
		countries := []string{"US", "CA", "FR", "DE", "NL"}
		numbers := map[string][]string{}
		for _, country := range countries {
			numbers[country] = smsSendCapTestNumbers(t, country, 5)
		}
		globalRefusals := 0
		for i := range 5 {
			for _, country := range countries {
				err := sendSmsSendCapTestText(t, sender, clock, numbers[country][i])
				if admitted < AccountSmsSendGlobalCap {
					if err != nil {
						t.Fatalf("text %d to %s with %d of the global cap spent: send error = %v", i, country, admitted, err)
					}
					admitted += 1
					continue
				}
				requireSmsSendCapRefusal(t, err, smsSendCapGlobal, country)
				globalRefusals += 1
			}
		}
		if want := AccountSmsSendCountryCap + 5*len(countries) - AccountSmsSendGlobalCap; globalRefusals != want {
			t.Fatalf("the global cap refused %d texts, want %d", globalRefusals, want)
		}
		if calls := endpoint.requestCount.Load(); calls != AccountSmsSendGlobalCap {
			t.Fatalf("Pinpoint was asked for %d texts, want the global cap %d", calls, AccountSmsSendGlobalCap)
		}
		if refused := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(smsSendCapGlobal)) - refusedBefore; refused != float64(globalRefusals) {
			t.Fatalf("counted %v global cap refusals, want %d", refused, globalRefusals)
		}
	})
}

// A cap has room again once the window has passed its texts, with no wait in
// real time: the first text sent leaves first.
func TestSmsSendCapAdmitsAgainOnceTheWindowPasses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		endpoint := newAwsEndpoint(t, smsSendCapTestContentType, smsSendCapTestAnswer)
		clock := &smsSendCapTestClock{now: smsSendCapTestStart}
		sender := endpoint.cappedSender(clock)

		numbers := smsSendCapTestNumbers(t, "NL", AccountSmsSendCountryCap+3)
		for i := range AccountSmsSendCountryCap {
			if err := sendSmsSendCapTestText(t, sender, clock, numbers[i]); err != nil {
				t.Fatalf("text %d under the country cap: send error = %v", i, err)
			}
		}
		// the first text was sent a second after the start
		firstSent := smsSendCapTestStart.Add(time.Second)
		requireSmsSendCapRefusal(t, sendSmsSendCapTestText(t, sender, clock, numbers[AccountSmsSendCountryCap]), smsSendCapCountry, "NL")

		// a millisecond before the first text leaves the window, still none
		clock.now = firstSent.Add(AccountSmsSendCapWindow - time.Millisecond)
		err := sendWithGuard(t, func() error {
			return sender.SendAccountMessageTemplate(numbers[AccountSmsSendCountryCap], &AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"})
		})
		if refusal := requireSmsSendCapRefusal(t, err, smsSendCapCountry, "NL"); refusal.retryAfter != time.Millisecond {
			t.Fatalf("refusal a millisecond before the window passes waits %s, want 1ms", refusal.retryAfter)
		}

		// once it has left, one more text has room, and the next does not
		clock.now = firstSent.Add(AccountSmsSendCapWindow)
		err = sendWithGuard(t, func() error {
			return sender.SendAccountMessageTemplate(numbers[AccountSmsSendCountryCap], &AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"})
		})
		if err != nil {
			t.Fatalf("text after the window passed the first text: send error = %v", err)
		}
		err = sendWithGuard(t, func() error {
			return sender.SendAccountMessageTemplate(numbers[AccountSmsSendCountryCap+1], &AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"})
		})
		requireSmsSendCapRefusal(t, err, smsSendCapCountry, "NL")

		// a full window after the last text, the cap is empty again
		clock.now = firstSent.Add(2 * AccountSmsSendCapWindow)
		for i := AccountSmsSendCountryCap + 1; i < len(numbers); i += 1 {
			if err := sendSmsSendCapTestText(t, sender, clock, numbers[i]); err != nil {
				t.Fatalf("text %d a window after the last: send error = %v", i, err)
			}
		}
		if calls, want := endpoint.requestCount.Load(), int64(AccountSmsSendCountryCap+1+2); calls != want {
			t.Fatalf("Pinpoint was asked for %d texts, want %d", calls, want)
		}
	})
}

// Send caps that refuse every text by one cap.
type refuseEverySms struct {
	capKind string
}

func (self *refuseEverySms) admitSms(ctx context.Context, phoneNumber string) error {
	return &smsSendCapRefusal{
		capKind:    self.capKind,
		capLimit:   1,
		country:    smsDestinationCountry(phoneNumber),
		retryAfter: 90 * time.Second,
	}
}

// A refused text never reaches Pinpoint: the sender returns the refusal, a
// rate limit, and counts it by its cap. The production sender's caps are the
// Redis caps on the server clock. No Redis: the caps here are a fake.
func TestSmsSendCapRefusalStopsTheTextBeforePinpoint(t *testing.T) {
	if caps, ok := (&AWSMessageSender{}).sendCaps().(*redisSmsSendCaps); !ok || caps.now != nil {
		t.Fatalf("the production sender's caps are %T, want the Redis caps on the server clock", (&AWSMessageSender{}).sendCaps())
	}

	endpoint := newAwsEndpoint(t, smsSendCapTestContentType, smsSendCapTestAnswer)
	for _, capKind := range []string{smsSendCapGlobal, smsSendCapCountry} {
		sender := endpoint.sender(5 * time.Second)
		sender.smsCaps = &refuseEverySms{capKind: capKind}
		refusedBefore := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(capKind))
		sentBefore := testutil.ToFloat64(accountMessageSendCounter.WithLabelValues(accountMessageChannelSms, "sent"))

		for _, template := range []Template{
			&AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"},
			&AuthPasswordResetTemplate{ResetCode: strings.Repeat("ab", 64)},
			&AuthPasswordSetTemplate{},
		} {
			err := sendWithGuard(t, func() error {
				return sender.SendAccountMessageTemplate("+1 201-555-0123", template)
			})
			refusal := requireSmsSendCapRefusal(t, err, capKind, "US")
			if refusal.RetryAfterSeconds() != 90 {
				t.Fatalf("%s: retry after %d s, want 90", template.Name(), refusal.RetryAfterSeconds())
			}
		}
		if calls := endpoint.requestCount.Load(); calls != 0 {
			t.Fatalf("Pinpoint was asked for %d refused texts", calls)
		}
		if refused := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(capKind)) - refusedBefore; refused != 3 {
			t.Fatalf("counted %v %s cap refusals, want 3", refused, capKind)
		}
		if sent := testutil.ToFloat64(accountMessageSendCounter.WithLabelValues(accountMessageChannelSms, "sent")) - sentBefore; sent != 0 {
			t.Fatalf("counted %v refused texts as sent", sent)
		}
	}
}

// The destination country of a text: the number's calling code, and inside a
// shared calling code (+1, +44, +7) the country the number is in. A number
// that names no one country shares the unknown country's cap.
func TestSmsDestinationCountry(t *testing.T) {
	for _, c := range []struct {
		phoneNumber string
		country     string
	}{
		{"+1 201-555-0123", "US"},
		{"+12015550123", "US"},
		{"+1 (201) 555-0123", "US"},
		{"+1 613-555-0123", "CA"},
		{"+1 416-555-0199", "CA"},
		// another country of the +1 calling code is its own
		{"+1 876-555-0123", "JM"},
		{"+1 787-555-0123", "PR"},
		{"+44 7400 123456", "GB"},
		{"+33 6 12 34 56 78", "FR"},
		{"+49 1512 3456789", "DE"},
		{"+7 912 345-67-89", "RU"},
		{"+7 701 234 5678", "KZ"},
		{"+61 412 345 678", "AU"},
		{"+81 90-1234-5678", "JP"},
		// a non-geographic calling code
		{"+800 1234 5678", "001"},
		// numbers in no one country
		{"+1 555-555-0100", smsUnknownCountry},
		{"+44 7700 900123", smsUnknownCountry},
		{"+999 1234 5678", smsUnknownCountry},
		// a number without its calling code is not taken for a US one
		{"2015550123", smsUnknownCountry},
		{"not a number", smsUnknownCountry},
		{"", smsUnknownCountry},
	} {
		if country := smsDestinationCountry(c.phoneNumber); country != c.country {
			t.Errorf("destination country of %q = %q, want %q", c.phoneNumber, country, c.country)
		}
	}

	// the normal phone user auth, which is what the sender is handed
	for _, c := range []struct {
		userAuth string
		country  string
	}{
		{"+1 201 555 0123", "US"},
		{"+16135550123", "CA"},
		{"+33612345678", "FR"},
	} {
		normalUserAuth, userAuthType := model.NormalUserAuth(c.userAuth)
		if userAuthType != model.UserAuthTypePhone {
			t.Fatalf("%q is not a phone user auth", c.userAuth)
		}
		if country := smsDestinationCountry(normalUserAuth); country != c.country {
			t.Errorf("destination country of the user auth %q = %q, want %q", normalUserAuth, country, c.country)
		}
	}
}

// The caps leave the countries most normal texts go to more room than the
// rest, and no country more than every country together.
func TestAccountSmsSendCapsLeaveRoomForNormalSends(t *testing.T) {
	if AccountSmsSendCapWindow != time.Hour {
		t.Fatalf("the caps count texts over %s, want an hour", AccountSmsSendCapWindow)
	}
	if AccountSmsSendCountryCap <= 0 || AccountSmsSendGlobalCap < AccountSmsSendCountryCap {
		t.Fatalf("country cap %d, global cap %d", AccountSmsSendCountryCap, AccountSmsSendGlobalCap)
	}
	for _, country := range []string{"US", "CA"} {
		if countryCap := accountSmsSendCountryCap(country); countryCap <= AccountSmsSendCountryCap || AccountSmsSendGlobalCap < countryCap {
			t.Fatalf("%s cap %d, want more than the %d of other countries and at most the global %d", country, countryCap, AccountSmsSendCountryCap, AccountSmsSendGlobalCap)
		}
	}
	if countryCap := accountSmsSendCountryCap(smsUnknownCountry); countryCap != AccountSmsSendCountryCap {
		t.Fatalf("unknown country cap %d, want %d", countryCap, AccountSmsSendCountryCap)
	}
}

// A code whose text the send caps refused reaches the client as a rate limit
// with the caps' wait and message, as the code budgets' refusals do, and never
// as a failed send: after sign-up and login (send_error), on
// /auth/verify-send and /auth/password-reset (error), and as the 429 with
// Retry-After an older client is answered. No database or Redis.
func TestAuthCodeSendsReportAnSmsSendCapRefusalAsARateLimit(t *testing.T) {
	refusal := &smsSendCapRefusal{
		capKind:    smsSendCapCountry,
		capLimit:   AccountSmsSendCountryCap,
		country:    "FR",
		retryAfter: 41*time.Minute + 500*time.Millisecond,
	}
	const wantRetryAfterSeconds = 41*60 + 1
	requireCapError := func(t *testing.T, path string, sendError *AuthVerifySendError) {
		if sendError == nil || sendError.Code != model.AuthVerifySendErrorCodeRateLimited {
			t.Fatalf("%s: error = %+v, want %q", path, sendError, model.AuthVerifySendErrorCodeRateLimited)
		}
		if sendError.Message != smsSendCapRefusedMessage || sendError.RetryAfterSeconds != wantRetryAfterSeconds {
			t.Fatalf("%s: error = %+v, want the caps' message and a wait of %d s", path, sendError, wantRetryAfterSeconds)
		}
		legacyErr := legacyAuthVerifySendError(sendError)
		var rateLimit interface{ RetryAfterSeconds() int }
		if legacyErr.Error() != "429 "+smsSendCapRefusedMessage || !errors.As(legacyErr, &rateLimit) || rateLimit.RetryAfterSeconds() != wantRetryAfterSeconds {
			t.Fatalf("%s: older clients are answered %v, want the 429 with Retry-After %d", path, legacyErr, wantRetryAfterSeconds)
		}
	}

	sender := &fakeVerifySendMessageSender{err: refusal}
	verifyResult, err := authVerifySend(
		AuthVerifySendArgs{UserAuth: "+1 201-555-0123", ResultErrors: true},
		verifySendTestSession(),
		createCodeOk,
		sender,
	)
	if err != nil || sender.sendCount != 1 {
		t.Fatalf("verify send: err = %v, send count = %d", err, sender.sendCount)
	}
	requireCapError(t, "/auth/verify-send", verifyResult.Error)

	verifySend := func(args AuthVerifySendArgs, clientSession *session.ClientSession) (*AuthVerifySendResult, error) {
		return authVerifySend(args, clientSession, createCodeOk, &fakeVerifySendMessageSender{err: refusal})
	}
	loginResult, err := authLoginWithPassword(
		model.AuthLoginWithPasswordArgs{UserAuth: "+1 201-555-0123", Password: "password"},
		verifySendTestSession(),
		verificationRequiredLogin,
		verifySend,
	)
	if err != nil {
		t.Fatalf("login: err = %v", err)
	}
	requireCapError(t, "login verification_required.send_error", loginResult.VerificationRequired.SendError)

	resetCode := strings.Repeat("ab", 64)
	createResetCode := func(model.AuthPasswordResetCreateCodeArgs, *session.ClientSession) (*model.AuthPasswordResetCreateCodeResult, error) {
		return &model.AuthPasswordResetCreateCodeResult{ResetCode: &resetCode}, nil
	}
	sender = &fakeVerifySendMessageSender{err: refusal}
	resetResult, err := authPasswordReset(
		AuthPasswordResetArgs{UserAuth: "+1 201-555-0123", ResultErrors: true},
		verifySendTestSession(),
		createResetCode,
		sender,
	)
	if err != nil || sender.sendCount != 1 {
		t.Fatalf("password reset: err = %v, send count = %d", err, sender.sendCount)
	}
	requireCapError(t, "/auth/password-reset", resetResult.Error)

	// any other send error is still a failed send
	sender = &fakeVerifySendMessageSender{err: fmt.Errorf("pinpoint: %w", errors.New("throttled"))}
	verifyResult, err = authVerifySend(
		AuthVerifySendArgs{UserAuth: "+1 201-555-0123", ResultErrors: true},
		verifySendTestSession(),
		createCodeOk,
		sender,
	)
	if err != nil || verifyResult.Error == nil || verifyResult.Error.Code != model.AuthVerifySendErrorCodeSendFailed {
		t.Fatalf("a failed text: result = %+v, err = %v, want %q", verifyResult, err, model.AuthVerifySendErrorCodeSendFailed)
	}
}
