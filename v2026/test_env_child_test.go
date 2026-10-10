// Pure checks qualify inherited fixture resources without contacting a service.
package server

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const childTestDatabase = "test_1780000000000_0123456789abcdef0123456789abcdef"

// Table arms restore overrides and environment immediately, including on Fatal.
type childTestScope struct {
	testing.TB
	cleanups []func()
}

// Defer cleanup within one table arm, rather than at the outer test's end.
func (self *childTestScope) Cleanup(cleanup func()) {
	self.cleanups = append(self.cleanups, cleanup)
}

// Keep each arm's environment changes private to that arm's lifetime.
func (self *childTestScope) Setenv(key, value string) {
	previous, present := os.LookupEnv(key)
	if err := os.Setenv(key, value); err != nil {
		self.Fatal("set synthetic test environment")
	}
	self.Cleanup(func() {
		var err error
		if present {
			err = os.Setenv(key, previous)
		} else {
			err = os.Unsetenv(key)
		}
		if err != nil {
			self.Error("restore synthetic test environment")
		}
	})
}

// Each normal table arm has a scope, without creating a testing subtest.
func runScopedChildTest(t *testing.T, name string, run func(testing.TB)) {
	t.Helper()
	t.Logf("fixture qualification case: %s", name)
	scope := &childTestScope{TB: t}
	defer func() {
		for index := len(scope.cleanups) - 1; index >= 0; index-- {
			scope.cleanups[index]()
		}
	}()
	run(scope)
}

// Install complete synthetic resources; cleanup follows the supplied scope.
func childTestResources(t testing.TB) (map[string]any, map[string]any) {
	t.Helper()
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())
	t.Setenv("BRINGYOUR_POSTGRES_HOSTNAME", "postgres.example")
	t.Setenv("BRINGYOUR_REDIS_HOSTNAME", "redis.example")
	t.Setenv("WARP_TEST_ENV_PORTABLE_ROOT", "")
	t.Setenv("WARP_TEST_ENV_USE_PORTABLE_RESOURCES", "")
	t.Setenv("WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES", "")
	t.Cleanup(Vault.PushSimpleResource(DefaultPgVaultResourceName, childTestWire(t, map[string]any{
		"authority": "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:5432", "user": "test", "password": "not-a-secret", "db": "test",
	})))
	t.Cleanup(Vault.PushSimpleResource("redis.yml", childTestWire(t, map[string]any{
		"authority": "{{ env:BRINGYOUR_REDIS_HOSTNAME }}:6379", "password": "", "db": 0, "cluster": false,
	})))
	for _, name := range []string{DefaultPgConfigResourceName, "redis.yml"} {
		t.Cleanup(Config.PushSimpleResource(name, []byte("min_connections: 0\nmax_connections: 2\n")))
	}
	return map[string]any{
			"authority": "127.0.0.1:5432", "user": "test", "password": "not-a-secret", "db": childTestDatabase,
		}, map[string]any{
			"authority": "redis.example:6379", "password": "", "db": 1, "cluster": false,
		}
}

// Encode only synthetic resource data for the same parser used by the child.
func childTestWire(t testing.TB, resource map[string]any) []byte {
	t.Helper()
	wire, err := yaml.Marshal(resource)
	if err != nil {
		t.Fatal("marshal synthetic resource")
	}
	return wire
}

// Pin a synthetic managed alias without using the machine's resolver.
func childTestLookup(_ context.Context, host string) ([]string, error) {
	if host != "postgres.example" {
		return nil, errors.New("unexpected fixture host")
	}
	return []string{"127.0.0.1"}, nil
}

