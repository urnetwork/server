// The contract degradation valve. When contracts close much more slowly than
// they are created, every open contract keeps the escrow it reserved, payers
// whose balance is held that way are refused new contracts, and escrow
// admission under the resulting hot payers holds database connections across
// Redis reservations. The valve then makes new contracts zero cost: public and
// companion contracts are created without escrow (subscription_zero_escrow.go),
// no balance is reserved or debited, and no provider is paid for their traffic.
//
// The CheckContractDegradation task owns the decision. Every 15 minutes it
// counts the contracts created in the last 60 minutes and the contracts whose
// terminal outcome was claimed in those 60 minutes, and publishes one Redis
// state with a one hour TTL, four checks. A check is degraded when fewer than
// 70% as many contracts closed as were created; a window with no creations is
// healthy. One degraded check turns zero cost on at once. Charging resumes only
// on the second consecutive healthy check; the state carries the consecutive
// healthy count. A failed check publishes nothing, so the previous state and
// its count stand until their TTL ends, and a stopped task lets them expire.
//
// Contract creation reads the state through a 15 second in-process cache,
// never inside a database transaction. A contract is zero cost only when
// config/<env>/degraded.yml enables the valve and the published state is
// present, unexpired, well formed and zero cost. Every other case charges
// normally: a disabled, absent or unreadable config, a missing or expired key,
// a Redis error, or a malformed or inconsistent value. The valve decides only
// how a new contract is created; it never changes an existing contract.
//
// Safe for concurrent use.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// config/<env>/degraded.yml, the control that enables both the check task and
// the valve. Absent is disabled.
const NetworkDegradationResourceName = "degraded.yml"

// The window each check measures.
const ContractDegradationWindow = 60 * time.Minute

// The check task's period.
const ContractDegradationCheckInterval = 15 * time.Minute

// Four check periods: a stopped or disabled task falls back to charging.
const ContractDegradationStateTtl = 4 * ContractDegradationCheckInterval

// Consecutive healthy checks that turn charging back on.
const ContractDegradationResumeChecks = 2

// A check is degraded when close_count < 7/10 × open_count.
const contractDegradationCloseNumerator = 7
const contractDegradationCloseDenominator = 10

// How long contract creation trusts one read of the published state.
const zeroContractCostCacheTtl = 15 * time.Second

// After a Redis error the process charges normally and asks again this soon.
const zeroContractCostErrorRetry = time.Second

// Bounds every Redis round trip of the reader and the check.
const contractDegradationRedisTimeout = time.Second

// Bounds each count of a check.
const contractDegradationStatementTimeout = 60 * time.Second

// The clock skew tolerated between the checking host and a reader.
const contractDegradationClockSkew = time.Minute

const contractDegradationStateKey = "network_degradation:zero_contract_cost:v1"

// Counts the contracts created in [$1, $2) over transfer_contract_create_time
// (create_time, open, contract_id).
const contractDegradationOpenCountSql = `
	SELECT count(*)
	FROM transfer_contract
	WHERE $1 <= create_time AND create_time < $2
`

// Counts the terminal outcomes claimed in [$1, $2) over the partial
// transfer_contract_closed_usage (close_time, contract_id) WHERE outcome IS NOT
// NULL; the outcome predicate is what lets the planner use that index.
const contractDegradationCloseCountSql = `
	SELECT count(*)
	FROM transfer_contract
	WHERE outcome IS NOT NULL AND $1 <= close_time AND close_time < $2
`

// Replaces the published state only if it is still the one the check read, so
// an overlapping or stale check can never overwrite a newer decision. ARGV[1]
// is 1 when a value was read and ARGV[2] is that value; ARGV[3] is the new
// value and ARGV[4] its TTL in milliseconds.
const contractDegradationPublishScript = `
local current = redis.call('GET', KEYS[1])
if ARGV[1] == '1' then
 if current ~= ARGV[2] then return 0 end
elseif current then
 return 0
end
redis.call('SET', KEYS[1], ARGV[3], 'PX', ARGV[4])
return 1
`

var contractDegradationChecks = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_contract_degradation_checks_total",
	Help: "Contract degradation checks by outcome: zero_cost (degraded), resuming (healthy, zero cost held), charging (healthy), or a failure that published nothing.",
}, []string{"outcome"})

var contractDegradationZeroCost = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_contract_degradation_zero_cost",
	Help: "1 while the last state this process published makes new contracts zero cost, else 0.",
})

