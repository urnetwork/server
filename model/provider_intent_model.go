// Provider intent: a top-level client that declares on its connection that it
// provides publicly (the H1 `X-UR-Provide-Intent: 1` header, or
// `Auth.provide_intent` on frame-authenticated transports) is judged as a
// provider instead of counted toward the network's concurrent client limit.
//
// Each client gets one qualification attempt per allowance. An attempt starts
// with a grace in which the client is exempt from the limit. The grace closes
// only once at least one egress probe has been attempted, so a broken probe
// system extends it rather than failing providers. At the end of the grace the
// client qualifies when it is a registered public provider with a passing
// egress probe in the grace, and passes the 12 hour reliability floor once it
// has that much history. A qualified client is re-checked every probe refresh.
// A client that fails holds a normal client slot when one is free, otherwise it
// is over the limit and the connection kicks it while enforcement is on.
//
// The state lives in redis under the network's peer hash tag, next to the
// connected zsets it is counted against, so one script can decide a normal slot
// atomically. Connections refresh a presence entry while they declare intent:
// exemption needs both an exempt status and a live intent connection, so a
// client that reconnects without intent counts as a normal client again. The
// taskworker check (controller.ProviderIntentCheck) is the only writer of
// qualification outcomes and runs only while the client is present.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

// the qualification state of one intent client
type ProviderIntentStatus string

const (
	// in the qualification grace, exempt from the client limit
	ProviderIntentStatusPending ProviderIntentStatus = "pending"
	// qualified as a public provider, exempt and re-checked every refresh
	ProviderIntentStatusQualified ProviderIntentStatus = "qualified"
	// failed qualification and holds a normal client slot
	ProviderIntentStatusNormal ProviderIntentStatus = "normal"
	// failed qualification with no normal slot free. While enforcement is on
	// the connection disconnects the client with the client limit close.
	ProviderIntentStatusOverLimit ProviderIntentStatus = "over_limit"
)

// Failed qualification within the current attempt allowance.
func (self ProviderIntentStatus) Failed() bool {
	return self == ProviderIntentStatusNormal || self == ProviderIntentStatusOverLimit
}

// 0.25 of the probe refresh: the time a new intent provider has to qualify
const providerIntentGraceTimeout = ProviderEgressProbeRefreshAge / 4

// one qualification attempt per client per allowance
const ProviderIntentAttemptAllowance = 8 * time.Hour

// a qualified provider is re-checked at each probe refresh
const providerIntentRecheckTimeout = ProviderEgressProbeRefreshAge

// How often a pending attempt is checked while it waits: in the grace until its
// probe cycle is admitted (each check renews the probe priority, which admits
// the cycle once the provider's location is ready), and after the grace until a
// probe was attempted.
const ProviderIntentGraceExtendTimeout = 15 * time.Minute

// The reliability floor applies once a client has existed this long, so the
// lookback it is judged on is fully covered by the client's own history.
const providerIntentReliabilityHistory = 12 * time.Hour

// the lookback index of the 12 hour reliability window in `ClientLookbacks`
const providerIntentReliabilityLookbackIndex = 2

// A record is kept this long past its last write and never less than its
// allowance, so a failed client cannot start a new attempt early.
const providerIntentRecordRetention = ProviderIntentAttemptAllowance

const (
	providerIntentMutationMaxAttempts = 8
	providerIntentMutationRetryDelay  = 5 * time.Millisecond
)

// a bounded prune per mutation keeps every script call small
const providerIntentPruneLimit = 256

var providerIntentAttemptsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_intent_attempts_total",
	Help: "Provider intent qualification events by a fixed event class",
}, []string{"event"})

var providerIntentSlotDecisionsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_intent_slot_decisions_total",
	Help: "Normal client slot decisions for intent clients that failed qualification, enforced or computed in shadow mode",
}, []string{"decision", "mode"})

// Every label value is exported from the start, so a rate is defined before
// the first event.
func init() {
	for _, event := range []string{
		providerIntentEventStarted,
		providerIntentEventQualified,
		providerIntentEventFailed,
		providerIntentEventGraceExtended,
		providerIntentEventRecheckPassed,
		providerIntentEventRecheckFailed,
		providerIntentEventStopped,
	} {
		providerIntentAttemptsCounter.WithLabelValues(event)
	}
	for _, decision := range []string{"normal_slot", "over_limit"} {
		for _, mode := range []string{"enforced", "shadow"} {
			providerIntentSlotDecisionsCounter.WithLabelValues(decision, mode)
		}
	}
	prometheus.MustRegister(providerIntentAttemptsCounter, providerIntentSlotDecisionsCounter)
}

