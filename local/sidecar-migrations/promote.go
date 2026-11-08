package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The two files a promotion edits, in the server directory.
const (
	pendingFileName    = "db_migrations_sidecar_pending.go"
	migrationsFileName = "db_migrations.go"
)

// The function that returns a group, and those that return its owners' parts.
var (
	groupFunctionPattern = regexp.MustCompile(`^sidecarMg([0-9])Migrations$`)
	partFunctionPattern  = regexp.MustCompile(`^sidecarMg([0-9])[A-Z][A-Za-z0-9]*Migrations$`)
	groupNamePattern     = regexp.MustCompile(`^MG([0-9])$`)
)

// What a promotion did: the group now holds versions firstVersion to lastVersion,
// which never change again.
type promoteResult struct {
	groupName    string
	firstVersion int
	lastVersion  int
}

// One parsed Go file, with its bytes, so edits are made by offset and keep every
// raw string's bytes, and with them each migration's identity.
type goSource struct {
	path    string
	data    []byte
	fileSet *token.FileSet
	syntax  *ast.File
}

func parseGoSource(path string, data []byte) (*goSource, error) {
	fileSet := token.NewFileSet()
	syntax, err := parser.ParseFile(fileSet, path, data, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	return &goSource{path: path, data: data, fileSet: fileSet, syntax: syntax}, nil
}

func (self *goSource) offset(position token.Pos) int {
	return self.fileSet.Position(position).Offset
}

// The text from one position to another.
func (self *goSource) text(from token.Pos, to token.Pos) string {
	return string(self.data[self.offset(from):self.offset(to)])
}

// The top-level functions, by name.
func (self *goSource) functions() map[string]*ast.FuncDecl {
	functions := map[string]*ast.FuncDecl{}
	for _, decl := range self.syntax.Decls {
		if function, ok := decl.(*ast.FuncDecl); ok && function.Recv == nil {
			functions[function.Name.Name] = function
		}
	}
	return functions
}

// The one expression a function returns, when its body is a single return.
func returnedExpression(function *ast.FuncDecl) (ast.Expr, error) {
	if function.Body == nil || len(function.Body.List) != 1 {
		return nil, fmt.Errorf("%s is not a single return statement", function.Name.Name)
	}
	statement, ok := function.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(statement.Results) != 1 {
		return nil, fmt.Errorf("%s is not a single return statement", function.Name.Name)
	}
	return statement.Results[0], nil
}

// Whether expression is a []any composite literal.
func anySliceLiteral(expression ast.Expr) (*ast.CompositeLit, bool) {
	literal, ok := expression.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	arrayType, ok := literal.Type.(*ast.ArrayType)
	if !ok || arrayType.Len != nil {
		return nil, false
	}
	elementType, ok := arrayType.Elt.(*ast.Ident)
	if !ok || elementType.Name != "any" {
		return nil, false
	}
	return literal, true
}

// Whether call is slices.Concat(...).
func slicesConcatCall(expression ast.Expr) (*ast.CallExpr, bool) {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return nil, false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	packageName, ok := selector.X.(*ast.Ident)
	return call, ok && packageName.Name == "slices" && selector.Sel.Name == "Concat"
}

// A group's entries as they stand in the pending file: the literals its function
// concatenates, in order, and the part functions it calls.
type pendingGroup struct {
	number        int
	function      *ast.FuncDecl
	partFunctions []*ast.FuncDecl
	literals      []*ast.CompositeLit
}

// Reads group number's function and its parts. A group is a []any literal, or
// slices.Concat of its part functions' calls and []any literals, each part
// function a single return of a []any literal: the shape the pending file's
// header asks for.
func readPendingGroup(source *goSource, number int) (*pendingGroup, error) {
	functions := source.functions()
	name := fmt.Sprintf("sidecarMg%dMigrations", number)
	function, ok := functions[name]
	if !ok {
		return nil, fmt.Errorf("MG%d is not pending: %s has no %s", number, filepath.Base(source.path), name)
	}
	group := &pendingGroup{number: number, function: function}
	expression, err := returnedExpression(function)
	if err != nil {
		return nil, err
	}
	addLiteral := func(expression ast.Expr) error {
		literal, ok := anySliceLiteral(expression)
		if !ok {
			return fmt.Errorf("%s returns something other than a []any literal", name)
		}
		group.literals = append(group.literals, literal)
		return nil
	}
	if call, ok := slicesConcatCall(expression); ok {
		for _, argument := range call.Args {
			partCall, isCall := argument.(*ast.CallExpr)
			if !isCall {
				if err := addLiteral(argument); err != nil {
					return nil, err
				}
				continue
			}
			partName, isIdent := partCall.Fun.(*ast.Ident)
			if !isIdent || len(partCall.Args) != 0 {
				return nil, fmt.Errorf("%s concatenates a call other than one of its parts", name)
			}
			match := partFunctionPattern.FindStringSubmatch(partName.Name)
			if match == nil || match[1] != strconv.Itoa(number) {
				return nil, fmt.Errorf("%s concatenates %s, which is not one of MG%d's parts", name, partName.Name, number)
			}
			partFunction, ok := functions[partName.Name]
			if !ok {
				return nil, fmt.Errorf("%s calls %s, which the pending file does not declare", name, partName.Name)
			}
			partExpression, err := returnedExpression(partFunction)
			if err != nil {
				return nil, err
			}
			partLiteral, ok := anySliceLiteral(partExpression)
			if !ok {
				return nil, fmt.Errorf("%s returns something other than a []any literal", partName.Name)
			}
			group.partFunctions = append(group.partFunctions, partFunction)
			group.literals = append(group.literals, partLiteral)
		}
	} else if err := addLiteral(expression); err != nil {
		return nil, err
	}
	return group, nil
}

// The group's entries as source text, comments and raw strings byte for byte, one
// literal after another. A literal written on one line gains the comma a list
// needs.
func (self *pendingGroup) entriesText(source *goSource) string {
	texts := []string{}
	for _, literal := range self.literals {
		if len(literal.Elts) == 0 {
			continue
		}
		text := strings.Trim(source.text(literal.Lbrace+1, literal.Rbrace), " \t\n")
		if !strings.HasSuffix(text, ",") {
			text += ","
		}
		texts = append(texts, text)
	}
	return strings.Join(texts, "\n\n")
}

// The names the group's entries use that the pending file alone declares. Moved
// into db_migrations.go they would not compile on a release leg, so a group must
// not use any.
func (self *pendingGroup) pendingOnlyNames(source *goSource) []string {
	declared := map[string]bool{}
	movedFunctions := map[*ast.FuncDecl]bool{self.function: true}
	for _, partFunction := range self.partFunctions {
		movedFunctions[partFunction] = true
	}
	for _, decl := range source.syntax.Decls {
		switch value := decl.(type) {
		case *ast.FuncDecl:
			if value.Recv == nil && !movedFunctions[value] {
				declared[value.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, spec := range value.Specs {
				switch spec := spec.(type) {
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						declared[name.Name] = true
					}
				case *ast.TypeSpec:
					declared[spec.Name.Name] = true
				}
			}
		}
	}
	used := map[string]bool{}
	for _, literal := range self.literals {
		ast.Inspect(literal, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok && declared[ident.Name] {
				used[ident.Name] = true
			}
			return true
		})
	}
	names := []string{}
	for name := range used {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// The migrations slice's literal: the first argument of `var migrations =
// append([]any{...}, sidecarPendingMigrations()...)`.
func migrationsLiteral(source *goSource) (*ast.CompositeLit, error) {
	for _, decl := range source.syntax.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.VAR {
			continue
		}
		for _, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok || len(valueSpec.Names) != 1 || valueSpec.Names[0].Name != "migrations" || len(valueSpec.Values) != 1 {
				continue
			}
			call, ok := valueSpec.Values[0].(*ast.CallExpr)
			if !ok || len(call.Args) != 2 || !call.Ellipsis.IsValid() {
				return nil, errors.New("the migrations slice is not append([]any{...}, sidecarPendingMigrations()...)")
			}
			if fun, ok := call.Fun.(*ast.Ident); !ok || fun.Name != "append" {
				return nil, errors.New("the migrations slice is not built by append")
			}
			if tail, ok := call.Args[1].(*ast.CallExpr); !ok || !isIdentNamed(tail.Fun, "sidecarPendingMigrations") {
				return nil, errors.New("the migrations slice does not end with sidecarPendingMigrations()...")
			}
			literal, ok := anySliceLiteral(call.Args[0])
			if !ok {
				return nil, errors.New("the migrations slice does not start with a []any literal")
			}
			return literal, nil
		}
	}
	return nil, fmt.Errorf("%s declares no migrations slice", filepath.Base(source.path))
}

func isIdentNamed(expression ast.Expr, name string) bool {
	ident, ok := expression.(*ast.Ident)
	return ok && ident.Name == name
}

// A byte range to cut from a file, and what takes its place.
type cutRange struct {
	from        int
	to          int
	replacement string
}

// Applies cuts from the end, so each range's offsets hold as earlier ones apply.
func applyCuts(data []byte, cuts []cutRange) []byte {
	slices.SortFunc(cuts, func(a cutRange, b cutRange) int { return b.from - a.from })
	out := slices.Clone(data)
	for _, cut := range cuts {
		out = slices.Concat(out[:cut.from], []byte(cut.replacement), out[cut.to:])
	}
	return out
}

// A declaration's range with its doc comment and the line it ends.
func (self *goSource) declarationRange(function *ast.FuncDecl) cutRange {
	from := self.offset(function.Pos())
	if function.Doc != nil {
		from = self.offset(function.Doc.Pos())
	}
	to := self.offset(function.End())
	for to < len(self.data) && self.data[to] == '\n' {
		to += 1
	}
	return cutRange{from: from, to: to}
}

// Drops imports nothing in the file uses any more, as the last group's
// promotion leaves context unused.
func dropUnusedImports(path string, data []byte) ([]byte, error) {
	source, err := parseGoSource(path, data)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	ast.Inspect(source.syntax, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			if ident, ok := selector.X.(*ast.Ident); ok {
				used[ident.Name] = true
			}
		}
		return true
	})
	cuts := []cutRange{}
	for _, spec := range source.syntax.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, err
		}
		name := filepath.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "_" || used[name] {
			continue
		}
		from := source.offset(spec.Pos())
		to := source.offset(spec.End())
		// the whole line, its indent included
		for 0 < from && (data[from-1] == '\t' || data[from-1] == ' ') {
			from -= 1
		}
		if to < len(data) && data[to] == '\n' {
			to += 1
		}
		cuts = append(cuts, cutRange{from: from, to: to})
	}
	return format.Source(applyCuts(data, cuts))
}

