// Redis byte-family attribution (SIGNALS.md §3.3b): which bounded key family
// owns the memory, rather than merely the largest key count. Keep this probe
// paired with signal_redis_bytes_test.go when the catalog entry changes.
package monitor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"
)

func NewRedisBytesSignal() Signal {
	return &signalAdapter{
		number: "3.3b",
		key:    "redis-bytes",
		name:   "Redis sampled byte-family attribution",
		probe:  redisBytesProbe{},
	}
}

type redisBytesProbe struct{}

const (
	redisByteSampleKeyLimit    = 1000
	redisByteSampleBatchLimit  = 20
	redisByteSampleScanCount   = 250
	redisByteMinimumMeasured   = 500
	redisByteDominanceFraction = 0.50
	redisByteDominanceMinimum  = int64(128 << 10)
	// This mirrors model.clientScoreAliasReadyKey/value without importing the
	// model package into monitoring. The key is a durable rollout boundary:
	// writers publish it only after the compatibility export completes.
	redisScoreAliasReadyKey   = "client_score_alias_v1_ready"
	redisScoreAliasReadyValue = "1"
)

var redisByteFamilyOrder = []string{"score", "provide", "client-key", "connect", "stream", "other"}

// Classification stays inside Redis so arbitrary binary keys never enter a
// shell variable or monitor output. MEMORY USAGE SAMPLES 1 bounds collection
// cost while preserving the large-value discriminator that a key-count
// histogram cannot see.
const redisByteFamiliesLua = `
local cursor = '0'
local seen = 0
local measured = 0
local total_bytes = 0
local batches = 0
local sample_limit = tonumber(ARGV[1])
local batch_limit = tonumber(ARGV[2])
local scan_count = tonumber(ARGV[3])
local names = {'score', 'provide', 'client-key', 'connect', 'stream', 'other'}
local counts = {}
local bytes = {}
for _, name in ipairs(names) do
  counts[name] = 0
  bytes[name] = 0
end

local function family(key)
  if string.sub(key, 1, 4) == '{cs_' then return 'score' end
  if string.sub(key, 1, 4) == '{pm_' then return 'provide' end
  if string.sub(key, 1, 5) == 'ckey_' then return 'client-key' end
  if string.find(key, '}s_sk_', 1, true) or string.find(key, '}s2_sk_', 1, true) then return 'stream' end
  if string.find(key, '}s2_c_', 1, true) then return 'connect' end
  return 'other'
end

repeat
  local result = redis.call('SCAN', cursor, 'COUNT', scan_count)
  cursor = result[1]
  batches = batches + 1
  for _, key in ipairs(result[2]) do
    if seen < sample_limit then
      seen = seen + 1
      local usage = redis.call('MEMORY', 'USAGE', key, 'SAMPLES', 1)
      if usage then
        local name = family(key)
        measured = measured + 1
        total_bytes = total_bytes + usage
        counts[name] = counts[name] + 1
        bytes[name] = bytes[name] + usage
      end
    end
  end
until cursor == '0' or batches >= batch_limit or seen >= sample_limit

local response = {seen, measured, total_bytes}
for _, name in ipairs(names) do
  table.insert(response, name)
  table.insert(response, counts[name])
  table.insert(response, bytes[name])
end
return response`

type redisByteFamily struct {
	keys  int64
	bytes int64
}

type redisByteSample struct {
	seen       int64
	measured   int64
	totalBytes int64
	families   map[string]redisByteFamily
}

// Keep typed observation causes available to lifecycle/admission reducers while
// withholding native command diagnostics from error text and public context.
type redisScoreAliasObservationError struct {
	cause error
}

// Expose a fixed boundary instead of optional source text.
func (self *redisScoreAliasObservationError) Error() string {
	return "Redis score-alias observation unavailable"
}

// Retain cancellation and admission causes for the owning reducers.
func (self *redisScoreAliasObservationError) Unwrap() error { return self.cause }

func (redisBytesProbe) id() string             { return "redis/byte-families" }
func (redisBytesProbe) tier() string           { return tierWarn }
func (redisBytesProbe) cadence() time.Duration { return 15 * time.Minute }

