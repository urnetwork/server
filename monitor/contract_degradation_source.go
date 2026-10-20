package monitor

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Desired Config authority only; no value attests a deployed consumer's cache.
type ContractDegradationConfigStatus string

const (
	ContractDegradationConfigUnknown  ContractDegradationConfigStatus = "unknown"
	ContractDegradationConfigEnabled  ContractDegradationConfigStatus = "enabled"
	ContractDegradationConfigDisabled ContractDegradationConfigStatus = "disabled"
	contractDegradationKey                                            = "network_degradation:zero_contract_cost:v1"
	contractDegradationValueBytes                                     = 1024
	contractDegradationClockSkew                                      = time.Minute
)

func (self ContractDegradationConfigStatus) normalized() ContractDegradationConfigStatus {
	if self == ContractDegradationConfigEnabled || self == ContractDegradationConfigDisabled {
		return self
	}
	return ContractDegradationConfigUnknown
}

// Unlike the fail-closed consumer, a monitor must distinguish an explicit
// disabled intent from missing, unreadable or malformed configuration.
func loadContractDegradationConfig() ContractDegradationConfigStatus {
	resource, err := server.Config.SimpleResource(model.NetworkDegradationResourceName)
	if err != nil {
		return ContractDegradationConfigUnknown
	}
	data, err := resource.BytesE()
	if err != nil {
		return ContractDegradationConfigUnknown
	}
	return inspectContractDegradationConfig(data)
}

func inspectContractDegradationConfig(data []byte) ContractDegradationConfigStatus {
	if len(data) > 4096 {
		return ContractDegradationConfigUnknown
	}
	var document struct {
		Enabled *bool `yaml:"enabled"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if decoder.Decode(&document) != nil || document.Enabled == nil {
		return ContractDegradationConfigUnknown
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ContractDegradationConfigUnknown
	}
	if *document.Enabled {
		return ContractDegradationConfigEnabled
	}
	return ContractDegradationConfigDisabled
}

// EVAL_RO is required, not an optional fallback. One atomic fixed-key read
// supplies source TIME and PTTL. STRLEN bounds GET before allocating its value.
// Only the actual producer's ordered numeric/Boolean/UTC JSON grammar may
// leave Redis; malformed values, arbitrary fields and raw errors are omitted.
// A successful complete frame also establishes installed EVAL_RO support.
const contractDegradationReadScript = `
local clock = redis.call('TIME')
local function frame(status, ttl, value)
 return table.concat({'contract-degradation-v1',clock[1],clock[2],status,tostring(ttl),value,'complete'},'\n')
end
local kind = redis.call('TYPE',KEYS[1])
if type(kind) == 'table' then kind = kind.ok end
if kind == 'none' then return frame('missing',-2,'-') end
if kind ~= 'string' then return frame('wrong-type',0,'-') end
if redis.call('STRLEN',KEYS[1]) > 1024 then return frame('oversized',0,'-') end
local value = redis.call('GET',KEYS[1])
local ttl = redis.call('PTTL',KEYS[1])
if not value then return frame('missing',-2,'-') end
local zero, opened, closed, ratio, healthy, evaluated = string.match(value,'^%{"zero_cost":(%a+),"open_count":(%d+),"close_count":(%d+),"ratio":([%d%.eE%+%-]+),"consecutive_healthy_count":(%d+),"evaluated_at":"([%dT:%.Z%-]+)"%}$')
if (zero ~= 'true' and zero ~= 'false') or not evaluated then return frame('malformed',0,'-') end
return frame('present',ttl,value)
`

// This is the wire order written by json.Marshal(ContractDegradationState).
// Requiring it preserves int64 lexemes and rejects duplicate/missing fields
// without allowing a Lua floating-point conversion to round the counts.
var contractDegradationWirePattern = regexp.MustCompile(`^\{"zero_cost":(?:true|false),"open_count":(?:0|[1-9][0-9]*),"close_count":(?:0|[1-9][0-9]*),"ratio":(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:e[+-]?[0-9]+)?,"consecutive_healthy_count":(?:0|[1-9][0-9]*),"evaluated_at":"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?Z"\}$`)

func contractDegradationUnsigned(raw string) (int64, bool) {
	if raw == "" || len(raw) > 19 {
		return 0, false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	return value, err == nil
}

// Timestamp and PTTL remain independent: publication occurs after its measured
// window ended, so PTTL is not required to equal evaluated_at + one hour.
func parseContractDegradationFrame(raw string, started, completed time.Time) (*model.ContractDegradationState, int64, string) {
	if len(raw) > 2048 || completed.Before(started) || completed.Sub(started) > 30*time.Second {
		return nil, 0, "source-window"
	}
	lines := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	if len(lines) != 7 || lines[0] != "contract-degradation-v1" || lines[6] != "complete" {
		return nil, 0, "framing"
	}
	seconds, secondsOk := contractDegradationUnsigned(lines[1])
	micros, microsOk := contractDegradationUnsigned(lines[2])
	if !secondsOk || !microsOk || micros >= 1000000 {
		return nil, 0, "source-clock"
	}
	observedAt := time.Unix(seconds, micros*1000).UTC()
	if observedAt.Before(started.Add(-contractDegradationClockSkew)) || completed.Add(contractDegradationClockSkew).Before(observedAt) {
		return nil, 0, "source-clock"
	}
	if lines[3] != "present" {
		switch lines[3] {
		case "missing", "wrong-type", "oversized", "malformed":
			return nil, 0, lines[3]
		default:
			return nil, 0, "framing"
		}
	}
	ttlMillis, ttlOk := contractDegradationUnsigned(lines[4])
	if !ttlOk || ttlMillis <= 0 || ttlMillis > model.ContractDegradationStateTtl.Milliseconds() || time.Duration(ttlMillis)*time.Millisecond <= completed.Sub(started) {
		return nil, 0, "ttl"
	}
	value := lines[5]
	if len(value) > contractDegradationValueBytes || !contractDegradationWirePattern.MatchString(value) {
		return nil, 0, "schema"
	}
	var state model.ContractDegradationState
	if json.Unmarshal([]byte(value), &state) != nil || !contractDegradationUsable(&state, observedAt) || !contractDegradationUsable(&state, completed) {
		return nil, 0, "state"
	}
	return &state, ttlMillis, ""
}

// Mirror the producer/consumer rule on its safe integer domain. Impossibly
// large values that would overflow the producer's 7*x or 10*x are unknown,
// never an alternate healthy/degraded decision from different arithmetic.
func contractDegradationUsable(state *model.ContractDegradationState, now time.Time) bool {
	if state.OpenCount < 0 || state.CloseCount < 0 || state.OpenCount > math.MaxInt64/7 || state.CloseCount > math.MaxInt64/10 || state.EvaluatedAt.IsZero() || !now.Before(state.EvaluatedAt.Add(model.ContractDegradationStateTtl)) || now.Add(contractDegradationClockSkew).Before(state.EvaluatedAt) {
		return false
	}
	ratio := float64(0)
	if state.OpenCount > 0 {
		ratio = float64(state.CloseCount) / float64(state.OpenCount)
	}
	if state.Ratio != ratio {
		return false
	}
	healthy := state.OpenCount == 0 || 7*state.OpenCount <= 10*state.CloseCount
	if !healthy {
		return state.ZeroCost && state.ConsecutiveHealthyCount == 0
	}
	return state.ConsecutiveHealthyCount >= 1 && state.ConsecutiveHealthyCount <= model.ContractDegradationResumeChecks && (!state.ZeroCost || state.ConsecutiveHealthyCount < model.ContractDegradationResumeChecks)
}