// the `event` label values of urnetwork_provider_intent_attempts_total
const (
	providerIntentEventStarted       = "started"
	providerIntentEventQualified     = "qualified"
	providerIntentEventFailed        = "failed"
	providerIntentEventGraceExtended = "grace_extended"
	providerIntentEventRecheckPassed = "recheck_passed"
	providerIntentEventRecheckFailed = "recheck_failed"
	providerIntentEventStopped       = "stopped"
)

// Shadow while enforce_concurrent_clients is false: decisions are computed and
// counted, and nothing is refused or kicked.
func providerIntentEnforcementMode() string {
	if Pro().EnforceConcurrentClients {
		return "enforced"
	}
	return "shadow"
}

// The qualification state of one client. Times are UTC.
type ProviderIntentState struct {
	Status ProviderIntentStatus `json:"status"`
	// the start of the latest attempt, or the latest re-check failure. A new
	// attempt is allowed one allowance after it.
	AttemptTime time.Time `json:"attempt_time"`
	// the end of the latest attempt's grace
	GraceEndTime time.Time `json:"grace_end_time"`
	// the next time the check task evaluates the state
	CheckTime time.Time `json:"check_time"`
}

// A new attempt is allowed only for a failed client whose allowance has passed.
func (self *ProviderIntentState) attemptAllowed(now time.Time) bool {
	return self.Status.Failed() && !now.Before(self.AttemptTime.Add(ProviderIntentAttemptAllowance))
}

// Records keep the status in front of the json so the redis scripts can read it
// without decoding json.
func (self *ProviderIntentState) record() string {
	stateJson, err := json.Marshal(self)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%s %s", self.Status, stateJson)
}

