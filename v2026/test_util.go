package server

import (
	"context"
	"testing"
	// "time"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "net/http/pprof" // Import for side effects

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/glog/v2026"
)

// each test runs with its own postgres and redis db
// the database is dropped at the end of the test

var pprofServer = sync.OnceFunc(func() {
	go func() {
		http.ListenAndServe(":6060", nil)
	}()
	// e.g. `go tool pprof http://127.0.0.1:6060/debug/pprof/profile`
})

const (
	testRedisLeaseCoordinatorDb     = 0
	testRedisLeaseTtl               = 15 * time.Minute
	testRedisLeaseWait              = 25 * time.Millisecond
	testRedisLeaseReleasedMarkerTtl = time.Minute
	testRedisLeaseRetryTimeout      = 2 * time.Minute
	testRedisLeaseRetryMinWait      = 100 * time.Millisecond
	testRedisLeaseRetryMaxWait      = 5 * time.Second
	testEnvironmentProbeTimeout     = 3 * time.Second
)

// The coordinator client auto-retries a command whose response was lost on a
// broken connection (go-redis MaxRetries), so the release script must be
// idempotent: the first executed-but-unacknowledged attempt deletes the lease
// key and leaves a released marker (KEYS[2], expiring), and the retried
// attempt reports success through the marker instead of misreading its own
// deletion as a lost lease. 1 = released, 2 = already released by this token,
// -1 = another token owns the lease, 0 = lease key gone.
const testRedisLeaseReleaseScript = `
	if redis.call("get", KEYS[1]) == ARGV[1] then
		redis.call("del", KEYS[1])
		redis.call("set", KEYS[2], "1", "px", ARGV[2])
		return 1
	end
	if redis.call("get", KEYS[2]) == "1" then
		return 2
	end
	if redis.call("exists", KEYS[1]) == 1 then
		return -1
	end
	return 0
`

type testRedisDbLease struct {
	client      *redis.Client
	db          int
	key         string
	releasedKey string
	token       string
	renewCancel context.CancelFunc
	renewDone   chan struct{}
	renewLock   sync.Mutex
	renewErr    error
}

func testRedisDbCandidates(databaseCount int, reservedDb int, offset int) []int {
	if databaseCount <= 1 {
		return nil
	}

	candidateCount := databaseCount - 1
	start := offset % candidateCount
	if start < 0 {
		start += candidateCount
	}

	candidates := make([]int, 0, candidateCount)
	for i := 0; i < candidateCount; i += 1 {
		db := 1 + (start+i)%candidateCount
		if db != reservedDb {
			candidates = append(candidates, db)
		}
	}
	return candidates
}

// Retries only connection failures while an idempotent lease setup operation
// remains inside one bounded setup horizon.
func retryTestRedisLeaseConnectionOperation(
	ctx context.Context,
	operation func(context.Context) error,
) error {
	retryCtx, cancel := context.WithTimeout(ctx, testRedisLeaseRetryTimeout)
	defer cancel()
	backoff := &retryBackoff{
		retryMinTimeout: testRedisLeaseRetryMinWait,
		retryMaxTimeout: testRedisLeaseRetryMaxWait,
	}
	var lastErr error
	for {
		select {
		case <-retryCtx.Done():
			if lastErr == nil {
				return retryCtx.Err()
			}
			return fmt.Errorf(
				"redis test lease connection retry ended: %w",
				errors.Join(retryCtx.Err(), lastErr),
			)
		default:
		}

		err := operation(retryCtx)
		if err == nil {
			return nil
		}
		if !isRedisConnectionError(err) {
			return err
		}
		lastErr = err

		select {
		case <-retryCtx.Done():
			return fmt.Errorf(
				"redis test lease connection retry ended: %w",
				errors.Join(retryCtx.Err(), lastErr),
			)
		case <-time.After(backoff.NextRetryTimeout()):
		}
	}
}

