// Checks retained-baseline metadata independently of the full dataset verifier.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
		Datasets []map[string]json.RawMessage `json:"datasets"`
	}
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		t.Fatal(err)
	}
	checkDigest(index.Report.Path, index.Report.Sha256, "INDEX.json report")
	for _, dataset := range index.Datasets {
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
