package monitor

// Every alert class the package source names is classified in alertClassKinds,
// so a new class cannot reach the Slack notifier without saying whether it
// reports an unknown state.

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The alert classes the package source names (see harvestAlertClasses).
type sourceAlertClasses struct {
	// class -> "file:line" of each site naming it
	classPositions map[string][]string
	// "file function" -> source of each class value it computes
	siteComputedValues map[string][]string
	// Alert literals, and finding literals of probe monitor/visibility
	markedSites []alertClassSite
	// "function#parameter" of each helper that takes a class
	helpers map[string]bool
}

// One composite literal that sets an alert class.
type alertClassSite struct {
	position string
	alert    bool
	// the SignalID or probeId literal
	signalId string
	// the unobservable value, or nil
	unobservable ast.Expr
	// every class the literal can take
	classes []string
}

// A declared function, or a closure assigned to a local name.
type classFrame struct {
	key            string
	nameParameters map[string]int
}

// What a class value resolves to: classes, the helper parameters it passes
// on, and whether any part of it is computed.
type classValue struct {
	classes  []string
	helpers  []string
	computed bool
}

// Walks the package's non-test source for every alert class it names. A class
// site is the class or Class key of a composite literal, an assignment to a
// class or Class field, or the name of a log class or burst. Its value
// resolves through string literals, constants, variables assigned only such
// values, range keys and values over literal lists, and the parameters of
// helpers, whose call sites resolve in turn. Any other value is computed.
func harvestAlertClasses(t *testing.T) sourceAlertClasses {
	// composite types whose class field is not an alert class or only copies one
	deniedTypes := map[string]bool{
		"consoleJsonEvent": true, "dnsmessage.Question": true, "egressSitePoolDestination": true,
		"egressSitePoolRegionalFailure": true, "egressSitePoolThinPlace": true,
		"sshAdmissionRequest": true, "ticketIdentity": true, "warpctlLogQueryError": true,
	}
	// observation error classes, not alert classes
	deniedFunctions := map[string]bool{"warpctlLogDiagnosticClass": true}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	files := []*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}

	text := func(expr ast.Expr) string {
		var builder strings.Builder
		printer.Fprint(&builder, fileSet, expr)
		return builder.String()
	}
	position := func(node ast.Node) string {
		p := fileSet.Position(node.Pos())
		return filepath.Base(p.Filename) + ":" + strconv.Itoa(p.Line)
	}
	stringValue := func(expr ast.Expr) (string, bool) {
		literal, ok := expr.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(literal.Value)
		return value, err == nil
	}
	keyValues := func(literal *ast.CompositeLit) map[string]ast.Expr {
		values := map[string]ast.Expr{}
		for _, element := range literal.Elts {
			if pair, ok := element.(*ast.KeyValueExpr); ok {
				if key, ok := pair.Key.(*ast.Ident); ok {
					values[key.Name] = pair.Value
				}
			}
		}
		return values
	}
	parameters := func(fields *ast.FieldList) map[string]int {
		nameParameters := map[string]int{}
		index := 0
		for _, field := range fields.List {
			for _, name := range field.Names {
				nameParameters[name.Name] = index
				index++
			}
			if len(field.Names) == 0 {
				index++
			}
		}
		return nameParameters
	}
	// name -> each value assigned to it, nil for one that cannot be followed
	collectValues := func(root ast.Node, nameValues map[string][]ast.Expr) {
		add := func(variable ast.Expr, value ast.Expr) {
			if ident, ok := variable.(*ast.Ident); ok && ident.Name != "_" {
				nameValues[ident.Name] = append(nameValues[ident.Name], value)
			}
		}
		ast.Inspect(root, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.ValueSpec:
				for i, name := range typed.Names {
					if len(typed.Values) == len(typed.Names) {
						add(name, typed.Values[i])
					} else if 0 < len(typed.Values) {
						add(name, nil)
					}
				}
			case *ast.AssignStmt:
				for i, left := range typed.Lhs {
					if len(typed.Lhs) == len(typed.Rhs) {
						add(left, typed.Rhs[i])
					} else {
						add(left, nil)
					}
				}
			case *ast.RangeStmt:
				list, _ := typed.X.(*ast.CompositeLit)
				if list == nil {
					add(typed.Key, nil)
					add(typed.Value, nil)
					break
				}
				for _, element := range list.Elts {
					if pair, ok := element.(*ast.KeyValueExpr); ok {
						add(typed.Key, pair.Key)
						add(typed.Value, pair.Value)
					} else {
						add(typed.Value, element)
					}
				}
			}
			return true
		})
	}
	packageNameValues := map[string][]ast.Expr{}
	for _, file := range files {
		for _, declaration := range file.Decls {
			if general, ok := declaration.(*ast.GenDecl); ok {
				collectValues(general, packageNameValues)
			}
		}
	}

	var resolve func(expr ast.Expr, frames []classFrame, nameValues map[string][]ast.Expr, depth int) classValue
	resolve = func(expr ast.Expr, frames []classFrame, nameValues map[string][]ast.Expr, depth int) classValue {
		if class, ok := stringValue(expr); ok {
			return classValue{classes: []string{class}}
		}
		ident, ok := expr.(*ast.Ident)
		if !ok || 8 < depth {
			return classValue{computed: true}
		}
		// a parameter shadows the function's other variables of its name
		for i := len(frames) - 1; 0 <= i; i-- {
			if index, ok := frames[i].nameParameters[ident.Name]; ok {
				return classValue{helpers: []string{frames[i].key + "#" + strconv.Itoa(index)}}
			}
		}
		values, local := nameValues[ident.Name]
		if !local {
			values = packageNameValues[ident.Name]
		}
		value := classValue{computed: len(values) == 0}
		for _, assigned := range values {
			if assigned == nil {
				value.computed = true
				continue
			}
			inner := resolve(assigned, frames, nameValues, depth+1)
			value.classes = append(value.classes, inner.classes...)
			value.helpers = append(value.helpers, inner.helpers...)
			value.computed = value.computed || inner.computed
		}
		return value
	}

	type classSite struct {
		site  string
		node  ast.Node
		expr  ast.Expr
		value classValue
		// set for a literal whose marker is checked
		marked *alertClassSite
	}
	type classCall struct {
		call       *ast.CallExpr
		frames     []classFrame
		nameValues map[string][]ast.Expr
		site       string
	}
	sites := []classSite{}
	calls := []classCall{}
	walk := func(root ast.Node, file string, function string, frames []classFrame, nameValues map[string][]ast.Expr) {
		site := file + " " + function
		stack := []ast.Node{}
		frameDepths := []int{}
		ast.Inspect(root, func(node ast.Node) bool {
			if node == nil {
				if 0 < len(frameDepths) && frameDepths[len(frameDepths)-1] == len(stack) {
					frameDepths = frameDepths[:len(frameDepths)-1]
					frames = frames[:len(frames)-1]
				}
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, node)
			switch typed := node.(type) {
			case *ast.FuncLit:
				key := function + ".func@" + position(typed)
				if assign, ok := stack[len(stack)-2].(*ast.AssignStmt); ok && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
					if ident, ok := assign.Lhs[0].(*ast.Ident); ok {
						key = function + "." + ident.Name
					}
				}
				frames = append(append([]classFrame{}, frames...), classFrame{key: key, nameParameters: parameters(typed.Type.Params)})
				frameDepths = append(frameDepths, len(stack))
			case *ast.CompositeLit:
				typeName := ""
				switch literalType := typed.Type.(type) {
				case *ast.Ident:
					typeName = literalType.Name
				case *ast.SelectorExpr:
					typeName = text(literalType)
				}
				if deniedTypes[typeName] || deniedFunctions[function] {
					break
				}
				values := keyValues(typed)
				for _, key := range []string{"class", "Class"} {
					expr, ok := values[key]
					if !ok {
						continue
					}
					marked := &alertClassSite{position: position(typed), alert: typeName == "Alert", unobservable: values["unobservable"]}
					marked.signalId, _ = stringValue(values["SignalID"])
					if probeId, ok := stringValue(values["probeId"]); ok {
						marked.signalId = probeId
					}
					if !marked.alert && marked.signalId != "monitor/visibility" {
						marked = nil
					}
					sites = append(sites, classSite{site: site, node: typed, expr: expr, value: resolve(expr, frames, nameValues, 0), marked: marked})
				}
				_, logClass := values["re"]
				_, logBurst := values["eventRe"]
				if expr, ok := values["name"]; ok && (logClass || logBurst) {
					sites = append(sites, classSite{site: site, node: typed, expr: expr, value: resolve(expr, frames, nameValues, 0)})
				}
			case *ast.AssignStmt:
				for i, left := range typed.Lhs {
					selector, ok := left.(*ast.SelectorExpr)
					if !ok || (selector.Sel.Name != "class" && selector.Sel.Name != "Class") {
						continue
					}
					if len(typed.Lhs) != len(typed.Rhs) {
						sites = append(sites, classSite{site: site, node: typed, expr: selector, value: classValue{computed: true}})
						continue
					}
					sites = append(sites, classSite{site: site, node: typed, expr: typed.Rhs[i], value: resolve(typed.Rhs[i], frames, nameValues, 0)})
				}
			case *ast.CallExpr:
				calls = append(calls, classCall{call: typed, frames: append([]classFrame{}, frames...), nameValues: nameValues, site: site})
			}
			return true
		})
	}
	for _, file := range files {
		name := filepath.Base(fileSet.Position(file.Pos()).Filename)
		for _, declaration := range file.Decls {
			switch typed := declaration.(type) {
			case *ast.GenDecl:
				walk(typed, name, "package", nil, packageNameValues)
			case *ast.FuncDecl:
				if typed.Body == nil {
					continue
				}
				nameValues := map[string][]ast.Expr{}
				collectValues(typed.Body, nameValues)
				frame := classFrame{key: typed.Name.Name, nameParameters: parameters(typed.Type.Params)}
				walk(typed.Body, name, typed.Name.Name, []classFrame{frame}, nameValues)
			}
		}
	}

	harvest := sourceAlertClasses{
		classPositions:     map[string][]string{},
		siteComputedValues: map[string][]string{},
		helpers:            map[string]bool{},
	}
	for _, site := range sites {
		for _, helper := range site.value.helpers {
			harvest.helpers[helper] = true
		}
	}
	// the helper parameter a call argument binds, or ""
	argumentHelper := func(call classCall, index int) string {
		suffix := "#" + strconv.Itoa(index)
		switch fun := call.call.Fun.(type) {
		case *ast.Ident:
			if 0 < len(call.frames) {
				if closure := call.frames[0].key + "." + fun.Name + suffix; harvest.helpers[closure] {
					return closure
				}
			}
			if harvest.helpers[fun.Name+suffix] {
				return fun.Name + suffix
			}
		case *ast.SelectorExpr:
			if harvest.helpers[fun.Sel.Name+suffix] {
				return fun.Sel.Name + suffix
			}
		}
		return ""
	}
	// a caller passing its own parameter to a helper is a helper too
	for grown := true; grown; {
		grown = false
		for _, call := range calls {
			for i, arg := range call.call.Args {
				if argumentHelper(call, i) == "" {
					continue
				}
				for _, helper := range resolve(arg, call.frames, call.nameValues, 0).helpers {
					if !harvest.helpers[helper] {
						harvest.helpers[helper] = true
						grown = true
					}
				}
			}
		}
	}
	// helper -> the classes and helpers its call sites pass
	helperValues := map[string]classValue{}
	for _, call := range calls {
		for i, arg := range call.call.Args {
			helper := argumentHelper(call, i)
			if helper == "" {
				continue
			}
			resolved := resolve(arg, call.frames, call.nameValues, 0)
			sites = append(sites, classSite{site: call.site, node: arg, expr: arg, value: resolved})
			value := helperValues[helper]
			value.classes = append(value.classes, resolved.classes...)
			value.helpers = append(value.helpers, resolved.helpers...)
			helperValues[helper] = value
		}
	}
	helperClasses := func(helper string) []string {
		seen := map[string]bool{}
		classes := []string{}
		var visit func(string)
		visit = func(helper string) {
			if seen[helper] {
				return
			}
			seen[helper] = true
			classes = append(classes, helperValues[helper].classes...)
			for _, inner := range helperValues[helper].helpers {
				visit(inner)
			}
		}
		visit(helper)
		return classes
	}
	for _, site := range sites {
		for _, class := range site.value.classes {
			harvest.classPositions[class] = append(harvest.classPositions[class], position(site.node))
		}
		if site.value.computed {
			harvest.siteComputedValues[site.site] = append(harvest.siteComputedValues[site.site], text(site.expr))
		}
		if site.marked != nil {
			site.marked.classes = append(site.marked.classes, site.value.classes...)
			for _, helper := range site.value.helpers {
				site.marked.classes = append(site.marked.classes, helperClasses(helper)...)
			}
			harvest.markedSites = append(harvest.markedSites, *site.marked)
		}
	}
	return harvest
}