func acquireTestRedisDbLease(
	ctx context.Context,
	authority string,
	password string,
	reservedDb int,
	token string,
	offset int,
	ttl time.Duration,
) *testRedisDbLease {
	client := redis.NewClient(&redis.Options{
		Addr:         authority,
		Password:     password,
		DB:           testRedisLeaseCoordinatorDb,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		// The bounded outer setup retry passes one deadline through every
		// internal command and dial attempt instead of letting one nested
		// retry extend beyond the setup horizon.
		ContextTimeoutEnabled: true,
	})

	var config map[string]string
	err := retryTestRedisLeaseConnectionOperation(ctx, func(operationCtx context.Context) error {
		var operationErr error
		config, operationErr = client.ConfigGet(operationCtx, "databases").Result()
		return operationErr
	})
	if err != nil {
		client.Close()
		panic(fmt.Errorf("read redis logical database count: %w", err))
	}
	databaseCount, err := strconv.Atoi(config["databases"])
	if err != nil {
		client.Close()
		panic(fmt.Errorf("parse redis logical database count %q: %w", config["databases"], err))
	}
	candidates := testRedisDbCandidates(databaseCount, reservedDb, offset)
	if len(candidates) == 0 {
		client.Close()
		panic(fmt.Errorf(
			"redis has no test database besides coordinator db %d and reserved db %d",
			testRedisLeaseCoordinatorDb,
			reservedDb,
		))
	}

	for {
		for _, db := range candidates {
			key := fmt.Sprintf("urnetwork:server-test:redis-db-lease:%d", db)
			var acquired bool
			err := retryTestRedisLeaseConnectionOperation(ctx, func(operationCtx context.Context) error {
				var operationErr error
				acquired, operationErr = client.SetNX(operationCtx, key, token, ttl).Result()
				return operationErr
			})
			if err != nil {
				client.Close()
				panic(fmt.Errorf("lease redis test db %d: %w", db, err))
			}
			if !acquired {
				// A lost SET response that go-redis retried internally reports
				// not-acquired while the key already holds this process's
				// token. Nothing else can write this token, so claim the lease.
				var current string
				getErr := retryTestRedisLeaseConnectionOperation(ctx, func(operationCtx context.Context) error {
					var operationErr error
					current, operationErr = client.Get(operationCtx, key).Result()
					return operationErr
				})
				if getErr != nil && !errors.Is(getErr, redis.Nil) {
					client.Close()
					panic(fmt.Errorf("verify redis test db %d lease owner: %w", db, getErr))
				}
				acquired = getErr == nil && current == token
			}
			if acquired {
				lease := &testRedisDbLease{
					client:      client,
					db:          db,
					key:         key,
					releasedKey: fmt.Sprintf("urnetwork:server-test:redis-db-lease-released:%s", token),
					token:       token,
				}
				lease.startRenewal(ttl)
				return lease
			}
		}

		select {
		case <-ctx.Done():
			client.Close()
			panic(fmt.Errorf("wait for redis test database lease: %w", ctx.Err()))
		case <-time.After(testRedisLeaseWait):
		}
	}
}

func testRedisLeaseRenewInterval(ttl time.Duration) time.Duration {
	return max(time.Millisecond, ttl/3)
}

func (self *testRedisDbLease) startRenewal(ttl time.Duration) {
	renewCtx, renewCancel := context.WithCancel(context.Background())
	self.renewCancel = renewCancel
	self.renewDone = make(chan struct{})

	go func() {
		defer close(self.renewDone)

		const renewIfOwned = `
			if redis.call("get", KEYS[1]) == ARGV[1] then
				return redis.call("pexpire", KEYS[1], ARGV[2])
			end
			return 0
		`
		ticker := time.NewTicker(testRedisLeaseRenewInterval(ttl))
		defer ticker.Stop()

		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				renewed, err := self.client.Eval(
					renewCtx,
					renewIfOwned,
					[]string{self.key},
					self.token,
					max(int64(1), ttl.Milliseconds()),
				).Int64()
				if err != nil {
					if renewCtx.Err() != nil {
						return
					}
					self.setRenewError(fmt.Errorf("renew redis test db %d lease: %w", self.db, err))
					return
				}
				if renewed != 1 {
					self.setRenewError(fmt.Errorf("lost redis test db %d lease while test was active", self.db))
					return
				}
			}
		}
	}()
}

func (self *testRedisDbLease) setRenewError(err error) {
	self.renewLock.Lock()
	defer self.renewLock.Unlock()
	self.renewErr = err
}

func (self *testRedisDbLease) renewalError() error {
	self.renewLock.Lock()
	defer self.renewLock.Unlock()
	return self.renewErr
}

func (self *testRedisDbLease) release(ctx context.Context) {
	self.renewCancel()
	<-self.renewDone

	released, releaseErr := self.client.Eval(
		ctx,
		testRedisLeaseReleaseScript,
		[]string{self.key, self.releasedKey},
		self.token,
		testRedisLeaseReleasedMarkerTtl.Milliseconds(),
	).Int64()
	closeErr := self.client.Close()
	Raise(self.renewalError())
	Raise(releaseErr)
	switch released {
	case 1, 2:
	case -1:
		panic(fmt.Errorf("redis test db %d lease was owned by another process during release", self.db))
	default:
		panic(fmt.Errorf("redis test db %d lease was not owned during release", self.db))
	}
	Raise(closeErr)
}

