// The contract degradation valve: the rate rule and its hysteresis, the
// measurement over synthetic contracts, publication, and the contract creation
// read, which charges normally for every state it cannot trust.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Reads the raw published value, or "" when the key is missing.
func readContractDegradationTestRaw(t testing.TB, ctx context.Context) string {
	t.Helper()
	raw, present, err := readContractDegradationState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		return ""
	}
	return raw
}

// Publishes raw as the state with the state TTL, bypassing the check.
func writeContractDegradationTestRaw(t testing.TB, ctx context.Context, raw string) {
	t.Helper()
	server.Redis(ctx, func(client server.RedisClient) {
		server.Raise(client.Set(ctx, contractDegradationStateKey, raw, ContractDegradationStateTtl).Err())
	})
}

// Removes the published state.
func deleteContractDegradationTestState(ctx context.Context) {
	server.Redis(ctx, func(client server.RedisClient) {
		server.Raise(client.Del(ctx, contractDegradationStateKey).Err())
	})
}

// Enables degraded.yml for the test and drops the reader cache around it.
func enableNetworkDegradationForTest(t testing.TB) {
	t.Helper()
	pop := server.Config.PushSimpleResource(NetworkDegradationResourceName, []byte("enabled: true\n"))
	Testing_ResetZeroContractCostCache()
	t.Cleanup(func() {
		pop()
		Testing_ResetZeroContractCostCache()
	})
}

