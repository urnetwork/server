// Actual private Redis proves protected minima and atomic bounded idempotency.
package model

import (
	"crypto/sha256"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/server"
	"gopkg.in/yaml.v3"
)

// Small independent transport capacities do not replace any protocol bound.
func stReservedUploadTestOwner() StReservedAttemptUploadOwner {
	return StReservedAttemptUploadOwner{Hotkey: sha256.Sum256([]byte("historical-hotkey")), OperatorNoID: 1}
}

// An observed valid intent can spend only retry allowance, never later fresh
// object/byte reservations. Every underlying decision uses the actual script.
func TestStReservedUploadObservedRetryFloodPreservesFreshMinima(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		server.RedisDoOnce(tb.Context(), func(client server.RedisClient) {
			keys := stReservedAttemptUploadKeys(StDeploymentKey(server.NewId().String()), 2, stReservedUploadTestOwner())
			budget := StReservedAttemptUploadBudget{RetryRequestsPerHour: 2, ObjectsPerHour: 2, BytesPerHour: 16}
			object := sha256.Sum256([]byte("same signed object"))
			fresh, err := reserveStReservedAttemptUploadWithClient(tb.Context(), client, keys, object, 8, 7225, 60, budget, 7200)
			if err != nil || !fresh {
				tb.Fatalf("genuine first reservation: %v", err)
			}
			for index := range 6 {
				fresh, err := reserveStReservedAttemptUploadWithClient(tb.Context(), client, keys, object, 8, 7225, 60, budget, 7200)
				var limited interface{ RetryAfterSeconds() int }
				if index < 2 {
					if err != nil || fresh {
						tb.Fatalf("idempotent retry %d minted capacity: %v", index, err)
					}
				} else if fresh || !errors.As(err, &limited) {
					tb.Fatalf("replayed intent %d bypassed retry bound: %v", index, err)
				}
			}
			next := sha256.Sum256([]byte("later legitimate distinct object"))
			fresh, err = reserveStReservedAttemptUploadWithClient(tb.Context(), client, keys, next, 8, 7225, 60, budget, 7200)
			if err != nil || !fresh {
				tb.Fatalf("observed-intent flood denied protected fresh staging: %v", err)
			}
			counters, err := client.HGetAll(tb.Context(), keys[0]).Result()
			if err != nil || counters["objects"] != "2" || counters["bytes"] != "16" || counters["retries"] != "2" {
				tb.Fatalf("protected reservation accounting differs: %v/%v", counters, err)
			}
		})
	})
}

// Arbitrary real account counters have a disjoint namespace from every
// authenticated owner; exhausting one cannot change the other's admission.
func TestStReservedUploadOrdinaryAccountsCannotSpendProtectedMinima(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		server.RedisDoOnce(tb.Context(), func(client server.RedisClient) {
			deployment := StDeploymentKey(server.NewId().String())
			ordinary := StAttemptUploadBudget{RequestsPerHour: 2, BytesPerHour: 16, AccountRequestsPerHour: 1, AccountBytesPerHour: 8}
			for range 2 {
				if err := reserveStAttemptUploadWithClient(tb.Context(), client, stAttemptUploadQuotaKeys(deployment, server.NewId()), 8, ordinary, 7200); err != nil {
					tb.Fatal(err)
				}
			}
			if err := reserveStAttemptUploadWithClient(tb.Context(), client, stAttemptUploadQuotaKeys(deployment, server.NewId()), 1, ordinary, 7200); err == nil {
				tb.Fatal("ordinary flood prerequisite did not exhaust capacity")
			}
			budget := StReservedAttemptUploadBudget{RetryRequestsPerHour: 1, ObjectsPerHour: 1, BytesPerHour: 8}
			fresh, err := reserveStReservedAttemptUploadWithClient(tb.Context(), client, stReservedAttemptUploadKeys(deployment, 2, stReservedUploadTestOwner()), sha256.Sum256([]byte("protected")), 8, 7225, 60, budget, 7200)
			if err != nil || !fresh {
				tb.Fatalf("ordinary accounts consumed validator minima: %v", err)
			}
		})
	})
}