// Holds the connection coordinates needed before an integration test may
// create disposable PostgreSQL databases or lease logical Redis databases.
type testEnvironmentConfiguration struct {
	postgresAuthority string
	postgresUser      string
	postgresPassword  string
	postgresDatabase  string
	redisAuthority    string
	redisPassword     string
	redisReservedDb   int
}

// Refuses an unset or non-local environment before any destructive test setup
// can resolve credentials or connect to a backing service.
func validateTestEnvironmentName() error {
	env, err := Env()
	if err != nil {
		return errors.New("WARP_ENV must be set to local")
	}
	if env != "local" {
		return fmt.Errorf("WARP_ENV must be local, got %q", env)
	}
	return nil
}

// Loads only the Redis fixture used by lease integration tests that do not
// create a PostgreSQL database.
func loadTestRedisLeaseConfiguration() (
	configuration testEnvironmentConfiguration,
	returnErr error,
) {
	if err := validateTestEnvironmentName(); err != nil {
		return configuration, err
	}
	redisResource, err := Vault.SimpleResource("redis.yml")
	if err != nil {
		return configuration, fmt.Errorf("required vault resource redis.yml: %w", err)
	}
	defer func() {
		if value := recover(); value != nil {
			returnErr = fmt.Errorf("invalid local redis test resource: %v", value)
		}
	}()
	configuration.redisAuthority = redisResource.RequireString("authority")
	configuration.redisPassword = redisResource.RequireString("password")
	configuration.redisReservedDb = redisResource.RequireInt("db")
	if redisResource.RequireBool("cluster") {
		return configuration, errors.New("local tests require logical redis databases, not a redis cluster")
	}
	host, _, err := net.SplitHostPort(configuration.redisAuthority)
	if err != nil {
		return configuration, fmt.Errorf("invalid redis authority: %w", err)
	}
	if expectedHost := os.Getenv("BRINGYOUR_REDIS_HOSTNAME"); expectedHost != "" && host != expectedHost {
		return configuration, fmt.Errorf(
			"redis authority host %q does not match configured local host %q",
			host,
			expectedHost,
		)
	}
	return configuration, nil
}

// Converts resource parser panics into one non-retryable preflight error. The
// returned error names resources and keys but never includes credential values.
func loadTestEnvironmentConfiguration() (
	configuration testEnvironmentConfiguration,
	returnErr error,
) {
	if err := validateTestEnvironmentName(); err != nil {
		return configuration, err
	}

	pgResource, err := Vault.SimpleResource(DefaultPgVaultResourceName)
	if err != nil {
		return configuration, fmt.Errorf("required vault resource %s: %w", DefaultPgVaultResourceName, err)
	}
	redisResource, err := Vault.SimpleResource("redis.yml")
	if err != nil {
		return configuration, fmt.Errorf("required vault resource redis.yml: %w", err)
	}
	dbConfigResource, err := Config.SimpleResource(DefaultPgConfigResourceName)
	if err != nil {
		return configuration, fmt.Errorf("required config resource %s: %w", DefaultPgConfigResourceName, err)
	}
	redisConfigResource, err := Config.SimpleResource("redis.yml")
	if err != nil {
		return configuration, fmt.Errorf("required config resource redis.yml: %w", err)
	}

	defer func() {
		if value := recover(); value != nil {
			returnErr = fmt.Errorf("invalid local integration test resource: %v", value)
		}
	}()
	configuration.postgresAuthority = pgResource.RequireString("authority")
	configuration.postgresUser = pgResource.RequireString("user")
	configuration.postgresPassword = pgResource.RequireString("password")
	configuration.postgresDatabase = pgResource.RequireString("db")
	configuration.redisAuthority = redisResource.RequireString("authority")
	configuration.redisPassword = redisResource.RequireString("password")
	configuration.redisReservedDb = redisResource.RequireInt("db")
	if redisResource.RequireBool("cluster") {
		return configuration, errors.New("local tests require logical redis databases, not a redis cluster")
	}
	dbConfigResource.RequireInt("min_connections")
	dbConfigResource.RequireInt("max_connections")
	redisConfigResource.RequireInt("min_connections")
	redisConfigResource.RequireInt("max_connections")
	services := []struct {
		name         string
		authority    string
		expectedHost string
	}{
		{
			name:         "postgres",
			authority:    configuration.postgresAuthority,
			expectedHost: os.Getenv("BRINGYOUR_POSTGRES_HOSTNAME"),
		},
		{
			name:         "redis",
			authority:    configuration.redisAuthority,
			expectedHost: os.Getenv("BRINGYOUR_REDIS_HOSTNAME"),
		},
	}
	for _, service := range services {
		host, _, err := net.SplitHostPort(service.authority)
		if err != nil {
			return configuration, fmt.Errorf("invalid %s authority: %w", service.name, err)
		}
		if service.expectedHost != "" && host != service.expectedHost {
			return configuration, fmt.Errorf(
				"%s authority host %q does not match configured local host %q",
				service.name,
				host,
				service.expectedHost,
			)
		}
	}
	return configuration, nil
}

