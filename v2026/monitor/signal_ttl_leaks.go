package monitor

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// SIGNALS.md §3.3a maps to signal_ttl_leaks.go and signal_ttl_leaks_test.go.
// The command-side TTL guard catches new writes; this probe catches the
// persistent keyspace left behind by a pre-fix writer.
func NewTTLLeaksSignal() Signal {
	return &signalAdapter{
		number: "3.3a",
		key:    "ttl-leaks",
		name:   "Redis impossible-TTL residue",
		probe:  redisTTLLeaksProbe{},
	}
}

type redisTTLLeaksProbe struct{}

// Annual net-escrow counters can legitimately approach 395 days. A fleet
// average beyond two years cannot be explained by that exception and remains
// far below the historical 913,000-year duration-as-seconds fingerprint.
const redisImpossibleAverageTTL = 2 * 365 * 24 * time.Hour

const redisTTLAttributionLua = `
local cursor = '0'
local seen = 0
local suspect = 0
local legacy_contracts = 0
local legacy_ids = 0
local current_contracts = 0
local current_ids = 0
local other = 0
local suspect_bytes = 0
local max_suspect_bytes = 0
local max_pttl = -1
local max_family = 'none'
local batches = 0
local suspect_limit = tonumber(ARGV[1])
local batch_limit = tonumber(ARGV[2])
local scan_count = tonumber(ARGV[3])

local function family(key)
  if string.sub(key, -8) == '}s_sk_cs' then return 'legacy-contracts' end
  if string.sub(key, -9) == '}s_sk_sid' then return 'legacy-ids' end
  if string.sub(key, -9) == '}s2_sk_cs' then return 'current-contracts' end
  if string.sub(key, -10) == '}s2_sk_sid' then return 'current-ids' end
  return 'other'
end

repeat
  local result = redis.call('SCAN', cursor, 'COUNT', scan_count)
  cursor = result[1]
  batches = batches + 1
  for _, key in ipairs(result[2]) do
    local pttl = redis.call('PTTL', key)
    local key_family = family(key)
    seen = seen + 1
    if pttl > max_pttl then
      max_pttl = pttl
      max_family = key_family
    end
    if pttl > suspect_limit then
      suspect = suspect + 1
	  local usage = redis.call('MEMORY', 'USAGE', key, 'SAMPLES', 1)
	  if usage then
	    suspect_bytes = suspect_bytes + usage
	    if usage > max_suspect_bytes then max_suspect_bytes = usage end
	  end
      if key_family == 'legacy-contracts' then
        legacy_contracts = legacy_contracts + 1
      elseif key_family == 'legacy-ids' then
        legacy_ids = legacy_ids + 1
      elseif key_family == 'current-contracts' then
        current_contracts = current_contracts + 1
      elseif key_family == 'current-ids' then
        current_ids = current_ids + 1
      else
        other = other + 1
      end
    end
  end
until cursor == '0' or batches >= batch_limit

return {seen, suspect, legacy_contracts, legacy_ids, current_contracts,
        current_ids, other, suspect_bytes, max_suspect_bytes, max_pttl,
        max_family}`

type redisTTLAttribution struct {
	seen             int64
	suspect          int64
	legacyContracts  int64
	legacyIDs        int64
	currentContracts int64
	currentIDs       int64
	other            int64
	suspectBytes     int64
	maxSuspectBytes  int64
	maxPTTLMillis    int64
	maxFamily        string
}

func (redisTTLLeaksProbe) id() string             { return "redis/ttl-leaks" }
func (redisTTLLeaksProbe) tier() string           { return tierWarn }
func (redisTTLLeaksProbe) cadence() time.Duration { return 15 * time.Minute }

