package model

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// The seed-phrase sign-up budget of a source network (owner, 2026-11-13, "accept
// a+b recommended fixes", fix a: limit seed-phrase sign-ups per source). A
// seed-phrase account needs no email, phone, wallet or SSO identity, so the
// caller's address is all a sign-up says about who makes it. Two budgets count
// a seed-phrase sign-up (CheckNetworkCreateRateLimit):
//   - the address budget: NetworkCreateDailyLimit per address bucket (ipv4 /29,
//     ipv6 /56, server.ClientIpHashForAddr) per rolling 24 h, in PostgreSQL;
//   - the source network budget here: the ipv4 /24 or ipv6 /48 that holds the
//     address may make ShortLimit sign-ups per ShortWindow and LongLimit per
//     LongWindow, in Redis.
//
// A /24 holds 32 address buckets and a /48 holds 256, so one caller holding a
// /24 could make 160 accounts a day and one holding a /48 1,280 within the
// address budget alone. The source network budget closes that rotation.
//
// What it does not stop: the 2026-11-13 wave (about 11.9k seed-phrase accounts
// in five hours, each with five hosted proxies) came from about 10,200 address
// buckets in 130 countries, almost every bucket used once an hour, the shape of
// a residential proxy pool. Its /24, /48 and autonomous system spread could not
// be measured afterwards, because only peppered address-bucket hashes are kept.
// urnetwork_seedphrase_signup_source_signups records, for every admitted
// sign-up, how many sign-ups its address bucket, its source network and its
// autonomous system had made in their windows, so the next wave shows what a
// tighter budget at each scope would refuse.
//
// Counting: the windows are fixed, aligned to the unix epoch (whole UTC hours
// and days). Only an admitted sign-up is counted: a refused attempt changes no
// counter, so a caller retrying a refusal does not extend it. Both counters of a
// source network share one Redis hash slot, and one script checks and counts
// them together, so concurrent sign-ups cannot overrun a budget. A key names the
// keyed (peppered) hash of the masked network address, never the address.
//
// A Redis failure admits the sign-up: the budget fails open, as the hosted
// proxy create rate does, and the address budget still applies.
//
// The autonomous system of an admitted sign-up is counted per AsnWindow for the
// histogram only. It is not a budget: a whole internet provider's customers
// share an autonomous system, and a sign-up spike of real users (a censorship
// event in one country) concentrates in a few of them.
type NetworkCreateSourceLimitSettings struct {
	// The prefix length, in bits, of the ipv4 network that holds a caller's
	// address. 24: the smallest block routed between networks, 32 address
	// buckets.
	Ipv4PrefixLength int
	// The prefix length, in bits, of the ipv6 network that holds a caller's
	// address. 48: the usual allocation to one site, 256 address buckets.
	Ipv6PrefixLength int
	// The short budget's fixed window.
	ShortWindow time.Duration
	// Admitted sign-ups a source network may make in one short window.
	// Non-positive disables the short budget. 20, four whole address budgets
	// in an hour: on 2026-11-12, 91% of the address buckets that signed up
	// made one sign-up all day, and none made more than 5 in an hour. How
	// real sign-ups spread over /24s and /48s was not measurable (only
	// address bucket hashes are kept), so the network budgets are multiples
	// of the address budget with room for a shared egress, until the
	// histogram shows the spread.
	ShortLimit int
	// The long budget's fixed window.
	LongWindow time.Duration
	// Admitted sign-ups a source network may make in one long window.
	// Non-positive disables the long budget. 50, ten whole address budgets a
	// day: on 2026-11-12, 99.4% of the address buckets that signed up made at
	// most 4.
	LongLimit int
	// The fixed window an autonomous system's admitted sign-ups are counted
	// in, for the histogram. Non-positive: not counted.
	AsnWindow time.Duration
}

// The production budgets. Each call returns a new instance.
func DefaultNetworkCreateSourceLimitSettings() *NetworkCreateSourceLimitSettings {
	return &NetworkCreateSourceLimitSettings{
		Ipv4PrefixLength: 24,
		Ipv6PrefixLength: 48,
		ShortWindow:      time.Hour,
		ShortLimit:       20,
		LongWindow:       24 * time.Hour,
		LongLimit:        50,
		AsnWindow:        time.Hour,
	}
}