// Returns nil for an empty record.
func parseProviderIntentRecord(record string) (*ProviderIntentState, error) {
	if record == "" {
		return nil, nil
	}
	_, stateJson, ok := strings.Cut(record, " ")
	if !ok {
		return nil, fmt.Errorf("Malformed provider intent record.")
	}
	var state ProviderIntentState
	if err := json.Unmarshal([]byte(stateJson), &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// How long the record is kept after a write: the retention, and never less
// than the allowance of a failed attempt.
func (self *ProviderIntentState) expireTime(now time.Time) time.Time {
	expireTime := now.Add(providerIntentRecordRetention)
	if allowanceTime := self.AttemptTime.Add(ProviderIntentAttemptAllowance); expireTime.Before(allowanceTime) {
		expireTime = allowanceTime
	}
	return expireTime
}

// Starts a new pending attempt at `now`. The first check runs at once so the
// provider is requested probe priority at the start of its grace.
func newProviderIntentAttempt(now time.Time) *ProviderIntentState {
	return &ProviderIntentState{
		Status:       ProviderIntentStatusPending,
		AttemptTime:  now,
		GraceEndTime: now.Add(providerIntentGraceTimeout),
		CheckTime:    now,
	}
}

// The state an intent connection starts with: a new attempt when the client
// has none or its allowance passed, else the existing state.
func providerIntentConnectState(state *ProviderIntentState, now time.Time) (next *ProviderIntentState, started bool) {
	if state == nil || state.attemptAllowed(now) {
		return newProviderIntentAttempt(now), true
	}
	return state, false
}

// What the check task learned about one client. Probe and reliability evidence
// covers the window the check asked for.
type providerIntentEvidence struct {
	// provide mode public with provide keys registered
	PublicProvider bool
	// an egress probe of the client completed in the window, pass or fail
	ProbeAttempted bool
	// an egress probe of the client passed in the window
	ProbePassed bool
	// the client's creation time; zero for a missing client
	ClientCreateTime time.Time
	// the 12 hour reliability weight, nil when the client has no score
	ReliabilityWeight *float64
	// the floor the weight must reach
	ReliabilityMinimum float64
	// the provider's egress URL probe cycle is admitted, so the probe system
	// will claim it without further checks in the grace
	ProbeCycleReady bool
}

// The reliability floor applies only with enough history: missing history is
// neutral, like the provider selection gate.
func (self *providerIntentEvidence) reliabilityPasses(now time.Time) bool {
	if self.ClientCreateTime.IsZero() || now.Sub(self.ClientCreateTime) < providerIntentReliabilityHistory {
		return true
	}
	if self.ReliabilityWeight == nil {
		return true
	}
	return self.ReliabilityMinimum <= *self.ReliabilityWeight
}

// The end of the grace requires a passing probe in the grace.
func (self *providerIntentEvidence) qualifies(now time.Time) bool {
	return self.PublicProvider && self.ProbePassed && self.reliabilityPasses(now)
}

// A re-check with no probe attempted in its window keeps the earlier judgment
// on probes, so a broken probe system does not fail qualified providers.
func (self *providerIntentEvidence) requalifies(now time.Time) bool {
	return self.PublicProvider && (!self.ProbeAttempted || self.ProbePassed) && self.reliabilityPasses(now)
}

// The probe evidence window a check needs, and whether it needs evidence at all.
func providerIntentEvidenceSince(state *ProviderIntentState, now time.Time) (since time.Time, needed bool) {
	if now.Before(state.CheckTime) {
		return time.Time{}, false
	}
	switch state.Status {
	case ProviderIntentStatusPending:
		if now.Before(state.GraceEndTime) {
			return time.Time{}, false
		}
		return state.AttemptTime, true
	case ProviderIntentStatusQualified:
		return now.Add(-providerIntentRecheckTimeout), true
	default:
		return time.Time{}, false
	}
}

// One check of the qualification state machine. `decideSlot` means the next
// state failed and needs a normal slot decision (normal, else over limit).
func providerIntentCheckState(
	state *ProviderIntentState,
	evidence *providerIntentEvidence,
	now time.Time,
) (next *ProviderIntentState, event string, decideSlot bool) {
	if now.Before(state.CheckTime) {
		return state, "", false
	}
	switch state.Status {
	case ProviderIntentStatusPending:
		if now.Before(state.GraceEndTime) {
			// in the grace: renew the probe priority until the probe cycle is
			// admitted, then wait for the end of the grace
			next := *state
			next.CheckTime = state.GraceEndTime
			if !evidence.ProbeCycleReady {
				next.CheckTime = now.Add(ProviderIntentGraceExtendTimeout)
				if state.GraceEndTime.Before(next.CheckTime) {
					next.CheckTime = state.GraceEndTime
				}
			}
			return &next, "", false
		}
		if !evidence.ProbeAttempted {
			// the grace does not close before a probe was attempted
			next := *state
			next.CheckTime = now.Add(ProviderIntentGraceExtendTimeout)
			return &next, providerIntentEventGraceExtended, false
		}
		if evidence.qualifies(now) {
			return &ProviderIntentState{
				Status:       ProviderIntentStatusQualified,
				AttemptTime:  state.AttemptTime,
				GraceEndTime: state.GraceEndTime,
				CheckTime:    now.Add(providerIntentRecheckTimeout),
			}, providerIntentEventQualified, false
		}
		return &ProviderIntentState{
			Status:       ProviderIntentStatusNormal,
			AttemptTime:  state.AttemptTime,
			GraceEndTime: state.GraceEndTime,
			CheckTime:    state.AttemptTime.Add(ProviderIntentAttemptAllowance),
		}, providerIntentEventFailed, true
	case ProviderIntentStatusQualified:
		if evidence.requalifies(now) {
			next := *state
			next.CheckTime = now.Add(providerIntentRecheckTimeout)
			return &next, providerIntentEventRecheckPassed, false
		}
		// leaving qualification counts as an attempt, so the client waits a full
		// allowance before it can try again
		return &ProviderIntentState{
			Status:       ProviderIntentStatusNormal,
			AttemptTime:  now,
			GraceEndTime: state.GraceEndTime,
			CheckTime:    now.Add(ProviderIntentAttemptAllowance),
		}, providerIntentEventRecheckFailed, true
	default:
		if state.attemptAllowed(now) {
			return newProviderIntentAttempt(now), providerIntentEventStarted, false
		}
		next := *state
		next.CheckTime = state.AttemptTime.Add(ProviderIntentAttemptAllowance)
		return &next, "", false
	}
}

// all provider intent keys share the network's peer hash tag

// hash: client id bytes -> `status json` record
func providerIntentKey(networkId server.Id) string {
	return fmt.Sprintf("{np_%s}pi", networkId)
}

// zset: client id bytes scored by the presence expiry unix milli of its
// intent connections
func providerIntentPresentKey(networkId server.Id) string {
	return fmt.Sprintf("{np_%s}pi_present", networkId)
}

// zset: client id bytes scored by the record expiry unix milli
func providerIntentExpireKey(networkId server.Id) string {
	return fmt.Sprintf("{np_%s}pi_expire", networkId)
}

// The keys of the scripts, in their KEYS order.
func providerIntentKeys(networkId server.Id) []string {
	return []string{
		providerIntentKey(networkId),
		providerIntentPresentKey(networkId),
		providerIntentExpireKey(networkId),
		networkPeerConnectedKey(networkId),
		networkPeerConnectedProxyKey(networkId),
		networkPeerConnectedProviderKey(networkId),
	}
}

// Counts the live connected top-level clients of a network, except
// `skip_member`: normal clients, and intent clients exempt from the limit (an
// exempt status, a live record and a live intent connection). Peers and
// provider installs follow the same rules, so the registry category never
// changes a count; hosted proxy clients are always normal. `live_min` is the
// decimal unix milli string of the first live expiry.
const providerIntentCountLua = `
local function count_clients(pi_key, present_key, expire_key, connected_key, proxy_key, provider_key, live_min, skip_member)
	local live_min_ms = tonumber(live_min)
	local normal_count = redis.call('ZCOUNT', proxy_key, live_min, '+inf')
	local exempt_count = 0
	for _, registry_key in ipairs({connected_key, provider_key}) do
		local members = redis.call('ZRANGEBYSCORE', registry_key, live_min, '+inf')
		for _, member in ipairs(members) do
			if member ~= skip_member then
				local exempt = false
				local present = redis.call('ZSCORE', present_key, member)
				local expire = redis.call('ZSCORE', expire_key, member)
				if present and expire and live_min_ms <= tonumber(present) and live_min_ms <= tonumber(expire) then
					local record = redis.call('HGET', pi_key, member)
					if record then
						local status = string.match(record, '^(%S+)')
						exempt = status == 'pending' or status == 'qualified'
					end
				end
				if exempt then
					exempt_count = exempt_count + 1
				else
					normal_count = normal_count + 1
				end
			end
		end
	end
	return normal_count, exempt_count
end
`

// Compares the client's record with the one the caller read and, when they
// match, writes the next record. An expired record compares as none. With an
// over limit record the script decides a normal slot atomically with the
// count: the next record when the normal count (without this client) is under
// the limit, else the over limit record. Presence is refreshed when asked.
// Expired records and presences are pruned first, a bounded batch per call.
// Returns {chosen, record}: chosen 0 when the comparison failed (record is the
// current one), 1 for the next record and 2 for the over limit record.
var providerIntentApplyLua = fmt.Sprintf(`%s
local pi_key = KEYS[1]
local present_key = KEYS[2]
local expire_key = KEYS[3]
local connected_key = KEYS[4]
local proxy_key = KEYS[5]
local provider_key = KEYS[6]

local member = ARGV[1]
local expected_record = ARGV[2]
local next_record = ARGV[3]
local over_limit_record = ARGV[4]
local limit = tonumber(ARGV[5])
local now = ARGV[6]
local live_min = ARGV[7]
local record_expiry_ms = ARGV[8]
local presence_expiry_ms = ARGV[9]
local key_ttl_seconds = ARGV[10]
local prune_limit = tonumber(ARGV[11])
local now_ms = tonumber(now)

local expired = redis.call('ZRANGEBYSCORE', expire_key, '-inf', now, 'LIMIT', 0, prune_limit)
for _, expired_member in ipairs(expired) do
	redis.call('HDEL', pi_key, expired_member)
	redis.call('ZREM', expire_key, expired_member)
	redis.call('ZREM', present_key, expired_member)
end
local expired_presences = redis.call('ZRANGEBYSCORE', present_key, '-inf', now, 'LIMIT', 0, prune_limit)
for _, expired_member in ipairs(expired_presences) do
	redis.call('ZREM', present_key, expired_member)
end

local current_record = redis.call('HGET', pi_key, member)
if current_record then
	local current_expire = redis.call('ZSCORE', expire_key, member)
	if current_expire == false or tonumber(current_expire) <= now_ms then
		current_record = false
	end
end
if current_record == false then
	current_record = ''
end
if current_record ~= expected_record then
	return {0, current_record}
end

local chosen = 1
local record = next_record
if over_limit_record ~= '' and 0 < limit then
	local normal_count, exempt_count = count_clients(pi_key, present_key, expire_key, connected_key, proxy_key, provider_key, live_min, member)
	if limit <= normal_count then
		chosen = 2
		record = over_limit_record
	end
end

redis.call('HSET', pi_key, member, record)
redis.call('ZADD', expire_key, record_expiry_ms, member)
if presence_expiry_ms ~= '0' then
	redis.call('ZADD', present_key, presence_expiry_ms, member)
end
redis.call('EXPIRE', pi_key, key_ttl_seconds)
redis.call('EXPIRE', present_key, key_ttl_seconds)
redis.call('EXPIRE', expire_key, key_ttl_seconds)
return {chosen, record}
`, providerIntentCountLua)

// Returns {normal_count, exempt_count} for the network.
var providerIntentCountClientsLua = fmt.Sprintf(`%s
local normal_count, exempt_count = count_clients(KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6], ARGV[1], '')
return {normal_count, exempt_count}
`, providerIntentCountLua)

// Refreshes the presence of a client with a live record and returns the
// record, or false when the client has no live record.
const providerIntentObserveLua = `
local pi_key = KEYS[1]
local present_key = KEYS[2]
local expire_key = KEYS[3]

local member = ARGV[1]
local now_ms = tonumber(ARGV[2])
local presence_expiry_ms = ARGV[3]
local key_ttl_seconds = ARGV[4]

local record = redis.call('HGET', pi_key, member)
local expire = redis.call('ZSCORE', expire_key, member)
if record == false or expire == false or tonumber(expire) <= now_ms then
	return false
end
redis.call('ZADD', present_key, presence_expiry_ms, member)
redis.call('EXPIRE', present_key, key_ttl_seconds)
return record
`

// The outcome of one apply.
type providerIntentApplyResult struct {
	applied bool
	// the record written, or the current record when the comparison failed
	record    string
	overLimit bool
}

// Writes `next` if the client's record is still `expectedRecord`. With
// `decideSlot` the script decides between `next` as a normal client and the
// same state over the limit. A replay of a write that already applied (a lost
// response) compares equal to one of the candidates and reports applied.
func applyProviderIntentState(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	expectedRecord string,
	next *ProviderIntentState,
	decideSlot bool,
	normalLimit int,
	presenceExpireTime time.Time,
	now time.Time,
) (result providerIntentApplyResult) {
	nextRecord := next.record()
	overLimitRecord := ""
	if decideSlot {
		overLimitState := *next
		overLimitState.Status = ProviderIntentStatusOverLimit
		overLimitRecord = overLimitState.record()
	}
	presenceExpiryMs := int64(0)
	if !presenceExpireTime.IsZero() {
		presenceExpiryMs = presenceExpireTime.UnixMilli()
	}
	server.Redis(ctx, func(r server.RedisClient) {
		values, err := r.Eval(
			ctx,
			providerIntentApplyLua,
			providerIntentKeys(networkId),
			string(clientId.Bytes()),
			expectedRecord,
			nextRecord,
			overLimitRecord,
			normalLimit,
			now.UnixMilli(),
			// count only entries whose expiry is still in the future
			now.UnixMilli()+1,
			next.expireTime(now).UnixMilli(),
			presenceExpiryMs,
			int64(networkPeerKeyTtl/time.Second),
			providerIntentPruneLimit,
		).Slice()
		if err != nil {
			panic(err)
		}
		chosen := values[0].(int64)
		record, _ := values[1].(string)
		switch {
		case chosen == 1:
			result = providerIntentApplyResult{applied: true, record: record}
		case chosen == 2:
			result = providerIntentApplyResult{applied: true, record: record, overLimit: true}
		case record == nextRecord:
			// a replay of this write
			result = providerIntentApplyResult{applied: true, record: record}
		case overLimitRecord != "" && record == overLimitRecord:
			result = providerIntentApplyResult{applied: true, record: record, overLimit: true}
		default:
			result = providerIntentApplyResult{applied: false, record: record}
		}
	})
	return
}

// Rate-limits a conflicting retry; false when the context ended.
func waitProviderIntentMutationRetry(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(providerIntentMutationRetryDelay):
		return true
	}
}

// Reads the client's live record. An expired record reads as none.
func getProviderIntentRecord(ctx context.Context, networkId server.Id, clientId server.Id, now time.Time) (record string, state *ProviderIntentState) {
	member := string(clientId.Bytes())
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.Pipeline()
		recordCmd := pipe.HGet(ctx, providerIntentKey(networkId), member)
		expireCmd := pipe.ZScore(ctx, providerIntentExpireKey(networkId), member)
		_, err := pipe.Exec(ctx)
		if err != nil && err != server.RedisNil {
			panic(err)
		}
		record_, err := recordCmd.Result()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		expiryMs, err := expireCmd.Result()
		if err == server.RedisNil || (err == nil && int64(expiryMs) <= now.UnixMilli()) {
			return
		}
		if err != nil {
			panic(err)
		}
		record = record_
	})
	state, err := parseProviderIntentRecord(record)
	if err != nil {
		// an unreadable record is replaced by the next write
		return record, nil
	}
	return record, state
}

