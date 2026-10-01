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
	limits := []string{}
	files, err := productionAlertClassSourcePaths(directory)
	if err != nil {
		t.Fatal(err)
	}
	fileset := token.NewFileSet()
	parsedFiles := map[string]*ast.File{}
	for _, path := range files {
		parsed, err := parser.ParseFile(fileset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Base(path), err)
		}
		parsedFiles[filepath.Base(path)] = parsed
	}
	scanner := newFixedAlertClassScanner(parsedFiles, fileset)
	scanner.collect(classes, &limits)
	sort.Strings(limits)
	t.Logf("Bounded class crosswalk: fixed=%d unresolved_flow_sites=%d; computed/interprocedural flows are explicit audit limits, not semantic completeness", len(classes), len(limits))
	for _, limit := range limits {
		t.Log(limit)
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

func productionAlertClassSourcePaths(directory string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		return nil, err
	}
	productionPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		if !strings.HasSuffix(path, "_test.go") {
			productionPaths = append(productionPaths, path)
		}
	}
	return productionPaths, nil
}

func collectFixedLogAlertClassLiterals(tailer *ast.File, source string, classes map[string][]string) {
	scanner := newFixedAlertClassScanner(map[string]*ast.File{source: tailer})
	limits := []string{}
	scanner.collectLogClasses(tailer, source, classes, &limits)
}

func collectFixedAlertClassLiterals(parsed ast.Node, source string, classes map[string][]string) {
	file, ok := parsed.(*ast.File)
	if !ok {
		return
	}
	scanner := newFixedAlertClassScanner(map[string]*ast.File{source: file})
	limits := []string{}
	scanner.collect(classes, &limits)
}

// This is a bounded test-only dataflow census, not a Go type checker. Resolve
// AST-bound finite strings and helper parameters tied to typed Alert/finding
// sinks; unknown aliases, receiver dispatch and computed flows stay explicit.
type fixedAlertClassScanner struct {
	files              map[string]*ast.File
	bindings           map[*ast.Object][]ast.Expr
	returnSlotBindings map[*ast.Object][]fixedAlertClassReturnSlot
	globalBindings     map[string][]ast.Expr
	globalFunctions    map[string][]*ast.FuncDecl
	helperInputIndexes map[string]map[int]bool
	helperObjectInputs map[*ast.Object]map[int]bool
	compositeTypes     map[*ast.CompositeLit]string
	fileset            *token.FileSet
}

// Preserve the selected assignment slot; neighboring explanations are not
// classes, and unresolved dispatch must not borrow an unrelated declaration.
type fixedAlertClassReturnSlot struct {
	call        *ast.CallExpr
	resultIndex int
	resultCount int
}