var contractDegradationContracts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "urnetwork_contract_degradation_contracts",
	Help: "Contracts created (open) and terminally closed (close) in the window of the last check this process published.",
}, []string{"kind"})

var zeroContractCostReads = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_zero_contract_cost_reads_total",
	Help: "Contract creation cache refreshes of the degradation state by result; only zero_cost makes new contracts zero cost.",
}, []string{"result"})

// Materializes every bounded label, so an explicit zero is visible before the
// first check or read.
func init() {
	for _, outcome := range []string{"zero_cost", "resuming", "charging", "measurement_failed", "state_read_failed", "publish_failed", "superseded"} {
		contractDegradationChecks.WithLabelValues(outcome)
	}
	for _, kind := range []string{"open", "close"} {
		contractDegradationContracts.WithLabelValues(kind)
	}
	for _, result := range []string{"zero_cost", "charging", "disabled", "missing", "expired", "malformed", "error"} {
		zeroContractCostReads.WithLabelValues(result)
	}
	prometheus.MustRegister(contractDegradationChecks, contractDegradationZeroCost, contractDegradationContracts, zeroContractCostReads)
}

// The degraded.yml document. Unknown keys are refused, so a misspelled key
// fails toward charging instead of being silently ignored.
type networkDegradationSettings struct {
	Enabled bool `yaml:"enabled"`
}

// Reads degraded.yml on each call, as other optional config/<env> resources
// are read; absent is disabled. A malformed file is an error.
func loadNetworkDegradationSettings() (networkDegradationSettings, error) {
	resource, err := server.Config.SimpleResource(NetworkDegradationResourceName)
	if errors.Is(err, server.ErrResourceNotFound) {
		return networkDegradationSettings{}, nil
	}
	if err != nil {
		return networkDegradationSettings{}, err
	}
	data, err := resource.BytesE()
	if err != nil {
		return networkDegradationSettings{}, err
	}
	var settings networkDegradationSettings
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&settings); err != nil && !errors.Is(err, io.EOF) {
		return networkDegradationSettings{}, fmt.Errorf("%s: %w", NetworkDegradationResourceName, err)
	}
	return settings, nil
}

// The last settings error logged, so a broken file is reported once per change
// rather than on every cache refresh.
var lastNetworkDegradationSettingsError atomic.Pointer[string]

// Whether degraded.yml enables the check and the valve. An unreadable or
// malformed file is disabled.
func NetworkDegradationEnabled() bool {
	settings, err := loadNetworkDegradationSettings()
	if err != nil {
		message := err.Error()
		if previous := lastNetworkDegradationSettingsError.Swap(&message); previous == nil || *previous != message {
			glog.Errorf("[degradation]%s is unreadable; the contract degradation valve is disabled and contracts are charged normally: %v\n", NetworkDegradationResourceName, err)
		}
		return false
	}
	return settings.Enabled
}

// One published check. The counts cover the window that ended at evaluated_at.
type ContractDegradationState struct {
	ZeroCost   bool  `json:"zero_cost"`
	OpenCount  int64 `json:"open_count"`
	CloseCount int64 `json:"close_count"`
	// close_count / open_count, or 0 when nothing was created
	Ratio                   float64   `json:"ratio"`
	ConsecutiveHealthyCount int       `json:"consecutive_healthy_count"`
	EvaluatedAt             time.Time `json:"evaluated_at"`
}

// A window is degraded when fewer than 70% as many terminal outcomes as
// creations were claimed in it; a window with no creations is healthy. Integer
// arithmetic keeps the 0.7 boundary exact.
func contractDegradationHealthy(openCount int64, closeCount int64) bool {
	return openCount <= 0 || contractDegradationCloseNumerator*openCount <= contractDegradationCloseDenominator*closeCount
}

// The state a check publishes after previous, which is nil when there is none
// or it is unusable; that starts from charging. A degraded window turns zero
// cost on at once and resets the healthy count. A healthy window counts one
// more consecutive healthy check, capped at ContractDegradationResumeChecks,
// and turns zero cost off only when that count reaches it.
func nextContractDegradationState(
	previous *ContractDegradationState,
	openCount int64,
	closeCount int64,
	evaluatedAt time.Time,
) *ContractDegradationState {
	state := &ContractDegradationState{
		OpenCount:   openCount,
		CloseCount:  closeCount,
		EvaluatedAt: evaluatedAt.UTC(),
	}
	if 0 < openCount {
		state.Ratio = float64(closeCount) / float64(openCount)
	}
	if !contractDegradationHealthy(openCount, closeCount) {
		state.ZeroCost = true
		return state
	}
	previousHealthyCount := 0
	previousZeroCost := false
	if previous != nil {
		previousHealthyCount = previous.ConsecutiveHealthyCount
		previousZeroCost = previous.ZeroCost
	}
	state.ConsecutiveHealthyCount = min(previousHealthyCount+1, ContractDegradationResumeChecks)
	state.ZeroCost = previousZeroCost && state.ConsecutiveHealthyCount < ContractDegradationResumeChecks
	return state
}

