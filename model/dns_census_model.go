package model

// §12.5's census (DNS-M0-01): before labels and aliases exist, how many of
// today's network names the label rules would refuse. One read-only SELECT,
// the design's own, run once by a person against a read replica: it returns
// four counts and never a name, since network names are user data.
//
// $1 is the reserved list's exact labels and $2 one anchored POSIX regex
// built from its patterns, both from sidecar's embedded list, so the census
// counts what the build that runs it would refuse.

import (
	"context"
	"errors"
	"strings"

	"github.com/urnetwork/server"
	"github.com/urnetwork/sidecar"
)

// §12.5's census query, as the design writes it.
const dnsNetworkNameCensusSql = `
SELECT count(*) FILTER (WHERE network_name !~ '^[a-z0-9-]{5,50}$') AS pre_rule,
       count(*) FILTER (WHERE network_name ~ '(^-|-$)') AS edge_hyphen,
       count(*) FILTER (WHERE substr(network_name, 3, 2) = '--') AS dash34,
       count(*) FILTER (WHERE network_name = ANY($1) OR network_name ~ $2) AS reserved
FROM network
`

// The census's four counts. A name may be in several, and one in none is
// a name the label rules allow, unless a brand rule refuses it, which no SQL
// regex holds.
type DnsNetworkNameCensus struct {
	// names ValidateNetworkName would not produce: not 5 to 50 of [a-z0-9-]
	PreRule int64
	// a dash first or last, which Boulder refuses
	EdgeHyphen int64
	// dashes as the third and fourth characters, xn-- among them
	Dash34 int64
	// a reserved label, exactly or by a pattern
	Reserved int64
}

// The one anchored POSIX regex of the reserved patterns: each * is .*, and
// every other character stands for itself, escaped unless a letter or digit.
func DnsReservedPatternRegex(patterns []string) (string, error) {
	if len(patterns) == 0 {
		return "", errors.New("no reserved patterns")
	}
	alternatives := []string{}
	for _, pattern := range patterns {
		var alternative strings.Builder
		for _, character := range pattern {
			switch {
			case character == '*':
				alternative.WriteString(".*")
			case 'a' <= character && character <= 'z', '0' <= character && character <= '9':
				alternative.WriteRune(character)
			default:
				alternative.WriteRune('\\')
				alternative.WriteRune(character)
			}
		}
		alternatives = append(alternatives, alternative.String())
	}
	return "^(" + strings.Join(alternatives, "|") + ")$", nil
}

// Runs the census in one read-only statement on the replica pool. A failed
// statement raises.
func CensusDnsNetworkNames(ctx context.Context) *DnsNetworkNameCensus {
	reservedPatternRegex, err := DnsReservedPatternRegex(sidecar.ReservedDnsPatterns())
	server.Raise(err)
	census := &DnsNetworkNameCensus{}
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, dnsNetworkNameCensusSql, sidecar.ReservedDnsLabels(), reservedPatternRegex)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&census.PreRule, &census.EdgeHyphen, &census.Dash34, &census.Reserved))
			}
		})
	}, server.OptReadOnly())
	return census
}
