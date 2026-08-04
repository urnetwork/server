package server

import (
	"context"
	"testing"
	// "time"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	_ "net/http/pprof" // Import for side effects

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/glog"
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
	})

	config, err := client.ConfigGet(ctx, "databases").Result()
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
			acquired, err := client.SetNX(ctx, key, token, ttl).Result()
			if err != nil {
				client.Close()
				panic(fmt.Errorf("lease redis test db %d: %w", db, err))
			}
			if !acquired {
				// A lost SET response that go-redis retried internally reports
				// not-acquired while the key already holds this process's
				// token. Nothing else can write this token, so claim the lease.
				current, getErr := client.Get(ctx, key).Result()
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

type TestEnv struct {
	ApplyDbMigrations bool
	Warmup            bool
	RerunCount        int
	RerunTimeout      time.Duration
}

func DefaultTestEnv() *TestEnv {
	return &TestEnv{
		ApplyDbMigrations: true,
		Warmup:            false,
		RerunCount:        4,
		RerunTimeout:      15 * time.Second,
	}
}

// in each test file, `func TestMain(m *testing.M) {(&server.TestEnv{}).TestMain(m)}`
// https://pkg.go.dev/testing
func (self *TestEnv) TestMain(m *testing.M) {
	teardown := self.setup()
	defer teardown()
	code := m.Run()
	defer os.Exit(code)
}

func (self *TestEnv) Run(t *testing.T, callback func(t testing.TB)) {
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
			teardown := self.setup()
			defer teardown()
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

	pg := Vault.RequireSimpleResource("pg.yml").Parse()
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

	popPg := Vault.PushSimpleResource(
		"pg.yml",
		[]byte(fmt.Sprintf(
			`
authority: "%s"
user: "%s"
password: "%s"
db: "%s"`,
			pg["authority"],
			pg["user"],
			pg["password"],
			testPgDbName,
		)),
	)
	PgReset()

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
		Warmup()
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

		popPg()
		PgReset()

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
