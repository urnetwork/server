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

// Both native proof paths retain their actual failure while keeping object
// identities hashed and receipt permissions root-owner-only in production.
func TestDockerProxyNativeProofRefusalRetainsPrivateReason(t *testing.T) {
	for _, operation := range []string{"verify-network", "verify-id-map"} {
		fixture := newDockerProxyTest(t)
		var err error
		objectId := strings.Repeat("d", 64)
		if operation == "verify-network" {
			_, err = fixture.proxy.proveHostNetworkWithDiagnostics(t.Context(), objectId)
		} else {
			_, err = fixture.proxy.proveHostNamespaceWithDiagnostics(t.Context(), objectId)
		}
		if err == nil {
			t.Fatal("missing native authority unexpectedly proved its object")
		}
		files, globErr := filepath.Glob(filepath.Join(fixture.plan.StateDirectory, "scope-refusal-*.json"))
		if globErr != nil || len(files) != 1 {
			t.Fatal("native refusal disappeared behind generic RPC error", files, globErr)
		}
		content, readErr := os.ReadFile(files[0])
		if readErr != nil {
			t.Fatal(readErr)
		}
		var receipt struct {
			Reason, Path string
			Sha256       string `json:"request_sha256"`
		}
		if json.Unmarshal(content, &receipt) != nil || receipt.Reason == "" || !strings.Contains(err.Error(), receipt.Reason) ||
			receipt.Path != "/_urnetwork/host-files/"+operation || receipt.Sha256 != digestDockerBytes([]byte(objectId)) || bytes.Contains(content, []byte(objectId)) {
			t.Fatal("native refusal lost private reason or retained plaintext identity", string(content))
		}
		info, statErr := os.Stat(files[0])
		if statErr != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatal("native refusal receipt is not private", statErr)
		}
	}
}

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
