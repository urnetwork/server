package monitor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderHTTPRejectsTrailingJSONAndRedactsHTTPBodies(t *testing.T) {
	t.Run("trailing value", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"first":true} {"second":true}`))
		}))
		defer server.Close()
		var response map[string]any
		err := newProviderHTTP(server.Client()).json(context.Background(), http.MethodGet, server.URL, "synthetic provider", nil, nil, &response)
		if err == nil || !strings.Contains(err.Error(), "trailing JSON value") {
			t.Fatalf("trailing JSON error = %v", err)
		}
	})

	t.Run("error body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"token":"must-not-leak"}`))
		}))
		defer server.Close()
		_, err := newProviderHTTP(server.Client()).bytes(context.Background(), http.MethodGet, server.URL+"?signature=url-secret", "synthetic provider", nil, nil, 1024)
		if err == nil || !strings.Contains(err.Error(), "HTTP status 401") {
			t.Fatalf("HTTP status error = %v", err)
		}
		for _, secret := range []string{"must-not-leak", "url-secret", server.URL} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("provider error leaked %q: %v", secret, err)
			}
		}
	})

	t.Run("transport error", func(t *testing.T) {
		client := newProviderHTTP(providerDoerFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New(`Get "https://storage.example/archive?signature=url-secret": authorization Bearer ya29.private-access`)
		}))
		client.wait = func(context.Context, time.Duration) error { return nil }
		_, err := client.bytes(context.Background(), http.MethodGet, "https://api.example/report", "synthetic provider", nil, nil, 1024)
		if err == nil {
			t.Fatal("transport error was lost")
		}
		for _, secret := range []string{"storage.example", "url-secret", "private-access", "ya29."} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("transport error leaked %q: %v", secret, err)
			}
		}
		for _, marker := range []string{"redacted-provider-url", "redacted-token"} {
			if !strings.Contains(err.Error(), marker) {
				t.Fatalf("transport error missing %q: %v", marker, err)
			}
		}
	})
}

func TestProviderPaginationRejectsCyclesAndForeignOrigins(t *testing.T) {
	pagination := providerPagination{limit: 2}
	more, err := pagination.next("opaque-one")
	if err != nil || !more {
		t.Fatalf("first page token more=%v err=%v", more, err)
	}
	if _, err := pagination.next("opaque-one"); err == nil || !strings.Contains(err.Error(), "repeated") {
		t.Fatalf("repeated token error = %v", err)
	}
	if _, err := providerNextURL("https://api.example", "https://storage.example/page?token=secret"); err == nil {
		t.Fatal("foreign pagination origin was accepted")
	}
	if next, err := providerNextURL("https://api.example", "/next?cursor=opaque"); err != nil || next != "https://api.example/next?cursor=opaque" {
		t.Fatalf("same-origin pagination next=%q err=%v", next, err)
	}
}

func TestProviderRedirectPoliciesKeepCredentialsOnOriginAndDownloadsOnHTTPS(t *testing.T) {
	initial, _ := http.NewRequest(http.MethodGet, "https://api.example.test/v1/data", nil)
	same, _ := http.NewRequest(http.MethodGet, "https://api.example.test/v1/next", nil)
	foreign, _ := http.NewRequest(http.MethodGet, "https://storage.example.test/object", nil)
	downgrade, _ := http.NewRequest(http.MethodGet, "http://storage.example.test/object", nil)
	if err := providerSameOriginRedirect(same, []*http.Request{initial}); err != nil {
		t.Fatalf("same-origin redirect rejected: %v", err)
	}
	if err := providerSameOriginRedirect(foreign, []*http.Request{initial}); err == nil {
		t.Fatal("credential-bearing redirect changed origin")
	}
	if err := providerHTTPSDownloadRedirect(foreign, []*http.Request{initial}); err != nil {
		t.Fatalf("HTTPS signed download redirect rejected: %v", err)
	}
	if err := providerHTTPSDownloadRedirect(downgrade, []*http.Request{initial}); err == nil {
		t.Fatal("signed download redirect downgraded to HTTP")
	}
}

func TestProviderStateIsPrivateBoundedAndCrossProcessLocked(t *testing.T) {
	stateDir := t.TempDir()
	value := map[string]string{"cursor": "one"}
	if err := saveProviderState(stateDir, "synthetic-provider", 1, value); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDir, "provider-reports", "synthetic-provider.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cursor mode=%#o, want 0600", info.Mode().Perm())
	}
	var loaded map[string]string
	found, err := loadProviderState(stateDir, "synthetic-provider", 1, &loaded)
	if err != nil || !found || loaded["cursor"] != "one" {
		t.Fatalf("loaded=%v found=%v err=%v", loaded, found, err)
	}

	first, err := lockProviderState(context.Background(), stateDir, "synthetic-provider")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lockProviderState(canceled, stateDir, "synthetic-provider"); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("contended canceled lock error = %v", err)
	}
}

func TestProviderStateRejectsOversizedCursorBeforeReplacingLastGoodState(t *testing.T) {
	stateDir := t.TempDir()
	if err := saveProviderState(stateDir, "synthetic-provider", 1, map[string]string{"cursor": "good"}); err != nil {
		t.Fatal(err)
	}
	oversized := map[string]string{"cursor": strings.Repeat("x", providerJSONLimit)}
	if err := saveProviderState(stateDir, "synthetic-provider", 1, oversized); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized cursor error = %v", err)
	}
	var loaded map[string]string
	found, err := loadProviderState(stateDir, "synthetic-provider", 1, &loaded)
	if err != nil || !found || loaded["cursor"] != "good" {
		t.Fatalf("last good cursor changed: loaded=%v found=%v err=%v", loaded, found, err)
	}
}

func TestProviderEvidenceRedactsWithoutMarkingShortTextTruncated(t *testing.T) {
	short := providerEvidence("failure at Main.start")
	if strings.Contains(short, "truncated") {
		t.Fatalf("short evidence marked truncated: %q", short)
	}
	secret := "alice@example.com abcdefghijklmnop.qrstuvwxyzABCDEF.ghijklmnopqrstuv Bearer short-provider-token ya29.play-token https://example.test/path?secret=value"
	redacted := providerEvidence(secret)
	for _, raw := range []string{"alice@example.com", "abcdefghijklmnop.qrstuvwxyzABCDEF.ghijklmnopqrstuv", "short-provider-token", "ya29.play-token", "secret=value"} {
		if strings.Contains(redacted, raw) {
			t.Fatalf("evidence leaked %q: %s", raw, redacted)
		}
	}
	for _, marker := range []string{"redacted-email", "redacted-token", "redacted"} {
		if !strings.Contains(redacted, marker) {
			t.Fatalf("evidence missing %q: %s", marker, redacted)
		}
	}
	long := providerEvidence(strings.Repeat("line\n", 50))
	if !strings.Contains(long, "[report truncated]") {
		t.Fatalf("long evidence lacks truncation marker: %q", long)
	}
	injected := providerEvidence("<script>alert(1)</script>\n## forged heading\n[link](javascript:alert(1))")
	for _, unsafe := range []string{"<script>", "\n## forged", "[link]"} {
		if strings.Contains(injected, unsafe) {
			t.Fatalf("provider evidence retained Markdown/HTML injection %q: %s", unsafe, injected)
		}
	}
}

type providerDoerFunc func(*http.Request) (*http.Response, error)

func (function providerDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}