// Authenticates and checks the exact read-only capabilities setup needs before
// it can safely enter longer Redis operation retries or create a database.
func probeTestEnvironmentService(
	ctx context.Context,
	serviceName string,
	configuration testEnvironmentConfiguration,
) error {
	switch serviceName {
	case "postgres":
		postgresUrl := (&url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword(configuration.postgresUser, configuration.postgresPassword),
			Host:     configuration.postgresAuthority,
			Path:     "/" + configuration.postgresDatabase,
			RawQuery: "sslmode=disable",
		}).String()
		connectionConfig, err := pgx.ParseConfig(postgresUrl)
		if err != nil {
			return fmt.Errorf("parse PostgreSQL test configuration: %w", err)
		}
		connection, err := pgx.ConnectConfig(ctx, connectionConfig)
		if err != nil {
			return fmt.Errorf("authenticate PostgreSQL test connection: %w", err)
		}
		defer connection.Close(ctx)
		var canCreateDatabase bool
		if err := connection.QueryRow(
			ctx,
			"SELECT rolcreatedb FROM pg_roles WHERE rolname = current_user",
		).Scan(&canCreateDatabase); err != nil {
			return fmt.Errorf("read PostgreSQL test role: %w", err)
		}
		if !canCreateDatabase {
			return errors.New("PostgreSQL test role lacks CREATEDB")
		}
		return nil
	case "redis":
		client := redis.NewClient(&redis.Options{
			Addr:                  configuration.redisAuthority,
			Password:              configuration.redisPassword,
			DB:                    testRedisLeaseCoordinatorDb,
			ContextTimeoutEnabled: true,
		})
		defer client.Close()
		redisConfig, err := client.ConfigGet(ctx, "databases").Result()
		if err != nil {
			return fmt.Errorf("authenticate and read Redis logical databases: %w", err)
		}
		databaseCount, err := strconv.Atoi(redisConfig["databases"])
		if err != nil {
			return fmt.Errorf("parse Redis logical database count: %w", err)
		}
		if len(testRedisDbCandidates(databaseCount, configuration.redisReservedDb, 0)) == 0 {
			return fmt.Errorf(
				"Redis has no test database besides coordinator db %d and reserved db %d",
				testRedisLeaseCoordinatorDb,
				configuration.redisReservedDb,
			)
		}
		return nil
	default:
		return fmt.Errorf("unknown test environment service %q", serviceName)
	}
}

// Returns the address used in diagnostics without exposing credentials.
func testEnvironmentServiceAuthority(
	serviceName string,
	configuration testEnvironmentConfiguration,
) string {
	switch serviceName {
	case "postgres":
		return configuration.postgresAuthority
	case "redis":
		return configuration.redisAuthority
	default:
		return "unknown"
	}
}

// Applies the same short connection budget to each dependency without
// consuming the Redis lease operation's two-minute transient retry horizon.
func preflightTestEnvironmentService(
	serviceName string,
	configuration testEnvironmentConfiguration,
	probe func(context.Context, string, testEnvironmentConfiguration) error,
) error {
	authority := testEnvironmentServiceAuthority(serviceName, configuration)
	ctx, cancel := context.WithTimeout(context.Background(), testEnvironmentProbeTimeout)
	err := probe(ctx, serviceName, configuration)
	cancel()
	if err != nil {
		return fmt.Errorf(
			"%s preflight failed at %s: %w; start ./local/run-local.sh",
			serviceName,
			authority,
			err,
		)
	}
	return nil
}

// Checks both backing services exactly once before flaky-test retries begin.
func preflightTestEnvironment(
	probe func(context.Context, string, testEnvironmentConfiguration) error,
) error {
	configuration, err := loadTestEnvironmentConfiguration()
	if err != nil {
		return err
	}
	for _, serviceName := range []string{"postgres", "redis"} {
		if err := preflightTestEnvironmentService(serviceName, configuration, probe); err != nil {
			return err
		}
	}
	return nil
}

type TestEnv struct {
	ApplyDbMigrations bool
	Warmup            bool
	RerunCount        int
	RerunTimeout      time.Duration
}