func (redisTTLLeaksProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	h := env.cfg.hostByRole("redis-cluster")
	if h == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	ports := h.redisNodePorts()
	if len(ports) == 0 {
		return nil, fmt.Errorf("no redis node ports configured")
	}

	// One SSH round trip obtains the local DB key count, expiring-key count,
	// and Redis' average TTL in milliseconds for every node.
	out, err := env.runner.shell(ctx, h, fmt.Sprintf(`for p in $(seq %d %d); do
  k=$(timeout 3 redis-cli -p $p INFO keyspace 2>/dev/null | tr -d '\r')
  [ -z "$k" ] && { echo "$p unreachable"; continue; }
  echo "$p $(echo "$k" | awk -F'[:,=]' '
    BEGIN{keys=0;expires=0;avg=0}
    /^db0:/{keys=$3; expires=$5; avg=$7}
    END{print keys" "expires" "avg}')"
done`, ports[0], ports[len(ports)-1]))
	if err != nil {
		return nil, err
	}

	findings := []finding{}
	parsed := 0
	unreachable := []string{}
	limitMillis := redisImpossibleAverageTTL.Milliseconds()
	type residueNode struct {
		port             int
		keys             int64
		expires          int64
		averageTTLMillis int64
	}
	affected := []residueNode{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "unreachable" {
			unreachable = append(unreachable, fields[0])
			continue
		}
		if len(fields) < 4 {
			continue
		}
		port := atoi(fields[0])
		keys := atoi64(fields[1])
		expires := atoi64(fields[2])
		averageTTLMillis := atoi64(fields[3])
		if port == 0 {
			continue
		}
		parsed++
		if limitMillis < averageTTLMillis {
			affected = append(affected, residueNode{
				port:             port,
				keys:             keys,
				expires:          expires,
				averageTTLMillis: averageTTLMillis,
			})
		}
	}
	if parsed == 0 {
		return nil, fmt.Errorf("no Redis keyspace INFO parsed (unreachable: %s)", strings.Join(unreachable, ","))
	}
	if len(affected) == 0 {
		findings = append(findings, healthyFinding("redis/ttl-leaks", tierWarn, "ttl-leaks", h.name))
	} else {
		affectedPorts := make([]string, 0, len(affected))
		minKeys, maxKeys := affected[0].keys, affected[0].keys
		minExpires, maxExpires := affected[0].expires, affected[0].expires
		maxTTLNode := affected[0]
		for _, node := range affected {
			affectedPorts = append(affectedPorts, fmt.Sprint(node.port))
			minKeys = min(minKeys, node.keys)
			maxKeys = max(maxKeys, node.keys)
			minExpires = min(minExpires, node.expires)
			maxExpires = max(maxExpires, node.expires)
			if maxTTLNode.averageTTLMillis < node.averageTTLMillis {
				maxTTLNode = node
			}
		}
		maxAverageDays := float64(maxTTLNode.averageTTLMillis) / float64((24 * time.Hour).Milliseconds())
		observed := fmt.Sprintf(
			"affected_nodes=%d parsed_nodes=%d configured_nodes=%d affected_ports=%s keys_range=%d-%d expiring_keys_range=%d-%d max_avg_ttl_ms=%d max_avg_ttl_days=%.0f max_node_port=%d",
			len(affected), parsed, len(ports), strings.Join(affectedPorts, ","), minKeys, maxKeys, minExpires, maxExpires, maxTTLNode.averageTTLMillis, maxAverageDays, maxTTLNode.port,
		)
		mechanism := "One or more expiring Redis key families have TTLs far beyond any intentional retention window. A fixed writer does not repair the persistent expiry metadata already stored in the keyspace."
		evidence := "INFO keyspace independently shows the impossible-TTL residue across ports " + strings.Join(affectedPorts, ",") + "."
		action := "Run a bounded, binary-safe PTTL attribution before changing data. Repair the identified writer and cleanup scope; do not raise maxmemory to conceal effectively immortal residue."
		context := "INFO keyspace is read-only and aggregate. The attribution sample runs SCAN and PTTL inside Redis through EVAL_RO, so binary keys never cross a shell variable."
		attribution, attributionErr := sampleRedisTTLAttribution(ctx, env, h, maxTTLNode.port)
		if attributionErr != nil {
			observed += " sample_status=unavailable"
			context += " The bounded sample was unavailable during this tick: " + attributionErr.Error()
		} else {
			observed += fmt.Sprintf(
				" sample_port=%d sample_examined=%d sample_suspect=%d sample_legacy_contracts=%d sample_legacy_ids=%d sample_current_contracts=%d sample_current_ids=%d sample_other=%d sample_suspect_bytes=%d sample_max_suspect_bytes=%d sample_max_pttl_ms=%d sample_max_family=%s",
				maxTTLNode.port, attribution.seen, attribution.suspect, attribution.legacyContracts, attribution.legacyIDs, attribution.currentContracts, attribution.currentIDs, attribution.other, attribution.suspectBytes, attribution.maxSuspectBytes, attribution.maxPTTLMillis, attribution.maxFamily,
			)
			evidence = fmt.Sprintf(
				"A bounded Redis-side sample on port %d examined %d binary-safe keys and found %d over 120 days; legacy_contracts=%d legacy_ids=%d current_contracts=%d current_ids=%d other=%d. Those suspect keys occupied %d sampled bytes (largest %d bytes); the maximum PTTL was %dms in family %s.",
				maxTTLNode.port, attribution.seen, attribution.suspect, attribution.legacyContracts, attribution.legacyIDs, attribution.currentContracts, attribution.currentIDs, attribution.other, attribution.suspectBytes, attribution.maxSuspectBytes, attribution.maxPTTLMillis, attribution.maxFamily,
			)
			streamSuspect := attribution.legacyContracts + attribution.legacyIDs + attribution.currentContracts + attribution.currentIDs
			if 0 < streamSuspect && attribution.other == 0 {
				mechanism = "A pre-fix raw time.Duration reached Redis Lua as nanoseconds and EXPIRE interpreted it as seconds, leaving effectively immortal stream keys."
				if 0 < attribution.legacyContracts+attribution.legacyIDs {
					mechanism += " The first cleanup matched only current s2 stream names, while production residue uses legacy s_sk suffixes."
				}
				mechanism += fmt.Sprintf(" The bounded sample measured %d bytes across those suspect keys; this proves the expiry defect but does not make it the capacity root cause.", attribution.suspectBytes)
				action = "Confirm current stream writers emit no stream-key redis-ttl-suspect lines; diagnose warnings for other key families independently. With explicit maintenance authority, run the corrected binary-safe expire-leaked-ttls cleanup that covers legacy s_sk and current s2_sk names. Use redis-bytes for capacity ownership; do not assume this residue creates material headroom, or raise maxmemory to conceal either defect."
			}
		}
		findings = append(findings, finding{
			probeId: "redis/ttl-leaks", tier: tierWarn,
			class: "ttl-leaks", target: h.name, frame: "duration-as-seconds", sustain: 1,
			symptom: fmt.Sprintf(
				"%d of %d Redis nodes have an impossible average TTL; the fleet maximum is %.0f days on port %d",
				len(affected), len(ports), maxAverageDays, maxTTLNode.port,
			),
			mechanism: mechanism,
			baseline:  "Every node's average TTL stays below two years; the longest intentional Redis exception is an annual net-escrow balance plus 30 days.",
			observed:  observed,
			evidence:  evidence,
			context:   context + " TTL and MEMORY USAGE answer different questions: this signal proves invalid expiry metadata; redis-bytes attributes capacity.",
			action:    action,
			verify:    "No sampled key exceeds its family TTL, every node's avg_ttl returns below two years, and no new TTL warning appears; verify capacity recovery independently with redis-bytes and redis-memory.",
			playbook:  "SIGNALS.md §3.3a, §3.3b, and §4",
		})
	}
	if len(unreachable) > 0 {
		findings = append(findings, finding{
			probeId: "redis/ttl-leaks", tier: tierWarn,
			class: "cannot-observe", target: h.name, frame: strings.Join(unreachable, ","), sustain: 2,
			symptom:  fmt.Sprintf("TTL distribution unavailable from Redis port(s) %s", strings.Join(unreachable, ",")),
			baseline: "INFO keyspace returns from every configured Redis node.",
			observed: "unreachable_ports=" + strings.Join(unreachable, ","),
			action:   "Restore access to the listed nodes, then rerun ttl-leaks.",
			verify:   "Every node returns keys, expires, and avg_ttl.",
			playbook: "SIGNALS.md §1.4",
		})
	}
	return findings, nil
}