// Whether a state is unexpired at now and consistent with the rule that
// produced it. A value no check could have written is unusable, so neither a
// corrupted nor a hand-edited value can turn zero cost on.
func (self *ContractDegradationState) usableAt(now time.Time) bool {
	if self.OpenCount < 0 || self.CloseCount < 0 || self.EvaluatedAt.IsZero() {
		return false
	}
	if !now.Before(self.EvaluatedAt.Add(ContractDegradationStateTtl)) ||
		now.Add(contractDegradationClockSkew).Before(self.EvaluatedAt) {
		return false
	}
	ratio := float64(0)
	if 0 < self.OpenCount {
		ratio = float64(self.CloseCount) / float64(self.OpenCount)
	}
	if self.Ratio != ratio {
		return false
	}
	if !contractDegradationHealthy(self.OpenCount, self.CloseCount) {
		return self.ZeroCost && self.ConsecutiveHealthyCount == 0
	}
	if self.ConsecutiveHealthyCount < 1 || ContractDegradationResumeChecks < self.ConsecutiveHealthyCount {
		return false
	}
	return !self.ZeroCost || self.ConsecutiveHealthyCount < ContractDegradationResumeChecks
}

// Decodes one published value strictly: unknown fields or trailing data are
// malformed.
func parseContractDegradationState(raw string) (*ContractDegradationState, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	state := &ContractDegradationState{}
	if err := decoder.Decode(state); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("contract degradation state has trailing data")
	}
	return state, nil
}

// Reads the published value; present is false for a missing key. Replaced in
// tests to inject a Redis error.
var readContractDegradationState = func(ctx context.Context) (raw string, present bool, returnErr error) {
	readCtx, cancel := context.WithTimeout(ctx, contractDegradationRedisTimeout)
	defer cancel()
	returnErr = server.RedisWithDeadline(readCtx, func(client server.RedisClient) error {
		value, err := client.Get(readCtx, contractDegradationStateKey).Result()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		raw, present = value, true
		return nil
	})
	if returnErr != nil {
		raw, present = "", false
	}
	return
}

// The published state as stored, for diagnostics and tests; present is false
// when the key is missing. It is neither validated nor checked for expiry:
// ZeroContractCost is the only reader that decides how contracts are created.
func GetContractDegradationState(ctx context.Context) (state *ContractDegradationState, present bool, returnErr error) {
	raw, present, returnErr := readContractDegradationState(ctx)
	if returnErr != nil || !present {
		return nil, present, returnErr
	}
	state, returnErr = parseContractDegradationState(raw)
	return
}

// Publishes next with the state TTL if the key still holds what was read.
func publishContractDegradationState(ctx context.Context, readRaw string, readPresent bool, next string) (published bool, returnErr error) {
	publishCtx, cancel := context.WithTimeout(ctx, contractDegradationRedisTimeout)
	defer cancel()
	expected := "0"
	if readPresent {
		expected = "1"
	}
	returnErr = server.RedisWithDeadline(publishCtx, func(client server.RedisClient) error {
		result, err := client.Eval(publishCtx, contractDegradationPublishScript, []string{contractDegradationStateKey},
			expected, readRaw, next, ContractDegradationStateTtl.Milliseconds()).Int()
		published = result == 1
		return err
	})
	return
}