// Reads a client's live provider intent state, or nil.
func GetProviderIntentState(ctx context.Context, networkId server.Id, clientId server.Id) *ProviderIntentState {
	_, state := getProviderIntentRecord(ctx, networkId, clientId, server.NowUtc())
	return state
}

// Writes a client's state as the check would,
// with a live intent connection when `presenceTimeout` is positive.
func Testing_SetProviderIntentState(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	state *ProviderIntentState,
	presenceTimeout time.Duration,
) {
	now := server.NowUtc()
	record, _ := getProviderIntentRecord(ctx, networkId, clientId, now)
	presenceExpireTime := time.Time{}
	if 0 < presenceTimeout {
		presenceExpireTime = now.Add(presenceTimeout)
	}
	result := applyProviderIntentState(ctx, networkId, clientId, record, state, false, 0, presenceExpireTime, now)
	if !result.applied {
		panic(fmt.Errorf("Provider intent state not applied."))
	}
}

// The normal client limit of a network's plan, whether or not it is enforced:
// its Embed plan allowance when it has one, otherwise its tier's. Zero or less
// is unlimited.
func networkNormalClientLimit(ctx context.Context, networkId server.Id) int {
	return networkConcurrentClientLimit(ctx, networkId)
}

// The outcome of an intent connection's start.
type ProviderIntentConnectResult struct {
	State *ProviderIntentState
	// a new qualification attempt started
	AttemptStarted bool
	// the check task must be scheduled now: a new attempt, or a check that is
	// overdue because the task chain stopped while the client was away
	ScheduleCheck bool
}