// Managed local resources must qualify after the same endpoint pinning as setup.
func TestTestEnvironmentChildManagedResources(t *testing.T) {
	pg, redisResource := childTestResources(t)
	var qualified testEnvironmentChildResources
	err := validateTestEnvironmentChildResources(context.Background(), childTestWire(t, pg), childTestWire(t, redisResource), 1234,
		childTestLookup, func(_ context.Context, child testEnvironmentChildResources) error { qualified = child; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if qualified.postgresAuthority != "127.0.0.1:5432" || qualified.redisAuthority != "redis.example:6379" ||
		qualified.postgresDatabase != childTestDatabase || qualified.redisDatabase != 1 || qualified.parentPid != 1234 {
		t.Fatal("managed fixture identity was not preserved")
	}
	if Vault.RequireSimpleResource(DefaultPgVaultResourceName).RequireString("db") != "test" ||
		Vault.RequireSimpleResource("redis.yml").RequireInt("db") != 0 {
		t.Fatal("validation mutated resource scopes")
	}
}

// Every identity or namespace mismatch must fail before contacting a service.
func TestTestEnvironmentChildRejectsUnqualifiedPayloadBeforeAttestation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(testing.TB, map[string]any, map[string]any)
	}{
		{"unset environment", func(t testing.TB, _, _ map[string]any) { t.Setenv("WARP_ENV", "") }},
		{"remote environment", func(t testing.TB, _, _ map[string]any) { t.Setenv("WARP_ENV", "main") }},
		{"unset PostgreSQL host", func(t testing.TB, _, _ map[string]any) { t.Setenv("BRINGYOUR_POSTGRES_HOSTNAME", "") }},
		{"unset Redis host", func(t testing.TB, _, _ map[string]any) { t.Setenv("BRINGYOUR_REDIS_HOSTNAME", "") }},
		{"unqualified portable profile", func(t testing.TB, _, _ map[string]any) { t.Setenv("WARP_TEST_ENV_USE_PORTABLE_RESOURCES", "1") }},
		{"unqualified unmanaged profile", func(t testing.TB, _, _ map[string]any) {
			t.Setenv("WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES", "1")
		}},
		{"different PostgreSQL host", func(_ testing.TB, pg, _ map[string]any) { pg["authority"] = "192.0.2.19:5432" }},
		{"different PostgreSQL port", func(_ testing.TB, pg, _ map[string]any) { pg["authority"] = "127.0.0.1:5433" }},
		{"nonstring PostgreSQL authority", func(_ testing.TB, pg, _ map[string]any) { pg["authority"] = []string{"127.0.0.1:5432"} }},
		{"nonstring PostgreSQL user", func(_ testing.TB, pg, _ map[string]any) { pg["user"] = map[string]string{"nested": "test"} }},
		{"different PostgreSQL user", func(_ testing.TB, pg, _ map[string]any) { pg["user"] = "another-user" }},
		{"different PostgreSQL password", func(_ testing.TB, pg, _ map[string]any) { pg["password"] = "synthetic-private-sentinel" }},
		{"base PostgreSQL database", func(_ testing.TB, pg, _ map[string]any) { pg["db"] = "test" }},
		{"prefix-only PostgreSQL database", func(_ testing.TB, pg, _ map[string]any) { pg["db"] = "test_not_owned" }},
		{"malformed PostgreSQL suffix", func(_ testing.TB, pg, _ map[string]any) { pg["db"] = "test_1780000000000_0123" }},
		{"different Redis host", func(_ testing.TB, _, r map[string]any) { r["authority"] = "remote.example:6379" }},
		{"nonstring Redis authority", func(_ testing.TB, _, r map[string]any) { r["authority"] = []string{"redis.example:6379"} }},
		{"different Redis port", func(_ testing.TB, _, r map[string]any) { r["authority"] = "redis.example:6380" }},
		{"different Redis password", func(_ testing.TB, _, r map[string]any) { r["password"] = "synthetic-private-sentinel" }},
		{"Redis coordinator", func(_ testing.TB, _, r map[string]any) { r["db"] = 0 }},
		{"negative Redis database", func(_ testing.TB, _, r map[string]any) { r["db"] = -1 }},
		{"noninteger Redis database", func(_ testing.TB, _, r map[string]any) { r["db"] = "1" }},
		{"Redis cluster", func(_ testing.TB, _, r map[string]any) { r["cluster"] = true }},
		{"missing Redis cluster mode", func(_ testing.TB, _, r map[string]any) { delete(r, "cluster") }},
		{"reserved Redis database", func(t testing.TB, _, r map[string]any) {
			pop := Vault.PushSimpleResource("redis.yml", childTestWire(t, r))
			t.Cleanup(pop)
		}},
		{"same generated base database", func(t testing.TB, _, _ map[string]any) {
			pop := Vault.PushSimpleResource(DefaultPgVaultResourceName, childTestWire(t, map[string]any{
				"authority": "postgres.example:5432", "user": "test", "password": "not-a-secret", "db": childTestDatabase,
			}))
			t.Cleanup(pop)
		}},
	} {
		runScopedChildTest(t, tc.name, func(t testing.TB) {
			pg, redisResource := childTestResources(t)
			tc.mutate(t, pg, redisResource)
			called := false
			err := validateTestEnvironmentChildResources(context.Background(), childTestWire(t, pg), childTestWire(t, redisResource), 1234,
				childTestLookup, func(context.Context, testEnvironmentChildResources) error { called = true; return nil })
			if err == nil || called {
				t.Fatal("unqualified payload reached live attestation")
			}
			if strings.Contains(err.Error(), "synthetic-private-sentinel") || strings.Contains(err.Error(), childTestDatabase) {
				t.Fatal("validation error exposed private fixture data")
			}
		})
	}
}

