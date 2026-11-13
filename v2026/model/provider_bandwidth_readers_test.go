package model

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Active bandwidth figures are advisory (ProviderBandwidth): they come from
// fixed targets a modified provider could recognize and favor, so nothing
// that ranks or selects providers may read them, or a provider could buy a
// better placement by serving the speed test well. This pins the readers of
// provider_bandwidth across the repository's sources: the probe's own due
// draw, and nothing else. A reader added to provider discovery, the score
// export or the URL probe fails here.
func TestProviderBandwidthHasNoRankingReader(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root not found from model: %v", err)
	}
	// a table reference ends at whitespace, a parenthesis, a comma or a
	// semicolon; provider_bandwidth_quota and provider_bandwidth.yml do not
	tableRead := regexp.MustCompile(`(?i)\b(from|join)\s+provider_bandwidth([\s),;]|$)`)
	// any reference: a seam holding the function reads the table as a call does
	pageCall := regexp.MustCompile(`\bGetProviderBandwidthProbeSample\b`)
	var tableReaders, pageCallers []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "third_party", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if tableRead.Match(source) {
			tableReaders = append(tableReaders, relative)
		}
		if pageCall.Match(source) && relative != "model/provider_bandwidth_probe.go" {
			pageCallers = append(pageCallers, relative)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(tableReaders)
	slices.Sort(pageCallers)
	if !slices.Equal(tableReaders, []string{"model/provider_bandwidth_probe.go"}) {
		t.Fatalf("provider_bandwidth readers = %v, want the bandwidth probe's due draw alone", tableReaders)
	}
	if !slices.Equal(pageCallers, []string{"taskworker/work/provider_bandwidth_probe_work.go"}) {
		t.Fatalf("bandwidth probe draw referrers = %v, want the bandwidth probe alone", pageCallers)
	}
}