// Starts the provider intent of a client connection that declared intent: it
// begins a new attempt when allowed, decides a normal slot for a client that
// failed, and refreshes the client's presence.
func ConnectProviderIntent(ctx context.Context, networkId server.Id, clientId server.Id, presenceTimeout time.Duration) ProviderIntentConnectResult {
	return connectProviderIntentAt(ctx, networkId, clientId, presenceTimeout, server.NowUtc())
}

// Connects at an explicit time, for deterministic tests.
func connectProviderIntentAt(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	presenceTimeout time.Duration,
	now time.Time,
) (result ProviderIntentConnectResult) {
	normalLimit := 0
	normalLimitLoaded := false
	for attemptIndex := 0; attemptIndex < providerIntentMutationMaxAttempts; attemptIndex += 1 {
		record, state := getProviderIntentRecord(ctx, networkId, clientId, now)
		next, started := providerIntentConnectState(state, now)
		decideSlot := !started && next.Status.Failed()
		if decideSlot && !normalLimitLoaded {
			normalLimit = networkNormalClientLimit(ctx, networkId)
			normalLimitLoaded = true
		}
		if decideSlot {
			// a failed client decides its slot again on each connection
			next = &ProviderIntentState{
				Status:       ProviderIntentStatusNormal,
				AttemptTime:  next.AttemptTime,
				GraceEndTime: next.GraceEndTime,
				CheckTime:    next.CheckTime,
			}
		}
		applyResult := applyProviderIntentState(
			ctx,
			networkId,
			clientId,
			record,
			next,
			decideSlot,
			normalLimit,
			now.Add(presenceTimeout),
			now,
		)
		if !applyResult.applied {
			if !waitProviderIntentMutationRetry(ctx) {
				break
			}
			continue
		}
		appliedState, err := parseProviderIntentRecord(applyResult.record)
		server.Raise(err)
		result.State = appliedState
		result.AttemptStarted = started
		// a check due before this connection means no task is pending for it
		result.ScheduleCheck = started || !now.Before(appliedState.CheckTime)
		if started {
			providerIntentAttemptsCounter.WithLabelValues(providerIntentEventStarted).Inc()
		}
		if decideSlot {
			countProviderIntentSlotDecision(applyResult.overLimit)
		}
		return
	}
	// a client changing continuously keeps its last state for this connection
	_, result.State = getProviderIntentRecord(ctx, networkId, clientId, now)
	return
}

