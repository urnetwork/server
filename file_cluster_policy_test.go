package server

// What server keeps of D-OPS-4 and of each block's MinIO user (OPS-M2-04). The clusters are LAN-only,
// unshared and without presigned URLs, so no server code calls a Presigned* method. Each block reaches
// the clusters through a user of its own, whose policy xops renders from
// templates/file-clusters/policy-<block>.json.j2; file_cluster_policy_localstack_test.go applies those
// templates to the local stack's MinIO through renderFileClusterPolicyTemplate below, which covers the
// part of Jinja2 they use, so the policies are shown on a real store without Ansible.

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The server module's root: the package's own directory, where go test runs it. The source path the
// runtime reports is a module path under -trimpath, so it names no directory.
func fileClusterPolicyServerRoot(t *testing.T) string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "file_cluster.go")); err != nil {
		t.Fatalf("not run from the server package's directory: %v", err)
	}
	return root
}

// Every call of a Presign* method in the non-test Go source under root, as file:line, and how many files
// were read. Only files that mention Presign are parsed, and only selector calls count, so a comment naming
// one does not.
func fileClusterPresignedCalls(root string) ([]string, int, error) {
	calls := []string{}
	fileCount := 0
	fileSet := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "testdata", "vendor", "node_modules", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fileCount += 1
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(data, []byte("Presign")) {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, data, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && strings.HasPrefix(selector.Sel.Name, "Presign") {
					position := fileSet.Position(selector.Pos())
					relativePath, _ := filepath.Rel(root, position.Filename)
					calls = append(calls, fmt.Sprintf("%s:%d %s", relativePath, position.Line, selector.Sel.Name))
				}
			}
			return true
		})
		return nil
	})
	return calls, fileCount, err
}

// D-OPS-4: no presigned URL reaches anyone, since direct access would expose client addresses to the
// clusters and skip the conditional writes that order evs and gkm.
func TestServerCallsNoPresignedMethod(t *testing.T) {
	root := fileClusterPolicyServerRoot(t)
	calls, fileCount, err := fileClusterPresignedCalls(root)
	if err != nil {
		t.Fatal(err)
	}
	if fileCount < 100 {
		t.Fatalf("the scan read %d files under %s, so it read almost nothing", fileCount, root)
	}
	for _, call := range calls {
		t.Errorf("a presigned URL in server (D-OPS-4): %s", call)
	}
}

// The scan finds a planted call, however it is reached, and passes a comment that only names one.
func TestPresignedScanFindsAPlantedCall(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"store.go":      "package store\n\nfunc share(client minioClient) {\n\tclient.PresignedGetObject(nil, \"bucket\", \"key\", 0, nil)\n}\n",
		"nested/put.go": "package nested\n\nfunc put(core minioCore) {\n\t_, _ = core.Client.PresignedPutObject(nil, \"b\", \"k\", 0)\n}\n",
		"comment.go":    "package store\n\n// no PresignedGetObject here, only its name\nfunc none() {}\n",
		"store_test.go": "package store\n\nfunc testShare(client minioClient) { client.PresignedGetObject(nil, \"b\", \"k\", 0, nil) }\n",
	}
	for name, text := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	calls, fileCount, err := fileClusterPresignedCalls(root)
	if err != nil {
		t.Fatal(err)
	}
	if fileCount != 3 {
		t.Errorf("read %d files, want the 3 that are not tests", fileCount)
	}
	if strings.Join(calls, "\n") != "nested/put.go:4 PresignedPutObject\nstore.go:4 PresignedGetObject" {
		t.Errorf("calls = %q", calls)
	}
}

var (
	fileClusterTemplateCommentRegex = regexp.MustCompile(`(?s)\{#.*?#\}\n?`)
	fileClusterTemplateTagRegex     = regexp.MustCompile(`(?s)\{%\s*(.*?)\s*%\}\n?|\{\{\s*(.*?)\s*\}\}`)
	fileClusterTemplatePathRegex    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	fileClusterTemplateCompareRegex = regexp.MustCompile(`^(\S+)\s*(==|!=)\s*("[^"]*"|'[^']*')$`)
)