// Inserts createdCount synthetic contracts created at createTime, of which the
// first closedCount reached a terminal outcome at closeTime.
func insertContractDegradationTestContracts(t testing.TB, ctx context.Context, createTime time.Time, closeTime time.Time, createdCount int, closedCount int) {
	t.Helper()
	label := server.NewId().String()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO transfer_contract (
					contract_id,
					source_network_id,
					source_id,
					destination_network_id,
					destination_id,
					transfer_byte_count,
					create_time,
					outcome,
					close_time
				)
				SELECT
					md5($1 || '-' || n)::uuid,
					$2, $3, $4, $5,
					1024,
					$6,
					CASE WHEN n <= $8 THEN 'canceled' END,
					CASE WHEN n <= $8 THEN $7::timestamp END
				FROM generate_series(1, $9::int) AS n
			`,
			label,
			server.NewId(),
			server.NewId(),
			server.NewId(),
			server.NewId(),
			createTime,
			closeTime,
			closedCount,
			createdCount,
		))
	})
}

// The hysteresis on the pure transition: one degraded check turns zero cost on
// at once; only the second consecutive healthy check turns it off; a degraded
// check between healthy checks resets the count; and no creations is healthy.
func TestContractDegradationHysteresis(t *testing.T) {
	type check struct {
		openCount        int64
		closeCount       int64
		wantZeroCost     bool
		wantHealthyCount int
	}
	degraded := func(zeroCost bool, healthyCount int) check {
		return check{openCount: 100, closeCount: 50, wantZeroCost: zeroCost, wantHealthyCount: healthyCount}
	}
	healthy := func(zeroCost bool, healthyCount int) check {
		return check{openCount: 100, closeCount: 90, wantZeroCost: zeroCost, wantHealthyCount: healthyCount}
	}
	for _, test := range []struct {
		name   string
		checks []check
	}{
		{name: "charging stays charging", checks: []check{healthy(false, 1), healthy(false, 2), healthy(false, 2)}},
		{name: "one degraded check turns zero cost on", checks: []check{healthy(false, 1), degraded(true, 0)}},
		{name: "the second healthy check resumes charging", checks: []check{degraded(true, 0), healthy(true, 1), healthy(false, 2)}},
		{name: "a degraded check resets the count", checks: []check{degraded(true, 0), healthy(true, 1), degraded(true, 0), healthy(true, 1), healthy(false, 2)}},
		{name: "repeated degraded checks hold zero cost", checks: []check{degraded(true, 0), degraded(true, 0), degraded(true, 0)}},
		{name: "no creations is healthy", checks: []check{
			degraded(true, 0),
			{openCount: 0, closeCount: 7, wantZeroCost: true, wantHealthyCount: 1},
			{openCount: 0, closeCount: 0, wantZeroCost: false, wantHealthyCount: 2},
		}},
	} {
		var previous *ContractDegradationState
		evaluatedAt := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
		for index, c := range test.checks {
			state := nextContractDegradationState(previous, c.openCount, c.closeCount, evaluatedAt)
			if state.ZeroCost != c.wantZeroCost || state.ConsecutiveHealthyCount != c.wantHealthyCount {
				t.Fatalf("%s: check %d zero_cost=%t healthy=%d, want %t %d", test.name, index, state.ZeroCost, state.ConsecutiveHealthyCount, c.wantZeroCost, c.wantHealthyCount)
			}
			if !state.usableAt(evaluatedAt) {
				t.Fatalf("%s: check %d published a state its readers refuse: %+v", test.name, index, state)
			}
			previous = state
			evaluatedAt = evaluatedAt.Add(ContractDegradationCheckInterval)
		}
	}
}

// The rate rule at its boundary, over synthetic contracts in one window: of 100
// contracts created, 69 closed is degraded and turns zero cost on, while 70 and
// 71 are healthy. A window with closes but no creations is healthy. Contracts
// created or closed outside the window are not counted. Each check starts from
// no published state.
func TestCheckContractDegradationRatioBoundaries(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		base := server.NowUtc().Add(-24 * time.Hour)
		for index, test := range []struct {
			name         string
			createdCount int
			closedCount  int
			wantZeroCost bool
		}{
			{name: "0.69", createdCount: 100, closedCount: 69, wantZeroCost: true},
			{name: "0.70", createdCount: 100, closedCount: 70, wantZeroCost: false},
			{name: "0.71", createdCount: 100, closedCount: 71, wantZeroCost: false},
			{name: "open_count 0", createdCount: 0, closedCount: 0, wantZeroCost: false},
		} {
			// each case owns a disjoint window two hours apart
			now := base.Add(time.Duration(index) * 2 * time.Hour)
			insertContractDegradationTestContracts(t, ctx, now.Add(-30*time.Minute), now.Add(-10*time.Minute), test.createdCount, test.closedCount)
			// outside the window: created too early, and closes after its end
			insertContractDegradationTestContracts(t, ctx, now.Add(-61*time.Minute), now.Add(-61*time.Minute), 40, 40)
			insertContractDegradationTestContracts(t, ctx, now.Add(-90*time.Minute), now.Add(time.Minute), 5, 5)
			deleteContractDegradationTestState(ctx)

			state, err := CheckContractDegradation(ctx, now)
			if err != nil || state == nil {
				t.Fatalf("%s: check failed: %v", test.name, err)
			}
			if state.OpenCount != int64(test.createdCount) || state.CloseCount != int64(test.closedCount) {
				t.Fatalf("%s: counted open=%d close=%d, want %d %d", test.name, state.OpenCount, state.CloseCount, test.createdCount, test.closedCount)
			}
			if state.ZeroCost != test.wantZeroCost {
				t.Fatalf("%s: zero_cost=%t, want %t", test.name, state.ZeroCost, test.wantZeroCost)
			}
			published, err := parseContractDegradationState(readContractDegradationTestRaw(t, ctx))
			if err != nil || published.ZeroCost != test.wantZeroCost || !published.EvaluatedAt.Equal(now.UTC()) {
				t.Fatalf("%s: published %+v %v", test.name, published, err)
			}
		}
		// closes with no creations in the window are healthy, not a division
		now := base.Add(10 * 2 * time.Hour)
		insertContractDegradationTestContracts(t, ctx, now.Add(-3*time.Hour), now.Add(-5*time.Minute), 10, 10)
		deleteContractDegradationTestState(ctx)
		state, err := CheckContractDegradation(ctx, now)
		if err != nil || state == nil || state.OpenCount != 0 || state.CloseCount != 10 || state.ZeroCost || state.Ratio != 0 {
			t.Fatalf("closes without creations: %+v %v", state, err)
		}
	})
}

// The hysteresis through publication and the contract creation read: a
// degraded check turns zero cost on, the first healthy check keeps it on, a
// degraded check between healthy checks resets the published count, and only
// the second consecutive healthy check turns it off. The checks are 50 minutes
// apart, inside the state TTL, so each window holds only its own contracts.
func TestCheckContractDegradationResumesOnSecondHealthyCheck(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		enableNetworkDegradationForTest(t)
		defer func() { zeroContractCostNow = server.NowUtc }()
		start := server.NowUtc().Add(-12 * time.Hour)
		for index, test := range []struct {
			closedCount      int
			wantZeroCost     bool
			wantHealthyCount int
		}{
			{closedCount: 10, wantZeroCost: true, wantHealthyCount: 0},
			{closedCount: 80, wantZeroCost: true, wantHealthyCount: 1},
			{closedCount: 10, wantZeroCost: true, wantHealthyCount: 0},
			{closedCount: 80, wantZeroCost: true, wantHealthyCount: 1},
			{closedCount: 80, wantZeroCost: false, wantHealthyCount: 2},
		} {
			now := start.Add(time.Duration(index) * 50 * time.Minute)
			zeroContractCostNow = func() time.Time { return now }
			// older than the next check's window, newer than this one's start
			insertContractDegradationTestContracts(t, ctx, now.Add(-30*time.Minute), now.Add(-20*time.Minute), 100, test.closedCount)
			state, err := CheckContractDegradation(ctx, now)
			if err != nil || state == nil {
				t.Fatalf("check %d failed: %v", index, err)
			}
			if state.OpenCount != 100 || state.CloseCount != int64(test.closedCount) {
				t.Fatalf("check %d counted open=%d close=%d, want only its own window", index, state.OpenCount, state.CloseCount)
			}
			if state.ZeroCost != test.wantZeroCost || state.ConsecutiveHealthyCount != test.wantHealthyCount {
				t.Fatalf("check %d: zero_cost=%t healthy=%d, want %t %d", index, state.ZeroCost, state.ConsecutiveHealthyCount, test.wantZeroCost, test.wantHealthyCount)
			}
			if got := ZeroContractCost(ctx); got != test.wantZeroCost {
				t.Fatalf("check %d: contract creation reads zero cost %t, want %t", index, got, test.wantZeroCost)
			}
		}
	})
}

// A check that cannot count or cannot read the published state publishes
// nothing: the published zero cost and its healthy count stay byte-identical.
func TestCheckContractDegradationFailureKeepsState(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		published, err := json.Marshal(nextContractDegradationState(
			nextContractDegradationState(nil, 100, 10, now.Add(-ContractDegradationCheckInterval)),
			100, 90, now.Add(-time.Minute),
		))
		server.Raise(err)
		writeContractDegradationTestRaw(t, ctx, string(published))
		// healthy synthetic contracts, so a check that ran would change the count
		insertContractDegradationTestContracts(t, ctx, now.Add(-20*time.Minute), now.Add(-10*time.Minute), 100, 100)

		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if state, err := CheckContractDegradation(canceled, now); err == nil || state != nil {
			t.Fatalf("a check that could not count succeeded: %+v %v", state, err)
		}
		if raw := readContractDegradationTestRaw(t, ctx); raw != string(published) {
			t.Fatalf("a failed count changed the published state to %s", raw)
		}

		previousRead := readContractDegradationState
		defer func() { readContractDegradationState = previousRead }()
		readContractDegradationState = func(context.Context) (string, bool, error) {
			return "", false, errors.New("synthetic redis read failure")
		}
		if state, err := CheckContractDegradation(ctx, now); err == nil || state != nil {
			t.Fatalf("a check that could not read the state succeeded: %+v %v", state, err)
		}
		readContractDegradationState = previousRead
		if raw := readContractDegradationTestRaw(t, ctx); raw != string(published) {
			t.Fatalf("a failed state read changed the published state to %s", raw)
		}
	})
}

// Publication replaces only the value the check read: a value that changed
// since, or appeared since a missing read, is kept.
func TestPublishContractDegradationStateKeepsNewerState(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		newer := `{"synthetic":"newer"}`
		writeContractDegradationTestRaw(t, ctx, newer)
		for _, test := range []struct {
			name        string
			readRaw     string
			readPresent bool
		}{
			{name: "read missing", readPresent: false},
			{name: "read older", readRaw: `{"synthetic":"older"}`, readPresent: true},
		} {
			published, err := publishContractDegradationState(ctx, test.readRaw, test.readPresent, `{"synthetic":"stale"}`)
			if err != nil || published {
				t.Fatalf("%s: published=%t err=%v, want refused", test.name, published, err)
			}
			if raw := readContractDegradationTestRaw(t, ctx); raw != newer {
				t.Fatalf("%s: overwrote the newer state with %s", test.name, raw)
			}
		}
		published, err := publishContractDegradationState(ctx, newer, true, `{"synthetic":"next"}`)
		if err != nil || !published {
			t.Fatalf("matching publication refused: %t %v", published, err)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			ttl, err := client.PTTL(ctx, contractDegradationStateKey).Result()
			server.Raise(err)
			if ttl <= 0 || ContractDegradationStateTtl < ttl {
				t.Fatalf("published state ttl %s, want at most %s", ttl, ContractDegradationStateTtl)
			}
		})
	})
}

// Contract creation charges normally for every state it cannot trust: a
// disabled, absent or malformed degraded.yml, a missing key, a Redis error, an
// expired state, including one that expires inside the reader's cache period,
// and malformed or inconsistent values. A valid zero cost state is the control.
func TestZeroContractCostChargesNormallyForUntrustedState(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		defer func() { zeroContractCostNow = server.NowUtc }()
		zeroContractCostNow = func() time.Time { return now }
		valid, err := json.Marshal(nextContractDegradationState(nil, 100, 10, now.Add(-time.Minute)))
		server.Raise(err)
		read := func() bool {
			Testing_ResetZeroContractCostCache()
			return ZeroContractCost(ctx)
		}

		// disabled while absent, so a valid zero cost state is ignored
		writeContractDegradationTestRaw(t, ctx, string(valid))
		if read() {
			t.Fatal("an absent degraded.yml made contracts zero cost")
		}
		popDisabled := server.Config.PushSimpleResource(NetworkDegradationResourceName, []byte("enabled: false\n"))
		if read() {
			t.Fatal("a disabled degraded.yml made contracts zero cost")
		}
		popDisabled()
		popMalformed := server.Config.PushSimpleResource(NetworkDegradationResourceName, []byte("enable: true\n"))
		if read() {
			t.Fatal("a misspelled degraded.yml made contracts zero cost")
		}
		popMalformed()

		enableNetworkDegradationForTest(t)
		if !read() {
			t.Fatal("control: a valid zero cost state did not make contracts zero cost")
		}

		// a state cached five seconds before its ttl stops being zero cost ten
		// seconds later, inside the cache period and without another read
		nearlyExpired, err := json.Marshal(nextContractDegradationState(nil, 100, 10, now.Add(-ContractDegradationStateTtl).Add(5*time.Second)))
		server.Raise(err)
		writeContractDegradationTestRaw(t, ctx, string(nearlyExpired))
		if !read() {
			t.Fatal("control: a state inside its ttl did not make contracts zero cost")
		}
		previousRead := readContractDegradationState
		readContractDegradationState = func(context.Context) (string, bool, error) {
			t.Error("the reader refreshed inside its cache period")
			return "", false, errors.New("synthetic unexpected read")
		}
		zeroContractCostNow = func() time.Time { return now.Add(10 * time.Second) }
		if ZeroContractCost(ctx) {
			t.Fatal("a cached state stayed zero cost past its ttl")
		}
		readContractDegradationState = previousRead
		zeroContractCostNow = func() time.Time { return now }

		expired, err := json.Marshal(nextContractDegradationState(nil, 100, 10, now.Add(-ContractDegradationStateTtl)))
		server.Raise(err)
		inconsistentHealthy, err := json.Marshal(&ContractDegradationState{ZeroCost: true, OpenCount: 100, CloseCount: 90, Ratio: 0.9, ConsecutiveHealthyCount: 2, EvaluatedAt: now})
		server.Raise(err)
		inconsistentRatio, err := json.Marshal(&ContractDegradationState{ZeroCost: true, OpenCount: 100, CloseCount: 10, Ratio: 0.5, EvaluatedAt: now})
		server.Raise(err)
		future, err := json.Marshal(nextContractDegradationState(nil, 100, 10, now.Add(time.Hour)))
		server.Raise(err)
		for _, test := range []struct {
			name string
			raw  string
		}{
			{name: "expired", raw: string(expired)},
			{name: "not json", raw: "synthetic-not-json"},
			{name: "unknown field", raw: strings.Replace(string(valid), `{`, `{"synthetic":1,`, 1)},
			{name: "trailing data", raw: string(valid) + " {}"},
			{name: "zero cost with two healthy checks", raw: string(inconsistentHealthy)},
			{name: "ratio not from counts", raw: string(inconsistentRatio)},
			{name: "evaluated in the future", raw: string(future)},
			{name: "only zero_cost", raw: `{"zero_cost":true}`},
		} {
			writeContractDegradationTestRaw(t, ctx, test.raw)
			if read() {
				t.Errorf("%s: an untrusted state made contracts zero cost", test.name)
			}
		}

		deleteContractDegradationTestState(ctx)
		if read() {
			t.Fatal("a missing state made contracts zero cost")
		}

		writeContractDegradationTestRaw(t, ctx, string(valid))
		defer func() { readContractDegradationState = previousRead }()
		readContractDegradationState = func(context.Context) (string, bool, error) {
			return "", false, errors.New("synthetic redis read failure")
		}
		if read() {
			t.Fatal("a Redis error made contracts zero cost")
		}
		// an error is retried after a second, not a whole cache period
		readContractDegradationState = previousRead
		if ZeroContractCost(ctx) {
			t.Fatal("the error result was not cached for its retry period")
		}
		zeroContractCostNow = func() time.Time { return now.Add(zeroContractCostErrorRetry) }
		if !ZeroContractCost(ctx) {
			t.Fatal("the reader did not retry after its error period")
		}
	})
}

// Each count is a range over its named index. The test database is small, so
// sequential scans are disabled to show the predicates match the indexes.
func TestContractDegradationCountsUseTheirIndexes(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
			for _, test := range []struct {
				name  string
				sql   string
				index string
			}{
				{name: "open", sql: contractDegradationOpenCountSql, index: "transfer_contract_create_time"},
				{name: "close", sql: contractDegradationCloseCountSql, index: "transfer_contract_closed_usage"},
			} {
				plan := []string{}
				result, err := tx.Query(ctx, `EXPLAIN `+test.sql, now.Add(-ContractDegradationWindow), now)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var line string
						server.Raise(result.Scan(&line))
						plan = append(plan, line)
					}
				})
				if !strings.Contains(strings.Join(plan, "\n"), test.index) {
					t.Errorf("%s count does not use %s:\n%s", test.name, test.index, strings.Join(plan, "\n"))
				}
			}
		})
	})
}
