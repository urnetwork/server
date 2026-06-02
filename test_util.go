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
	"sync"
	"time"

	_ "net/http/pprof" // Import for side effects

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

		// The attempt runs in its own goroutine rather than inline. tb.Fatal /
		// FailNow (and assert.Equal, which calls FailNow) end a failed attempt
		// with runtime.Goexit, exactly like a real *testing.T. Goexit terminates
		// the *current* goroutine and cannot be caught by recover, so if the
		// attempt ran inline the first failure would unwind and kill this rerun
		// loop (and the whole test) instead of advancing to the next attempt.
		// The child goroutine confines that Goexit to the single attempt; the
		// loop blocks on done and then inspects the wrapper's failed/skipped
		// state to decide whether to rerun.
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				// A panic (e.g. from Raise) is a failed attempt too. Capture it
				// so the final attempt can re-raise it on the test goroutine.
				if r := recover(); r != nil {
					panicValue = r
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
				glog.Infof("[FLAKY]test passed iteration[%d/%d]", i+1, n)
			}
			return
		}
		glog.Infof("[FLAKY]test failed iteration[%d/%d]. err = %v", i+1, n, panicValue)
		if i+1 < n {
			select {
			case <-time.After(self.RerunTimeout):
			}
			continue
		}
		// Out of reruns: surface the failure on the test goroutine so the real
		// *testing.T fails (re-raising the original panic if there was one).
		if panicValue != nil {
			panic(panicValue)
		}
		t.FailNow()
	}
}

// retryTB wraps a *testing.T (through the testing.TB interface) so that a failed
// assertion in a rerun iteration is recorded locally instead of failing the
// parent test. Embedding testing.TB promotes its unexported private() method,
// which is what lets this type satisfy the interface at all; the methods below
// override the embedded ones so failure and skip state never propagate to the
// real test. Non-failure methods (Log, Helper, Name, TempDir, ...) fall through
// to the embedded *testing.T.
type retryTB struct {
	testing.TB
	mu      sync.Mutex
	failed  bool
	skipped bool
}

func (self *retryTB) Fail() {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.failed = true
}

func (self *retryTB) FailNow() {
	self.Fail()
	runtime.Goexit()
}

func (self *retryTB) Failed() bool {
	self.mu.Lock()
	defer self.mu.Unlock()
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
	self.mu.Lock()
	self.skipped = true
	self.mu.Unlock()
	runtime.Goexit()
}

func (self *retryTB) Skipped() bool {
	self.mu.Lock()
	defer self.mu.Unlock()
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
	redis := Vault.RequireSimpleResource("redis.yml").Parse()

	bytes := make([]byte, 16)
	_, err := rand.Read(bytes)
	Raise(err)
	testPgDbName := fmt.Sprintf(
		"test_%d_%s",
		NowUtc().UnixMilli(),
		hex.EncodeToString(bytes),
	)

	testRedisDb := 10

	Db(ctx, func(conn PgConn) {
		_, err := conn.Exec(
			ctx,
			fmt.Sprintf(
				`
					CREATE DATABASE %s
					WITH
						OWNER=%s 
						ENCODING=UTF8
						LOCALE='en_US.UTF-8'
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
			redis["authority"],
			redis["password"],
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

	return func() {
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