// Counts the contracts created in [now - window, now) and the terminal outcomes
// claimed in that window in one read-only snapshot, each a range over its index
// and bounded by a statement timeout.
func MeasureContractDegradation(ctx context.Context, now time.Time) (openCount int64, closeCount int64, returnErr error) {
	windowStart := now.Add(-ContractDegradationWindow)
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = '%dms'`, contractDegradationStatementTimeout.Milliseconds())))
			server.Raise(tx.QueryRow(ctx, contractDegradationOpenCountSql, windowStart, now).Scan(&openCount))
			server.Raise(tx.QueryRow(ctx, contractDegradationCloseCountSql, windowStart, now).Scan(&closeCount))
		}, pgx.ReadOnly, server.OptNoRetry())
	}, func(err error) {
		openCount, closeCount, returnErr = 0, 0, err
	})
	return
}

// Runs one check at now and publishes its state. A failed count or state read
// publishes nothing and returns the error, leaving the published state and its
// healthy count to their TTL. A state that changed during the check is kept,
// and the result is nil. An unusable published state is replaced as if absent.
func CheckContractDegradation(ctx context.Context, now time.Time) (*ContractDegradationState, error) {
	openCount, closeCount, err := MeasureContractDegradation(ctx, now)
	if err != nil {
		contractDegradationChecks.WithLabelValues("measurement_failed").Inc()
		glog.Errorf("[degradation]contract check could not count; the published state is unchanged until its ttl: %v\n", err)
		return nil, err
	}
	raw, present, err := readContractDegradationState(ctx)
	if err != nil {
		contractDegradationChecks.WithLabelValues("state_read_failed").Inc()
		glog.Errorf("[degradation]contract check could not read the published state; it is unchanged until its ttl: open=%d close=%d err=%v\n", openCount, closeCount, err)
		return nil, err
	}
	var previous *ContractDegradationState
	if present {
		if parsed, parseErr := parseContractDegradationState(raw); parseErr == nil && parsed.usableAt(now) {
			previous = parsed
		} else {
			glog.Infof("[degradation]contract check replaces an unusable published state as if absent\n")
		}
	}
	state := nextContractDegradationState(previous, openCount, closeCount, now)
	next, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	published, err := publishContractDegradationState(ctx, raw, present, string(next))
	if err != nil {
		contractDegradationChecks.WithLabelValues("publish_failed").Inc()
		glog.Errorf("[degradation]contract check could not publish; the published state is unchanged until its ttl: open=%d close=%d err=%v\n", openCount, closeCount, err)
		return nil, err
	}
	if !published {
		contractDegradationChecks.WithLabelValues("superseded").Inc()
		glog.Infof("[degradation]contract check superseded by a concurrent publication: open=%d close=%d\n", openCount, closeCount)
		return nil, nil
	}

	outcome := "charging"
	zeroCostValue := float64(0)
	if state.ZeroCost {
		zeroCostValue = 1
		outcome = "zero_cost"
		if contractDegradationHealthy(openCount, closeCount) {
			outcome = "resuming"
		}
	}
	contractDegradationChecks.WithLabelValues(outcome).Inc()
	contractDegradationZeroCost.Set(zeroCostValue)
	contractDegradationContracts.WithLabelValues("open").Set(float64(openCount))
	contractDegradationContracts.WithLabelValues("close").Set(float64(closeCount))
	previousZeroCost := previous != nil && previous.ZeroCost
	glog.Infof(
		"[degradation]contract check %s: open=%d close=%d ratio=%.4f healthy=%t consecutive_healthy=%d zero_cost=%t previous_zero_cost=%t\n",
		outcome, openCount, closeCount, state.Ratio, contractDegradationHealthy(openCount, closeCount),
		state.ConsecutiveHealthyCount, state.ZeroCost, previousZeroCost,
	)
	// this process sees its own decision at once; others within the cache period
	zeroContractCostCache.invalidate()
	return state, nil
}

// One cached read of the config and published state.
type zeroContractCostSnapshot struct {
	loadTime  time.Time
	expiresAt time.Time
	enabled   bool
	state     *ContractDegradationState
}

// The state is rechecked at the time of the question, so the cache never
// extends a state past its TTL.
func (self *zeroContractCostSnapshot) zeroCostAt(now time.Time) bool {
	return self.enabled && self.state != nil && self.state.ZeroCost && self.state.usableAt(now)
}

// The process cache. One caller refreshes a stale snapshot while the others
// keep using it; only the first read of the process waits for the refresh.
type zeroContractCostCacheState struct {
	refreshLock sync.Mutex
	snapshot    atomic.Pointer[zeroContractCostSnapshot]
}

var zeroContractCostCache = &zeroContractCostCacheState{}

// The reader's clock. Replaced in tests.
var zeroContractCostNow = server.NowUtc

// A snapshot serves reads until it expires, and never for a clock that moved
// behind its load.
func (self *zeroContractCostCacheState) fresh(snapshot *zeroContractCostSnapshot, now time.Time) bool {
	return snapshot != nil && now.Before(snapshot.expiresAt) && !now.Before(snapshot.loadTime)
}

// The snapshot for a read at now, refreshing a stale one: the first read of
// the process waits for the refresh, later readers keep the stale snapshot
// while one caller refreshes.
func (self *zeroContractCostCacheState) current(ctx context.Context, now time.Time) *zeroContractCostSnapshot {
	snapshot := self.snapshot.Load()
	if self.fresh(snapshot, now) {
		return snapshot
	}
	if snapshot == nil {
		self.refreshLock.Lock()
	} else if !self.refreshLock.TryLock() {
		return snapshot
	}
	defer self.refreshLock.Unlock()
	if latest := self.snapshot.Load(); self.fresh(latest, now) {
		return latest
	}
	refreshed := loadZeroContractCostSnapshot(ctx, now)
	self.snapshot.Store(refreshed)
	return refreshed
}

// Drops the snapshot, so the next read refreshes.
func (self *zeroContractCostCacheState) invalidate() {
	self.snapshot.Store(nil)
}

// Reads degraded.yml and, while it is enabled, the published state. The Redis
// read is detached from the caller's cancellation so one canceled request
// cannot make the whole process charge for a cache period.
func loadZeroContractCostSnapshot(ctx context.Context, now time.Time) *zeroContractCostSnapshot {
	snapshot := &zeroContractCostSnapshot{loadTime: now, expiresAt: now.Add(zeroContractCostCacheTtl)}
	if !NetworkDegradationEnabled() {
		zeroContractCostReads.WithLabelValues("disabled").Inc()
		return snapshot
	}
	snapshot.enabled = true
	raw, present, err := readContractDegradationState(context.WithoutCancel(ctx))
	if err != nil {
		snapshot.expiresAt = now.Add(zeroContractCostErrorRetry)
		zeroContractCostReads.WithLabelValues("error").Inc()
		if glog.V(1) {
			glog.Infof("[degradation]zero contract cost read failed; charging normally: %v\n", err)
		}
		return snapshot
	}
	if !present {
		zeroContractCostReads.WithLabelValues("missing").Inc()
		return snapshot
	}
	state, err := parseContractDegradationState(raw)
	if err != nil {
		zeroContractCostReads.WithLabelValues("malformed").Inc()
		return snapshot
	}
	if !state.usableAt(now) {
		result := "malformed"
		if !state.EvaluatedAt.IsZero() && !now.Before(state.EvaluatedAt.Add(ContractDegradationStateTtl)) {
			result = "expired"
		}
		zeroContractCostReads.WithLabelValues(result).Inc()
		return snapshot
	}
	snapshot.state = state
	if state.ZeroCost {
		zeroContractCostReads.WithLabelValues("zero_cost").Inc()
	} else {
		zeroContractCostReads.WithLabelValues("charging").Inc()
	}
	return snapshot
}

// Whether a contract created now is zero cost. At most one Redis read per cache
// period per process; callers must not hold a database transaction.
func ZeroContractCost(ctx context.Context) bool {
	now := zeroContractCostNow()
	return zeroContractCostCache.current(ctx, now).zeroCostAt(now)
}

// Drops the process cache, so the next read sees the current config and state.
func Testing_ResetZeroContractCostCache() {
	zeroContractCostCache.invalidate()
}

// Enables degraded.yml and publishes, with the state TTL, the state a single
// check would publish to turn zero cost on, or a healthy charging state, then
// drops the process cache. The returned function removes both and drops the
// cache again. Not for parallel tests.
func Testing_SetZeroContractCost(ctx context.Context, zeroCost bool) func() {
	popConfig := server.Config.PushSimpleResource(NetworkDegradationResourceName, []byte("enabled: true\n"))
	openCount, closeCount := int64(10), int64(10)
	if zeroCost {
		closeCount = 0
	}
	next, err := json.Marshal(nextContractDegradationState(nil, openCount, closeCount, server.NowUtc()))
	server.Raise(err)
	server.Redis(ctx, func(client server.RedisClient) {
		server.Raise(client.Set(ctx, contractDegradationStateKey, string(next), ContractDegradationStateTtl).Err())
	})
	Testing_ResetZeroContractCostCache()
	return func() {
		server.Redis(context.WithoutCancel(ctx), func(client server.RedisClient) {
			server.Raise(client.Del(context.WithoutCancel(ctx), contractDegradationStateKey).Err())
		})
		popConfig()
		Testing_ResetZeroContractCostCache()
	}
}
