package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/nyaruka/phonenumbers"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The send caps of account texts (owner decisions 2026-11-25, hourly caps, and
// 2026-11-26, daily caps and an alert before the AWS SMS limit is raised).
//
// The server texts a phone only for account security (smsTemplateNames), and
// the price of a text depends on where it goes: most texts go to the US and
// Canada, and a text to many other countries costs many times as much. The
// limits before this one are per email or phone (model/auth_code_send_limit.go)
// and per client address (model/auth_model_attempt.go), so codes asked for many
// different numbers never meet them, and unexpected high-cost SMS volume could
// spend the month's send budget in hours. Every text now also passes four caps
// right before its Pinpoint call (AWSMessageSender.sendSms), on every path that
// sends one: the verification and password reset codes, and the
// password-changed notice.
//   - The global caps: AccountSmsSendGlobalCap texts in any
//     AccountSmsSendCapWindow (an hour) and AccountSmsSendGlobalDailyCap texts
//     in any AccountSmsSendDailyCapWindow (24 hours), across every service.
//   - The destination country's caps: AccountSmsSendCountryCap texts in any
//     hour and AccountSmsSendCountryDailyCap texts in any 24 hours, or the
//     country's own caps in accountSmsSendCountryCaps and
//     accountSmsSendCountryDailyCaps.
//
// The hourly caps stop a burst within the hour. The daily caps stop volume
// spread over many hours, each under the hourly caps, which would otherwise
// spend 24 hourly caps a day. The caps are several times the normal volume of
// their window, so they bind only on unusual volume. A text a cap refuses is
// not sent and spends no cap. The refusal is a rate limit, the same kind the
// daily code budget of an email or phone answers: a 429 with the wait until
// every cap of the text has room again (Retry-After, and `retry_after_seconds`
// in a verify send result). Each refusal is counted by the cap that refused it
// (urnetwork_account_message_sms_cap_refusals_total), which the Grafana rule
// AccountSmsSendCapRefusals (warp grafana/alerting/account-sms-send-cap.yml)
// warns on, and logged with the destination country, never the number. Caps
// that cannot be read refuse the text as a failed send, so no text goes out
// uncounted.
//
// The counts live in Redis, as the auth attempt limits do. Each cap is a
// sliding window of the texts it admitted, each scored by when it leaves the
// window, and every key shares one hash tag, so one script reads every cap of
// a text and spends all of them or none.

// The trailing window each hourly cap counts texts over.
const AccountSmsSendCapWindow = time.Hour

// The trailing window each daily cap counts texts over. It slides like the
// hourly one: a text leaves it 24 hours after it was sent, not at midnight.
const AccountSmsSendDailyCapWindow = 24 * time.Hour

// The most account texts all services together send in an hour.
const AccountSmsSendGlobalCap = 30

// The most account texts all services together send in 24 hours.
const AccountSmsSendGlobalDailyCap = 100

// The most account texts sent in an hour to one destination country that has
// no hourly cap of its own. Numbers in no one country (smsUnknownCountry) share
// this cap too.
const AccountSmsSendCountryCap = 10

// The most account texts sent in 24 hours to one destination country that has
// no daily cap of its own. Numbers in no one country (smsUnknownCountry) share
// this cap too.
const AccountSmsSendCountryDailyCap = 30

// The destination countries with an hourly cap of their own, by ISO 3166-1
// region code. Most normal sends go to these.
var accountSmsSendCountryCaps = map[string]int{
	"US": 20,
	"CA": 20,
}

// The destination countries with a daily cap of their own.
var accountSmsSendCountryDailyCaps = map[string]int{
	"US": 60,
	"CA": 60,
}

// The hourly cap the texts to a destination country count against.
func accountSmsSendCountryCap(country string) int {
	if countryCap, ok := accountSmsSendCountryCaps[country]; ok {
		return countryCap
	}
	return AccountSmsSendCountryCap
}

// The daily cap the texts to a destination country count against.
func accountSmsSendCountryDailyCap(country string) int {
	if countryCap, ok := accountSmsSendCountryDailyCaps[country]; ok {
		return countryCap
	}
	return AccountSmsSendCountryDailyCap
}

// The caps, as a refusal names them and the refusal metric labels them. The
// hourly caps keep the names they had before the daily caps.
const (
	smsSendCapGlobal       = "global"
	smsSendCapCountry      = "country"
	smsSendCapGlobalDaily  = "global_daily"
	smsSendCapCountryDaily = "country_daily"
)

// One cap a text passes: the most texts it admits in its trailing window.
type smsSendCap struct {
	// how a refusal names the cap and the refusal metric labels it
	kind   string
	window time.Duration
	// a count for each destination country, rather than one across them all
	perCountry bool
	// the cap for a text to a destination country
	limit func(country string) int
}

