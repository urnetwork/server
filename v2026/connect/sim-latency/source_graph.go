// Audits the exact measured server module graph without executing candidate
// code or consulting the moving control-plane checkout's dependency choices.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Matches the network-free `go mod edit -json` representation.
type sourceModuleVersion struct {
	Path    string
	Version string
}

// A local replacement belongs to its first sibling repository, including
// nested modules whose root module need not itself be required.
type sourceModuleReplacement struct {
	Old sourceModuleVersion
	New sourceModuleVersion
}

// Parsing never resolves modules or permits toolchain auto-downloads.
type sourceModuleGraph struct {
	Module  sourceModuleVersion
	Require []sourceModuleVersion
	Replace []sourceModuleReplacement
}

// In-tree replacements belong to the enclosing frozen repository, rather than
// adding a separately versioned sibling. Their module files need Git custody.
type sourceModuleLayout struct {
	repositoryNames []string
	inTreePaths     []string
}

// Reads only effective required local replacements. A moving main checkout
// can neither add dependencies to an old lock nor shrink its historical set.
func requiredLocalSourceRepositories(moduleBytes []byte) ([]string, error) {
	layout, err := requiredLocalSourceLayout(moduleBytes)
	return layout.repositoryNames, err
}

// Canonical relative paths distinguish committed in-tree modules from siblings;
// cleaning an escape or alias into an acceptable path would change its owner.
func requiredLocalSourceLayout(moduleBytes []byte) (sourceModuleLayout, error) {
	var module sourceModuleGraph
	if err := json.Unmarshal(moduleBytes, &module); err != nil {
		return sourceModuleLayout{}, err
	}
	requiredVersions := map[string]string{}
	for _, requirement := range module.Require {
		requiredVersions[requirement.Path] = requirement.Version
	}
	repositoryNames := []string{filepath.Base(module.Module.Path)}
	inTreePaths := []string{}
	for _, replacement := range module.Replace {
		requiredVersion, required := requiredVersions[replacement.Old.Path]
		if !required || replacement.New.Version != "" || (replacement.Old.Version != "" && replacement.Old.Version != requiredVersion) {
			continue
		}
		repositoryPath, sibling := strings.CutPrefix(replacement.New.Path, "../")
		inTreePath, inTree := strings.CutPrefix(replacement.New.Path, "./")
		if inTree {
			repositoryPath = inTreePath
		}
		components := strings.Split(repositoryPath, "/")
		if (!sibling && !inTree) || strings.ContainsAny(replacement.New.Path, "\\\x00\r\n\t") || slices.Contains(components, "") || slices.Contains(components, ".") || slices.Contains(components, "..") {
			return sourceModuleLayout{}, fmt.Errorf("required local module %s is outside the sibling-repository layout or canonical in-tree layout: %s", replacement.Old.Path, replacement.New.Path)
		}
		if inTree {
			inTreePaths = append(inTreePaths, inTreePath)
		} else {
			repositoryNames = append(repositoryNames, components[0])
		}
	}
	slices.Sort(repositoryNames)
	slices.Sort(inTreePaths)
	return sourceModuleLayout{repositoryNames: slices.Compact(repositoryNames), inTreePaths: slices.Compact(inTreePaths)}, nil
}

// Any additional measured local repository requires a separately reviewed
// source protocol and image release, never an implicit dependency checkout.
func requireLocalSourceRepositoryCoverage(moduleBytes []byte, lockedRepositoryNames []string) error {
	repositories, err := requiredLocalSourceRepositories(moduleBytes)
	if err != nil {
		return err
	}
	for _, repository := range repositories {
		if !slices.Contains(lockedRepositoryNames, repository) {
			return fmt.Errorf("required local repository %s is absent from frozen source repositories %v", repository, lockedRepositoryNames)
		}
	}
	return nil
}

// Parses immutable Git blob bytes in a private empty directory. `go mod edit`
// only parses this file; no build, scripts, imports, network or main env exist.
func sourceModuleGraphAtCommit(ctx context.Context, repositoryRoot, commit string) ([]byte, error) {
	if !sourceGitShaPattern.MatchString(commit) {
		return nil, fmt.Errorf("module audit requires an exact source commit")
	}
	content, err := sourceGitBlobAtCommit(ctx, repositoryRoot, commit, "go.mod", maximumSourceConfigBytes)
	if err != nil {
		return nil, err
	}
	return parseSourceModuleGraph(ctx, content)
}

// Parsing local replacement files follows the same offline syntax-only path;
// it does not execute packages or resolve their dependencies.
func parseSourceModuleGraph(ctx context.Context, content []byte) ([]byte, error) {
	root, err := os.MkdirTemp("", "sim-latency-module-audit-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	path := filepath.Join(root, "go.mod")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, "go", "mod", "edit", "-json", path)
	command.Dir = root
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "GOTOOLCHAIN=local", "GOPROXY=off", "GOWORK=off", "GOENV=off"}
	output, stderr := &sourceBoundedWriter{limit: maximumSourceConfigBytes}, &sourceBoundedWriter{limit: 64 * 1024}
	command.Stdout, command.Stderr = output, stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("parse frozen module graph: %w: %s", err, strings.TrimSpace(string(stderr.content)))
	}
	return output.content, nil
}

// Both fresh bootstrap and reviewed source staging use the measured commit,
// with the same strict coverage rules as immutable source-record generation.
func verifyFrozenSourceGraph(ctx context.Context, repositoryRoot, commit string) error {
	return verifyFrozenSourceGraphForMode(ctx, repositoryRoot, commit, false)
}

// Audits the exact frozen module against its mode's allowed repositories.
func verifyFrozenSourceGraphForMode(ctx context.Context, repositoryRoot, commit string, staging bool) error {
	module, err := sourceModuleGraphAtCommit(ctx, repositoryRoot, commit)
	if err != nil {
		return err
	}
	if err := requireLocalSourceRepositoryCoverage(module, sourceRepositoryNamesForMode(staging)); err != nil {
		return err
	}
	layout, err := requiredLocalSourceLayout(module)
	if err != nil || len(layout.inTreePaths) == 0 {
		return err
	}
	// One bounded Git-read owner covers all nested paths, including a missing
	// local commit's disposable fetch. No moving worktree can supply a file.
	_, err = sourceGitReadAtCommit(ctx, repositoryRoot, commit, defaultSourceGitReadSettings(), func(readCtx context.Context, root string) ([]byte, error) {
		for _, relative := range layout.inTreePaths {
			modulePath := relative + "/go.mod"
			entry, err := sourceGitBytes(readCtx, root, maximumSourceConfigBytes, "--literal-pathspecs", "ls-tree", "-z", "--full-tree", commit, "--", modulePath)
			if err != nil {
				return nil, err
			}
			metadata, name, found := strings.Cut(strings.TrimSuffix(string(entry), "\x00"), "\t")
			fields := strings.Fields(metadata)
			if !found || name != modulePath || len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" || !sourceGitShaPattern.MatchString(fields[2]) {
				return nil, fmt.Errorf("in-tree local module %s has no regular go.mod in frozen source", relative)
			}
			content, err := sourceGitBytes(readCtx, root, maximumSourceConfigBytes, "cat-file", "blob", fields[2])
			if err != nil {
				return nil, err
			}
			if _, err := parseSourceModuleGraph(readCtx, content); err != nil {
				return nil, fmt.Errorf("parse frozen in-tree module %s: %w", relative, err)
			}
		}
		return nil, nil
	})
	return err
}