// Counts one normal slot decision in the current enforcement mode.
func countProviderIntentSlotDecision(overLimit bool) {
	decision := "normal_slot"
	if overLimit {
		decision = "over_limit"
	}
	providerIntentSlotDecisionsCounter.WithLabelValues(decision, providerIntentEnforcementMode()).Inc()
}

// Refreshes the presence of an intent connection's client and returns its live
// state, or nil when the client has no live record (the connection then
// connects again).
func ObserveProviderIntent(ctx context.Context, networkId server.Id, clientId server.Id, presenceTimeout time.Duration) *ProviderIntentState {
	return observeProviderIntentAt(ctx, networkId, clientId, presenceTimeout, server.NowUtc())
}

// Observes at an explicit time, for deterministic tests.
func observeProviderIntentAt(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	presenceTimeout time.Duration,
	now time.Time,
) (state *ProviderIntentState) {
	var record string
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Eval(
			ctx,
			providerIntentObserveLua,
			providerIntentKeys(networkId)[:3],
			string(clientId.Bytes()),
			now.UnixMilli(),
			now.Add(presenceTimeout).UnixMilli(),
			int64(networkPeerKeyTtl/time.Second),
		).Text()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		record = value
	})
	state, err := parseProviderIntentRecord(record)
	if err != nil {
		return nil
	}
	return state
}

