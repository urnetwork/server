package monitor

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
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
	// The residue is a cluster-level judgement reported on the entry host; the
	// census runs on every redis-cluster host over its own node ports. A host
	// that cannot be read is reported and the other hosts' nodes are judged.
	h := env.cfg.redisEntryHost()
	hosts := env.cfg.redisSweepHosts()
	if h == nil || len(hosts) == 0 {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	type hostCensus struct {
		host *host
		out  string
	}
	ports := []int{}
	censuses := []hostCensus{}
	failures := []redisHostFailure{}
	for _, nodeHost := range hosts {
		hostPorts := nodeHost.redisNodePorts()
		if len(hostPorts) == 0 {
			return nil, fmt.Errorf("no redis node ports configured")
		}
		ports = append(ports, hostPorts...)

		// One SSH round trip per host obtains the local DB key count,
		// expiring-key count, and Redis' average TTL in milliseconds for every
		// node.
		portNames := make([]string, len(hostPorts))
		for i, port := range hostPorts {
			portNames[i] = strconv.Itoa(port)
		}
		hostOut, err := env.runner.shell(ctx, nodeHost, redisKeyspaceInfoShell+fmt.Sprintf(`for p in %s; do
  redis_keyspace_info || { echo "$p unreachable"; continue; }
  echo "$p $stats"
done`, strings.Join(portNames, " ")))
		if err != nil {
			failures = append(failures, redisHostFailure{host: nodeHost, err: err})
			continue
		}
		censuses = append(censuses, hostCensus{host: nodeHost, out: hostOut})
	}
	if len(failures) == len(hosts) {
		return nil, redisHostFailuresError(failures)
	}

	findings := []finding{}
	parsed := 0
	type hostUnreachable struct {
		host  *host
		ports []string
	}
	unreachableHosts := []hostUnreachable{}
	limitMillis := redisImpossibleAverageTTL.Milliseconds()
	type residueNode struct {
		host             *host
		port             int
		keys             int64
		expires          int64
		averageTTLMillis int64
	}
	affected := []residueNode{}
	for _, census := range censuses {
		nodes, complete := parseRedisTtlNodes(census.out, census.host.redisNodePorts())
		for _, node := range nodes {
			parsed++
			if limitMillis < node.averageTtlMillis {
				affected = append(affected, residueNode{host: census.host, port: node.port, keys: node.keys, expires: node.expires, averageTTLMillis: node.averageTtlMillis})
			}
		}
		if !complete {
			validPorts := map[int]bool{}
			for _, node := range nodes {
				validPorts[node.port] = true
			}
			portNames := []string{}
			for _, port := range census.host.redisNodePorts() {
				if !validPorts[port] {
					portNames = append(portNames, strconv.Itoa(port))
				}
			}
			// An unexpected row can invalidate scope even if all expected rows exist.
			if len(portNames) == 0 {
				for _, port := range census.host.redisNodePorts() {
					portNames = append(portNames, strconv.Itoa(port))
				}
			}
			unreachableHosts = append(unreachableHosts, hostUnreachable{host: census.host, ports: portNames})
		}
	}
	if len(affected) == 0 {
		// an unread host's nodes may hold the residue
		if len(failures) == 0 && len(unreachableHosts) == 0 {
			findings = append(findings, healthyFinding("redis/ttl-leaks", tierWarn, "ttl-leaks", h.name))
		}
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
		symptom := fmt.Sprintf(
			"%d of %d Redis nodes have an impossible average TTL; the fleet maximum is %.0f days on port %d",
			len(affected), len(ports), maxAverageDays, maxTTLNode.port,
		)
		if len(failures) > 0 {
			// the count and maximum cover only the read hosts' nodes
			observed += " " + redisUnobservedHosts(failures)
			symptom += redisPartialSymptom(failures)
		}
		if len(unreachableHosts) > 0 {
			observed += " census_complete=false"
			symptom += "; the maximum covers only valid observed nodes"
		}
		mechanism := "One or more expiring Redis key families have TTLs far beyond any intentional retention window. A fixed writer does not repair the persistent expiry metadata already stored in the keyspace."
		evidence := "INFO keyspace independently shows the impossible-TTL residue across ports " + strings.Join(affectedPorts, ",") + "."
		action := "Run a bounded, binary-safe PTTL attribution before changing data. Repair the identified writer and cleanup scope; do not raise maxmemory to conceal effectively immortal residue."
		verify := "Every configured node's average TTL returns to at most two years, and a complete binary-safe raw-integer PTTL sample validates the affected family's TTL contract. Verify dataset capacity independently with redis-bytes and redis-memory."
		context := "INFO keyspace is read-only and aggregate. The attribution sample runs SCAN and raw-integer PTTL inside Redis through EVAL_RO, so binary keys never cross a shell variable and multi-century TTLs never overflow a Go time.Duration."
		attribution, attributionErr := sampleRedisTTLAttribution(ctx, env, maxTTLNode.host, maxTTLNode.port)
		if attributionErr != nil {
			observed += " sample_status=unavailable"
			context += " The bounded sample was unavailable during this tick; no key-family attribution is established."
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
				mechanism = "Over-limit keys in the bounded sample belong to stream-key families. The originating writer and cleanup history are unverified; this sample does not establish a duration conversion, a cleanup overflow, or a particular artifact."
				if 0 < attribution.legacyContracts+attribution.legacyIDs {
					mechanism += " Legacy s_sk suffixes are present; that schema label does not identify which binary wrote or restored the keys."
				}
				mechanism += fmt.Sprintf(" The measured %d bytes quantify only sampled suspect stream keys, not the population behind INFO's average or the capacity root cause.", attribution.suspectBytes)
				context += " Historical duration-as-seconds writes and typed-duration PTTL cleanup overflow are investigative hypotheses, not causes established by this sample. Even a very large matching PTTL is not immutable writer or cleanup provenance."
				verify = "Every configured node's average TTL returns to at most two years. Independently verify the current stream TTL contract and original writer or cleanup authority, then validate any separately authorized repair with binary-safe raw-integer PTTL and exact schema scope. Verify capacity recovery independently."
				action = "Confirm the current stream TTL contract, original writer/artifact and any prior cleanup outcome before choosing a repair or cleanup. Do not infer a duration-conversion writer, cleanup overflow or deployed fix from family names or PTTL alone. Any separately authorized maintenance requires schema-specific dry-run review and binary-safe raw-integer PTTL handling; do not run a historical cleanup or delete keys solely from this sample."
			}
		}
		findings = append(findings, finding{
			probeId: "redis/ttl-leaks", tier: tierWarn,
			class: "ttl-leaks", target: h.name, frame: "duration-as-seconds", sustain: 1,
			symptom:   symptom,
			mechanism: mechanism,
			baseline:  "Every node's average TTL stays at most two years; the longest intentional Redis exception is an annual net-escrow balance plus 30 days.",
			observed:  observed,
			evidence:  evidence,
			context:   context + " TTL and MEMORY USAGE answer different questions: this signal proves invalid expiry metadata; redis-bytes attributes capacity.",
			action:    action,
			verify:    verify,
			playbook:  "SIGNALS.md §3.3a, §3.3b, and §4",
		})
	}
	// unreachable nodes are reported on the host that runs them
	for _, hostUnreachable := range unreachableHosts {
		unreachable := hostUnreachable.ports
		findings = append(findings, finding{
			probeId: "redis/ttl-leaks", tier: tierWarn,
			class: "cannot-observe", target: hostUnreachable.host.name, frame: strings.Join(unreachable, ","), sustain: 2,
			symptom:   fmt.Sprintf("TTL census incomplete for configured Redis port(s) %s", strings.Join(unreachable, ",")),
			mechanism: "A native INFO command failed, its keyspace frame was invalid, or the configured-port census was incomplete; the missing state is unknown rather than healthy zero.",
			baseline:  "Every configured Redis node returns one successful, valid INFO keyspace observation.",
			observed:  "census_incomplete_configured_ports=" + strings.Join(unreachable, ","),
			action:    "Check native INFO success, permissions, and keyspace response shape for the configured nodes; restore complete valid observations and rerun ttl-leaks.",
			verify:    "Every configured port returns exactly once with valid keyspace counts and average TTL, including a successful empty database.",
			playbook:  "SIGNALS.md §1.4",
		})
	}
	for _, failure := range failures {
		findings = append(findings, redisHostCannotObserveFinding(failure, "ttl-census"))
	}
	return findings, nil
}

