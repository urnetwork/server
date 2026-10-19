// Exercises the relocated smoke-only resource generator through the real TLS
// and IP-data consumers, without Docker, services, or production configuration.
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Copies only the host helper and its bundled data into a private release
// layout; no source checkout or inherited service credentials are available.
func evaluatorRuntimeFixtureCommand(t *testing.T) (string, *exec.Cmd) {
	t.Helper()
	releaseRoot := t.TempDir()
	for _, name := range []string{"prepare-runtime.sh", "testdata/runtime/geolite2.mmdb.b64"} {
		content, err := os.ReadFile(filepath.Join("evaluator", "container", name))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(releaseRoot, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runtimeRoot := t.TempDir()
	command := exec.CommandContext(t.Context(), "bash", filepath.Join(releaseRoot, "prepare-runtime.sh"), runtimeRoot)
	command.Dir = releaseRoot
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "LANG=C", "LC_ALL=C",
		"EVALUATION_DB_USER=synthetic", "EVALUATION_DB_NAME=synthetic",
		"EVALUATION_DB_PASSWORD=synthetic-db-fixture-not-a-secret",
		"EVALUATION_REDIS_PASSWORD=synthetic-redis-fixture-not-a-secret",
		"EVALUATION_DB_MIN_CONNECTIONS=1", "EVALUATION_DB_MAX_CONNECTIONS=2",
		"EVALUATION_REDIS_MIN_CONNECTIONS=1", "EVALUATION_REDIS_MAX_CONNECTIONS=2",
	}
	return runtimeRoot, command
}

// The real constructor must find tls.yml and a usable pair with self-sign
// fallback disabled. IP warmup must use only the bundled empty synthetic DB.
func TestEvaluatorRuntimeFixtureLoadsConnectTlsAndIpDatabase(t *testing.T) {
	runtimeRoot, command := evaluatorRuntimeFixtureCommand(t)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("prepare relocated runtime: %v: %s", err, output)
	}
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(runtimeRoot, "config"))
	t.Setenv("WARP_VAULT_HOME", filepath.Join(runtimeRoot, "vault"))
	t.Setenv(server.TestRealVaultEnv, "")
	settings := server.DefaultTransportTlsSettings()
	if settings.EnableSelfSign {
		t.Fatal("fixture test must use the normal fail-closed TLS policy")
	}
	transportTls, err := server.NewTransportTlsFromConfig(settings)
	if err != nil {
		t.Fatalf("actual Connect TLS loader rejected generated config: %v", err)
	}
	tlsConfig, err := transportTls.GetTlsConfigForClient(&tls.ClientHelloInfo{ServerName: "sim.example"})
	if err != nil || len(tlsConfig.Certificates) != 1 {
		t.Fatalf("actual TLS loader rejected generated key pair: %v", err)
	}
	certificate, err := x509.ParseCertificate(tlsConfig.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	if _, err := certificate.Verify(x509.VerifyOptions{DNSName: "sim.example", Roots: roots}); err != nil {
		t.Fatalf("generated fixture certificate is unusable: %v", err)
	}
	if _, err := transportTls.GetTlsConfig("unapproved.example"); err == nil {
		t.Fatal("fixture widened TLS host authorization")
	}
	ipPath, err := server.Config.ResourcePath("mmdb/geolite2.mmdb")
	if err != nil || ipPath != filepath.Join(runtimeRoot, "config", "local", "mmdb", "geolite2.mmdb") {
		t.Fatalf("IP fixture escaped its exact local leaf: %q: %v", ipPath, err)
	}
	ipDatabase, err := server.OpenIpInfoDatabase(ipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ipDatabase.Close()
	ipInfo, err := ipDatabase.GetIpInfo(netip.MustParseAddr("192.0.2.1"))
	if err != nil || ipInfo == nil || ipInfo.CountryCode != "" || ipInfo.City != "" {
		t.Fatalf("synthetic IP database returned real location data: %+v: %v", ipInfo, err)
	}
	wantIpBytes, err := os.ReadFile(filepath.Join("..", "..", "local", "testdata", "config", "local", "mmdb", "geolite2.mmdb"))
	if err != nil {
		t.Fatal(err)
	}
	actualIpBytes, err := os.ReadFile(ipPath)
	if err != nil || !bytes.Equal(actualIpBytes, wantIpBytes) {
		t.Fatalf("host bundle does not contain the checked-in synthetic database: %v", err)
	}
	if err := os.Remove(filepath.Join(runtimeRoot, "vault", "local", "tls", "sim.example", "sim.example.key")); err != nil {
		t.Fatal(err)
	}
	transportTls, err = server.NewTransportTlsFromConfig(settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transportTls.GetTlsConfig("sim.example"); err == nil {
		t.Fatal("missing fixture key enabled a TLS fallback")
	}
}

// Every generated entry belongs to one of the two local leaves; certificates,
// keys and database bytes stay read-only, and no fallback overlay is created.
func TestEvaluatorRuntimeFixtureHasExactLocalOnlyTree(t *testing.T) {
	runtimeRoot, command := evaluatorRuntimeFixtureCommand(t)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("prepare relocated runtime: %v: %s", err, output)
	}
	want := []string{
		"config", "config/local", "config/local/db.yml", "config/local/db_maintenance.yml",
		"config/local/mmdb", "config/local/mmdb/geolite2.mmdb", "config/local/redis.yml", "config/local/settings.yml", "config/local/tls.yml",
		"vault", "vault/local", "vault/local/client.yml", "vault/local/jwt-key.pem", "vault/local/jwt.yml",
		"vault/local/password.yml", "vault/local/pg.yml", "vault/local/proxy.yml", "vault/local/redis.yml",
		"vault/local/tls", "vault/local/tls/sim.example", "vault/local/tls/sim.example/sim.example.crt",
		"vault/local/tls/sim.example/sim.example.key", "vault/local/wireguard.yml",
	}
	var actual []string
	err := filepath.WalkDir(runtimeRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == runtimeRoot {
			return err
		}
		relative, err := filepath.Rel(runtimeRoot, path)
		if err != nil {
			return err
		}
		actual = append(actual, filepath.ToSlash(relative))
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if info.Mode().Perm() != 0o700 {
				t.Errorf("directory %s mode=%o, want 0700", relative, info.Mode().Perm())
			}
		} else if !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 {
			t.Errorf("file %s mode=%v, want a read-only regular file", relative, info.Mode())
		}
		return nil
	})
	if err != nil || !slices.Equal(actual, want) {
		t.Fatalf("runtime local-only tree=%v, want %v: %v", actual, want, err)
	}
}

// A relocated bundle cannot borrow a live database when its fixture is absent
// or corrupt; refusal occurs before generating any runtime credentials.
func TestEvaluatorRuntimeFixtureRejectsMissingOrCorruptBundledIpData(t *testing.T) {
	for _, fixture := range []struct{ name, content string }{
		{name: "missing"}, {name: "malformed", content: "not-base64!"}, {name: "wrong-digest", content: "c3ludGhldGlj\n"},
	} {
		runtimeRoot, command := evaluatorRuntimeFixtureCommand(t)
		fixturePath := filepath.Join(command.Dir, "testdata", "runtime", "geolite2.mmdb.b64")
		if fixture.name == "missing" {
			if err := os.Remove(fixturePath); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(fixturePath, []byte(fixture.content), 0o600); err != nil {
			t.Fatal(err)
		}
		output, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "synthetic GeoLite fixture") {
			t.Fatalf("%s bundled fixture was not refused: %v: %s", fixture.name, err, output)
		}
		entries, err := os.ReadDir(runtimeRoot)
		if err != nil || len(entries) != 0 {
			t.Fatalf("%s bundle failure generated runtime files: %v: %v", fixture.name, entries, err)
		}
	}
}
