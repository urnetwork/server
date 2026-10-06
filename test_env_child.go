// Independent test processes inherit only the live parent's disposable state.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"
)

// Connection identity is qualified before any read-only ownership probe.
type testEnvironmentChildResources struct {
	postgresAuthority string
	postgresUser      string
	postgresPassword  string
	postgresDatabase  string
	redisAuthority    string
	redisPassword     string
	redisDatabase     int
	parentPid         int
}

// Qualify a subprocess against the original local fixture before it installs
// the parent's anonymous-pipe
// resources. It creates no database, lease, resource override or connection
// pool. Only the exact live parent's disposable databases may be inherited.
// Errors deliberately omit resource values, credentials and lease tokens.
func ValidateTestEnvironmentChildResources(ctx context.Context, pgWire, redisWire []byte) error {
	return validateTestEnvironmentChildResources(ctx, pgWire, redisWire, os.Getppid(),
		newTestEnvironmentProbeResolver().LookupHost, attestTestEnvironmentChildOwnership)
}

// Share setup's authority rules and endpoint pinning before checking ownership.
func validateTestEnvironmentChildResources(
	ctx context.Context, pgWire, redisWire []byte, parentPid int,
	lookup func(context.Context, string) ([]string, error),
	attest func(context.Context, testEnvironmentChildResources) error,
) error {
	base, err := loadTestEnvironmentConfiguration()
	if err != nil {
		return errors.New("child local fixture configuration is invalid")
	}
	if os.Getenv("WARP_TEST_ENV_PORTABLE_ROOT") != "" {
		// loadTestEnvironmentConfiguration already requires both escape flags,
		// distinct canonical loopback endpoints and exact maintenance identity.
		if base.postgresAuthority == "127.0.0.1:5432" {
			return errors.New("child portable PostgreSQL endpoint is not private")
		}
	} else {
		// Managed local and attested suite-proxy launchers export these hosts.
		// Unqualified direct invocations cannot invent a local fixture merely
		// by providing a test-shaped database name on stdin.
		if os.Getenv("BRINGYOUR_POSTGRES_HOSTNAME") == "" || os.Getenv("BRINGYOUR_REDIS_HOSTNAME") == "" ||
			(os.Getenv("WARP_TEST_ENV_USE_PORTABLE_RESOURCES") != "" && os.Getenv("WARP_TEST_ENV_USE_PORTABLE_RESOURCES") != "0") ||
			(os.Getenv("WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES") != "" && os.Getenv("WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES") != "0") {
			return errors.New("child local fixture has no qualified launcher profile")
		}
	}
	var pg, redisResource map[string]any
	if yaml.Unmarshal(pgWire, &pg) != nil || yaml.Unmarshal(redisWire, &redisResource) != nil {
		return errors.New("child fixture payload is invalid")
	}
	database, ok := pg["db"].(string)
	if _, valid := parseTestPgDbName(database); !ok || !valid || database == base.postgresDatabase || parentPid <= 1 {
		return errors.New("child PostgreSQL database is not disposable")
	}
	redisDatabase, ok := redisResource["db"].(int)
	cluster, clusterOk := redisResource["cluster"].(bool)
	if !ok || redisDatabase <= testRedisLeaseCoordinatorDb || redisDatabase == base.redisReservedDb || !clusterOk || cluster {
		return errors.New("child Redis database is not disposable")
	}
	if pg["user"] != base.postgresUser || pg["password"] != base.postgresPassword ||
		redisResource["authority"] != base.redisAuthority || redisResource["password"] != base.redisPassword {
		return errors.New("child fixture connection identity differs from the local fixture")
	}
	// Resolve with exactly the same files-first resolver and fallback-preserving
	// representation as TestEnv.setup. Validate the original resource first;
	// accepting an arbitrary pinned address would discard its authority attestation.
	resolved, err := resolveTestPgResource(ctx, map[string]any{"authority": base.postgresAuthority}, lookup)
	if err != nil || pg["authority"] != resolved["authority"] {
		return errors.New("child PostgreSQL endpoint differs from the local fixture")
	}
	return attest(ctx, testEnvironmentChildResources{
		postgresAuthority: resolved["authority"].(string), postgresUser: base.postgresUser,
		postgresPassword: base.postgresPassword, postgresDatabase: database,
		redisAuthority: base.redisAuthority, redisPassword: base.redisPassword,
		redisDatabase: redisDatabase, parentPid: parentPid,
	})
}