// Validate the complete fixed aggregate before it can identify a key family.
// Raw transport errors and unknown labels never become alert text.
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
		return redisTTLAttribution{}, fmt.Errorf("bounded Redis TTL attribution command failed")
	}
	fields := strings.Fields(out)
	if len(fields) != 11 {
		return redisTTLAttribution{}, fmt.Errorf("bounded Redis TTL attribution returned an invalid aggregate shape")
	}
	values := [10]int64{}
	for index, field := range fields[:10] {
		value, err := strconv.ParseInt(field, 10, 64)
		minimum := int64(0)
		if index == 9 {
			// The Lua maximum starts at -1 when no sampled key has a TTL.
			minimum = -1
		}
		if err != nil || value < minimum {
			return redisTTLAttribution{}, fmt.Errorf("bounded Redis TTL attribution returned an invalid numeric aggregate")
		}
		values[index] = value
	}
	attribution := redisTTLAttribution{
		seen:             values[0],
		suspect:          values[1],
		legacyContracts:  values[2],
		legacyIDs:        values[3],
		currentContracts: values[4],
		currentIDs:       values[5],
		other:            values[6],
		suspectBytes:     values[7],
		maxSuspectBytes:  values[8],
		maxPTTLMillis:    values[9],
		maxFamily:        fields[10],
	}
	if attribution.seen <= 0 || attribution.suspect > attribution.seen {
		return redisTTLAttribution{}, fmt.Errorf("bounded Redis TTL attribution returned inconsistent sample counts")
	}
	remaining := attribution.suspect
	for _, count := range values[2:7] {
		if count > remaining {
			return redisTTLAttribution{}, fmt.Errorf("bounded Redis TTL attribution returned inconsistent family counts")
		}
		remaining -= count
	}
	if remaining != 0 || attribution.maxSuspectBytes > attribution.suspectBytes ||
		(attribution.suspectBytes > 0 && attribution.maxSuspectBytes == 0) ||
		(attribution.suspect == 0 && attribution.suspectBytes != 0) ||
		(attribution.maxSuspectBytes > 0 && (attribution.suspectBytes-1)/attribution.maxSuspectBytes >= attribution.suspect) {
		return redisTTLAttribution{}, fmt.Errorf("bounded Redis TTL attribution returned inconsistent aggregates")
	}
	maxFamilySuspect := int64(0)
	switch attribution.maxFamily {
	case "none":
		if attribution.maxPTTLMillis != -1 {
			return redisTTLAttribution{}, fmt.Errorf("bounded Redis TTL attribution returned an inconsistent maximum family")
		}
	case "legacy-contracts":
		maxFamilySuspect = attribution.legacyContracts
	case "legacy-ids":
		maxFamilySuspect = attribution.legacyIDs
	case "current-contracts":
		maxFamilySuspect = attribution.currentContracts
	case "current-ids":
		maxFamilySuspect = attribution.currentIDs
	case "other":
		maxFamilySuspect = attribution.other
	default:
		return redisTTLAttribution{}, fmt.Errorf("bounded Redis TTL attribution returned an invalid family label")
	}
	limitMillis := (120 * 24 * time.Hour).Milliseconds()
	if (attribution.maxPTTLMillis == -1 && attribution.maxFamily != "none") ||
		(attribution.suspect == 0 && attribution.maxPTTLMillis > limitMillis) ||
		(attribution.suspect > 0 && (attribution.maxPTTLMillis <= limitMillis || maxFamilySuspect == 0)) {
		return redisTTLAttribution{}, fmt.Errorf("bounded Redis TTL attribution returned inconsistent TTL aggregates")
	}
	return attribution, nil
}