// Whether a client has a live intent connection.
func providerIntentPresent(ctx context.Context, networkId server.Id, clientId server.Id, now time.Time) (present bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		expiryMs, err := r.ZScore(ctx, providerIntentPresentKey(networkId), string(clientId.Bytes())).Result()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		present = now.UnixMilli() < int64(expiryMs)
	})
	return
}

// Counts the live connected top-level clients of a network that count toward
// its concurrent client limit (normal clients, failed intent clients and hosted
// proxy clients) and, separately, the intent clients exempt from it.
func GetNetworkConnectedClientCounts(ctx context.Context, networkId server.Id) (normalCount int, providerIntentCount int) {
	return getNetworkConnectedClientCountsAt(ctx, networkId, server.NowUtc())
}

// Counts at an explicit time, for deterministic tests.
func getNetworkConnectedClientCountsAt(ctx context.Context, networkId server.Id, now time.Time) (normalCount int, providerIntentCount int) {
	server.Redis(ctx, func(r server.RedisClient) {
		values, err := r.Eval(
			ctx,
			providerIntentCountClientsLua,
			providerIntentKeys(networkId),
			// count only entries whose expiry is still in the future
			now.UnixMilli()+1,
		).Int64Slice()
		if err != nil {
			panic(err)
		}
		normalCount = int(values[0])
		providerIntentCount = int(values[1])
	})
	return
}

// Runs one check of a client's qualification, and returns when the next check
// must run, or nil when the check chain stops: the client has no live record or
// no live intent connection. A stopped chain is started again by the client's
// next intent connection.
func CheckProviderIntent(ctx context.Context, networkId server.Id, clientId server.Id) *time.Time {
	return checkProviderIntentAt(ctx, networkId, clientId, server.NowUtc())
}

// Checks at an explicit time, for deterministic tests.
func checkProviderIntentAt(ctx context.Context, networkId server.Id, clientId server.Id, now time.Time) *time.Time {
	for attemptIndex := 0; attemptIndex < providerIntentMutationMaxAttempts; attemptIndex += 1 {
		record, state := getProviderIntentRecord(ctx, networkId, clientId, now)
		if state == nil {
			// the record expired with the client away
			removeProviderIntentProbePriority(ctx, clientId)
			return nil
		}
		if !providerIntentPresent(ctx, networkId, clientId, now) {
			// cleanup: the chain runs only while the client is connected with
			// intent, and the record expires on its own
			providerIntentAttemptsCounter.WithLabelValues(providerIntentEventStopped).Inc()
			if state.Status == ProviderIntentStatusPending {
				removeProviderIntentProbePriority(ctx, clientId)
			}
			return nil
		}
		if now.Before(state.CheckTime) {
			// an early run, when a connection scheduled a check that was pending
			checkTime := state.CheckTime
			return &checkTime
		}

		evidence := &providerIntentEvidence{}
		if since, needed := providerIntentEvidenceSince(state, now); needed {
			evidence = getProviderIntentEvidence(ctx, clientId, since, now)
		}
		if state.Status == ProviderIntentStatusPending {
			// the grace is judged on probes: get one within the hour. A new
			// attempt of a failed client is due at once and gets it next run.
			evidence.ProbeCycleReady = setProviderIntentProbePriority(ctx, clientId, state.AttemptTime)
		}
		next, event, decideSlot := providerIntentCheckState(state, evidence, now)
		normalLimit := 0
		if decideSlot {
			normalLimit = networkNormalClientLimit(ctx, networkId)
		}
		applyResult := applyProviderIntentState(
			ctx,
			networkId,
			clientId,
			record,
			next,
			decideSlot,
			normalLimit,
			time.Time{},
			now,
		)
		if !applyResult.applied {
			if !waitProviderIntentMutationRetry(ctx) {
				break
			}
			continue
		}
		if event != "" {
			providerIntentAttemptsCounter.WithLabelValues(event).Inc()
		}
		if decideSlot {
			countProviderIntentSlotDecision(applyResult.overLimit)
		}
		if state.Status == ProviderIntentStatusPending && next.Status != ProviderIntentStatusPending {
			removeProviderIntentProbePriority(ctx, clientId)
		}
		checkTime := next.CheckTime
		return &checkTime
	}
	// a state changing continuously is checked again soon
	checkTime := now.Add(time.Minute)
	return &checkTime
}