// testEnvTeardownBound is how long a failed attempt waits for teardown
// before abandoning it (see the comment at the teardown defer in Run).
// Env-overridable so the harness's own meta-test can exercise the abandon
// path without a 60s wait per attempt.
func testEnvTeardownBound() time.Duration {
	if v := os.Getenv("WARP_TEST_TEARDOWN_BOUND_SECONDS"); v != "" {
		if seconds, err := strconv.Atoi(v); err == nil && 0 < seconds {
			return time.Duration(seconds) * time.Second
		}
	}
	return 60 * time.Second
}

func DefaultTestEnv() *TestEnv {
	return &TestEnv{
		ApplyDbMigrations: true,
		Warmup:            false,
		RerunCount:        4,
		RerunTimeout:      15 * time.Second,
	}
}

// Runs package tests inside one environment and tears it down before returning
// the status that the caller passes to os.Exit.
func runTestMain(setup func() func(), run func() int) int {
	teardown := setup()
	defer teardown()
	return run()
}

func testPgResourceForDatabase(pg map[string]any, database string) []byte {
	return []byte(fmt.Sprintf(
		`
authority: "%s"
user: "%s"
password: "%s"
db: "%s"`,
		pg["authority"],
		pg["user"],
		pg["password"],
		database,
	))
}

// Redirect both application and direct-maintenance pools to one ephemeral
// test database. Production-shaped profiles define pg_maintenance.yml; if it
// remained pointed at the persistent database, migrations would run there
// while the test itself saw an empty temporary schema.
func pushTestPgResources(pg, maintenancePg map[string]any, database string) func() {
	popPg := Vault.PushSimpleResource(
		DefaultPgVaultResourceName,
		testPgResourceForDatabase(pg, database),
	)
	popMaintenance := Vault.PushSimpleResource(
		MaintenancePgVaultResourceName,
		testPgResourceForDatabase(maintenancePg, database),
	)
	PgReset()
	return func() {
		popMaintenance()
		popPg()
		PgReset()
	}
}

// in each test file, `func TestMain(m *testing.M) {(&server.TestEnv{}).TestMain(m)}`
// https://pkg.go.dev/testing
func (self *TestEnv) TestMain(m *testing.M) {
	if err := preflightTestEnvironment(probeTestEnvironmentService); err != nil {
		fmt.Fprintf(os.Stderr, "local integration test preflight: %v\n", err)
		os.Exit(1)
		return
	}
	os.Exit(runTestMain(self.setup, m.Run))
}

// Runs a test against disposable PostgreSQL and Redis state after validating
// the shared local dependencies once, outside the flaky-attempt loop.
func (self *TestEnv) Run(t *testing.T, callback func(t testing.TB)) {
	self.runWithSetup(
		t,
		callback,
		func() error {
			return preflightTestEnvironment(probeTestEnvironmentService)
		},
		self.setup,
	)
}

// Accepts injected lifecycle boundaries so retry mechanics can be tested
// hermetically without provisioning databases that those unit tests never use.
func (self *TestEnv) runWithSetup(
	t *testing.T,
	callback func(t testing.TB),
	preflight func() error,
	setup func() func(),
) {
	if err := preflight(); err != nil {
		t.Fatalf("local integration test preflight: %v", err)
	}
	n := self.RerunCount + 1
	for i := 0; i < n; i += 1 {
		// Each attempt runs against a retryTB wrapper, so a failed assertion is
		// recorded locally instead of failing the real *testing.T (see retryTB).
		tb := &retryTB{TB: t}
		var panicValue any
		var panicStack []byte

		// Run each attempt in its own goroutine: tb.Fatal/FailNow (and assert.*,
		// which call FailNow) end a failed attempt with runtime.Goexit, which
		// can't be recovered. Inline, that Goexit would unwind and kill the whole
		// rerun loop; the child goroutine confines it to one attempt. The loop
		// blocks on done, then inspects the wrapper's failed/skipped state.
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				// A panic (e.g. from Raise) is a failed attempt too. Capture it
				// so the final attempt can re-raise it on the test goroutine.
				if r := recover(); r != nil {
					panicValue = r
					panicStack = debug.Stack()
					tb.Fail()
				}
			}()
			teardown := setup()
			defer func() {
				// Teardown can block forever when the attempt failed an
				// assertion while sibling goroutines it spawned still hold
				// pool connections: FailNow's Goexit runs these defers LIFO,
				// so teardown's pgxpool Close() waits on connections whose
				// owner goroutines were never joined, close(done) below never
				// runs, and the whole package hangs to its timeout instead of
				// reporting the failure (cost two 1h+ CI runs before it was
				// found). Bound the wait: a teardown that cannot complete is
				// abandoned loudly so the failure itself gets reported; the
				// leaked attempt goroutines die with the test process.
				teardownDone := make(chan struct{})
				go func() {
					defer close(teardownDone)
					teardown()
				}()
				select {
				case <-teardownDone:
				case <-time.After(testEnvTeardownBound()):
					glog.Errorf("[test_env]teardown blocked >60s (attempt goroutines still holding env resources); abandoning teardown so the failure can report\n")
				}
			}()
			callback(tb)
		}()
		<-done

		// A Skip in the test body is intentional, not flaky: skip the real test
		// and stop rerunning.
		if tb.Skipped() {
			t.SkipNow()
			return
		}
		if !tb.Failed() {
			if 0 < i {
				glog.Infof("[flaky]test passed iteration[%d/%d]", i+1, n)
			}
			return
		}
		if panicValue == nil {
			glog.Infof("[flaky]test failed iteration[%d/%d] (assertion failure, see test log)", i+1, n)
		} else {
			glog.Infof(
				"[flaky]test failed iteration[%d/%d] err = %v\n%s",
				i+1,
				n,
				panicValue,
				panicStack,
			)
		}
		if i+1 < n {
			select {
			case <-time.After(self.RerunTimeout):
			}
			continue
		}
		// Out of reruns: surface the failure on the test goroutine so the real
		// *testing.T fails (re-raising the original panic if there was one).
		if panicValue != nil {
			t.Fatalf("panic after %d attempts: %v\n%s", n, panicValue, panicStack)
		}
		t.FailNow()
	}
}

