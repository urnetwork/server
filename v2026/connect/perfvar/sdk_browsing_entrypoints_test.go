// Source contracts keep manual provider4 research outside the automatic
// functional entry point without constructing a service-backed fixture.
package perfvar

import (
	"bytes"
	"errors"
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
	if err := sdkBrowsingEntrypointContract(source); err != nil {
		t.Fatal(err)
	}
}

// Each edit of the actual admission source breaks one protected property and
// must be refused. Deleting the opt-in observer alone remains acceptable.
func TestSdkBrowsingEntrypointContractRefusesBrokenSharingAndSkips(t *testing.T) {
	source, err := os.ReadFile("sdk_browsing_admission_test.go")
	if err != nil {
		t.Fatal(err)
	}
	edits := []struct {
		name, old, new, want string
	}{
		{
			name: "workload lost its short-only skip",
			old:  "\tif testing.Short() {\n\t\tt.Skip(\"actual SDK/H1/provider topology needs shared PERFVAR services\")\n\t}\n",
			want: "functional workload lost its existing short-only skip",
		},
		{
			name: "shared driver broadens the initial cohort",
			old:  "testSdkBrowsingPublicServiceRecoveryWithInitialOutage(t, acceptOwnedSpeed, false)",
			new:  "testSdkBrowsingPublicServiceRecoveryWithInitialOutage(t, acceptOwnedSpeed, true)",
			want: "shared functional driver no longer delegates both roots to the original-cohort workload",
		},
		{
			name: "shared driver selects before delegating",
			old:  "func testSdkBrowsingPublicServiceRecovery(t *testing.T, acceptOwnedSpeed bool) {\n",
			new:  "func testSdkBrowsingPublicServiceRecovery(t *testing.T, acceptOwnedSpeed bool) {\n\tacceptOwnedSpeed = acceptOwnedSpeed && os.Getenv(\"CONNECT_PERFVAR_STRICT\") != \"1\"\n",
			want: "shared functional driver does more than delegate",
		},
		{
			name: "functional root bypasses the shared driver",
			old:  "testSdkBrowsingPublicServiceRecovery(t, true)",
			new:  "testSdkBrowsingPublicServiceRecoveryWithInitialOutage(t, true, false)",
			want: "mandatory functional root gained a selector, skip or different workload",
		},
		{
			name: "research root bypasses the shared driver",
			old:  "testSdkBrowsingPublicServiceRecovery(t, false)",
			new:  "testSdkBrowsingPublicServiceRecoveryWithInitialOutage(t, false, false)",
			want: "opted-in research no longer uses its original strict verdict",
		},
		{
			name: "workload gained an environment selector",
			old:  "scenario.acceptOwnedSpeedRecovery = acceptOwnedSpeed\n",
			new:  "scenario.acceptOwnedSpeedRecovery = acceptOwnedSpeed && os.Getenv(\"CONNECT_PERFVAR_STRICT\") != \"1\"\n",
			want: "functional workload gained an environment selector",
		},
		{
			name: "observer opt-in also changes the workload",
			old:  "scenario.page1Timeline = &sdkBrowsingPage1Timeline{}\n",
			new:  "scenario.page1Timeline = &sdkBrowsingPage1Timeline{}\n\t\t\tplan.maxLivePages = 1\n",
			want: "functional workload gained an environment selector",
		},
		{
			name: "workload gained a post-verdict skip",
			old:  "scenario.log(t, observation)\n",
			new:  "scenario.log(t, observation)\n\t\tif verdictErr != nil {\n\t\t\tt.Skip(\"post-verdict\")\n\t\t}\n",
			want: "functional workload gained a skip after workload or verdict evaluation",
		},
		{
			name: "verdict consults the opt-in observer",
			old:  "func (self *sdkBrowsingScenario) verdict(observation *nativeBrowsingObservation) error {\n",
			new:  "func (self *sdkBrowsingScenario) verdict(observation *nativeBrowsingObservation) error {\n\tif self.page1Timeline != nil {\n\t\treturn nil\n\t}\n",
			want: "sdk browsing verdict consults the opt-in page1 observer",
		},
		{
			name: "observer opt-in removed",
			old:  "\t\tif os.Getenv(sdkBrowsingPage1TimelineFlag) == \"1\" {\n\t\t\tscenario.page1Timeline = &sdkBrowsingPage1Timeline{}\n\t\t}\n",
		},
	}
	for _, edit := range edits {
		if strings.Count(string(source), edit.old) != 1 {
			t.Fatalf("%s: the edit no longer matches the admission source exactly once", edit.name)
		}
		err := sdkBrowsingEntrypointContract([]byte(strings.Replace(string(source), edit.old, edit.new, 1)))
		if edit.want == "" {
			if err != nil {
				t.Errorf("%s: contract refused an acceptable edit: %v", edit.name, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), edit.want) {
			t.Errorf("%s: contract result %v, want %q", edit.name, err, edit.want)
		}
	}
}

// Both roots share one driver, which only delegates, with its mode argument
// unchanged, to the workload with the original quality-only fault; the
// contract follows that delegate. The workload's first statement is the
// short-only skip and it has no other skip. Its only environment read
// attaches the opt-in page1 observer, which is nil-safe when absent
// (TestSdkBrowsingPage1TimelineBoundsAndStops) and which the verdict never
// consults; any other read is a selector.
func sdkBrowsingEntrypointContract(source []byte) error {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "sdk_browsing_admission_test.go", source, 0)
	if err != nil {
		return err
	}
	var formatErr error
	text := func(node ast.Node) string {
		var wire bytes.Buffer
		if err := format.Node(&wire, fileSet, node); err != nil {
			formatErr = err
		}
		return strings.TrimSpace(wire.String())
	}
	refuse := func(reason string) error {
		return errors.Join(errors.New(reason), formatErr)
	}
	functions := map[string]*ast.FuncDecl{}
	var verdict *ast.FuncDecl
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if function.Recv == nil {
			functions[function.Name.Name] = function
		} else if function.Name.Name == "verdict" && len(function.Recv.List) == 1 &&
			text(function.Recv.List[0].Type) == "*sdkBrowsingScenario" {
			verdict = function
		}
	}
	functional := functions["TestFullTunH1Sdk32ServiceRefusalRecovery100Pages"]
	if functional == nil || functional.Body == nil || len(functional.Body.List) != 1 ||
		text(functional.Body.List[0]) != "testSdkBrowsingPublicServiceRecovery(t, true)" {
		return refuse("mandatory functional root gained a selector, skip or different workload")
	}
	research := functions["TestFullTunH1Sdk32LateQualityPublicFullWindow100Pages"]
	if research == nil || research.Body == nil || len(research.Body.List) != 2 {
		return refuse("manual research root lost its explicit pre-workload gate")
	}
	gate, ok := research.Body.List[0].(*ast.IfStmt)
	if !ok || gate.Init != nil || gate.Else != nil ||
		text(gate.Cond) != `os.Getenv("CONNECT_PERFVAR_LATE_QUALITY_RESEARCH") != "1"` ||
		len(gate.Body.List) != 1 || !strings.HasPrefix(text(gate.Body.List[0]), "t.Skip(") ||
		!strings.Contains(text(gate.Body.List[0]), "not qualification") {
		return refuse("research must require exact opt-in and declare its default skip outside TestEnv")
	}
	if text(research.Body.List[1]) != "testSdkBrowsingPublicServiceRecovery(t, false)" {
		return refuse("opted-in research no longer uses its original strict verdict")
	}
	shared := functions["testSdkBrowsingPublicServiceRecovery"]
	if shared == nil || shared.Body == nil {
		return refuse("shared functional driver is absent")
	}
	if len(shared.Body.List) != 1 {
		return refuse("shared functional driver does more than delegate")
	}
	var delegate *ast.Ident
	if statement, ok := shared.Body.List[0].(*ast.ExprStmt); ok {
		if call, ok := statement.X.(*ast.CallExpr); ok {
			delegate, _ = call.Fun.(*ast.Ident)
		}
	}
	if delegate == nil ||
		text(shared.Body.List[0]) != "testSdkBrowsingPublicServiceRecoveryWithInitialOutage(t, acceptOwnedSpeed, false)" {
		return refuse("shared functional driver no longer delegates both roots to the original-cohort workload")
	}
	workload := functions[delegate.Name]
	if workload == nil || workload.Body == nil || len(workload.Body.List) < 2 {
		return refuse("shared functional workload is absent")
	}
	shortGate, ok := workload.Body.List[0].(*ast.IfStmt)
	if !ok || shortGate.Init != nil || shortGate.Else != nil || text(shortGate.Cond) != "testing.Short()" ||
		len(shortGate.Body.List) != 1 || !strings.HasPrefix(text(shortGate.Body.List[0]), "t.Skip(") {
		return refuse("functional workload lost its existing short-only skip")
	}
	var observerRead *ast.CallExpr
	selectorFound := false
	skipCount := 0
	ast.Inspect(workload.Body, func(node ast.Node) bool {
		// The one sanctioned read is visited with its statement, before the call.
		if statement, ok := node.(*ast.IfStmt); ok && observerRead == nil &&
			statement.Init == nil && statement.Else == nil &&
			text(statement.Cond) == `os.Getenv(sdkBrowsingPage1TimelineFlag) == "1"` &&
			len(statement.Body.List) == 1 &&
			text(statement.Body.List[0]) == "scenario.page1Timeline = &sdkBrowsingPage1Timeline{}" {
			if condition, ok := statement.Cond.(*ast.BinaryExpr); ok {
				observerRead, _ = condition.X.(*ast.CallExpr)
			}
		}
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
		if owner.Name == "os" && selector.Sel.Name == "Getenv" && call != observerRead {
			selectorFound = true
		}
		if owner.Name == "t" && (selector.Sel.Name == "Skip" || selector.Sel.Name == "Skipf" || selector.Sel.Name == "SkipNow") {
			skipCount++
		}
		return true
	})
	if selectorFound {
		return refuse("functional workload gained an environment selector")
	}
	if skipCount != 1 {
		return refuse("functional workload gained a skip after workload or verdict evaluation")
	}
	if verdict == nil || verdict.Body == nil {
		return refuse("sdk browsing verdict is absent")
	}
	consultsObserver := false
	ast.Inspect(verdict.Body, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "page1Timeline" {
			consultsObserver = true
		}
		return true
	})
	if consultsObserver {
		return refuse("sdk browsing verdict consults the opt-in page1 observer")
	}
	return formatErr
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
