// Retain native INFO status and accept only a complete keyspace protocol frame.
package monitor

import (
	"strconv"
	"strings"
)

// A successful empty keyspace has its section header but no db0 row. Error
// replies and failed commands must never acquire zero defaults through awk.
const redisKeyspaceInfoShell = `redis_keyspace_info() {
  info=$(timeout 3 redis-cli -p "$p" --raw INFO keyspace 2>/dev/null)
  info_exit=$?
  [ "$info_exit" -eq 0 ] && [ -n "$info" ] || return 1
  stats=$(printf '%s\n' "$info" | LC_ALL=C awk '
    function valid_number(value) {
      return value ~ /^(0|[1-9][0-9]*)$/ &&
        (length(value)<19 || (length(value)==19 && "x"value<="x9223372036854775807"))
    }
    function greater(left, right) {
      return length(left)>length(right) || (length(left)==length(right) && "x"left>"x"right)
    }
    BEGIN {section=0;db=0;bad=0;keys=0;expires=0;avg=0}
    {sub(/\r$/, "")}
    /^$/ {next}
    /^# Keyspace$/ {if(section++)bad=1;next}
    {
      if(section!=1 || $0 !~ /^db0:/ || db++) {bad=1;next}
      sub(/^db0:/, "")
      n=split($0, fields, ",")
      if(n!=3) {bad=1;next}
      for(i=1;i<=n;i++) {
        if(split(fields[i], pair, "=")!=2 || !valid_number(pair[2])) {bad=1;continue}
        if(pair[1]=="keys" && !k++)keys=pair[2]
        else if(pair[1]=="expires" && !e++)expires=pair[2]
        else if(pair[1]=="avg_ttl" && !a++)avg=pair[2]
        else bad=1
      }
      if(k!=1 || e!=1 || a!=1 || greater(expires, keys))bad=1
    }
    END {if(bad || section!=1)exit 1;print keys" "expires" "avg}')
  [ "$?" -eq 0 ] && [ -n "$stats" ]
}
`

// Keep valid independent nodes while recording any loss of census authority.
// Duplicate ports invalidate every observation of that port.
func parseRedisTtlNodes(out string, expectedPorts []int) ([]redisTtlNode, bool) {
	expected := map[int]bool{}
	for _, port := range expectedPorts {
		expected[port] = true
	}
	seen := map[int]bool{}
	invalid := map[int]bool{}
	portNodes := map[int]redisTtlNode{}
	complete := len(expected) == len(expectedPorts) && len(expected) > 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		port, err := strconv.Atoi(fields[0])
		if err != nil || !expected[port] {
			complete = false
			continue
		}
		if seen[port] {
			invalid[port] = true
			delete(portNodes, port)
			complete = false
			continue
		}
		seen[port] = true
		if len(fields) != 4 {
			complete = false
			continue
		}
		values := [3]int64{}
		valid := true
		for i, field := range fields[1:] {
			value, err := strconv.ParseInt(field, 10, 64)
			if err != nil || value < 0 {
				valid = false
			}
			values[i] = value
		}
		if !valid || values[1] > values[0] {
			complete = false
			continue
		}
		portNodes[port] = redisTtlNode{port: port, keys: values[0], expires: values[1], averageTtlMillis: values[2]}
	}
	nodes := []redisTtlNode{}
	for _, port := range expectedPorts {
		node, ok := portNodes[port]
		if !ok || invalid[port] {
			complete = false
			continue
		}
		nodes = append(nodes, node)
	}
	return nodes, complete
}

// Numeric projection shared by the strict TTL census reducer.
type redisTtlNode struct {
	port             int
	keys             int64
	expires          int64
	averageTtlMillis int64
}