func (redisBytesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	entryHost := env.cfg.redisEntryHost()
	if entryHost == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	// the fullest node may be on any redis-cluster host; sample it where it runs.
	// Missing or unranked nodes leave fleet selection unknown; sample the
	// fullest observed bounded node with an explicit coverage qualification.
	h, fullest, failures, err := fullestRedisNodeUsage(ctx, env)
	if err != nil {
		return nil, err
	}
	visibility := []finding{}
	for _, failure := range failures {
		visibility = append(visibility, redisUsageCannotObserveFinding(failure, "byte-families"))
	}
	sample, err := sampleRedisByteFamilies(ctx, env, h, fullest.port)
	if err != nil {
		return nil, err
	}
	if sample.measured < redisByteMinimumMeasured {
		return nil, fmt.Errorf("Redis byte sample measured only %d of %d required keys", sample.measured, redisByteMinimumMeasured)
	}

	target := fmt.Sprintf("%s:%d", h.name, fullest.port)
	usedFraction := float64(fullest.usedBytes) / float64(fullest.maxmemoryBytes)
	score := sample.families["score"]
	scoreByteFraction := float64(score.bytes) / float64(sample.totalBytes)
	scoreKeyFraction := float64(score.keys) / float64(sample.measured)
	observedParts := []string{
		fmt.Sprintf("node_used_bytes=%d", fullest.usedBytes),
		fmt.Sprintf("node_maxmemory_bytes=%d", fullest.maxmemoryBytes),
		fmt.Sprintf("node_used_pct=%.1f", 100*usedFraction),
		fmt.Sprintf("sample_seen=%d", sample.seen),
		fmt.Sprintf("sample_measured=%d", sample.measured),
		fmt.Sprintf("sample_bytes=%d", sample.totalBytes),
		fmt.Sprintf("score_key_pct=%.1f", 100*scoreKeyFraction),
		fmt.Sprintf("score_byte_pct=%.1f", 100*scoreByteFraction),
	}
	for _, name := range redisByteFamilyOrder {
		family := sample.families[name]
		observedParts = append(observedParts,
			fmt.Sprintf("%s_keys=%d", name, family.keys),
			fmt.Sprintf("%s_bytes=%d", name, family.bytes),
		)
	}
	if len(failures) > 0 {
		observedParts = append(observedParts, redisUsageSelectionScope(failures))
	}
	glog.Infof("[monitor]redis byte families on %s: %s\n", target, strings.Join(observedParts, " "))

	if 0.85 <= usedFraction &&
		redisByteDominanceFraction <= scoreByteFraction &&
		redisByteDominanceMinimum <= score.bytes {
		aliasesReady, aliasesReadyErr := redisScoreAliasesReady(ctx, env, entryHost)
		action := "Verify the current score-writer artifact and compatibility-export status. If the writer is caller-oriented, deploy the alias-aware score cache: write one zero-caller baseline per target, one-byte aliases for unchanged callers, and full overrides only for callers whose exclusions intersect the target. If the writer is already alias-aware, let its compatibility pass finish and publish the marker before applying the legacy expiry window. Do not delete cache keys or raise maxmemory to mask the amplification."
		mechanism := "The historical client-score cache is keyed by both caller location and target, but most callers do not exclude a network present in a given target. Materializing the same gob counts, filter, and provider samples under every unchanged caller multiplies large values across hundreds of keys; key-count histograms understate this because score keys are fewer but much larger. Sampled family bytes alone do not distinguish legacy duplicates from required current score payloads."
		context := "This is sampled byte attribution, not a full key census. Pair it with node-memory pressure and the five-hour score-cache TTL. Impossible-TTL stream residue is a separate, real defect and must not be assumed to own capacity without its own measured bytes."
		switch {
		case aliasesReadyErr != nil:
			observedParts = append(observedParts, "alias_schema_ready=unknown")
			context += " The ready-marker state is unavailable; no deployment or expiry boundary can be inferred from it."
			action = "Verify the typed client_score_alias_v1_ready result, current score-writer artifact, and publication log before choosing a deployment change. If a caller-oriented writer is independently confirmed, deploy the alias-aware cache; if an alias-aware writer and publication less than five hours old are confirmed, allow the legacy TTL to drain. Do not delete cache keys or raise maxmemory to mask the amplification."
		case aliasesReady:
			observedParts = append(observedParts, "alias_schema_ready=true alias_marker_scope=global alias_publication_time=unknown score_exporter_capability=unverified")
			mechanism += " The durable global ready marker shows that some writer previously completed a compatibility export. It does not attest the current writer artifact or when publication occurred; this marker alone cannot attribute sampled score bytes to an in-progress legacy drain."
			context += " Establish the current score-writer artifact and the original taskworker `client score alias schema ready` log before applying the five-hour expiry window. A retained marker can outlive a writer rollback."
			action = "Verify the current score-writer artifact and marker-publication log. If the alias-aware writer remains active and publication is less than five hours old, allow the legacy TTL to drain and resample. If that window has elapsed or writer capability is unknown, investigate surviving payloads and current writers before choosing a deployment change. Do not redeploy, delete legacy keys, or raise maxmemory merely because the marker is present."
		default:
			observedParts = append(observedParts, "alias_schema_ready=false")
		}
		return append([]finding{{
			probeId: "redis/byte-families", tier: tierWarn,
			class: "score-byte-dominance", target: target, frame: "score", sustain: 1,
			symptom: fmt.Sprintf(
				"score-cache keys account for %.1f%% of sampled Redis bytes on %s, which is %.1f%% full",
				100*scoreByteFraction, target, 100*usedFraction,
			),
			mechanism: mechanism,
			baseline:  "A Redis node above 85% has no score family owning at least 50% and 128KiB of a 1,000-key MEMORY USAGE sample.",
			observed:  strings.Join(observedParts, " "),
			evidence:  "The probe selected the observed bounded node closest to maxmemory; any missing selection authority is qualified in the observed values. It classified keys and summed MEMORY USAGE SAMPLES 1 entirely inside a bounded EVAL_RO. Only aggregate family counts and bytes left Redis.",
			context:   context,
			action:    action,
			verify:    "After one complete alias-aware export plus the five-hour legacy TTL, sampled score bytes fall below 50% or the node falls below 85%; aliases resolve to the same provider sets, excluded callers still use overrides, and OOM/error counters remain flat.",
			playbook:  "SIGNALS.md §3.3b and §5.4",
		}}, visibility...), nil
	}

	if len(failures) > 0 {
		return visibility, nil
	}
	return []finding{healthyFinding("redis/byte-families", tierWarn, "score-byte-dominance", target)}, nil
}

