// Source contracts keep manual provider4 research outside the automatic
// functional entry point without constructing a service-backed fixture.
package perfvar

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/v2026"
)

// The default skip precedes TestEnv and any workload. The opt-in tail and
// ungated functional root retain their exact shared-driver mode arguments.
func TestSdkBrowsingEntrypointsSeparateResearchAndQualification(t *testing.T) {
	source, err := os.ReadFile("sdk_browsing_admission_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "sdk_browsing_admission_test.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	functions := map[string]*ast.FuncDecl{}
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil {
			functions[function.Name.Name] = function
		}
	}
	text := func(node ast.Node) string {
		var wire bytes.Buffer
		if err := format.Node(&wire, fileSet, node); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(wire.String())
	}
	functional := functions["TestFullTunH1Sdk32ServiceRefusalRecovery100Pages"]
	if functional == nil || functional.Body == nil || len(functional.Body.List) != 1 ||
		text(functional.Body.List[0]) != "testSdkBrowsingPublicServiceRecovery(t, true)" {
		t.Fatal("mandatory functional root gained a selector, skip or different workload")
	}
	research := functions["TestFullTunH1Sdk32LateQualityPublicFullWindow100Pages"]
	if research == nil || research.Body == nil || len(research.Body.List) != 2 {
		t.Fatal("manual research root lost its explicit pre-workload gate")
	}
	gate, ok := research.Body.List[0].(*ast.IfStmt)
	if !ok || gate.Init != nil || gate.Else != nil ||
		text(gate.Cond) != `os.Getenv("CONNECT_PERFVAR_LATE_QUALITY_RESEARCH") != "1"` ||
		len(gate.Body.List) != 1 || !strings.HasPrefix(text(gate.Body.List[0]), "t.Skip(") ||
		!strings.Contains(text(gate.Body.List[0]), "not qualification") {
		t.Fatal("research must require exact opt-in and declare its default skip outside TestEnv")
	}
	if text(research.Body.List[1]) != "testSdkBrowsingPublicServiceRecovery(t, false)" {
		t.Fatal("opted-in research no longer uses its original strict verdict")
	}
	shared := functions["testSdkBrowsingPublicServiceRecovery"]
	if shared == nil || shared.Body == nil || len(shared.Body.List) < 2 {
		t.Fatal("shared functional workload is absent")
	}
	shortGate, ok := shared.Body.List[0].(*ast.IfStmt)
	if !ok || shortGate.Init != nil || shortGate.Else != nil || text(shortGate.Cond) != "testing.Short()" ||
		len(shortGate.Body.List) != 1 || !strings.HasPrefix(text(shortGate.Body.List[0]), "t.Skip(") {
		t.Fatal("functional workload lost its existing short-only skip")
	}
	skipCount := 0
	ast.Inspect(shared.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		owner, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		if owner.Name == "os" && selector.Sel.Name == "Getenv" {
			t.Fatal("functional workload gained an environment selector")
		}
		if owner.Name == "t" && (selector.Sel.Name == "Skip" || selector.Sel.Name == "Skipf" || selector.Sel.Name == "SkipNow") {
			skipCount++
		}
		return true
	})
	if skipCount != 1 {
		t.Fatal("functional workload gained a skip after workload or verdict evaluation")
	}
}

// A mechanism miss cannot hide concurrent latency or body failures. The
// functional mode must retain those same failures when it permits speed5.
func TestSdkBrowsingResearchAndFunctionalMixedFailuresRemainErrors(t *testing.T) {
	for _, acceptOwnedSpeed := range []bool{false, true} {
		scenario, observation := sdkBrowsingServiceRecoveryControl()
		scenario.acceptOwnedSpeedRecovery = acceptOwnedSpeed
		scenario.combinedFirstByteNs = int64(sdk.DefaultDeviceLocalSettings().SendTimeout)
		observation.Pressure.assets[20][0].bodyClosed.Store(false)
		err := scenario.verdict(observation)
		if err == nil || !strings.Contains(err.Error(), "NATIVE_BROWSING_NAVIGATION_STALL") ||
			!strings.Contains(err.Error(), "asset response body ownership") ||
			strings.Contains(err.Error(), "SDK_BROWSING_LATE_PROVIDER_NOT_EXERCISED") == acceptOwnedSpeed {
			t.Fatalf("accept-owned-speed=%t discarded a mixed failure: %v", acceptOwnedSpeed, err)
		}
	}
}