// Invalid resource encodings must not reach resolution or ownership probes.
func TestTestEnvironmentChildMalformedWireFailsBeforeAttestation(t *testing.T) {
	pg, redisResource := childTestResources(t)
	for _, arm := range []string{"PostgreSQL", "Redis"} {
		runScopedChildTest(t, arm, func(t testing.TB) {
			pgWire, redisWire := childTestWire(t, pg), childTestWire(t, redisResource)
			if arm == "PostgreSQL" {
				pgWire = []byte("authority: [unterminated")
			} else {
				redisWire = []byte("authority: [unterminated")
			}
			err := validateTestEnvironmentChildResources(context.Background(), pgWire, redisWire, 1234,
				func(context.Context, string) ([]string, error) {
					t.Fatal("malformed payload reached resolver")
					return nil, nil
				},
				func(context.Context, testEnvironmentChildResources) error {
					t.Fatal("malformed payload reached live attestation")
					return nil
				})
			if err == nil {
				t.Fatal("malformed payload was accepted")
			}
		})
	}
}

// Portable fixtures retain their explicit escape flags and exact private ports.
func TestTestEnvironmentChildPrivatePortableResources(t *testing.T) {
	for _, arm := range []string{"valid", "missing portable flag", "missing unmanaged flag", "wrong port", "shared endpoints", "default PostgreSQL port"} {
		runScopedChildTest(t, arm, func(t testing.TB) {
			pg, redisResource := childTestResources(t)
			t.Setenv("WARP_TEST_ENV_PORTABLE_ROOT", t.TempDir())
			t.Setenv("WARP_TEST_ENV_USE_PORTABLE_RESOURCES", "1")
			t.Setenv("WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES", "1")
			t.Setenv("BRINGYOUR_POSTGRES_HOSTNAME", "127.0.0.1")
			t.Setenv("BRINGYOUR_REDIS_HOSTNAME", "127.0.0.1")
			pg["authority"], redisResource["authority"] = "127.0.0.1:15432", "127.0.0.1:16379"
			t.Setenv("WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY", "127.0.0.1:15432")
			t.Setenv("WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY", "127.0.0.1:16379")
			switch arm {
			case "missing portable flag":
				t.Setenv("WARP_TEST_ENV_USE_PORTABLE_RESOURCES", "")
			case "missing unmanaged flag":
				t.Setenv("WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES", "")
			case "wrong port":
				t.Setenv("WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY", "127.0.0.1:15433")
			case "shared endpoints":
				redisResource["authority"] = pg["authority"]
				t.Setenv("WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY", "127.0.0.1:15432")
			case "default PostgreSQL port":
				pg["authority"] = "127.0.0.1:5432"
				t.Setenv("WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY", "127.0.0.1:5432")
			}
			basePg := map[string]any{"authority": pg["authority"], "user": "test", "password": "not-a-secret", "db": "test"}
			for _, name := range []string{DefaultPgVaultResourceName, MaintenancePgVaultResourceName} {
				pop := Vault.PushSimpleResource(name, childTestWire(t, basePg))
				t.Cleanup(pop)
			}
			pop := Vault.PushSimpleResource("redis.yml", childTestWire(t, map[string]any{
				"authority": redisResource["authority"], "password": "", "db": 0, "cluster": false,
			}))
			t.Cleanup(pop)
			called := false
			err := validateTestEnvironmentChildResources(context.Background(), childTestWire(t, pg), childTestWire(t, redisResource), 1234,
				func(context.Context, string) ([]string, error) {
					t.Fatal("portable literal endpoint used DNS")
					return nil, nil
				},
				func(context.Context, testEnvironmentChildResources) error { called = true; return nil })
			if arm == "valid" {
				if err != nil || !called {
					t.Fatal("exact private portable fixture was rejected", err)
				}
			} else if err == nil || called {
				t.Fatal("unqualified portable endpoint reached live attestation")
			}
		})
	}
}

