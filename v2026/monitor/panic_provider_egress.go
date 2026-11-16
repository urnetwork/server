package monitor

// Retains only the three source-defined provider-egress refusals. The frame
// validation follows the independently qualified bounded panic reader; a
// later caller cannot lend ownership to an unknown inner frame.

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var panicProviderFunctionRe = regexp.MustCompile(`^github\.com/urnetwork/(server(?:/v2026)?(?:/[A-Za-z_][A-Za-z0-9_-]*)*)\.((?:\(\*?[A-Za-z_][A-Za-z0-9_]*(?:\[\.\.\.\])?\)|[A-Za-z_][A-Za-z0-9_]*(?:\[\.\.\.\])?)(?:\.[A-Za-z_][A-Za-z0-9_]*(?:\[\.\.\.\])?|\.[0-9]+)*)(?:-fm)?\(`)
var panicProviderSourceRe = regexp.MustCompile(`(?:^|/)([A-Za-z_][A-Za-z0-9_]*\.go):([0-9]{1,7})(?: \+0x[0-9a-f]+)?$`)

// The reader's exact server-wrapper/file pairs, plus the one owning caller.
// Arbitrary application symbols remain outside this diagnostic authority.
var panicProviderSourceFiles = map[string]string{
	"server.Db":                                    "db.go",
	"server.DbInConn":                              "db_connection_owner.go",
	"server.HandleError":                           "trace.go",
	"server.HandleError1":                          "trace.go",
	"server.HandleError2":                          "trace.go",
	"server.HandleErrorWithReturn":                 "trace.go",
	"server.MaintenanceDb":                         "db.go",
	"server.MaintenanceTx":                         "db.go",
	"server.OwnedTx":                               "db_owned_transaction.go",
	"server.PgOwnedSession.Tx":                     "db_owned_session.go",
	"server.PgOwnedSession.txWithResource":         "db_owned_session.go",
	"server.Raise":                                 "util.go",
	"server.RaisePgResult":                         "db.go",
	"server.Redis":                                 "redis.go",
	"server.RedisDoOnce":                           "redis.go",
	"server.ReplicaDb":                             "db.go",
	"server.TryOwnedTx":                            "db_owned_transaction.go",
	"server.Tx":                                    "db.go",
	"server.TxInConn":                              "db_connection_owner.go",
	"server.WithPgResult":                          "db.go",
	"server.dbWithPool":                            "db.go",
	"server.ownedTxWithResource":                   "db_owned_transaction.go",
	"server.ownedTxWithResourcePolicy":             "db_owned_transaction.go",
	"server.pgOwnedConnection.run":                 "db_owned_transaction.go",
	"server.redisWithClient":                       "redis.go",
	"server.txWithPool":                            "db.go",
	"server.txWithConnection":                      "db.go",
	"server/controller.statsRefreshProviderEgress": "stats_collector.go",
}

// Checks the complete symbol/file pair before discarding version and closure
// syntax. The source line is bounded but is not evidence of an emitting image.
func panicProviderSourceFrame(function, source string) string {
	match := panicProviderFunctionRe.FindStringSubmatch(strings.TrimSpace(function))
	location := panicProviderSourceRe.FindStringSubmatch(strings.TrimSpace(source))
	if len(match) != 3 || len(location) != 3 {
		return ""
	}
	module, name := match[1], strings.ReplaceAll(match[2], "[...]", "")
	if closure := panicLogClosureRe.FindStringIndex(name); closure != nil {
		name = name[:closure[0]]
	}
	name = strings.NewReplacer("(*", "", "(", "", ")", "").Replace(name)
	if module == "server/v2026" || strings.HasPrefix(module, "server/v2026/") {
		module = strings.Replace(module, "server/v2026", "server", 1)
	}
	symbol := module + "." + name
	file, known := panicProviderSourceFiles[symbol]
	line, err := strconv.Atoi(location[2])
	if !known || file != location[1] || err != nil || line < 1 || line > 1000000 {
		return ""
	}
	return symbol
}

// Called only after the bounded, duplicate-rejecting ErrorJson decoder.
// Busy leaves freshness and holder progress unknown, including failed-fill
// fences; none of these classes changes panic counting or severity.
func panicProviderEgressErrorClass(line, errorText string, stack []string) string {
	if !utf8.ValidString(line) {
		return "unknown"
	}
	for index := 0; index < len(stack); index++ {
		function := strings.TrimSpace(stack[index])
		if !strings.HasPrefix(function, "github.com/urnetwork/") {
			continue
		}
		if index+1 >= len(stack) {
			return "unknown"
		}
		symbol := panicProviderSourceFrame(function, stack[index+1])
		if symbol == "" {
			return "unknown"
		}
		if strings.HasPrefix(symbol, "server.") {
			index++
			continue
		}
		if symbol != "server/controller.statsRefreshProviderEgress" {
			return "unknown"
		}
		switch errorText {
		case "*errors.errorString=provider egress stats refresh in progress":
			return "provider_egress_refresh_busy"
		case "*errors.errorString=provider egress stats snapshot invalid":
			return "provider_egress_snapshot_invalid"
		case "*errors.errorString=provider egress stats unavailable":
			return "provider_egress_unavailable"
		default:
			return "unknown"
		}
	}
	return "unknown"
}