func sampleRedisTTLAttribution(ctx context.Context, env *probeEnv, h *host, port int) (redisTTLAttribution, error) {
	encodedScript := base64.StdEncoding.EncodeToString([]byte(redisTTLAttributionLua))
	command := fmt.Sprintf(
		`redis-cli -p %d --raw EVAL_RO "$(printf %%s %s | base64 -d)" 0 %d 10 500`,
		port,
		encodedScript,
		(120 * 24 * time.Hour).Milliseconds(),
	)
	out, err := env.runner.shell(ctx, h, command)
	if err != nil {
		return redisTTLAttribution{}, err
	}
	fields := strings.Fields(out)
	if len(fields) != 11 {
		return redisTTLAttribution{}, fmt.Errorf("expected 11 attribution fields, got %d", len(fields))
	}
	attribution := redisTTLAttribution{
		seen:             atoi64(fields[0]),
		suspect:          atoi64(fields[1]),
		legacyContracts:  atoi64(fields[2]),
		legacyIDs:        atoi64(fields[3]),
		currentContracts: atoi64(fields[4]),
		currentIDs:       atoi64(fields[5]),
		other:            atoi64(fields[6]),
		suspectBytes:     atoi64(fields[7]),
		maxSuspectBytes:  atoi64(fields[8]),
		maxPTTLMillis:    atoi64(fields[9]),
		maxFamily:        fields[10],
	}
	if attribution.seen <= 0 {
		return redisTTLAttribution{}, fmt.Errorf("attribution sample examined no keys")
	}
	return attribution, nil
}