// Independent owner queries preserve all pinned address families and their order.
func TestTestEnvironmentChildPinnedFallbacks(t *testing.T) {
	pg, redisResource := childTestResources(t)
	pg["authority"] = "::1,127.0.0.1,:5432"
	err := validateTestEnvironmentChildResources(context.Background(), childTestWire(t, pg), childTestWire(t, redisResource), 1234,
		func(context.Context, string) ([]string, error) { return []string{"::1", "127.0.0.1"}, nil },
		func(_ context.Context, child testEnvironmentChildResources) error {
			config, err := testEnvironmentChildPgConfig(child)
			if err != nil {
				return err
			}
			if config.Host != "::1" || config.Port != 5432 || len(config.Fallbacks) != 1 ||
				config.Fallbacks[0].Host != "127.0.0.1" || config.Fallbacks[0].Port != 5432 ||
				config.Database != childTestDatabase || config.User != "test" || config.Password != "not-a-secret" {
				t.Fatal("independent connection changed pinned identity or fallback order")
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
}

// A live lease must identify this parent and this database before owner lookup.
func TestTestEnvironmentChildRequiresCorrelatedLiveOwnership(t *testing.T) {
	for _, arm := range []string{"valid", "absent lease", "wrong parent", "wrong fixture", "lease read failure", "no database owner", "missing database", "invalid parent", "invalid name"} {
		runScopedChildTest(t, arm, func(t testing.TB) {
			child := testEnvironmentChildResources{postgresDatabase: childTestDatabase, parentPid: 1234}
			lease := "1234-0123456789abcdef0123456789abcdef"
			switch arm {
			case "absent lease":
				lease = ""
			case "wrong parent":
				lease = "1235-0123456789abcdef0123456789abcdef"
			case "wrong fixture":
				lease = "1234-ffffffffffffffffffffffffffffffff"
			case "invalid parent":
				child.parentPid = 1
			case "invalid name":
				child.postgresDatabase = "test_unowned"
			}
			leaseReads, ownerReads := 0, 0
			err := checkTestEnvironmentChildOwnership(context.Background(), child,
				func(context.Context, testEnvironmentChildResources) (string, error) {
					leaseReads++
					if arm == "lease read failure" {
						return "", errors.New("synthetic-private-sentinel")
					}
					return lease, nil
				}, func(context.Context, testEnvironmentChildResources) (bool, error) {
					ownerReads++
					if arm == "missing database" {
						return false, errors.New("synthetic-private-sentinel")
					}
					return arm != "no database owner", nil
				})
			if arm == "valid" {
				if err != nil || leaseReads != 1 || ownerReads != 1 {
					t.Fatal("correlated parent fixture ownership failed", err)
				}
			} else if err == nil || strings.Contains(err.Error(), "synthetic-private-sentinel") || strings.Contains(err.Error(), lease) && lease != "" {
				t.Fatal("invalid ownership was accepted or leaked private diagnostics")
			}
			if (arm == "invalid parent" || arm == "invalid name") && leaseReads != 0 {
				t.Fatal("invalid ownership identity reached a live service")
			}
			if (arm == "absent lease" || arm == "wrong parent" || arm == "wrong fixture" || arm == "lease read failure") && ownerReads != 0 {
				t.Fatal("unowned Redis lease reached PostgreSQL")
			}
		})
	}
}
