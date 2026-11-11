package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// Quota.CountRate on {sq_<network_id>} (DESIGN.md 20.7's Counters; QUOTA-M2-03): the rates evs counts
// through ServiceHost.Quota, by the key grammar sidecar/host_quota_keys.go fixes, in one all-or-none
// script a call (quota_rate_script.go):
//
//   - each eraser's ?closure=1 uploads, its own 60 a minute and its seat in the network's closure
//     pacing (R = evs_closure_uploads_per_s, in equal turns), and its closure bytes against its own
//     evs_closure_bytes_per_day a UTC day, never below evs_closure_bytes_floor (owner decision 146),
//     so one eraser past any of these leaves another passing;
//   - the network's ERASE, CUT and CLOSURE pushes against its 60 a minute, which only classes a push
//     (P0 within it, P1 past it, committing either way);
//   - drops, drop previews and read receipts against their daily limits.
//
// Windows are a minute and a UTC day turning at 00:00:00, on the caller's clock. A refusal counts
// nothing and names the key that stopped the call, with retry_after_ms to its window's end, an
// allowance's to the next 00:00 UTC, a turn's to the eraser's seat. A debit id that passed answers pass
// for the keys' 48 h and counts nothing; a refused one recorded nothing and is judged afresh, so the
// upload route keeps one debit id an upload, and an upload refused for its turn can come back under it
// (DESIGN-DETAILS 13.2.1, QD-652).
//
// Redis is soft state (owner decision 136): a lost Redis forgets counters and opens every window
// afresh, and a call Redis does not answer passes, counted in urnetwork_quota_rate_fail_open_total,
// so the rates fail open for one window and never refuse what Redis cannot count (§20.7).
//
// Limits come from the caller, cached 5 s (§20.7's producers). Drop, preview and receipt limits ship
// dark behind enforce_sidecar_limits (quotaRateRuleDark); the closure bounds hold at the storage limit
// and while spent whatever the switch, since evs asks only then and an empty balance refuses as escrow
// does (§20.5). Bytes count only at the limit or while spent because evsservice.QuotaGate asks only
// then; a drop spends no allowance because no drop asks. {smq_} and {acl_}q stay §14's and §7.9's.

type QuotaRateSettings struct {
	// How long every {sq_} key lives past its last write, a debit id's first answer included: §20.7's
	// 48 h, api-13's counters ttl.
	KeyTtl time.Duration
	// Bounds one call whose context has no deadline: past it the call passes, as when Redis is lost.
	CallTimeout time.Duration
}

func DefaultQuotaRateSettings() *QuotaRateSettings {
	return &QuotaRateSettings{
		KeyTtl:      48 * time.Hour,
		CallTimeout: 2 * time.Second,
	}
}

var quotaRateFailOpenCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_quota_rate_fail_open_total",
	Help: "Quota.CountRate calls that passed because Redis did not answer, so nothing was counted (§20.7)",
})

func init() {
	prometheus.MustRegister(quotaRateFailOpenCounter)
}

// Counts a Quota's rates on one Redis. Safe for concurrent use.
type QuotaRateCounter struct {
	redis    QuotaRateRedis
	settings *QuotaRateSettings
}

// A counter on redis, nil being this process's own Redis, with settings, nil being the defaults.
func NewQuotaRateCounter(redis QuotaRateRedis, settings *QuotaRateSettings) *QuotaRateCounter {
	if settings == nil {
		settings = DefaultQuotaRateSettings()
	}
	if redis == nil {
		redis = NewQuotaRateServerRedis(settings.CallTimeout)
	}
	return &QuotaRateCounter{
		redis:    redis,
		settings: settings,
	}
}

// Quota.CountRate for networkId under limits, at now on the caller's clock. A non-nil refusal counted
// nothing. An error is the caller's bug, from a zero id to a key no family names, and counts nothing;
// the caller's own context ending is its error too. Every other failure passes (see the file header).
func (self *QuotaRateCounter) CountRate(
	ctx context.Context,
	networkId server.Id,
	limits *sidecar.QuotaLimits,
	keyAmounts map[string]int64,
	debitId server.Id,
	now time.Time,
) (*sidecar.QuotaRefusal, error) {
	call, err := newQuotaRateCall(networkId, limits, keyAmounts, debitId, now, self.settings, Pro().EnforceSidecarLimits)
	if err != nil {
		return nil, err
	}
	if call == nil {
		// every key is dark: nothing counts, with no I/O
		return nil, nil
	}
	reply, err := self.redis.RunScript(ctx, quotaRateScript, call.keys, call.args)
	if err == nil {
		var refusal *sidecar.QuotaRefusal
		refusal, err = parseQuotaRateReply(reply)
		if err == nil {
			return refusal, nil
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	quotaRateFailOpenCounter.Inc()
	if glog.V(1) {
		glog.Infof("[quota]rate for %s passed open: %s\n", networkId, err)
	}
	return nil, nil
}

// The counter of a process with Postgres (api, connect, taskworker): this process's Redis and the
// default settings.
var quotaRateLocalCounter = NewQuotaRateCounter(nil, nil)

// Quota.CountRate in a process with Postgres, at real time, under the network's limits from this
// process's caches (NetworkQuotaLimits: the tier's two-tier cache and the 5 s row snapshot).
func CountQuotaRate(ctx context.Context, networkId server.Id, keyAmounts map[string]int64, debitId server.Id) (*sidecar.QuotaRefusal, error) {
	if networkId == (server.Id{}) {
		return nil, errors.New("quota rate: a zero network id")
	}
	return quotaRateLocalCounter.CountRate(ctx, networkId, NetworkQuotaLimits(ctx, networkId), keyAmounts, debitId, server.NowUtc())
}

// The network's closure bytes counted in the UTC day holding now, every eraser's together: what §21.7's
// closure-growth case reads (QUOTA-M5-01). Zero when none counted that day, or when Redis lost them.
func NetworkClosureBytesToday(ctx context.Context, networkId server.Id, now time.Time) (byteCount int64, returnErr error) {
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultQuotaRateSettings().CallTimeout)
		defer cancel()
	}
	var value string
	returnErr = server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		var err error
		value, err = client.Get(ctx, quotaRateClosureBytesDayKey(networkId)).Result()
		if errors.Is(err, redis.Nil) {
			value = ""
			return nil
		}
		return err
	}, server.OptRedisNoPreflight())
	if returnErr != nil || value == "" {
		return 0, returnErr
	}
	day, total, found := strings.Cut(value, ":")
	if !found {
		return 0, fmt.Errorf("quota rate: a malformed day total %q", value)
	}
	if day != strconv.FormatInt(sidecar.QuotaRateWindowUtcDay.Index(now.UTC().Truncate(time.Microsecond)), 10) {
		return 0, nil
	}
	return strconv.ParseInt(total, 10, 64)
}