// Duplicate concurrently observed requests produce one object debit, never a
// read/then-write race; all remaining requests use bounded retry accounting.
func TestStReservedUploadConcurrentSameObjectReservesExactlyOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		server.RedisDoOnce(tb.Context(), func(client server.RedisClient) {
			keys := stReservedAttemptUploadKeys(StDeploymentKey(server.NewId().String()), 2, stReservedUploadTestOwner())
			budget := StReservedAttemptUploadBudget{RetryRequestsPerHour: 2, ObjectsPerHour: 1, BytesPerHour: 8}
			object := sha256.Sum256([]byte("same object"))
			start := make(chan struct{})
			type outcome struct {
				fresh bool
				err   error
			}
			results := make(chan outcome, 3)
			var joined sync.WaitGroup
			for range 3 {
				joined.Add(1)
				go func() {
					defer joined.Done()
					<-start
					fresh, err := reserveStReservedAttemptUploadWithClient(tb.Context(), client, keys, object, 8, 7225, 60, budget, 7200)
					results <- outcome{fresh: fresh, err: err}
				}()
			}
			close(start)
			joined.Wait()
			close(results)
			freshCount := 0
			for result := range results {
				if result.err != nil {
					tb.Fatal(result.err)
				}
				if result.fresh {
					freshCount++
				}
			}
			if freshCount != 1 {
				tb.Fatalf("same-object race reserved %d objects", freshCount)
			}
			if count, err := client.HLen(tb.Context(), keys[1]).Result(); err != nil || count != 1 {
				tb.Fatalf("object census differs: %d/%v", count, err)
			}
		})
	})
}

// Neither independent capacity can hide behind the other one binding first.
func TestStReservedUploadObjectAndByteBoundsAreIndependent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		server.RedisDoOnce(tb.Context(), func(client server.RedisClient) {
			for _, variation := range []struct {
				name   string
				budget StReservedAttemptUploadBudget
			}{
				{name: "count-only", budget: StReservedAttemptUploadBudget{RetryRequestsPerHour: 4, ObjectsPerHour: 1, BytesPerHour: 800}},
				{name: "byte-only", budget: StReservedAttemptUploadBudget{RetryRequestsPerHour: 4, ObjectsPerHour: 100, BytesPerHour: 8}},
			} {
				keys := stReservedAttemptUploadKeys(StDeploymentKey(server.NewId().String()), 2, stReservedUploadTestOwner())
				first := sha256.Sum256([]byte("first"))
				if fresh, err := reserveStReservedAttemptUploadWithClient(tb.Context(), client, keys, first, 8, 7225, 60, variation.budget, 7200); err != nil || !fresh {
					tb.Fatalf("%s exact boundary: %v", variation.name, err)
				}
				before, err := client.HGetAll(tb.Context(), keys[0]).Result()
				if err != nil {
					tb.Fatal(err)
				}
				second := sha256.Sum256([]byte("second"))
				if fresh, err := reserveStReservedAttemptUploadWithClient(tb.Context(), client, keys, second, 1, 7225, 60, variation.budget, 7200); err == nil || fresh {
					tb.Fatalf("%s exhaustion was accepted", variation.name)
				}
				after, err := client.HGetAll(tb.Context(), keys[0]).Result()
				if err != nil || !reflect.DeepEqual(before, after) {
					tb.Fatalf("%s refused reservation mutated counters: %v", variation.name, err)
				}
				if fresh, err := reserveStReservedAttemptUploadWithClient(tb.Context(), client, keys, first, 8, 7225, 60, variation.budget, 7200); err != nil || fresh {
					tb.Fatalf("%s full minima denied idempotent object: %v", variation.name, err)
				}
			}
		})
	})
}

// Partial/malformed Redis state is not permission to reset ownership. Valid
// expiry/rollover is explicit and signed stale/future intents never consume.
func TestStReservedUploadClockCorruptionAndObjectDriftFailClosed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		server.RedisDoOnce(tb.Context(), func(client server.RedisClient) {
			budget := StReservedAttemptUploadBudget{RetryRequestsPerHour: 4, ObjectsPerHour: 4, BytesPerHour: 40}
			object := sha256.Sum256([]byte("identity"))
			for _, fault := range []string{"expired", "far-future", "counter", "partial", "census", "size", "rollback"} {
				keys := stReservedAttemptUploadKeys(StDeploymentKey(server.NewId().String()), 2, stReservedUploadTestOwner())
				if _, err := reserveStReservedAttemptUploadWithClient(tb.Context(), client, keys, object, 8, 7225, 60, budget, 7200); err != nil {
					tb.Fatal(err)
				}
				now, expiry, size := int64(7200), uint64(7225), uint64(8)
				switch fault {
				case "expired":
					expiry = 7200
				case "far-future":
					expiry = 7261
				case "counter":
					if err := client.HSet(tb.Context(), keys[0], "bytes", "1.5").Err(); err != nil {
						tb.Fatal(err)
					}
				case "partial":
					if err := client.HDel(tb.Context(), keys[0], "objects").Err(); err != nil {
						tb.Fatal(err)
					}
				case "census":
					if err := client.HSet(tb.Context(), keys[1], "foreign-object", "8").Err(); err != nil {
						tb.Fatal(err)
					}
				case "size":
					size = 7
				case "rollback":
					now, expiry = 7199, 7224
				}
				before, err := client.HGetAll(tb.Context(), keys[0]).Result()
				if err != nil {
					tb.Fatal(err)
				}
				if fresh, err := reserveStReservedAttemptUploadWithClient(tb.Context(), client, keys, object, size, expiry, 60, budget, now); err == nil || fresh {
					tb.Fatalf("%s invalid state admitted", fault)
				}
				after, err := client.HGetAll(tb.Context(), keys[0]).Result()
				if err != nil || !reflect.DeepEqual(before, after) {
					tb.Fatalf("%s refusal changed state: %v", fault, err)
				}
			}
			keys := stReservedAttemptUploadKeys(StDeploymentKey(server.NewId().String()), 2, stReservedUploadTestOwner())
			for _, now := range []int64{7200, 10800} {
				if fresh, err := reserveStReservedAttemptUploadWithClient(tb.Context(), client, keys, object, 8, uint64(now+25), 60, budget, now); err != nil || !fresh {
					tb.Fatalf("bounded UTC rollover differs: %v", err)
				}
			}
		})
	})
}

