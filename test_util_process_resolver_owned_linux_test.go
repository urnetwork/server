//go:build linux

package server

// Real owned-child controls retain the original cgroup/guardian admission and
// join. They require explicit delegation and create no service authority.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// Every actual public mount ignores mutable homes and refuses foreign misses.
func TestTestProcessOwnedResolverSealsEveryPublicMount(t *testing.T) {
	if IsOwnedTestProcess() {
		if err := ClaimTestProcessGeneration(t.Name()); err != nil {
			t.Fatal(err)
		}
		foreign := t.TempDir()
		for _, name := range []string{"runtime.yml", "outside.yml"} {
			if err := os.WriteFile(filepath.Join(foreign, name), []byte("marker: foreign\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("WARP_HOME", foreign)
		for _, row := range []struct {
			name, key string
			resolver  *Resolver
		}{
			{name: "config", key: "WARP_CONFIG_HOME", resolver: Config},
			{name: "vault", key: "WARP_VAULT_HOME", resolver: Vault},
			{name: "site", key: "WARP_SITE_HOME", resolver: Site},
		} {
			t.Setenv(row.key, foreign)
			want := []byte("marker: " + row.name + "-original\n")
			if value := row.resolver.RequireBytes("runtime.yml"); !bytes.Equal(value, want) {
				t.Fatalf("public %s byte reader followed mutable homes: %q", row.name, value)
			}
			if value := row.resolver.RequireSimpleResource("runtime.yml").RequireString("marker"); value != row.name+"-original" {
				t.Fatalf("public %s lazy reader followed mutable homes: %q", row.name, value)
			}
			if paths, err := row.resolver.ResourcePaths("outside.yml"); err == nil || paths != nil {
				t.Fatalf("public %s miss escaped sealed authority: %v %v", row.name, paths, err)
			}
			file, err := os.Open(row.resolver.RequirePath("runtime.yml"))
			if err != nil {
				t.Fatal(err)
			}
			seals, sealErr := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
			closeErr := file.Close()
			if sealErr != nil || seals&testProcessSeals != testProcessSeals || closeErr != nil {
				t.Fatalf("public %s returned an unsealed or unclosed descriptor: seals=%d error=%v close=%v", row.name, seals, sealErr, closeErr)
			}
		}
		return
	}
	fixture := newTestProcessExecutionFixture(t)
	for _, mount := range []string{"config", "vault", "site"} {
		addTestProcessConfigurationFile(t, &fixture.spec.Configuration, mount+"/runtime.yml", []byte("marker: "+mount+"-original\n"))
	}
	fixture.start(t, "all-public-mounts")
	outcome := fixture.finish(t)
	if outcome.err != nil || !outcome.result.Started || !outcome.result.Joined ||
		!outcome.result.GenerationClaimed || outcome.result.ExitCode != 0 {
		t.Fatalf("full public-mount owner did not join successfully: %+v %v", outcome.result, outcome.err)
	}
}
