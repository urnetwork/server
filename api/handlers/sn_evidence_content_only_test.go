// Client-key evidence kinds have only their content route: the history routes
// refuse them and an upload of one is rejected before any store write.
package handlers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/urnetwork/server/startifact"
)

// Listing and exact-hash history reads of either client-key kind are refused
// with a pointer to the content route; an indexed kind is still served.
func TestSnEvidenceHistoryRefusesContentOnlyKinds(t *testing.T) {
	config, artifactKey := configureSnEvidenceHandler(t)
	for _, kind := range []string{startifact.ClientKeyRegistrationEvidenceKind, startifact.ClientKeyObservationEvidenceKind} {
		base := "/sn/evidence/history?deployment_id=" + url.QueryEscape(config.DeploymentId) +
			"&netuid=7&kind=" + url.QueryEscape(kind) +
			"&run_id=" + url.QueryEscape(startifact.EvidenceDeploymentHistoryRunID)
		for _, target := range []string{base, base + "&hash=" + url.QueryEscape("sha256:"+strings.Repeat("ab", 32))} {
			response := serveSnEvidenceHistory(target)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "/sn/evidence?hash=") {
				t.Fatalf("%s history = %d %q, want 400 naming the content route", kind, response.Code, response.Body.String())
			}
		}
	}
	evidence := signedSnEvidence(t, config, artifactKey)
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if post := serveSnEvidence(http.MethodPost, "/sn/evidence", encoded); post.Code != http.StatusOK {
		t.Fatalf("indexed POST status = %d, body = %s", post.Code, post.Body.String())
	}
	indexed := serveSnEvidenceHistory(
		"/sn/evidence/history?deployment_id=" + url.QueryEscape(evidence.DeploymentID) +
			"&netuid=7&kind=" + url.QueryEscape(evidence.Kind) +
			"&run_id=" + url.QueryEscape(evidence.RunID),
	)
	if indexed.Code != http.StatusOK {
		t.Fatalf("indexed history status = %d, body = %s", indexed.Code, indexed.Body.String())
	}
}

// A validly signed client-key envelope is not accepted by the upload route,
// whose receipt names a history key: nothing reaches the store.
func TestSnEvidencePostRefusesContentOnlyKinds(t *testing.T) {
	config, artifactKey := configureSnEvidenceHandler(t)
	for _, kind := range []string{startifact.ClientKeyRegistrationEvidenceKind, startifact.ClientKeyObservationEvidenceKind} {
		evidence := signedSnEvidence(t, config, artifactKey)
		evidence.Kind = kind
		evidence.RunID = ""
		if err := startifact.SignEvidence(evidence, artifactKey); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		if post := serveSnEvidence(http.MethodPost, "/sn/evidence", encoded); post.Code != http.StatusBadRequest {
			t.Fatalf("%s POST status = %d, body = %s, want 400", kind, post.Code, post.Body.String())
		}
		if get := serveSnEvidence(http.MethodGet, "/sn/evidence?hash="+url.QueryEscape(evidence.ContentHash), nil); get.Code != http.StatusNotFound {
			t.Fatalf("%s refused upload reached the store: GET status = %d", kind, get.Code)
		}
	}
}