func newFixedAlertClassScanner(files map[string]*ast.File, filesets ...*token.FileSet) *fixedAlertClassScanner {
	self := &fixedAlertClassScanner{
		files: files, bindings: map[*ast.Object][]ast.Expr{}, globalBindings: map[string][]ast.Expr{},
		returnSlotBindings: map[*ast.Object][]fixedAlertClassReturnSlot{}, globalFunctions: map[string][]*ast.FuncDecl{},
		helperInputIndexes: map[string]map[int]bool{}, helperObjectInputs: map[*ast.Object]map[int]bool{}, compositeTypes: map[*ast.CompositeLit]string{},
	}
	if len(filesets) != 0 {
		self.fileset = filesets[0]
	}
	for _, file := range files {
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil {
				self.globalFunctions[function.Name.Name] = append(self.globalFunctions[function.Name.Name], function)
			}
			if general, ok := declaration.(*ast.GenDecl); ok {
				for _, raw := range general.Specs {
					if spec, ok := raw.(*ast.ValueSpec); ok && len(spec.Values) == len(spec.Names) {
						for index, name := range spec.Names {
							self.globalBindings[name.Name] = append(self.globalBindings[name.Name], spec.Values[index])
						}
					}
				}
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.ValueSpec:
				if len(value.Values) == len(value.Names) {
					for index, name := range value.Names {
						self.bindings[name.Obj] = append(self.bindings[name.Obj], value.Values[index])
					}
				}
			case *ast.AssignStmt:
				if len(value.Lhs) == len(value.Rhs) {
					for index, target := range value.Lhs {
						if name, ok := target.(*ast.Ident); ok && name.Obj != nil {
							if value.Tok == token.ASSIGN || value.Tok == token.DEFINE {
								self.bindings[name.Obj] = append(self.bindings[name.Obj], value.Rhs[index])
							} else {
								self.bindings[name.Obj] = append(self.bindings[name.Obj], nil)
							}
						}
					}
				} else if len(value.Lhs) > 1 && len(value.Rhs) == 1 {
					call, directCall := value.Rhs[0].(*ast.CallExpr)
					for index, target := range value.Lhs {
						if name, ok := target.(*ast.Ident); ok && name.Obj != nil {
							if directCall && (value.Tok == token.ASSIGN || value.Tok == token.DEFINE) {
								self.returnSlotBindings[name.Obj] = append(self.returnSlotBindings[name.Obj], fixedAlertClassReturnSlot{
									call: call, resultIndex: index, resultCount: len(value.Lhs),
								})
							} else {
								self.bindings[name.Obj] = append(self.bindings[name.Obj], nil)
							}
						}
					}
				}
			case *ast.CompositeLit:
				if name, ok := value.Type.(*ast.Ident); ok {
					self.compositeTypes[value] = name.Name
				}
				if array, ok := value.Type.(*ast.ArrayType); ok {
					if name, ok := array.Elt.(*ast.Ident); ok {
						for _, element := range value.Elts {
							if child, ok := element.(*ast.CompositeLit); ok && child.Type == nil {
								self.compositeTypes[child] = name.Name
							}
						}
					}
				}
			}
			return true
		})
	}
	// Derive helper argument positions only from real typed class sinks, and
	// propagate through direct function calls until the finite graph stabilizes.
	type classHelper struct {
		object     *ast.Object
		name       string
		parameters *ast.FieldList
		body       *ast.BlockStmt
		global     bool
	}
	helpers := []classHelper{}
	for _, file := range files {
		for _, raw := range file.Decls {
			if function, ok := raw.(*ast.FuncDecl); ok && function.Recv == nil && function.Body != nil {
				helpers = append(helpers, classHelper{object: function.Name.Obj, name: function.Name.Name, parameters: function.Type.Params, body: function.Body, global: true})
			}
		}
	}
	for object, bindings := range self.bindings {
		if len(bindings) == 1 {
			if function, ok := bindings[0].(*ast.FuncLit); ok {
				helpers = append(helpers, classHelper{object: object, parameters: function.Type.Params, body: function.Body})
			}
		}
	}
	for pass := 0; pass <= len(helpers); pass++ {
		changed := false
		for _, function := range helpers {
			parameters := map[*ast.Object]int{}
			index := 0
			for _, field := range function.parameters.List {
				for _, name := range field.Names {
					parameters[name.Obj] = index
					index++
				}
			}
			for _, input := range self.classInputs(function.body) {
				var inspectInput func(ast.Expr, map[*ast.Object]bool)
				inspectInput = func(expression ast.Expr, seen map[*ast.Object]bool) {
					if parenthesized, ok := expression.(*ast.ParenExpr); ok {
						inspectInput(parenthesized.X, seen)
						return
					}
					name, ok := expression.(*ast.Ident)
					if !ok || name.Obj == nil || seen[name.Obj] {
						return
					}
					seen[name.Obj] = true
					if parameterIndex, ok := parameters[name.Obj]; ok {
						if self.helperObjectInputs[function.object] == nil {
							self.helperObjectInputs[function.object] = map[int]bool{}
							if function.global {
								self.helperInputIndexes[function.name] = self.helperObjectInputs[function.object]
							}
						}
						if !self.helperObjectInputs[function.object][parameterIndex] {
							self.helperObjectInputs[function.object][parameterIndex] = true
							changed = true
						}
					} else {
						for _, binding := range self.bindings[name.Obj] {
							inspectInput(binding, seen)
						}
					}
				}
				inspectInput(input, map[*ast.Object]bool{})
			}
		}
		if !changed {
			break
		}
	}
	return self
}

