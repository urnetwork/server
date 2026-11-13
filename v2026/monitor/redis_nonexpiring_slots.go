// Bound native topology bytes and retain producer completion before deriving
// owned-slot counts for the non-expiring fleet census (SIGNALS.md §3.3c).
package monitor

// A private FIFO bounds the retained response to 256 KiB plus one overflow
// byte. Wait for redis-cli independently of the reader; a positive prefix from
// a failed producer has no slot authority. Raw topology never leaves the host.
const redisNonexpiringSlotsShell = `redis_keyspace_slots() (
  umask 077
  redis_slots_dir=$(mktemp -d "${TMPDIR:-/tmp}/monitor-redis-slots.XXXXXXXX") || exit 1
  redis_slots_pid=
  trap 'if [ -n "$redis_slots_pid" ]; then kill "$redis_slots_pid" 2>/dev/null; wait "$redis_slots_pid" 2>/dev/null; fi; rm -f "$redis_slots_dir/wire" "$redis_slots_dir/body" "$redis_slots_dir/invalid" "$redis_slots_dir/last"; rmdir "$redis_slots_dir"' 0
  trap 'exit 1' HUP INT TERM
  mkfifo "$redis_slots_dir/wire" || exit 1
  timeout 3 redis-cli -p "$p" --raw CLUSTER NODES >"$redis_slots_dir/wire" 2>/dev/null &
  redis_slots_pid=$!
  head -c 262145 <"$redis_slots_dir/wire" >"$redis_slots_dir/body"
  redis_slots_read_exit=$?
  wait "$redis_slots_pid"
  redis_slots_native_exit=$?
  redis_slots_pid=
  [ "$redis_slots_native_exit" -eq 0 ] && [ "$redis_slots_read_exit" -eq 0 ] || exit 1
  redis_slots_bytes=$(wc -c <"$redis_slots_dir/body") || exit 1
  [ "$redis_slots_bytes" -gt 0 ] && [ "$redis_slots_bytes" -le 262144 ] || exit 1
  tail -c 1 <"$redis_slots_dir/body" >"$redis_slots_dir/last" || exit 1
  redis_slots_last_lines=$(wc -l <"$redis_slots_dir/last") || exit 1
  [ "$redis_slots_last_lines" -eq 1 ] || exit 1
  LC_ALL=C tr -d '\012\015\040-\176' <"$redis_slots_dir/body" >"$redis_slots_dir/invalid" || exit 1
  [ ! -s "$redis_slots_dir/invalid" ] || exit 1
  LC_ALL=C awk -v expected_port="$p" '
    function uint(value) {
      return value ~ /^(0|[1-9][0-9]*)$/ &&
        (length(value)<20 || (length(value)==20 && "x"value<="x18446744073709551615"))
    }
    function node_id(value) {return length(value)==40 && value !~ /[^0-9a-f]/}
    function slot(value) {return uint(value) && length(value)<=5 && value+0<16384}
    function port(value) {return uint(value) && length(value)<=5 && value+0<=65535}
    function range(first, last, own, key) {
      if(!slot(first) || !slot(last) || first+0>last+0) {bad=1;return}
      for(key=first+0;key<=last+0;key++) {
        if(owners[key]++) {bad=1;return}
        if(own)slots++
      }
    }
    BEGIN {bad=0;self=0;slots=0;rows=0}
    {
      if(bad)exit 1
      sub(/\r$/, "")
      if($0 ~ /[^ -~]/) {bad=1;next}
      if(NF==0)next
      if(NF<8 || !node_id($1) || ids[$1]++) {bad=1;next}
      rows++
      own=0;master=0;replica=0;unsafe=0
      n=split($3, flags, ",")
      for(i=1;i<=n;i++) {
        if(flag_seen[rows,flags[i]]++)bad=1
        if(flags[i]=="myself")own=1
        else if(flags[i]=="master")master=1
        else if(flags[i]=="slave")replica=1
        else if(flags[i]=="fail" || flags[i]=="fail?" || flags[i]=="handshake" || flags[i]=="noaddr" || flags[i]=="noflags")unsafe=1
        else if(flags[i]!="nofailover")bad=1
      }
      split($2, address, ",")
      if(split(address[1], endpoint, "@")!=2 || endpoint[1] !~ /:/) {bad=1;next}
      client_port=endpoint[1];sub(/^.*:/, "", client_port)
      if(!port(client_port) || !port(endpoint[2]) || ($4!="-" && !node_id($4)) ||
         !uint($5) || !uint($6) || !uint($7) || ($8!="connected" && $8!="disconnected") ||
         (master && replica) || (master && $4!="-"))bad=1
      if(own) {
        self++
        if(!master || replica || unsafe || client_port+0!=expected_port+0 || endpoint[2]+0==0 || $8!="connected")bad=1
      }
      for(i=9;i<=NF;i++) {
        if(!master) {bad=1;continue}
        if($i ~ /^[0-9]+$/)range($i, $i, own)
        else if($i ~ /^[0-9]+-[0-9]+$/) {
          split($i, ends, "-");range(ends[1], ends[2], own)
        } else if($i ~ /^\[[0-9]+-(>|<)-[0-9a-f]+\]$/) {
          annotation=$i;sub(/^\[/, "", annotation);sub(/\]$/, "", annotation)
          split(annotation, parts, "-")
          if(!own || !slot(parts[1]) || !node_id(parts[3]) || annotations[parts[1],parts[2]]++)bad=1
        } else bad=1
      }
    }
    END {if(bad || rows==0 || self!=1 || slots<=0 || slots>16384)exit 1;print slots}
  ' "$redis_slots_dir/body"
) 2>/dev/null
`
