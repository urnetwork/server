package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func TestEgressCoverageAPIDueCapFindings(t *testing.T) {
	desired := syntheticDesiredEgressConfig(t, strings.Replace(syntheticEgressDesiredConfig, "limit: 8", "limit: 1000", 1))
	findings := egressCoverageAPIDueCapFindings("synthetic-api", desired, egressCoverageAPIDueCap{value: 500, state: "fallback"})
	if len(findings) != 2 || findings[1].class != "egress-probe-api-due-cap" || findings[1].tier != tierPage || findings[1].healthy {
		t.Fatalf("missing API cap should page for clipped workers: %+v", findings)
	}
	if !strings.Contains(findings[1].observed, "required_minimum=5000") {
		t.Fatalf("bounded successor lookahead omitted: %s", findings[1].observed)
	}
	findings = egressCoverageAPIDueCapFindings("synthetic-api", desired, egressCoverageAPIDueCap{value: 5000, state: "configured"})
	if len(findings) != 2 || findings[1].class != "egress-probe-api-due-cap" || !findings[1].healthy {
		t.Fatalf("sufficient cap should clear page: %+v", findings)
	}
	findings = egressCoverageAPIDueCapFindings("synthetic-api", desired, egressCoverageAPIDueCap{state: "unobservable"})
	if len(findings) != 1 || findings[0].class != "egress-probe-api-due-cap-unobservable" || findings[0].tier != tierPage || findings[0].healthy {
		t.Fatalf("unobservable cap should page separately: %+v", findings)
	}
	desired.enabled = false
	if findings := egressCoverageAPIDueCapFindings("synthetic-api", desired, egressCoverageAPIDueCap{value: 500, state: "fallback"}); len(findings) != 0 {
		t.Fatalf("disabled probes should not require API capacity: %+v", findings)
	}
}

func TestEgressCoverageAPIDueCapResource(t *testing.T) {
	pop := server.Config.PushSimpleResource("provider_egress_due.yml", []byte("max_due_limit: 5000\n"))
	if got := loadEgressCoverageAPIDueCap(); got.value != 5000 || got.state != "configured" {
		t.Fatalf("configured cap: %+v", got)
	}
	pop()
	pop = server.Config.PushSimpleResource("provider_egress_due.yml", []byte("max_due_limit: 0\n"))
	if got := loadEgressCoverageAPIDueCap(); got.value != egressCoverageAPIDueFallback || got.state != "invalid-fallback" {
		t.Fatalf("invalid cap should use API fallback: %+v", got)
	}
	pop()
	pop = server.Config.PushSimpleResource("provider_egress_due.yml", []byte("max_due_limit: [\n"))
	defer pop()
	if got := loadEgressCoverageAPIDueCap(); got.state != "unobservable" {
		t.Fatalf("malformed cap should be unknown: %+v", got)
	}
}
