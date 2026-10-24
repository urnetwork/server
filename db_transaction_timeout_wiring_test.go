// Pins, from the source, which operations lift the application role's
// transaction bound (PARTITION-MAINTENANCE-PLAN.md A6). The behavior of the
// helpers is pinned on a fake session, on the wire and against PostgreSQL; here
// the rule is that every long operation the inventory found calls one, so a
// refactor cannot silently drop an exemption and leave the operation to be
// ended at 15 minutes. Only go/ast and file names are used, so the test also
// runs, and fails, against the source before the exemptions.
package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The functions of the non-test files of directory, by name, and methods by
// their receiver type and name (`Type.method`).
func transactionTimeoutWiringFunctions(t *testing.T, directory string) map[string]*ast.FuncDecl {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	functions := map[string]*ast.FuncDecl{}
	fileset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			name := function.Name.Name
			if function.Recv != nil && len(function.Recv.List) == 1 {
				receiver := function.Recv.List[0].Type
				if star, ok := receiver.(*ast.StarExpr); ok {
					receiver = star.X
				}
				if identifier, ok := receiver.(*ast.Ident); ok {
					name = identifier.Name + "." + name
				}
			}
			functions[name] = function
		}
	}
	return functions
}

// Whether the function's body names identifier (a call, a selector's name or
// a constant) or carries a string literal containing literal.
func transactionTimeoutWiringUses(function *ast.FuncDecl, identifier string, literal string) bool {
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.Ident:
			if identifier != "" && value.Name == identifier {
				found = true
			}
		case *ast.BasicLit:
			if literal != "" && value.Kind == token.STRING {
				if text, err := strconv.Unquote(value.Value); err == nil && strings.Contains(text, literal) {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// Whether the function lifts or replaces the transaction bound in any of the
// ways the helpers provide.
func transactionTimeoutWiringLifts(function *ast.FuncDecl) bool {
	for _, identifier := range []string{"WithoutTransactionTimeout", "SetTransactionTimeoutInTx", "liftTransactionTimeoutInTxSql", "boundReliabilityTransactionInTx"} {
		if transactionTimeoutWiringUses(function, identifier, "") {
			return true
		}
	}
	return transactionTimeoutWiringUses(function, "", "transaction_timeout = 0")
}

// A statement whose statement_timeout is lifted for a long run lifts the
// transaction bound in the same function, or the bound would end it anyway.
func TestLiftedStatementTimeoutsAlsoLiftTransactionTimeout(t *testing.T) {
	checked := 0
	for _, directory := range []string{".", "model", "controller", "taskworker/work", "task", "bringyourctl"} {
		if _, err := os.Stat(directory); err != nil {
			t.Fatalf("%s: %v", directory, err)
		}
		for name, function := range transactionTimeoutWiringFunctions(t, directory) {
			if !transactionTimeoutWiringUses(function, "", "statement_timeout = 0") {
				continue
			}
			checked += 1
			if !transactionTimeoutWiringLifts(function) {
				t.Errorf("%s/%s lifts statement_timeout without lifting the transaction bound", directory, name)
			}
		}
	}
	if checked < 8 {
		t.Fatalf("found %d functions that lift statement_timeout, want the inventory's at least 8", checked)
	}
}

// Each long operation of the A6 inventory lifts the bound for itself, in any
// of the helpers' forms; the identifiers and literals a case lists are what it
// must keep using besides. A case with a file is an operation of a sibling
// plan branch, enforced wherever that file is present (after its merge).
func TestLongOperationsLiftTransactionTimeout(t *testing.T) {
	for _, c := range []struct {
		directory string
		file      string
		function  string
		// what the inventory needs this function to keep using
		identifiers []string
		literals    []string
	}{
		// a rebuild's discarded session (A1-A3 lift it with the helper)
		{directory: ".", function: "dbMaintenanceRunReindex", identifiers: []string{"AcquireMaintenanceDbConn", "discardPgConnection"}},
		// an operator VACUUM FULL's own discarded session
		{directory: ".", function: "VacuumFullAllTables", identifiers: []string{"AcquireMaintenanceDbConn", "discardPgConnection"}, literals: []string{"SET transaction_timeout = 0"}},
		// A4: a bounded pgstattuple_approx scan on a discarded session
		{directory: ".", file: "db_bloat_measure.go", function: "dbBloatScanRelation", identifiers: []string{"WithoutTransactionTimeout"}},
		// C2: the egress health history copy, its locked swap's early count,
		// and the rows the swap left behind
		{directory: "model", file: "model/provider_egress_health_history_cutover.go", function: "providerEgressHealthHistoryCutover.copyWindow", identifiers: []string{"WithoutTransactionTimeout"}},
		{directory: "model", file: "model/provider_egress_health_history_cutover.go", function: "providerEgressHealthHistoryCutover.swap", identifiers: []string{"WithoutTransactionTimeout"}},
		{directory: "model", file: "model/provider_egress_health_history_cutover.go", function: "providerEgressHealthHistoryCutover.missingRows", identifiers: []string{"WithoutTransactionTimeout"}},
		// A7: an unbounded ANALYZE of a family's parent or leaf
		{directory: ".", file: "db_partition_registry.go", function: "partitionRegistryAnalyze"},
		// online migrations; schema fixes
		{directory: ".", function: "runOnlineSqlMigration", identifiers: []string{"WithoutTransactionTimeout"}},
		{directory: ".", function: "ApplySchemaFix", identifiers: []string{"SetTransactionTimeoutInTx"}},
		// the subnet epoch census, one statement in one snapshot
		{directory: "model", function: "getStEpochProviderUsageWithCensus", identifiers: []string{"MaintenanceDb", "WithoutTransactionTimeout"}},
		// the weekly payment plan
		{directory: "model", function: "configurePaymentPlanTransaction", identifiers: []string{"SetTransactionTimeoutInTx"}},
		// the reliability transactions over every client
		{directory: "model", function: "UpdateClientLocationReliabilities", identifiers: []string{"boundReliabilityTransactionInTx"}},
		{directory: "model", function: "UpdateClientReliabilityScores", identifiers: []string{"boundReliabilityTransactionInTx"}},
		{directory: "model", function: "UpdateNetworkReliabilityWindow", identifiers: []string{"boundReliabilityTransactionInTx"}},
		{directory: "model", function: "boundReliabilityTransactionInTx", identifiers: []string{"SetTransactionTimeoutInTx", "reliabilityRunningCheckpointStatementTimeout"}},
		// operator backfills and a partition index build
		{directory: "model", function: "BackfillProviderAuditEvents", identifiers: []string{"SetTransactionTimeoutInTx"}},
		{directory: "model", function: "BackfillDeviceAuditEvents", identifiers: []string{"SetTransactionTimeoutInTx"}},
		{directory: "model", function: "BackfillSweepDestinationIds", identifiers: []string{"SetTransactionTimeoutInTx"}},
		{directory: "model", function: "UpgradeClientReliabilitySecondaryIndex", identifiers: []string{"WithoutTransactionTimeout"}},
		// a full location search re-index
		{directory: "taskworker/work", function: "IndexSearchLocations", identifiers: []string{"SetTransactionTimeoutInTx"}},
	} {
		if c.file != "" {
			if _, err := os.Stat(c.file); err != nil {
				t.Logf("%s is not in this tree; %s is checked once it is", c.file, c.function)
				continue
			}
		}
		function, ok := transactionTimeoutWiringFunctions(t, c.directory)[c.function]
		if !ok {
			t.Errorf("%s/%s is missing", c.directory, c.function)
			continue
		}
		if !transactionTimeoutWiringLifts(function) {
			t.Errorf("%s/%s no longer lifts the transaction bound", c.directory, c.function)
		}
		for _, identifier := range c.identifiers {
			if !transactionTimeoutWiringUses(function, identifier, "") {
				t.Errorf("%s/%s no longer uses %s", c.directory, c.function, identifier)
			}
		}
		for _, literal := range c.literals {
			if !transactionTimeoutWiringUses(function, "", literal) {
				t.Errorf("%s/%s no longer runs %q", c.directory, c.function, literal)
			}
		}
	}
}
