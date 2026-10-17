// Shared PostgreSQL client-owner reduction keeps socket addresses out of
// monitor logs and durable alerts while preserving inventory attribution.
package monitor

import "strings"

const postgresLoopbackClientSQL = `(client_addr <<= inet '127.0.0.0/8' OR client_addr = inet '::1')`

func privacySafePostgresClientOwner(cfg *monitorConfig, value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "local") {
		return "local-postgres-client"
	}
	address, ok := parseMonitorAddress(value)
	if !ok {
		return "unmapped-service-client"
	}
	if address.IsLoopback() {
		return "loopback-service-client"
	}
	if cfg == nil {
		return "unmapped-service-client"
	}
	owner := boundedPgCapacityLabel(reliabilityTaskSourceHost(cfg, value), 80)
	// Inventory names are expected to be aliases. Keep a malformed address-like
	// name from turning the inventory lookup itself into an address disclosure.
	if _, addressLike := parseMonitorAddress(owner); addressLike ||
		strings.Contains(strings.ToLower(owner), strings.ToLower(value)) ||
		strings.Contains(strings.ToLower(owner), strings.ToLower(address.String())) {
		return "mapped-service-client"
	}
	return owner
}
