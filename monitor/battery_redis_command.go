// The Redis connection collector emits only fixed keys and numeric values.
// Source failures stop this optional command before any result is trusted.
package monitor

import "strconv"

// Preserve one host round trip, the existing two-second latency budget and
// bounded 62-block ownership lookback. Required INFO fields are validated at
// their source before aggregation; a successful final pipeline is insufficient.
func redisConnectionBatteryCommand(port int) string {
	return "redis_port=" + strconv.Itoa(port) + `
export LC_ALL=C
redis_read() { redis-cli --raw -p "$redis_port" "$@" 2>/dev/null; }
info_integers() {
  info=$(redis_read INFO "$1") || return 1
  printf '%s\n' "$info" | awk -v section="$2" -v wanted="$3" -v suffix="$4" '
  BEGIN { n=split(wanted, keys, " "); for (i=1;i<=n;i++) selected[keys[i]]=1 }
  { sub(/\r$/, "") }
  $0=="# " section { if (header++) invalid=1; next }
  $0=="" { next }
  {
    separator=index($0, ":")
    if (!separator) { invalid=1; next }
    key=substr($0,1,separator-1); value=substr($0,separator+1)
    if (selected[key]) {
      if (seen[key]++ || value !~ /^[0-9]+$/ || value+0>9007199254740991) invalid=1
      values[key]=value
    }
  }
  END {
    if (invalid || !header) exit 1
    for (i=1;i<=n;i++) if (!seen[keys[i]]) exit 1
    for (i=1;i<=n;i++) printf "%s%s=%.0f\n",keys[i],suffix,values[keys[i]]
  }'
}
hget_calls() {
  stats=$(redis_read INFO commandstats) || return 1
  printf '%s\n' "$stats" | awk '
  { sub(/\r$/, "") }
  $0=="# Commandstats" { if (header++) invalid=1; next }
  $0=="" { next }
  $0 !~ /^cmdstat_[a-zA-Z0-9_|-]+:/ { invalid=1; next }
  /^cmdstat_hget:/ {
    if (hget++) invalid=1
    sub(/^cmdstat_hget:/, "")
    n=split($0, fields, ",")
    for (i=1;i<=n;i++) {
      valueCount=split(fields[i], value, "=")
      if (value[1]=="calls") {
        if (seen++ || valueCount!=2 || value[2] !~ /^[0-9]+$/ || value[2]+0>9007199254740991) invalid=1
        calls=value[2]+0
      }
    }
    if (!seen) invalid=1
  }
  END { if (invalid || !header) exit 1; printf "%.0f\n",calls }
  '
}
client_list=$(redis_read CLIENT LIST) || exit 1
client_summary=$(printf '%s\n' "$client_list" | awk '` + redisBatteryClientsAwk + `') || exit 1
max_expire_idle=$(printf '%s\n' "$client_summary" | awk '{for(i=1;i<=NF;i++) if($i ~ /^max_expire_idle_s=/) {split($i,v,"=");print v[2]}}')

marker_slot=$(redis_read CLUSTER KEYSLOT client_reliability_stats_blocks) || exit 1
case "$marker_slot" in ''|*[!0-9]*) exit 1 ;; esac
[ "$marker_slot" -le 16383 ] || exit 1
node_rows=$(redis_read CLUSTER NODES) || exit 1
# Resolve the marker owner and this node from one complete slot census. A
# migrating, failed, missing or duplicate owner cannot prove zero ownership.
topology=$(printf '%s\n' "$node_rows" | awk -v wanted="$redis_port" -v marker="$marker_slot" '
{
  if (NF<8 || $8!="connected") { invalid=1; next }
  address=$2; sub(/@.*/,"",address); sub(/^.*:/,"",address)
  if (address !~ /^[0-9]+$/ || address+0<1 || address+0>65535) { invalid=1; next }
  if (address==wanted) queried++
  master=0; replica=0
  n=split($3, flags, ",")
  for (j=1;j<=n;j++) {
    if (flags[j]=="master") master=1
    else if (flags[j]=="slave") replica=1
    else if (flags[j]!="myself") invalid=1
  }
  if (master+replica!=1) invalid=1
  if (!master) { if (NF!=8) invalid=1; next }
  for (i=9;i<=NF;i++) {
    if ($i !~ /^[0-9]+(-[0-9]+)?$/) { invalid=1; continue }
    count=split($i, range, "-"); lo=range[1]+0; hi=(count==1?lo:range[2]+0)
    if (lo>hi || hi>16383) { invalid=1; continue }
    for (slot=lo;slot<=hi;slot++) {
      if (owners[slot]++) invalid=1
      if (slot==marker) markerOwner=address+0
    }
    if (address==wanted) tokens=tokens " " sprintf("%.0f-%.0f",lo,hi)
  }
}
END {
  if (invalid || queried!=1 || markerOwner==0) exit 1
  for (slot=0;slot<16384;slot++) if (owners[slot]!=1) exit 1
  printf "%.0f%s\n",markerOwner,tokens
}') || exit 1
set -- $topology
marker_owner=$1
shift
node_slot_tokens="$*"
slot_owned_by_queried_node() {
  wanted_slot=$1
  for token in $node_slot_tokens; do
    range_start=${token%-*}; range_end=${token#*-}
    if [ "$range_start" -le "$wanted_slot" ] && [ "$wanted_slot" -le "$range_end" ]; then return 0; fi
  done
  return 1
}
current_block=$(($(date +%s) / 60))
lookback_blocks=$((max_expire_idle / 60 + 2))
[ "$lookback_blocks" -le 62 ] || lookback_blocks=62
oldest_block=$((current_block - lookback_blocks + 1))
# Capture each finite range once and check both status and complete contents.
# Empty/partial successful output must not become zero shard ownership.
block_values=$(seq "$oldest_block" "$current_block") || exit 1
shard_values=$(seq 0 31) || exit 1
[ "${#block_values}" -le 2048 ] && [ "${#shard_values}" -le 128 ] || exit 1
expected_block=$oldest_block
for block in $block_values; do
  [ "$block" = "$expected_block" ] || exit 1
  expected_block=$((expected_block + 1))
done
[ "$expected_block" -eq "$((current_block + 1))" ] || exit 1
expected_shard=0
for shard in $shard_values; do
  [ "$shard" = "$expected_shard" ] || exit 1
  expected_shard=$((expected_shard + 1))
done
[ "$expected_shard" -eq 32 ] || exit 1
slot_values=$(
  for block in $block_values; do
    for shard in $shard_values; do printf 'CLUSTER KEYSLOT client_reliability_stats.%s.%s\n' "$block" "$shard"; done
  done | redis-cli --raw -p "$redis_port" 2>/dev/null
) || exit 1
set -- $slot_values
current_shards=0; previous_shards=0; history_max=0; history_max_age=0; history_total=0
for block in $block_values; do
  count=0
  for shard in $shard_values; do
    [ "$#" -gt 0 ] || exit 1
    slot=$1; shift
    case "$slot" in ''|*[!0-9]*) exit 1 ;; esac
    [ "$slot" -le 16383 ] || exit 1
    if slot_owned_by_queried_node "$slot"; then count=$((count + 1)); fi
  done
  age=$((current_block - block))
  if [ "$age" -eq 0 ]; then current_shards=$count; fi
  if [ "$age" -eq 1 ]; then previous_shards=$count; fi
  history_total=$((history_total + count))
  if [ "$count" -ge "$history_max" ]; then history_max=$count; history_max_age=$age; fi
done
[ "$#" -eq 0 ] || exit 1
clients_info=$(info_integers clients Clients blocked_clients '') || exit 1
memory_info=$(info_integers memory Memory 'used_memory mem_clients_normal' _bytes) || exit 1
hget_calls_before=$(hget_calls) || exit 1
# The timeout budget is nominal. Status 0 or 124 plus valid output proves
# neither elapsed duration, timeout ownership nor Redis process continuity.
latency_exit=0
latency_raw=$(timeout 2 redis-cli --raw -p "$redis_port" --latency 2>/dev/null) || latency_exit=$?
case "$latency_exit" in 0|124) ;; *) exit 1 ;; esac
latency_avg=$(printf '%s\n' "$latency_raw" | tr '\r' '\n' | awk '
function number(v) { return v ~ /^[0-9]+([.][0-9]+)?$/ }
NF {
  if (NF==4) { minimum=$1; maximum=$2; average=$3; samples=$4 }
  else if (NF==8 && $1=="min:" && $3=="max:" && $5=="avg:" && $8=="samples)") {
    minimum=$2; maximum=$4; average=$6; samples=$7
    sub(/,$/,"",minimum); sub(/,$/,"",maximum); sub(/^[(]/,"",samples)
  } else { invalid=1; next }
  if (number(minimum) && number(maximum) && number(average) && samples ~ /^[0-9]+$/ && samples+0>0 && minimum+0<=average+0 && average+0<=maximum+0) value=average
  else invalid=1
}
END { if(invalid || value=="") exit 1; print value }
') || exit 1
hget_calls_after=$(hget_calls) || exit 1
hget_delta=$(awk -v before="$hget_calls_before" -v after="$hget_calls_after" 'BEGIN {if(after<before)exit 1;printf "%.0f",after-before}') || exit 1
hget_rate=$(awk -v delta="$hget_delta" 'BEGIN {printf "%.3f",delta/2}') || exit 1
queues=$(ss -lnt 2>/dev/null) || exit 1
queue_summary=$(printf '%s\n' "$queues" | awk -v port="$redis_port" '` + redisBatteryQueueAwk + `') || exit 1

printf 'redis_connection_diagnostics_version=1 queried_node_port=%s reliability_marker_slot=%s reliability_marker_owner_port=%s\n' "$redis_port" "$marker_slot" "$marker_owner"
printf 'reliability_stats_current_block=%s current_reliability_shards_on_node=%s previous_reliability_shards_on_node=%s reliability_shard_count=32 reliability_shard_lookback_blocks=%s reliability_shards_recent_max=%s reliability_shards_recent_max_age_blocks=%s reliability_shards_recent_total=%s\n' "$current_block" "$current_shards" "$previous_shards" "$lookback_blocks" "$history_max" "$history_max_age" "$history_total"
printf '%s\n' "$clients_info" "$memory_info" "$client_summary" "$queue_summary"
printf 'latency_avg_ms=%s hget_nominal_sample_seconds=2 hget_calls_delta=%s hget_calls_per_nominal_second=%s\n' "$latency_avg" "$hget_delta" "$hget_rate"
`
}
