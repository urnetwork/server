package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The body of `function` in `path`, as source.
func lockBoundFunctionBody(t *testing.T, path string, function string) string {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, path, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == function && fn.Body != nil {
			return string(source[files.Position(fn.Body.Pos()).Offset:files.Position(fn.Body.End()).Offset])
		}
	}
	t.Fatalf("%s: function %s not found", path, function)
	return ""
}

// The main pool bounds every lock wait at 2 s (server.PgLockTimeout). These
// transactions replace the bound on purpose, each named here so a lost option
// is caught by this test rather than by a failed long run or a pinned pool:
// the long single-transaction jobs wait longer, and the best-effort stamps on
// the contract and connect paths wait less.
func TestTransactionsKeepTheirLockBounds(t *testing.T) {
	for _, c := range []struct {
		path     string
		function string
		call     string
	}{
		{path: "audit_provider_sweep_model.go", function: "BackfillDeviceAuditEvents", call: "server.TxLockTimeout(auditBackfillLockTimeout)"},
		{path: "audit_provider_sweep_model.go", function: "BackfillProviderAuditEvents", call: "server.TxLockTimeout(auditBackfillLockTimeout)"},
		{path: "network_client_model.go", function: "RemoveNetworkClientsBatch", call: "server.TxLockTimeout(removeNetworkClientsBatchLockTimeout)"},
		{path: "network_stats_model.go", function: "StampTopLevelClientContractTime", call: "server.TxLockTimeout(contractTimeStampLockTimeout)"},
		{path: "device_client_version_model.go", function: "SetDeviceClientVersion", call: "server.TxLockTimeout(deviceClientVersionLockTimeout)"},
	} {
		if body := lockBoundFunctionBody(t, c.path, c.function); !strings.Contains(body, c.call) {
			t.Errorf("%s: %s lost its lock bound %s", c.path, c.function, c.call)
		}
	}
	if !(auditBackfillLockTimeout > 2e9 && removeNetworkClientsBatchLockTimeout > 2e9) {
		t.Fatal("a long transaction's bound is not longer than the main pool's")
	}
	if !(contractTimeStampLockTimeout < 2e9 && deviceClientVersionLockTimeout < 2e9) {
		t.Fatal("a best-effort stamp's bound is not shorter than the main pool's")
	}
}
