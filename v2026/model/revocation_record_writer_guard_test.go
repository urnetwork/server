// A source guard for the Redis revocation authority (owner decision,
// 2026-10-26). A positive projection outlives a revocation that does not
// write the record, so every SQL statement that ends a credential's state must
// sit in a function that writes the record before its commit, or name why it
// cannot end a live credential.
package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Statements that end what the state query reads: an active client, a client
// row, a network row, or a credential epoch.
var revokingStatementPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?is)\bupdate\s+network_client\b.*\bset\b.*\bactive\s*=\s*false\b`),
	regexp.MustCompile(`(?is)\bdelete\s+from\s+network_client\b`),
	regexp.MustCompile(`(?is)\bdelete\s+from\s+network(\s|$)`),
	regexp.MustCompile(`(?is)\bupdate\s+network_user\b.*\bset\b.*\bcredential_change_time\s*=`),
}

// file:function (or file:const) -> the record writes it must call, or the
// reason it ends no live credential.
var revokingStatementOwners = map[string]struct {
	hooks  []string
	reason string
}{
	"model/network_client_model.go:deactivateLockedNetworkClientsInTx": {hooks: []string{"RevokeClientRecordsInTx"}},
	// the abandoned top-level mark and the stale child reap end active
	// clients; its inactive reap deletes only rows already ended
	"model/network_client_model.go:RemoveDisconnectedNetworkClients":              {hooks: []string{"RevokeNetworkClientRecordsInTx", "RevokeRetiredClientRecordsInTx"}},
	"model/network_client_retired_hosted.go:removeRetiredHostedNetworkClientsSql": {reason: "deletes only rows already inactive (NOT locked.active)"},
	"model/prober_shard_model.go:ReapProberShard":                                 {hooks: []string{"RetireNetworkRecords"}},
	"model/account_model.go:RemoveNetworkWithStoreSnapshot":                       {hooks: []string{"RetireNetworkRecords"}},
	"model/lag_guest_model.go:removeExpiredLagGuest":                              {hooks: []string{"RetireNetworkRecords"}},
	"model/auth_model.go:AuthPasswordSet":                                         {hooks: []string{"RotateCredentialRecordsInTx"}},
}

func TestEveryRevokingStatementWritesTheRevocationRecord(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			// other modules and generated trees carry no server statements
			if relative != "." {
				if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "vendor" || entry.Name() == "node_modules" {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		// schema and one-time code migrations predate every live credential
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasPrefix(entry.Name(), "db_migrations") {
			return nil
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		check := func(owner string, body ast.Node, literal *ast.BasicLit) {
			text, err := strconv.Unquote(literal.Value)
			if err != nil {
				return
			}
			for _, pattern := range revokingStatementPatterns {
				if !pattern.MatchString(text) {
					continue
				}
				key := relative + ":" + owner
				found[key] = true
				rule, ok := revokingStatementOwners[key]
				if !ok {
					t.Errorf("%s: a statement that ends a credential's state (%q) writes no revocation record; call the session record write before its commit and add it to revokingStatementOwners", fileSet.Position(literal.Pos()), pattern.String())
					return
				}
				for _, hook := range rule.hooks {
					if body == nil || !callsFunction(body, hook) {
						t.Errorf("%s: %s must call session.%s before its commit", fileSet.Position(literal.Pos()), key, hook)
					}
				}
				return
			}
		}
		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				ast.Inspect(declaration, func(node ast.Node) bool {
					if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
						check(declaration.Name.Name, declaration, literal)
					}
					return true
				})
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, expression := range value.Values {
						ast.Inspect(expression, func(node ast.Node) bool {
							if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING && i < len(value.Names) {
								check(value.Names[i].Name, nil, literal)
							}
							return true
						})
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for key, rule := range revokingStatementOwners {
		if !found[key] {
			t.Errorf("%s no longer holds a revoking statement; remove it from revokingStatementOwners", key)
		}
		if len(rule.hooks) == 0 && rule.reason == "" {
			t.Errorf("%s names neither a record write nor a reason", key)
		}
	}
}

// Whether body calls name, qualified by any package or not.
func callsFunction(body ast.Node, name string) bool {
	called := false
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return !called
		}
		switch function := call.Fun.(type) {
		case *ast.SelectorExpr:
			called = called || function.Sel.Name == name
		case *ast.Ident:
			called = called || function.Name == name
		}
		return !called
	})
	return called
}
