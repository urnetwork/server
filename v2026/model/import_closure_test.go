package model

import (
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// router's tests import model (router/router_test.go), so no server package
// that model imports, directly or through others, may import router: that
// makes router's test build an import cycle, and no test of model or of the
// package that added the import reports it. 4053ec942 added model -> oauth
// while oauth/handlers.go imported router for its route table, which is now
// api/oauth_routes.go.
func TestModelImportClosureExcludesRouter(t *testing.T) {
	const modulePath = "github.com/urnetwork/server/v2026"
	const modelPath = modulePath + "/model"
	const routerPath = modulePath + "/router"

	// go test runs in the package directory; the module root is its parent
	moduleRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	goMod, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(goMod), "module "+modulePath+"\n") {
		t.Fatalf("%s is not the root of %s", moduleRoot, modulePath)
	}

	// the platform the test runs on, and the platform the services run on
	linux := build.Default
	linux.GOOS = "linux"
	linux.GOARCH = "amd64"
	for _, ctxt := range []build.Context{build.Default, linux} {
		// breadth first over the server packages in model's non-test
		// imports; importer is the first package found to import each one
		importer := map[string]string{modelPath: ""}
		queue := []string{modelPath}
		for 0 < len(queue) {
			importPath := queue[0]
			queue = queue[1:]
			dir := filepath.Join(moduleRoot, filepath.FromSlash(strings.TrimPrefix(importPath, modulePath)))
			pkg, err := ctxt.ImportDir(dir, 0)
			if err != nil {
				t.Fatalf("%s/%s %s: %v", ctxt.GOOS, ctxt.GOARCH, importPath, err)
			}
			for _, imported := range pkg.Imports {
				if imported != modulePath && !strings.HasPrefix(imported, modulePath+"/") {
					continue
				}
				if _, seen := importer[imported]; seen {
					continue
				}
				importer[imported] = importPath
				if imported == routerPath {
					chain := []string{}
					for p := imported; p != ""; p = importer[p] {
						chain = append([]string{p}, chain...)
					}
					t.Fatalf("%s/%s: model imports router, and router's tests import model: %s", ctxt.GOOS, ctxt.GOARCH, strings.Join(chain, " -> "))
				}
				queue = append(queue, imported)
			}
		}
		if len(importer) < 2 {
			t.Fatalf("%s/%s: model imports no server package; the walk is broken", ctxt.GOOS, ctxt.GOARCH)
		}
	}
}
