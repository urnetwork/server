// Checks retained-baseline metadata independently of the full dataset verifier.
package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Failed frontier measurements retain raw identity and explicit ineligibility;
// completing a measurement window alone must never create a valid baseline.
func TestFrontierIncompletePointsRemainIneligibleAndBoundToRawArtifacts(t *testing.T) {
	root := filepath.Join("baseline", "prelaunch-frontier-2026-10-24")
	content, err := os.ReadFile(filepath.Join(root, "INDEX.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Official             bool   `json:"official"`
		ValidBaselineSamples int    `json:"valid_baseline_samples"`
		ExchangeHosts        int    `json:"exchange_hosts"`
		EvaluatedCommit      string `json:"evaluated_server_commit"`
		Points               []struct {
			Path             string `json:"path"`
			CompletionState  string `json:"completion_state"`
			IncompleteCode   string `json:"incomplete_code"`
			BaselineEligible *bool  `json:"baseline_eligible"`
		} `json:"points"`
	}
	if err := json.Unmarshal(content, &index); err != nil || index.Official || index.ValidBaselineSamples != 0 || len(index.Points) == 0 || index.ExchangeHosts <= 0 {
		t.Fatal("failed frontier archive claims official or usable baseline evidence", err)
	}
	for _, point := range index.Points {
		if !filepath.IsLocal(point.Path) || point.BaselineEligible == nil || *point.BaselineEligible || point.CompletionState != "incomplete" || point.IncompleteCode == "" {
			t.Fatal("failed frontier point lost explicit baseline ineligibility", point.Path)
		}
		content, err := os.ReadFile(filepath.Join(root, point.Path, "run.json"))
		if err != nil {
			t.Fatal(err)
		}
		var run RunStats
		if err := json.Unmarshal(content, &run); err != nil || run.CompletionState != point.CompletionState || run.IncompleteCode != point.IncompleteCode || run.BuildRevision != index.EvaluatedCommit || run.Flags["hosts"] != fmt.Sprint(index.ExchangeHosts) {
			t.Fatal("failed frontier index differs from actual run identity or completion", point.Path, err)
		}
		csv, err := os.ReadFile(filepath.Join(root, point.Path, "results.csv"))
		if err != nil || run.ResultsCsvBytes != int64(len(csv)) || run.ResultsCsvSha256 != fmt.Sprintf("%x", sha256.Sum256(csv)) {
			t.Fatal("failed frontier CSV differs from retained sidecar", point.Path, err)
		}
		file, err := os.Open(filepath.Join(root, point.Path, "providers.yml.gz"))
		if err != nil {
			t.Fatal(err)
		}
		reader, err := gzip.NewReader(file)
		if err != nil {
			file.Close()
			t.Fatal(err)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, reader)
		gzipErr, closeErr := reader.Close(), file.Close()
		if copyErr != nil || gzipErr != nil || closeErr != nil || run.ConfigSha256 != fmt.Sprintf("%x", hash.Sum(nil)) {
			t.Fatal("failed frontier provider input differs from measured config", point.Path, copyErr, gzipErr, closeErr)
		}
	}
}

// The exploratory dataset is not interchangeable with the four-host policy.
// Compare its declared topology to every retained measured run, not a comment.
func TestExploratoryBaselineExchangeHostCensusMatchesRawRuns(t *testing.T) {
	root := filepath.Join("baseline", "prelaunch-2026-10-24")
	content, err := os.ReadFile(filepath.Join(root, "INDEX.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		ExchangeHosts int `json:"exchange_hosts"`
		Replicates    int `json:"replicates"`
	}
	if err := json.Unmarshal(content, &index); err != nil || index.ExchangeHosts <= 0 || index.Replicates <= 0 {
		t.Fatal("exploratory topology metadata is absent or invalid", err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "runs", "*.run.json"))
	if err != nil || len(paths) != index.Replicates {
		t.Fatal("exploratory run census differs from declared replicates", err)
	}
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var run struct {
			Flags map[string]string `json:"flags"`
		}
		if err := json.Unmarshal(content, &run); err != nil || run.Flags["hosts"] != fmt.Sprint(index.ExchangeHosts) {
			t.Fatal("declared exchange topology differs from retained measurement", path, err)
		}
	}
}

// Mutable report metadata must stay bound to its bytes in both the index and
// manifest. The separate verifier authenticates the complete retained dataset.
func TestRetainedBaselineMetadataDigests(t *testing.T) {
	manifestBytes, err := os.ReadFile("baseline/MANIFEST.sha256")
	if err != nil {
		t.Fatal(err)
	}
	manifestPathDigests := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(manifestBytes), "\n"), "\n") {
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 || len(parts[0]) != sha256.Size*2 || !filepath.IsLocal(parts[1]) {
			t.Fatalf("malformed baseline manifest entry: %q", line)
		}
		if _, ok := manifestPathDigests[parts[1]]; ok {
			t.Fatalf("duplicate baseline manifest path: %q", parts[1])
		}
		manifestPathDigests[parts[1]] = parts[0]
	}
	// The module boundary is maintained metadata, not historical measurement
	// evidence. Omitting it must not hide drift from a toolchain update.
	if _, present := manifestPathDigests["go.mod"]; !present {
		t.Fatal("baseline module boundary is absent from MANIFEST.sha256")
	}

	checkDigest := func(path string, expected string, source string) {
		t.Helper()
		if !filepath.IsLocal(path) {
			t.Errorf("%s contains a non-local baseline path: %q", source, path)
			return
		}
		contents, err := os.ReadFile(filepath.Join("baseline", path))
		if err != nil {
			t.Errorf("%s: %v", source, err)
			return
		}
		actual := fmt.Sprintf("%x", sha256.Sum256(contents))
		if actual != expected {
			t.Errorf("%s digest for %s = %s, actual %s", source, path, expected, actual)
		}
		if manifestPathDigests[path] != expected {
			t.Errorf("%s digest for %s disagrees with MANIFEST.sha256", source, path)
		}
	}
	for path, digest := range manifestPathDigests {
		if !strings.Contains(path, "/") {
			checkDigest(path, digest, "MANIFEST.sha256")
		}
	}

	indexBytes, err := os.ReadFile("baseline/INDEX.json")
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Report struct {
			Path   string `json:"path"`
			Sha256 string `json:"sha256"`
		} `json:"report"`
		Datasets           []map[string]json.RawMessage `json:"datasets"`
		AdditionalDatasets []map[string]json.RawMessage `json:"additional_datasets"`
	}
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		t.Fatal(err)
	}
	checkDigest(index.Report.Path, index.Report.Sha256, "INDEX.json report")
	for _, dataset := range append(index.Datasets, index.AdditionalDatasets...) {
		for key, digestBytes := range dataset {
			if !strings.HasSuffix(key, "_sha256") {
				continue
			}
			pathKey := strings.TrimSuffix(key, "_sha256") + "_path"
			var path, digest string
			if err := json.Unmarshal(dataset[pathKey], &path); err != nil {
				t.Fatalf("INDEX.json dataset %s: %v", pathKey, err)
			}
			if err := json.Unmarshal(digestBytes, &digest); err != nil {
				t.Fatalf("INDEX.json dataset %s: %v", key, err)
			}
			checkDigest(path, digest, "INDEX.json dataset")
		}
	}
}