// Only typed composites or direct calls into a proved class parameter are
// sinks; identically named map labels, DNS fields and formatting are not.
func (self *fixedAlertClassScanner) classInputs(root ast.Node) []ast.Expr {
	inputs := []ast.Expr{}
	ast.Inspect(root, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.CompositeLit:
			kind := self.compositeTypes[value]
			if kind == "finding" || kind == "Alert" {
				for _, raw := range value.Elts {
					if field, ok := raw.(*ast.KeyValueExpr); ok {
						if name, ok := field.Key.(*ast.Ident); ok && (kind == "finding" && name.Name == "class" || kind == "Alert" && name.Name == "Class") {
							inputs = append(inputs, field.Value)
						}
					}
				}
			}
		case *ast.CallExpr:
			if function, ok := value.Fun.(*ast.Ident); ok {
				indexes := self.helperObjectInputs[function.Obj]
				if function.Obj == nil {
					indexes = self.helperInputIndexes[function.Name]
				}
				for index := range indexes {
					if index < len(value.Args) {
						inputs = append(inputs, value.Args[index])
					}
				}
			}
		}
		return true
	})
	return inputs
}

// Preserve finite alternatives and report a site as unresolved if any branch
// is computed. A cycle or unsupported expression never self-defines health.
func (self *fixedAlertClassScanner) strings(expression ast.Expr, seen map[*ast.Object]bool, depth int) (map[string]bool, bool) {
	values := map[string]bool{}
	if depth > 64 {
		return values, false
	}
	switch value := expression.(type) {
	case *ast.BasicLit:
		if value.Kind == token.STRING {
			if text, err := strconv.Unquote(value.Value); err == nil {
				values[text] = true
				return values, true
			}
		}
	case *ast.ParenExpr:
		return self.strings(value.X, seen, depth+1)
	case *ast.Ident:
		bindings := self.bindings[value.Obj]
		returnSlots := self.returnSlotBindings[value.Obj]
		if value.Obj == nil {
			bindings = self.globalBindings[value.Name]
		} else if seen[value.Obj] {
			return values, false
		}
		if len(bindings) == 0 && len(returnSlots) == 0 {
			return values, false
		}
		if value.Obj != nil {
			seen[value.Obj] = true
			defer delete(seen, value.Obj)
		}
		complete := true
		for _, binding := range bindings {
			alternatives, known := self.strings(binding, seen, depth+1)
			complete = complete && known
			for alternative := range alternatives {
				values[alternative] = true
			}
		}
		for _, slot := range returnSlots {
			alternatives, known := self.returnSlotStrings(slot, seen, depth+1)
			complete = complete && known
			for alternative := range alternatives {
				values[alternative] = true
			}
		}
		return values, complete
	case *ast.BinaryExpr:
		if value.Op == token.ADD {
			left, leftKnown := self.strings(value.X, seen, depth+1)
			right, rightKnown := self.strings(value.Y, seen, depth+1)
			if len(left)*len(right) > 64 {
				return values, false
			}
			for prefix := range left {
				for suffix := range right {
					values[prefix+suffix] = true
				}
			}
			return values, leftKnown && rightKnown
		}
	}
	return values, false
}