// Reads what qualifies a provider: a registered public provide key, egress
// probes since `since`, its creation time and its 12 hour reliability.
func getProviderIntentEvidence(ctx context.Context, clientId server.Id, since time.Time, now time.Time) *providerIntentEvidence {
	evidence := &providerIntentEvidence{
		ReliabilityMinimum: providerReliabilityMinimums()[providerIntentReliabilityLookbackIndex],
	}
	server.Db(ctx, func(conn server.PgConn) {
		var clientCreateTime *time.Time
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT
					EXISTS (
						SELECT 1 FROM provide_key
						WHERE client_id = $1 AND provide_mode = $2
					) AS public_provider,
					EXISTS (
						SELECT 1 FROM provider_egress_health_history
						WHERE
							client_id = $1 AND
							url_probe AND
							0 < total_count AND
							$3 <= measured_at AND
							measured_at <= $4
					) OR EXISTS (
						SELECT 1 FROM provider_url_probe_run
						WHERE
							client_id = $1 AND
							$3 <= claimed_at AND
							completed_at IS NOT NULL AND
							completed_at <= $4
					) AS probe_attempted,
					EXISTS (
						SELECT 1 FROM provider_egress_health_history
						WHERE
							client_id = $1 AND
							url_probe AND
							0 < ok_count AND
							$3 <= measured_at AND
							measured_at <= $4
					) AS probe_passed,
					(
						SELECT create_time FROM network_client
						WHERE client_id = $1
					) AS client_create_time,
					(
						SELECT independent_reliability_weight FROM client_connection_reliability_score
						WHERE client_id = $1 AND lookback_index = $5
					) AS reliability_weight
			`,
			clientId,
			ProvideModePublic,
			since.UTC(),
			now.UTC(),
			providerIntentReliabilityLookbackIndex,
		).Scan(
			&evidence.PublicProvider,
			&evidence.ProbeAttempted,
			&evidence.ProbePassed,
			&clientCreateTime,
			&evidence.ReliabilityWeight,
		))
		if clientCreateTime != nil {
			evidence.ClientCreateTime = *clientCreateTime
		}
	})
	return evidence
}

// SQL true when a client is an intent provider in its qualification grace,
// which admits it to egress URL probes before it has reliability history. The
// caller supplies a trusted SQL expression, never request text.
func providerIntentProbePrioritySql(clientIdExpression string) string {
	return fmt.Sprintf(`EXISTS (
		SELECT 1 FROM provider_intent_probe_priority AS intent_priority
		WHERE intent_priority.client_id = %s
	)`, clientIdExpression)
}

// Gives an intent provider in its grace priority for egress URL probes: it is
// admitted without reliability history, and its probe cycle is due from the
// start of its attempt, ahead of everything that became due later. The cycle is
// created or reactivated now when the provider's location is ready, else by the
// next eligibility pass. Returns whether the cycle is admitted.
func setProviderIntentProbePriority(ctx context.Context, clientId server.Id, attemptTime time.Time) (ready bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO provider_intent_probe_priority (
					client_id,
					priority_since,
					update_time
				)
				VALUES ($1, $2, $3)
				ON CONFLICT (client_id) DO UPDATE
				SET
					priority_since = $2,
					update_time = $3
			`,
			clientId,
			attemptTime.UTC(),
			server.NowUtc(),
		))
		updateProviderUrlProbeEligibilityForClient(ctx, tx, clientId)
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE provider_egress_probe_cycle
				SET next_attempt_at = $2
				WHERE
					client_id = $1 AND
					$2 < next_attempt_at
			`,
			clientId,
			attemptTime.UTC(),
		))
		server.Raise(tx.QueryRow(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1 FROM provider_egress_probe_cycle
					WHERE client_id = $1 AND eligible
				)
			`,
			clientId,
		).Scan(&ready))
	})
	return
}

// A pending attempt renews its probe priority every grace extend timeout, so a
// priority this old belongs to a chain that ended without removing it, such as
// the chain of a deleted client.
const providerIntentProbePriorityRetention = 24 * time.Hour

// Drops the probe priorities no check renewed within the retention, a backstop
// for chains that ended without removing theirs.
func RemoveExpiredProviderIntentProbePriorities(ctx context.Context, now time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM provider_intent_probe_priority
				WHERE update_time < $1
			`,
			now.Add(-providerIntentProbePriorityRetention).UTC(),
		))
	})
}

// Ends a provider's probe priority when its attempt resolves or its check chain
// stops.
func removeProviderIntentProbePriority(ctx context.Context, clientId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM provider_intent_probe_priority
				WHERE client_id = $1
			`,
			clientId,
		))
	})
}
