// Redis byte-family attribution (SIGNALS.md §3.3b): which bounded key family
// owns the memory, rather than merely the largest key count. Keep this probe
// paired with signal_redis_bytes_test.go when the catalog entry changes.
package monitor

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/glog"
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

func (redisBytesProbe) id() string             { return "redis/byte-families" }
func (redisBytesProbe) tier() string           { return tierWarn }
func (redisBytesProbe) cadence() time.Duration { return 15 * time.Minute }

func (redisBytesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	h := env.cfg.hostByRole("redis-cluster")
	if h == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	fullest, err := fullestRedisNodeUsage(ctx, env, h)
	if err != nil {
		return nil, err
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
	glog.Infof("[monitor]redis byte families on %s: %s\n", target, strings.Join(observedParts, " "))

	if 0.85 <= usedFraction &&
		redisByteDominanceFraction <= scoreByteFraction &&
		redisByteDominanceMinimum <= score.bytes {
		aliasesReady, aliasesReadyErr := redisScoreAliasesReady(ctx, env, h)
		action := "Deploy the alias-aware score cache: write one zero-caller baseline per target, one-byte aliases for unchanged callers, and full overrides only for callers whose exclusions intersect the target. Let the first compatibility pass refresh legacy payloads and publish its ready marker; then let duplicates expire naturally. Do not delete cache keys or raise maxmemory to mask the amplification."
		mechanism := "The client-score cache is keyed by both caller location and target, but most callers do not exclude a network present in a given target. Materializing the same gob counts, filter, and provider samples under every unchanged caller multiplies large values across hundreds of keys; key-count histograms understate this because score keys are fewer but much larger."
		context := "This is sampled byte attribution, not a full key census. Pair it with node-memory pressure and the five-hour score-cache TTL. Impossible-TTL stream residue is a separate, real defect and must not be assumed to own capacity without its own measured bytes."
		switch {
		case aliasesReadyErr != nil:
			observedParts = append(observedParts, "alias_schema_ready=unknown")
			context += " The ready marker could not be read: " + aliasesReadyErr.Error()
			action = "Verify the client_score_alias_v1_ready marker before changing the deployment. If absent, deploy the alias-aware writer and let its compatibility pass publish the marker; if present, do not redeploy or delete keys—allow one full five-hour legacy TTL after publication. Do not raise maxmemory to mask the amplification."
		case aliasesReady:
			observedParts = append(observedParts, "alias_schema_ready=true")
			mechanism += " The durable alias-schema ready marker proves the compatibility export completed; continuing dominance now measures legacy duplicate payloads inside their normal five-hour TTL, not an absent software fix."
			context += " Marker publication has no Redis creation timestamp, so use the taskworker `client score alias schema ready` log as the drain clock."
			action = "The alias-aware software fix is already active. Do not redeploy it, delete legacy score keys, or raise maxmemory. Allow one full five-hour TTL after the ready-marker log, then resample; intervene only if duplicates survive that boundary or Redis reaches an immediate capacity limit before expiry."
		default:
			observedParts = append(observedParts, "alias_schema_ready=false")
		}
		return []finding{{
			probeId: "redis/byte-families", tier: tierWarn,
			class: "score-byte-dominance", target: target, frame: "score", sustain: 1,
			symptom: fmt.Sprintf(
				"score-cache keys account for %.1f%% of sampled Redis bytes on %s, which is %.1f%% full",
				100*scoreByteFraction, target, 100*usedFraction,
			),
			mechanism: mechanism,
			baseline:  "A Redis node above 85% has no single avoidably duplicated family owning at least 50% and 128KiB of a 1,000-key MEMORY USAGE sample.",
			observed:  strings.Join(observedParts, " "),
			evidence:  "The probe selected the node closest to maxmemory, then classified keys and summed MEMORY USAGE SAMPLES 1 entirely inside a bounded EVAL_RO. Only aggregate family counts and bytes left Redis.",
			context:   context,
			action:    action,
			verify:    "After one complete alias-aware export plus the five-hour legacy TTL, sampled score bytes fall below 50% or the node falls below 85%; aliases resolve to the same provider sets, excluded callers still use overrides, and OOM/error counters remain flat.",
			playbook:  "SIGNALS.md §3.3b and §5.4",
		}}, nil
	}

	return []finding{healthyFinding("redis/byte-families", tierWarn, "score-byte-dominance", target)}, nil
}

func redisScoreAliasesReady(ctx context.Context, env *probeEnv, h *host) (bool, error) {
	value, err := env.runner.redis(
		ctx,
		h,
		h.redisEntryPort,
		"-c", "--raw", "GET", redisScoreAliasReadyKey,
	)
	if err != nil {
		return false, err
	}
	return value == redisScoreAliasReadyValue, nil
}

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
	if len(fields) < 3 || (len(fields)-3)%3 != 0 {
		return redisByteSample{}, fmt.Errorf("expected byte sample header plus family triples, got %d fields", len(fields))
	}
	sample := redisByteSample{
		seen:       atoi64(fields[0]),
		measured:   atoi64(fields[1]),
		totalBytes: atoi64(fields[2]),
		families:   map[string]redisByteFamily{},
	}
	for i := 3; i < len(fields); i += 3 {
		sample.families[fields[i]] = redisByteFamily{
			keys:  atoi64(fields[i+1]),
			bytes: atoi64(fields[i+2]),
		}
	}
	if sample.seen <= 0 || sample.measured <= 0 || sample.totalBytes <= 0 {
		return redisByteSample{}, fmt.Errorf("Redis byte sample returned no measurable keys")
	}
	for _, name := range redisByteFamilyOrder {
		if _, ok := sample.families[name]; !ok {
			return redisByteSample{}, fmt.Errorf("Redis byte sample omitted family %q", name)
		}
	}
	return sample, nil
}