// retryTB wraps a *testing.T (via testing.TB) so a failed assertion in a rerun
// iteration is recorded locally instead of failing the parent test. Embedding
// testing.TB promotes its unexported private() method, satisfying the interface;
// the methods below override the embedded ones so failure/skip state never
// propagates. Other methods (Log, Helper, Name, ...) fall through to *testing.T.
type retryTB struct {
	testing.TB
	stateLock sync.Mutex
	failed    bool
	skipped   bool
}

func (self *retryTB) Fail() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failed = true
}

func (self *retryTB) FailNow() {
	self.Fail()
	runtime.Goexit()
}

func (self *retryTB) Failed() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.failed
}

func (self *retryTB) Error(args ...any) {
	self.TB.Log(args...)
	self.Fail()
}

func (self *retryTB) Errorf(format string, args ...any) {
	self.TB.Logf(format, args...)
	self.Fail()
}

func (self *retryTB) Fatal(args ...any) {
	self.TB.Log(args...)
	self.FailNow()
}

func (self *retryTB) Fatalf(format string, args ...any) {
	self.TB.Logf(format, args...)
	self.FailNow()
}

func (self *retryTB) Skip(args ...any) {
	self.TB.Log(args...)
	self.SkipNow()
}

func (self *retryTB) Skipf(format string, args ...any) {
	self.TB.Logf(format, args...)
	self.SkipNow()
}

func (self *retryTB) SkipNow() {
	self.stateLock.Lock()
	self.skipped = true
	self.stateLock.Unlock()
	runtime.Goexit()
}

func (self *retryTB) Skipped() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.skipped
}

