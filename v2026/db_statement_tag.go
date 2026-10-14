package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"sync"
)

// One tag per process, shared by the targeted fixed SQL statements. Only
// deployment identity enters the digest; request, account and database
// credentials never enter the statement tag.
var processPgStatementTag = sync.OnceValue(func() string {
	host := os.Getenv("WARP_HOST")
	if host == "" {
		host, _ = os.Hostname()
	}
	var nonce [12]byte
	_, _ = rand.Read(nonce[:])
	return pgStatementTag(os.Getenv("WARP_SERVICE"), host,
		os.Getenv("WARP_BLOCK"), os.Getenv("WARP_VERSION"), nonce)
})

func pgStatementTag(service, host, block, version string, nonce [12]byte) string {
	// Never reflect an arbitrary environment value into diagnostic output.
	switch service {
	case "api", "connect", "taskworker", "proxy", "alt", "config-updater", "mcp", "web", "app", "gossip", "grafana":
	default:
		service = "other"
	}
	identity := sha256.Sum256([]byte(strings.Join([]string{host, block, version}, "\x00")))
	// The longest allowed service produces 61 ASCII bytes. SQL comment parsing
	// accepts only this closed alphabet and fixed field widths.
	return "urn1/" + service + "/" + hex.EncodeToString(identity[:8]) + "/" + hex.EncodeToString(nonce[:])
}

// TaggedDatabaseStatement adds bounded runtime identity to a fixed statement.
// Construct it once with a package variable, never with request-dependent SQL.
// A stable comment preserves pgx prepared-statement reuse within the process
// without changing PgBouncer's per-client session parameters.
func TaggedDatabaseStatement(query string) string {
	return "/*" + processPgStatementTag() + "*/ " + query
}