// Renders a policy template with vars, as Ansible's template module does (StrictUndefined, trim_blocks),
// for the part of Jinja2 the policy templates use: {# comments #}, {{ a.b }} and {% if %}, {% else %} and
// {% endif %}, whose condition is a path, `not` a path, or a path compared by == or != with a quoted string,
// joined by `and` and `or` without parentheses. Anything else is an error, so a template that grows past
// this subset fails here rather than rendering wrong.
func renderFileClusterPolicyTemplate(text string, vars map[string]any) (string, error) {
	lookup := func(path string) (any, error) {
		if !fileClusterTemplatePathRegex.MatchString(path) {
			return nil, fmt.Errorf("%q is not a path this renderer reads", path)
		}
		var value any = vars
		for _, name := range strings.Split(path, ".") {
			mapping, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%q: %s is not a mapping", path, name)
			}
			if value, ok = mapping[name]; !ok {
				return nil, fmt.Errorf("%q is undefined", path)
			}
		}
		return value, nil
	}
	truth := func(value any) bool {
		switch v := value.(type) {
		case bool:
			return v
		case string:
			return v != ""
		case int:
			return v != 0
		case nil:
			return false
		}
		return true
	}
	evaluateAtom := func(atom string) (bool, error) {
		atom = strings.TrimSpace(atom)
		if match := fileClusterTemplateCompareRegex.FindStringSubmatch(atom); match != nil {
			value, err := lookup(match[1])
			if err != nil {
				return false, err
			}
			literal := match[3][1 : len(match[3])-1]
			equal := fmt.Sprint(value) == literal
			if _, isString := value.(string); !isString {
				equal = false
			}
			return equal == (match[2] == "=="), nil
		}
		negate := false
		if rest, ok := strings.CutPrefix(atom, "not "); ok {
			negate = true
			atom = strings.TrimSpace(rest)
		}
		value, err := lookup(atom)
		if err != nil {
			return false, err
		}
		return truth(value) != negate, nil
	}
	evaluate := func(condition string) (bool, error) {
		for _, disjunct := range strings.Split(condition, " or ") {
			all := true
			for _, conjunct := range strings.Split(disjunct, " and ") {
				value, err := evaluateAtom(conjunct)
				if err != nil {
					return false, err
				}
				all = all && value
			}
			if all {
				return true, nil
			}
		}
		return false, nil
	}

	text = fileClusterTemplateCommentRegex.ReplaceAllString(text, "")
	var out strings.Builder
	// one entry per open if: whether its branch now taken renders, and whether the if's own
	// condition held
	type frame struct {
		rendering bool
		held      bool
		inElse    bool
	}
	frames := []*frame{}
	rendering := func() bool {
		for _, f := range frames {
			if !f.rendering {
				return false
			}
		}
		return true
	}
	offset := 0
	for _, match := range fileClusterTemplateTagRegex.FindAllStringSubmatchIndex(text, -1) {
		if rendering() {
			out.WriteString(text[offset:match[0]])
		}
		offset = match[1]
		if match[2] < 0 {
			if !rendering() {
				continue
			}
			value, err := lookup(text[match[4]:match[5]])
			if err != nil {
				return "", err
			}
			switch value.(type) {
			case string, int:
				out.WriteString(fmt.Sprint(value))
			default:
				return "", fmt.Errorf("{{ %s }} is not a string or a number", text[match[4]:match[5]])
			}
			continue
		}
		statement := text[match[2]:match[3]]
		switch {
		case strings.HasPrefix(statement, "if "):
			held, err := evaluate(strings.TrimPrefix(statement, "if "))
			if err != nil {
				return "", err
			}
			frames = append(frames, &frame{rendering: held, held: held})
		case statement == "else":
			if len(frames) == 0 || frames[len(frames)-1].inElse {
				return "", fmt.Errorf("an else outside an if")
			}
			top := frames[len(frames)-1]
			top.inElse = true
			top.rendering = !top.held
		case statement == "endif":
			if len(frames) == 0 {
				return "", fmt.Errorf("an endif outside an if")
			}
			frames = frames[:len(frames)-1]
		default:
			return "", fmt.Errorf("{%% %s %%} is not a tag this renderer reads", statement)
		}
	}
	if 0 < len(frames) {
		return "", fmt.Errorf("an if with no endif")
	}
	out.WriteString(text[offset:])
	return out.String(), nil
}

// The renderer agrees with Jinja2 on the subset it reads, and refuses what it does not.
func TestFileClusterPolicyRendererReadsItsSubset(t *testing.T) {
	vars := map[string]any{
		"file_cluster": map[string]any{"bucket": "meta-1", "kind": "metadata", "first": true},
		"switch":       false,
	}
	template := "{# a comment\n spanning lines #}\n[\n{% if file_cluster.kind == \"segment\" or switch %}\n  \"seg\",\n{% else %}\n  \"meta\",\n{% endif %}\n{% if not switch and file_cluster.first %}\n  \"first\",\n{% endif %}\n  \"{{ file_cluster.bucket }}\"\n]\n"
	rendered, err := renderFileClusterPolicyTemplate(template, vars)
	if err != nil {
		t.Fatal(err)
	}
	if want := "[\n  \"meta\",\n  \"first\",\n  \"meta-1\"\n]\n"; rendered != want {
		t.Errorf("rendered %q, want %q", rendered, want)
	}
	for _, refused := range []string{
		"{{ missing.name }}",
		"{{ file_cluster.bucket | upper }}",
		"{% for x in y %}{% endfor %}",
		"{% if file_cluster.kind == segment %}{% endif %}",
		"{% if switch %}",
		"{% endif %}",
		"{{ switch }}",
	} {
		if _, err := renderFileClusterPolicyTemplate(refused, vars); err == nil {
			t.Errorf("%q rendered", refused)
		}
	}
}