// Resolve only explicit, named string result slots from an unambiguous direct
// declaration. This is not receiver dispatch, argument substitution or a type
// checker; unsupported returns and partially computed branches remain limits.
func (self *fixedAlertClassScanner) returnSlotStrings(slot fixedAlertClassReturnSlot, seen map[*ast.Object]bool, depth int) (map[string]bool, bool) {
	values := map[string]bool{}
	name, direct := slot.call.Fun.(*ast.Ident)
	if !direct || slot.call.Ellipsis.IsValid() || depth > 64 || slot.resultCount > 16 {
		return values, false
	}
	functions := self.globalFunctions[name.Name]
	if len(functions) != 1 {
		return values, false
	}
	function := functions[0]
	if name.Obj != nil && name.Obj.Decl != function {
		return values, false
	}
	if function.Body == nil || function.Type.Results == nil {
		return values, false
	}
	resultCount := 0
	stringSlot := false
	for _, field := range function.Type.Results.List {
		if len(field.Names) == 0 {
			return values, false
		}
		if resultCount <= slot.resultIndex && slot.resultIndex < resultCount+len(field.Names) {
			kind, ok := field.Type.(*ast.Ident)
			stringSlot = ok && kind.Name == "string"
		}
		resultCount += len(field.Names)
	}
	if resultCount != slot.resultCount || !stringSlot {
		return values, false
	}
	returns := []ast.Expr{}
	complete := true
	deferredResult := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		if _, deferred := node.(*ast.DeferStmt); deferred {
			deferredResult = true
			return false
		}
		if statement, ok := node.(*ast.ReturnStmt); ok {
			if len(statement.Results) != resultCount || len(returns) >= 64 {
				complete = false
			} else {
				returns = append(returns, statement.Results[slot.resultIndex])
			}
			return false
		}
		return true
	})
	if len(returns) == 0 || deferredResult {
		return values, false
	}
	for _, expression := range returns {
		alternatives, known := self.strings(expression, seen, depth+1)
		complete = complete && known
		for alternative := range alternatives {
			values[alternative] = true
		}
	}
	return values, complete
}

func (self *fixedAlertClassScanner) collect(classes map[string][]string, limits *[]string) {
	for source, file := range self.files {
		for _, input := range self.classInputs(file) {
			values, complete := self.strings(input, map[*ast.Object]bool{}, 0)
			for value := range values {
				appendFixedAlertClass(classes, value, source)
			}
			if !complete {
				location := source
				if self.fileset != nil {
					location += ":" + strconv.Itoa(self.fileset.Position(input.Pos()).Line)
				}
				kind := "computed/local producer flow"
				switch expression := input.(type) {
				case *ast.Ident:
					if expression.Obj != nil {
						if _, parameter := expression.Obj.Decl.(*ast.Field); parameter {
							kind = "helper-parameter passthrough; direct fixed call inputs scanned, other dispatch unresolved"
						}
					}
				case *ast.SelectorExpr:
					kind = "field/copy flow; not an independently resolved new producer"
				case *ast.CallExpr:
					kind = "computed-call producer flow"
				case *ast.BinaryExpr:
					kind = "computed-concatenation producer flow"
				}
				*limits = append(*limits, location+": unresolved "+kind)
			}
		}
		self.collectLogClasses(file, source, classes, limits)
	}
}

func appendFixedAlertClass(classes map[string][]string, class, source string) {
	if class == "" {
		return
	}
	for _, existing := range classes[class] {
		if existing == source {
			return
		}
	}
	classes[class] = append(classes[class], source)
}

