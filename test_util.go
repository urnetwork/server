package server

import (
	"context"
	"testing"
	// "time"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/urnetwork/glog"
)

// each test runs with its own postgres and redis db
// the database is dropped at the end of the test

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
		RerunCount:        1,
		RerunTimeout:      2 * time.Second,
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

func (self *TestEnv) Run(callback func()) {
	n := self.RerunCount + 1
	for i := 0; i < n; i += 1 {
		var r any
		func() {
			if i+1 < n {
				defer func() {
					r = recover()
				}()
			}
			teardown := self.setup()
			defer teardown()
			callback()
		}()
		if r == nil {
			if 0 < i {
				glog.Infof("[FLAKY]test passed iteration[%d/%d]", i+i, n, r)
			}
			return
		}
		// glog.Infof("[FLAKY]test failed iteration[%d/%d]. err = %s", i+i, n, r)
		select {
		case <-time.After(self.RerunTimeout):
		}
	}
}

func (self *TestEnv) setup() func() {
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
db: %d`,
			redis["authority"],
			redis["password"],
			testRedisDb,
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
