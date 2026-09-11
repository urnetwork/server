package monitor

import (
	"strings"
	"testing"
)

const syntheticTxRollbackMaskLine = `[api-node.fixture.example][api][g2][cid:private-fixture][I][2000-01-01T00:00:00Z][router.go:104][h]unhandled error from route POST ^/network/remove-client$: {"error":"*errors.errorString=tx is closed","stack":["goroutine 42 [running]:","github.com/urnetwork/server/v2026.txWithPool.func1.1()","github.com/urnetwork/server/v2026/db.go:676 +0x7d","panic({0x1?, 0x2?})","github.com/urnetwork/server/v2026.txWithPool.func1(0x3?)","github.com/urnetwork/server/v2026/db.go:718 +0x24a"]}`

func TestTxRollbackMaskRequiresExactDualFrameAndUsesFixedEvidence(t *testing.T) {
	tailer := newLogTailer("api", nil)
	for range 5 {
		tailer.classify(syntheticTxRollbackMaskLine)
	}
	findings := tailer.drainWindow()
	finding := findingByClass(t, findings, "tx-rollback-mask")
	if finding.healthy || finding.tier != tierPage {
		t.Fatalf("transaction rollback mask finding = %+v", finding)
	}
	if finding.frame != "txWithPool-dual-rollback" {
		t.Fatalf("transaction rollback mask frame = %q", finding.frame)
	}
	if panicFinding := findingByClass(t, findings, "panic"); !panicFinding.healthy {
		t.Fatalf("exact rollback mask also became generic panic: %+v", panicFinding)
	}
	if novelFinding := findingByClass(t, findings, "novel"); !novelFinding.healthy {
		t.Fatalf("exact rollback mask also became novel: %+v", novelFinding)
	}

	const fixedSample = "txWithPool rollback cleanup replaced an existing transaction error (details omitted)"
	if !strings.Contains(finding.evidence, fixedSample) {
		t.Fatalf("transaction rollback mask evidence lacks fixed sample: %s", finding.evidence)
	}
	for _, private := range []string{
		"api-node.fixture.example",
		"private-fixture",
		"/network/remove-client",
		"goroutine 42",
		"0x3",
	} {
		if strings.Contains(finding.evidence, private) {
			t.Fatalf("transaction rollback mask evidence retained %q: %s", private, finding.evidence)
		}
	}
}

func TestTxRollbackMaskNearMissesRemainGenericPanic(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{
			name: "ordinary tx closed",
			line: `[api-node.fixture.example][api][g2][cid:private-fixture]Unexpected error: ` +
				`{"error":"*errors.errorString=tx is closed","stack":["goroutine 42 [running]:"]}`,
		},
		{
			name: "outer frame absent",
			line: strings.Replace(
				syntheticTxRollbackMaskLine,
				".txWithPool.func1.1()",
				".txWithPool.cleanup()",
				1,
			),
		},
		{
			name: "inner frame absent",
			line: strings.Replace(
				syntheticTxRollbackMaskLine,
				".txWithPool.func1(0x3?)",
				".txWithPool.callback(0x3?)",
				1,
			),
		},
		{
			name: "outer rollback site differs",
			line: strings.Replace(syntheticTxRollbackMaskLine, "/db.go:676", "/db.go:677", 1),
		},
		{
			name: "transient rollback site differs",
			line: strings.Replace(syntheticTxRollbackMaskLine, "/db.go:718", "/db.go:719", 1),
		},
		{
			name: "final error differs",
			line: strings.Replace(
				syntheticTxRollbackMaskLine,
				"*errors.errorString=tx is closed",
				"*errors.errorString=context canceled",
				1,
			),
		},
	}

	for _, test := range tests {
		tailer := newLogTailer("api", nil)
		for range 5 {
			tailer.classify(test.line)
		}
		findings := tailer.drainWindow()
		if finding := findingByClass(t, findings, "tx-rollback-mask"); !finding.healthy {
			t.Fatalf("%s: near miss became exact rollback mask: %+v", test.name, finding)
		}
		if finding := findingByClass(t, findings, "panic"); finding.healthy {
			t.Fatalf("%s: near miss disappeared instead of remaining generic panic", test.name)
		}
	}
}
