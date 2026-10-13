package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The api's robots.txt keeps crawlers out of /lag/, so a crawler that renders
// lag.dev creates no guest network, and leaves every other path as open as
// the old 404 did.
func TestRobotsTxtDisallowsOnlyTheLagRoutes(t *testing.T) {
	response := httptest.NewRecorder()
	RobotsTxt(response, httptest.NewRequest("GET", "/robots.txt", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", contentType)
	}
	body := response.Body.String()
	if body != "User-agent: *\nDisallow: /lag/\n" {
		t.Errorf("body = %q", body)
	}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.HasPrefix(line, "Disallow:") && strings.TrimSpace(strings.TrimPrefix(line, "Disallow:")) != "/lag/" {
			t.Errorf("robots.txt disallows more than /lag/: %q", line)
		}
	}
}