// Recognize the actual typed parent/event and emitted burst only. Canonical
// metrics and correlation-only bursts are distinct observability names.
func (self *fixedAlertClassScanner) collectLogClasses(file *ast.File, source string, classes map[string][]string, limits *[]string) {
	ast.Inspect(file, func(node ast.Node) bool {
		composite, ok := node.(*ast.CompositeLit)
		if !ok || self.compositeTypes[composite] != "logClass" {
			return true
		}
		fields := map[string]ast.Expr{}
		for _, raw := range composite.Elts {
			if field, ok := raw.(*ast.KeyValueExpr); ok {
				if name, ok := field.Key.(*ast.Ident); ok {
					fields[name.Name] = field.Value
				}
			}
		}
		if metricOnly, ok := fields["metricOnly"].(*ast.Ident); ok && metricOnly.Name == "true" {
			return false
		}
		if fields["metricOnly"] != nil {
			if value, ok := fields["metricOnly"].(*ast.Ident); !ok || value.Name != "false" {
				*limits = append(*limits, source+": unresolved log metric-only mode")
				return false
			}
		}
		values, known := self.strings(fields["name"], map[*ast.Object]bool{}, 0)
		for value := range values {
			appendFixedAlertClass(classes, value, source)
		}
		if !known {
			*limits = append(*limits, source+": unresolved typed log class name")
		}
		burstExpression := fields["burst"]
		if pointer, ok := burstExpression.(*ast.UnaryExpr); ok && pointer.Op == token.AND {
			burstExpression = pointer.X
		}
		burst, ok := burstExpression.(*ast.CompositeLit)
		if !ok || self.compositeTypes[burst] != "logBurst" {
			if burstExpression != nil {
				if value, nilValue := burstExpression.(*ast.Ident); !nilValue || value.Name != "nil" {
					*limits = append(*limits, source+": unresolved burst expression")
				}
			}
			return false
		}
		burstFields := map[string]ast.Expr{}
		for _, raw := range burst.Elts {
			if field, ok := raw.(*ast.KeyValueExpr); ok {
				if name, ok := field.Key.(*ast.Ident); ok {
					burstFields[name.Name] = field.Value
				}
			}
		}
		if flag := burstFields["correlationOnly"]; flag != nil {
			if value, ok := flag.(*ast.Ident); !ok || value.Name != "false" {
				if !ok || value.Name != "true" {
					*limits = append(*limits, source+": unresolved burst correlation mode")
				}
				return false
			}
		}
		values, known = self.strings(burstFields["name"], map[*ast.Object]bool{}, 0)
		for value := range values {
			appendFixedAlertClass(classes, value, source)
		}
		if !known {
			*limits = append(*limits, source+": unresolved emitted burst name")
		}
		return false
	})
}

func TestFixedAlertClassScannerIncludesInternalAndPublicLiteralFields(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "signal_synthetic.go", `package synthetic
var internal = finding{class: "synthetic-internal-class"}
var public = Alert{Class: "synthetic-public-class"}
var unrelated = Alert{Symptom: "not-an-alert-class"}
var computed = Alert{Class: dynamicClass}
var empty = Alert{Class: ""}
var mapped = map[string]string{"Class": "not-a-struct-field"}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string][]string{}
	collectFixedAlertClassLiterals(parsed, "signal_synthetic.go", classes)
	if len(classes) != 2 {
		t.Fatalf("literal scanner captured %d classes, want internal and public: %+v", len(classes), classes)
	}
	for _, expected := range []string{"synthetic-internal-class", "synthetic-public-class"} {
		if sources := classes[expected]; len(sources) != 1 || sources[0] != "signal_synthetic.go" {
			t.Fatalf("literal scanner lost exact source for %q: %+v", expected, sources)
		}
	}
}

func TestFixedAlertClassScannerIncludesSharedProductionSources(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"signal_synthetic.go", "postgres_checks.go", "tailer.go", "conn.go", "signal_synthetic_test.go", "shared_test.go"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("package synthetic\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := productionAlertClassSourcePaths(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 4 {
		t.Fatalf("source inventory has %d files, want all four production files without tests", len(paths))
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			t.Fatal("test fixture entered the production class inventory")
		}
	}
}

func TestFixedAlertClassScannerResolvesConstantsLocalAlternativesAndHelperInputs(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "shared_checks.go", `package synthetic
const fixedClass = "synthetic-constant-class"
func typedClassHelper(class string) finding { return finding{class: class} }
func observe(useAlternate bool) finding {
 class := "synthetic-local-class"
 if useAlternate { class = "synthetic-alternate-class" }
 _ = finding{class: fixedClass}
 _ = typedClassHelper("synthetic-helper-class")
 return finding{class: class}
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string][]string{}
	collectFixedAlertClassLiterals(parsed, "shared_checks.go", classes)
	if len(classes) != 4 {
		t.Fatalf("fixed bindings/helper inventory has %d classes, want four", len(classes))
	}
	for _, class := range []string{"synthetic-constant-class", "synthetic-local-class", "synthetic-alternate-class", "synthetic-helper-class"} {
		if sources := classes[class]; len(sources) != 1 || sources[0] != "shared_checks.go" {
			t.Fatalf("fixed class %q lost its source attribution", class)
		}
	}
}

