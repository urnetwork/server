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

// Reads only effective required local replacements. A moving main checkout
// can neither add dependencies to an old lock nor shrink its historical set.
func requiredLocalSourceRepositories(moduleBytes []byte) ([]string, error) {
	var module sourceModuleGraph
	if err := json.Unmarshal(moduleBytes, &module); err != nil {
		return nil, err
	}
	requiredVersions := map[string]string{}
	for _, requirement := range module.Require {
		requiredVersions[requirement.Path] = requirement.Version
	}
	repositoryNames := []string{filepath.Base(module.Module.Path)}
	for _, replacement := range module.Replace {
		requiredVersion, required := requiredVersions[replacement.Old.Path]
		if !required || replacement.New.Version != "" || (replacement.Old.Version != "" && replacement.Old.Version != requiredVersion) {
			continue
		}
		repositoryPath, sibling := strings.CutPrefix(replacement.New.Path, "../")
		components := strings.Split(repositoryPath, "/")
		if !sibling || strings.Contains(replacement.New.Path, "\\") || slices.Contains(components, "") || slices.Contains(components, ".") || slices.Contains(components, "..") {
			return nil, fmt.Errorf("required local module %s is outside the sibling-repository layout: %s", replacement.Old.Path, replacement.New.Path)
		}
		repositoryNames = append(repositoryNames, components[0])
	}
	slices.Sort(repositoryNames)
	return slices.Compact(repositoryNames), nil
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
	module, err := sourceModuleGraphAtCommit(ctx, repositoryRoot, commit)
	if err != nil {
		return err
	}
	return requireLocalSourceRepositoryCoverage(module, sourceRepositoryNames())
}