// Every class the source names is in alertClassKinds, and every entry is a
// class the source names. A class value the source computes is acknowledged
// below with each class it can take. Unknown-state names are visibility, a
// literal of probe monitor/visibility has only visibility classes, and a
// direct Alert constructor, which bypasses alertFromFinding, marks its alert
// exactly when its class is visibility.
func TestEveryAlertClassIsClassified(t *testing.T) {
	// "file function" -> every class its computed class values can take
	siteComputedClasses := map[string][]string{
		"pg_query_sample_continuous.go pgSampleFindings":            {"pg-query-repeated-work", "pg-query-slow"},
		"registry.go visibilityAlert":                               {"provider-api", "provider-authentication", "provider-data-invalid"},
		"signal_credentials.go Run":                                 {"credential-fields-missing", "credential-resource-malformed", "credential-resource-missing"},
		"signal_edge_ipv6.go edgeIPv6Findings":                      {"edge-ipv6-http", "edge-ipv6-policy-route", "edge-ipv6-reset", "edge-ipv6-timeout", "edge-ipv6-upstream-drop", "edge-lb-config-rejected"},
		"signal_egress_coverage.go egressCoverageStallFinding":      {"egress-blackhole-stalled", "egress-full-stalled"},
		"signal_mimir_continuity.go mimirContinuityHealthyFindings": {"mimir-continuity-gap-unclassified", "mimir-ingestion-gap", "mimir-query-store-visibility-gap"},
		"signal_provider_count.go providerCountFinding":             {"provider-count-effective-empty", "provider-count-small-list", "provider-count-small-list-unclassified"},
		// literals elsewhere name these: the finding's own class, the
		// probeSchemaContract classes, the MinIO error patterns, and the log
		// class and burst names
		"signal.go alertFromFinding":                         nil,
		"schema_readiness.go probeSchemaUnavailableFinding":  nil,
		"signal_derived_locations.go check":                  nil,
		"signal_egress_site_pool.go check":                   nil,
		"signal_hmac_cutover.go check":                       nil,
		"signal_minio_storage.go evaluateMinioRequestErrors": nil,
		"tailer.go drainWindow":                              nil,
	}

	harvest := harvestAlertClasses(t)
	classPositions := harvest.classPositions
	sites := make([]string, 0, len(harvest.siteComputedValues))
	for site := range harvest.siteComputedValues {
		sites = append(sites, site)
	}
	sort.Strings(sites)
	for _, site := range sites {
		if _, ok := siteComputedClasses[site]; !ok {
			t.Errorf("%s computes a class from %s: name it with a literal, or acknowledge every class it can take", site, strings.Join(harvest.siteComputedValues[site], ", "))
		}
	}
	for site, classes := range siteComputedClasses {
		if _, ok := harvest.siteComputedValues[site]; !ok {
			t.Errorf("%s no longer computes a class: remove its acknowledgment", site)
		}
		for _, class := range classes {
			classPositions[class] = append(classPositions[class], site)
		}
	}

	classes := make([]string, 0, len(classPositions))
	for class := range classPositions {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	for _, class := range classes {
		if _, ok := alertClassKinds[class]; !ok {
			t.Errorf("class %s (%s) is unclassified: add it to alertClassKinds as visibilityAlertClass if it reports an unknown state in place of an incident, else as incidentAlertClass", class, strings.Join(classPositions[class], " "))
		}
	}
	for class, kind := range alertClassKinds {
		if _, ok := classPositions[class]; !ok {
			t.Errorf("alertClassKinds classifies %s, which the source does not name", class)
		}
		if (class == "cannot-observe" || strings.HasSuffix(class, "-unobservable")) && kind != visibilityAlertClass {
			t.Errorf("%s names an unknown state but is classified an incident", class)
		}
	}

	for _, site := range harvest.markedSites {
		visibility, incident := site.signalId == "monitor/visibility", []string{}
		for _, class := range site.classes {
			if isVisibilityAlertClass(class) {
				visibility = true
			} else {
				incident = append(incident, class)
			}
		}
		if site.signalId == "monitor/visibility" && 0 < len(incident) {
			t.Errorf("%s: monitor/visibility reports incident classes %v", site.position, incident)
		}
		if !site.alert {
			continue
		}
		marker, markerIsTrue := site.unobservable.(*ast.Ident)
		markerIsTrue = markerIsTrue && marker.Name == "true"
		if visibility && site.unobservable == nil {
			t.Errorf("%s: Alert %s of %v does not set unobservable", site.position, site.signalId, site.classes)
		}
		if !visibility && 0 < len(incident) && markerIsTrue {
			t.Errorf("%s: Alert %s of incident classes %v sets unobservable", site.position, site.signalId, incident)
		}
	}

	if len(classPositions) < 400 || len(harvest.markedSites) < 15 || len(harvest.helpers) < 15 {
		t.Fatalf("harvest too small to be the package: %d classes, %d marked literals, %d class helpers", len(classPositions), len(harvest.markedSites), len(harvest.helpers))
	}
}

// alertFromFinding marks a finding's alert by its class: every visibility
// class and an unclassified one, never an incident class. The probe
// monitor/visibility and a finding that sets unobservable mark it too.
func TestAlertFromFindingMarksUnknownStatesByClass(t *testing.T) {
	settings := syntheticSettings(nil)
	marked := func(observed finding) bool {
		return alertFromFinding(settings, "0", "inventory", "Inventory", observed).unobservable
	}
	for class, kind := range alertClassKinds {
		visibility := kind == visibilityAlertClass
		if marked(finding{probeId: "inventory/probe", tier: tierWarn, class: class, target: "target"}) != visibility {
			t.Errorf("class %s: visibility %t, but its alert is marked %t", class, visibility, !visibility)
		}
	}
	if !marked(finding{probeId: "inventory/probe", tier: tierWarn, class: "synthetic-unclassified-state", target: "target"}) {
		t.Error("an unclassified class does not mark its alert")
	}
	if !marked(finding{probeId: "monitor/visibility", tier: tierWarn, class: "active-pileup", target: "target"}) {
		t.Error("probe monitor/visibility does not mark its alert")
	}
	if !marked(finding{probeId: "inventory/probe", tier: tierWarn, class: "active-pileup", target: "target", unobservable: true}) {
		t.Error("a finding that sets unobservable does not mark its alert")
	}
}
