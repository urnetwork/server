//go:build linux

// A denied CREATE leaves root-only reasons, never body/header data upstream.
package competitionagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// The composed handler retains the failed gate but exposes only generic refusal.
func TestDockerProxyScopeRefusalRetainsReasonWithoutRequestSecrets(t *testing.T) {
	fixture := newDockerProxyTest(t)
	var object map[string]any
	if err := json.Unmarshal(fixture.create, &object); err != nil {
		t.Fatal(err)
	}
	object["Env"] = []string{"SYNTHETIC_TOKEN=never-retain-value"}
	object["HostConfig"].(map[string]any)["Privileged"] = true
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://worker/v1.56/containers/create?synthetic=never-retain-query", bytes.NewReader(body))
	request.Header.Set("Authorization", "never-retain-header")
	response := httptest.NewRecorder()
	fixture.proxy.workerHandler(response, request)
	if response.Code != http.StatusForbidden || strings.TrimSpace(response.Body.String()) != "Docker scope rejected" {
		t.Fatalf("private reason exposed: %d %s", response.Code, response.Body.String())
	}
	files, err := filepath.Glob(filepath.Join(fixture.plan.StateDirectory, "scope-refusal-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("missing refusal receipt: %v %v", files, err)
	}
	content, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Reason string `json:"reason"`
		Sha256 string `json:"request_sha256"`
		Bytes  int64  `json:"request_bytes"`
	}
	if err := json.Unmarshal(content, &receipt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(receipt.Reason, "closed namespace or privilege policy") || receipt.Sha256 != digestDockerBytes(body) || receipt.Bytes != int64(len(body)) {
		t.Fatalf("refusal lost exact gate/digest: %s", content)
	}
	for _, secret := range []string{"never-retain-value", "never-retain-query", "never-retain-header"} {
		if bytes.Contains(content, []byte(secret)) {
			t.Fatalf("receipt retained a request secret: %s", secret)
		}
	}
	info, err := os.Stat(files[0])
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("receipt not private: %v", err)
	}
}

// Unknown field names/errors cannot enlarge one receipt beyond its owner's cap.
func TestDockerProxyScopeRefusalReasonIsBounded(t *testing.T) {
	fixture := newDockerProxyTest(t)
	fixture.proxy.settings.MaximumRefusalReasonBytes = 16
	request := httptest.NewRequest(http.MethodPost, "http://worker/containers/create", http.NoBody)
	if err := fixture.proxy.persistScopeRefusal(strings.Repeat("a", 32), request, strings.Repeat("b", 64), 0, errors.New(strings.Repeat("x", 128))); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(fixture.plan.StateDirectory, "scope-refusal-"+strings.Repeat("a", 32)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Reason    string `json:"reason"`
		Truncated bool   `json:"reason_truncated"`
	}
	if err := json.Unmarshal(content, &receipt); err != nil {
		t.Fatal(err)
	}
	if len(receipt.Reason) != 16 || !receipt.Truncated {
		t.Fatalf("unbounded refusal: %s", content)
	}
}

// Splitting a multibyte character must not grow the decoded diagnostic cap.
func TestDockerProxyScopeRefusalUtf8BoundaryIsBounded(t *testing.T) {
	fixture := newDockerProxyTest(t)
	fixture.proxy.settings.MaximumRefusalReasonBytes = 17
	request := httptest.NewRequest(http.MethodPost, "http://worker/containers/create", http.NoBody)
	id := strings.Repeat("c", 32)
	if err := fixture.proxy.persistScopeRefusal(id, request, strings.Repeat("b", 64), 0, errors.New(strings.Repeat("🧪", 128))); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(fixture.plan.StateDirectory, "scope-refusal-"+id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Reason    string `json:"reason"`
		Truncated bool   `json:"reason_truncated"`
	}
	if err := json.Unmarshal(content, &receipt); err != nil {
		t.Fatal(err)
	}
	if len(receipt.Reason) != 16 || !utf8.ValidString(receipt.Reason) || !receipt.Truncated {
		t.Fatalf("unicode refusal exceeded its byte budget: %s", content)
	}
}
