package monitor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A new fixed Alert class without an exact SIGNALS.md spelling breaks the
// catalog -> implementation -> ledger crosswalk. Parse Go syntax rather than
// grepping so comments and test fixtures cannot satisfy the implementation
// side of the contract.
func TestEveryFixedProductionAlertClassIsCataloged(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate monitor package")
	}
	directory := filepath.Dir(currentFile)
	catalogBytes, err := os.ReadFile(filepath.Join(directory, "SIGNALS.md"))
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(catalogBytes)

	classes := map[string][]string{}
	files, err := filepath.Glob(filepath.Join(directory, "signal_*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fileset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Base(path), err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			field, ok := node.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			name, ok := field.Key.(*ast.Ident)
			if !ok || name.Name != "class" {
				return true
			}
			literal, ok := field.Value.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil || value == "" {
				return true
			}
			classes[value] = append(classes[value], filepath.Base(path))
			return true
		})
	}

	tailerPath := filepath.Join(directory, "tailer.go")
	tailer, err := parser.ParseFile(fileset, tailerPath, nil, 0)
	if err != nil {
		t.Fatalf("parse tailer.go: %v", err)
	}
	for _, declaration := range tailer.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.VAR {
			continue
		}
		for _, spec := range general.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok || len(valueSpec.Names) != 1 || valueSpec.Names[0].Name != "logClasses" || len(valueSpec.Values) != 1 {
				continue
			}
			list, ok := valueSpec.Values[0].(*ast.CompositeLit)
			if !ok {
				t.Fatal("logClasses is not a composite literal")
			}
			for _, element := range list.Elts {
				class, ok := element.(*ast.CompositeLit)
				if !ok {
					continue
				}
				for _, rawField := range class.Elts {
					field, ok := rawField.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					name, ok := field.Key.(*ast.Ident)
					literal, literalOK := field.Value.(*ast.BasicLit)
					if !ok || name.Name != "name" || !literalOK || literal.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(literal.Value)
					if err == nil && value != "" {
						classes[value] = append(classes[value], "tailer.go")
					}
				}
			}
		}
	}

	if len(classes) < 150 {
		t.Fatalf("fixed class inventory unexpectedly small: %d", len(classes))
	}
	missing := make([]string, 0)
	for class := range classes {
		if !catalogHasExactClass(catalog, class) {
			missing = append(missing, class)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Fatalf("fixed production Alert classes absent from SIGNALS.md: %s", strings.Join(missing, ", "))
	}
}

func catalogHasExactClass(catalog, class string) bool {
	isClassByte := func(value byte) bool {
		return 'a' <= value && value <= 'z' || 'A' <= value && value <= 'Z' ||
			'0' <= value && value <= '9' || value == '-' || value == '_'
	}
	for offset := 0; offset <= len(catalog)-len(class); {
		relative := strings.Index(catalog[offset:], class)
		if relative < 0 {
			return false
		}
		start := offset + relative
		end := start + len(class)
		leftBoundary := start == 0 || !isClassByte(catalog[start-1])
		rightBoundary := end == len(catalog) || !isClassByte(catalog[end])
		if leftBoundary && rightBoundary {
			return true
		}
		offset = start + 1
	}
	return false
}
