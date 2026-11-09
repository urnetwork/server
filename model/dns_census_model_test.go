package model

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/sidecar"
)

// §12.5's census (DNS-M0-01) against a Go reference over the same rows.

// What the census's SQL counts for one name, in Go: Postgres's ~ and = are
// case-sensitive, and substr counts characters.
func testDnsCensusReference(names []string) *DnsNetworkNameCensus {
	preRule := regexp.MustCompile(`^[a-z0-9-]{5,50}$`)
	reservedLabels := sidecar.ReservedDnsLabels()
	reservedPattern := func(name string) bool {
		for _, pattern := range sidecar.ReservedDnsPatterns() {
			if prefix, ok := strings.CutSuffix(pattern, "*"); ok && strings.HasPrefix(name, prefix) {
				return true
			}
			if suffix, ok := strings.CutPrefix(pattern, "*"); ok && strings.HasSuffix(name, suffix) {
				return true
			}
		}
		return false
	}
	census := &DnsNetworkNameCensus{}
	for _, name := range names {
		if !preRule.MatchString(name) {
			census.PreRule += 1
		}
		if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
			census.EdgeHyphen += 1
		}
		if characters := []rune(name); 4 <= len(characters) && string(characters[2:4]) == "--" {
			census.Dash34 += 1
		}
		if slices.Contains(reservedLabels, name) || reservedPattern(name) {
			census.Reserved += 1
		}
	}
	return census
}

// Every network name, as the reference reads them.
func testDnsCensusNames(ctx context.Context) []string {
	names := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT network_name FROM network`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var name string
				server.Raise(result.Scan(&name))
				names = append(names, name)
			}
		})
	})
	return names
}

// Fixture names in each class, and in several at once, give the counts a Go
// reference gives over the same rows, on a session that can only read, as a
// replica's is.
func TestCensusDnsNetworkNamesMatchesAReference(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixtures := []string{
			// pre_rule
			"Census-Upper", "abc", "under_score_name", "café-name", strings.Repeat("a", 51),
			// edge_hyphen, the first also pre_rule's shape
			"-census-lead", "census-trail-", "-",
			// dash34
			"ce--nsus", "xn--census-idna",
			// reserved: exact, each pattern's side, and a case the SQL does not fold
			"postmaster", "census-lb", "main-census", "by-census-host", "MAIN-census",
			// reserved and edge_hyphen
			"main-",
			// none
			"census-ordinary", "lab--el-census",
		}
		for _, name := range fixtures {
			Testing_CreateNetwork(ctx, server.NewId(), name, server.NewId())
		}

		want := testDnsCensusReference(testDnsCensusNames(ctx))
		fixtureCensus := testDnsCensusReference(fixtures)
		if fixtureCensus.PreRule < 5 || fixtureCensus.EdgeHyphen < 4 || fixtureCensus.Dash34 < 2 || fixtureCensus.Reserved < 5 {
			t.Fatalf("the fixtures do not cover each class: %+v", fixtureCensus)
		}

		server.RequirePgReadOnly()
		defer server.Testing_ReleasePgReadOnly()
		got := CensusDnsNetworkNames(ctx)
		if *got != *want {
			t.Fatalf("census %+v, reference %+v", got, want)
		}
	})
}

// The patterns' regex: anchored, each * a run of anything, everything else
// literal, and the census's $2 for sidecar's list.
func TestDnsReservedPatternRegex(t *testing.T) {
	regex, err := DnsReservedPatternRegex([]string{"*-lb", "main-*", "a.b*"})
	if err != nil {
		t.Fatal(err)
	}
	if regex != `^(.*\-lb|main\-.*|a\.b.*)$` {
		t.Fatalf("regex %q", regex)
	}
	compiled := regexp.MustCompile(regex)
	for name, want := range map[string]bool{"edge-lb": true, "-lb": true, "main-": true, "mainx": false, "lb-edge": false, "a.bc": true, "axbc": false} {
		if compiled.MatchString(name) != want {
			t.Errorf("%q matched %t", name, !want)
		}
	}
	if _, err := DnsReservedPatternRegex(nil); err == nil {
		t.Fatal("built a regex of no patterns, which would match nothing or everything")
	}
	if _, err := DnsReservedPatternRegex(sidecar.ReservedDnsPatterns()); err != nil {
		t.Fatal(err)
	}
}