// What the source network budget decided for one sign-up.
type networkCreateSourceDecision struct {
	// the budget that refused the sign-up, "" when it was admitted
	limited networkCreateSourceBudget
	// seconds until the refusing budget's window ends; 0 when admitted
	retryAfterSeconds int
	// the source network's admitted sign-ups in each window, this one included
	// when it was admitted
	shortCount int64
	longCount  int64
}

type networkCreateSourceBudget string

const (
	networkCreateSourceBudgetShort networkCreateSourceBudget = "short"
	networkCreateSourceBudgetLong  networkCreateSourceBudget = "long"
)

// Checks both budgets of a source network and counts the sign-up in both only
// when both have room. Returns {status, short count, long count}: status 0
// admitted, 1 refused by the short budget, 2 by the long one. A limit of 0 or
// less never refuses.
//
// KEYS[1] the short window's counter, KEYS[2] the long window's (one hash slot)
// ARGV[1] short limit, ARGV[2] long limit, ARGV[3] short ttl ms, ARGV[4] long ttl ms
var networkCreateSourceLimitScript = redis.NewScript(`
local short_count = tonumber(redis.call('GET', KEYS[1]) or '0')
local long_count = tonumber(redis.call('GET', KEYS[2]) or '0')
local short_limit = tonumber(ARGV[1])
local long_limit = tonumber(ARGV[2])
if 0 < short_limit and short_limit <= short_count then
    return {1, short_count, long_count}
end
if 0 < long_limit and long_limit <= long_count then
    return {2, short_count, long_count}
end
short_count = redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
long_count = redis.call('INCR', KEYS[2])
redis.call('PEXPIRE', KEYS[2], ARGV[4])
return {0, short_count, long_count}
`)

// The fixed window of length `window` that `now` falls in: its index since the
// unix epoch and its end.
func networkCreateSourceWindow(now time.Time, window time.Duration) (index int64, end time.Time) {
	windowMillis := window.Milliseconds()
	index = now.UnixMilli() / windowMillis
	end = time.UnixMilli((index + 1) * windowMillis)
	return
}

// Whole seconds from now until end, rounded up so that a retry after them is
// never early, and at least 1.
func networkCreateSourceRetryAfterSeconds(now time.Time, end time.Time) int {
	seconds := int((end.Sub(now) + time.Second - 1) / time.Second)
	return max(1, seconds)
}

// The hash tag of the source network that holds addr: the keyed hash of the
// masked network address, so a key never carries the address.
func networkCreateSourceNetworkTag(addr netip.Addr, settings *NetworkCreateSourceLimitSettings) string {
	networkHash := server.ClientIpHashForAddrPrefix(addr, settings.Ipv4PrefixLength, settings.Ipv6PrefixLength)
	return "ncsrc_" + hex.EncodeToString(networkHash[:])
}

// The counters of the source network that holds addr for the windows `now`
// falls in, and the windows' ends. A key names its window's length and index,
// so a changed window length never reads another length's counter.
func networkCreateSourceNetworkKeys(
	addr netip.Addr,
	now time.Time,
	settings *NetworkCreateSourceLimitSettings,
) (shortKey string, shortEnd time.Time, longKey string, longEnd time.Time) {
	tag := networkCreateSourceNetworkTag(addr, settings)
	shortIndex, shortEnd := networkCreateSourceWindow(now, settings.ShortWindow)
	longIndex, longEnd := networkCreateSourceWindow(now, settings.LongWindow)
	shortKey = fmt.Sprintf("{%s}w%d_%d", tag, settings.ShortWindow.Milliseconds(), shortIndex)
	longKey = fmt.Sprintf("{%s}w%d_%d", tag, settings.LongWindow.Milliseconds(), longIndex)
	return
}

