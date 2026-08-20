package server

import (
	"net/netip"
	"os"
	"slices"

	"testing"

	"github.com/urnetwork/connect/v2026"
)

func TestLocalEvaluationCredentialRequiresExplicitLocalMode(t *testing.T) {
	t.Setenv("APEX_CONTAINER_EVALUATION", "")
	t.Setenv("EVALUATION_DB_PASSWORD", "override")
	if got := localEvaluationCredential("EVALUATION_DB_PASSWORD", "configured"); got != "configured" {
		t.Fatalf("ordinary environment used evaluator credential %q", got)
	}

	t.Setenv("APEX_CONTAINER_EVALUATION", "true")
	t.Setenv("WARP_ENV", "local")
	if got := localEvaluationCredential("EVALUATION_DB_PASSWORD", "configured"); got != "override" {
		t.Fatalf("evaluator credential = %q, want override", got)
	}

	t.Setenv("WARP_ENV", "main")
	assertPanics(t, func() {
		localEvaluationCredential("EVALUATION_DB_PASSWORD", "configured")
	})
	t.Setenv("WARP_ENV", "local")
	t.Setenv("EVALUATION_DB_PASSWORD", "")
	assertPanics(t, func() {
		localEvaluationCredential("EVALUATION_DB_PASSWORD", "configured")
	})
}

func assertPanics(t *testing.T, run func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	run()
}

func TestLimitExcludePrefixes(t *testing.T) {

	v := os.Getenv("WARP_LIMIT_EXCLUDE_SUBNETS")
	defer os.Setenv("WARP_LIMIT_EXCLUDE_SUBNETS", v)
	os.Setenv("WARP_LIMIT_EXCLUDE_SUBNETS", "10.0.0.0/8;172.16.0.0/12;192.168.0.0/16")

	prefixes := limitExcludePrefixes()
	connect.AssertEqual(t, len(prefixes), 3)
	connect.AssertEqual(t, slices.Contains(prefixes, netip.MustParsePrefix("10.0.0.0/8")), true)
	connect.AssertEqual(t, slices.Contains(prefixes, netip.MustParsePrefix("172.16.0.0/12")), true)
	connect.AssertEqual(t, slices.Contains(prefixes, netip.MustParsePrefix("192.168.0.0/16")), true)

	connect.AssertEqual(t, IsLimitExcludeAddr(netip.MustParseAddr("1.1.1.1")), false)
	connect.AssertEqual(t, IsLimitExcludeAddr(netip.MustParseAddr("192.168.1.1")), true)
	connect.AssertEqual(t, IsLimitExcludeAddr(netip.MustParseAddr("10.1.1.1")), true)
	connect.AssertEqual(t, IsLimitExcludeAddr(netip.MustParseAddr("172.16.1.1")), true)

	os.Setenv("WARP_LIMIT_EXCLUDE_SUBNETS", "")
	prefixes = limitExcludePrefixes()
}