// Preserve Redis null separately from an empty or malformed stored string.
// Protocol failures can arrive with a successful redis-cli process status.
func redisScoreAliasesReady(ctx context.Context, env *probeEnv, h *host) (bool, error) {
	out, err := env.runner.redis(
		ctx,
		h,
		h.redisEntryPort,
		"-c", "--json", "GET", redisScoreAliasReadyKey,
	)
	if err != nil {
		return false, &redisScoreAliasObservationError{cause: err}
	}
	var value *string
	if err := json.Unmarshal([]byte(out), &value); err != nil {
		return false, fmt.Errorf("Redis score-alias ready marker reply invalid")
	}
	if value == nil {
		return false, nil
	}
	if *value != redisScoreAliasReadyValue {
		return false, fmt.Errorf("Redis score-alias ready marker reply invalid")
	}
	return true, nil
}

// Validate the complete code-owned Lua reply before any byte-share decision.
// A valid empty sample remains subject to the probe's minimum-measured guard.
func sampleRedisByteFamilies(ctx context.Context, env *probeEnv, h *host, port int) (redisByteSample, error) {
	encodedScript := base64.StdEncoding.EncodeToString([]byte(redisByteFamiliesLua))
	command := fmt.Sprintf(
		`timeout 45 redis-cli -p %d --raw EVAL_RO "$(printf %%s %s | base64 -d)" 0 %d %d %d`,
		port,
		encodedScript,
		redisByteSampleKeyLimit,
		redisByteSampleBatchLimit,
		redisByteSampleScanCount,
	)
	out, err := env.runner.shell(ctx, h, command)
	if err != nil {
		return redisByteSample{}, err
	}
	fields := strings.Fields(out)
	if len(fields) != 3+3*len(redisByteFamilyOrder) {
		return redisByteSample{}, fmt.Errorf("Redis byte sample reply shape invalid")
	}
	values := make([]int64, len(fields))
	for i, field := range fields {
		if 3 <= i && (i-3)%3 == 0 {
			continue
		}
		value, err := strconv.ParseInt(field, 10, 64)
		if err != nil || value < 0 || strconv.FormatInt(value, 10) != field {
			return redisByteSample{}, fmt.Errorf("Redis byte sample integer invalid")
		}
		values[i] = value
	}
	sample := redisByteSample{
		seen:       values[0],
		measured:   values[1],
		totalBytes: values[2],
		families:   map[string]redisByteFamily{},
	}
	if redisByteSampleKeyLimit < sample.seen || sample.seen < sample.measured {
		return redisByteSample{}, fmt.Errorf("Redis byte sample counts invalid")
	}
	remainingKeys, remainingBytes := sample.measured, sample.totalBytes
	for i, name := range redisByteFamilyOrder {
		position := 3 + 3*i
		if fields[position] != name {
			return redisByteSample{}, fmt.Errorf("Redis byte sample family invalid")
		}
		family := redisByteFamily{keys: values[position+1], bytes: values[position+2]}
		// Each measured key contributes positive bytes to exactly one family.
		// Subtract from the header to validate sums without integer overflow.
		if remainingKeys < family.keys || remainingBytes < family.bytes ||
			(family.keys == 0) != (family.bytes == 0) || family.bytes < family.keys {
			return redisByteSample{}, fmt.Errorf("Redis byte sample totals invalid")
		}
		remainingKeys -= family.keys
		remainingBytes -= family.bytes
		sample.families[name] = family
	}
	if remainingKeys != 0 || remainingBytes != 0 {
		return redisByteSample{}, fmt.Errorf("Redis byte sample totals invalid")
	}
	return sample, nil
}