func TestFixedAlertClassScannerIgnoresNonAlertFieldsAndArbitraryArguments(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "shared_checks.go", `package synthetic
// finding{class: "synthetic-comment-not-emitted"}
var dns = dnsQuestion{Class: "synthetic-dns-not-emitted"}
var labels = label{class: "synthetic-label-not-emitted"}
var mapped = map[string]string{"class": "synthetic-map-not-emitted"}
func format(class string) string { return class }
var formatted = format("synthetic-format-not-emitted")
var actual = []finding{{class: "synthetic-actual-class"}}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string][]string{}
	collectFixedAlertClassLiterals(parsed, "shared_checks.go", classes)
	if len(classes) != 1 || len(classes["synthetic-actual-class"]) != 1 {
		t.Fatalf("non-alert data self-defined the class inventory: %+v", classes)
	}
}

func TestFixedAlertClassScannerRetainsCrossFileHelpersShadowingAndUnknownFlowLimits(t *testing.T) {
	files := map[string]*ast.File{}
	for source, code := range map[string]string{
		"shared_checks.go": `package synthetic
const sharedClass = "synthetic-shared-class"
func emit(class string) finding { return finding{class: class} }
`,
		"signal_synthetic.go": `package synthetic
func observe(dynamic string) finding {
 _ = emit(sharedClass)
 { emit := func(class string) string { return class }; _ = emit("synthetic-shadow-not-alert") }
 local := func(class string) finding { return finding{class: class} }
 _ = local("synthetic-local-helper-class")
 class := "synthetic-known-alternative"
 if dynamic != "" { class = dynamic }
 return finding{class: class}
}
`,
	} {
		parsed, err := parser.ParseFile(token.NewFileSet(), source, code, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[source] = parsed
	}
	classes := map[string][]string{}
	limits := []string{}
	newFixedAlertClassScanner(files).collect(classes, &limits)
	for _, class := range []string{"synthetic-shared-class", "synthetic-local-helper-class", "synthetic-known-alternative"} {
		if sources := classes[class]; len(sources) != 1 || sources[0] != "signal_synthetic.go" {
			t.Fatalf("cross-file/local helper lost its emitting call source: class=%q sources=%v", class, sources)
		}
	}
	if len(classes) != 3 || len(limits) == 0 {
		t.Fatalf("shadowed formatter or dynamic input was falsely exact-certified: fixed=%d limits=%d", len(classes), len(limits))
	}
}

func TestFixedAlertClassScannerDoesNotTreatTransformedHelperArgumentsAsExactClasses(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "shared_checks.go", `package synthetic
func transformed(class string) finding { return finding{class: "prefix-" + class} }
func computed(class string) finding { return finding{class: normalize(class)} }
var first = transformed("synthetic-transformed-input-not-exact-class")
var second = computed("synthetic-computed-input-not-exact-class")
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string][]string{}
	limits := []string{}
	newFixedAlertClassScanner(map[string]*ast.File{"shared_checks.go": parsed}).collect(classes, &limits)
	if len(classes) != 0 || len(limits) != 2 {
		t.Fatalf("transformed/computed helper inputs were falsely exact-certified: fixed=%d limits=%d", len(classes), len(limits))
	}
}

