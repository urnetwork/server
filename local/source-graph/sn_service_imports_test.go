// A source lock must satisfy the service's actual SN imports, not only the
// older runtime capabilities that its selected dependency already provided.
package sourcegraph

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Resolve every direct service-layer import offline. A stale SN checkout with
// an API YAML directory but no Go package must fail before a lengthy build.
func TestReleaseSourceGraphResolvesActualSnServiceImports(t *testing.T) {
	root := serverDirectory(t)
	fileSet := token.NewFileSet()
	imports := map[string]bool{}
	for _, directory := range []string{"controller", "model", "monitor"} {
		files, err := filepath.Glob(filepath.Join(root, directory, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("inspect service import boundary %s: %v", directory, err)
		}
		for _, name := range files {
			file, err := parser.ParseFile(fileSet, name, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imported := range file.Imports {
				path, err := strconv.Unquote(imported.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(path, "github.com/urfoundation/sn/") {
					imports[path] = true
				}
			}
		}
	}
	if len(imports) == 0 {
		t.Fatal("service SN import census is unexpectedly empty")
	}
	paths := make([]string, 0, len(imports))
	for path := range imports {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	arguments := append([]string{"list", "-mod=readonly"}, paths...)
	command := exec.CommandContext(t.Context(), "go", arguments...)
	command.Dir = root
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOENV=off", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("reviewed SN cannot satisfy actual service imports: %v\n%s", err, output)
	}
}