// Moves group groupName from the pending file into the migrations slice. Nothing
// is written unless every check passes and both results parse.
func promote(serverDirectory string, groupName string) (*promoteResult, error) {
	match := groupNamePattern.FindStringSubmatch(groupName)
	if match == nil {
		return nil, fmt.Errorf("%q names no group: give MG0, MG1, MG2 or MG5 to MG9", groupName)
	}
	number, _ := strconv.Atoi(match[1])
	if number == 3 || number == 4 {
		return nil, fmt.Errorf("MG%d creates nothing (§19.7), so there is nothing to promote", number)
	}

	pendingPath := filepath.Join(serverDirectory, pendingFileName)
	migrationsPath := filepath.Join(serverDirectory, migrationsFileName)
	pendingData, err := os.ReadFile(pendingPath)
	if err != nil {
		return nil, err
	}
	migrationsData, err := os.ReadFile(migrationsPath)
	if err != nil {
		return nil, err
	}
	pending, err := parseGoSource(pendingPath, pendingData)
	if err != nil {
		return nil, err
	}
	main, err := parseGoSource(migrationsPath, migrationsData)
	if err != nil {
		return nil, err
	}

	// groups register in order, so an earlier group still pending stops this one
	earliest := number
	for name := range pending.functions() {
		if groupMatch := groupFunctionPattern.FindStringSubmatch(name); groupMatch != nil {
			if earlier, _ := strconv.Atoi(groupMatch[1]); earlier < earliest {
				earliest = earlier
			}
		}
	}
	if earliest < number {
		return nil, fmt.Errorf("MG%d is still pending: promote it first, at its own Gn-out", earliest)
	}
	group, err := readPendingGroup(pending, number)
	if err != nil {
		return nil, err
	}
	if names := group.pendingOnlyNames(pending); 0 < len(names) {
		return nil, fmt.Errorf("MG%d's entries use %s, which only the pending file declares", number, strings.Join(names, ", "))
	}
	entries := group.entriesText(pending)
	markerId := fmt.Sprintf("sidecarMg%dMigrationId", number)
	if !strings.Contains(entries, "newCodeMigration("+markerId) {
		return nil, fmt.Errorf("MG%d does not end with its marker, newCodeMigration(%s, ...)", number, markerId)
	}

	// the pending side: the group's functions go, and so does its call in
	// sidecarPendingMigrations
	cuts := []cutRange{pending.declarationRange(group.function)}
	for _, partFunction := range group.partFunctions {
		cuts = append(cuts, pending.declarationRange(partFunction))
	}
	pendingFunction, ok := pending.functions()["sidecarPendingMigrations"]
	if !ok {
		return nil, errors.New("the pending file declares no sidecarPendingMigrations")
	}
	removedCall := false
	ast.Inspect(pendingFunction, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || removedCall {
			return !removedCall
		}
		for index, argument := range call.Args {
			argumentCall, ok := argument.(*ast.CallExpr)
			if !ok || !isIdentNamed(argumentCall.Fun, group.function.Name.Name) {
				continue
			}
			cut := cutRange{from: pending.offset(argument.Pos()), to: pending.offset(argument.End())}
			if index+1 < len(call.Args) {
				cut.to = pending.offset(call.Args[index+1].Pos())
			} else if 0 < index {
				cut.from = pending.offset(call.Args[index-1].End())
			} else {
				// the last group: slices.Concat cannot infer a type from no argument
				cut = cutRange{from: pending.offset(call.Pos()), to: pending.offset(call.End()), replacement: "nil"}
			}
			cuts = append(cuts, cut)
			removedCall = true
			return false
		}
		return true
	})
	if !removedCall {
		return nil, fmt.Errorf("sidecarPendingMigrations does not call %s", group.function.Name.Name)
	}
	newPending, err := format.Source(applyCuts(pending.data, cuts))
	if err != nil {
		return nil, fmt.Errorf("the pending file would not parse: %w", err)
	}
	newPending, err = dropUnusedImports(pendingPath, newPending)
	if err != nil {
		return nil, fmt.Errorf("the pending file would not parse: %w", err)
	}

	// the main side: the entries go at the literal's end, before the pending tail
	literal, err := migrationsLiteral(main)
	if err != nil {
		return nil, err
	}
	before := len(literal.Elts)
	insertAt := main.offset(literal.Rbrace)
	insertion := fmt.Sprintf("\n\t// MG%d (§19.7), registered by its Gn-out with sidecar-migrations promote:\n\t// its versions never change.\n\n%s\n", number, entries)
	var newMain bytes.Buffer
	newMain.Write(main.data[:insertAt])
	// the literal's last entry ends its line; the insertion starts a new one
	newMain.WriteString(insertion)
	newMain.Write(main.data[insertAt:])
	formattedMain, err := format.Source(newMain.Bytes())
	if err != nil {
		return nil, fmt.Errorf("db_migrations.go would not parse: %w", err)
	}
	promoted, err := parseGoSource(migrationsPath, formattedMain)
	if err != nil {
		return nil, err
	}
	promotedLiteral, err := migrationsLiteral(promoted)
	if err != nil {
		return nil, err
	}
	after := len(promotedLiteral.Elts)
	if after <= before {
		return nil, fmt.Errorf("MG%d holds no entry", number)
	}

	// pending first: a crash between the writes leaves the group in neither file,
	// which no build can mistake for registered, rather than in both
	if err := writeFileAtomically(pendingPath, newPending); err != nil {
		return nil, err
	}
	if err := writeFileAtomically(migrationsPath, formattedMain); err != nil {
		return nil, err
	}
	return &promoteResult{groupName: groupName, firstVersion: before + 1, lastVersion: after}, nil
}

// Writes beside the file and renames over it, keeping its mode.
func writeFileAtomically(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".promote-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
