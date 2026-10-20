// Reconcile the bounded keyspace sampler's INFO-memory census without treating
// absent measurements or an unlimited ceiling as fleet selection authority.
package monitor

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Retain only configured port numbers and fixed classifications. Raw Redis
// replies must never enter errors, observations, logs, or alert identities.
type redisNodeUsageCensusError struct {
	unobservedPorts []int
	unboundedPorts  []int
	invalidRows     bool
}

// Missing measurements differ from valid maxmemory=0, which cannot be ranked
// by utilization. Either boundary prevents a complete fleet-fullest sample.
func (self *redisNodeUsageCensusError) Error() string {
	return fmt.Sprintf("Redis node usage selection unavailable: unobserved_ports=%v unbounded_ports=%v invalid_rows=%t",
		self.unobservedPorts, self.unboundedPorts, self.invalidRows)
}

// Require one unambiguous row per configured node. Keep valid zero usage and
// valid sibling rows; ignore foreign ports and reject every duplicated node.
func parseRedisNodeUsage(h *host, out string) ([]redisNodeUsage, error) {
	ports := h.redisNodePorts()
	expectedPorts := make(map[int]bool, len(ports))
	seenPorts := make(map[int]bool, len(ports))
	nodeByPort := make(map[int]redisNodeUsage, len(ports))
	for _, port := range ports {
		expectedPorts[port] = true
	}
	censusErr := &redisNodeUsageCensusError{}
	parseBytes := func(value string) (int64, bool) {
		if value == "" {
			return 0, false
		}
		for _, digit := range value {
			if digit < '0' || '9' < digit {
				return 0, false
			}
		}
		number, err := strconv.ParseInt(value, 10, 64)
		return number, err == nil
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		port, err := strconv.Atoi(fields[0])
		if err != nil || strconv.Itoa(port) != fields[0] || !expectedPorts[port] {
			censusErr.invalidRows = true
			continue
		}
		if seenPorts[port] {
			delete(nodeByPort, port)
			censusErr.invalidRows = true
			continue
		}
		seenPorts[port] = true
		if len(fields) != 3 {
			censusErr.invalidRows = censusErr.invalidRows || len(fields) != 2 || fields[1] != "unreachable"
			continue
		}
		usedBytes, usedOk := parseBytes(fields[1])
		maxmemoryBytes, maxmemoryOk := parseBytes(fields[2])
		if !usedOk || !maxmemoryOk {
			censusErr.invalidRows = true
			continue
		}
		nodeByPort[port] = redisNodeUsage{port: port, usedBytes: usedBytes, maxmemoryBytes: maxmemoryBytes}
	}
	nodes := make([]redisNodeUsage, 0, len(ports))
	for _, port := range ports {
		if node, ok := nodeByPort[port]; ok {
			nodes = append(nodes, node)
			if node.maxmemoryBytes == 0 {
				censusErr.unboundedPorts = append(censusErr.unboundedPorts, port)
			}
		} else {
			censusErr.unobservedPorts = append(censusErr.unobservedPorts, port)
		}
	}
	if len(censusErr.unobservedPorts) > 0 || len(censusErr.unboundedPorts) > 0 || censusErr.invalidRows {
		return nodes, censusErr
	}
	return nodes, nil
}

// Keep the existing host/observation identity, with exact in-host gaps when
// available. A valid unlimited ceiling is observed but unranked, not unreachable.
func redisUsageCannotObserveFinding(failure redisHostFailure, observation string) finding {
	unknown := cannotObserveFinding(failure.host.name+"/"+observation, failure.err)
	var censusErr *redisNodeUsageCensusError
	if errors.As(failure.err, &censusErr) {
		unknown.observed += fmt.Sprintf(" unobserved_nodes=%d unobserved_ports=%v unbounded_nodes=%d unbounded_ports=%v invalid_rows=%t",
			len(censusErr.unobservedPorts), censusErr.unobservedPorts, len(censusErr.unboundedPorts), censusErr.unboundedPorts, censusErr.invalidRows)
		unknown.mechanism = "The configured INFO-memory census has missing or ambiguous measurements, or a valid unlimited maxmemory ceiling that cannot be ranked by utilization. The fleet's fullest node is unknown; valid bounded sibling measurements remain usable for qualified findings."
	} else {
		unknown.observed += fmt.Sprintf(" unobserved_nodes=%d", len(failure.host.redisNodePorts()))
	}
	unknown.observed += " fleet_fullest=unknown"
	return unknown
}

// Qualify the sample without overstating an in-host gap as a wholly unread
// host. Existing host names remain useful alongside exact missing-node counts.
func redisUsageSelectionScope(failures []redisHostFailure) string {
	hostNames := make([]string, 0, len(failures))
	unobservedNodes := 0
	unboundedNodes := 0
	for _, failure := range failures {
		hostNames = append(hostNames, failure.host.name)
		var censusErr *redisNodeUsageCensusError
		if errors.As(failure.err, &censusErr) {
			unobservedNodes += len(censusErr.unobservedPorts)
			unboundedNodes += len(censusErr.unboundedPorts)
		} else {
			unobservedNodes += len(failure.host.redisNodePorts())
		}
	}
	return fmt.Sprintf("selection_scope=observed-bounded-nodes fleet_fullest=unknown unobserved_hosts=%s unobserved_nodes=%d unbounded_nodes=%d",
		strings.Join(hostNames, ","), unobservedNodes, unboundedNodes)
}
