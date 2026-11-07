package nativefeefixture

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The lines of the gate's native-fee recipe that the build mirrors: its export
// rows and digest row, the private output directories, and, in order, the
// image built in sn where it stays, made read-only, and its exporter run from
// ./mainnet with the variable, test and deadline that the build passes.
func gateRecipe() []string {
	verifier := verifierDirectoryName + "/" + verifierName
	return []string{
		fixtureDirectoryVariable + " " + fixtureDirectoryName + " sn",
		verifierVariable + " " + verifier + " sn'",
		"digests='" + verifierSha256Variable + " " + verifier + " sn'",
		`mkdir -m 700 -- "$output/` + fixtureDirectoryName + `" "$output/` + verifierDirectoryName + `"`,
		`verifier="$output/` + verifier + `"`,
		strings.Join([]string{
			`(`,
			`  cd "$sn_repo"`,
			`  go test -c -o "$verifier" ./mainnet`,
			`  chmod 0500 -- "$verifier"`,
			`  cd "$sn_repo/mainnet"`,
			"  " + exportDirectoryVariable + `="$output/` + fixtureDirectoryName + `" "$verifier" -test.run '` + exportTestPattern + `' -test.timeout ` + exportTestTimeout,
			`)`,
		}, "\n"),
	}
}

// The recipe lines the gate's script does not have exactly once.
func recipeDrift(gate string) []string {
	var drift []string
	for _, recipe := range gateRecipe() {
		if strings.Count(gate, "\n"+recipe+"\n") != 1 {
			drift = append(drift, recipe)
		}
	}
	return drift
}

// The build is the gate's own recipe, read from the sn checkout this server
// builds against. A change to that recipe fails here until the build follows
// it; a renamed exporter test, another deadline, a writable image or a run
// from another directory each count as one.
func TestProvideFollowsTheGatesNativeFeeRecipe(t *testing.T) {
	goTool, err := defaultGoTool()
	if err != nil {
		t.Fatal(err)
	}
	sn, err := resolveSn(context.Background(), settings{goTool: goTool, environ: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sn, "scripts", "build-mainnet-exports.sh")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gate := string(raw)
	if drift := recipeDrift(gate); len(drift) != 0 {
		t.Fatalf("%s no longer has exactly one of each of\n%s\nThe gate's native-fee recipe changed, and internal/nativefeefixture must follow it.", path, strings.Join(drift, "\n---\n"))
	}
	for _, change := range []struct{ old, new string }{
		{"'^TestNativeFeeOutcomeExportSettlementFixtures$'", "'^TestNativeFeeOutcomeExportSettlementFixturesRenamed$'"},
		{"-test.timeout 10m\n", "-test.timeout 20m\n"},
		{`chmod 0500 -- "$verifier"`, `chmod 0700 -- "$verifier"`},
		{"\n  cd \"$sn_repo/mainnet\"\n", "\n"},
		{"URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER native-fee-verifier/mainnet.test sn'", "URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER native-fee-image/mainnet.test sn'"},
	} {
		if strings.Count(gate, change.old) != 1 {
			t.Fatalf("the mutation names no unique text of the gate: %q", change.old)
		}
		if len(recipeDrift(strings.Replace(gate, change.old, change.new, 1))) == 0 {
			t.Fatalf("the recipe check accepted %q in place of %q", change.new, change.old)
		}
	}
}