func (self *TestEnv) setup() func() {
	pprofServer()

	Reset()

	// tests are allowed only in the `local` env
	env := RequireEnv()
	if env != "local" {
		panic(fmt.Errorf("Can only run tests in the local env (%s)", env))
	}

	ctx := context.Background()

	pg := Vault.RequireSimpleResource(DefaultPgVaultResourceName).Parse()
	maintenancePg := pg
	if resource, resourceErr := Vault.SimpleResource(MaintenancePgVaultResourceName); resourceErr == nil {
		maintenancePg = resource.Parse()
	}
	redisResource := Vault.RequireSimpleResource("redis.yml")
	redisAuthority := redisResource.RequireString("authority")
	redisPassword := redisResource.RequireString("password")
	redisDb := redisResource.RequireInt("db")
	if redisResource.RequireBool("cluster") {
		panic(fmt.Errorf("local tests require logical redis databases, not a redis cluster"))
	}

	bytes := make([]byte, 16)
	_, err := rand.Read(bytes)
	Raise(err)
	token := fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(bytes))
	testPgDbName := fmt.Sprintf(
		"test_%d_%s",
		NowUtc().UnixMilli(),
		hex.EncodeToString(bytes),
	)

	testRedisLease := acquireTestRedisDbLease(
		ctx,
		redisAuthority,
		redisPassword,
		redisDb,
		token,
		int(bytes[0]),
		testRedisLeaseTtl,
	)
	setupSucceeded := false
	defer func() {
		if !setupSucceeded {
			testRedisLease.release(ctx)
		}
	}()
	testRedisDb := testRedisLease.db

	reapOrphanedTestPgDbs(ctx)

	Db(ctx, func(conn PgConn) {
		_, err := conn.Exec(
			ctx,
			fmt.Sprintf(
				`
					CREATE DATABASE %s
					WITH
						OWNER=%s
						ENCODING=UTF8
						LC_COLLATE='en_US.UTF-8'
						LC_CTYPE='en_US.UTF-8'
						TEMPLATE='template0'
				`,
				testPgDbName,
				pg["user"],
			),
		)
		Raise(err)
	}, OptReadWrite())

	popPgResources := pushTestPgResources(pg, maintenancePg, testPgDbName)

	popRedis := Vault.PushSimpleResource(
		"redis.yml",
		[]byte(fmt.Sprintf(
			`
authority: "%s"
password: "%s"
db: %d
cluster: %t`,
			redisAuthority,
			redisPassword,
			testRedisDb,
			false,
		)),
	)
	RedisReset()

	Redis(ctx, func(client RedisClient) {
		cmd := client.FlushDB(ctx)
		_, err := cmd.Result()
		Raise(err)
	})

	if self.ApplyDbMigrations {
		ApplyDbMigrations(ctx)
	}

	if self.Warmup {
		Warmup(AllWarmupTargets()...)
	}

	// PEERSSTREAMS2: key-event delivery defaults on, so make the test redis
	// generate keyspace notifications — event-mode listeners are otherwise
	// non-deterministic (silent degradation to the minutes-scale corrective
	// poll). Best-effort: a test env without a live redis skips it.
	func() {
		defer func() {
			recover()
		}()
		Testing_EnableKeyspaceNotifications(ctx)
	}()

	setupSucceeded = true
	return func() {
		defer testRedisLease.release(ctx)

		Reset()

		Redis(ctx, func(client RedisClient) {
			cmd := client.FlushDB(ctx)
			_, err := cmd.Result()
			Raise(err)
		})

		popRedis()
		RedisReset()

		popPgResources()

		Db(ctx, func(conn PgConn) {
			_, err := conn.Exec(
				ctx,
				fmt.Sprintf(
					`
						DROP DATABASE %s
					`,
					testPgDbName,
				),
			)
			Raise(err)
		}, OptReadWrite())
	}
}

// testPgDbOrphanAge is how old an abandoned test database must be before the
// reaper drops it. A TestEnv's database lives for one test, so this is orders
// of magnitude longer than any live database's lifetime — a running suite is
// never touched even when a single package runs for hours.
const testPgDbOrphanAge = 2 * time.Hour

var reapOrphanedTestPgDbsOnce sync.Once

// Validates the generated identifier and returns its embedded creation time.
func parseTestPgDbName(datname string) (int64, bool) {
	parts := strings.Split(datname, "_")
	if len(parts) != 3 || parts[0] != "test" {
		return 0, false
	}
	millis, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, false
	}
	randomBytes, err := hex.DecodeString(parts[2])
	if err != nil || len(randomBytes) != 16 {
		return 0, false
	}
	return millis, true
}