// Bound the two read-only probes independently of the later admission workload.
func attestTestEnvironmentChildOwnership(ctx context.Context, child testEnvironmentChildResources) error {
	ctx, cancel := context.WithTimeout(ctx, testEnvironmentProbeTimeout)
	defer cancel()
	return checkTestEnvironmentChildOwnership(ctx, child, readTestEnvironmentChildRedisLease, readTestEnvironmentChildPgOwner)
}

// Require the same fixture nonce across the parent's lease and live database.
func checkTestEnvironmentChildOwnership(
	ctx context.Context, child testEnvironmentChildResources,
	readLease func(context.Context, testEnvironmentChildResources) (string, error),
	readOwner func(context.Context, testEnvironmentChildResources) (bool, error),
) error {
	// TestEnv uses the same random suffix for its database name and its Redis
	// lease, whose prefix is the actual test parent process id. Correlate both, rather
	// than accepting an unrelated active fixture or a name beginning test_.
	parts := strings.Split(child.postgresDatabase, "_")
	if _, ok := parseTestPgDbName(child.postgresDatabase); !ok || child.parentPid <= 1 {
		return errors.New("child fixture ownership identity is invalid")
	}
	want := fmt.Sprintf("%d-%s", child.parentPid, parts[2])
	lease, err := readLease(ctx, child)
	if err != nil || lease != want {
		return errors.New("child Redis lease is not owned by the live parent fixture")
	}
	owned, err := readOwner(ctx, child)
	if err != nil || !owned {
		return errors.New("child PostgreSQL database has no live fixture owner")
	}
	return nil
}

// Inspect the coordinator's existing lease without acquiring or renewing one.
func readTestEnvironmentChildRedisLease(ctx context.Context, child testEnvironmentChildResources) (string, error) {
	client := redis.NewClient(&redis.Options{
		Addr: child.redisAuthority, Password: child.redisPassword,
		DB: testRedisLeaseCoordinatorDb, MaxRetries: -1,
		DialTimeout: testEnvironmentProbeTimeout, ReadTimeout: testEnvironmentProbeTimeout,
		WriteTimeout: testEnvironmentProbeTimeout, ContextTimeoutEnabled: true,
	})
	defer client.Close()
	return client.Get(ctx, fmt.Sprintf("urnetwork:server-test:redis-db-lease:%d", child.redisDatabase)).Result()
}

// Preserve the parent's entire pinned connection graph for the owner query.
func testEnvironmentChildPgConfig(child testEnvironmentChildResources) (*pgx.ConnConfig, error) {
	endpoint := &url.URL{Scheme: "postgres", User: url.UserPassword(child.postgresUser, child.postgresPassword),
		Host: child.postgresAuthority, Path: "/" + child.postgresDatabase, RawQuery: "sslmode=disable"}
	config, err := pgx.ParseConfig(endpoint.String())
	if err != nil {
		return nil, err
	}
	config.ConnectTimeout = testEnvironmentProbeTimeout
	config.LookupFunc = boundedTestEnvironmentLookup(newTestEnvironmentProbeResolver().LookupHost)
	return config, nil
}

// Opening the exact database proves existence; its separate owner must be live.
func readTestEnvironmentChildPgOwner(ctx context.Context, child testEnvironmentChildResources) (bool, error) {
	config, err := testEnvironmentChildPgConfig(child)
	if err != nil {
		return false, err
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return false, err
	}
	defer closePgConnection(context.WithoutCancel(ctx), conn)
	var owned bool
	err = conn.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_stat_activity
		WHERE datname = current_database()
		AND application_name = 'urnetwork-test-database-owner'
		AND pid <> pg_backend_pid()
	)`).Scan(&owned)
	return owned, err
}
