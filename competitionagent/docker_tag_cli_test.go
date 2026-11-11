// The real CLI must serialize the frozen alias accepted by the private proxy.
package competitionagent

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// This uses an inert daemon; no image or container can be changed.
func TestDockerProxyFrozenBaseTagCli(t *testing.T) {
	cli, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("supplementary Docker CLI serialization control")
	}
	fixture := newDockerProxyTest(t)
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		if dockerProxyApiPath(r.URL.Path) == "/_ping" {
			w.Header().Set("API-Version", "1.56")
		}
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("CLI request: %s %s", r.Method, r.URL.RequestURI())
		fixture.proxy.workerHandler(w, r)
	}))
	defer server.Close()
	alias := "urnetwork/sim-latency-evaluator-base-id:" + strings.TrimPrefix(fixture.plan.BaseImageDigest, "sha256:")
	command := exec.CommandContext(t.Context(), cli, "--host", "tcp://"+strings.TrimPrefix(server.URL, "http://"), "image", "tag", fixture.plan.BaseImageDigest, alias)
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "DOCKER_API_VERSION=1.56"}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("frozen alias rejected: %v: %s", err, output)
	}
}
