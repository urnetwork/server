// Client key registration loads the blob store on every connection.
// Production connect resolved minio.yml and both settings.yml, logged the
// authority interpolation, and built a new MinIO client each time; the
// configuration is now derived once per input version and the client reused.
// All endpoints, keys and names are synthetic.
package server

import (
	"path/filepath"
	"strings"
	"testing"
)

// Vault, config and site roots that do not exist, with a templated minio.yml
// pushed over the vault. Returns the roots for probe counting.
func pushTemplatedBlobResource(t testing.TB, bucket string) (vaultRoot string, configRoot string, siteRoot string, pop func()) {
	base := t.TempDir()
	vaultRoot = filepath.Join(base, "absent", "vault")
	configRoot = filepath.Join(base, "absent", "config")
	siteRoot = filepath.Join(base, "absent", "site")
	t.Setenv("WARP_VAULT_HOME", vaultRoot)
	t.Setenv("WARP_CONFIG_HOME", configRoot)
	t.Setenv("WARP_SITE_HOME", siteRoot)
	t.Setenv("WARP_ENV", "synthetic-main")
	t.Setenv("WARP_HOST", "synthetic-host")
	pop = Vault.PushSimpleResource("minio.yml", []byte(
		"authority: '{{ env:SYNTHETIC_BLOB_HOSTNAME }}:9000'\nbucket: "+bucket+"\naccess_key: synthetic-access\nsecret_key: synthetic-secret\n"))
	return vaultRoot, configRoot, siteRoot, pop
}

// The interpolation line for the authority template.
const blobAuthorityLookup = "[env]lookup env var \"SYNTHETIC_BLOB_HOSTNAME\""

func TestLoadBlobStoreConfigDerivesOncePerVersion(t *testing.T) {
	t.Setenv("SYNTHETIC_BLOB_HOSTNAME", "blob.example")
	vaultRoot, configRoot, siteRoot, pop := pushTemplatedBlobResource(t, "synthetic")
	defer pop()
	loads := 16
	var configs []*BlobStoreConfig
	output := captureProcessStderr(t, func() {
		for i := 0; i < loads; i += 1 {
			config, present := LoadBlobStoreConfig()
			if !present || config.Authority != "blob.example:9000" || config.Bucket != "synthetic" || config.Local {
				t.Fatalf("blob configuration = %+v, %t", config, present)
			}
			configs = append(configs, config)
		}
	})
	for _, count := range []struct {
		name  string
		probe string
	}{
		{name: "minio.yml", probe: resolverProbe(vaultRoot, "minio.yml")},
		{name: "config settings.yml", probe: resolverProbe(configRoot, "settings.yml")},
		{name: "site settings.yml", probe: resolverProbe(siteRoot, "settings.yml")},
		{name: "the authority interpolation", probe: blobAuthorityLookup},
	} {
		if n := strings.Count(output, count.probe); n != 1 {
			t.Fatalf("%d blob configuration loads read %s %d times; want once", loads, count.name, n)
		}
	}
	// callers own their configuration
	configs[0].Bucket = "changed-by-a-caller"
	if config, _ := LoadBlobStoreConfig(); config == configs[1] || config.Bucket != "synthetic" {
		t.Fatalf("a caller's change reached a later load: %+v", config)
	}
}

// A changed environment or minio.yml is seen by the next load, including an
// unresolvable authority that disables the store and its later repair.
func TestLoadBlobStoreConfigFollowsEnvironmentAndResource(t *testing.T) {
	t.Setenv("SYNTHETIC_BLOB_HOSTNAME", "blob.example")
	_, _, _, pop := pushTemplatedBlobResource(t, "synthetic")
	defer pop()
	authority := func() string {
		config, present := LoadBlobStoreConfig()
		if !present {
			return ""
		}
		return config.Authority
	}
	if got := authority(); got != "blob.example:9000" {
		t.Fatalf("authority = %q", got)
	}
	t.Setenv("SYNTHETIC_BLOB_HOSTNAME", "moved-blob.example")
	if got := authority(); got != "moved-blob.example:9000" {
		t.Fatalf("a changed authority environment was not read again: %q", got)
	}
	t.Setenv("SYNTHETIC_BLOB_HOSTNAME", "")
	if config, present := LoadBlobStoreConfig(); present {
		t.Fatalf("an unresolvable authority kept a store: %+v", config)
	}
	t.Setenv("SYNTHETIC_BLOB_HOSTNAME", "blob.example")
	if got := authority(); got != "blob.example:9000" {
		t.Fatalf("a repaired authority environment was not read again: %q", got)
	}

	popOther := Vault.PushSimpleResource("minio.yml", []byte("authority: other-blob.example:9000\nbucket: other\n"))
	if config, present := LoadBlobStoreConfig(); !present || config.Bucket != "other" || config.Authority != "other-blob.example:9000" {
		t.Fatalf("a new minio.yml was not read: %+v", config)
	}
	popOther()
	pop()
	if config, present := LoadBlobStoreConfig(); present {
		t.Fatalf("a removed minio.yml kept a configuration: %+v", config)
	}
}

// One MinIO client, with its connection pool, serves an unchanged
// configuration; a changed configuration gets its own client.
func TestLoadBlobStoreReusesMinioClientPerConfiguration(t *testing.T) {
	t.Setenv("SYNTHETIC_BLOB_HOSTNAME", "blob.example")
	_, _, _, pop := pushTemplatedBlobResource(t, "synthetic")
	defer pop()
	first, ok := LoadBlobStore()
	if !ok || first.Bucket() != "synthetic" {
		t.Fatalf("blob store = %v, %t", first, ok)
	}
	if again, ok := LoadBlobStore(); !ok || again != first {
		t.Fatal("an unchanged configuration built another MinIO client")
	}
	popOther := Vault.PushSimpleResource("minio.yml", []byte(
		"authority: '{{ env:SYNTHETIC_BLOB_HOSTNAME }}:9000'\nbucket: other\naccess_key: synthetic-access\nsecret_key: synthetic-secret\n"))
	defer popOther()
	other, ok := LoadBlobStore()
	if !ok || other == first || other.Bucket() != "other" {
		t.Fatalf("a changed configuration reused the previous client: %v", other)
	}
}
