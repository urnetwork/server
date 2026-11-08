// CODESTYLE's comment rules over every Go file of this module, tests included: a comment at the top
// of each file, and a doc comment on each function, method and type that does not open by
// repeating its name. The root's style_test.go holds the root to the same; this module is outside
// it.

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What one parsed file leaves undocumented, as findings.
func sidecarctlCommentFindings(fileName string, file *ast.File) []string {
	findings := []string{}
	if len(file.Comments) == 0 || file.Package < file.Comments[0].Pos() {
		findings = append(findings, fileName+" has no comment at its top")
	}
	check := func(name string, doc *ast.CommentGroup) {
		if doc == nil {
			findings = append(findings, fileName+": "+name+" has no doc comment")
		} else if strings.HasPrefix(doc.Text(), name+" ") {
			findings = append(findings, fileName+": "+name+"'s comment repeats its name")
		}
	}
	for _, decl := range file.Decls {
		switch declaration := decl.(type) {
		case *ast.FuncDecl:
			check(declaration.Name.Name, declaration.Doc)
		case *ast.GenDecl:
			if declaration.Tok != token.TYPE {
				continue
			}
			for _, spec := range declaration.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				if typeSpec.Doc != nil {
					check(typeSpec.Name.Name, typeSpec.Doc)
				} else {
					check(typeSpec.Name.Name, declaration.Doc)
				}
			}
		}
	}
	return findings
}

// The rule over this directory's own files.
func TestEverySidecarctlDeclarationHasADocComment(t *testing.T) {
	fileNames, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(fileNames) == 0 {
		t.Fatal("found no Go files, so this read nothing")
	}
	for _, fileName := range fileNames {
		source, err := os.ReadFile(fileName)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), fileName, source, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range sidecarctlCommentFindings(fileName, file) {
			t.Error(finding)
		}
	}
}

// The rule finds each kind of gap it names, so the pass above means something.
func TestSidecarctlCommentRuleCatchesPlantedGaps(t *testing.T) {
	cases := []struct {
		source string
		want   string
	}{
		{source: "package main\n\n// A thing.\nfunc thing() {}\n", want: "has no comment at its top"},
		{source: "// A file.\n\npackage main\n\nfunc thing() {}\n", want: "thing has no doc comment"},
		{source: "// A file.\n\npackage main\n\n// thing does it.\nfunc thing() {}\n", want: "thing's comment repeats its name"},
		{source: "// A file.\n\npackage main\n\ntype holder struct{}\n", want: "holder has no doc comment"},
		{source: "// A file.\n\npackage main\n\n// A holder.\ntype holder struct{}\n\nfunc (self *holder) get() {}\n", want: "get has no doc comment"},
	}
	for _, c := range cases {
		file, err := parser.ParseFile(token.NewFileSet(), "planted.go", c.source, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		findings := sidecarctlCommentFindings("planted.go", file)
		if len(findings) != 1 || !strings.Contains(findings[0], c.want) {
			t.Errorf("planted %q: findings %v, want one with %q", c.source, findings, c.want)
		}
	}
}
