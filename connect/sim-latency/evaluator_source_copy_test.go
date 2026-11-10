// Exercise non-root Docker extraction against an inert frozen-image archive.
package main

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A readonly parent must not prevent creation of its later archive children.
// File bytes and executable bits survive, while the private checkout is writable.
func TestEvaluatorSourceCopyReadonlyDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("non-root Docker CLI extraction control")
	}
	cli, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("supplementary actual Docker CLI control")
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, header := range []*tar.Header{
		{Name: "server/", Typeflag: tar.TypeDir, Mode: 0555},
		{Name: "server/.synthetic/", Typeflag: tar.TypeDir, Mode: 0555},
		{Name: "server/.synthetic/tool", Typeflag: tar.TypeReg, Mode: 0555, Size: 4},
	} {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := writer.Write([]byte("data")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	stat, err := json.Marshal(map[string]any{"name": "server", "size": 0, "mode": uint32(os.ModeDir | 0555), "mtime": "2020-01-01T00:00:00Z", "linkTarget": ""})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/archive") {
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(stat))
			w.Header().Set("Content-Type", "application/x-tar")
			if r.Method == http.MethodGet {
				_, _ = w.Write(archive.Bytes())
			}
			return
		}
		w.Header().Set("API-Version", "1.56")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	helper, err := filepath.Abs(filepath.Join("evaluator", "container", "subevaluation-ownership.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, corrected := range []bool{false, true} {
		destination := t.TempDir()
		operation := `evaluation_docker cp synthetic:/workspace/server "$3/"`
		if corrected {
			operation = `evaluation_docker_copy_tree synthetic:/workspace/server "$3/"`
		}
		body := `set -Eeuo pipefail
umask 077
source "$1"
subevaluation_scoped=true
evaluation_docker() { "$DOCKER_TEST_CLI" --host "$DOCKER_TEST_HOST" "$@"; }
` + operation
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body, "source-copy-test", helper, "unused", destination)
		command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "DOCKER_API_VERSION=1.56", "DOCKER_TEST_CLI=" + cli, "DOCKER_TEST_HOST=tcp://" + strings.TrimPrefix(server.URL, "http://")}
		output, err := command.CombinedOutput()
		// Restore write access even for the deliberately broken extraction.
		t.Cleanup(func() { _ = exec.Command("/bin/chmod", "-R", "u+rwX", destination).Run() })
		if !corrected {
			if err == nil || !strings.Contains(string(output), "permission denied") {
				t.Fatalf("old non-root extraction did not reproduce: %v: %s", err, output)
			}
			continue
		}
		if err != nil {
			t.Fatalf("corrected source extraction: %v: %s", err, output)
		}
		file := filepath.Join(destination, "server", ".synthetic", "tool")
		content, err := os.ReadFile(file)
		if err != nil || string(content) != "data" {
			t.Fatalf("source content changed: %v %q", err, content)
		}
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm()&0700 != 0700 {
			t.Fatalf("executable owner permissions lost: %v", err)
		}
		if err := os.WriteFile(filepath.Join(destination, "server", ".synthetic", "new"), []byte("patch"), 0600); err != nil {
			t.Fatalf("checkout not writable: %v", err)
		}
	}
}