func (self *NetworkCreateSourceLimitSettings) valid() bool {
	return self != nil &&
		0 < self.Ipv4PrefixLength && self.Ipv4PrefixLength <= 32 &&
		0 < self.Ipv6PrefixLength && self.Ipv6PrefixLength <= 128 &&
		time.Millisecond <= self.ShortWindow && time.Millisecond <= self.LongWindow
}

// Checks one seed-phrase sign-up from addr against its source network's budgets
// at `now`, and counts it when both admit it. An error means the budget could
// not be read; the caller admits the sign-up (fail open).
func checkNetworkCreateSourceLimit(
	ctx context.Context,
	addr netip.Addr,
	now time.Time,
	settings *NetworkCreateSourceLimitSettings,
) (decision networkCreateSourceDecision, returnErr error) {
	if !settings.valid() {
		returnErr = fmt.Errorf("invalid seed-phrase source network settings")
		return
	}
	if !addr.IsValid() {
		returnErr = fmt.Errorf("no source address")
		return
	}
	addr = addr.Unmap()
	shortKey, shortEnd, longKey, longEnd := networkCreateSourceNetworkKeys(addr, now, settings)

	var values []int64
	server.Redis(ctx, func(r server.RedisClient) {
		values, returnErr = networkCreateSourceLimitScript.Run(
			ctx,
			r,
			[]string{shortKey, longKey},
			settings.ShortLimit,
			settings.LongLimit,
			settings.ShortWindow.Milliseconds(),
			settings.LongWindow.Milliseconds(),
		).Int64Slice()
	})
	if returnErr != nil {
		return
	}
	if len(values) != 3 {
		returnErr = fmt.Errorf("seed-phrase source network budget returned %d values", len(values))
		return
	}
	decision.shortCount = values[1]
	decision.longCount = values[2]
	switch values[0] {
	case 0:
	case 1:
		decision.limited = networkCreateSourceBudgetShort
		decision.retryAfterSeconds = networkCreateSourceRetryAfterSeconds(now, shortEnd)
	case 2:
		decision.limited = networkCreateSourceBudgetLong
		decision.retryAfterSeconds = networkCreateSourceRetryAfterSeconds(now, longEnd)
	default:
		returnErr = fmt.Errorf("seed-phrase source network budget returned status %d", values[0])
	}
	return
}

// The counter of an autonomous system's admitted sign-ups in the window `now`
// falls in.
func networkCreateSourceAsnKey(asn uint32, now time.Time, settings *NetworkCreateSourceLimitSettings) string {
	index, _ := networkCreateSourceWindow(now, settings.AsnWindow)
	return fmt.Sprintf("{ncsrc_asn_%d}w%d_%d", asn, settings.AsnWindow.Milliseconds(), index)
}

// Counts one admitted sign-up from an autonomous system and returns the
// system's admitted sign-ups in the window, this one included. For the
// histogram only: nothing is refused on it.
func countNetworkCreateSourceAsn(
	ctx context.Context,
	asn uint32,
	now time.Time,
	settings *NetworkCreateSourceLimitSettings,
) (count int64, returnErr error) {
	key := networkCreateSourceAsnKey(asn, now, settings)
	server.Redis(ctx, func(r server.RedisClient) {
		var incr *redis.IntCmd
		_, returnErr = r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			incr = pipe.Incr(ctx, key)
			pipe.PExpire(ctx, key, settings.AsnWindow)
			return nil
		})
		if returnErr == nil {
			count = incr.Val()
		}
	})
	return
}

// The refusal of a sign-up over its source network's budget. Like the address
// budget's refusal, it says the budget is shared, so a person refused for what
// others on their network did is not told it was their own doing; the scope is
// the address range, not the one address.
func maxNetworkCreateSourceAttemptsError(retryAfterSeconds int) error {
	return &rateLimitError{
		message: "429 Too many accounts have been created recently from your network's " +
			"address range. This limit is shared with everyone connecting from the same " +
			"range, so this may not be your own activity. Please try again later or " +
			"from a different connection.",
		retryAfterSeconds: retryAfterSeconds,
	}
}

// The outcome of one seed-phrase sign-up's admission, a closed set.
type seedphraseSignupOutcome string