// Every cap a text passes, in the order the script reads them.
var accountSmsSendCaps = []smsSendCap{
	{
		kind:   smsSendCapGlobal,
		window: AccountSmsSendCapWindow,
		limit:  func(string) int { return AccountSmsSendGlobalCap },
	},
	{
		kind:       smsSendCapCountry,
		window:     AccountSmsSendCapWindow,
		perCountry: true,
		limit:      accountSmsSendCountryCap,
	},
	{
		kind:   smsSendCapGlobalDaily,
		window: AccountSmsSendDailyCapWindow,
		limit:  func(string) int { return AccountSmsSendGlobalDailyCap },
	},
	{
		kind:       smsSendCapCountryDaily,
		window:     AccountSmsSendDailyCapWindow,
		perCountry: true,
		limit:      accountSmsSendCountryDailyCap,
	},
}

// The destination country of a number that names no one country: one that does
// not parse, or one that matches no country of a calling code several countries
// share. Such texts share the caps of this one country, "ZZ".
const smsUnknownCountry = phonenumbers.UNKNOWN_REGION

// A region code: two letters, or "001" for the non-geographic calling codes.
var smsCountryPattern = regexp.MustCompile(`^(?:[A-Z]{2}|001)$`)

// The destination country of a text to phoneNumber, as an ISO 3166-1 region
// code: the country of the number's calling code, and for a calling code that
// several countries share (+1 has the US, Canada and two dozen others) the
// country the number is numbered in. A number must be in international form, as
// a normal phone user auth is (model.NormalUserAuth): one without its calling
// code names no country.
func smsDestinationCountry(phoneNumber string) string {
	number, err := phonenumbers.Parse(phoneNumber, phonenumbers.UNKNOWN_REGION)
	if err != nil {
		return smsUnknownCountry
	}
	country := phonenumbers.GetRegionCodeForNumber(number)
	if !smsCountryPattern.MatchString(country) {
		return smsUnknownCountry
	}
	return country
}

// What a client is told about a text the caps refused, unless it shows its own
// words for a rate limit.
const smsSendCapRefusedMessage = "Too many texts are being sent right now. Please try again later, or use an email address instead."

// A text the caps refused. It is a rate limit, like the code budgets' refusals:
// Error carries the "429 " prefix router.RaiseHttpError turns into the status,
// and RetryAfterSeconds the wait it turns into a Retry-After header.
type smsSendCapRefusal struct {
	// the kind of the cap that refused (accountSmsSendCaps)
	capKind string
	// the cap that refused, in texts per window
	capLimit int
	// the trailing window of the cap that refused
	capWindow time.Duration
	// the destination country (smsDestinationCountry)
	country string
	// until every cap of the text has room again
	retryAfter time.Duration
}

func (self *smsSendCapRefusal) Error() string {
	return "429 " + smsSendCapRefusedMessage
}

// Whole seconds, rounded up so that trying again after them is never early.
func (self *smsSendCapRefusal) RetryAfterSeconds() int {
	return int(max((self.retryAfter+time.Second-1)/time.Second, 1))
}

// The verify send result error of a code whose text the caps refused: a rate
// limit with its wait, as the code budgets answer, rather than a failed send.
// Nil for any other send error.
func smsSendCapVerifySendError(sendErr error) *AuthVerifySendError {
	var refusal *smsSendCapRefusal
	if !errors.As(sendErr, &refusal) {
		return nil
	}
	return &AuthVerifySendError{
		Code:              model.AuthVerifySendErrorCodeRateLimited,
		Message:           smsSendCapRefusedMessage,
		RetryAfterSeconds: refusal.RetryAfterSeconds(),
	}
}

// The caps a text passes before its Pinpoint call.
type smsSendCaps interface {
	// Admits a text to phoneNumber and spends it from the caps. A refusal is a
	// *smsSendCapRefusal; any other error means the caps could not be read.
	admitSms(ctx context.Context, phoneNumber string) error
}

// The production caps, kept in Redis.
type redisSmsSendCaps struct {
	// nil reads the server clock; tests move time without waiting
	now func() time.Time
}

// The caps of the production sender, AWSMessageSender's zero value.
var defaultSmsSendCaps smsSendCaps = &redisSmsSendCaps{}

const smsSendCapKeyPrefix = "account_sms_send_cap."

// Every key of the caps shares this hash tag, so one script can read and spend
// every cap of a text together.
const smsSendCapHashTag = "account_sms_send_cap"

// The key of the cap's count for texts to country. The hourly caps keep the
// keys they had before the daily caps, so their windows carry over a deploy.
func (self *smsSendCap) key(country string) string {
	if self.perCountry {
		return fmt.Sprintf("%s{%s}.%s.%s", smsSendCapKeyPrefix, smsSendCapHashTag, self.kind, country)
	}
	return fmt.Sprintf("%s{%s}.%s", smsSendCapKeyPrefix, smsSendCapHashTag, self.kind)
}