func TestFixedLogAlertClassScannerDistinguishesTypedEventsAndObservabilityNames(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "tailer.go", `package synthetic
var logClasses = []logClass{
 {name: "synthetic-log-class", canonical: &logCanonical{name: "synthetic-canonical-not-alert"}, burst: &logBurst{name: "synthetic-burst-class"}},
 {name: "synthetic-correlation-parent", burst: &logBurst{name: "synthetic-correlation-not-alert", correlationOnly: true}},
 {name: "synthetic-metric-not-alert", metricOnly: true},
}
var arbitrary = []label{{name: "synthetic-label-not-alert"}}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string][]string{}
	collectFixedLogAlertClassLiterals(parsed, "tailer.go", classes)
	if len(classes) != 3 {
		t.Fatalf("typed log class inventory has %d classes, want parent/parent/burst", len(classes))
	}
	for _, class := range []string{"synthetic-log-class", "synthetic-correlation-parent", "synthetic-burst-class"} {
		if sources := classes[class]; len(sources) != 1 || sources[0] != "tailer.go" {
			t.Fatalf("typed log class %q lost exact source attribution", class)
		}
	}
}

func TestFixedAlertClassScannerReturnSlotViolatedCatalog(t *testing.T) {
	files := map[string]*ast.File{}
	for source, code := range map[string]string{
		"shared_checks.go": `package synthetic
func classify(branch int) (class, mechanism, action string) {
 if branch == 0 { return "synthetic-first-class", "not-a-class-mechanism", "not-a-class-action" }
 if branch == 1 { return "synthetic-second-class", "not-a-class-mechanism", "not-a-class-action" }
 return "synthetic-missing-class", "not-a-class-mechanism", "not-a-class-action"
}
`,
		"signal_synthetic.go": `package synthetic
func observe(branch int) finding {
 class, mechanism, action := classify(branch)
 return finding{class: class, context: mechanism, action: action}
}
`,
	} {
		parsed, err := parser.ParseFile(token.NewFileSet(), source, code, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[source] = parsed
	}
	classes := map[string][]string{}
	limits := []string{}
	newFixedAlertClassScanner(files).collect(classes, &limits)
	if len(classes) != 3 || len(limits) != 0 {
		t.Fatalf("return-slot producer escaped the inventory: fixed=%d unresolved=%d, want three exact classes", len(classes), len(limits))
	}
	catalog := "`synthetic-first-class`, `synthetic-second-class`"
	missing := []string{}
	for class, sources := range classes {
		if len(sources) != 1 || sources[0] != "signal_synthetic.go" {
			t.Fatalf("return-slot class %q lost its typed sink source: %v", class, sources)
		}
		if !catalogHasExactClass(catalog, class) {
			missing = append(missing, class)
		}
	}
	if len(missing) != 1 || missing[0] != "synthetic-missing-class" {
		t.Fatalf("return-slot catalog omission was falsely certified: missing=%v", missing)
	}
}

func TestFixedAlertClassScannerReturnSlotHealthyEmptyClass(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "shared_checks.go", `package synthetic
func classify() (class string, mechanism string, broken bool) { return "", "healthy explanation is not a class", false }
func observe() finding {
 class, mechanism, broken := classify()
 return finding{class: class, context: mechanism, violated: broken}
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string][]string{}
	limits := []string{}
	newFixedAlertClassScanner(map[string]*ast.File{"shared_checks.go": parsed}).collect(classes, &limits)
	if len(classes) != 0 || len(limits) != 0 {
		t.Fatalf("proved empty result became a class or unresolved producer: fixed=%d unresolved=%d", len(classes), len(limits))
	}
}

