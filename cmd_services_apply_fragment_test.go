// Tests for `sidecarctl services apply-fragment`: the merge itself is warp's and tested there
// (warp/services/fragment_test.go), so these hold the command to its contract: where it reads, what it
// prints, that it writes only --out and never over a file, and that a refused merge writes nothing.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/warp/services"
)

// A synthetic vault's services.yml: example.com names and documentation addresses only.
const applyFragmentBaseYaml = `domain: example.com
domains:
    example.com: route53
versions:
-   external_ports: 7000-7099
    internal_ports: 7100-7999
    routing_tables: 100-110
    parallel_block_count: 4
    services_docker_network: testservices
    lb:
        ports:
            - 80
        interfaces:
            edge-0.example.com:
                eth0:
                    docker_network: warpeth0
                    ipv4: 192.0.2.10
    services:
        api:
            status: standard
            ports:
                - 80
            blocks:
                - beta: 1
                - g1: 99
`

const applyFragmentEvsYaml = `version:
    services:
        evs:
            mount:
                vault: no
                config: yes
                site: no
            user: "65532:65532"
            ports:
                - 80
            blocks:
                - beta: 1
                - g1: 99
`

// A WARP_HOME whose vault holds env test's services.yml, and a fragment file. Returns the vault's
// services.yml, the fragment and a directory for --out.
func applyFragmentHome(t *testing.T, fragmentYaml string) (string, string, string) {
	t.Helper()
	warpHome := t.TempDir()
	t.Setenv("WARP_HOME", warpHome)
	t.Setenv("WARP_VAULT_HOME", "")
	servicesYamlPath := filepath.Join(warpHome, "vault", "test", "services.yml")
	if err := os.MkdirAll(filepath.Dir(servicesYamlPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(servicesYamlPath, []byte(applyFragmentBaseYaml), 0600); err != nil {
		t.Fatal(err)
	}
	fragmentPath := filepath.Join(t.TempDir(), "evs.yml")
	if err := os.WriteFile(fragmentPath, []byte(fragmentYaml), 0600); err != nil {
		t.Fatal(err)
	}
	return servicesYamlPath, fragmentPath, t.TempDir()
}

// The command reads the vault warpctl uses, writes the merged file to --out alone, and prints what it
// added and the ports it assigned.
func TestServicesApplyFragmentWritesTheMergeToOut(t *testing.T) {
	servicesYamlPath, fragmentPath, outDir := applyFragmentHome(t, applyFragmentEvsYaml)
	outPath := filepath.Join(outDir, "services.yml")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := sidecarctlCommands.run([]string{"services", "apply-fragment", "test", fragmentPath, "--out=" + outPath}, stdout, stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"services.evs", "evs beta port 80: external ", "evs g1 port 80: external ", "wrote " + outPath} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("the output lacks %q:\n%s", want, stdout)
		}
	}
	vaultYaml, err := os.ReadFile(servicesYamlPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(vaultYaml) != applyFragmentBaseYaml {
		t.Error("the vault's services.yml was written")
	}
	merged, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	servicesConfig, err := services.ParseServicesConfig(merged, outPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(servicesConfig.Versions) != 2 || servicesConfig.Versions[0].Services["evs"] == nil || servicesConfig.Versions[1].Services["evs"] != nil {
		t.Errorf("the merged file is not a new top version holding evs:\n%s", merged)
	}
	info, err := os.Stat(outPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Errorf("--out is %v, %v, want mode 0600", info, err)
	}
}

// --out never names a file that exists, and a merge warp refuses writes nothing.
func TestServicesApplyFragmentWritesNothingItShouldNot(t *testing.T) {
	_, fragmentPath, outDir := applyFragmentHome(t, applyFragmentEvsYaml)
	existing := filepath.Join(outDir, "existing.yml")
	if err := os.WriteFile(existing, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	if code := sidecarctlCommands.run([]string{"services", "apply-fragment", "test", fragmentPath, "--out=" + existing}, &bytes.Buffer{}, stderr); code != 1 || !strings.Contains(stderr.String(), "never writes over a file") {
		t.Errorf("an existing --out: exit %d: %s", code, stderr)
	}
	if kept, _ := os.ReadFile(existing); string(kept) != "keep" {
		t.Errorf("an existing --out was written: %q", kept)
	}

	_, changingPath, changingOutDir := applyFragmentHome(t, "version:\n    services:\n        api:\n            status: none\n")
	outPath := filepath.Join(changingOutDir, "services.yml")
	stderr = &bytes.Buffer{}
	if code := sidecarctlCommands.run([]string{"services", "apply-fragment", "test", changingPath, "--out=" + outPath}, &bytes.Buffer{}, stderr); code != 1 || !strings.Contains(stderr.String(), "services.api.status") {
		t.Errorf("a changing fragment: exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Errorf("a refused merge wrote --out: %v", err)
	}
}

// A fragment the file already holds adds nothing and writes nothing.
func TestServicesApplyFragmentWritesNothingWhenNothingChanges(t *testing.T) {
	servicesYamlPath, fragmentPath, outDir := applyFragmentHome(t, applyFragmentEvsYaml)
	firstOut := filepath.Join(outDir, "first.yml")
	if code := sidecarctlCommands.run([]string{"services", "apply-fragment", "test", fragmentPath, "--out=" + firstOut}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	merged, err := os.ReadFile(firstOut)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(servicesYamlPath, merged, 0600); err != nil {
		t.Fatal(err)
	}
	secondOut := filepath.Join(outDir, "second.yml")
	stdout := &bytes.Buffer{}
	if code := sidecarctlCommands.run([]string{"services", "apply-fragment", "test", fragmentPath, "--out=" + secondOut}, stdout, &bytes.Buffer{}); code != 0 || !strings.Contains(stdout.String(), "nothing written") {
		t.Errorf("a second apply: exit %d: %s", code, stdout)
	}
	if _, err := os.Stat(secondOut); !os.IsNotExist(err) {
		t.Errorf("a merge that changed nothing wrote --out: %v", err)
	}
}

// The command line wants an env, a fragment and --out.
func TestServicesApplyFragmentWantsItsArguments(t *testing.T) {
	_, fragmentPath, outDir := applyFragmentHome(t, applyFragmentEvsYaml)
	for _, args := range [][]string{
		{"services", "apply-fragment", "test", fragmentPath},
		{"services", "apply-fragment", "test", "--out=" + filepath.Join(outDir, "out.yml")},
		{"services", "apply-fragment"},
	} {
		if code := sidecarctlCommands.run(args, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
}
