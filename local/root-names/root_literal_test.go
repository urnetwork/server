// Package rootnames keeps the user root out of code and out of the connect
// program's tls names (sidecar DESIGN §12.4, D-DNS-1, R-CTX-20; DNS-M5-20).
//
// A root is configuration, an environment's config/<env>/dns.yml roots, and
// the platform's names live on bringyour.com. So a Go string literal in
// server, connect or sidecar that names a root is either a platform name left
// under it or a root written into code that should read it from config.
// TestRootLiteralsStayOutOfCode fails on one in any file that
// root-literal-allowlist.yml does not list with why, and on a listed file that
// holds none any more. Comments are prose and may name a root.
//
// The tests read the sibling checkouts server's go.mod already requires
// beside it, ../connect and ../sidecar, and ../config for the roots, as other
// server tests do. They need no store and no network.
package rootnames

import (
	"bytes"
	"fmt"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The allowlist, beside this file.
const rootLiteralAllowlistPath = "root-literal-allowlist.yml"

// The sibling config checkout, from this package's directory.
const rootConfigDir = "../../../config"

// The repositories the lint reads, by the name the allowlist's paths begin
// with, from this package's directory.
var rootLiteralRepos = []rootLiteralRepo{
	{name: "server", dir: "../.."},
	{name: "connect", dir: "../../../connect"},
	{name: "sidecar", dir: "../../../sidecar"},
}

// Directories the scan never enters, beside every name that starts with a
// dot: fixtures, other people's trees, and the artifact trees server's
// test-dirs.sh prunes.
var rootLiteralSkippedDirs = []string{"testdata", "node_modules", "vendor", "bin", "build", "profile", "temp"}

// The last labels of the special-use domains (RFC 2606, RFC 6761). A root
// under one is a test root, which code and tests may name freely.
var specialUseLabels = []string{"example", "test", "invalid", "localhost"}

// One repository the scan reads.
type rootLiteralRepo struct {
	name string
	dir  string
}

// One string literal that names a root.
type rootLiteral struct {
	// the repository's name, then the path within it, with slashes
	path    string
	line    int
	column  int
	literal string
}

// What may name a root, by path, each with why.
type rootLiteralAllowlist struct {
	Files map[string]string `yaml:"files"`
}

// The roots as whole names inside a literal, in any case, each dot written
// plain or escaped as a regular expression writes it, and the match ending at
// anything that cannot continue the last label. For a root user.example,
// user.example, API.USER.EXAMPLE., user\.example and the file name
// user.example.crt match; superuser.example, my-user.example and
// user.examples do not.
func rootLiteralPattern(roots []string) *regexp.Regexp {
	alternatives := []string{}
	for _, root := range roots {
		labels := []string{}
		for _, label := range strings.Split(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(root)), "."), ".") {
			labels = append(labels, regexp.QuoteMeta(label))
		}
		alternatives = append(alternatives, strings.Join(labels, `\\*\.`))
	}
	return regexp.MustCompile(`(?i)(?:^|[^a-z0-9-])(?:` + strings.Join(alternatives, "|") + `)(?:[^a-z0-9-]|$)`)
}

// Every string literal of every Go file under the repositories that names one
// of roots, in walk order. Symbolic links below a repository's directory are
// not followed.
func scanRootLiterals(repos []rootLiteralRepo, roots []string) ([]rootLiteral, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("no root to look for")
	}
	pattern := rootLiteralPattern(roots)
	literals := []rootLiteral{}
	for _, repo := range repos {
		repoDir, err := filepath.EvalSymlinks(repo.dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", repo.name, err)
		}
		err = filepath.WalkDir(repoDir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path != repoDir && (strings.HasPrefix(entry.Name(), ".") || slices.Contains(rootLiteralSkippedDirs, entry.Name())) {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".go") {
				return nil
			}
			source, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			// most files name no root anywhere, so only those that do are tokenized
			if !pattern.Match(source) {
				return nil
			}
			relativePath, err := filepath.Rel(repoDir, path)
			if err != nil {
				return err
			}
			fileSet := token.NewFileSet()
			file := fileSet.AddFile(path, fileSet.Base(), len(source))
			var goScanner scanner.Scanner
			// a file that does not tokenize cleanly is still read to its end
			goScanner.Init(file, source, nil, 0)
			for {
				position, tok, literal := goScanner.Scan()
				if tok == token.EOF {
					break
				}
				if tok != token.STRING || !pattern.MatchString(literal) {
					continue
				}
				filePosition := fileSet.Position(position)
				literals = append(literals, rootLiteral{
					path:    repo.name + "/" + filepath.ToSlash(relativePath),
					line:    filePosition.Line,
					column:  filePosition.Column,
					literal: literal,
				})
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", repo.name, err)
		}
	}
	return literals, nil
}

// What literals break against allowlist, sorted: a literal in a file the list
// does not name, as "<path>:<line>:<column>: ...", and a listed file that
// holds none, as "<path>: ...".
func rootLiteralProblems(literals []rootLiteral, allowlist map[string]string) []string {
	problems := []string{}
	holdingPaths := map[string]bool{}
	for _, literal := range literals {
		holdingPaths[literal.path] = true
		if _, ok := allowlist[literal.path]; !ok {
			problems = append(problems, fmt.Sprintf("%s:%d:%d: %s names a user root: read roots from config/<env>/dns.yml and keep platform names on bringyour.com, or list the file in %s with why", literal.path, literal.line, literal.column, literal.literal, rootLiteralAllowlistPath))
		}
	}
	for path := range allowlist {
		if !holdingPaths[path] {
			problems = append(problems, fmt.Sprintf("%s: listed in %s, but no literal there names a root any more: drop its entry", path, rootLiteralAllowlistPath))
		}
	}
	sort.Strings(problems)
	return problems
}

