package work

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Every taskworker probe pass that opens provider tunnels places them
// (connect/MOVE.md §13): the tunnel config each capability and URL probe pass
// builds names the pass's own placement, which the pass creates and closes. A
// config built without one would leave the pass's tunnels to DNS, which is
// caught here rather than by an imbalance in production.
func TestProbePassesPlaceTheirTunnels(t *testing.T) {
	for _, c := range []struct {
		path     string
		function string
	}{
		{path: "capability_probe_work.go", function: "runCapabilityProbeShard"},
		{path: "provider_egress_probe_work.go", function: "runProviderEgressProbe"},
	} {
		files := token.NewFileSet()
		parsed, err := parser.ParseFile(files, c.path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var body *ast.BlockStmt
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == c.function {
				body = fn.Body
			}
		}
		if body == nil {
			t.Fatalf("%s: function %s not found", c.path, c.function)
		}
		created, closed := false, false
		configs, placedConfigs := 0, 0
		ast.Inspect(body, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.AssignStmt:
				if len(n.Lhs) == 1 && len(n.Rhs) == 1 {
					if ident, ok := n.Lhs[0].(*ast.Ident); ok && ident.Name == "placement" {
						if call, ok := n.Rhs[0].(*ast.CallExpr); ok {
							if fun, ok := call.Fun.(*ast.Ident); ok && fun.Name == "newProbeEdgePlacement" {
								created = true
							}
						}
					}
				}
			case *ast.DeferStmt:
				if selector, ok := n.Call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Close" {
					if ident, ok := selector.X.(*ast.Ident); ok && ident.Name == "placement" {
						closed = true
					}
				}
			case *ast.CompositeLit:
				selector, ok := n.Type.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Config" || len(n.Elts) == 0 {
					return true
				}
				if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != "providertunnel" {
					return true
				}
				configs += 1
				for _, elt := range n.Elts {
					if keyValue, ok := elt.(*ast.KeyValueExpr); ok {
						key, keyOk := keyValue.Key.(*ast.Ident)
						value, valueOk := keyValue.Value.(*ast.Ident)
						if keyOk && valueOk && key.Name == "PlatformPlacement" && value.Name == "placement" {
							placedConfigs += 1
						}
					}
				}
			}
			return true
		})
		if !created || !closed {
			t.Errorf("%s: %s does not create and close its pass placement (created=%t closed=%t)", c.path, c.function, created, closed)
		}
		if configs == 0 || placedConfigs != configs {
			t.Errorf("%s: %s builds %d tunnel configs, %d of them placed", c.path, c.function, configs, placedConfigs)
		}
	}
}
