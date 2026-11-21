//go:build sidecar_localstack

package server

// The second metadata cluster for the restore policy test has a separate object store and IAM database.
// Start it from the private stack's already installed MinIO image and remove it when the test ends.
import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest/localstack"
)

// Runs the second MinIO with disposable credentials and an independently assigned, high loopback port.
func secondRestorePolicyAdmin(t *testing.T) *fileClusterStackAdmin {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	docker := func(env []string, args ...string) string {
		t.Helper()
		command := exec.CommandContext(ctx, "docker", args...)
		command.Env = append(os.Environ(), env...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("private MinIO setup: %v: %s", err, output)
		}
		return strings.TrimSpace(string(output))
	}
	image := docker(nil, "inspect", "--format", "{{.Image}}", "sidecar-test-minio")
	name := "sidecar-test-restore-minio-" + NewId().String()
	admin := &fileClusterStackAdmin{accessKey: "sidecar-test-restore", secretKey: NewId().String()}
	// Match the private stack's file ownership and writable drive volume. Docker removes the anonymous volume with the container.
	docker([]string{"MINIO_ROOT_USER=" + admin.accessKey, "MINIO_ROOT_PASSWORD=" + admin.secretKey},
		"run", "--pull=never", "--rm", "-d", "--name", name, "--user", "0:0", "--mount", "type=volume,destination=/drives",
		"-e", "MINIO_ROOT_USER", "-e", "MINIO_ROOT_PASSWORD", "-p", "127.0.0.1:59500-59599:9000",
		image, "server", "/drives/drive{1...4}", "--address", ":9000")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if output, err := exec.CommandContext(ctx, "docker", "rm", "-f", "-v", name).CombinedOutput(); err != nil {
			t.Errorf("remove private MinIO: %v: %s", err, output)
		}
	})
	admin.endpoint = docker(nil, "port", name, "9000/tcp")
	if err := localstack.CheckStackAddress(admin.endpoint); err != nil {
		t.Fatal(err)
	}
	// The process can answer while its drives are offline. Wait for write quorum before creating its first bucket.
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+admin.endpoint+"/minio/health/cluster", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return admin
			}
			err = fmt.Errorf("status %d", response.StatusCode)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("private MinIO readiness: %v (%v)", ctx.Err(), err)
		case <-tick.C:
		}
	}
}