const (
	// admitted by the address budget and the source network budget
	seedphraseSignupAllowed seedphraseSignupOutcome = "allowed"
	// admitted by the address budget; the source network budget cannot apply
	// to a session that kept only its address hash
	seedphraseSignupAddressOnly seedphraseSignupOutcome = "address_only"
	// admitted by the address budget; the source network budget could not be
	// read, so it admitted the sign-up (fail open)
	seedphraseSignupUnavailable seedphraseSignupOutcome = "unavailable"
	// no source budget applies: an excluded infrastructure address, a tests.yml
	// acceptance source, or no client address at all
	seedphraseSignupUnscoped seedphraseSignupOutcome = "unscoped"
	// refused by the address budget
	seedphraseSignupLimitedAddress seedphraseSignupOutcome = "limited_address"
	// refused by the source network's short budget
	seedphraseSignupLimitedNetworkShort seedphraseSignupOutcome = "limited_network_short"
	// refused by the source network's long budget
	seedphraseSignupLimitedNetworkLong seedphraseSignupOutcome = "limited_network_long"
)

var seedphraseSignupOutcomes = []seedphraseSignupOutcome{
	seedphraseSignupAllowed,
	seedphraseSignupAddressOnly,
	seedphraseSignupUnavailable,
	seedphraseSignupUnscoped,
	seedphraseSignupLimitedAddress,
	seedphraseSignupLimitedNetworkShort,
	seedphraseSignupLimitedNetworkLong,
}

// The address family of a caller, a closed set: "4", "6", or "unknown" for a
// session without a usable address.
func seedphraseSignupFamily(addr netip.Addr) string {
	switch server.IpVersionForAddr(addr) {
	case 4:
		return "4"
	case 6:
		return "6"
	default:
		return "unknown"
	}
}

var seedphraseSignupFamilies = []string{"4", "6", "unknown"}

// The scopes the histogram counts a sign-up's source at, a closed set.
const (
	// the address bucket (ipv4 /29, ipv6 /56) over the rolling 24 h
	seedphraseSignupScopeAddress = "address"
	// the source network (ipv4 /24, ipv6 /48) over the short window
	seedphraseSignupScopeNetworkShort = "network_short"
	// the source network over the long window
	seedphraseSignupScopeNetworkLong = "network_long"
	// the autonomous system over AsnWindow
	seedphraseSignupScopeAsn = "asn"
)

var seedphraseSignupAdmissionsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "seedphrase_signup",
		Name:      "admissions_total",
		Help:      "Seed-phrase sign-up admissions by outcome (allowed, address_only, unavailable, unscoped, or the budget that refused: limited_address, limited_network_short, limited_network_long) and the caller's address family",
	},
	[]string{"outcome", "family"},
)

var seedphraseSignupSourceSignupsHistogram = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "urnetwork",
		Subsystem: "seedphrase_signup",
		Name:      "source_signups",
		Help:      "For each admitted seed-phrase sign-up, the admitted sign-ups its source made in the scope's window, this one included: address (ipv4 /29, ipv6 /56, rolling 24 h), network_short and network_long (ipv4 /24, ipv6 /48, the source network budget's windows), asn (autonomous system, hourly, not a budget)",
		Buckets:   []float64{1, 2, 3, 5, 10, 20, 50, 100, 200, 500, 1000},
	},
	[]string{"scope", "family"},
)

func init() {
	// materialize the closed partition, so a fleet that refuses nothing is
	// told apart from one that does not report
	for _, family := range seedphraseSignupFamilies {
		for _, outcome := range seedphraseSignupOutcomes {
			seedphraseSignupAdmissionsCounter.WithLabelValues(string(outcome), family)
		}
	}
	prometheus.MustRegister(seedphraseSignupAdmissionsCounter, seedphraseSignupSourceSignupsHistogram)
}

func noteSeedphraseSignupAdmission(outcome seedphraseSignupOutcome, family string) {
	seedphraseSignupAdmissionsCounter.WithLabelValues(string(outcome), family).Inc()
}

func observeSeedphraseSignupSource(scope string, family string, count int64) {
	seedphraseSignupSourceSignupsHistogram.WithLabelValues(scope, family).Observe(float64(count))
}
