package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// These fixtures use only their TestEnv's Redis database and synthetic keys.
// A list-valued key gives a real GET error after the wrapper's PING succeeds;
// a missing key remains a normal cache miss. No shared client hook is changed.
func providerPickerMetadataFixture(t testing.TB, mode string) (context.Context, map[server.Id]bool, map[server.Id]*ClientLocation, error) {
	t.Helper()
	ctx := t.Context()
	cityId, countryId, missingId := server.NewId(), server.NewId(), server.NewId()
	city := &ClientLocation{LocationId: cityId, LocationType: LocationTypeCity, Name: "Synthetic city", CountryLocationId: &countryId}
	country := &ClientLocation{LocationId: countryId, LocationType: LocationTypeCountry, Name: "Synthetic country"}
	requested := map[server.Id]bool{cityId: true}
	want := map[server.Id]*ClientLocation{cityId: city, countryId: country}
	var expected error
	server.Redis(ctx, func(r server.RedisClient) {
		set := func(id server.Id, value *ClientLocation) {
			t.Helper()
			server.Raise(r.Set(ctx, clientLocationKey(id), gobEncodeForTest(t, value), time.Minute).Err())
		}
		set(cityId, city)
		set(countryId, country)
		var failedId server.Id
		switch mode {
		case "matched-failure":
			failedId = cityId
		case "matched-mixed-failure":
			requested[countryId] = true
			requested[missingId] = true
			failedId = cityId
		case "related-failure":
			failedId = countryId
		case "related-mixed-failure":
			city.RegionLocationId = &missingId
			set(cityId, city)
			failedId = countryId
		case "corrupt-related":
			server.Raise(r.Set(ctx, clientLocationKey(countryId), "not-gob", time.Minute).Err())
		case "missing":
			requested = map[server.Id]bool{missingId: true}
			want = map[server.Id]*ClientLocation{}
		case "missing-related":
			server.Raise(r.Del(ctx, clientLocationKey(countryId)).Err())
			delete(want, countryId)
		case "empty-value":
			server.Raise(r.Set(ctx, clientLocationKey(cityId), "", time.Minute).Err())
			want = map[server.Id]*ClientLocation{}
		case "valid":
			requested[missingId] = true
		default:
			t.Fatal("unknown metadata fixture")
		}
		if failedId != (server.Id{}) {
			key := clientLocationKey(failedId)
			server.Raise(r.Del(ctx, key).Err())
			server.Raise(r.RPush(ctx, key, "synthetic-wrong-type").Err())
			server.Raise(r.Expire(ctx, key, time.Minute).Err())
			expected = r.Get(ctx, key).Err()
			if expected == nil || !strings.HasPrefix(expected.Error(), "WRONGTYPE ") {
				t.Fatal("fixture did not establish a Redis GET error")
			}
			if strings.Contains(mode, "mixed") {
				// Establish the real protocol behavior that makes checking only
				// Exec's error insufficient. The production map order is arbitrary.
				pipe := r.Pipeline()
				missing := pipe.Get(ctx, clientLocationKey(missingId))
				failed := pipe.Get(ctx, key)
				_, err := pipe.Exec(ctx)
				if !errors.Is(err, redis.Nil) || !errors.Is(missing.Err(), redis.Nil) || !errors.Is(failed.Err(), expected) {
					t.Fatal("fixture did not establish a later failure behind redis.Nil")
				}
			}
		}
	})
	return ctx, requested, want, expected
}

func TestProviderPickerMetadataRejectsReadFailure(t *testing.T) {
	for _, mode := range []string{"matched-failure", "matched-mixed-failure", "related-failure", "related-mixed-failure"} {
		t.Run(mode, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx, requested, _, expected := providerPickerMetadataFixture(t, mode)
				result, err := loadClientLocations(ctx, requested)
				if expected == nil || !errors.Is(err, expected) || result != nil {
					t.Fatalf("metadata failure became empty/partial success: error_preserved=%t result_nil=%t", errors.Is(err, expected), result == nil)
				}
			})
		})
	}
}

func TestProviderPickerMetadataRejectsPartialDecode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, requested, _, _ := providerPickerMetadataFixture(t, "corrupt-related")
		result, err := loadClientLocations(ctx, requested)
		if err == nil || result != nil {
			t.Fatal("corrupt related metadata retained a partial result")
		}
	})
}

// The metadata loader's map traversal has no defined command order. Exercise
// its shared batch validator with a deterministic missing-then-failed wire
// order as well as the actual matched/related loader paths above.
func TestProviderPickerMetadataOrderedMaskedCommandFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, requested, _, expected := providerPickerMetadataFixture(t, "matched-failure")
		var failedId server.Id
		for id := range requested {
			failedId = id
		}
		server.Redis(ctx, func(r server.RedisClient) {
			missingKey := clientLocationKey(server.NewId())
			ordered := func() redis.Pipeliner {
				pipe := r.Pipeline()
				pipe.Get(ctx, missingKey)
				pipe.Get(ctx, clientLocationKey(failedId))
				return pipe
			}
			commands, err := ordered().Exec(ctx)
			if !errors.Is(err, redis.Nil) || len(commands) != 2 || !errors.Is(commands[1].Err(), expected) {
				t.Fatal("ordered actual Redis fixture did not mask the second failure")
			}
			if err := execClientScoreReadPipeline(ctx, ordered()); !errors.Is(err, expected) {
				t.Fatal("shared metadata batch validator lost a failure behind redis.Nil")
			}
		})
	})
}

func TestProviderPickerMetadataMissingAndValidControls(t *testing.T) {
	for _, mode := range []string{"missing", "missing-related", "empty-value", "valid"} {
		t.Run(mode, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx, requested, want, _ := providerPickerMetadataFixture(t, mode)
				result, err := loadClientLocations(ctx, requested)
				if err != nil || len(result) != len(want) {
					t.Fatalf("legitimate metadata read changed: error=%v count=%d want=%d", err, len(result), len(want))
				}
				for id, expected := range want {
					if actual := result[id]; actual == nil || actual.LocationId != expected.LocationId || actual.Name != expected.Name || actual.LocationType != expected.LocationType {
						t.Fatal("valid matched or related metadata disappeared")
					}
				}
			})
		})
	}
}

func TestProviderPickerMetadataSearchFailureIsNotEmptySuccess(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, requested, _, expected := providerPickerMetadataFixture(t, "matched-failure")
		for id := range requested {
			locationSearch().Add(ctx, "syntheticmetadata", id, 0)
		}
		beforeError := testutil.ToFloat64(providerPickerMetrics.children["search/error"])
		beforeEmpty := testutil.ToFloat64(providerPickerMetrics.children["search/empty"])
		result, err := FindProviderLocations(&FindLocationsArgs{Query: "syntheticmetadata"}, &session.ClientSession{Ctx: ctx, ClientAddress: "127.0.0.1:1234"})
		if !errors.Is(err, expected) || result != nil {
			t.Fatal("search POST hid its metadata read failure")
		}
		if testutil.ToFloat64(providerPickerMetrics.children["search/error"])-beforeError != 1 || testutil.ToFloat64(providerPickerMetrics.children["search/empty"]) != beforeEmpty {
			t.Fatal("failed metadata read was not attributed to exactly one search error")
		}
	})
}