// reapOrphanedTestPgDbs drops test databases left behind by test processes
// that died before teardown.
//
// Teardown drops its own database, but a process killed mid-run (SIGKILL, an
// interrupted suite, a harness panic) never reaches it, and nothing else ever
// cleans up: 87 orphans had accumulated over a month. They are not inert.
// Every CREATE DATABASE copies a template and scans pg_database, so setup gets
// slower as they pile up — a cross-process setup that normally takes ~1.2s was
// observed taking ~30s, which is what broke the lease-separation test's
// rendezvous.
//
// Age comes from the UnixMilli stamp the name already carries, so no catalog
// timestamp is needed. Runs once per test binary. Failures are ignored: a drop
// losing a race with another process's live database (or its own teardown) is
// expected and must never fail the run that happened to sweep.
func reapOrphanedTestPgDbs(ctx context.Context) {
	reapOrphanedTestPgDbsOnce.Do(func() {
		HandleError(func() {
			cutoff := NowUtc().Add(-testPgDbOrphanAge).UnixMilli()
			orphans := []string{}
			Db(ctx, func(conn PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT datname
					FROM pg_database
					WHERE datname LIKE 'test\_%'
					`,
				)
				WithPgResult(result, err, func() {
					for result.Next() {
						var datname string
						Raise(result.Scan(&datname))
						millis, ok := parseTestPgDbName(datname)
						if !ok {
							continue
						}
						if millis < cutoff {
							orphans = append(orphans, datname)
						}
					}
				})
			})
			for _, datname := range orphans {
				func() {
					defer func() {
						// a live database, or one being dropped by its own
						// teardown right now, is not this sweep's problem
						recover()
					}()
					Db(ctx, func(conn PgConn) {
						_, err := conn.Exec(
							ctx,
							fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, datname),
						)
						Raise(err)
					}, OptReadWrite())
				}()
			}
			if 0 < len(orphans) {
				glog.Infof("[test]reaped %d orphaned test databases\n", len(orphans))
			}
		})
	})
}

// ---- test listen port allocation ------------------------------------------
//
// Test servers (proxy socks/http/https/api, wg) take a port NUMBER in their
// settings and bind it themselves on the wildcard address, so tests must pick
// ports up front and there is inherently a reserve -> release -> server-bind
// window. Two rules make that window safe, both learned from certification
// failure c12-1 (TestProxyWgHandoffFastReconnect):
//
//  1. Allocate BELOW the OS ephemeral port range (macOS: 49152+, Linux
//     default: 32768+). A reservation obtained from ":0" lands in the
//     ephemeral range, and in the release->bind window the process's own
//     outbound dials (pg, redis, platform http) are assigned local ports from
//     exactly that range — in c12-1 two of them landed on the just-released
//     numbers and the servers' wildcard binds failed EADDRINUSE. Ports below
//     the range are never assigned to outbound sockets, which removes that
//     collision class instead of narrowing the window.
//
//  2. Probe the WILDCARD address the servers actually bind, not one loopback
//     address. A loopback probe asks the kernel about only one local address;
//     the server later asks for every IPv4 address. The reservation must ask
//     about the same scope, independent of platform-specific socket reuse.
//
// The counter is pid-salted so concurrent test processes walk disjoint
// sequences, and each returned port is held by a wildcard reservation socket
// until release so sequential callers in one process cannot collide either.

const (
	// [testListenPortFloor, testListenPortCeiling) stays clear of well-known
	// service ports below and every platform's ephemeral range above.
	testListenPortFloor   = 20000
	testListenPortCeiling = 32768
)

var testListenPortNext int64 = int64(
	testListenPortFloor +
		(os.Getpid()*7919)%(testListenPortCeiling-testListenPortFloor),
)

// testNextListenPortCandidate returns the next port number the allocator will
// probe, without advancing it. Deterministic tests use this to occupy the
// candidate and assert the probe's scope.
func testNextListenPortCandidate() int {
	return testListenPortFloor +
		int(atomic.LoadInt64(&testListenPortNext)-testListenPortFloor)%
			(testListenPortCeiling-testListenPortFloor)
}

// Uses the same IPv4 wildcard scope as the servers that consume the returned
// port numbers.
func testListenPortAddress(port int) string {
	return fmt.Sprintf("0.0.0.0:%d", port)
}

// ReserveTestListenPorts picks one free port per requested network ("tcp" or
// "udp"), probed and reserved on the wildcard address. The reservations stay
// bound until release, which callers invoke immediately before starting the
// servers that re-bind the same numbers.
func ReserveTestListenPorts(networks ...string) (ports []int, release func(), returnErr error) {
	var reservations []io.Closer
	closeAll := func() {
		for _, reservation := range reservations {
			reservation.Close()
		}
	}
	for _, network := range networks {
		port, reservation, err := reserveTestListenPort(network)
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		ports = append(ports, port)
		reservations = append(reservations, reservation)
	}
	var releaseOnce sync.Once
	release = func() {
		releaseOnce.Do(closeAll)
	}
	return ports, release, nil
}

func reserveTestListenPort(network string) (int, io.Closer, error) {
	span := testListenPortCeiling - testListenPortFloor
	for range span {
		port := testListenPortFloor +
			int(atomic.AddInt64(&testListenPortNext, 1)-1-testListenPortFloor)%span
		switch network {
		case "tcp":
			listener, err := net.Listen("tcp4", testListenPortAddress(port))
			if err == nil {
				return port, listener, nil
			}
		case "udp":
			packetConn, err := net.ListenPacket("udp4", testListenPortAddress(port))
			if err == nil {
				return port, packetConn, nil
			}
		default:
			return 0, nil, fmt.Errorf("reserve test listen port: unsupported network %q", network)
		}
	}
	return 0, nil, fmt.Errorf("reserve test listen port: no free %s port in [%d, %d)", network, testListenPortFloor, testListenPortCeiling)
}