// Reads an allowlist: known keys only, each path under one of the
// repositories, clean and relative, and each with a reason.
func readRootLiteralAllowlist(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	allowlist := &rootLiteralAllowlist{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(allowlist); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	for file, reason := range allowlist.Files {
		repoName, _, _ := strings.Cut(file, "/")
		if !slices.ContainsFunc(rootLiteralRepos, func(repo rootLiteralRepo) bool { return repo.name == repoName }) ||
			filepath.ToSlash(filepath.Clean(file)) != file || filepath.IsAbs(file) || strings.Contains(file, "..") {
			t.Errorf("%s: %q is not a clean path in server, connect or sidecar", path, file)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s: %s gives no reason", path, file)
		}
	}
	return allowlist.Files
}

// Every root an environment of a config checkout serves, from each
// <env>/dns.yml: lowercase, without a trailing dot, sorted and without
// repeats.
func readConfigRoots(t *testing.T, configDir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(configDir, "*", "dns.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no environment in %s has a dns.yml", configDir)
	}
	roots := []string{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// dns.yml holds much more, which sidecar/dnsservice reads strictly
		dnsConfig := &struct {
			Roots []string `yaml:"roots"`
		}{}
		if err := yaml.Unmarshal(data, dnsConfig); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, root := range dnsConfig.Roots {
			roots = append(roots, strings.TrimSuffix(strings.ToLower(strings.TrimSpace(root)), "."))
		}
	}
	sort.Strings(roots)
	return slices.Compact(roots)
}

// Whether a name lies under a special-use domain.
func isSpecialUseName(name string) bool {
	labels := strings.Split(strings.TrimSuffix(strings.ToLower(name), "."), ".")
	return slices.Contains(specialUseLabels, labels[len(labels)-1])
}

// Copies a fixture tree into a new directory, each "<name>.fixture" file as
// "<name>", so that no fixture is a Go file of this module.
func copyRootLiteralFixture(t *testing.T, fixtureDir string) string {
	t.Helper()
	targetDir := t.TempDir()
	err := filepath.WalkDir(fixtureDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relativePath, err := filepath.Rel(fixtureDir, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(targetDir, strings.TrimSuffix(relativePath, ".fixture"))
		if entry.IsDir() {
			return os.MkdirAll(targetPath, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(targetPath, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return targetDir
}

// No string literal in server, connect or sidecar names a production root
// outside the allowlisted files, and every allowlisted file still holds one.
func TestRootLiteralsStayOutOfCode(t *testing.T) {
	roots := []string{}
	for _, root := range readConfigRoots(t, rootConfigDir) {
		if !isSpecialUseName(root) {
			roots = append(roots, root)
		}
	}
	if len(roots) == 0 {
		t.Fatalf("no environment's dns.yml in %s names a root outside the special-use domains, so the lint would look for nothing", rootConfigDir)
	}
	literals, err := scanRootLiterals(rootLiteralRepos, roots)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range rootLiteralProblems(literals, readRootLiteralAllowlist(t, rootLiteralAllowlistPath)) {
		t.Error(problem)
	}
}

// On a fixture with a root of its own: a literal naming the root in any of
// its spellings fails in any repository, a name that only resembles it does
// not, comments, fixtures, dot directories and other files are not read, an
// allowlisted file passes, and an allowlisted file that names no root fails.
func TestRootLiteralLintOnAFixture(t *testing.T) {
	fixtureDir := copyRootLiteralFixture(t, filepath.Join("testdata", "root-literal"))
	repos := []rootLiteralRepo{}
	for _, repo := range rootLiteralRepos {
		repos = append(repos, rootLiteralRepo{name: repo.name, dir: filepath.Join(fixtureDir, repo.name)})
	}
	literals, err := scanRootLiterals(repos, []string{"User.Example."})
	if err != nil {
		t.Fatal(err)
	}
	problems := rootLiteralProblems(literals, readRootLiteralAllowlist(t, filepath.Join(fixtureDir, "allowlist.yml")))
	want := []string{
		"connect/clean.go: listed",
		"server/planted.go:10:",
		"server/planted.go:12:",
		"server/planted.go:14:",
		"server/planted.go:6:",
		"server/planted.go:8:",
		"server/planted.go:8:",
		"sidecar/nested/deep.go:4:",
	}
	if len(problems) != len(want) {
		t.Fatalf("%d problems, want %d:\n%s", len(problems), len(want), strings.Join(problems, "\n"))
	}
	for index, problem := range problems {
		if !strings.HasPrefix(problem, want[index]) {
			t.Errorf("problem %d is %q, want one starting %q", index, problem, want[index])
		}
	}
	// the allowlisted literal was found, and only its own file holds it
	allowed := 0
	for _, literal := range literals {
		if literal.path == "connect/allowed_test.go" {
			allowed += 1
			if literal.line != 4 || literal.literal != `"user.example"` {
				t.Errorf("the allowlisted literal is %+v", literal)
			}
		}
	}
	if allowed != 1 {
		t.Errorf("the allowlisted file holds %d literals, want 1", allowed)
	}
}

// A scan with no root to look for refuses rather than passing everything.
func TestRootLiteralLintRefusesNoRoot(t *testing.T) {
	if _, err := scanRootLiterals(rootLiteralRepos, nil); err == nil {
		t.Fatal("a scan for no root passed")
	}
}