// Mutable activations/VPKs never enter key construction. Distinct historical
// owners and replica destinations cannot debit each other's allocation.
func TestStReservedUploadStableOwnerAndReplicaNamespaces(t *testing.T) {
	t.Parallel()
	owner := stReservedUploadTestOwner()
	first := stReservedAttemptUploadKeys("945:coordinator", 2, owner)
	if !reflect.DeepEqual(first, stReservedAttemptUploadKeys("945:coordinator", 2, owner)) {
		t.Fatal("same hotkey owner changed on refresh")
	}
	for _, variation := range []string{"hotkey", "operator", "replica", "deployment"} {
		nextOwner, replica, deployment := owner, uint64(2), StDeploymentKey("945:coordinator")
		switch variation {
		case "hotkey":
			nextOwner.Hotkey[0] ^= 1
		case "operator":
			nextOwner.OperatorNoID++
		case "replica":
			replica++
		case "deployment":
			deployment = "945:other"
		}
		if reflect.DeepEqual(first, stReservedAttemptUploadKeys(deployment, replica, nextOwner)) {
			t.Fatalf("%s namespace collapsed", variation)
		}
	}
	for _, keys := range [][]string{first, stReservedAttemptUploadKeys("injected}{identity", 2, owner)} {
		end := strings.IndexByte(keys[0], '}')
		if end < 1 || keys[0][:end+1] != keys[1][:end+1] || strings.Count(keys[0], "{") != 1 || strings.Count(keys[0], "}") != 1 {
			t.Fatal("reserved key escaped cluster ownership")
		}
	}
}

// Lossy/partial scalar parsing cannot expand the explicit reservation budget.
func TestStReservedUploadBudgetRejectsLossyOrMissingCapacity(t *testing.T) {
	t.Parallel()
	good := StReservedAttemptUploadBudget{RetryRequestsPerHour: 2, ObjectsPerHour: 4, BytesPerHour: 40}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	var decoded StReservedAttemptUploadBudget
	if err := yaml.Unmarshal(raw, &decoded); err != nil || decoded != good {
		t.Fatalf("exact YAML prerequisite: %v", err)
	}
	for _, field := range []string{"retry_requests_per_hour: 2", "objects_per_hour: 4", "bytes_per_hour: 40"} {
		name := strings.SplitN(field, ":", 2)[0]
		for _, scalar := range []string{"1.5", "\"2\"", "02", "+2", "2e0", "true", "null", "-1", "18446744073709551616"} {
			next := good
			encoded := strings.Replace(string(raw), field, name+": "+scalar, 1)
			if encoded == string(raw) {
				t.Fatalf("fixture field %s missing", name)
			}
			if err := yaml.Unmarshal([]byte(encoded), &next); err == nil || next != good {
				t.Fatalf("%s=%s silently changed capacity: %v", name, scalar, err)
			}
		}
	}
	for _, budget := range []StReservedAttemptUploadBudget{{}, {RetryRequestsPerHour: 2, ObjectsPerHour: 4}, {RetryRequestsPerHour: 2, ObjectsPerHour: 4, BytesPerHour: 9007199254740992}} {
		if err := budget.Validate(); err == nil {
			t.Fatal("missing/overflowing capacity accepted")
		}
	}
}