func TestFixedAlertClassScannerReturnSlotUnknownRetainsFiniteBranch(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "shared_checks.go", `package synthetic
func classify(dynamic string, known bool) (class, mechanism, action string) {
 if known { return "synthetic-known-class", "not-a-class-mechanism", "not-a-class-action" }
 return normalize(dynamic), "not-a-class-mechanism", "not-a-class-action"
}
func observe() finding {
 class, mechanism, action := classify("synthetic-input-not-a-proved-class", false)
 return finding{class: class, context: mechanism, action: action}
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string][]string{}
	limits := []string{}
	newFixedAlertClassScanner(map[string]*ast.File{"shared_checks.go": parsed}).collect(classes, &limits)
	if len(classes) != 1 || len(classes["synthetic-known-class"]) != 1 || len(limits) != 1 {
		t.Fatalf("partly computed return was falsely exact or lost its finite branch: classes=%v unresolved=%d", classes, len(limits))
	}
}

func TestFixedAlertClassScannerReturnSlotShadowedAndUnsupportedCalls(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "shared_checks.go", `package synthetic
func classify() (class, mechanism, action string) { return "synthetic-global-not-called-class", "", "" }
func observe() finding {
 classify := func() (class, mechanism, action string) { return "synthetic-shadow-unsupported-class", "", "" }
 class, mechanism, action := classify()
 return finding{class: class, context: mechanism, action: action}
}
func parameter(classify func() (string, string, string)) finding {
 class, mechanism, action := classify()
 return finding{class: class, context: mechanism, action: action}
}
func receiver(source classifier) finding {
 class, mechanism, action := source.classify()
 return finding{class: class, context: mechanism, action: action}
}
func wrongArity() finding {
 class, mechanism := classify()
 return finding{class: class, context: mechanism}
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string][]string{}
	limits := []string{}
	newFixedAlertClassScanner(map[string]*ast.File{"shared_checks.go": parsed}).collect(classes, &limits)
	if len(classes) != 0 || len(limits) != 4 {
		t.Fatalf("unsupported/shadowed result borrowed a global definition: classes=%v unresolved=%d", classes, len(limits))
	}
}

func TestFixedAlertClassScannerReturnSlotIsolation(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "shared_checks.go", `package synthetic
func classify() (mechanism, class, action string) {
 nested := func() (mechanism, class, action string) { return "", "synthetic-nested-not-called-class", "" }
 _ = nested
 return "synthetic-mechanism-not-class", "synthetic-selected-class", "synthetic-action-not-class"
}
func observe() Alert {
 mechanism, class, action := classify()
 return Alert{Class: class, Context: mechanism, Action: action}
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string][]string{}
	limits := []string{}
	newFixedAlertClassScanner(map[string]*ast.File{"shared_checks.go": parsed}).collect(classes, &limits)
	if len(classes) != 1 || len(classes["synthetic-selected-class"]) != 1 || len(limits) != 0 {
		t.Fatalf("return-slot or nested-return isolation failed: classes=%v unresolved=%d", classes, len(limits))
	}
}

func TestFixedAlertClassScannerReturnSlotUnknownDeferredResult(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "shared_checks.go", `package synthetic
func classify() (class, mechanism, action string) {
 defer func() { class = normalize(class) }()
 return "synthetic-pre-defer-not-proved-class", "", ""
}
func observe() finding {
 class, mechanism, action := classify()
 return finding{class: class, context: mechanism, action: action}
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string][]string{}
	limits := []string{}
	newFixedAlertClassScanner(map[string]*ast.File{"shared_checks.go": parsed}).collect(classes, &limits)
	if len(classes) != 0 || len(limits) != 1 {
		t.Fatalf("named-result mutation was falsely resolved before deferred work: classes=%v unresolved=%d", classes, len(limits))
	}
}

func TestFixedAlertClassScannerReturnSlotUnknownAmbiguousDeclaration(t *testing.T) {
	files := map[string]*ast.File{}
	for source, code := range map[string]string{
		"shared_checks.go": `package synthetic
func classify() (class, mechanism, action string) { return "synthetic-ambiguous-first-class", "", "" }
`,
		"other_checks.go": `package synthetic
func classify() (class, mechanism, action string) { return "synthetic-ambiguous-second-class", "", "" }
`,
		"signal_synthetic.go": `package synthetic
func observe() finding {
 class, mechanism, action := classify()
 return finding{class: class, context: mechanism, action: action}
}
`,
	} {
		parsed, err := parser.ParseFile(token.NewFileSet(), source, code, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[source] = parsed
	}
	classes := map[string][]string{}
	limits := []string{}
	newFixedAlertClassScanner(files).collect(classes, &limits)
	if len(classes) != 0 || len(limits) != 1 {
		t.Fatalf("ambiguous declarations self-selected a class source: classes=%v unresolved=%d", classes, len(limits))
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