// Admits one text by every cap in KEYS, or refuses it. Each cap is a sorted set
// of the texts it admitted, scored by when they leave its window. A refused
// text is added to none of them. ARGV: now, the text's member, then for the
// i-th key, at 3i to 3i+2, its cap, when this text would leave its window, and
// its window (all times in milliseconds). Returns {0, 0} for an admitted text,
// or the index in KEYS (from 1) of a cap with no room and the milliseconds
// until every cap has room: when several have no room, the one with the
// longest wait, and the later key on a tie.
var smsSendCapScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local refused = 0
local wait = 0
for i = 1, #KEYS do
    redis.call('ZREMRANGEBYSCORE', KEYS[i], '-inf', now)
    if tonumber(ARGV[3 * i]) <= redis.call('ZCARD', KEYS[i]) then
        local oldest = redis.call('ZRANGE', KEYS[i], 0, 0, 'WITHSCORES')
        local capWait = math.max(tonumber(oldest[2]) - now, 1)
        if wait <= capWait then
            refused = i
            wait = capWait
        end
    end
end
if 0 < refused then
    return {refused, wait}
end
for i = 1, #KEYS do
    redis.call('ZADD', KEYS[i], ARGV[3 * i + 1], ARGV[2])
    redis.call('PEXPIRE', KEYS[i], ARGV[3 * i + 2])
end
return {0, 0}
`)

func (self *redisSmsSendCaps) admitSms(ctx context.Context, phoneNumber string) error {
	now := server.NowUtc()
	if self.now != nil {
		now = self.now()
	}
	country := smsDestinationCountry(phoneNumber)
	keys := make([]string, 0, len(accountSmsSendCaps))
	args := []any{now.UnixMilli(), server.NewId().String()}
	for i := range accountSmsSendCaps {
		sendCap := &accountSmsSendCaps[i]
		keys = append(keys, sendCap.key(country))
		args = append(
			args,
			sendCap.limit(country),
			now.Add(sendCap.window).UnixMilli(),
			sendCap.window.Milliseconds(),
		)
	}
	var values []int64
	var err error
	server.Redis(ctx, func(r server.RedisClient) {
		// the error is returned rather than raised: a raised connection error
		// would run the script again, and a run whose answer was lost would
		// count the text twice
		values, err = smsSendCapScript.Run(ctx, r, keys, args...).Int64Slice()
	})
	if err != nil {
		return fmt.Errorf("the sms send caps could not be read: %w", err)
	}
	if len(values) != 2 || values[0] < 0 || int64(len(accountSmsSendCaps)) < values[0] {
		return fmt.Errorf("the sms send caps answered %v", values)
	}
	if values[0] == 0 {
		return nil
	}
	sendCap := &accountSmsSendCaps[values[0]-1]
	return &smsSendCapRefusal{
		capKind:    sendCap.kind,
		capLimit:   sendCap.limit(country),
		capWindow:  sendCap.window,
		country:    country,
		retryAfter: time.Duration(values[1]) * time.Millisecond,
	}
}

// Counts the account texts the caps refused before their Pinpoint call, by the
// cap that refused them.
var smsSendCapRefusalCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "account_message",
		Name:      "sms_cap_refusals_total",
		Help:      "Account texts the send caps refused before their Pinpoint SMS call, by cap (global, country, global_daily, country_daily)",
	},
	[]string{"cap"},
)

// Registers the refusal metric with every cap at zero, so a first refusal is an
// increase.
func init() {
	prometheus.MustRegister(smsSendCapRefusalCounter)
	for i := range accountSmsSendCaps {
		smsSendCapRefusalCounter.WithLabelValues(accountSmsSendCaps[i].kind)
	}
}

// The default-level report of caps that could not be read. Every text asks the
// caps, so a line per failure would let an outage fill the logs.
var smsSendCapFailures = newBoundedReport(accountMessageSendReportInterval)

// The caps this sender's texts pass: its own, or the production caps.
func (self *AWSMessageSender) sendCaps() smsSendCaps {
	if self.smsCaps != nil {
		return self.smsCaps
	}
	return defaultSmsSendCaps
}

// Admits a text of the template to phoneNumber by the caps, before its Pinpoint
// call. A refusal is counted by its cap and logged, one line each, with the
// destination country and never the number; caps that cannot be read are
// reported at most once an interval. Either way the text is not sent and the
// error is returned.
func (self *AWSMessageSender) admitSms(ctx context.Context, phoneNumber string, templateName string) error {
	err := self.sendCaps().admitSms(ctx, phoneNumber)
	if err == nil {
		return nil
	}
	var refusal *smsSendCapRefusal
	if errors.As(err, &refusal) {
		smsSendCapRefusalCounter.WithLabelValues(refusal.capKind).Inc()
		glog.Infof(
			"[aws]account sms %s to %s refused by the %s send cap (%d texts in %d h); room again in %s\n",
			templateName,
			refusal.country,
			refusal.capKind,
			refusal.capLimit,
			int(refusal.capWindow/time.Hour),
			refusal.retryAfter.Round(time.Second),
		)
		return err
	}
	if suppressedCount, ok := smsSendCapFailures.Allow(time.Now()); ok {
		glog.Infof(
			"[aws]account sms %s not sent: %s; %d more since the last report\n",
			templateName,
			err,
			suppressedCount,
		)
	}
	return err
}
