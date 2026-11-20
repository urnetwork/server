// GET /sn/evidence serves content by its hash whether it was published at its
// sharded key or, before that layout, at its legacy flat key. A publication
// writes and names only the sharded key. Synthetic signer and deployment.
package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/startifact"
)

func TestSnEvidenceServesBothContentLayouts(t *testing.T) {
	config, artifactKey := configureSnEvidenceHandler(t)
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("the evidence fixture has no blob store")
	}
	// the last of a content hash's store-relative keys, in read order
	legacyContentKey := func(contentHash string) string {
		suffixes, err := startifact.EvidenceContentKeySuffixes(contentHash)
		if err != nil {
			t.Fatal(err)
		}
		return path.Join(store.Prefix(), suffixes[len(suffixes)-1])
	}

	// written at its legacy flat key, as every publication was before
	legacyEvidence := signedSnEvidence(t, config, artifactKey)
	legacyEvidence.RunID = "run-legacy"
	if err := startifact.SignEvidence(legacyEvidence, artifactKey); err != nil {
		t.Fatal(err)
	}
	legacyBytes, err := startifact.EvidenceBytes(legacyEvidence)
	if err != nil {
		t.Fatal(err)
	}
	legacyKey := legacyContentKey(legacyEvidence.ContentHash)
	source := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(source, legacyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), legacyKey, source, "application/json"); err != nil {
		t.Fatal(err)
	}
	get := serveSnEvidence(http.MethodGet, "/sn/evidence?hash="+url.QueryEscape(legacyEvidence.ContentHash), nil)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), legacyBytes) || get.Header().Get("ETag") != `"`+legacyEvidence.ContentHash+`"` {
		t.Fatalf("legacy GET status = %d, body = %s", get.Code, get.Body.String())
	}

	evidence := signedSnEvidence(t, config, artifactKey)
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := startifact.EvidenceBytes(evidence)
	if err != nil {
		t.Fatal(err)
	}
	post := serveSnEvidence(http.MethodPost, "/sn/evidence", encoded)
	if post.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", post.Code, post.Body.String())
	}
	var published startifact.Published
	if err := json.Unmarshal(post.Body.Bytes(), &published); err != nil {
		t.Fatal(err)
	}
	h := strings.TrimPrefix(evidence.ContentHash, "sha256:")
	if !strings.HasSuffix(published.ContentKey, "/st/v1/evidence/content/sha256/"+h[0:2]+"/"+h[2:4]+"/"+h+".json") {
		t.Fatalf("published content key %q is not sharded", published.ContentKey)
	}
	unwritten := legacyContentKey(evidence.ContentHash)
	if reader, err := store.Get(t.Context(), unwritten); err == nil {
		reader.Close()
		t.Fatalf("the publication wrote its legacy flat key %s", unwritten)
	}
	get = serveSnEvidence(http.MethodGet, "/sn/evidence?hash="+url.QueryEscape(evidence.ContentHash), nil)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), expected) {
		t.Fatalf("sharded GET status = %d, body = %s", get.Code, get.Body.String())
	}

	missing := serveSnEvidence(http.MethodGet, "/sn/evidence?hash=sha256:"+strings.Repeat("ff", 32), nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("absent GET status = %d, body = %s", missing.Code, missing.Body.String())
	}
	invalid := serveSnEvidence(http.MethodGet, "/sn/evidence?hash=sha256:short", nil)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid hash GET status = %d, body = %s", invalid.Code, invalid.Body.String())
	}
}
